package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is stored entirely as a KEY=VALUE env file (0600).
type Config struct {
	// --- OCI credentials ---
	TenancyOCID    string
	UserOCID       string
	Fingerprint    string
	Region         string
	PrivateKeyPath string

	// --- instance target ---
	ADName         string
	SubnetOCID     string
	ImageOCID      string
	OSName         string
	OSVersion      string
	Shape          string
	OCPUs          int // 0 = auto (limit API)
	MemoryGB       int // 0 = auto
	BootVolumeGB   int
	DisplayName    string
	AssignPublicIP bool
	SSHKeys        string

	// --- hunting behavior ---
	IntervalBaseSecs   int
	IntervalJitterSecs int
	BackoffFactor      float64
	BackoffMaxSecs     int
	MaxInstances       int
	BatteryMinPct      int
	WiFiOnly           bool

	// --- webui ---
	WebUIPort int
	AuthToken string

	// --- notifications ---
	TelegramEnabled bool
	TelegramToken   string
	TelegramChatID  string
	TelegramFoundTemplate string
	TelegramSummaryTemplate string
	TelegramSummaryIntervalMins int

	extra []string // unrecognized lines, preserved so they are not lost
}

func defaultConfig() *Config {
	return &Config{
		Region:           "",
		PrivateKeyPath:   "oci_api_key.pem", // relatif terhadap dir config
		OSName:           "Canonical Ubuntu",
		OSVersion:        "24.04",
		Shape:            "VM.Standard.A1.Flex",
		OCPUs:            0,
		MemoryGB:         0,
		BootVolumeGB:     50,
		DisplayName:      "vm-a1",
		AssignPublicIP:   true,
		SSHKeys:          "",
		SubnetOCID:       "",
		IntervalBaseSecs: 120,
		IntervalJitterSecs: 40,
		BackoffFactor:    1.5,
		BackoffMaxSecs:   600,
		MaxInstances:     1,
		BatteryMinPct:    15,
		WiFiOnly:         true,
		WebUIPort:        8080,
		TelegramFoundTemplate: "Oracle A1 secured!\nName: {name}\nIP: {ip}\nLogin: ssh ubuntu@{ip}\nAttempts: {sum}\nOCID: {ocid}\nTime: {time}",
		TelegramSummaryTemplate: "Oracle A1 Hunter summary\nAttempts: {sum} | No capacity: {capacity} | 429: {rate_limited} | Errors: {errors}\nUptime: {uptime} | Battery: {battery}%\nRegion: {region} | Target: {target}\nTime: {time}",
		TelegramSummaryIntervalMins: 0,
	}
}

// Multi-line values (message templates) are stored in the env-style config
// file with escaped newlines, since the file format is line-based.
func escNewlines(s string) string   { return strings.ReplaceAll(s, "\n", "\\n") }
func unescNewlines(s string) string { return strings.ReplaceAll(s, "\\n", "\n") }

func genToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// PrivateKeyPathAbs returns the absolute path of the private key.
func (c *Config) PrivateKeyPathAbs(baseDir string) string {
	p := c.PrivateKeyPath
	if p == "" {
		p = "oci_api_key.pem"
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(baseDir, p)
}

func LoadConfig(path string) (*Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg.AuthToken = genToken()
			if err := SaveConfig(path, cfg); err != nil {
				return nil, err
			}
			return cfg, nil
		}
		return nil, err
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			cfg.extra = append(cfg.extra, line)
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		v = strings.Trim(v, `"'`)
		switch k {
		case "OCI_TENANCY_OCID":
			cfg.TenancyOCID = v
		case "OCI_USER_OCID":
			cfg.UserOCID = v
		case "OCI_FINGERPRINT":
			cfg.Fingerprint = v
		case "OCI_REGION":
			cfg.Region = v
		case "OCI_PRIVATE_KEY_PATH":
			cfg.PrivateKeyPath = v
		case "AD_NAME":
			cfg.ADName = v
		case "SUBNET_OCID":
			cfg.SubnetOCID = v
		case "IMAGE_OCID":
			cfg.ImageOCID = v
		case "OS_NAME":
			cfg.OSName = v
		case "OS_VERSION":
			cfg.OSVersion = v
		case "SHAPE":
			cfg.Shape = v
		case "OCPUS":
			cfg.OCPUs, _ = strconv.Atoi(v)
		case "MEMORY_GB":
			cfg.MemoryGB, _ = strconv.Atoi(v)
		case "BOOT_VOLUME_GB":
			cfg.BootVolumeGB, _ = strconv.Atoi(v)
		case "DISPLAY_NAME":
			cfg.DisplayName = v
		case "ASSIGN_PUBLIC_IP":
			cfg.AssignPublicIP = truthy(v)
		case "SSH_AUTHORIZED_KEYS":
			cfg.SSHKeys = v
		case "INTERVAL_BASE_SECS":
			cfg.IntervalBaseSecs, _ = strconv.Atoi(v)
		case "INTERVAL_JITTER_SECS":
			cfg.IntervalJitterSecs, _ = strconv.Atoi(v)
		case "BACKOFF_FACTOR":
			cfg.BackoffFactor, _ = strconv.ParseFloat(v, 64)
		case "BACKOFF_MAX_SECS":
			cfg.BackoffMaxSecs, _ = strconv.Atoi(v)
		case "MAX_INSTANCES":
			cfg.MaxInstances, _ = strconv.Atoi(v)
		case "BATTERY_MIN_PCT":
			cfg.BatteryMinPct, _ = strconv.Atoi(v)
		case "WIFI_ONLY":
			cfg.WiFiOnly = truthy(v)
		case "WEBUI_PORT":
			cfg.WebUIPort, _ = strconv.Atoi(v)
		case "AUTH_TOKEN":
			cfg.AuthToken = v
		case "TELEGRAM_ENABLED":
			cfg.TelegramEnabled = truthy(v)
		case "TELEGRAM_BOT_TOKEN":
			cfg.TelegramToken = v
		case "TELEGRAM_CHAT_ID":
			cfg.TelegramChatID = v
		case "TELEGRAM_FOUND_TEMPLATE":
			cfg.TelegramFoundTemplate = unescNewlines(v)
		case "TELEGRAM_SUMMARY_TEMPLATE":
			cfg.TelegramSummaryTemplate = unescNewlines(v)
		case "TELEGRAM_SUMMARY_INTERVAL_MINS":
			cfg.TelegramSummaryIntervalMins, _ = strconv.Atoi(v)
		default:
			cfg.extra = append(cfg.extra, line)
		}
	}
	if cfg.AuthToken == "" {
		cfg.AuthToken = genToken()
		_ = SaveConfig(path, cfg)
	}
	return cfg, nil
}

func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func SaveConfig(path string, c *Config) error {
	var b strings.Builder
	b.WriteString("# oracle-hunter — configuration (keep permissions 0600)\n")
	b.WriteString("# OCI credentials\n")
	b.WriteString("OCI_TENANCY_OCID=" + c.TenancyOCID + "\n")
	b.WriteString("OCI_USER_OCID=" + c.UserOCID + "\n")
	b.WriteString("OCI_FINGERPRINT=" + c.Fingerprint + "\n")
	b.WriteString("OCI_REGION=" + c.Region + "\n")
	b.WriteString("OCI_PRIVATE_KEY_PATH=" + c.PrivateKeyPath + "\n")
	b.WriteString("\n# Instance target (empty = auto-discover)\n")
	b.WriteString("AD_NAME=" + c.ADName + "\n")
	b.WriteString("SUBNET_OCID=" + c.SubnetOCID + "\n")
	b.WriteString("IMAGE_OCID=" + c.ImageOCID + "\n")
	b.WriteString("OS_NAME=" + c.OSName + "\n")
	b.WriteString("OS_VERSION=" + c.OSVersion + "\n")
	b.WriteString("SHAPE=" + c.Shape + "\n")
	b.WriteString(fmt.Sprintf("OCPUS=%d\n", c.OCPUs))
	b.WriteString(fmt.Sprintf("MEMORY_GB=%d\n", c.MemoryGB))
	b.WriteString(fmt.Sprintf("BOOT_VOLUME_GB=%d\n", c.BootVolumeGB))
	b.WriteString("DISPLAY_NAME=" + c.DisplayName + "\n")
	b.WriteString("ASSIGN_PUBLIC_IP=" + boolStr(c.AssignPublicIP) + "\n")
	b.WriteString("SSH_AUTHORIZED_KEYS=" + c.SSHKeys + "\n")
	b.WriteString("\n# Hunting behavior\n")
	b.WriteString(fmt.Sprintf("INTERVAL_BASE_SECS=%d\n", c.IntervalBaseSecs))
	b.WriteString(fmt.Sprintf("INTERVAL_JITTER_SECS=%d\n", c.IntervalJitterSecs))
	b.WriteString(fmt.Sprintf("BACKOFF_FACTOR=%g\n", c.BackoffFactor))
	b.WriteString(fmt.Sprintf("BACKOFF_MAX_SECS=%d\n", c.BackoffMaxSecs))
	b.WriteString(fmt.Sprintf("MAX_INSTANCES=%d\n", c.MaxInstances))
	b.WriteString(fmt.Sprintf("BATTERY_MIN_PCT=%d\n", c.BatteryMinPct))
	b.WriteString("WIFI_ONLY=" + boolStr(c.WiFiOnly) + "\n")
	b.WriteString("\n# WebUI\n")
	b.WriteString(fmt.Sprintf("WEBUI_PORT=%d\n", c.WebUIPort))
	b.WriteString("AUTH_TOKEN=" + c.AuthToken + "\n")
	b.WriteString("\n# Notifications (optional)\n")
	b.WriteString("TELEGRAM_ENABLED=" + boolStr(c.TelegramEnabled) + "\n")
	b.WriteString("TELEGRAM_BOT_TOKEN=" + c.TelegramToken + "\n")
	b.WriteString("TELEGRAM_CHAT_ID=" + c.TelegramChatID + "\n")
	b.WriteString("TELEGRAM_SUMMARY_INTERVAL_MINS=" + strconv.Itoa(c.TelegramSummaryIntervalMins) + "\n")
	b.WriteString("TELEGRAM_FOUND_TEMPLATE=" + escNewlines(c.TelegramFoundTemplate) + "\n")
	b.WriteString("TELEGRAM_SUMMARY_TEMPLATE=" + escNewlines(c.TelegramSummaryTemplate) + "\n")
	for _, l := range c.extra {
		b.WriteString(l + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	os.Chmod(path, 0o600)
	return nil
}

// MissingCreds returns the list of missing credential fields.
func (c *Config) MissingCreds(baseDir string) []string {
	var m []string
	if c.TenancyOCID == "" {
		m = append(m, "OCI_TENANCY_OCID")
	}
	if c.UserOCID == "" {
		m = append(m, "OCI_USER_OCID")
	}
	if c.Fingerprint == "" {
		m = append(m, "OCI_FINGERPRINT")
	}
	if c.Region == "" {
		m = append(m, "OCI_REGION")
	}
	if _, err := os.Stat(c.PrivateKeyPathAbs(baseDir)); err != nil {
		m = append(m, "OCI_PRIVATE_KEY (file "+c.PrivateKeyPathAbs(baseDir)+" not found)")
	}
	return m
}
