package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed static
var staticFS embed.FS

type WebUI struct {
	h      *Hunter
	log    *Logger
	srv    *http.Server
	static fs.FS
}

func NewWebUI(h *Hunter, log *Logger) *WebUI {
	sub, _ := fs.Sub(staticFS, "static")
	w := &WebUI{h: h, log: log, static: sub}

	mux := http.NewServeMux()
	mux.HandleFunc("/", w.handleIndex)
	mux.HandleFunc("/api/status", w.auth(w.handleStatus))
	mux.HandleFunc("/api/control", w.auth(w.handleControl))
	mux.HandleFunc("/api/logs", w.auth(w.handleLogs))
	mux.HandleFunc("/api/logs/stream", w.auth(w.handleLogStream))
	mux.HandleFunc("/api/config", w.auth(w.handleConfig))
	mux.HandleFunc("/api/telegram/test", w.auth(w.handleTelegramTest))

	port := 8080
	h.mu.Lock()
	if h.cfg != nil && h.cfg.WebUIPort > 0 {
		port = h.cfg.WebUIPort
	}
	h.mu.Unlock()

	w.srv = &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // SSE needs long-lived connections
	}
	return w
}

func (w *WebUI) Start() {
	ln, err := net.Listen("tcp", w.srv.Addr)
	if err != nil {
		w.log.Error("WebUI listen failed: " + err.Error())
		return
	}
	w.log.Info(fmt.Sprintf("WebUI listening on %s", ln.Addr().String()))
	ips := lanIPs()
	if len(ips) > 0 {
		w.log.Info("candidate URLs: " + strings.Join(ips, ", "))
	}
	if err := w.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		w.log.Error("WebUI serve: " + err.Error())
	}
}

func lanIPs() []string {
	var out []string
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				ip4 := ipn.IP.To4()
				if ip4 != nil {
					out = append(out, "http://"+ip4.String()+":8080")
				}
			}
		}
	}
	return out
}

// ---------- auth ----------

func (w *WebUI) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		token := req.Header.Get("X-Auth-Token")
		if token == "" {
			token = req.URL.Query().Get("token")
		}
		w.h.mu.Lock()
		want := ""
		if w.h.cfg != nil {
			want = w.h.cfg.AuthToken
		}
		w.h.mu.Unlock()
		if want == "" || subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusUnauthorized)
			rw.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		next(rw, req)
	}
}

// ---------- handlers ----------

func (w *WebUI) handleIndex(rw http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(rw, req)
		return
	}
	data, err := fs.ReadFile(w.static, "index.html")
	if err != nil {
		rw.WriteHeader(500)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Write(data)
}

func (w *WebUI) handleStatus(rw http.ResponseWriter, req *http.Request) {
	h := w.h
	h.mu.Lock()
	st := h.stats
	state := h.state
	found := h.found
	rt := h.rt
	recent := append([]Attempt(nil), h.recent...)
	running := h.running
	cfg := h.cfg
	h.mu.Unlock()

	wifi, online := netState()
	resp := map[string]any{
		"version":       version,
		"state":         state,
		"running":       running,
		"stats":         st,
		"recent":        recent,
		"found":         found,
		"target":        rt,
		"battery":       batteryPct(),
		"memoryMB":      rssMB(),
		"wifi":          wifi,
		"online":        online,
		"now":           time.Now(),
		"missing":       cfg.MissingCreds(h.baseDir),
		"credsComplete": len(cfg.MissingCreds(h.baseDir)) == 0,
		"settings": map[string]any{
			"intervalBase":   cfg.IntervalBaseSecs,
			"intervalJitter": cfg.IntervalJitterSecs,
			"maxInstances":   cfg.MaxInstances,
			"batteryMinPct":  cfg.BatteryMinPct,
			"wifiOnly":       cfg.WiFiOnly,
		},
	}
	writeJSON(rw, resp)
}

func (w *WebUI) handleControl(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		rw.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Action == "" {
		writeJSONErr(rw, 400, "invalid body")
		return
	}
	h := w.h
	switch body.Action {
	case "start":
		if !h.CredsComplete() {
			writeJSONErr(rw, 400, "credentials incomplete: "+strings.Join(h.cfg.MissingCreds(h.baseDir), ", "))
			return
		}
		if h.state == StateFound {
			writeJSONErr(rw, 400, "instance already secured — hunter stopped by design")
			return
		}
		h.Start()
		w.log.Info("WebUI: start")
	case "stop":
		h.Stop()
		w.log.Info("WebUI: stop")
	case "restart":
		h.Restart()
		w.log.Info("WebUI: restart")
	case "checknow":
		if !h.CredsComplete() {
			writeJSONErr(rw, 400, "credentials incomplete")
			return
		}
		if !h.IsRunning() {
			h.Start()
		}
		h.SendControl("checknow")
		w.log.Info("WebUI: check now")
	default:
		writeJSONErr(rw, 400, "unknown action: "+body.Action)
		return
	}
	writeJSON(rw, map[string]any{"ok": true})
}

func (w *WebUI) handleLogs(rw http.ResponseWriter, req *http.Request) {
	n := 300
	if v := req.URL.Query().Get("n"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			n = p
		}
	}
	writeJSON(rw, map[string]any{"lines": w.log.Snapshot(n)})
}

func (w *WebUI) handleLogStream(rw http.ResponseWriter, req *http.Request) {
	fl, ok := rw.(http.Flusher)
	if !ok {
		writeJSONErr(rw, 500, "streaming unsupported")
		return
	}
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("X-Accel-Buffering", "no")

	ch := w.log.Subscribe()
	defer w.log.Unsubscribe(ch)

	for _, l := range w.log.Snapshot(50) {
		fmt.Fprintf(rw, "data: %s\n\n", l)
	}
	fl.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-req.Context().Done():
			return
		case line := <-ch:
			fmt.Fprintf(rw, "data: %s\n\n", line)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(rw, ": ping\n\n")
			fl.Flush()
		}
	}
}

// handleTelegramTest renders the summary template with live stats and sends
// it through the configured bot — used by the "Send test" button.
func (w *WebUI) handleTelegramTest(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		rw.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	h := w.h
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	if !cfg.TelegramEnabled || cfg.TelegramToken == "" || cfg.TelegramChatID == "" {
		writeJSONErr(rw, 400, "configure Telegram first: enable it and fill the bot token and chat ID")
		return
	}
	msg := h.renderTemplate(cfg.TelegramSummaryTemplate, nil)
	if err := h.sendTelegram(msg); err != nil {
		writeJSONErr(rw, 502, err.Error())
		return
	}
	writeJSON(rw, map[string]any{"ok": true, "message": msg})
}

// ---------- config ----------

type Field struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Group  string `json:"group"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
	Type   string `json:"type"` // text | number | bool | textarea
	Hint   string `json:"hint,omitempty"`
	Info   string `json:"info,omitempty"` // tooltip text
}

func maskVal(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 8 {
		return "***"
	}
	return v[:4] + "…" + v[len(v)-4:]
}

const tplInfoFound = "Message sent when an instance is secured. Placeholders: {name} {ip} {ocid} {sum} {time} {region} {target} {uptime} {battery}."

const tplInfoSummary = "Periodic status message. Placeholders: {sum} {capacity} {rate_limited} {errors} {uptime} {battery} {region} {target} {time}."

func (w *WebUI) fields(cfg *Config) []Field {
	return []Field{
		{Key: "OCI_TENANCY_OCID", Label: "Tenancy OCID", Group: "OCI Credentials", Value: cfg.TenancyOCID, Type: "text", Hint: "ocid1.tenancy.oc1…",
			Info: "Your Oracle Cloud tenancy identifier. Find it in the Console under Profile → Tenancy."},
		{Key: "OCI_USER_OCID", Label: "User OCID", Group: "OCI Credentials", Value: cfg.UserOCID, Type: "text", Hint: "ocid1.user.oc1…",
			Info: "Your OCI user identifier. Console → Profile → User."},
		{Key: "OCI_FINGERPRINT", Label: "API Key Fingerprint", Group: "OCI Credentials", Value: cfg.Fingerprint, Secret: true, Type: "text", Hint: "aa:bb:cc:…",
			Info: "Fingerprint of your API key (MD5 of the public key, shown by the Console when you add it). Console → Profile → API keys."},
		{Key: "OCI_REGION", Label: "Region", Group: "OCI Credentials", Value: cfg.Region, Type: "text",
			Info: "Your home region identifier, e.g. ap-batam-1. All API calls go to this region."},
		{Key: "OCI_PRIVATE_KEY_PATH", Label: "Private Key Path", Group: "OCI Credentials", Value: cfg.PrivateKeyPath, Type: "text",
			Info: "Path to the API signing private key (PEM). Relative paths resolve against the config directory."},

		{Key: "AD_NAME", Label: "Availability Domain", Group: "Instance Target", Value: cfg.ADName, Type: "text", Hint: "empty = auto",
			Info: "Availability domain to launch in. Leave empty to auto-discover the first one."},
		{Key: "SUBNET_OCID", Label: "Subnet OCID", Group: "Instance Target", Value: cfg.SubnetOCID, Type: "text",
			Info: "Subnet for the instance VNIC. Find it under Networking → Virtual Cloud Networks."},
		{Key: "IMAGE_OCID", Label: "Image OCID", Group: "Instance Target", Value: cfg.ImageOCID, Type: "text", Hint: "empty = auto-pick latest",
			Info: "Custom image OCID. Leave empty to auto-pick the newest image matching OS name/version below."},
		{Key: "OS_NAME", Label: "OS Name", Group: "Instance Target", Value: cfg.OSName, Type: "text",
			Info: "Operating system name used for image auto-discovery, e.g. Canonical Ubuntu."},
		{Key: "OS_VERSION", Label: "OS Version", Group: "Instance Target", Value: cfg.OSVersion, Type: "text",
			Info: "Operating system version used for image auto-discovery, e.g. 24.04."},
		{Key: "SHAPE", Label: "Shape", Group: "Instance Target", Value: cfg.Shape, Type: "text",
			Info: "Instance shape. VM.Standard.A1.Flex is the Always Free ARM shape."},
		{Key: "OCPUS", Label: "OCPUs (0 = auto)", Group: "Instance Target", Value: strconv.Itoa(cfg.OCPUs), Type: "number",
			Info: "CPU count. 0 = automatic: use the largest allocation your tenancy still allows."},
		{Key: "MEMORY_GB", Label: "Memory GB (0 = auto)", Group: "Instance Target", Value: strconv.Itoa(cfg.MemoryGB), Type: "number",
			Info: "Memory in GB. 0 = automatic, paired with the OCPU setting."},
		{Key: "BOOT_VOLUME_GB", Label: "Boot Volume GB", Group: "Instance Target", Value: strconv.Itoa(cfg.BootVolumeGB), Type: "number",
			Info: "Boot volume size. Always Free includes up to 200 GB total; 50 GB keeps room for more volumes."},
		{Key: "DISPLAY_NAME", Label: "Instance Name", Group: "Instance Target", Value: cfg.DisplayName, Type: "text",
			Info: "Display name of the instance in the Console."},
		{Key: "ASSIGN_PUBLIC_IP", Label: "Assign Public IP", Group: "Instance Target", Value: boolStr(cfg.AssignPublicIP), Type: "bool",
			Info: "Automatically assign a public IPv4 to the instance so it is reachable over the internet."},
		{Key: "SSH_AUTHORIZED_KEYS", Label: "SSH Public Keys", Group: "Instance Target", Value: cfg.SSHKeys, Type: "text",
			Info: "Public key(s) injected into the instance for SSH login (authorized_keys)."},

		{Key: "INTERVAL_BASE_SECS", Label: "Base interval (seconds)", Group: "Hunting Behavior", Value: strconv.Itoa(cfg.IntervalBaseSecs), Type: "number",
			Info: "Delay between launch attempts. 120 is proven safe; 90 or less tends to trigger 429 rate limiting."},
		{Key: "INTERVAL_JITTER_SECS", Label: "Random jitter (seconds)", Group: "Hunting Behavior", Value: strconv.Itoa(cfg.IntervalJitterSecs), Type: "number",
			Info: "Extra random delay on top of the base interval to avoid bursts. Set 0 for an exact interval."},
		{Key: "BACKOFF_FACTOR", Label: "429 backoff factor", Group: "Hunting Behavior", Value: fmt.Sprintf("%g", cfg.BackoffFactor), Type: "number",
			Info: "Multiplier applied to the interval after each 429 rate-limit response. 1.0 disables backoff."},
		{Key: "BACKOFF_MAX_SECS", Label: "Max backoff (seconds)", Group: "Hunting Behavior", Value: strconv.Itoa(cfg.BackoffMaxSecs), Type: "number",
			Info: "Upper bound for the backoff delay."},
		{Key: "MAX_INSTANCES", Label: "Max A1 instances", Group: "Hunting Behavior", Value: strconv.Itoa(cfg.MaxInstances), Type: "number",
			Info: "Stop hunting once this many A1 instances exist. Always Free allows up to 4 OCPU/24 GB total."},
		{Key: "BATTERY_MIN_PCT", Label: "Pause below battery (%)", Group: "Hunting Behavior", Value: strconv.Itoa(cfg.BatteryMinPct), Type: "number",
			Info: "Pause hunting when battery drops below this percentage (0 disables the pause)."},
		{Key: "WIFI_ONLY", Label: "Wi-Fi only", Group: "Hunting Behavior", Value: boolStr(cfg.WiFiOnly), Type: "bool",
			Info: "Only attempt launches while connected to Wi-Fi, saving mobile data."},

		{Key: "WEBUI_PORT", Label: "WebUI port", Group: "WebUI", Value: strconv.Itoa(cfg.WebUIPort), Type: "number",
			Info: "TCP port the built-in web interface listens on. Requires a service restart to apply."},
		{Key: "AUTH_TOKEN", Label: "Auth token", Group: "WebUI", Value: cfg.AuthToken, Secret: true, Type: "text",
			Info: "Token required to open the WebUI. Change it if you exposed the port to others."},

		{Key: "TELEGRAM_ENABLED", Label: "Telegram notifications", Group: "Notifications", Value: boolStr(cfg.TelegramEnabled), Type: "bool",
			Info: "Enable Telegram bot notifications (found alert and optional periodic summary)."},
		{Key: "TELEGRAM_BOT_TOKEN", Label: "Bot token", Group: "Notifications", Value: cfg.TelegramToken, Secret: true, Type: "text",
			Info: "Token from @BotFather after creating your bot."},
		{Key: "TELEGRAM_CHAT_ID", Label: "Chat ID", Group: "Notifications", Value: cfg.TelegramChatID, Secret: true, Type: "text",
			Info: "Your chat id: send a message to your bot, then read it via api.telegram.org/bot<token>/getUpdates."},
		{Key: "TELEGRAM_SUMMARY_INTERVAL_MINS", Label: "Summary interval (minutes)", Group: "Notifications", Value: strconv.Itoa(cfg.TelegramSummaryIntervalMins), Type: "number",
			Info: "Send a status summary every N minutes. 0 disables. Minimum enforced: 15."},
		{Key: "TELEGRAM_FOUND_TEMPLATE", Label: "Found message template", Group: "Notifications", Value: cfg.TelegramFoundTemplate, Type: "textarea",
			Info: tplInfoFound},
		{Key: "TELEGRAM_SUMMARY_TEMPLATE", Label: "Summary message template", Group: "Notifications", Value: cfg.TelegramSummaryTemplate, Type: "textarea",
			Info: tplInfoSummary},
	}
}

func (w *WebUI) handleConfig(rw http.ResponseWriter, req *http.Request) {
	h := w.h
	switch req.Method {
	case http.MethodGet:
		h.mu.Lock()
		cfg := h.cfg
		h.mu.Unlock()
		fields := w.fields(cfg)
		for i := range fields {
			if fields[i].Secret {
				fields[i].Value = maskVal(fields[i].Value)
			}
		}
		writeJSON(rw, map[string]any{"fields": fields})
	case http.MethodPost:
		var in map[string]string
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(rw, 400, "invalid JSON")
			return
		}
		h.mu.Lock()
		nc := *h.cfg // copy
		h.mu.Unlock()
		applyFields(&nc, in)
		if err := SaveConfig(h.baseDir+"/config.env", &nc); err != nil {
			writeJSONErr(rw, 500, "save failed: "+err.Error())
			return
		}
		h.ApplyConfig(&nc)
		writeJSON(rw, map[string]any{"ok": true})
	default:
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func applyFields(c *Config, in map[string]string) {
	get := func(k string) (string, bool) { v, ok := in[k]; return strings.TrimSpace(v), ok }
	if v, ok := get("OCI_TENANCY_OCID"); ok { c.TenancyOCID = v }
	if v, ok := get("OCI_USER_OCID"); ok { c.UserOCID = v }
	if v, ok := get("OCI_FINGERPRINT"); ok && !strings.Contains(v, "…") && v != "***" { c.Fingerprint = v }
	if v, ok := get("OCI_REGION"); ok { c.Region = v }
	if v, ok := get("OCI_PRIVATE_KEY_PATH"); ok { c.PrivateKeyPath = v }
	if v, ok := get("AD_NAME"); ok { c.ADName = v }
	if v, ok := get("SUBNET_OCID"); ok { c.SubnetOCID = v }
	if v, ok := get("IMAGE_OCID"); ok { c.ImageOCID = v }
	if v, ok := get("OS_NAME"); ok { c.OSName = v }
	if v, ok := get("OS_VERSION"); ok { c.OSVersion = v }
	if v, ok := get("SHAPE"); ok { c.Shape = v }
	if v, ok := get("OCPUS"); ok { c.OCPUs, _ = strconv.Atoi(v) }
	if v, ok := get("MEMORY_GB"); ok { c.MemoryGB, _ = strconv.Atoi(v) }
	if v, ok := get("BOOT_VOLUME_GB"); ok { c.BootVolumeGB, _ = strconv.Atoi(v) }
	if v, ok := get("DISPLAY_NAME"); ok { c.DisplayName = v }
	if v, ok := get("ASSIGN_PUBLIC_IP"); ok { c.AssignPublicIP = truthy(v) }
	if v, ok := get("SSH_AUTHORIZED_KEYS"); ok { c.SSHKeys = v }
	if v, ok := get("INTERVAL_BASE_SECS"); ok { if n, e := strconv.Atoi(v); e == nil && n >= 20 { c.IntervalBaseSecs = n } }
	if v, ok := get("INTERVAL_JITTER_SECS"); ok { if n, e := strconv.Atoi(v); e == nil && n >= 0 { c.IntervalJitterSecs = n } }
	if v, ok := get("BACKOFF_FACTOR"); ok { if f, e := strconv.ParseFloat(v, 64); e == nil && f >= 1 { c.BackoffFactor = f } }
	if v, ok := get("BACKOFF_MAX_SECS"); ok { if n, e := strconv.Atoi(v); e == nil && n >= 60 { c.BackoffMaxSecs = n } }
	if v, ok := get("MAX_INSTANCES"); ok { if n, e := strconv.Atoi(v); e == nil && n >= 1 { c.MaxInstances = n } }
	if v, ok := get("BATTERY_MIN_PCT"); ok { if n, e := strconv.Atoi(v); e == nil && n >= 0 && n <= 50 { c.BatteryMinPct = n } }
	if v, ok := get("WIFI_ONLY"); ok { c.WiFiOnly = truthy(v) }
	if v, ok := get("WEBUI_PORT"); ok { if n, e := strconv.Atoi(v); e == nil && n > 0 && n < 65536 { c.WebUIPort = n } }
	if v, ok := get("AUTH_TOKEN"); ok && !strings.Contains(v, "…") && v != "***" && v != "" { c.AuthToken = v }
	if v, ok := get("TELEGRAM_ENABLED"); ok { c.TelegramEnabled = truthy(v) }
	if v, ok := get("TELEGRAM_BOT_TOKEN"); ok && !strings.Contains(v, "…") && v != "***" { c.TelegramToken = v }
	if v, ok := get("TELEGRAM_CHAT_ID"); ok && !strings.Contains(v, "…") && v != "***" { c.TelegramChatID = v }
	if v, ok := get("TELEGRAM_SUMMARY_INTERVAL_MINS"); ok { if n, e := strconv.Atoi(v); e == nil && n >= 0 { c.TelegramSummaryIntervalMins = n } }
	if v, ok := get("TELEGRAM_FOUND_TEMPLATE"); ok && !strings.Contains(v, "***") { c.TelegramFoundTemplate = v }
	if v, ok := get("TELEGRAM_SUMMARY_TEMPLATE"); ok && !strings.Contains(v, "***") { c.TelegramSummaryTemplate = v }
}

// ---------- util ----------

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(v)
}

func writeJSONErr(rw http.ResponseWriter, code int, msg string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	json.NewEncoder(rw).Encode(map[string]any{"error": msg})
}
