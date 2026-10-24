package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type HunterState string

const (
	StateNeedsConfig    HunterState = "NEEDS_CONFIG"
	StateStopped        HunterState = "STOPPED"
	StateHunting        HunterState = "HUNTING"
	StateBackoff        HunterState = "BACKOFF"
	StatePausedBattery  HunterState = "PAUSED_BATTERY"
	StatePausedOffline  HunterState = "PAUSED_OFFLINE"
	StateFound          HunterState = "FOUND"
	StateComplete       HunterState = "COMPLETE" // enough instances per MAX_INSTANCES
	StateError          HunterState = "ERROR"
)

type Attempt struct {
	At     time.Time `json:"at"`
	Ms     int64     `json:"ms"`
	Status int       `json:"status"`
	Kind   string    `json:"kind"` // CAPACITY / RATE_LIMIT / FOUND / ERROR / AUTH / QUOTA
}

type Stats struct {
	Checks        int64     `json:"checks"`
	Errors        int64     `json:"errors"`
	RateLimited   int64     `json:"rateLimited"`
	CapacityMiss  int64     `json:"capacityMiss"`
	StartedAt     time.Time `json:"startedAt"`
	LastCheckAt   time.Time `json:"lastCheckAt"`
	LastResult    string    `json:"lastResult"`
	NextCheckAt   time.Time `json:"nextCheckAt"`
	CurrentDelay  int       `json:"currentDelaySecs"`
}

type InstanceInfo struct {
	OCID   string    `json:"ocid"`
	Name   string    `json:"name"`
	IP     string    `json:"ip"`
	FoundAt time.Time `json:"foundAt"`
}

type RuntimeInfo struct {
	AD        string `json:"ad"`
	Subnet    string `json:"subnet"`
	ImageID   string `json:"imageId"`
	ImageName string `json:"imageName"`
	TargetCPU int    `json:"targetOcpus"`
	TargetMem int    `json:"targetMemGb"`
	Tenancy   string `json:"tenancyName"`
	Discovered bool  `json:"discovered"`
}

type Hunter struct {
	mu      sync.Mutex
	cfg     *Config
	baseDir string
	log     *Logger

	state  HunterState
	stats  Stats
	recent []Attempt
	found  *InstanceInfo
	rt     RuntimeInfo

	oci     *OCIClient
	stopCh  chan struct{}
	control chan string
	running bool
	gen     uint64

	wakeWarned bool
	rtcWarned  bool

	lastSummary time.Time
}

var currentHunter *Hunter

func NewHunter(cfg *Config, log *Logger, baseDir string) *Hunter {
	statsPath = filepath.Join(baseDir, "stats.json")
	h := &Hunter{cfg: cfg, log: log, baseDir: baseDir, state: StateStopped}
	h.stats.StartedAt = time.Now()
	h.loadStats() // counters/uptime/lastSummary continue across reboots
	currentHunter = h
	return h
}

// note — catat lastResult secara thread-safe.
func (h *Hunter) note(msg string) {
	h.mu.Lock()
	h.stats.Note(msg)
	h.mu.Unlock()
}

// ---------- state & accessors ----------

func (h *Hunter) SetState(s HunterState) {
	h.mu.Lock()
	h.state = s
	h.mu.Unlock()
}

func (h *Hunter) CredsComplete() bool {
	return len(h.cfg.MissingCreds(h.baseDir)) == 0
}

func (h *Hunter) IsRunning() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}

// ---------- lifecycle ----------

func (h *Hunter) Start() {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return
	}
	h.running = true
	h.gen++
	h.stopCh = make(chan struct{})
	h.control = make(chan string, 16)
	gen := h.gen
	h.mu.Unlock()
	go h.run(gen)
}

// staleGen reports whether this loop is obsolete (a newer Start() has
// run) — e.g. an old loop caught mid-HTTP during Restart that never saw
// the closed old stopCh. Such loops must terminate themselves.
func (h *Hunter) staleGen(gen uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gen != gen
}

func (h *Hunter) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.running {
		return
	}
	h.running = false
	close(h.stopCh)
	if h.state != StateFound && h.state != StateComplete {
		h.state = StateStopped
	}
}

func (h *Hunter) Restart() {
	h.Stop()
	time.Sleep(300 * time.Millisecond)
	h.Start()
}

func (h *Hunter) SendControl(c string) {
	h.mu.Lock()
	ch := h.control
	h.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- c:
	default:
	}
}

// ---------- main loop ----------

func (h *Hunter) run(gen uint64) {
	defer func() {
		if r := recover(); r != nil {
			h.log.Error(fmt.Sprintf("panic in hunter loop: %v", r))
			h.SetState(StateError)
		}
	}()

	alarm, alarmErr := NewAlarmTimer()
	if alarmErr != nil {
		h.log.Warn(fmt.Sprintf("timerfd unavailable (%v) — falling back to plain sleep", alarmErr))
	} else {
		defer alarm.Close()
	}
	h.log.Info("hunter loop started")

	backoffMult := 1.0
	netErrStreak := 0
	needCfgCycles := 0

	for {
		if h.staleGen(gen) {
			return
		}
		h.mu.Lock()
		stopped := h.stopCh
		h.mu.Unlock()
		select {
		case <-stopped:
			return
		default:
		}

		// guard: credentials
		if !h.CredsComplete() {
			h.SetState(StateNeedsConfig)
			needCfgCycles++
			if needCfgCycles%10 == 1 {
				h.log.Info(fmt.Sprintf("waiting for OCI credentials (cycle %d) — fill them in via the WebUI Config tab", needCfgCycles))
			}
			// First 4 cycles are tight (fast response right after config is
			// filled in), then loosen to 5 minutes to avoid waking the phone
			// all night while unconfigured.
			waitCfg := 30 * time.Second
			if needCfgCycles > 4 {
				waitCfg = 300 * time.Second
			}
			if exited := h.sleepInterruptible(alarm, waitCfg); exited || h.staleGen(gen) {
				return
			}
			continue
		}

		// guard: battery
		batt := batteryPct()
		if batt >= 0 && batt < h.cfg.BatteryMinPct {
			h.SetState(StatePausedBattery)
			h.note(fmt.Sprintf("paused: battery %d%% below minimum %d%%", batt, h.cfg.BatteryMinPct))
			if exited := h.sleepInterruptible(alarm, 120*time.Second); exited || h.staleGen(gen) {
				return
			}
			continue
		}

		// guard: network
		wifiUp, anyUp := netState()
		if !anyUp || (h.cfg.WiFiOnly && !wifiUp) {
			h.SetState(StatePausedOffline)
			h.note("paused: network requirement not met (offline or not WiFi)")
			if exited := h.sleepInterruptible(alarm, 60*time.Second); exited || h.staleGen(gen) {
				return
			}
			continue
		}

		// discovery (once per session)
		if !h.rt.Discovered {
			ok := h.discover()
			if !ok {
				if exited := h.sleepInterruptible(alarm, 60*time.Second); exited {
					return
				}
				continue
			}
		}
		// already own enough instances?
		if h.state == StateComplete {
			if exited := h.sleepInterruptible(alarm, 10*time.Minute); exited || h.staleGen(gen) {
				return
			}
			continue
		}

		if h.staleGen(gen) {
			return
		}
		h.SetState(StateHunting)
		kind := h.doCheck()
		h.saveStats()
		if kind == "NETERR" {
			netErrStreak++
		} else {
			netErrStreak = 0
		}

		delay := time.Duration(h.cfg.IntervalBaseSecs)*time.Second +
			time.Duration(rand.IntN(maxi(1, h.cfg.IntervalJitterSecs)))*time.Second
		switch kind {
		case "RATE_LIMIT":
			backoffMult *= h.cfg.BackoffFactor
			if backoffMult > float64(h.cfg.BackoffMaxSecs)/float64(maxi(1, h.cfg.IntervalBaseSecs)) {
				backoffMult = float64(h.cfg.BackoffMaxSecs) / float64(maxi(1, h.cfg.IntervalBaseSecs))
			}
			delay = time.Duration(float64(h.cfg.IntervalBaseSecs)*backoffMult) * time.Second
			h.SetState(StateBackoff)
		case "ERROR":
			backoffMult *= 1.2
			delay = time.Duration(float64(h.cfg.IntervalBaseSecs)*backoffMult) * time.Second
		case "NETERR":
			if netErrStreak >= 5 {
				delay = 5 * time.Minute
				if netErrStreak == 5 {
					h.log.Warn("network down — 5 consecutive failures, pausing 5 minutes")
				}
			} else {
				backoffMult *= 1.2
				delay = time.Duration(float64(h.cfg.IntervalBaseSecs)*backoffMult) * time.Second
			}
		case "AUTH":
			h.SetState(StateError)
			if exited := h.sleepInterruptible(alarm, 5*time.Minute); exited {
				return
			}
			continue
		case "FOUND":
			return
		default:
			backoffMult = 1.0
		}
		if delay > time.Duration(h.cfg.BackoffMaxSecs)*time.Second {
			delay = time.Duration(h.cfg.BackoffMaxSecs) * time.Second
		}
		h.maybeSendTelegramSummary()
		h.mu.Lock()
		h.stats.CurrentDelay = int(delay.Seconds())
		h.mu.Unlock()
		if exited := h.sleepInterruptible(alarm, delay); exited || h.staleGen(gen) {
			return
		}
	}
}

// sleepInterruptible sleeps until the alarm/timeout fires, a control
// message (checknow) arrives, or the hunter is stopped.
// Returns true when the loop must exit (stopped).
func (h *Hunter) sleepInterruptible(alarm *AlarmTimer, d time.Duration) bool {
	h.mu.Lock()
	h.stats.NextCheckAt = time.Now().Add(d)
	h.stats.CurrentDelay = int(d.Seconds())
	stopCh := h.stopCh
	ctrl := h.control
	h.mu.Unlock()

	var tick <-chan time.Time
	var alarmCh <-chan error
	rtcArmed := false
	if alarm != nil && !alarm.Broken() {
		if err := alarm.Set(d); err == nil {
			alarmCh = alarm.WaitCh()
			if err2 := armRtcWake(d); err2 == nil {
				rtcArmed = true
			} else if !h.rtcWarned {
				h.rtcWarned = true
				h.log.Warn("rtc wakealarm failed (" + err2.Error() + ") — will retry every cycle")
			}
		} else {
			tick = time.After(d)
		}
	} else {
		tick = time.After(d)
	}
	if rtcArmed {
		defer disarmRtcWake()
	}

	for {
		select {
		case <-stopCh:
			return true
		case c := <-ctrl:
			if c == "checknow" {
				return false
			}
			if c == "stop" {
				return true
			}
		case <-tick:
			return false
		case err := <-alarmCh:
			if err != nil {
				// Read error (e.g. fd closed / odd condition) — do not kill
				// the loop; fall back to a plain timer for this and later cycles.
				h.log.Warn("alarm timer error (" + err.Error() + ") — falling back to plain sleep")
				tick = time.After(d)
				alarmCh = nil
				continue
			}
			return false
		}
	}
}

func maxi(a, b int) int { if a > b { return a }; return b }

// ---------- discovery ----------

// client returns a thread-safe snapshot of the OCIClient. ApplyConfig
// may replace h.oci at any moment, so never hold h.oci directly.
func (h *Hunter) client() *OCIClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.oci
}

func (h *Hunter) buildClient() error {
	oci, err := NewOCIClient(h.cfg, h.baseDir)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.oci = oci
	h.mu.Unlock()
	return nil
}

func (h *Hunter) discover() bool {
	oci := h.client()
	if oci == nil {
		if err := h.buildClient(); err != nil {
			h.log.Error("failed to load private key: " + err.Error())
			h.SetState(StateNeedsConfig)
			return false
		}
		oci = h.client()
	}
	h.log.Info("validating credentials (GetTenancy)… ")
	r, name := oci.GetTenancy()
	if r.Err != nil {
		h.log.Error("network: " + r.Err.Error())
		h.note("network error: " + r.Err.Error())
		h.SetState(StateError)
		return false
	}
	if r.HTTPStatus == 401 {
		h.log.Error("401 Unauthorized — tenancy/user OCID, fingerprint, or private key mismatch")
		h.SetState(StateError)
		return false
	}
	if r.HTTPStatus != 200 {
		h.log.Error(fmt.Sprintf("GetTenancy HTTP %d: %s", r.HTTPStatus, r.Message))
		return false
	}
	h.log.Info("credentials OK — tenancy: " + name)
	h.rt.Tenancy = name

	// AD
	if h.cfg.ADName != "" {
		h.rt.AD = h.cfg.ADName
	} else {
		rAD, ads := oci.ListADs()
		if len(ads) == 0 {
			h.log.Error(fmt.Sprintf("failed to list availability domains: HTTP %d %s %v", rAD.HTTPStatus, rAD.Message, rAD.Err))
			return false
		}
		h.rt.AD = ads[0]
	}

	// subnet
	if h.cfg.SubnetOCID != "" {
		h.rt.Subnet = h.cfg.SubnetOCID
	} else {
		rSN, subnets := oci.ListSubnets()
		if len(subnets) == 0 {
			h.log.Error(fmt.Sprintf("no subnet found (HTTP %d %s %v) — create a VCN + subnet in the Console or set SUBNET_OCID", rSN.HTTPStatus, rSN.Message, rSN.Err))
			return false
		}
		h.rt.Subnet = subnets[0]
	}

	// image
	if h.cfg.ImageOCID != "" {
		h.rt.ImageID = h.cfg.ImageOCID
		h.rt.ImageName = "(from config)"
	} else {
		rImg, imgs := oci.ListImages(h.cfg.OSName, h.cfg.OSVersion, h.cfg.Shape)
		if len(imgs) == 0 {
			h.log.Error(fmt.Sprintf("image %s %s for shape %s not found: HTTP %d %s %v",
				h.cfg.OSName, h.cfg.OSVersion, h.cfg.Shape, rImg.HTTPStatus, rImg.Message, rImg.Err))
			return false
		}
		h.rt.ImageID = imgs[0].ID
		h.rt.ImageName = imgs[0].DisplayName
	}

	// Target OCPU/RAM: explicit config wins; when 0, compute from limits
	// and existing instances.
	cpu, mem := h.cfg.OCPUs, h.cfg.MemoryGB
	if cpu <= 0 || mem <= 0 {
		availCPU, availMem := 4.0, 24.0
		if v := h.fetchLimit("standard-a1-core-count"); v >= 0 {
			availCPU = v
		}
		if v := h.fetchLimit("standard-a1-memory-count"); v >= 0 {
			availMem = v
		}
		rLI, insts := oci.ListInstances()
		if rLI.Err != nil || rLI.HTTPStatus != 200 {
			h.log.Warn(fmt.Sprintf("ListInstances: HTTP %d %s %v (target computed without instance data)", rLI.HTTPStatus, rLI.Message, rLI.Err))
		}
		usedCPU, usedMem := 0.0, 0.0
		a1Count := 0
		for _, in := range insts {
			if in.LifecycleState == "TERMINATED" || !strings.Contains(in.Shape, "A1") {
				continue
			}
			a1Count++
			usedCPU += in.ShapeConfig.OCpus
			usedMem += in.ShapeConfig.MemoryInGBs
		}
		if h.cfg.MaxInstances > 0 && a1Count >= h.cfg.MaxInstances {
			h.log.Info(fmt.Sprintf("%d A1 instance(s) already exist (MAX_INSTANCES=%d) — hunter done", a1Count, h.cfg.MaxInstances))
			h.SetState(StateComplete)
			h.rt.Discovered = true
			return true
		}
		if cpu <= 0 {
			cpu = int(availCPU - usedCPU + 0.001)
			if cpu > 4 {
				cpu = 4
			}
			if cpu < 1 {
				cpu = 1
			}
		}
		if mem <= 0 {
			mem = int(availMem - usedMem + 0.001)
			if mem > 24 {
				mem = 24
			}
			if mem < 6 {
				mem = 6
			}
		}
	}
	h.rt.TargetCPU, h.rt.TargetMem = cpu, mem
	h.rt.Discovered = true
	h.log.Info(fmt.Sprintf("target locked: %s | %s | %d OCPU / %d GB | image: %s",
		h.rt.AD, h.cfg.Shape, cpu, mem, h.rt.ImageName))
	return true
}

func (h *Hunter) fetchLimit(name string) float64 {
	ociC := h.client()
	if ociC == nil {
		return -1
	}
	r, v := ociC.LimitAvailability(name)
	if r.Err != nil || v < 0 {
		h.log.Warn(fmt.Sprintf("limit %s unreadable — using default", name))
		return -1
	}
	return v
}

// ---------- check ----------

func (h *Hunter) doCheck() string {
	release := holdWake("check")
	defer release()

	start := time.Now()
	ociC := h.client()
	if ociC == nil {
		return "AUTH"
	}
	r, inst := ociC.LaunchInstance(
		h.rt.AD, h.rt.Subnet, h.rt.ImageID, h.cfg.Shape,
		h.cfg.DisplayName, h.cfg.SSHKeys,
		h.rt.TargetCPU, h.rt.TargetMem, h.cfg.BootVolumeGB, h.cfg.AssignPublicIP,
	)
	ms := time.Since(start).Milliseconds()

	h.mu.Lock()
	h.stats.Checks++
	h.stats.LastCheckAt = time.Now()
	h.recent = append(h.recent, Attempt{At: start, Ms: ms, Status: r.HTTPStatus, Kind: classify(r)})
	if len(h.recent) > 30 {
		h.recent = h.recent[len(h.recent)-30:]
	}
	h.mu.Unlock()

	switch {
	case r.Err != nil:
		h.note(fmt.Sprintf("network error (%d ms): %v", ms, r.Err))
		h.mu.Lock(); h.stats.Errors++; h.mu.Unlock()
		return "NETERR"
	case r.HTTPStatus == 429:
		h.mu.Lock(); h.stats.RateLimited++; h.mu.Unlock()
		ra := r.RetryAfterSecs
		h.log.Error(fmt.Sprintf("429 TooManyRequests (%d ms) — backoff, Retry-After=%ds", ms, ra))
		h.note(fmt.Sprintf("429 rate-limited (Retry-After %ds)", ra))
		return "RATE_LIMIT"
	case r.HTTPStatus == 401 || r.HTTPStatus == 404:
		// OCI sometimes answers 401 "required information..." on the launch
		// endpoint when it is really a hidden rate limit. Verify with an
		// identity call: if it still returns 200, this is not an auth problem.
		if rv, _ := ociC.GetTenancy(); rv.Err == nil && rv.HTTPStatus == 200 {
			h.log.Error(fmt.Sprintf("%d on launch while identity is still 200 — hidden throttle, backing off", r.HTTPStatus))
			h.note("401 on launch = hidden rate limit, backing off")
			h.mu.Lock()
			h.stats.RateLimited++
			h.mu.Unlock()
			return "RATE_LIMIT"
		}
		h.log.Error(fmt.Sprintf("%d: %s — check credentials/OCIDs", r.HTTPStatus, r.Message))
		h.note(fmt.Sprintf("HTTP %d auth/OCID problem", r.HTTPStatus))
		return "AUTH"
	case r.HTTPStatus >= 200 && r.HTTPStatus < 300 && inst != nil:
		return h.handleFound(inst, ms)
	case r.IsCapacity():
		h.mu.Lock()
		h.stats.CapacityMiss++
		cm := h.stats.CapacityMiss
		h.mu.Unlock()
		h.note(fmt.Sprintf("out of capacity (%d ms)", ms))
		h.log.Log("CAPACITY", fmt.Sprintf("attempt #%d: out of capacity (%d ms)", cm, ms))
		return "CAPACITY"
	case strings.Contains(strings.ToLower(r.Code+" "+r.Message), "limitexceeded") ||
		strings.Contains(strings.ToLower(r.Code+" "+r.Message), "quota"):
		// quota exceeded → try auto-lowering to 2 OCPU/12 GB once
		if h.rt.TargetCPU > 2 {
			h.rt.TargetCPU = 2
			h.rt.TargetMem = 12
			h.log.Warn("quota insufficient — target auto-lowered to 2 OCPU / 12 GB")
			return "QUOTA"
		}
		h.log.Error("quota/limit: " + r.Code + " — " + r.Message)
		h.note("quota exceeded: " + r.Code)
		return "AUTH" // butuh perhatian, jangan spam
	default:
		h.mu.Lock(); h.stats.Errors++; h.mu.Unlock()
		msg := fmt.Sprintf("HTTP %d (%d ms)", r.HTTPStatus, ms)
		if r.Code != "" {
			msg += " code=" + r.Code
		}
		h.log.Error(msg)
		h.note(msg)
		return "ERROR"
	}
}

func classify(r *APIResult) string {
	switch {
	case r.Err != nil:
		return "NETERR"
	case r.HTTPStatus == 429:
		return "RATE_LIMIT"
	case r.HTTPStatus >= 200 && r.HTTPStatus < 300:
		return "FOUND"
	case r.IsCapacity():
		return "CAPACITY"
	case r.HTTPStatus == 401:
		return "AUTH"
	default:
		return "ERROR"
	}
}

func (h *Hunter) handleFound(inst map[string]any, ms int64) string {
	ociC := h.client()
	ocid, _ := inst["id"].(string)
	name, _ := inst["displayName"].(string)
	h.log.Log("SUCCESS", fmt.Sprintf("*** CAPACITY FOUND! Instance created in %d ms: %s (%s) ***", ms, name, ocid))
	h.note("*** FOUND — instance " + name + " created ***")

	ip := ""
	for i := 0; i < 10; i++ {
		if ip, _ = ociC.InstancePublicIP(ocid); ip != "" {
			break
		}
		time.Sleep(4 * time.Second)
	}
	info := &InstanceInfo{OCID: ocid, Name: name, IP: ip, FoundAt: time.Now()}
	h.mu.Lock()
	h.found = info
	h.state = StateFound
	h.mu.Unlock()

	msg := fmt.Sprintf("Oracle A1 secured! %s — IP: %s", name, ip)
	if ip == "" {
		msg = fmt.Sprintf("Oracle A1 secured! %s (IP not ready yet — check the WebUI)", name)
	} else {
		msg += fmt.Sprintf(" (login: ssh ubuntu@%s)", firstWord(ip))
	}
	h.log.Log("SUCCESS", msg)
	h.notify("Oracle A1 DIDAPAT 🎉", msg+"\nOCID: "+ocid)
	return "FOUND"
}

func firstWord(s string) string {
	if i := strings.IndexAny(s, " ("); i > 0 {
		return s[:i]
	}
	return s
}

// ---------- environment helpers ----------

func batteryPct() int {
	for _, p := range []string{
		"/sys/class/power_supply/battery/capacity",
		"/sys/class/power_supply/bms/capacity",
	} {
		if b, err := os.ReadFile(p); err == nil {
			var v int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &v); err == nil {
				return v
			}
		}
	}
	return -1
}

// netState returns (wifiUp, anyUp).
func netState() (bool, bool) {
	wifi, any := false, false
	entries, _ := os.ReadDir("/sys/class/net")
	for _, e := range entries {
		name := e.Name()
		if name == "lo" {
			continue
		}
		b, err := os.ReadFile("/sys/class/net/" + name + "/operstate")
		if err != nil || strings.TrimSpace(string(b)) != "up" {
			continue
		}
		any = true
		if strings.HasPrefix(name, "wlan") || strings.HasPrefix(name, "wifi") {
			wifi = true
		}
	}
	return wifi, any
}

// renderTemplate fills a message template with live stats.
// Placeholders: {sum} {capacity} {rate_limited} {errors} {uptime} {battery}
// {region} {target} {time} (+ {name} {ip} {ocid} for the found template).
func (h *Hunter) renderTemplate(tpl string, extra map[string]string) string {
	h.mu.Lock()
	st := h.stats
	cfg := h.cfg
	rt := h.rt
	h.mu.Unlock()
	m := map[string]string{
		"{sum}":         strconv.FormatInt(st.Checks, 10),
		"{capacity}":    strconv.FormatInt(st.CapacityMiss, 10),
		"{rate_limited}": strconv.FormatInt(st.RateLimited, 10),
		"{errors}":      strconv.FormatInt(st.Errors, 10),
		"{uptime}":      time.Since(st.StartedAt).Round(time.Second).String(),
		"{battery}":     strconv.Itoa(batteryPct()),
		"{region}":      cfg.Region,
		"{target}":      fmt.Sprintf("%s %d OCPU/%d GB", cfg.Shape, rt.TargetCPU, rt.TargetMem),
		"{time}":        time.Now().Format("2006-01-02 15:04:05"),
	}
	for k, v := range extra {
		m[k] = v
	}
	out := tpl
	for k, v := range m {
		out = strings.ReplaceAll(out, k, v)
	}
	return out
}

// maybeSendTelegramSummary sends the periodic summary when due.
func (h *Hunter) maybeSendTelegramSummary() {
	cfg := h.cfg
	if !cfg.TelegramEnabled || cfg.TelegramToken == "" || cfg.TelegramChatID == "" {
		return
	}
	interval := time.Duration(cfg.TelegramSummaryIntervalMins) * time.Minute
	if interval < 15*time.Minute {
		return
	}
	h.mu.Lock()
	checks := h.stats.Checks
	h.mu.Unlock()
	if checks == 0 {
		return
	}
	if time.Since(h.lastSummary) < interval {
		return
	}
	h.lastSummary = time.Now()
	msg := h.renderTemplate(cfg.TelegramSummaryTemplate, nil)
	h.sendTelegram(msg)
	h.log.Info("telegram summary sent")
}

// sendTelegram delivers a message via the Telegram bot API (synchronous —
// used by the WebUI test button; internal callers wrap it in a goroutine).
func (h *Hunter) sendTelegram(text string) error {
	cfg := h.cfg
	body := fmt.Sprintf(`{"chat_id":%q,"text":"%s"}`, cfg.TelegramChatID, text)
	req, err := http.NewRequest("POST", "https://api.telegram.org/bot"+cfg.TelegramToken+"/sendMessage",
		strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second, Transport: sharedTransport}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("telegram HTTP %d: %s", resp.StatusCode, string(rb))
	}
	return nil
}

// holdWake briefly holds a wake lock during the request; if SELinux
// refuses (ksu domain), just continue: the alarm timer already holds a
// short wakeup.
func holdWake(reason string) func() {
	name := "ohunter_" + reason
	if err := os.WriteFile("/sys/power/wake_lock", []byte(name), 0); err != nil {
		if h := currentHunter; h != nil && !h.wakeWarned {
			h.wakeWarned = true
			h.log.Warn("wake_lock not writable (" + err.Error() + ") — continuing without an explicit wake lock")
		}
		return func() {}
	}
	return func() {
		os.WriteFile("/sys/power/wake_unlock", []byte(name), 0)
	}
}

func (h *Hunter) notify(title, msg string) {
	go func() {
		cmd := exec.Command("/system/bin/sh", "-c",
			`/system/bin/cmd notification post -S bigtext -t "`+title+`" oracle-hunter "`+strings.ReplaceAll(msg, `"`, `'`)+`"`)
		if out, err := cmd.CombinedOutput(); err != nil {
			h.log.Warn("notification failed: " + string(out))
		}
	}()
	if h.cfg.TelegramEnabled && h.cfg.TelegramToken != "" && h.cfg.TelegramChatID != "" {
		text := h.renderTemplate(h.cfg.TelegramFoundTemplate, map[string]string{})
		if msg != "" {
			text = text + "\n" + msg
		}
		go func() {
			if err := h.sendTelegram(text); err != nil {
				h.log.Warn("telegram error: " + err.Error())
			}
		}()
	}
}

// ---------- one-shot mode ----------

func (h *Hunter) RunOnce() {
	if !h.CredsComplete() {
		for _, m := range h.cfg.MissingCreds(h.baseDir) {
			fmt.Println("KURANG:", m)
		}
		return
	}
	if !h.discover() {
		fmt.Println("discovery failed — check logs")
		return
	}
	kind := h.doCheck()
	fmt.Printf("result: %s\n", kind)
	fmt.Println("recent logs:")
	for _, l := range h.log.Snapshot(6) {
		fmt.Println(" ", l)
	}
}

func (h *Hunter) Validate() bool {
	if !h.CredsComplete() {
		for _, m := range h.cfg.MissingCreds(h.baseDir) {
			fmt.Println("KURANG:", m)
		}
		return false
	}
	if err := h.buildClient(); err != nil {
		fmt.Println("private key:", err)
		return false
	}
	oci := h.client()
	r, name := oci.GetTenancy()
	if r.Err != nil {
		fmt.Println("network:", r.Err)
		return false
	}
	if r.HTTPStatus != 200 {
		fmt.Printf("GetTenancy HTTP %d: %s\n", r.HTTPStatus, r.Message)
		return false
	}
	fmt.Println("OK — tenancy:", name)
	return h.discover()
}

// Note records lastResult (called on the stats struct).
func (s *Stats) Note(msg string) {
	s.LastResult = msg
}

// ApplyConfig is called by the WebUI after config changes. The loop is NOT
// restarted: the running timer finishes, changes take effect on the next
// cycle, and no extra attempt is triggered. Discovery is only invalidated
// when connection/target fields actually changed; otherwise the resolved
// runtime values are updated in place.
func (h *Hunter) ApplyConfig(cfg *Config) {
	h.mu.Lock()
	old := h.cfg
	h.cfg = cfg
	connChanged := old.TenancyOCID != cfg.TenancyOCID ||
		old.UserOCID != cfg.UserOCID ||
		old.Fingerprint != cfg.Fingerprint ||
		old.Region != cfg.Region ||
		old.PrivateKeyPath != cfg.PrivateKeyPath ||
		old.ADName != cfg.ADName ||
		old.SubnetOCID != cfg.SubnetOCID ||
		old.ImageOCID != cfg.ImageOCID ||
		old.OSName != cfg.OSName ||
		old.OSVersion != cfg.OSVersion ||
		old.Shape != cfg.Shape
	if connChanged {
		h.oci = nil
		h.rt = RuntimeInfo{}
	} else {
		if cfg.OCPUs > 0 {
			h.rt.TargetCPU = cfg.OCPUs
		}
		if cfg.MemoryGB > 0 {
			h.rt.TargetMem = cfg.MemoryGB
		}
	}
	h.mu.Unlock()
	if connChanged {
		h.log.Info("config updated — connection/target changed, discovery reruns next cycle (no restart)")
	} else {
		h.log.Info("config updated — takes effect on the next cycle (no restart)")
	}
}

// ---------- stats persistence (survives reboots) ----------

type statsFile struct {
	Stats       Stats     `json:"stats"`
	LastSummary time.Time `json:"last_summary"`
}

var statsPath string

func (h *Hunter) loadStats() {
	data, err := os.ReadFile(statsPath)
	if err != nil {
		return
	}
	var sf statsFile
	if json.Unmarshal(data, &sf) == nil {
		h.stats = sf.Stats
		h.lastSummary = sf.LastSummary
	}
}

func (h *Hunter) saveStats() {
	h.mu.Lock()
	sf := statsFile{Stats: h.stats, LastSummary: h.lastSummary}
	h.mu.Unlock()
	if data, err := json.Marshal(sf); err == nil {
		_ = os.WriteFile(statsPath, data, 0o644)
	}
}

// rssMB reports the process resident set size in MB (from /proc/self/statm).
func rssMB() float64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return -1
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return -1
	}
	pages, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return -1
	}
	return math.Round(pages*float64(os.Getpagesize())/1048576*10) / 10
}
