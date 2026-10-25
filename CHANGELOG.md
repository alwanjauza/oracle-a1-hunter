# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.1.0] — 2026-10-25

### Added

- **Hunter-attributed battery accounting.** The phone is rarely dedicated to
  hunting, so total %/h says nothing about the hunter's own cost. Instead,
  `current_now` is sampled every ~200 ms while the phone is awake for a check
  (a window the hunter itself created by waking the device) and integrated
  into **µAh per check** — energy that is 100% attributable to hunting.
  - Windows in which the **charger was connected** or the **screen was on**
    are flagged and excluded from the attribution (that energy is not the
    hunter's alone).
  - WebUI: new **Battery — hunting cost** panel with per-check / 24 h cost
    in µAh, mAh and % of battery, awake seconds and excluded-window count,
    plus a 24 h level chart (charging zones shaded, line breaks where the
    phone was off). The Battery card shows `hunt X%/24h · Y awake`.
  - Level history and energy history persist across restarts and reboots
    (bounded 720-entry rings, constant memory).
  - `current_now` sign conventions are calibrated automatically per boot.

### Fixed

- **Hunt silently stops after using "Check now"** (overnight freeze). Each
  sleep cycle spawned a new blocking reader on the shared timerfd; returning
  through the check-now control path left the previous reader alive, so the
  next cycle had two readers racing for a single timer expiration — and the
  kernel wakes only one. When the stale reader won, the active reader waited
  forever on a disarmed timer (`it_value` `(0, 0)`), and no further attempts
  were made until a human interacted with the WebUI again.
  `AlarmTimer.Cancel()` now fires a 1 ms fuse so a pending reader retires
  cleanly before the next cycle arms its own.

### Verified on-device (Redmi Note 8 Pro · AxionOS / Android 16 · KernelSU Next)

- "Check now" → attempt fires → the **next 120 s cycle still fires** (the
  exact scenario that froze the previous build; re-tested twice).
- 4.4 h on battery: 72 attributed checks (mean 36 µAh over a ~2.2 s window) =
  **2.6 mAh ≈ 0.06% of battery**, while total phone drain was 23% — the
  hunter accounted for 0.3% of it.
- 56 checks that fell while the screen was on were correctly flagged and
  excluded; 4 checks during charging likewise.
- Battery rings survive a service restart; RSS flat at ~12 MB across 190+
  checks (sampler goroutines do not leak).

## [1.0.0] — 2026-10-24

### Added

- Initial public release.
- Launch loop for `VM.Standard.A1.Flex` with configurable interval (default
  120 s — proven safe against 429 rate limiting) and optional jitter.
- Suspend-safe scheduling: relative `CLOCK_BOOTTIME` timerfd plus the RTC
  wakealarm (`/sys/class/rtc/rtc0/wakealarm`), the same wake path Android's
  AlarmManager uses — the phone deep-sleeps between checks (~1–3 s awake per
  check) with no persistent wake locks.
- Automatic discovery of availability domain, image and subnet; largest allowed
  A1 allocation when `OCPUS`/`MEMORY_GB` are 0.
- Hidden-throttle detection (launch endpoint 401 while identity returns 200)
  with automatic backoff.
- WebUI (token-authenticated): dashboard with status, attempt counters,
  battery, network, uptime and memory cards; start/stop/restart/check-now
  controls; live color-coded log stream (SSE); full configuration editor with
  ⓘ tooltips, boolean dropdowns and masked secrets; theme switcher
  (system/light/dark); inline SVG icons, zero external dependencies.
- Telegram notifications with editable templates, click-to-insert placeholder
  chips, periodic summaries and a send-test button.
- Constant-memory circular log buffer (1 000 lines) with 2 MB × 4 generation
  on-disk rotation (~8 MB cap).
- Persistent statistics across restarts and reboots (`stats.json`).
- Boot autostart via KernelSU/Magisk `service.d`; one-shot `-once` and
  credential-checking `-validate` CLI modes.
- Cross-builds published by GitHub Actions: android-arm64, linux-arm64,
  linux-amd64, plus SHA-256 checksums.

[1.1.0]: https://github.com/alwanjauza/oracle-a1-hunter/releases/tag/v1.1.0
[1.0.0]: https://github.com/alwanjauza/oracle-a1-hunter/releases/tag/v1.0.0
