package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteVersionMarker(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "solud")

	writeVersionMarker(bin, " 0.0.501 ")

	got, err := os.ReadFile(bin + ".version")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0.0.501\n" {
		t.Fatalf("marker = %q, want 0.0.501\\n", got)
	}
}

func TestWriteVersionMarkerSkipsDev(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "solud")

	writeVersionMarker(bin, "dev")

	if _, err := os.Stat(bin + ".version"); !os.IsNotExist(err) {
		t.Fatalf("dev marker should not exist: %v", err)
	}
}

func TestWaitForSuccessorBootTimesOut(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "solud.successor-ok")
	now := time.Unix(0, 0)
	if waitForSuccessorBootWithDeps(marker, 150*time.Millisecond, successorBootWaitDeps{
		now: func() time.Time { return now }, stat: os.Stat,
		sleep: func(d time.Duration) { now = now.Add(d) },
	}) {
		t.Fatal("expected timeout when marker never appears")
	}
}

func TestWaitForSuccessorBootSeesMarker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "solud.successor-ok")
	pollReached := make(chan struct{})
	releasePoll := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		done <- waitForSuccessorBootWithDeps(marker, 2*time.Second, successorBootWaitDeps{
			now:  func() time.Time { return time.Unix(0, 0) },
			stat: os.Stat,
			sleep: func(time.Duration) {
				close(pollReached)
				<-releasePoll
			},
		})
	}()
	select {
	case <-pollReached:
	case <-time.After(2 * time.Second):
		t.Fatal("successor wait did not reach its first missing-marker poll")
	}
	if err := os.WriteFile(marker, []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	close(releasePoll)
	if !receiveWorkflowBoot(t, done, "marker observation") {
		t.Fatal("expected to observe the marker once written")
	}
}

func TestWriteSuccessorBootMarkerRespectsEnv(t *testing.T) {
	// No env → no-op, no panic.
	t.Setenv("SOLU_SUCCESSOR_BOOT_MARKER", "")
	writeSuccessorBootMarker()

	marker := filepath.Join(t.TempDir(), "boot-ok")
	t.Setenv("SOLU_SUCCESSOR_BOOT_MARKER", marker)
	writeSuccessorBootMarker()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected marker written when env set: %v", err)
	}
}

// These are the current loopback launcher, its optional Windows route, and
// the legacy wildcard launcher. Only their primary address and two identity
// flags authorize updating the desktop supervisor's pidfile.
func managedWslArgsCases() []struct {
	name    string
	args    []string
	managed bool
} {
	return []struct {
		name    string
		args    []string
		managed bool
	}{
		{"current loopback", []string{"--addr", "127.0.0.1:8787", "--backend-id", "wsl", "--shell-platform", "windows-amd64"}, true},
		{"current NAT launcher", []string{"--addr", "127.0.0.1:8787", "--wsl-addr", "172.29.132.65:8787", "--relay", "wss://example.test/relay", "--backend-id", "wsl", "--shell-version", "0.0.1235", "--shell-platform", "windows-amd64"}, true},
		{"current loopback-only launcher", []string{"--addr", "127.0.0.1:8787", "--wsl-addr", "127.0.0.1:8787", "--backend-id", "wsl", "--shell-version", "0.0.1235", "--shell-platform", "windows-amd64"}, true},
		{"current equals flags", []string{"--shell-platform=windows-amd64", "--backend-id=wsl", "--wsl-addr=172.29.132.65:8787", "--addr=127.0.0.1:8787"}, true},
		{"legacy wildcard", []string{"--addr", "0.0.0.0:8787", "--relay", "wss://example.test/relay", "--backend-id=wsl", "--shell-version", "0.0.501", "--shell-platform", "windows-amd64"}, true},
		{"legacy equals flags", []string{"--addr=0.0.0.0:8787", "--backend-id=wsl", "--shell-platform=windows-amd64"}, true},
		{"standalone loopback", []string{"--addr", "127.0.0.1:8787"}, false},
		{"standalone wildcard", []string{"--addr", "0.0.0.0:8787"}, false},
		{"host backend", []string{"--addr", "127.0.0.1:8787", "--backend-id", "host", "--shell-platform", "windows-amd64"}, false},
		{"Windows backend", []string{"--addr", "127.0.0.1:8787", "--backend-id", "win", "--shell-platform", "windows-amd64"}, false},
		{"Linux shell", []string{"--addr", "127.0.0.1:8787", "--backend-id", "wsl", "--shell-platform", "linux-amd64"}, false},
		{"missing backend", []string{"--addr", "127.0.0.1:8787", "--shell-platform", "windows-amd64"}, false},
		{"missing shell platform", []string{"--addr", "127.0.0.1:8787", "--backend-id", "wsl"}, false},
		{"different port", []string{"--addr", "127.0.0.1:8788", "--backend-id", "wsl", "--shell-platform", "windows-amd64"}, false},
		{"interface primary", []string{"--addr", "172.29.132.65:8787", "--backend-id", "wsl", "--shell-platform", "windows-amd64"}, false},
		{"IPv6 primary", []string{"--addr", "[::1]:8787", "--backend-id", "wsl", "--shell-platform", "windows-amd64"}, false},
		{"secondary alone", []string{"--wsl-addr", "127.0.0.1:8787", "--backend-id", "wsl", "--shell-platform", "windows-amd64"}, false},
		{"address substring", []string{"--addr=127.0.0.1:8787-extra", "--backend-id", "wsl", "--shell-platform", "windows-amd64"}, false},
		{"empty address", []string{"--addr=", "--backend-id", "wsl", "--shell-platform", "windows-amd64"}, false},
	}
}

func TestIsManagedWslDaemonArgs(t *testing.T) {
	for _, tc := range managedWslArgsCases() {
		t.Run(tc.name, func(t *testing.T) {
			if got := isManagedWslDaemonArgs(tc.args); got != tc.managed {
				t.Fatalf("managed=%v, want %v for %q", got, tc.managed, tc.args)
			}
		})
	}
}

func TestUpdateManagedWslPidFile(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	for _, tc := range managedWslArgsCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "solud")
			pidFile := filepath.Join(dir, "solud-wsl.pid")
			if err := os.WriteFile(pidFile, []byte("old\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			os.Args = append([]string{"solud"}, tc.args...)
			for _, pid := range []int{12345, 67890} {
				updateManagedWslPidFile(bin, pid)
				got, err := os.ReadFile(pidFile)
				if err != nil {
					t.Fatal(err)
				}
				want := "old\n"
				if tc.managed {
					want = strconv.Itoa(pid) + "\n"
				}
				if string(got) != want {
					t.Fatalf("pidfile = %q, want %q", got, want)
				}
			}
		})
	}
}

// TestApplyBusyGate locks in the server-side update-safety gate: a non-forced
// Apply while the busy check reports a mid-turn Focus pane is refused with
// updateBusyError (mapped to 409 by the handler), and force=true bypasses the
// gate (proceeding to the next guard — here the no-manifest error — proving
// the busy gate was skipped, while the kill-switch path stays intact).
func TestApplyBusyGate(t *testing.T) {
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.restartGate = new(atomic.Bool)
	u.busyCheck = func() (bool, string, string) {
		return true, "a Focus pane is mid-turn (pane p1)", "p1"
	}

	err := u.Apply(context.Background(), func(int) {}, false)
	var busy *updateBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("non-forced Apply while busy = %v, want *updateBusyError", err)
	}
	if !strings.Contains(busy.reason, "pane p1") {
		t.Fatalf("busy reason = %q, want the offending pane surfaced", busy.reason)
	}
	if busy.cause != updateBusyCauseFocus || busy.paneID != "p1" {
		t.Fatalf("busy metadata = %+v, want focus cause + p1", busy)
	}

	err = u.Apply(context.Background(), func(int) {}, true)
	if errors.As(err, &busy) {
		t.Fatal("force=true still hit the busy gate; the explicit override must bypass it")
	}
	if err == nil || !strings.Contains(err.Error(), "no manifest") {
		t.Fatalf("forced Apply = %v, want the no-manifest guard (proof the gate was passed)", err)
	}
}

// TestApplyBusyGateNilCheckIsOpen: an updater with no busyCheck wired (tests,
// minimal setups) must not gate at all.
func TestApplyBusyGateNilCheckIsOpen(t *testing.T) {
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.restartGate = new(atomic.Bool)
	err := u.Apply(context.Background(), func(int) {}, false)
	var busy *updateBusyError
	if errors.As(err, &busy) {
		t.Fatalf("nil busyCheck gated the apply: %v", err)
	}
}

func TestApplyRestartReservationUsesBusyError(t *testing.T) {
	gate := new(atomic.Bool)
	gate.Store(true)
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.restartGate = gate
	err := u.Apply(context.Background(), func(int) {}, true)
	var busy *updateBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("Apply during memory-guard restart = %v, want *updateBusyError", err)
	}
	if busy.cause != updateBusyCauseRestart {
		t.Fatalf("restart busy cause = %q, want restart", busy.cause)
	}
}

func TestUpdateApplyHandlerReturns409DuringMemGuardRestart(t *testing.T) {
	gate := new(atomic.Bool)
	gate.Store(true)
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.restartGate = gate
	a := &apiServer{updater: u}
	req := httptest.NewRequest(http.MethodPost, "/api/update/apply?force=1", nil)
	rec := httptest.NewRecorder()
	a.updateApply(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		Busy  bool   `json:"busy"`
		Cause string `json:"cause"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Busy || body.Cause != updateBusyCauseRestart {
		t.Fatalf("restart busy response = %+v", body)
	}
}

func TestRestartSelfForMemGuardLogsReservationRefusal(t *testing.T) {
	gate := new(atomic.Bool)
	gate.Store(true)

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	err := restartSelfForMemGuardWithDeps("1.0.0", func(int) {
		t.Fatal("reservation refusal must not exit the daemon")
	}, memGuardRestartDeps{gate: gate})
	if err == nil {
		t.Fatal("reservation refusal returned nil")
	}
	if text := logs.String(); !strings.Contains(text, "[memguard]") || !strings.Contains(text, "restart") {
		t.Fatalf("reservation refusal log = %q, want a loud [memguard] restart error", text)
	}
}

func TestRestartSelfForMemGuardSystemdExitsImmediately(t *testing.T) {
	t.Setenv("INVOCATION_ID", "test-service")
	var exitCode int
	if err := restartSelfForMemGuardWithDeps("1.0.0", func(code int) { exitCode = code }, memGuardRestartDeps{gate: new(atomic.Bool)}); err != nil {
		t.Fatalf("systemd restart: %v", err)
	}
	if exitCode != ExitCodeRestartForUpdate {
		t.Fatalf("systemd exit code = %d, want %d", exitCode, ExitCodeRestartForUpdate)
	}
}

func TestRestartSelfForMemGuardSpawnFailureKeepsServing(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	var exits int
	deps := memGuardRestartDeps{
		gate:       new(atomic.Bool),
		executable: func() (string, error) { return filepath.Join(t.TempDir(), "solud"), nil },
		spawn: func(string, string, string) (*os.Process, error) {
			return nil, errors.New("os error 1455")
		},
	}
	err := restartSelfForMemGuardWithDeps("1.0.0", func(int) { exits++ }, deps)
	if err == nil || !strings.Contains(err.Error(), "1455") {
		t.Fatalf("spawn failure = %v, want os error 1455", err)
	}
	if exits != 0 {
		t.Fatalf("spawn failure called exitFn %d times, want current daemon left serving", exits)
	}
}

func TestRestartSelfForMemGuardExitsOnlyAfterHealthyHandshake(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	var events []string
	deps := memGuardRestartDeps{
		gate:       new(atomic.Bool),
		executable: func() (string, error) { return filepath.Join(t.TempDir(), "solud"), nil },
		spawn: func(string, string, string) (*os.Process, error) {
			events = append(events, "spawn")
			return &os.Process{Pid: 42}, nil
		},
		wait: func(string, time.Duration) bool {
			events = append(events, "healthy")
			return true
		},
		sleep: func(time.Duration) { events = append(events, "handoff") },
	}
	err := restartSelfForMemGuardWithDeps("1.0.0", func(int) { events = append(events, "exit") }, deps)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "spawn,healthy,handoff,exit" {
		t.Fatalf("restart event order = %q, want spawn,healthy,handoff,exit", got)
	}
}

func TestRestartDaemonFromCapturedBinaryHonorsCallerReservation(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	gate := new(atomic.Bool)
	gate.Store(true)
	var spawnedPath, spawnedVersion string
	exits := 0
	err := restartDaemonFromBinaryWithDeps("/installed/solud", "2.0.0", func(int) { exits++ }, memGuardRestartDeps{
		gate: gate,
		spawn: func(path, version, _ string) (*os.Process, error) {
			spawnedPath, spawnedVersion = path, version
			return &os.Process{Pid: 42}, nil
		},
		wait:  func(string, time.Duration) bool { return true },
		sleep: func(time.Duration) {},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if spawnedPath != "/installed/solud" || spawnedVersion != "2.0.0" || exits != 1 {
		t.Fatalf("captured handoff = path:%q version:%q exits:%d", spawnedPath, spawnedVersion, exits)
	}
	if !gate.Load() {
		t.Fatal("verified handoff released its caller-owned restart reservation")
	}
}

func TestRestartDaemonCapturedBinaryBootFailureKeepsPredecessorServing(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	gate := new(atomic.Bool)
	gate.Store(true)
	exits := 0
	err := restartDaemonFromBinaryWithDeps("/installed/solud", "2.0.0", func(int) { exits++ }, memGuardRestartDeps{
		gate:  gate,
		spawn: func(string, string, string) (*os.Process, error) { return &os.Process{Pid: 999999}, nil },
		wait:  func(string, time.Duration) bool { return false },
	}, true)
	if err == nil || !strings.Contains(err.Error(), "failed to boot") {
		t.Fatalf("boot failure = %v", err)
	}
	if exits != 0 || !gate.Load() {
		t.Fatalf("boot failure exits=%d reservation=%v", exits, gate.Load())
	}
}

// TestUpdateApplyHandler409 maps the busy refusal to HTTP: non-forced POST →
// 409 {busy:true, reason}, ?force=1 → past the gate (500 on no-manifest here).
func TestUpdateApplyHandler409(t *testing.T) {
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.restartGate = new(atomic.Bool)
	u.busyCheck = func() (bool, string, string) {
		return true, "a Focus pane is mid-turn (pane p9)", "p9"
	}
	a := &apiServer{updater: u}

	req := httptest.NewRequest(http.MethodPost, "/api/update/apply", nil)
	rec := httptest.NewRecorder()
	a.updateApply(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		Busy   bool   `json:"busy"`
		Cause  string `json:"cause"`
		Reason string `json:"reason"`
		PaneID string `json:"paneId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.Busy || body.Cause != updateBusyCauseFocus || body.PaneID != "p9" || !strings.Contains(body.Reason, "pane p9") {
		t.Fatalf("409 body = %q (err=%v), want typed focus hold", rec.Body.String(), err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/update/apply?force=1", nil)
	rec = httptest.NewRecorder()
	a.updateApply(rec, req)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "no manifest") {
		t.Fatalf("forced status = %d body=%q, want 500 no-manifest (gate bypassed)", rec.Code, rec.Body.String())
	}
}

func TestUpdateApplyHandler409WorkflowDrain(t *testing.T) {
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.restartGate = new(atomic.Bool)
	u.inflightCount = func() int { return 3 }
	u.mu.Lock()
	u.manifest = &Manifest{Version: "2.0.0", Platforms: map[string]ManifestPlatform{platformKeyCurrent(): {}}}
	u.status.Available = true
	u.mu.Unlock()
	a := &apiServer{updater: u}

	req := httptest.NewRequest(http.MethodPost, "/api/update/apply", nil)
	rec := httptest.NewRecorder()
	a.updateApply(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		Busy                    bool   `json:"busy"`
		Cause                   string `json:"cause"`
		Reason                  string `json:"reason"`
		PaneID                  string `json:"paneId"`
		WorkflowInflight        int    `json:"workflowInflight"`
		WorkflowDeferDeadlineMs int64  `json:"workflowDeferDeadlineMs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Busy || body.Cause != updateBusyCauseWorkflow || body.PaneID != "" || body.WorkflowInflight != 3 || body.WorkflowDeferDeadlineMs <= time.Now().UnixMilli() || !strings.Contains(body.Reason, "3 in-flight") {
		t.Fatalf("409 body = %+v, want typed workflow hold with inflight + deadline", body)
	}
}

func TestUpdateApplyHandler409WorkflowDeadlineDeferred(t *testing.T) {
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.restartGate = new(atomic.Bool)
	u.inflightCount = func() int { return 3 }
	u.mu.Lock()
	u.manifest = &Manifest{Version: "2.0.0", Platforms: map[string]ManifestPlatform{platformKeyCurrent(): {}}}
	u.status.Available = true
	u.drainStart = time.Now().Add(-u.drainWindow - time.Second)
	u.mu.Unlock()
	rec := httptest.NewRecorder()
	(&apiServer{updater: u}).updateApply(rec, httptest.NewRequest(http.MethodPost, "/api/update/apply", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Deferred    bool  `json:"deferred"`
		Inflight    int   `json:"inflight"`
		NextRetryMs int64 `json:"nextRetryMs"`
		Attempts    int   `json:"attempts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Deferred || body.Inflight != 3 || body.Attempts != 1 || body.NextRetryMs <= time.Now().UnixMilli() {
		t.Fatalf("defer body=%+v", body)
	}
	if u.WorkflowDrainPending() {
		t.Fatal("deferred API response left admission paused")
	}
}

func TestApplyUpdateAutomaticWorkflowDeferralIsLosslessOnCurrentMain(t *testing.T) {
	t.Setenv("INVOCATION_ID", "test-systemd-unit")
	payload := []byte("quiet automatic replacement")
	u, binaryURL := leaseTestUpdater(t, payload)
	u.metrics = NewMetrics()
	var downloads atomic.Int32
	u.httpClient = &http.Client{Transport: &drainProbeTransport{
		static: staticRoundTripper{binaryURL: payload}, binaryURL: binaryURL,
		onBinary: func() { downloads.Add(1) },
	}}
	workflowRunning := true
	u.inflightCount = func() int {
		if workflowRunning {
			return 1
		}
		return 0
	}
	u.mu.Lock()
	u.drainStart = time.Now().Add(-u.drainWindow - time.Second)
	u.mu.Unlock()

	rec := httptest.NewRecorder()
	(&apiServer{updater: u}).updateApply(rec, httptest.NewRequest(http.MethodPost, "/api/update/apply", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("automatic apply status = %d body=%s, want 409", rec.Code, rec.Body.String())
	}
	var body struct {
		Deferred    bool  `json:"deferred"`
		Inflight    int   `json:"inflight"`
		NextRetryMs int64 `json:"nextRetryMs"`
		Attempts    int   `json:"attempts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Deferred || body.Inflight != 1 || body.Attempts != 1 || body.NextRetryMs <= time.Now().UnixMilli() {
		t.Fatalf("automatic defer body = %+v", body)
	}
	if !workflowRunning {
		t.Fatal("ordinary automatic deferral interrupted the running workflow")
	}
	if downloads.Load() != 0 || u.metrics.UpdaterAppliesTotal.Load() != 0 {
		t.Fatalf("active-worker defer side effects: downloads=%d swaps=%d, want zero",
			downloads.Load(), u.metrics.UpdaterAppliesTotal.Load())
	}
	if u.WorkflowDrainPending() {
		t.Fatal("automatic deferral left workflow admission closed")
	}
	status := u.Status()
	if status.WorkflowDeferState != "backoff" || status.WorkflowNextRetryMs != body.NextRetryMs ||
		status.WorkflowDeferAttempts != 1 || status.WorkflowInflight != 1 {
		t.Fatalf("defer status = %+v, want truthful backoff state matching 409", status)
	}
	automaticApplyCalls := 0
	if err := runAutoApplyIfEligible(context.Background(), true, status, func(context.Context) error {
		automaticApplyCalls++
		return nil
	}); err != nil || automaticApplyCalls != 0 {
		t.Fatalf("automatic retry before nextRetryMs: err=%v calls=%d, want no call", err, automaticApplyCalls)
	}

	// Advance the updater's fake clock to the exposed retry boundary and let
	// the worker finish. The ordinary automatic path must now swap once.
	workflowRunning = false
	u.mu.Lock()
	u.nextRetryTime = time.Now().Add(-time.Millisecond)
	u.mu.Unlock()
	err := runAutoApplyIfEligible(context.Background(), true, u.Status(), func(ctx context.Context) error {
		automaticApplyCalls++
		return u.Apply(ctx, func(int) {}, false)
	})
	if err != nil {
		t.Fatalf("quiet automatic retry = %v", err)
	}
	if automaticApplyCalls != 1 || downloads.Load() != 1 || u.metrics.UpdaterAppliesTotal.Load() != 1 {
		t.Fatalf("quiet retry side effects: applies=%d downloads=%d swaps=%d, want exactly one each",
			automaticApplyCalls, downloads.Load(), u.metrics.UpdaterAppliesTotal.Load())
	}
	self, err := u.executable()
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(self)
	if err != nil || string(installed) != string(payload) {
		t.Fatalf("installed binary = %q err=%v, want replacement payload", installed, err)
	}
	status = u.Status()
	if status.WorkflowDeferState != "" || status.WorkflowNextRetryMs != 0 || status.WorkflowDeferred {
		t.Fatalf("successful quiet retry retained defer state: %+v", status)
	}
}

// TestApplySwappedIsTerminal: after a successful swap, `swapped` closes the
// ~600ms drain window before os.Exit. A second Apply in that window (another
// device, or a reloaded page that lost its client-side guard) must be refused
// outright — even forced — instead of starting a redundant download over the
// already-swapped binary. Observed as a double download + double swap in e2e
// before this guard existed.
func TestApplySwappedIsTerminal(t *testing.T) {
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.restartGate = new(atomic.Bool)
	u.mu.Lock()
	u.swapped = true
	u.mu.Unlock()

	for _, force := range []bool{false, true} {
		err := u.Apply(context.Background(), func(int) {}, force)
		if err == nil || !strings.Contains(err.Error(), "already in progress") {
			t.Fatalf("Apply(force=%v) after swap = %v, want already-in-progress refusal", force, err)
		}
	}
}

func TestApplyRollsBackWhenSuccessorNeverSignalsBoot(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("SOLU_SUCCESSOR_OF_UPDATE", "1")

	payload := []byte("replacement that never reports boot")
	u, binaryURL := leaseTestUpdater(t, payload)
	u.httpClient = &http.Client{Transport: staticRoundTripper{binaryURL: payload}}
	bus := newUIBus()
	u.bus = bus
	events := bus.subscribe()
	defer bus.unsubscribe(events)

	u.commitSleepFn = func(time.Duration) {}
	var successorMarker string
	u.spawnSuccessorFn = func(_, _, marker string) (*os.Process, error) {
		successorMarker = marker
		t.Setenv("SOLU_SUCCESSOR_BOOT_MARKER", marker)
		// Exercise the same fatal startup path for the exact marker the
		// predecessor waits on; an unrelated preflight would not prove rollback.
		if ln, err := startupHandoffListener("invalid-listener-network", "unused"); err == nil || ln != nil {
			t.Fatalf("invalid successor preflight = (%v, %v), want fatal error", ln, err)
		}
		return &os.Process{Pid: 12345}, nil
	}
	u.waitSuccessorBootFn = func(marker string, timeout time.Duration) bool {
		if timeout != successorBootTimeout {
			t.Fatalf("successor timeout = %s, want %s", timeout, successorBootTimeout)
		}
		if marker == "" || marker != successorMarker {
			t.Fatalf("wait marker = %q, spawned marker %q", marker, successorMarker)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("failed successor unexpectedly signalled boot: %v", err)
		}
		return false
	}
	var killed atomic.Int32
	u.killSuccessorFn = func(proc *os.Process) error {
		if proc == nil || proc.Pid != 12345 {
			t.Fatalf("kill target = %+v", proc)
		}
		killed.Add(1)
		return nil
	}

	err := u.Apply(context.Background(), func(int) { t.Fatal("failed successor must not exit predecessor") }, true)
	if err == nil || !strings.Contains(err.Error(), "rolled back to 1.0.0") {
		t.Fatalf("Apply error = %v, want successor rollback", err)
	}
	if killed.Load() != 1 {
		t.Fatalf("successor kills = %d, want 1", killed.Load())
	}
	self, err := u.executable()
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(self)
	if err != nil || string(installed) != "test daemon binary\n" {
		t.Fatalf("restored binary = %q err=%v", installed, err)
	}
	versionBytes, err := os.ReadFile(self + ".version")
	if err != nil || strings.TrimSpace(string(versionBytes)) != "1.0.0" {
		t.Fatalf("restored version marker = %q err=%v", versionBytes, err)
	}
	for _, artifact := range []string{self + ".new", self + ".old", successorMarker} {
		if _, err := os.Stat(artifact); !os.IsNotExist(err) {
			t.Errorf("rollback artifact remains at %s: %v", artifact, err)
		}
	}
	u.mu.Lock()
	swapped := u.swapped
	u.mu.Unlock()
	if swapped {
		t.Fatal("failed successor left swapped=true")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		select {
		case event := <-events:
			if event.Type == "update-aborted" {
				return
			}
		case <-ctx.Done():
			t.Fatalf("update-aborted was not published: %v", ctx.Err())
		}
	}
}
