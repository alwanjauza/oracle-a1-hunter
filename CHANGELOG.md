# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.1] — 2026-10-25

### Fixed

- **Hunt silently stops after using "Check now"** (overnight freeze). Each sleep
  cycle spawned a new blocking reader on the shared timerfd; returning through
  the check-now control path left the previous reader alive, so the next cycle
  had two readers racing for a single timer expiration — and the kernel wakes
  only one. When the stale reader won, the active reader waited forever on a
  disarmed timer (`it_value` `(0, 0)`), and no further attempts were made until
  a human interacted with the WebUI again.
  `AlarmTimer.Cancel()` now fires a 1 ms fuse so a pending reader retires
  cleanly before the next cycle arms its own.

### Verified on-device (Redmi Note 8 Pro · AxionOS / Android 16 · KernelSU Next)

- "Check now" → attempt fires → the **next 120 s cycle still fires** (the exact
  scenario that froze 1.0.0; reproduced and re-tested twice).
- timerfd confirmed armed after a check-now (~103 s remaining observed).
- 429 backoff cadence (120 s × 1.5) intact; upgrade keeps the attempt counter
  and statistics (`stats.json` is untouched by the binary swap).

### Upgrade notes

Replace the `hunter` binary and restart the service. Attempts continue from
where they left off; no configuration changes are required.

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

[1.0.1]: https://github.com/alwanjauza/oracle-a1-hunter/releases/tag/v1.0.1
[1.0.0]: https://github.com/alwanjauza/oracle-a1-hunter/releases/tag/v1.0.0
