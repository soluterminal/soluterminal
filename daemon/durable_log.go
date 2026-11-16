package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Durable daemon logging.
//
// The daemon's stderr is whatever its launcher happened to attach, and every
// launcher gets this wrong in a different way:
//
//   - The WSL start script redirects the supervisor's stdout/stderr to
//     daemon.log. Useful, but only for processes the supervisor forked.
//   - A self-update successor is forked by the OUTGOING daemon, not by the
//     supervisor, so it does NOT inherit that redirect. spawnSuccessor points
//     it at <bindir>/successor.log instead — which is truncated by the next
//     update apply, sits next to the binary rather than with the other logs,
//     and is therefore not where anyone looks. That is how the 2026-08-16
//     exit read as zero evidence: daemon.log stops dead at the moment of the
//     update apply, and the successor's own words are somewhere else.
//   - A manual `solud` run logs to whatever terminal is attached, which is
//     gone by the time anyone asks what happened.
//
// So the daemon stops depending on its launcher and opens its own log. On
// boot it tees every slog record into an append-mode, size-rotated file it
// owns, and points the Go runtime's crash output at the same file so an
// UNRECOVERED panic — the one class of death the in-process crash reporter
// can never catch — writes its dying words somewhere that survives.
//
// This is additive: records still reach os.Stderr (supervisor redirect,
// systemd journal) and still land in the in-memory ring the crash reporter
// samples. Nothing that worked before stops working.

// durableLogMaxBytes caps the live log before it rotates. Matches the Tauri
// shell's DAEMON_LOG_MAX_BYTES so both halves of the same story age out at
// the same rate.
//
// The bound is "cap plus at most one record": a record is never split across
// a rotation, so a single oversized record lands whole in an empty live file
// and is rotated out by the next write. Splitting would corrupt the one thing
// this file exists to preserve — a JSON record or a panic traceback torn in
// half reads as garbage — so the record wins and the cap gives by one write.
const durableLogMaxBytes int64 = 8 << 20

// durableLogName is the file the daemon writes itself, deliberately distinct
// from the launcher's daemon.log so the two streams never interleave.
const durableLogName = "solud.log"

// soluShellDataDir mirrors the Tauri shell's solu_data_dir(): Windows gets
// %LOCALAPPDATA%\soluterminal, everything else ~/.local/share/soluterminal
// (honouring XDG_DATA_HOME). Co-locating solud.log with the launcher's
// daemon.log keeps "where are the daemon logs?" to one answer per platform.
//
// Deliberately NOT the --data dir (~/.solu): this has to resolve before flag
// parsing, and a bad --data must not cost us the log that would explain it.
// The one exception is a daemon given a non-default --data dir (a side
// daemon); see resolveDurableLogDir.
func soluShellDataDir() (string, error) {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "soluterminal"), nil
		}
		return "", fmt.Errorf("no LOCALAPPDATA")
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "soluterminal"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "soluterminal"), nil
}

// resolveDurableLogDir picks the directory solud.log lives in.
//
//   - SOLU_DURABLE_LOG_DIR (envDir) wins everywhere. Callers handle "off".
//   - A side daemon (daemon_role.go: not on 8787/38787, or a --data dir other
//     than the default ~/.solu), on any OS, logs into <data>/logs; with no
//     --data that is the default data dir's logs folder, never the real
//     install's file. Side daemons are dev, test, capture or local Preview
//     daemons. Without this a side daemon
//     wrote into the real install's solud.log whenever it ran with the real
//     HOME: on Windows LOCALAPPDATA never follows the data dir, and on Linux
//     the shell data dir moves only with a scratch HOME (12 Nov, a capture
//     daemon on :18899 wrote into the real ~/.local/share/soluterminal log).
//     <data>/logs is the same name launchd already uses and the
//     fresh-install check tolerates.
//   - Otherwise (the primary daemon: every real install, which listens on a
//     canonical port and never passes --data) the log stays co-located with
//     the shell's daemon.log in soluShellDataDir.
//
// addr is the -addr value ("" means the flag's default, 127.0.0.1:8787).
// Pure apart from shellDir so the decision is testable on any host.
func resolveDurableLogDir(goos, envDir, addr, dataDir, defaultData string, shellDir func() (string, error)) (string, error) {
	if envDir = strings.TrimSpace(envDir); envDir != "" {
		return envDir, nil
	}
	if strings.TrimSpace(addr) == "" {
		addr = defaultDaemonAddr
	}
	if strings.TrimSpace(dataDir) == "" {
		dataDir = defaultData
	}
	if !decideDaemonRole(goos, addr, dataDir, defaultData).Primary {
		return filepath.Join(dataDir, "logs"), nil
	}
	return shellDir()
}

// sameDataDir compares two data directories after making them absolute and
// clean; Windows paths compare case-insensitively.
func sameDataDir(goos, a, b string) bool {
	norm := func(p string) string {
		p = strings.TrimSpace(p)
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return filepath.Clean(p)
	}
	x, y := norm(a), norm(b)
	if goos == "windows" {
		return strings.EqualFold(x, y)
	}
	return x == y
}

// durableLogDataFlag finds the --data value in the daemon's arguments.
func durableLogDataFlag(args []string) (string, bool) { return durableLogFlag(args, "data") }

// durableLogFlag finds a flag's value in the daemon's arguments. The durable
// log is attached before flag parsing (see soluShellDataDir), so it scans
// for it directly: -name/--name, with the value either attached (=) or as
// the next argument, stopping at "--". It cannot know which other flags take
// values, so it does not stop at non-flag arguments (the value of --addr
// would otherwise end the scan); the last occurrence wins, as it does in the
// flag package. A miss means the flag's default.
func durableLogFlag(args []string, flagName string) (string, bool) {
	value, found := "", false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		name, ok := strings.CutPrefix(a, "--")
		if !ok {
			if name, ok = strings.CutPrefix(a, "-"); !ok {
				continue
			}
		}
		if v, ok := strings.CutPrefix(name, flagName+"="); ok {
			value, found = v, true
			continue
		}
		if name == flagName && i+1 < len(args) {
			i++
			value, found = args[i], true
		}
	}
	return value, found
}

// rotatingLogWriter is an append-mode io.Writer that keeps the live file
// under maxBytes and retains two archived generations (.1, .2). Rotation is
// generational rather than truncating for the same reason the shell script's
// is: the bytes worth having are the tail of the run that just crashed.
type rotatingLogWriter struct {
	path     string
	maxBytes int64

	// armCrashOutput is set once attachDurableLogAt has pointed the Go
	// runtime's crash output at this file. Rotation has to re-arm it —
	// debug.SetCrashOutput dups the fd, so after a rename the runtime would
	// otherwise keep writing into the archived .1 file forever. Off by
	// default so a bare rotatingLogWriter (tests, future callers) cannot
	// silently hijack the process's panic output.
	armCrashOutput bool

	mu   sync.Mutex
	f    *os.File
	size int64
}

// newRotatingLogWriter opens path for appending, rotating first if the file
// already sits at or over the cap so a boot never lands in an oversized file.
func newRotatingLogWriter(path string, maxBytes int64) (*rotatingLogWriter, error) {
	if maxBytes <= 0 {
		maxBytes = durableLogMaxBytes
	}
	w := &rotatingLogWriter{path: path, maxBytes: maxBytes}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.openLocked(); err != nil {
		return nil, err
	}
	if w.size >= w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return nil, err
		}
	}
	return w, nil
}

// openLocked opens (creating if needed) the live file and syncs the tracked
// size to what is already on disk.
func (w *rotatingLogWriter) openLocked() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w.f = f
	w.size = 0
	if st, err := f.Stat(); err == nil {
		w.size = st.Size()
	}
	return nil
}

// rotateLocked shifts generations (.1 -> .2, live -> .1) and reopens a fresh
// live file. Every filesystem step is best-effort: losing an archive is worth
// far less than losing the ability to keep logging.
func (w *rotatingLogWriter) rotateLocked() error {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	_ = os.Remove(w.path + ".2")
	_ = os.Rename(w.path+".1", w.path+".2")
	_ = os.Rename(w.path, w.path+".1")
	if err := w.openLocked(); err != nil {
		return err
	}
	if w.armCrashOutput {
		debug.SetCrashOutput(w.f, debug.CrashOptions{})
	}
	return nil
}

// Write appends p, rotating first when it would push the file over the cap.
//
// It never reports an error. This writer sits inside an io.MultiWriter behind
// slog.Default(): a returned error (or a short count) would abort the record
// for stderr too, so a full disk would cost us the console logs as well as
// the file. A record we cannot persist is dropped silently instead.
func (w *rotatingLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return len(p), nil
	}
	// The size>0 guard keeps a record that is itself bigger than the cap from
	// rotating an empty file forever (rotate, still too big, rotate...). It
	// costs the cap one oversized record, which the next write clears.
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return len(p), nil
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		return len(p), nil
	}
	return len(p), nil
}

// neverFailWriter forwards to w and swallows its errors.
//
// It exists for the stderr half of the durable fan-out. io.MultiWriter stops
// at the FIRST writer that errors, so an unwrapped os.Stderr that has gone bad
// — closed by the launcher, a pipe whose reader is gone, a dying fd inherited
// through a process-group teardown — would silence every writer behind it,
// including the durable file this whole feature exists to fill. A daemon whose
// console is broken is exactly the daemon whose log you will want.
type neverFailWriter struct{ w io.Writer }

func (n neverFailWriter) Write(p []byte) (int, error) {
	if n.w != nil {
		_, _ = n.w.Write(p)
	}
	return len(p), nil
}

// durableSinks fans every record out to the durable file FIRST and the console
// second, because order is what decides who survives whose failure: the file is
// the sink that must not depend on the launcher, so it goes ahead of the sink
// the launcher owns, and the console is wrapped so it can never abort the pair.
func durableSinks(durable, console io.Writer) io.Writer {
	return io.MultiWriter(durable, neverFailWriter{w: console})
}

// armCrash points the Go runtime's crash output at the live file and marks the
// writer so every later rotation re-arms it.
//
// Arm and re-arm run under the same lock rotation takes: releasing it between
// setting the flag and pointing the runtime at w.f leaves a window where a
// rotation re-arms the NEW file and we then aim the runtime back at the renamed
// one, sending the traceback to solud.log.1 forever after.
func (w *rotatingLogWriter) armCrash() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armCrashOutput = true
	if w.f != nil {
		debug.SetCrashOutput(w.f, debug.CrashOptions{})
	}
}

// Close releases the live file. Only tests need this — the daemon holds its
// durable log open for the life of the process on purpose.
func (w *rotatingLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// durableLog is the writer attachDurableLogAt installed, kept so the process
// keeps a handle on its own log after boot. Written once, on the boot path,
// before anything else in the daemon is running; nil whenever the durable log
// is disabled or could not be opened.
var durableLog *rotatingLogWriter

// attachDurableLog wires the durable log up for a long-running daemon.
//
// SOLU_DURABLE_LOG_DIR overrides the directory; the literal value "off"
// disables the durable log entirely (the escape hatch for tests and
// side-port dev daemons that must not scribble on the real file).
//
// Entirely best-effort. A logging problem must never be the reason a daemon
// fails to boot, so every failure is one warning and a return.
func attachDurableLog() {
	envDir := strings.TrimSpace(os.Getenv("SOLU_DURABLE_LOG_DIR"))
	if strings.EqualFold(envDir, "off") {
		return
	}
	dataDir, _ := durableLogDataFlag(os.Args[1:])
	addr, _ := durableLogFlag(os.Args[1:], "addr")
	dir, err := resolveDurableLogDir(runtime.GOOS, envDir, addr, dataDir, defaultDataDir(), soluShellDataDir)
	if err != nil {
		slog.Warn("durable log: no data dir; logging to stderr only", "err", err)
		return
	}
	if err := attachDurableLogAt(dir); err != nil {
		slog.Warn("durable log: unavailable; logging to stderr only", "dir", dir, "err", err)
	}
}

// attachDurableLogAt opens <dir>/solud.log, stamps a boot banner, and rebuilds
// the default logger so every record reaches stderr AND the file AND the
// in-memory ring. Finally it points the Go runtime's crash output at the same
// file: an unrecovered panic's traceback is written by the runtime to stderr,
// and stderr is precisely what a self-update successor no longer has.
func attachDurableLogAt(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, durableLogName)
	w, err := newRotatingLogWriter(path, durableLogMaxBytes)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}

	// Plain text, not slog: under LOG_FORMAT=json this line is the only thing
	// that visibly demarcates one run from the next when you open the file.
	fmt.Fprintf(w, "=== %s solud boot version=%s pid=%d args=%s ===\n",
		time.Now().Format(time.RFC3339), version, os.Getpid(), strings.Join(os.Args, " "))

	// Reuse buildLogHandler so LOG_FORMAT / LOG_LEVEL / the ring wrap can
	// never drift between the stderr-only path and this one.
	slog.SetDefault(slog.New(buildLogHandler(durableSinks(w, os.Stderr))))

	w.armCrash()

	durableLog = w
	return nil
}
