//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"soluterminal/helperproto"
)

func TestHelperBackoffSchedule(t *testing.T) {
	var b helperBackoff
	var got []time.Duration
	for i := 0; i < 8; i++ {
		got = append(got, b.after(time.Second))
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("delays %v", got)
		}
	}
	if d := b.after(helperHealthyReset); d != time.Second {
		t.Fatalf("after 10 healthy minutes the delay is %v, want 1s", d)
	}
}

func TestHelperStderrLimits(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	now := time.Unix(1000, 0)
	l := newHelperStderrLogger("Ubuntu", func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(f, a...)))
	})
	l.now = func() time.Time { return now }
	long := strings.Repeat("x", helperStderrLineMax+500)
	in := "plain\n" + "esc\x1b[2Jape\n" + long + "\n"
	for i := 0; i < 5; i++ {
		in += strings.Repeat("y", 15<<10) + "\n" // 75 KiB: past 64 KiB/min
	}
	l.pump(strings.NewReader(in))
	mu.Lock()
	n := len(lines)
	joined := strings.Join(lines, "\n")
	mu.Unlock()
	if !strings.Contains(joined, `"plain"`) || !strings.Contains(joined, `esc\x1b[2Jape`) {
		t.Fatalf("lines not logged escaped: %q", joined[:200])
	}
	for _, line := range lines {
		if strings.Count(line, "x") > helperStderrLineMax {
			t.Fatal("a line passed the 16 KiB cap")
		}
	}
	if n >= 8 {
		t.Fatalf("rate limit let all %d lines through", n)
	}
	now = now.Add(time.Minute)
	l.line([]byte("next window"))
	mu.Lock()
	tail := lines[len(lines)-2:]
	mu.Unlock()
	if !strings.Contains(tail[0], "bytes of stderr dropped") || !strings.Contains(tail[1], "next window") {
		t.Fatalf("dropped count not reported: %q", tail)
	}
}

// helperSupervisorFixture runs the real helper (this test binary as `solud
// helper`) in place of wsl.exe.
type helperSupervisorFixture struct {
	bin, home, xdg string
	sleeps         []time.Duration
	reattached     [][]helperproto.AgentEntry
	mu             sync.Mutex
}

func newHelperSupervisorFixture(t *testing.T) *helperSupervisorFixture {
	t.Helper()
	base := shortTempDir(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := &helperSupervisorFixture{bin: filepath.Join(base, "solud-test"), home: filepath.Join(base, "home"), xdg: filepath.Join(base, "run")}
	if err := os.Symlink(self, f.bin); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{f.home, f.xdg} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { killHelperTestAgents(filepath.Join(f.xdg, "soluterminal-helper")) })
	return f
}

func (f *helperSupervisorFixture) config(t *testing.T, version string) helperSupervisorConfig {
	return helperSupervisorConfig{
		Distro:  "test",
		Version: version,
		Place: func(context.Context) (helperPlacement, error) {
			return helperPlacement{BinPath: f.bin, UID: os.Getuid(), Arch: "x86_64"}, nil
		},
		Command: func(p helperPlacement) *exec.Cmd {
			cmd := exec.Command(p.BinPath, "helper", "--proto", "1")
			cmd.Env = []string{"HOME=" + f.home, "XDG_RUNTIME_DIR=" + f.xdg, "PATH=/usr/bin:/bin"}
			return cmd
		},
		OnSession: func(ctx context.Context, s *helperproto.HostSession) error {
			all, err := s.ListAll(ctx)
			f.mu.Lock()
			f.reattached = append(f.reattached, all)
			f.mu.Unlock()
			return err
		},
		Logf: t.Logf,
		Sleep: func(_ context.Context, d time.Duration) error {
			f.mu.Lock()
			f.sleeps = append(f.sleeps, d)
			f.mu.Unlock()
			return nil
		},
	}
}

func killHelperTestAgents(runtimeDir string) {
	recs, _ := filepath.Glob(filepath.Join(runtimeDir, "agents", "*.json"))
	h := &linuxHelper{}
	for _, r := range recs {
		id := strings.TrimSuffix(filepath.Base(r), ".json")
		if rec, ok := h.readRecord(runtimeDir, id); ok && rec.Pid > 0 {
			_ = syscall.Kill(rec.Pid, syscall.SIGKILL)
		}
	}
}

func TestHelperSupervisorRestartsAndReattaches(t *testing.T) {
	f := newHelperSupervisorFixture(t)
	sup := newHelperSupervisor(f.config(t, version))
	defer sup.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s1, err := sup.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := sup.Session(ctx); again != s1 {
		t.Fatal("a live session was replaced")
	}
	sp, err := s1.Spawn(ctx, helperproto.SpawnArgs{
		PaneID: "pane_0123456789abcdef", Argv: []string{"sleep", "300"}, Cwd: f.home, Cols: 80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The helper dies; the agent doesn't. The next Session restarts it after
	// the backoff and reattaches the agent through OnSession.
	sup.mu.Lock()
	pid := sup.cmd.Process.Pid
	sup.mu.Unlock()
	_ = syscall.Kill(pid, syscall.SIGKILL)
	select {
	case <-s1.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the session outlived its helper")
	}
	// No caller asks: the supervisor restarts the helper itself, so panes
	// already open get their agents back.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.reattached)
		f.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sup.mu.Lock()
	s2 := sup.sess
	sup.mu.Unlock()
	if s2 == nil || s2 == s1 || !s2.HelloPassed() {
		t.Fatal("no fresh session after the helper died")
	}
	f.mu.Lock()
	sleeps, reattached := append([]time.Duration(nil), f.sleeps...), f.reattached
	f.mu.Unlock()
	if len(sleeps) != 1 || sleeps[0] <= 0 || sleeps[0] > helperBackoffMin {
		t.Fatalf("restart waits %v, want one backoff of up to 1s", sleeps)
	}
	if len(reattached) != 2 || len(reattached[1]) != 1 || reattached[1][0].AgentID != sp.AgentID || !reattached[1][0].Alive {
		t.Fatalf("reattach lists: %+v", reattached)
	}
	if pane, ok := s2.PaneForAgent(sp.AgentID); !ok || pane != "pane_0123456789abcdef" {
		t.Fatal("the new session doesn't know the reattached agent")
	}
}

func TestHelperSupervisorRefusesAVersionMismatch(t *testing.T) {
	f := newHelperSupervisorFixture(t)
	sup := newHelperSupervisor(f.config(t, version+"-other"))
	defer sup.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := sup.Session(ctx); err == nil || !helperproto.IsBreak(err) {
		t.Fatalf("a helper of another version was accepted: %v", err)
	}
	sup.mu.Lock()
	cmd, waiting := sup.cmd, sup.waiting
	sup.mu.Unlock()
	if cmd != nil || time.Until(waiting) <= 0 {
		t.Fatal("a refused helper must be gone, with a backoff before the next try")
	}
}

func TestHelperSupervisorHandoffDrains(t *testing.T) {
	f := newHelperSupervisorFixture(t)
	sup := newHelperSupervisor(f.config(t, version))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := sup.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sup.mu.Lock()
	exited := sup.exited
	sup.mu.Unlock()
	if err := sup.Handoff(ctx); err != nil {
		t.Fatal(err)
	}
	// After the handoff this host never serves the distro again.
	if _, err := sup.Session(ctx); err != errHelperClosed {
		t.Fatalf("Session after handoff: %v", err)
	}
	if _, err := s.Spawn(ctx, helperproto.SpawnArgs{PaneID: "pane_0123456789abcdef", Argv: []string{"sh"}, Cols: 80, Rows: 24}); err == nil {
		t.Fatal("the drained helper still spawns")
	}
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		t.Fatal("the drained helper did not exit")
	}
}

// Without proof that an old helper is gone, no new helper starts and no
// handoff succeeds; a retry checks again.
func TestHelperStaleHelperBlocksStartAndHandoff(t *testing.T) {
	init1, _ := os.FindProcess(1) // can't be killed by this user: Kill fails
	placed := 0
	sup := newHelperSupervisor(helperSupervisorConfig{
		Distro: "x", ExitWait: 50 * time.Millisecond,
		Place: func(context.Context) (helperPlacement, error) {
			placed++
			return helperPlacement{}, errors.New("unused")
		},
	})
	exited := make(chan struct{}) // never closes until the end
	sup.mu.Lock()
	sup.retireLocked(helperChild{cmd: &exec.Cmd{Process: init1}, exited: exited})
	stale := len(sup.stale)
	sup.mu.Unlock()
	if stale != 1 {
		t.Fatal("an unconfirmed helper was forgotten")
	}
	if _, err := sup.Session(context.Background()); err != errHelperStillRunning || placed != 0 {
		t.Fatalf("a new helper started beside an old one: %v (placed %d)", err, placed)
	}
	for i := 0; i < 2; i++ {
		if err := sup.Handoff(context.Background()); err != errHelperStillRunning {
			t.Fatalf("handoff %d: %v", i, err)
		}
	}
	close(exited)
	if err := sup.Handoff(context.Background()); err != nil {
		t.Fatalf("after the exit was seen: %v", err)
	}
}

// A start hung in placement (a stuck wsl.exe probe) never holds the lock:
// Close cancels it and returns.
func TestHelperCloseAbortsAHungStart(t *testing.T) {
	entered := make(chan struct{})
	sup := newHelperSupervisor(helperSupervisorConfig{
		Distro: "x",
		Place: func(ctx context.Context) (helperPlacement, error) {
			close(entered)
			<-ctx.Done()
			return helperPlacement{}, ctx.Err()
		},
	})
	res := make(chan error, 1)
	go func() { _, err := sup.Session(context.Background()); res <- err }()
	<-entered
	closed := make(chan struct{})
	go func() { sup.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited on a hung start")
	}
	if err := <-res; err == nil {
		t.Fatal("the aborted start reported a session")
	}
}

// A caller that slept out one backoff must not start early when another
// caller's failed start pushed the deadline later meanwhile.
func TestHelperWaiterHonoursANewerDeadline(t *testing.T) {
	var sleeps []time.Duration
	var sup *helperSupervisor
	stop := errors.New("stop")
	sup = newHelperSupervisor(helperSupervisorConfig{
		Distro: "x",
		Place: func(context.Context) (helperPlacement, error) {
			t.Fatal("started before the newer deadline")
			return helperPlacement{}, nil
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			if len(sleeps) == 1 {
				sup.mu.Lock()
				sup.waiting = time.Now().Add(2 * time.Hour) // another caller failed
				sup.mu.Unlock()
				return nil
			}
			return stop
		},
	})
	sup.waiting = time.Now().Add(time.Hour)
	if _, err := sup.Session(context.Background()); err != stop {
		t.Fatalf("Session: %v", err)
	}
	if len(sleeps) != 2 || sleeps[1] <= sleeps[0] {
		t.Fatalf("sleeps %v: the newer deadline was skipped", sleeps)
	}
}

func TestHelperStderrBlankLinesCost(t *testing.T) {
	n, drops := 0, 0
	l := newHelperStderrLogger("Ubuntu", func(f string, a ...any) {
		if strings.Contains(fmt.Sprintf(f, a...), "bytes of stderr dropped") {
			drops++
			return
		}
		n++
	})
	l.now = func() time.Time { return time.Unix(1000, 0) }
	l.pump(strings.NewReader(strings.Repeat("\n", 100000)))
	if n == 0 || n > helperStderrPerMinute/len(`""helper[Ubuntu]: `) {
		t.Fatalf("%d blank-line records logged", n)
	}
	if drops != 1 {
		t.Fatalf("the drops of a burst that went quiet were reported %d times", drops)
	}
	// Escaping can't carry a record past the line cap.
	var got string
	l2 := newHelperStderrLogger("Ubuntu", func(f string, a ...any) { got = fmt.Sprintf(f, a...) })
	l2.line([]byte(strings.Repeat("\x01", helperStderrLineMax)))
	if len(got) > helperStderrLineMax+len("helper[Ubuntu]: ") {
		t.Fatalf("an escaped record of %d bytes", len(got))
	}
}

// A pane that gives up early must not cancel the start every pane shares.
func TestHelperShortCallerDeadlineKeepsTheSharedStart(t *testing.T) {
	f := newHelperSupervisorFixture(t)
	cfg := f.config(t, version)
	release := make(chan struct{})
	place := cfg.Place
	cfg.Place = func(ctx context.Context) (helperPlacement, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return helperPlacement{}, ctx.Err()
		}
		return place(ctx)
	}
	sup := newHelperSupervisor(cfg)
	defer sup.Close()
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := sup.Session(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short caller: %v", err)
	}
	close(release)
	ctx, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	s, err := sup.Session(ctx)
	if err != nil || !s.HelloPassed() {
		t.Fatalf("the shared start was lost: %v", err)
	}
	f.mu.Lock()
	sleeps := len(f.sleeps)
	f.mu.Unlock()
	if sleeps != 0 {
		t.Fatal("an abandoned wait added backoff")
	}
}

// A pane's Session call that notices the dead session first and then gives
// up must still leave a restart running (the PR review's must-fix).
func TestHelperRestartSurvivesACallerThatGivesUp(t *testing.T) {
	f := newHelperSupervisorFixture(t)
	sup := newHelperSupervisor(f.config(t, version))
	defer sup.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s1, err := sup.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Close() // the session ends; the helper exits on EOF
	<-s1.Done()
	gone, giveUp := context.WithCancel(context.Background())
	giveUp()
	_, _ = sup.Session(gone) // ends the session itself, then gives up
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		sup.mu.Lock()
		s2 := sup.sess
		sup.mu.Unlock()
		if s2 != nil && s2 != s1 && s2.HelloPassed() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("nothing restarted the helper")
}

// Two callers waiting on the same stale helper start one helper between
// them, never two (the PR review's must-fix).
func TestHelperStaleWaitersStartOnce(t *testing.T) {
	f := newHelperSupervisorFixture(t)
	cfg := f.config(t, version)
	cfg.ExitWait = 200 * time.Millisecond
	var mu sync.Mutex
	placed := 0
	place := cfg.Place
	cfg.Place = func(ctx context.Context) (helperPlacement, error) {
		mu.Lock()
		placed++
		mu.Unlock()
		return place(ctx)
	}
	sup := newHelperSupervisor(cfg)
	defer sup.Close()
	exited := make(chan struct{})
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = sleeper.Wait(); close(exited) }()
	sup.mu.Lock()
	sup.stale = []helperChild{{cmd: sleeper, exited: exited}} // dies on the first reap's Kill
	sup.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	got := make([]*helperproto.HostSession, 2)
	for i := range got {
		wg.Add(1)
		go func(i int) { defer wg.Done(); got[i], _ = sup.Session(ctx) }(i)
	}
	wg.Wait()
	if got[0] == nil || got[0] != got[1] {
		t.Fatalf("callers got different sessions: %p %p", got[0], got[1])
	}
	mu.Lock()
	defer mu.Unlock()
	if placed != 1 {
		t.Fatalf("%d helpers started", placed)
	}
}

// W3's seams: stream handlers per start, the hello hook, Current(), and the
// backend's Integration view.
func TestHelperSupervisorSeamsForW3(t *testing.T) {
	f := newHelperSupervisorFixture(t)
	cfg := f.config(t, version)
	var mu sync.Mutex
	handlerCalls, hellos := 0, []helperproto.HelloResult{}
	cfg.StreamHandlers = func() map[string]helperproto.HostStreamHandler {
		mu.Lock()
		handlerCalls++
		mu.Unlock()
		return nil // fwd stays off: v1 behaviour
	}
	cfg.OnHello = func(h helperproto.HelloResult, _ helperPlacement) {
		mu.Lock()
		hellos = append(hellos, h)
		mu.Unlock()
	}
	b := newHelperBackend(func(string) *helperSupervisor { return newHelperSupervisor(cfg) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	in, err := b.Integration(ctx, "Ubuntu")
	sup, _ := b.supervisor("Ubuntu")
	t.Cleanup(sup.Close) // no helper subprocess outlives a failed assertion
	if err != nil {
		t.Fatal(err)
	}
	if in.RuntimeDir == "" || in.Kernel == "" || in.BinPath != f.bin {
		t.Fatalf("integration %+v", in)
	}
	mu.Lock()
	calls, got := handlerCalls, append([]helperproto.HelloResult(nil), hellos...)
	mu.Unlock()
	if calls != 1 || len(got) != 1 || got[0].RuntimeDir != in.RuntimeDir {
		t.Fatalf("handlers called %d, hellos %+v", calls, got)
	}
	if _, h, ok := sup.Current(); !ok || h.RuntimeDir != in.RuntimeDir {
		t.Fatal("Current() lost the live session")
	}
	s, _ := sup.Session(ctx)
	_ = s.Close() // the session ends; Current must say so at once
	if _, _, ok := sup.Current(); ok {
		t.Fatal("Current() reports an ended session")
	}
	sup.Close()
	if _, _, ok := sup.Current(); ok {
		t.Fatal("Current() reports a stopped helper")
	}
}
