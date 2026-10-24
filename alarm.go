package main

import (
	"encoding/binary"
	"os"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

// Linux clockids for timerfd.
// NOTE for this kernel (Android 4.14 "PoWeR"): CLOCK_BOOTTIME_ALARM (9) does
// NOT count suspended time on its timeline, so an absolute timer computed
// from CLOCK_BOOTTIME (7) drifts far into the future (verified via fdinfo:
// a 180s timer still showed ~110s remaining after 343s wall time).
// Therefore the main timer uses a RELATIVE timer on CLOCK_BOOTTIME (which
// includes suspend time = wall-clock cadence), and waking from deep sleep is
// handled by the RTC wakealarm (armRtcWake), the same mechanism the Android
// AlarmManager uses.
const (
	clockBoottime      = 7
	clockBoottimeAlarm = 9
	tfdTimerAbstime    = 1
)

type itimerspec struct {
	ItInterval syscall.Timespec
	ItValue    syscall.Timespec
}

type AlarmTimer struct {
	fd     int
	broken bool
}

// Broken reports whether this alarm timer should no longer be used
// (after a read failure); callers must fall back to a plain timer.
func (t *AlarmTimer) Broken() bool { return t == nil || t.broken }

func NewAlarmTimer() (*AlarmTimer, error) {
	fd, _, errno := syscall.Syscall(syscall.SYS_TIMERFD_CREATE, clockBoottime, 0, 0)
	if errno != 0 {
		fd, _, errno = syscall.Syscall(syscall.SYS_TIMERFD_CREATE, clockBoottimeAlarm, 0, 0)
		if errno != 0 {
			return nil, errno
		}
	}
	return &AlarmTimer{fd: int(fd)}, nil
}

const rtcWakealarmPath = "/sys/class/rtc/rtc0/wakealarm"

// armRtcWake schedules an RTC-level wakeup (raises the device from deep
// sleep), the same mechanism used by Android's AlarmManager.
// Cancel first: the RTC can return EBUSY while a previous alarm is still
// armed (e.g. the prior process was killed before disarming).
func armRtcWake(delay time.Duration) error {
	os.WriteFile(rtcWakealarmPath, []byte("0"), 0)
	epoch := time.Now().Unix() + int64(delay.Seconds()) + 5
	return os.WriteFile(rtcWakealarmPath, []byte(strconv.FormatInt(epoch, 10)), 0)
}

func disarmRtcWake() {
	os.WriteFile(rtcWakealarmPath, []byte("0"), 0)
}

// Set schedules a wakeup `delay` from now using a RELATIVE timer — the
// kernel does the arithmetic, making it immune to timeline differences
// between CLOCK_BOOTTIME and CLOCK_BOOTTIME_ALARM.
func (t *AlarmTimer) Set(delay time.Duration) error {
	its := itimerspec{
		ItInterval: syscall.Timespec{},
		ItValue:    syscall.NsecToTimespec(int64(delay)),
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_TIMERFD_SETTIME, uintptr(t.fd), 0,
		uintptr(unsafe.Pointer(&its)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// WaitCh returns a channel filled when the timer fires.
// Close() cancels (the blocked read fails with EBADF).
// EINTR (e.g. Go runtime SIGURG preemption) is retried, not treated as error.
func (t *AlarmTimer) WaitCh() <-chan error {
	ch := make(chan error, 1)
	go func() {
		var buf [8]byte
		for {
			n, err := syscall.Read(t.fd, buf[:])
			if err == syscall.EINTR {
				continue
			}
			if err == nil && n >= 8 {
				_ = binary.LittleEndian.Uint64(buf[:])
			}
			if err != nil {
				t.broken = true
			}
			ch <- err
			return
		}
	}()
	return ch
}

func (t *AlarmTimer) Close() error {
	return syscall.Close(t.fd)
}
