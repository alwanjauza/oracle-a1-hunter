package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// OCIClient is an OCI REST client with request signing version 1 (rsa-sha256),
// mimicking the headers of the official Python SDK (required by the launch
// endpoint: x-content-sha256 etc. — see sign()).
type OCIClient struct {
	Tenancy    string
	User       string
	Fingerprint string
	Region     string
	key        *rsa.PrivateKey
	hc         *http.Client
}

func LoadPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("file %s is not a valid PEM", path)
	}
	if strings.Contains(block.Type, "PUBLIC") {
		return nil, fmt.Errorf("file %s contains a PUBLIC key — the PRIVATE key is required (BEGIN PRIVATE KEY)", path)
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, fmt.Errorf("key is not RSA")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("failed to parse private key (supported: PKCS8 \"BEGIN PRIVATE KEY\" / PKCS1 \"BEGIN RSA PRIVATE KEY\")")
}

// dnsDialer — Android has no /etc/resolv.conf for native processes (DNS
// goes through netd for the app framework only), so Go's default resolver
// falls back to [::1]:53 and fails. Use explicit public resolvers.
var dnsDialer = &net.Dialer{
	Timeout:   10 * time.Second,
	Resolver: &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var lastErr error
			for _, ns := range []string{"8.8.8.8:53", "1.1.1.1:53", "8.8.4.4:53"} {
				d := net.Dialer{Timeout: 5 * time.Second}
				c, err := d.DialContext(ctx, "udp", ns)
				if err == nil {
					return c, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	},
}

// sharedTransport backs every HTTP client (OCI + Telegram).
var sharedTransport = &http.Transport{
	MaxIdleConns:        4,
	IdleConnTimeout:     90 * time.Second,
	DialContext:         dnsDialer.DialContext,
	TLSHandshakeTimeout: 15 * time.Second,
}

func NewOCIClient(cfg *Config, baseDir string) (*OCIClient, error) {
	key, err := LoadPrivateKey(cfg.PrivateKeyPathAbs(baseDir))
	if err != nil {
		return nil, err
	}
	return &OCIClient{
		Tenancy: cfg.TenancyOCID,
		User:    cfg.UserOCID,
		Fingerprint: cfg.Fingerprint,
		Region:  cfg.Region,
		key:     key,
		hc: &http.Client{
			Timeout:   25 * time.Second,
			Transport: sharedTransport,
		},
	}, nil
}

// APIResult is the raw outcome of an API call.
type APIResult struct {
	HTTPStatus     int
	Code           string // kode error OCI (mis. OutOfHostCapacity)
	Message        string
	Body           []byte
	RetryAfterSecs int
	Err            error
}

func (r *APIResult) IsCapacity() bool {
	if r.HTTPStatus == 429 {
		return false
	}
	c := strings.ToLower(r.Code + " " + r.Message)
	return strings.Contains(c, "outofhostcapacity") || strings.Contains(c, "out of host capacity") ||
		strings.Contains(c, "internalerror") || strings.Contains(c, "internal error")
}

// sign mimics the OCI Python SDK exactly (proven to pass the launch
// endpoint):
// - x-content-sha256 for body-bearing requests (body hash is signed too)
// - signing header order: date (request-target) host [content-length content-type x-content-sha256]
// - opc-request-id, opc-client-info and SDK-style user-agent headers
func (c *OCIClient) sign(req *http.Request, body []byte) error {
	now := time.Now().UTC().Format(http.TimeFormat)
	req.Header.Set("date", now)
	req.Header.Set("accept", "application/json")
	req.Header.Set("opc-client-info", "Oracle-PythonSDK/2.189.0")
	req.Header.Set("opc-request-id", randomRequestID())
	req.Header.Set("user-agent", "Oracle-PythonSDK/2.189.0 (python 3.14.8; x86_64-Linux)")

	var parts, names []string
	add := func(name, value string) {
		parts = append(parts, name+": "+value)
		names = append(names, name)
	}
	add("date", now)
	add("(request-target)", strings.ToLower(req.Method)+" "+req.URL.RequestURI())
	add("host", req.URL.Host)
	if body != nil {
		req.Header.Set("content-type", "application/json")
		req.Header.Set("content-length", strconv.Itoa(len(body)))
		sum := sha256.Sum256(body)
		xsha := base64.StdEncoding.EncodeToString(sum[:])
		req.Header.Set("x-content-sha256", xsha)
		add("content-length", strconv.Itoa(len(body)))
		add("content-type", "application/json")
		add("x-content-sha256", xsha)
	}
	mac := strings.Join(parts, "\n")
	sum := sha256.Sum256([]byte(mac))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return err
	}
	auth := fmt.Sprintf(`Signature version="1",keyId="%s/%s/%s",algorithm="rsa-sha256",headers="%s",signature="%s"`,
		c.Tenancy, c.User, c.Fingerprint, strings.Join(names, " "), base64.StdEncoding.EncodeToString(sig))
	req.Header.Set("authorization", auth)
	return nil
}

func randomRequestID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

func (c *OCIClient) do(host, method, path string, query url.Values, body []byte) *APIResult {
	u := host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		return &APIResult{Err: err}
	}
	if err := c.sign(req, body); err != nil {
		return &APIResult{Err: err}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return &APIResult{Err: err}
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	res := &APIResult{HTTPStatus: resp.StatusCode, Body: rb}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil {
			res.RetryAfterSecs = n
		}
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(rb, &e) == nil && e.Code != "" {
			res.Code, res.Message = e.Code, e.Message
		} else {
			res.Message = string(rb[:min(len(rb), 300)])
		}
	}
	return res
}

func min(a, b int) int { if a < b { return a }; return b }

func (c *OCIClient) iaas() string { return "https://iaas." + c.Region + ".oraclecloud.com" }
func (c *OCIClient) identity() string { return "https://identity." + c.Region + ".oraclecloud.com" }
func (c *OCIClient) limits() string { return "https://limits." + c.Region + ".oraclecloud.com" }

// GetTenancy validates credentials and returns the tenancy name.
func (c *OCIClient) GetTenancy() (*APIResult, string) {
	r := c.do(c.identity(), "GET", "/20160918/tenancies/"+url.PathEscape(c.Tenancy), nil, nil)
	if r.Err != nil || r.HTTPStatus != 200 {
		return r, ""
	}
	var t struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	json.Unmarshal(r.Body, &t)
	return r, t.Name
}

// ListADs lists availability domains.
func (c *OCIClient) ListADs() (*APIResult, []string) {
	q := url.Values{"compartmentId": {c.Tenancy}}
	r := c.do(c.identity(), "GET", "/20160918/availabilityDomains", q, nil)
	if r.Err != nil || r.HTTPStatus != 200 {
		return r, nil
	}
	var ads []struct {
		Name string `json:"name"`
	}
	json.Unmarshal(r.Body, &ads)
	out := make([]string, len(ads))
	for i, a := range ads {
		out[i] = a.Name
	}
	return r, out
}

type OCIImage struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	OperatingSystem   string `json:"operatingSystem"`
	OSVersion         string `json:"operatingSystemVersion"`
}

// ListImages finds images by OS name + version + shape.
func (c *OCIClient) ListImages(osName, osVersion, shape string) (*APIResult, []OCIImage) {
	q := url.Values{
		"compartmentId":          {c.Tenancy},
		"operatingSystem":        {osName},
		"operatingSystemVersion": {osVersion},
		"shape":                  {shape},
		"sortBy":                 {"TIMECREATED"},
		"sortOrder":              {"DESC"},
	}
	r := c.do(c.iaas(), "GET", "/20160918/images", q, nil)
	if r.Err != nil || r.HTTPStatus != 200 {
		return r, nil
	}
	var imgs []OCIImage
	json.Unmarshal(r.Body, &imgs)
	return r, imgs
}

type OCIInstance struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	Shape          string `json:"shape"`
	LifecycleState string `json:"lifecycleState"`
	ShapeConfig    struct {
		OCpus       float64 `json:"ocpus"`
		MemoryInGBs float64 `json:"memoryInGBs"`
	} `json:"shapeConfig"`
}

// ListSubnets lists the subnets of the tenancy (used for auto-discovery).
func (c *OCIClient) ListSubnets() (*APIResult, []string) {
	q := url.Values{"compartmentId": {c.Tenancy}}
	r := c.do(c.iaas(), "GET", "/20160918/subnets", q, nil)
	if r.Err != nil || r.HTTPStatus != 200 {
		return r, nil
	}
	var subs []struct {
		ID string `json:"id"`
	}
	json.Unmarshal(r.Body, &subs)
	out := make([]string, len(subs))
	for i, s := range subs {
		out[i] = s.ID
	}
	return r, out
}

func (c *OCIClient) ListInstances() (*APIResult, []OCIInstance) {
	q := url.Values{"compartmentId": {c.Tenancy}}
	r := c.do(c.iaas(), "GET", "/20160918/instances", q, nil)
	if r.Err != nil || r.HTTPStatus != 200 {
		return r, nil
	}
	var list []OCIInstance
	json.Unmarshal(r.Body, &list)
	return r, list
}

// LimitAvailability returns the remaining quota for a limit (e.g. standard-a1-core-count).
func (c *OCIClient) LimitAvailability(limitName string) (*APIResult, float64) {
	q := url.Values{
		"compartmentId": {c.Tenancy},
		"serviceName":   {"compute"},
	}
	r := c.do(c.limits(), "GET", "/20181225/limits/"+url.PathEscape(limitName)+"/resourceAvailability", q, nil)
	if r.Err != nil || r.HTTPStatus != 200 {
		return r, -1
	}
	var a struct {
		Used      float64 `json:"used"`
		Available float64 `json:"available"`
	}
	json.Unmarshal(r.Body, &a)
	return r, a.Available
}

// LaunchInstance attempts to create an instance — this IS the hunting
// "check": when no capacity is available Oracle answers 500 OutOfHostCapacity;
// when it is, the answer is 201 and the instance is created immediately.
func (c *OCIClient) LaunchInstance(ad, subnet, image, shape, displayName, sshKeys string, ocpus, memGB, bootGB int, assignPublicIP bool) (*APIResult, map[string]any) {
	launch := map[string]any{
		"availabilityDomain": ad,
		"compartmentId":      c.Tenancy,
		"displayName":        displayName,
		"shape":              shape,
		"sourceDetails": map[string]any{
			"sourceType":          "image",
			"imageId":             image,
			"bootVolumeSizeInGBs": bootGB,
		},
		"createVnicDetails": map[string]any{
			"assignPublicIp":          assignPublicIP,
			"assignPrivateDnsRecord":  true,
			"displayName":             displayName,
			"subnetId":                subnet,
		},
		"metadata": map[string]any{
			"ssh_authorized_keys": sshKeys,
		},
		"availabilityConfig": map[string]any{
			"recoveryAction": "RESTORE_INSTANCE",
		},
		"instanceOptions": map[string]any{
			"areLegacyImdsEndpointsDisabled": false,
		},
	}
	if ocpus > 0 || memGB > 0 {
		sc := map[string]any{}
		if ocpus > 0 {
			sc["ocpus"] = ocpus
		}
		if memGB > 0 {
			sc["memoryInGBs"] = memGB
		}
		launch["shapeConfig"] = sc
	}
	body, _ := json.Marshal(launch)
	r := c.do(c.iaas(), "POST", "/20160918/instances", nil, body)
	var inst map[string]any
	if r.HTTPStatus >= 200 && r.HTTPStatus < 300 {
		json.Unmarshal(r.Body, &inst)
	}
	return r, inst
}

// InstancePublicIP fetches the public IP of a freshly created instance (best effort).
func (c *OCIClient) InstancePublicIP(instanceID string) (string, error) {
	q := url.Values{
		"compartmentId": {c.Tenancy},
		"instanceId":    {instanceID},
	}
	r := c.do(c.iaas(), "GET", "/20160918/vnicAttachments", q, nil)
	if r.Err != nil || r.HTTPStatus != 200 {
		return "", fmt.Errorf("vnicAttachments: %v", r.Err)
	}
	var atts []struct {
		VnicID string `json:"vnicId"`
		State  string `json:"lifecycleState"`
	}
	json.Unmarshal(r.Body, &atts)
	for _, a := range atts {
		if a.VnicID == "" {
			continue
		}
		rv := c.do(c.iaas(), "GET", "/20160918/vnics/"+url.PathEscape(a.VnicID), nil, nil)
		if rv.Err != nil || rv.HTTPStatus != 200 {
			continue
		}
		var v struct {
			PublicIP string `json:"publicIp"`
			PrivateIP string `json:"privateIp"`
		}
		json.Unmarshal(rv.Body, &v)
		if v.PublicIP != "" {
			return v.PublicIP, nil
		}
		if v.PrivateIP != "" {
			return v.PrivateIP + " (private)", nil
		}
	}
	return "", fmt.Errorf("IP not yet available")
}
