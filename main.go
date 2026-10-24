package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	// Embedded IANA tzdata — Android has no /usr/share/zoneinfo; without
	// this the TZ env var is ignored and logs fall back to UTC.
	_ "time/tzdata"
)

const version = "1.0.1"

// init reads the device timezone straight from getprop so logs use local
// time. time.Local is set explicitly because its eager initialization
// happens before the tzdata embedded hook is registered.
func init() {
	tz := os.Getenv("TZ")
	if tz == "" {
		if out, err := exec.Command("/system/bin/getprop", "persist.sys.timezone").Output(); err == nil {
			tz = strings.TrimSpace(string(out))
		}
	}
	if tz != "" {
		os.Setenv("TZ", tz)
		if loc, err := time.LoadLocation(tz); err == nil {
			time.Local = loc
		}
	}
}

func main() {
	cfgDir := flag.String("config", "/data/adb/oracle-hunter", "config directory")
	once := flag.Bool("once", false, "run a single check and exit")
	validate := flag.Bool("validate", false, "validate credentials & discovery, then exit")
	flag.Parse()

	abs, err := filepath.Abs(*cfgDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "path error:", err)
		os.Exit(1)
	}
	os.MkdirAll(filepath.Join(abs, "logs"), 0o755)

	cfg, err := LoadConfig(filepath.Join(abs, "config.env"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	logger := NewLogger(filepath.Join(abs, "logs", "hunter.log"), 1000)
	h := NewHunter(cfg, logger, abs)

	if *validate {
		if !h.Validate() {
			os.Exit(1)
		}
		return
	}
	if *once {
		h.RunOnce()
		return
	}

	web := NewWebUI(h, logger)
	go web.Start()

	logger.Log("INFO", fmt.Sprintf("oracle-hunter v%s running — WebUI at http://<phone-ip>:%d/ (token in config.env AUTH_TOKEN)", version, cfg.WebUIPort))

	// The loop always runs; the NEEDS_CONFIG guard inside it waits for
	// credentials, so as soon as config is completed via the WebUI the
	// hunt resumes without a service restart.
	h.Start()
	if !h.CredsComplete() {
		logger.Log("WARN", "OCI credentials incomplete — complete them in the WebUI Config tab. Hunter standing by.")
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	h.Stop()
}
