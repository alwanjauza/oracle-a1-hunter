package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	logMaxBytes   = 2 * 1024 * 1024
	logRotateKeep = 3
)

// Logger: a fixed-size circular in-memory buffer (for the WebUI), a rotating
// file on disk (hard ceiling logMaxBytes × (logRotateKeep+1)), and broadcast
// to SSE subscribers. Memory usage is constant for the lifetime of the
// process: the buffer array is allocated once and never grows.
type Logger struct {
	mu    sync.Mutex
	buf   []string // fixed-capacity circular buffer
	head  int      // index of the oldest entry (valid when full)
	count int
	max   int
	f     *os.File
	path  string
	subs  map[chan string]struct{}
}

func NewLogger(path string, maxLines int) *Logger {
	l := &Logger{
		path: path,
		max:  maxLines,
		buf:  make([]string, 0, maxLines),
		subs: make(map[chan string]struct{}),
	}
	l.reopen()
	return l
}

func (l *Logger) reopen() {
	_ = os.MkdirAll(filepath.Dir(l.path), 0o755)
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	l.f = f
}

func (l *Logger) Log(level, msg string) {
	line := time.Now().Format("2006-01-02 15:04:05") + " [" + level + "] " + msg
	l.mu.Lock()
	// circular insert: overwrite the oldest slot once full
	if l.count < l.max {
		l.buf = append(l.buf, line)
		l.count++
	} else {
		l.buf[l.head] = line
		l.head = (l.head + 1) % l.max
	}
	if l.f != nil {
		if _, err := l.f.WriteString(line + "\n"); err == nil {
			st, _ := l.f.Stat()
			if st != nil && st.Size() > logMaxBytes {
				l.f.Close()
				for i := logRotateKeep - 1; i >= 1; i-- {
					_ = os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
				}
				_ = os.Rename(l.path, l.path+".1")
				l.reopen()
			}
		}
	}
	for ch := range l.subs {
		select {
		case ch <- line:
		default: // slow subscriber → drop, never block
		}
	}
	l.mu.Unlock()
}

func (l *Logger) Info(msg string)  { l.Log("INFO", msg) }
func (l *Logger) Warn(msg string)  { l.Log("WARN", msg) }
func (l *Logger) Error(msg string) { l.Log("ERROR", msg) }

// phys maps a logical index (0 = oldest) to the physical slot.
func (l *Logger) phys(i int) int {
	if l.count < l.max {
		return i
	}
	return (l.head + i) % l.max
}

func (l *Logger) Snapshot(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > l.count {
		n = l.count
	}
	out := make([]string, 0, n)
	for i := l.count - n; i < l.count; i++ {
		out = append(out, l.buf[l.phys(i)])
	}
	return out
}

func (l *Logger) Subscribe() chan string {
	ch := make(chan string, 256)
	l.mu.Lock()
	l.subs[ch] = struct{}{}
	l.mu.Unlock()
	return ch
}

func (l *Logger) Unsubscribe(ch chan string) {
	l.mu.Lock()
	delete(l.subs, ch)
	l.mu.Unlock()
}
