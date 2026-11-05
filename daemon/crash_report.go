package main

// Crash/error telemetry: ship recovered panics to the cloud (redacted) so panics
// across the fleet are VISIBLE instead of dying silently in a local log nobody
// can read. Homegrown (not Sentry) on purpose — the product promise is "secrets
// never leave the daemon", and Go panic values/stacks routinely embed paths and
// token-shaped strings; a homegrown sink keeps every byte in infra we control and
// runs the same redaction the log ring uses (daemon/redact.go).
//
// Two-layer rate limiting protects our own ingest from a crash-looping daemon:
// here (per-fingerprint cooldown + hourly ceiling) and again cloud-side per
// machine. Reports are fire-and-forget; a dropped report is a lost data point,
// not worth a retry queue.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"soluterminal/grid"

	"soluterminal-shared/machineauth"
)

const (
	crashPerFingerprintCooldown = 5 * time.Minute
	crashMaxPerHour             = 20
)

// crashSink holds the cloud creds (set once integrations come up, refreshed on
// token rotation) plus the daemon-side rate-limiter state.
type crashSink struct {
	mu        sync.Mutex
	cloudBase string
	token     string
	lastSent  map[string]time.Time // fingerprint -> last send time
	sentHour  int                  // sends in the current rolling hour
	hourStart time.Time
}

var globalCrashSink = &crashSink{lastSent: map[string]time.Time{}}

// init wires the grid sub-package's panic hook to the cloud reporter. grid can't
// import package main, so it exposes a func var we populate here. Safe before
// creds exist — reportPanicToCloud no-ops until setCreds runs.
func init() {
	grid.OnPanic = func(site string, recovered any) { reportPanicToCloud(site, recovered) }
}

func (s *crashSink) setCreds(base, token string) {
	s.mu.Lock()
	s.cloudBase = base
	s.token = token
	s.mu.Unlock()
}

type crashReportPayload struct {
	Version     string `json:"version"`
	Site        string `json:"site"`
	Kind        string `json:"kind"` // "panic" | "error"
	Message     string `json:"message"`
	Stack       string `json:"stack"`
	Fingerprint string `json:"fingerprint"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	GoVersion   string `json:"go_version"`
	OccurredAt  string `json:"occurred_at"`
	// RecentLogs is the tail of the in-memory log ring (already redacted at
	// capture) so a crash arrives with the log context that led up to it —
	// the difference between a debuggable report and a bare stack trace.
	RecentLogs string `json:"recent_logs"`
}

const (
	crashRecentLogLines = 50
	crashRecentLogBytes = 8 * 1024
)

// recentLogContext returns the redacted tail of the log ring, bounded in size,
// keeping the most-recent bytes (closest to the crash).
func recentLogContext() string {
	s := strings.Join(globalLogRing.tail(crashRecentLogLines), "\n")
	if len(s) > crashRecentLogBytes {
		s = s[len(s)-crashRecentLogBytes:]
	}
	return s
}

// reportPanicToCloud sends a redacted panic report. It does NOT log locally — the
// recover() sites already log with their own site-specific wording, so this only
// adds the cloud hop. Safe from any goroutine; never blocks the caller on the
// network; never re-panics.
func reportPanicToCloud(site string, recovered any) {
	stack := redactStack(debug.Stack())
	globalCrashSink.report(site, "panic", redactPanicValue(recovered), stack)
}

// reportErrorToCloud sends a redacted non-panic error of interest. Unused at the
// recover sites (they're panics) but available for explicit error telemetry.
func reportErrorToCloud(site string, err error) {
	if err == nil {
		return
	}
	globalCrashSink.report(site, "error", redactSensitive(err.Error()), "")
}

// guard wraps a long-lived goroutine body so an otherwise-unhandled panic is
// captured (logged + reported) instead of crashing the process. Use at the TOP
// of new goroutines; existing recover() sites instead call reportPanicToCloud
// directly so their established log wording is preserved.
func guard(site string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic recovered", "site", site, "panic", r)
			reportPanicToCloud(site, r)
		}
	}()
	fn()
}

func (s *crashSink) report(site, kind, message, stack string) {
	if !crashReportsAllowedNow() {
		return // switched off in Settings or by SOLU_DISABLE_CRASH_REPORTS
	}
	fingerprint := crashFingerprint(site, stack)
	s.mu.Lock()
	base, token := s.cloudBase, s.token
	if base == "" || token == "" {
		s.mu.Unlock()
		return // local-only daemon, nowhere to report
	}
	now := time.Now()
	if s.hourStart.IsZero() || now.Sub(s.hourStart) > time.Hour {
		s.hourStart = now
		s.sentHour = 0
	}
	if s.sentHour >= crashMaxPerHour {
		s.mu.Unlock()
		return
	}
	if last, ok := s.lastSent[fingerprint]; ok && now.Sub(last) < crashPerFingerprintCooldown {
		s.mu.Unlock()
		return // same crash already reported recently — send the first, drop the loop
	}
	s.lastSent[fingerprint] = now
	s.sentHour++
	s.mu.Unlock()

	// The recent log tail can mention project folders and pane titles, so
	// only team installs attach it. Everyone's report has user folders
	// replaced by [path]; the app's own source files and ~/.solu stay.
	recentLogs := ""
	if crashLogTailAllowedNow() {
		recentLogs = scrubUserPaths(recentLogContext())
	}
	p := crashReportPayload{
		Version:     version,
		Site:        site,
		Kind:        kind,
		Message:     scrubUserPaths(message),
		Stack:       scrubUserPaths(stack),
		Fingerprint: fingerprint,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		GoVersion:   runtime.Version(),
		OccurredAt:  now.UTC().Format(time.RFC3339),
		RecentLogs:  recentLogs,
	}
	go postCrashReport(base, token, p)
}

// crashFingerprint groups crashes stably by site + the top few normalized stack
// frames (instruction offsets and the goroutine header stripped), so the cloud
// can GROUP BY fingerprint. Function names + file:line are not secret; the value
// is hashed anyway.
func crashFingerprint(site, stack string) string {
	var keep []string
	for _, ln := range strings.Split(stack, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "goroutine ") {
			continue
		}
		if i := strings.Index(t, " +0x"); i >= 0 {
			t = t[:i] // drop the "+0x1a4" instruction offset (varies per build)
		}
		keep = append(keep, t)
		if len(keep) >= 6 { // ~3 frames (func line + file:line each)
			break
		}
	}
	h := sha256.Sum256([]byte(site + "\n" + strings.Join(keep, "\n")))
	return hex.EncodeToString(h[:8])
}

func postCrashReport(cloudBase, token string, p crashReportPayload) {
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	req, err := http.NewRequest("POST", cloudBase+"/api/internal/crash-report", bytes.NewReader(body))
	if err != nil {
		return
	}
	machineauth.SignRequest(req, token, body, time.Now())
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("crash-report POST failed", "err", err)
		return
	}
	_ = resp.Body.Close()
}

// ---------------------------------------------------------------------------
// Fatal-death reporting via the shell watchdog.
//
// Everything above only fires from recover() sites — panics the daemon
// SURVIVED. A daemon that actually dies (unrecovered panic, log.Fatalf, OOM,
// an external kill) reports nothing, by construction: the process that knows
// what happened no longer exists. The one party that reliably notices is the
// shell watchdog that respawns us, so it passes the observation to the NEW
// daemon via env (SOLU_RESPAWNED_AFTER = a one-line description of the death,
// SOLU_PREV_LOG = the daemon.log both generations share), and the new daemon
// files the report on its predecessor's behalf once cloud creds come up.
// ---------------------------------------------------------------------------

// watchdogRespawnEvidence is what the shell told us about our predecessor's
// death, captured once at boot.
type watchdogRespawnEvidence struct {
	reason  string
	logPath string
}

// captureWatchdogRespawnEvidence reads and then UNSETS the watchdog's env
// breadcrumbs. The unset matters: pane agents and update successors inherit
// our env, and a stale SOLU_RESPAWNED_AFTER would make every one of them
// re-confess a crash that already got reported.
func captureWatchdogRespawnEvidence() *watchdogRespawnEvidence {
	reason := strings.TrimSpace(os.Getenv("SOLU_RESPAWNED_AFTER"))
	logPath := strings.TrimSpace(os.Getenv("SOLU_PREV_LOG"))
	_ = os.Unsetenv("SOLU_RESPAWNED_AFTER")
	_ = os.Unsetenv("SOLU_PREV_LOG")
	if reason == "" {
		return nil
	}
	return &watchdogRespawnEvidence{reason: reason, logPath: logPath}
}

// reportWatchdogRespawn files the predecessor's death with the cloud. Called
// after the crash sink has creds. The attached "stack" is the tail of the
// shared daemon.log — for a Go fatal that is the actual panic traceback, the
// exact evidence that used to evaporate with the process. Redacted like every
// other crash payload; capped so a chatty log can't bloat the report.
func reportWatchdogRespawn(ev *watchdogRespawnEvidence) {
	if ev == nil {
		return
	}
	const tailCap = 4 * 1024
	tail := ""
	if ev.logPath != "" {
		if b, err := os.ReadFile(ev.logPath); err == nil {
			if len(b) > tailCap {
				b = b[len(b)-tailCap:]
			}
			tail = redactSensitive(string(b))
		}
	}
	if !crashLogTailAllowedNow() {
		// The shared daemon.log holds more than the traceback; keep only the
		// Go runtime's own panic lines for public installs.
		tail = goTracebackLines(tail)
	}
	globalCrashSink.report("daemon-death", "fatal",
		"daemon died and was respawned by the shell watchdog: "+redactSensitive(ev.reason), tail)
}

// reUserPath finds absolute or home-relative paths that start a word (after a
// space, quote, '=', '(' or '['), so Go function names such as
// "soluterminal/grid.(*X).y" are left alone.
var reUserPath = regexp.MustCompile(`(^|[\s"'=(\[])((?:~|[A-Za-z]:)?[\\/][^\s"'` + "`" + `:,;()<>\[\]{}]+)`)

// scrubUserPaths replaces paths that could name a user's own folders with
// "[path]". Module-relative Go source files (official builds use -trimpath), its data folder
// (~/.solu) and API routes are kept because they are what makes a report
// debuggable and say nothing about the user.
func scrubUserPaths(s string) string {
	return reUserPath.ReplaceAllStringFunc(s, func(m string) string {
		sub := reUserPath.FindStringSubmatch(m)
		lead, path := sub[1], sub[2]
		trimmed := strings.TrimRight(path, ".")
		if isAppOwnPath(trimmed) {
			return m
		}
		return lead + "[path]" + path[len(trimmed):]
	})
}

func isAppOwnPath(p string) bool {
	switch {
	case strings.HasPrefix(p, "~/.solu"), strings.HasPrefix(p, "~\\.solu"):
		return true
	case strings.HasPrefix(p, "/api/"):
		return true
	}
	return false
}

// goTracebackLines keeps only the Go runtime's panic output from a log tail:
// the panic/fatal header, goroutine headers, function frames and their
// file:line rows. Everything else in the log (pane titles, folders, prompts)
// is dropped.
func goTracebackLines(tail string) string {
	var keep []string
	inTrace := false
	for _, ln := range strings.Split(tail, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "panic:"), strings.HasPrefix(t, "fatal error:"),
			strings.HasPrefix(t, "goroutine "), strings.HasPrefix(t, "runtime:"):
			inTrace = true
			keep = append(keep, scrubUserPaths(t))
		case inTrace && t == "":
			inTrace = false
		case inTrace && (strings.HasPrefix(ln, "\t") || strings.HasSuffix(t, ")") || strings.Contains(t, ".go:")):
			keep = append(keep, scrubUserPaths(ln))
		}
	}
	return strings.Join(keep, "\n")
}
