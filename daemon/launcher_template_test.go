package main

// The WSL launcher (start-wsl.sh) is generated from a Rust template in
// desktop/src-tauri/src/daemon.rs and is the outermost thing keeping a WSL
// backend alive: a shell mistake there takes the whole backend down, and the
// only evidence of it would have been in the log the mistake just destroyed.
//
// The template has its own Rust unit tests, and `desktop check` now runs them.
// These tests are the cross-language guard next to that: they assert on the
// shipped source of truth (so a template edited into a different shape is
// caught even by someone with no Rust toolchain or Tauri system deps) and they
// EXECUTE the generated shell, because "rotation is best-effort under set -e"
// is a claim only bash can settle.
//
// The template is reconstructed here rather than mocked. If daemon.rs is
// reformatted into a shape this reader does not understand, these tests fail
// loudly instead of silently passing over a template they never read — which
// is the correct signal: the launcher moved, re-check the launcher tests.

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const wslLauncherSource = "../desktop/src-tauri/src/daemon.rs"

// testLogMaxBytes is the cap bound to the template's {logmax} when rendering
// for these tests. The shipped value is DAEMON_LOG_MAX_BYTES (8 MiB); using a
// small stand-in keeps the size-gate test from writing megabytes, and
// TestWSLLauncherSizeGateUsesTheSharedCap pins the real wiring separately.
const testLogMaxBytes = "4096"

// readLauncherSource returns desktop/src-tauri/src/daemon.rs.
func readLauncherSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(wslLauncherSource)
	if err != nil {
		t.Fatalf("read %s: %v", wslLauncherSource, err)
	}
	return string(raw)
}

// unescapeRustString turns the body of a Rust string literal into the bytes it
// denotes. Only the escapes the template actually uses are decoded; anything
// else is passed through verbatim so an unrecognised escape shows up in the
// rendered script (and in bash -n) rather than being silently swallowed.
func unescapeRustString(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		switch s[i+1] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte(c)
			continue // not an escape we decode; keep the backslash as-is.
		}
		i++
	}
	return b.String()
}

// renderWSLStartScript reconstructs the start-wsl.sh text from the concat!
// literal inside wsl_start_script_body.
func renderWSLStartScript(t *testing.T) string {
	t.Helper()
	src := readLauncherSource(t)

	fnAt := strings.Index(src, "fn wsl_start_script_body(")
	if fnAt < 0 {
		t.Fatalf("%s no longer defines wsl_start_script_body", wslLauncherSource)
	}
	concatAt := strings.Index(src[fnAt:], "concat!(")
	if concatAt < 0 {
		t.Fatalf("wsl_start_script_body no longer builds its body with concat!")
	}
	lines := strings.Split(src[fnAt+concatAt:], "\n")
	if len(lines) > 0 {
		lines = lines[1:] // skip the concat!( line itself
	}

	var b strings.Builder
	pieces, closed := 0, false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == ")," {
			closed = true
			break
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if !strings.HasPrefix(trimmed, `"`) {
			t.Fatalf("unexpected non-literal line inside the template concat!: %q", line)
		}
		var content string
		switch {
		case strings.HasSuffix(trimmed, `",`):
			content = trimmed[1 : len(trimmed)-2]
		case strings.HasSuffix(trimmed, `"`):
			content = trimmed[1 : len(trimmed)-1]
		default:
			t.Fatalf("unterminated string literal inside the template concat!: %q", line)
		}
		b.WriteString(unescapeRustString(content))
		pieces++
	}
	if !closed {
		t.Fatal("template concat! is never closed; the reader lost its place")
	}

	body := b.String()
	// Rust format! doubles literal braces, so undo that before substituting
	// the named arguments (whose braces are single).
	body = strings.ReplaceAll(body, "{{", "{")
	body = strings.ReplaceAll(body, "}}", "}")
	for placeholder, value := range map[string]string{
		"{log}":       "daemon.log",
		"{logmax}":    testLogMaxBytes,
		"{src_amd64}": "/mnt/c/a/solud-linux-amd64.gz",
		"{src_arm64}": "/mnt/c/b/solud-linux-arm64.gz",
		// {relay} is the whole `--relay <url> ` argument pair (or nothing in
		// a build without a hosted cloud), as desktop/src-tauri renders it.
		"{relay}":     "--relay https://relay.example ",
		"{backend}":   "wsl",
		"{shellver}":  "0.0.0",
		"{daemonver}": "0.0.0",
		"{shellplat}": "windows-amd64",
		// How the desktop app tells the launcher what its own routes to a
		// WSL daemon looked like (WSL_WINDOWS_ROUTES_ENV / _NONE in daemon.rs).
		"{routes_env}":  "SOLU_WINDOWS_ROUTES",
		"{routes_none}": "none",
	} {
		body = strings.ReplaceAll(body, placeholder, value)
	}

	// Cheap proof the reader understood what it read, so a mangled render
	// cannot masquerade as a broken launcher (or vice versa).
	if pieces < 100 {
		t.Fatalf("template rendered from only %d literal pieces; the reader lost the body", pieces)
	}
	if !strings.HasPrefix(body, "#!/bin/bash\nset -e\n") {
		t.Fatalf("rendered template does not start like the launcher:\n%.120s", body)
	}
	return body
}

// extractShellFunc lifts one shell function out of the rendered script so it
// can be run on its own.
func extractShellFunc(t *testing.T, body, name string) string {
	t.Helper()
	head := name + "() {\n"
	start := strings.Index(body, head)
	if start < 0 {
		t.Fatalf("%s is not defined in the generated start-wsl.sh", name)
	}
	rest := body[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("%s has no closing brace", name)
	}
	return rest[:end+3]
}

// runBash runs a snippet, skipping the test where bash is unavailable.
func runBash(t *testing.T, script string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	return string(out), err
}

// TestWSLLauncherNeverTruncatesTheDaemonLog is the original defect: the
// supervisor opened its log with a truncating `exec >`, so every session
// destroyed the record of the one before it and the only run you could read
// about was the one that had not gone wrong yet. `exec > "` and `exec >> "`
// differ by one character, which is exactly how it shipped.
//
// The bash -n check rides along here because a syntax error in this template
// bricks the WSL backend outright and nothing else in the Go build would see it.
func TestWSLLauncherNeverTruncatesTheDaemonLog(t *testing.T) {
	t.Parallel()
	body := renderWSLStartScript(t)

	if strings.Contains(body, `exec > "`) {
		t.Error("start-wsl.sh still opens its log with a TRUNCATING redirect")
	}
	if !strings.Contains(body, `exec >> "$DAEMON_DIR/daemon.log" 2>&1`) {
		t.Error("start-wsl.sh must append to daemon.log")
	}
	if !strings.Contains(body, "=== $(date -Is) starting soluterminal WSL daemon ===") {
		t.Error("the session start banner must survive")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "start-wsl.sh")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("generated start-wsl.sh is not valid bash: %v\n%s", err, out)
	}
}

// TestWSLLauncherRotationSurvivesHostileDaemonDir executes the claim that
// rotation is best-effort. The script runs under `set -e`, so a bare `rm`/`mv`
// that fails on a read-only, full, or otherwise hostile $DAEMON_DIR aborts the
// supervisor itself: the WSL backend goes dark until someone restarts the app
// by hand, and the evidence lost is the failure that caused it.
//
// Directories standing where the log files belong make `rm -f` fail with
// EISDIR (even as root) and `mv -f` fail onto a non-empty target — portable
// stand-ins for the read-only/quota failures we cannot simulate directly.
func TestWSLLauncherRotationSurvivesHostileDaemonDir(t *testing.T) {
	t.Parallel()
	rotate := extractShellFunc(t, renderWSLStartScript(t), "rotate_daemon_log")

	for _, tc := range []struct {
		name   string
		poison []string
	}{
		// Only the oldest generation is un-removable...
		{"oldest generation only", []string{"daemon.log.2"}},
		// ...and the case where every `mv` fails too.
		{"every generation", []string{"daemon.log", "daemon.log.1", "daemon.log.2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"daemon.log", "daemon.log.1", "daemon.log.2"} {
				path := filepath.Join(dir, name)
				poisoned := false
				for _, p := range tc.poison {
					if p == name {
						poisoned = true
					}
				}
				if poisoned {
					if err := os.MkdirAll(filepath.Join(path, "blocker"), 0o755); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.WriteFile(path, []byte("some log bytes\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			out, err := runBash(t, "set -e\nDAEMON_DIR='"+dir+"'\n"+rotate+"\nrotate_daemon_log\necho SURVIVED\n")
			if err != nil || !strings.Contains(out, "SURVIVED") {
				t.Fatalf("rotation aborted the supervisor on a hostile DAEMON_DIR: %v\n%s", err, out)
			}
		})
	}
}

// TestWSLLauncherRelaunchRotationIsSizeGated holds the crash-relaunch path to
// appending. A boot-crash loop runs 10 attempts 2s apart; rotating on each one
// would shift the session's own preamble — the staging decisions, the stale
// daemon cleanup, the start banner and the FIRST failure — out through three
// generations inside six seconds, deleting the only part anyone reads a crash
// log for. The cap is the one thing allowed to move the file aside.
func TestWSLLauncherRelaunchRotationIsSizeGated(t *testing.T) {
	t.Parallel()
	body := renderWSLStartScript(t)

	// The relaunch branch itself: gated rotation, never unconditional, and it
	// must re-point stdout or the supervisor keeps writing down its old fd
	// into the renamed daemon.log.1.
	start := strings.Index(body, "daemon exited (status=$status); relaunching in 2s")
	if start < 0 {
		t.Fatal("crash-relaunch branch is missing")
	}
	rest := body[start:]
	end := strings.Index(rest, "  launch_daemon\n")
	if end < 0 {
		t.Fatal("the crash-relaunch branch never relaunches")
	}
	branch := rest[:end]
	if !strings.Contains(branch, "rotate_daemon_log_if_big") {
		t.Errorf("the relaunch must rotate through the size gate:\n%s", branch)
	}
	for _, line := range strings.Split(branch, "\n") {
		if strings.TrimSpace(line) == "rotate_daemon_log" {
			t.Errorf("the relaunch must NOT rotate unconditionally:\n%s", branch)
		}
	}
	if !strings.Contains(branch, `exec >> "$DAEMON_DIR/daemon.log" 2>&1`) {
		t.Errorf("the relaunch must re-point stdout at the live file:\n%s", branch)
	}

	// And the gate, executed: under the cap the log is left exactly as it was.
	rotate := extractShellFunc(t, body, "rotate_daemon_log")
	gate := extractShellFunc(t, body, "rotate_daemon_log_if_big")
	run := func(t *testing.T, size int) (dir string, out string, err error) {
		t.Helper()
		dir = t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "daemon.log"), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err = runBash(t, "set -e\nDAEMON_DIR='"+dir+"'\n"+rotate+"\n"+gate+"\nrotate_daemon_log_if_big\necho DONE\n")
		return dir, out, err
	}

	t.Run("under the cap the session log is kept whole", func(t *testing.T) {
		dir, out, err := run(t, 1024)
		if err != nil || !strings.Contains(out, "DONE") {
			t.Fatalf("size gate failed under the cap: %v\n%s", err, out)
		}
		if st, err := os.Stat(filepath.Join(dir, "daemon.log")); err != nil || st.Size() != 1024 {
			t.Fatalf("an under-cap log was rotated away: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "daemon.log.1")); err == nil {
			t.Fatal("a crash relaunch rotated an under-cap log, evicting the session preamble")
		}
	})

	t.Run("over the cap it still rotates", func(t *testing.T) {
		dir, out, err := run(t, 8192)
		if err != nil || !strings.Contains(out, "DONE") {
			t.Fatalf("size gate failed over the cap: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "daemon.log.1")); err != nil {
			t.Fatalf("an over-cap log was never rotated: %v", err)
		}
	})
}

// TestWSLLauncherSizeGateUsesTheSharedCap keeps the gate wired to
// DAEMON_LOG_MAX_BYTES. A hard-coded number here would age one platform's logs
// out faster than the other's for no stated reason.
func TestWSLLauncherSizeGateUsesTheSharedCap(t *testing.T) {
	t.Parallel()
	src := readLauncherSource(t)
	if !strings.Contains(src, "logmax = DAEMON_LOG_MAX_BYTES,") {
		t.Error("the WSL size gate must be bound to DAEMON_LOG_MAX_BYTES, not a literal")
	}
	if !strings.Contains(src, `"  if [ \"$size\" -ge {logmax} ]; then rotate_daemon_log; fi\n",`) {
		t.Error("the WSL size gate no longer compares the live log against {logmax}")
	}
}

// TestWSLLauncherRefreshPrecedesDaemonReuse pins the deployment property that
// made the orphan fix fail on a machine that HAD installed it.
//
// start-wsl.sh is written by the desktop shell, and every path in
// ensure_daemon_running_wsl that finds an already-running daemon returns before
// the spawn path writes it. So a shell upgrade could ship a new launcher (with
// the stop_port_holder sweep, say) and the running app would proxy to the
// healthy daemon and never rewrite the file — the fix sat installed and unused
// until the daemon happened to be down at app start.
//
// The refresh must therefore happen at the top of the function, before the
// reuse return, and it must actually write the script.
func TestWSLLauncherRefreshPrecedesDaemonReuse(t *testing.T) {
	t.Parallel()
	src := readLauncherSource(t)

	fn := strings.Index(src, "pub fn ensure_daemon_running_wsl")
	if fn < 0 {
		t.Fatal("ensure_daemon_running_wsl is missing from daemon.rs")
	}
	rest := src[fn:]

	refresh := strings.Index(rest, "resolve_wsl_linux_bundles(app)")
	if refresh < 0 {
		t.Fatal("ensure_daemon_running_wsl no longer resolves the launcher bundles")
	}
	reuse := strings.Index(rest, "reusing existing WSL daemon")
	if reuse < 0 {
		t.Fatal("the WSL daemon reuse path is missing")
	}
	if refresh > reuse {
		t.Errorf("the launcher is refreshed AFTER the reuse return (refresh=%d reuse=%d), "+
			"so an upgraded shell would proxy to an already-running daemon and never write "+
			"its new start-wsl.sh", refresh, reuse)
	}

	// Resolving is not enough: the early path has to write the file.
	if !strings.Contains(rest[refresh:reuse], "write_wsl_start_script(") {
		t.Error("the launcher refresh resolves bundles but never writes start-wsl.sh before reuse")
	}
	// The spawn path must still fail loudly rather than exec with no launcher.
	if !strings.Contains(rest, "write startup script:") {
		t.Error("the spawn path lost its loud launcher-write failure")
	}
}

// TestWSLLauncherRetiresAConflictingSystemdUnit pins the fix for a machine that
// carries BOTH install paths: this desktop app, and the Linux one-liner's
// `systemd --user` unit — both wanting :8787. The app kills the unit's daemon
// to take the port and systemd's Restart=always resurrects it, so the user sees
// panes that never stop reconnecting. The desktop app owns the daemon, so the
// launcher retires the unit's autostart — scoped to that one unit, best-effort,
// and WITHOUT requiring systemd (which is the property that keeps a desktop
// install free of per-machine manual setup).
func TestWSLLauncherRetiresAConflictingSystemdUnit(t *testing.T) {
	t.Parallel()
	body := renderWSLStartScript(t)

	if !strings.Contains(body, "retire_conflicting_systemd_unit() {") {
		t.Fatal("the launcher no longer retires a conflicting systemd unit")
	}
	if !strings.Contains(body, "\nretire_conflicting_systemd_unit\n") {
		t.Error("the retire function is defined but never called")
	}
	// Scoped to the single unit we actually conflict with.
	if !strings.Contains(body, `unit="$HOME/.config/systemd/user/soluterminal.service"`) {
		t.Error("the conflict check must name the exact unit it retires")
	}
	// Systemd-optional: the autostart symlink is dropped directly, and
	// systemctl is consulted only when it exists.
	if !strings.Contains(body, `wants="$HOME/.config/systemd/user/default.target.wants/soluterminal.service"`) {
		t.Error("the launcher must drop the wants symlink without needing systemd")
	}
	if !strings.Contains(body, "command -v systemctl") {
		t.Error("systemctl must be guarded so a systemd-less host still boots")
	}
	if !strings.Contains(body, " -L \"$wants\" ") {
		t.Error("only an existing autostart symlink may be removed")
	}
	// Best-effort: the launcher runs under `set -e`, so a failed retire must
	// cost a warning rather than the whole backend.
	retire := extractShellFunc(t, body, "retire_conflicting_systemd_unit")
	for _, line := range strings.Split(retire, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "rm ") || strings.HasPrefix(trimmed, "systemctl ") {
			if !strings.Contains(trimmed, "|| true") {
				t.Errorf("retire step can abort the launcher under set -e: %s", trimmed)
			}
		}
	}
	if !strings.Contains(retire, "  return 0\n") {
		t.Error("retire_conflicting_systemd_unit must return 0 so a false test cannot trip set -e")
	}
}

// TestWSLLauncherStopsRelaunchingWhenTheDataDirIsOwned executes the generated
// supervisor loop. Status 73 means another daemon already serves this data
// directory; ten relaunches 2s apart cannot change that and would bury the
// one log line that explains it. Any other exit is still relaunched.
func TestWSLLauncherStopsRelaunchingWhenTheDataDirIsOwned(t *testing.T) {
	t.Parallel()
	body := renderWSLStartScript(t)
	start := strings.Index(body, "restarts=0\nwhile true; do\n")
	if start < 0 {
		t.Fatal("supervisor relaunch loop is missing")
	}
	end := strings.Index(body[start:], "\ndone\n")
	if end < 0 {
		t.Fatal("supervisor relaunch loop never closes")
	}
	loop := body[start : start+end+len("\ndone\n")]

	run := func(t *testing.T, statuses string) (string, string, error) {
		t.Helper()
		dir := t.TempDir()
		stubs := strings.Join([]string{
			"set -e",
			"DAEMON_DIR='" + dir + "'",
			"PIDFILE='" + filepath.Join(dir, "solud-wsl.pid") + "'",
			"STATUSES=(" + statuses + ")",
			"N=0",
			"daemon_pid=1",
			"await_daemon() { status=\"${STATUSES[$N]}\"; N=$((N + 1)); }",
			"launch_daemon() { echo LAUNCHED; }",
			"stop_port_holder() { :; }",
			"rotate_daemon_log_if_big() { :; }",
			"sleep() { :; }",
		}, "\n") + "\n"
		out, err := runBash(t, stubs+loop)
		logged, _ := os.ReadFile(filepath.Join(dir, "daemon.log"))
		return out, string(logged), err
	}

	t.Run("owned data directory is final", func(t *testing.T) {
		out, logged, err := run(t, "73")
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 73 {
			t.Fatalf("supervisor exit = %v, want 73\n%s%s", err, out, logged)
		}
		if strings.Contains(out+logged, "LAUNCHED") {
			t.Fatalf("an owned data directory must not be relaunched:\n%s%s", out, logged)
		}
		if !strings.Contains(out+logged, "not relaunching") {
			t.Fatalf("the reason must be logged:\n%s%s", out, logged)
		}
	})

	t.Run("a crash is still relaunched", func(t *testing.T) {
		out, logged, err := run(t, "1 73")
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 73 {
			t.Fatalf("supervisor exit = %v, want 73 after one relaunch\n%s%s", err, out, logged)
		}
		if got := strings.Count(out+logged, "LAUNCHED"); got != 1 {
			t.Fatalf("a crash must be relaunched exactly once here, got %d:\n%s%s", got, out, logged)
		}
	})
}

// TestWSLLauncherSuccessorHandoffIsRotationFree guards the frozen half of this
// change. A self-update successor is the SAME run continuing under a new pid,
// not a relaunch: rotating there would split one daemon's life across two files
// for no reason, and touching the block at all risks the pidfile handoff.
func TestWSLLauncherSuccessorHandoffIsRotationFree(t *testing.T) {
	t.Parallel()
	body := renderWSLStartScript(t)

	start := strings.Index(body, "supervising update successor pid=")
	if start < 0 {
		t.Fatal("successor handoff block is missing")
	}
	rest := body[start:]
	end := strings.Index(rest, "continue")
	if end < 0 {
		t.Fatal("successor handoff has no continue")
	}
	if block := rest[:end]; strings.Contains(block, "rotate_daemon_log") {
		t.Errorf("the successor handoff must not rotate:\n%s", block)
	}

	for _, needle := range []string{
		`PIDFILE="$DAEMON_DIR/solud-wsl.pid"`,
		"await_daemon() {",
		"launch_daemon() {",
		`while kill -0 "$daemon_pid" 2>/dev/null; do`,
		"supervising newer update successor pid=",
		"daemon_is_child=0",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("start-wsl.sh lost a frozen handoff primitive: %s", needle)
		}
	}
}

// TestHostNativeDaemonLogKeepsTwoGenerations pins the host-native supervisor to
// the same bound as the WSL launcher and the daemon's own solud.log. With a
// single archived generation, two crashes in a row overwrite the first one's
// tail — and a crash loop is exactly when the FIRST failure is the one worth
// having. The executing oracle for this is the Rust unit test
// daemon_log_rotation_tests, which `desktop check` now runs; this is the
// source-shape guard that holds without a Rust toolchain.
func TestHostNativeDaemonLogKeepsTwoGenerations(t *testing.T) {
	t.Parallel()
	src := readLauncherSource(t)

	fnAt := strings.Index(src, "fn rotate_daemon_log_file(")
	if fnAt < 0 {
		t.Fatal("host-native rotation helper rotate_daemon_log_file is missing")
	}
	end := strings.Index(src[fnAt:], "\n}\n")
	if end < 0 {
		t.Fatal("rotate_daemon_log_file has no closing brace")
	}
	fn := src[fnAt : fnAt+end]

	shiftDown := strings.Index(fn, "rename(&gen1, &gen2)")
	archive := strings.Index(fn, "rename(path, &gen1)")
	switch {
	case shiftDown < 0:
		t.Error("host-native rotation drops straight to one generation: no .1 -> .2 shift")
	case archive < 0:
		t.Error("host-native rotation never archives the live log to .1")
	case shiftDown > archive:
		t.Error("host-native rotation archives before shifting, overwriting the older generation")
	}
	if !strings.Contains(fn, "remove_file(&gen2)") {
		t.Error("host-native rotation must drop the oldest generation, or generations are unbounded")
	}
	if !strings.Contains(src, "rotate_daemon_log_file(&path, DAEMON_LOG_MAX_BYTES);") {
		t.Error("open_daemon_log must rotate at the shared cap before appending")
	}
}

// TestDaemonLogCapDocDescribesTwoGenerations holds the DAEMON_LOG_MAX_BYTES doc
// comment to what the code around it actually does.
//
// This is not pedantry about prose. That constant's doc is the only description
// of the retention bound anyone reads before writing a crash-log collector or a
// disk-cleanup script, and it used to say a single generation was kept. Acting
// on that — pruning everything but daemon.log.1 — throws away the oldest
// archive, which in a crash loop is the FIRST failure: precisely the one the
// second generation was added to preserve.
func TestDaemonLogCapDocDescribesTwoGenerations(t *testing.T) {
	t.Parallel()
	src := readLauncherSource(t)

	constAt := strings.Index(src, "const DAEMON_LOG_MAX_BYTES:")
	if constAt < 0 {
		t.Fatal("DAEMON_LOG_MAX_BYTES is gone; re-check what replaced the shared cap")
	}
	// Walk backwards over the contiguous run of /// lines above the const. The
	// final split element is the (empty) text on the const's own line, so the
	// scan starts one above it.
	lines := strings.Split(src[:constAt], "\n")
	var doc []string
	for i := len(lines) - 2; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "///") {
			break
		}
		doc = append([]string{line}, doc...)
	}
	if len(doc) == 0 {
		t.Fatal("DAEMON_LOG_MAX_BYTES has no doc comment; the retention bound must stay written down")
	}
	blob := strings.Join(doc, "\n")

	if !strings.Contains(blob, "daemon.log.2") {
		t.Errorf("DAEMON_LOG_MAX_BYTES doc never mentions the second retained generation (daemon.log.2); a reader will prune it:\n%s", blob)
	}
	for _, wrong := range []string{"one generation is kept", "a single generation is kept"} {
		if strings.Contains(strings.ToLower(blob), wrong) {
			t.Errorf("DAEMON_LOG_MAX_BYTES doc still claims %q, but rotate_daemon_log_file keeps .1 AND .2:\n%s", wrong, blob)
		}
	}
}

// --- Supervision admission: keep a healthy daemon, replace only a bad one ---
//
// The 2026-09-24 kill loop: the Windows shell's short-budget probe missed a
// slow-but-healthy WSL daemon, ran start-wsl.sh, and the script's sweep killed
// that daemon on sight. Each app start also left one more supervisor behind,
// and they took turns killing each other's daemons (30 forced restarts in one
// morning). These tests execute the generated admission logic against a real
// HTTP daemon stand-in, because "healthy daemons are kept" is only true if the
// shell actually reads the daemon's own version out of /api/health, whose
// fleet section repeats "version" and "pid" for every pane agent.

// fakeWSLDaemon serves /api/health like solud: top-level identity plus a fleet
// whose components carry their own, different, versions and pids.
func fakeWSLDaemon(t *testing.T, backend, version string, pid int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"backendID":%q,"fleet":{"components":[{"pid":1,"version":"0.0.1"},{"pid":2,"version":"99.0.0","note":"a \"quoted\" {brace}"}]},"pid":%d,"system":{"version":"8.8.8"},"version":%q,"webSourceSha":"x","webVersion":"y"}`, backend, pid, version)
	}))
	t.Cleanup(srv.Close)
	return "0.0.0.0:" + strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)
}

// admissionScript assembles the generated admission functions into a runnable
// snippet with its inputs pointed at a sandbox: the daemon address, the bundled
// version, and a lock/pidfile/HOME that belong to the test.
func admissionScript(t *testing.T, dir, addr, bundleVersion string) string {
	t.Helper()
	body := renderWSLStartScript(t)
	var b strings.Builder
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "HOME='%s'\nADDR='%s'\nBUNDLE_VERSION='%s'\n", dir, addr, bundleVersion)
	fmt.Fprintf(&b, "SUPERVISOR_SCRIPT='%s'\nSUPERVISOR_LOCK='%s'\nSUPERVISOR_PIDFILE='%s'\n",
		filepath.Join(dir, "never-running", "start-wsl.sh"), filepath.Join(dir, "start-wsl.lock"), filepath.Join(dir, "start-wsl.pid"))
	for _, fn := range []string{"ver_gt", "rollback_hold_active", "json_top_value", "probe_live_daemon",
		"live_daemon_is_current", "live_daemon_reachable_from_windows", "live_daemon_visible_here",
		"live_daemon_is_our_binary_here", "stop_pid", "is_supervisor_pid", "retire_stray_supervisors",
		"claim_supervision"} {
		b.WriteString(extractShellFunc(t, body, fn))
		b.WriteString("\n")
	}
	return b.String()
}

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable: %v", tool, err)
		}
	}
}

func TestWSLLauncherProbeReadsTheDaemonsOwnVersion(t *testing.T) {
	t.Parallel()
	requireTools(t, "bash", "timeout", "awk")
	addr := fakeWSLDaemon(t, "wsl", "1.2.3", 4242)
	out, err := runBash(t, admissionScript(t, t.TempDir(), addr, "1.2.3")+
		"probe_live_daemon\necho \"pid=$LIVE_PID backend=$LIVE_BACKEND version=$LIVE_VERSION\"\n")
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pid=4242 backend=wsl version=1.2.3") {
		t.Fatalf("probe read a nested fleet value instead of the daemon's own identity:\n%s", out)
	}
}

// The 2026-11-12 failure: another program had started `solud --backend-id wsl`
// with no listen flags, so it answered on WSL's loopback only. The desktop app
// connects to the eth0 address under NAT and could never reach it, yet the
// launcher adopted it or left it alone and the app waited out its whole
// timeout on every launch. With the app's own verdict that no route of its
// accepted a connection, the launcher must not adopt such a daemon; without
// that verdict (an older app, or a route that did open) it adopts as before.
func TestWSLLauncherDoesNotAdoptADaemonTheWindowsAppCannotReach(t *testing.T) {
	t.Parallel()
	requireTools(t, "bash", "timeout", "awk", "flock")
	daemon := exec.Command("sleep", "60")
	if err := daemon.Start(); err != nil {
		t.Skipf("cannot start stand-in process: %v", err)
	}
	t.Cleanup(func() { _ = daemon.Process.Kill(); _ = daemon.Wait() })
	pid := daemon.Process.Pid
	addr := fakeWSLDaemon(t, "wsl", "2.0.0", pid)
	// A port nothing listens on stands in for the eth0 address.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot reserve a port: %v", err)
	}
	unreachable := "127.0.0.2:" + strconv.Itoa(closed.Addr().(*net.TCPAddr).Port)
	_ = closed.Close()

	for _, tc := range []struct {
		routes  string
		adopted bool
	}{
		{routes: "none", adopted: false},
		{routes: "open", adopted: true},
		{routes: "", adopted: true},
	} {
		out, err := runBash(t, admissionScript(t, t.TempDir(), addr, "2.0.0")+
			// The socket-owner check comes from a constant outside the template.
			"pid_listens_on_port() { return 1; }\n"+
			fmt.Sprintf("WSL_ADDR='%s'\nWINDOWS_ROUTES='%s'\nLOCAL=/nonexistent/solud\n", unreachable, tc.routes)+
			"claim_supervision\necho \"ADOPT=[$ADOPT_PID]\"\n")
		if err != nil {
			t.Fatalf("routes=%q: admission aborted: %v\n%s", tc.routes, err, out)
		}
		adopted := strings.Contains(out, fmt.Sprintf("ADOPT=[%d]", pid))
		if adopted != tc.adopted {
			t.Fatalf("routes=%q: adopted=%v, want %v:\n%s", tc.routes, adopted, tc.adopted, out)
		}
		if !tc.adopted && !strings.Contains(out, "where the Windows app connects; not adopting it") {
			t.Fatalf("routes=%q: the launcher did not say why it would not adopt:\n%s", tc.routes, out)
		}
		// The stand-in is not the app's own staged binary, so it is never stopped.
		if err := daemon.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("routes=%q: a daemon that is not the app's own binary was stopped: %v", tc.routes, err)
		}
	}
}

func TestWSLLauncherAdoptsAHealthyUnsupervisedDaemon(t *testing.T) {
	t.Parallel()
	requireTools(t, "bash", "timeout", "awk", "flock")
	// A real, visible process stands in for the orphaned-but-healthy daemon.
	daemon := exec.Command("sleep", "60")
	if err := daemon.Start(); err != nil {
		t.Skipf("cannot start stand-in process: %v", err)
	}
	t.Cleanup(func() { _ = daemon.Process.Kill(); _ = daemon.Wait() })
	pid := daemon.Process.Pid
	addr := fakeWSLDaemon(t, "wsl", "2.0.0", pid)

	out, err := runBash(t, admissionScript(t, t.TempDir(), addr, "2.0.0")+
		"claim_supervision\necho \"ADOPT=[$ADOPT_PID]\"\n")
	if err != nil {
		t.Fatalf("admission aborted: %v\n%s", err, out)
	}
	if !strings.Contains(out, fmt.Sprintf("ADOPT=[%d]", pid)) || !strings.Contains(out, "adopting it instead of restarting it") {
		t.Fatalf("a healthy, current daemon with no supervisor must be adopted, not replaced:\n%s", out)
	}
	if err := daemon.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the adopted daemon was killed: %v", err)
	}
}

func TestWSLLauncherLeavesAHealthyDaemonToItsSupervisor(t *testing.T) {
	t.Parallel()
	requireTools(t, "bash", "timeout", "awk", "flock")
	dir := t.TempDir()
	lock := filepath.Join(dir, "start-wsl.lock")
	// Another supervisor holds the lock for its whole life.
	holder := exec.Command("flock", lock, "sleep", "60")
	if err := holder.Start(); err != nil {
		t.Skipf("cannot hold the lock: %v", err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for exec.Command("flock", "-n", lock, "true").Run() == nil {
		if time.Now().After(deadline) {
			t.Fatal("stand-in supervisor never took the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	addr := fakeWSLDaemon(t, "wsl", "3.0.0", 1)

	out, err := runBash(t, admissionScript(t, dir, addr, "3.0.0")+"claim_supervision\necho WENT-ON-TO-KILL\n")
	if err != nil {
		t.Fatalf("a second launch must exit 0 and leave things alone, got %v:\n%s", err, out)
	}
	if strings.Contains(out, "WENT-ON-TO-KILL") || !strings.Contains(out, "leaving it alone") {
		t.Fatalf("a second launch went past a supervised, healthy daemon:\n%s", out)
	}
}

func TestWSLLauncherReplacesAnOlderDaemon(t *testing.T) {
	t.Parallel()
	requireTools(t, "bash", "timeout", "awk", "flock")
	addr := fakeWSLDaemon(t, "wsl", "1.0.0", 1)
	out, err := runBash(t, admissionScript(t, t.TempDir(), addr, "2.0.0")+
		"claim_supervision\necho \"ADOPT=[$ADOPT_PID]\"\n")
	if err != nil {
		t.Fatalf("admission aborted: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ADOPT=[]") || !strings.Contains(out, "version 1.0.0 (bundle 2.0.0); replacing it") {
		t.Fatalf("a daemon older than the bundle must be replaced after an upgrade:\n%s", out)
	}
}

func TestWSLLauncherColdStartLaunchesFresh(t *testing.T) {
	t.Parallel()
	requireTools(t, "bash", "timeout", "awk", "flock")
	// A port nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	out, err := runBash(t, admissionScript(t, t.TempDir(), "0.0.0.0:"+strconv.Itoa(port), "1.0.0")+
		"claim_supervision\necho \"ADOPT=[$ADOPT_PID]\"\n")
	if err != nil || !strings.Contains(out, "ADOPT=[]") || strings.Contains(out, "replacing") {
		t.Fatalf("with no daemon the launcher must proceed to a fresh start: %v\n%s", err, out)
	}
}

// TestWSLLauncherExportsNodeToolchainPath executes the generated PATH prelude
// against a minimal incoming PATH and a fake home toolchain, then proves the
// daemon child would inherit ~/.local/bin, Volta/asdf paths, and nvm's selected
// default node bin. The bash -n coverage for the full script rides in
// TestWSLLauncherNeverTruncatesTheDaemonLog.
func TestWSLLauncherExportsNodeToolchainPath(t *testing.T) {
	t.Parallel()
	body := renderWSLStartScript(t)

	start := strings.Index(body, `BASE_PATH="$PATH"`)
	if start < 0 {
		t.Fatal("the generated launcher no longer establishes a node-toolchain PATH")
	}
	end := strings.Index(body[start:], "DAEMON_DIR=")
	if end < 0 {
		t.Fatal("the PATH prelude never reaches DAEMON_DIR")
	}
	prelude := body[start : start+end]
	for _, needle := range []string{
		`export PATH="$HOME/.local/bin:$HOME/.volta/bin:$BASE_PATH"`,
		`"$HOME/.asdf/asdf.sh"`,
		`"$HOME/.nvm/nvm.sh"`,
		`nvm use default`,
		`BASE_PATH="$PATH"`,
	} {
		if !strings.Contains(prelude, needle) {
			t.Errorf("PATH prelude missing %q:\n%s", needle, prelude)
		}
	}

	home := t.TempDir()
	for _, dir := range []string{".local/bin", ".volta/bin", ".asdf/bin", ".nvm"} {
		if err := os.MkdirAll(filepath.Join(home, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, ".nvm", "versions", "node", "v18.0.0", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".asdf", "asdf.sh"), []byte("export PATH=\"$HOME/.asdf/bin:$PATH\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nvm := "nvm() {\n  if [ \"$1\" = \"use\" ]; then export PATH=\"$HOME/.nvm/versions/node/v18.0.0/bin:$PATH\"; fi\n  return 0\n}\n"
	if err := os.WriteFile(filepath.Join(home, ".nvm", "nvm.sh"), []byte(nvm), 0o644); err != nil {
		t.Fatal(err)
	}
	minimal := t.TempDir()

	script := "set -e\nexport HOME='" + home + "'\nexport PATH='" + minimal + "'\n" + prelude + "\n/bin/bash -c 'printf %s \"$PATH\"'\n"
	out, err := runBash(t, script)
	if err != nil {
		t.Fatalf("generated PATH prelude failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".volta", "bin"),
		filepath.Join(home, ".asdf", "bin"),
		filepath.Join(home, ".nvm", "versions", "node", "v18.0.0", "bin"),
		minimal,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("daemon-child PATH %q is missing %q", out, want)
		}
	}
	if !strings.HasPrefix(out, filepath.Join(home, ".nvm", "versions", "node", "v18.0.0", "bin")) {
		t.Errorf("nvm's selected default node bin must lead the daemon PATH: %q", out)
	}
}
