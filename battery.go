package main

import (
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Battery accounting for the hunter.
//
// The phone is not dedicated to hunting, so total %/h says nothing about the
// hunter's own cost. Instead we attribute energy the only window that is
// provably ours: the CPU-awake window of each check. While the phone would
// otherwise be suspended (the RTC alarm woke it for us), current_now is
// sampled every ~200 ms and integrated into µAh per check. Windows in which
// the screen was on or the charger was connected are flagged and excluded —
// that energy is not attributable to the hunter alone.

// ---------- sysfs sensors ----------

// Node candidates in priority order; first one that exists wins
// (standard "battery", plus the MediaTek "bms" gauge).
var battNodeCandidates = []string{"battery", "bms"}

func battNode(file string) string {
	for _, n := range battNodeCandidates {
		p := "/sys/class/power_supply/" + n + "/" + file
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func readNodeInt(file string) (int, bool) {
	p := battNode(file)
	if p == "" {
		return 0, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return v, true
}

func readNodeStr(file string) string {
	p := battNode(file)
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// current_now sign conventions differ between vendors (some report charging
// as positive, some as negative). Calibrated once per boot against status.
var (
	signMu      sync.Mutex
	signFlipped bool
	signKnown   bool
)

func calibrateSign(charging bool, microA int) {
	if !charging || microA == 0 {
		return
	}
	signMu.Lock()
	if !signKnown {
		signFlipped = microA < 0
		signKnown = true
	}
	signMu.Unlock()
}

// BattReading is one sensor snapshot. MicroA is normalized: negative while
// discharging, positive while charging, 0 when unknown.
type BattReading struct {
	Pct      int  // 0-100, -1 unknown
	Charging bool // external power (Charging / Full / Not charging)
	MicroA   int  // µA, normalized sign
	MicroV   int  // µV, 0 unknown
}

func readBatt() BattReading {
	var r BattReading
	r.Pct = -1
	if v, ok := readNodeInt("capacity"); ok && v >= 0 && v <= 100 {
		r.Pct = v
	}
	switch readNodeStr("status") {
	case "Charging", "Full", "Not charging":
		r.Charging = true
	}
	if v, ok := readNodeInt("voltage_now"); ok {
		r.MicroV = v
	}
	if v, ok := readNodeInt("current_now"); ok && v != 0 {
		calibrateSign(r.Charging, v)
		signMu.Lock()
		flipped := signFlipped
		signMu.Unlock()
		if flipped {
			v = -v
		}
		// normalized: + charging, − discharging
		if r.Charging && v < 0 {
			v = -v
		} else if !r.Charging && v > 0 {
			v = -v
		}
		r.MicroA = v
	}
	return r
}

var screenBrightnessPaths = []string{
	"/sys/class/leds/lcd-backlight/brightness",
	"/sys/class/backlight/panel0-backlight/brightness",
}

// screenOn reports whether the display is lit. When no brightness node is
// found it assumes off — checks normally run while the phone is suspended.
func screenOn() bool {
	for _, p := range screenBrightnessPaths {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b)) != "0"
		}
	}
	return false
}

// chargeFullUAh returns the battery's full-charge capacity in µAh (used to
// convert hunter µAh into "% of battery"). Falls back to a typical phone
// battery size when the gauge does not expose it.
func chargeFullUah() int {
	if v, ok := readNodeInt("charge_full"); ok && v > 100_000 {
		return v
	}
	if v, ok := readNodeInt("charge_full_design"); ok && v > 100_000 {
		return v
	}
	return 4_500_000
}

// ---------- bounded history ----------

const (
	battRingMax   = 720 // ~24 h at the 120 s cadence
	energyRingMax = 720
)

// BattSample is a battery-level point for the context chart.
type BattSample struct {
	T   int64 `json:"t"`           // unix seconds
	Pct int   `json:"p"`           // 0-100, -1 unknown
	Chg bool  `json:"c,omitempty"` // external power
}

// EnergySample is the integrated cost of one check window.
type EnergySample struct {
	At   int64   `json:"at"`  // unix seconds, window start
	UAh  float32 `json:"uah"` // charge drawn during the window
	Secs float32 `json:"secs"`
	Fl   uint8   `json:"fl"` // bit0 charging, bit1 screen was on
}

// ---------- per-check energy integration ----------

type energySession struct {
	stop chan struct{}
	done chan EnergySample
}

// startCheckEnergy begins sampling current_now for the duration of a check
// window (the caller holds the wake lock). Finish() must be called to stop
// the sampler goroutine and collect the result.
func startCheckEnergy() *energySession {
	es := &energySession{stop: make(chan struct{}), done: make(chan EnergySample, 1)}
	start := time.Now()
	go func() {
		var uAs float64 // µA·s
		var fl uint8
		last := time.Now()
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-es.stop:
				es.done <- EnergySample{
					At:   start.Unix(),
					UAh:  float32(uAs / 3600),
					Secs: float32(time.Since(start).Seconds()),
					Fl:   fl,
				}
				return
			case <-t.C:
				now := time.Now()
				dt := now.Sub(last).Seconds()
				last = now
				r := readBatt()
				if r.Charging {
					fl |= 1
				}
				if screenOn() {
					fl |= 2
				}
				if r.MicroA < 0 {
					uAs += float64(-r.MicroA) * dt
				}
			}
		}
	}()
	return es
}

func (es *energySession) Finish() EnergySample {
	close(es.stop)
	return <-es.done
}

// HuntEnergy is the hunter-attributed battery summary for the last 24 h.
type HuntEnergy struct {
	UAhPerCheck float64 `json:"uAhPerCheck"`
	MAh24       float64 `json:"mAh24"`
	Pct24       float64 `json:"pct24"`
	AwakeSec24  float64 `json:"awakeSec24"`
	Checks24    int     `json:"checks24"`
	Excluded24  int     `json:"excluded24"`
	FullMAh     float64 `json:"batteryFullMAh"`
}

// huntEnergy24h folds the energy ring into a summary. Excluded windows
// (charging / screen on) are reported but never counted as hunter cost.
func huntEnergy24h(ring []EnergySample) HuntEnergy {
	full := float64(chargeFullUah())
	he := HuntEnergy{FullMAh: math.Round(full / 1000)}
	cut := time.Now().Add(-24 * time.Hour).Unix()
	var sum, awake float64
	n, excl := 0, 0
	for _, s := range ring {
		if s.At < cut {
			continue
		}
		if s.Fl&3 != 0 {
			excl++
			continue
		}
		sum += float64(s.UAh)
		awake += float64(s.Secs)
		n++
	}
	if n > 0 {
		he.UAhPerCheck = math.Round(sum/float64(n)*10) / 10
	}
	he.MAh24 = math.Round(sum/10) / 100
	he.Pct24 = math.Round(sum/full*10000) / 100
	he.AwakeSec24 = math.Round(awake)
	he.Checks24 = n
	he.Excluded24 = excl
	return he
}
