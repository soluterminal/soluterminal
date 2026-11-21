package main

import (
	"bufio"
	"io"
	"strconv"
	"sync"
	"time"
)

// Helper stderr limits on the host (helper-protocol-v1, Limits; ruling 8.5).
const (
	helperStderrLineMax   = 16 << 10
	helperStderrPerMinute = 64 << 10
)

// helperStderrLogger copies a helper's stderr into the host's log: each
// record is capped at 16 KiB after escaping control characters, and at most
// 64 KiB of records per minute is logged; the excess is dropped and counted,
// and the count is logged when the next window opens. A supervisor keeps one
// for its distro, so a restarting helper can't reset the budget.
type helperStderrLogger struct {
	distro string
	logf   func(format string, args ...any)
	now    func() time.Time

	mu          sync.Mutex
	windowStart time.Time
	windowBytes int
	dropped     int
}

func newHelperStderrLogger(distro string, logf func(string, ...any)) *helperStderrLogger {
	return &helperStderrLogger{distro: distro, logf: logf, now: time.Now}
}

// pump reads r until EOF. Lines longer than the cap are cut, and the rest
// of such a line is skipped.
func (l *helperStderrLogger) pump(r io.Reader) {
	br := bufio.NewReaderSize(r, helperStderrLineMax)
	for {
		line, isPrefix, err := br.ReadLine()
		if len(line) > 0 || (err == nil && !isPrefix) {
			l.line(line)
		}
		for isPrefix && err == nil { // skip the rest of an over-long line
			_, isPrefix, err = br.ReadLine()
		}
		if err != nil {
			l.flushDropped() // a helper that went quiet after a burst
			return
		}
	}
}

// flushDropped logs a pending dropped count now.
func (l *helperStderrLogger) flushDropped() {
	l.mu.Lock()
	dropped := l.dropped
	l.dropped = 0
	l.mu.Unlock()
	if dropped > 0 {
		l.logf("helper[%s]: %d bytes of stderr dropped over the rate limit", l.distro, dropped)
	}
}

// line logs one record. What it costs is what it logs: the escaped text
// plus the prefix, so a blank line still costs, and escaping can't carry a
// record past either cap.
func (l *helperStderrLogger) line(raw []byte) {
	if len(raw) > helperStderrLineMax {
		raw = raw[:helperStderrLineMax]
	}
	quoted := strconv.Quote(string(raw)) // escapes control characters
	if len(quoted) > helperStderrLineMax {
		quoted = quoted[:helperStderrLineMax-3] + "..."
	}
	cost := len(quoted) + len(l.distro) + len("helper[]: ")
	l.mu.Lock()
	now := l.now()
	if now.Sub(l.windowStart) >= time.Minute {
		if l.dropped > 0 {
			l.logf("helper[%s]: %d bytes of stderr dropped over the rate limit", l.distro, l.dropped)
		}
		l.windowStart, l.windowBytes, l.dropped = now, 0, 0
	}
	if l.windowBytes+cost > helperStderrPerMinute {
		l.dropped += cost
		l.mu.Unlock()
		return
	}
	l.windowBytes += cost
	l.mu.Unlock()
	l.logf("helper[%s]: %s", l.distro, quoted)
}
