package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"soluterminal/helperproto"
	"soluterminal/helperproto/helpertest"
)

// pairBackend serves one distro from an in-memory helper pair.
type pairBackend struct {
	distro string
	pair   *helpertest.Pair

	mu       sync.Mutex
	kills    []string
	reaps    []string
	lists    int
	listErr  error
	openErr  error
	killHang bool // Kill blocks until its context ends (a hung WSL)
	// openGate, when set, holds OpenAgent until it is closed.
	openGate chan struct{}
	// openDead: OpenAgent "succeeds" with a stream that ends at once, as
	// when the helper can't dial the agent's socket.
	openDead bool
	opens    int
}

func (b *pairBackend) check(distro string) error {
	if distro != b.distro {
		return errors.New("unknown distro " + distro)
	}
	return nil
}

func (b *pairBackend) Spawn(ctx context.Context, distro string, a helperproto.SpawnArgs) (helperproto.SpawnResult, error) {
	if err := b.check(distro); err != nil {
		return helperproto.SpawnResult{}, err
	}
	if err := checkHelperSpawnEnv(a.Env); err != nil {
		return helperproto.SpawnResult{}, err
	}
	return b.pair.Host.Spawn(ctx, a)
}

func (b *pairBackend) OpenAgent(_ context.Context, distro, id string) (net.Conn, error) {
	if err := b.check(distro); err != nil {
		return nil, err
	}
	b.mu.Lock()
	err, gate, dead := b.openErr, b.openGate, b.openDead
	b.opens++
	b.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if err != nil {
		return nil, err
	}
	if dead {
		near, far := net.Pipe()
		_ = far.Close()
		return near, nil
	}
	return b.pair.Host.OpenAgent(id)
}

func (b *pairBackend) Kill(ctx context.Context, distro, id, how string) error {
	b.mu.Lock()
	b.kills = append(b.kills, how)
	hang := b.killHang
	b.mu.Unlock()
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	return b.pair.Host.Kill(ctx, id, how)
}

func (b *pairBackend) Reap(ctx context.Context, distro, id string) error {
	b.mu.Lock()
	b.reaps = append(b.reaps, id)
	b.mu.Unlock()
	return b.pair.Host.Reap(ctx, id)
}

func (b *pairBackend) Cwd(ctx context.Context, distro, id string) (string, error) {
	return b.pair.Host.Cwd(ctx, id)
}

func (b *pairBackend) ListAll(ctx context.Context, distro string) ([]helperproto.AgentEntry, error) {
	b.mu.Lock()
	b.lists++
	err := b.listErr
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return b.pair.Host.ListAll(ctx)
}

func (b *pairBackend) Integration(ctx context.Context, distro string) (linuxPaneIntegration, error) {
	if err := b.check(distro); err != nil {
		return linuxPaneIntegration{}, err
	}
	return linuxPaneIntegration{RuntimeDir: b.pair.Hello.RuntimeDir, Kernel: b.pair.Hello.Kernel}, nil
}

func newLinuxPaneManager(t *testing.T) (*Manager, *pairBackend) {
	t.Helper()
	b := &pairBackend{distro: "Ubuntu", pair: helpertest.NewPair(t, helpertest.Options{})}
	m := NewManager(t.TempDir())
	m.linux = b
	return m, b
}

// agentEnd is the test's side of a fake agent: it receives the pane's
// stream and answers kill requests like a real agent does.
func agentEnd(t *testing.T, b *pairBackend, id string) net.Conn {
	t.Helper()
	ag, ok := b.pair.Fake.Agent(id)
	if !ok {
		t.Fatalf("no fake agent %s", id)
	}
	select {
	case c := <-ag.Conns():
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the pane never opened its agent stream")
		return nil
	}
}

func TestLinuxPaneHasNoWindowsProcessIdentity(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu", Cmd: []string{"claude", "--model", "x"}, Cwd: "/home/u/src"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	defer far.Close()
	if p.Pid != 0 || p.SocketPath != "" || p.processScope != nil {
		t.Fatalf("a Linux pane carries a local identity: pid %d socket %q scope %v", p.Pid, p.SocketPath, p.processScope)
	}
	if p.Linux.Distro != "Ubuntu" || p.Linux.Pid == 0 || p.Linux.StartTicks == 0 {
		t.Fatalf("linux ref %+v", p.Linux)
	}
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	spawn := ag.Spawn
	if spawn.PaneID != p.ID || spawn.Cwd != "/home/u/src" || spawn.Env["SOLU_PANE_ID"] != p.ID {
		t.Fatalf("spawn args %+v", spawn)
	}
	if got := strings.Join(spawn.Argv[len(spawn.Argv)-3:], " "); got != "claude --model x" {
		t.Fatalf("argv %q", spawn.Argv)
	}
	snap := p.Snapshot()
	if snap.Pid != 0 || snap.Linux == nil || snap.Linux.AgentID != p.Linux.AgentID {
		t.Fatalf("snapshot %+v", snap.Linux)
	}
	if info := persistedFromSnapshot(snap); info.AgentPid != 0 || info.SocketPath != "" || info.Linux == nil {
		t.Fatalf("persisted %+v", info)
	}
}

func TestLinuxPaneOutputAndKill(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	if p.Title != "Linux (Ubuntu)" {
		t.Fatalf("title %q", p.Title)
	}
	if err := WriteFrame(far, FrameData, []byte("hello from linux\r\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(string(p.buffer.Snapshot()), "hello from linux") {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(string(p.buffer.Snapshot()), "hello from linux") {
		t.Fatal("agent output never reached the pane")
	}
	// The agent answers the kill request by exiting: the stream closes.
	go func() {
		for {
			ft, _, err := ReadFrame(far)
			if err != nil {
				return
			}
			if ft == FrameKillRequest {
				ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
				ag.SetAlive(false)
				_ = far.Close()
				return
			}
		}
	}()
	if err := m.Kill(p.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the helper's records reaped", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.reaps) == 1
	})
	// The stop after a clean exit finds no live agent (not_found): nothing
	// is signalled.
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	if ag != nil && len(ag.Kills()) != 0 {
		t.Fatal("a clean exit still signalled the agent")
	}
}

// An agent that ignores its kill request is stopped through the helper,
// never by a pid on this machine.
func TestLinuxPaneKillFallsBackToTheHelper(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	go func() { // a wedged agent: it dies only when the helper signals it
		for len(ag.Kills()) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
		_ = far.Close()
	}()
	if err := m.Kill(p.ID); err != nil {
		t.Fatal(err)
	}
	if k := ag.Kills(); len(k) == 0 || k[0] != helperproto.KillTerm {
		t.Fatalf("helper kills %v", k)
	}
}

func TestLinuxPaneRefusals(t *testing.T) {
	m, _ := newLinuxPaneManager(t)
	for name, opts := range map[string]SpawnOpts{
		"kind":     {Distro: "Ubuntu", Kind: "workflow"},
		"role":     {Distro: "Ubuntu", Role: paneRoleTaskOwner},
		"resume":   {Distro: "Ubuntu", Agent: agentClaude, ClaudeSessionID: "s1"},
		"--resume": {Distro: "Ubuntu", Cmd: []string{"claude", "--resume", "s1"}},
		"provider": {Distro: "Ubuntu", Provider: "glm"},
		"pack":     {Distro: "Ubuntu", ContextPack: "/p"},
	} {
		var se *SpawnError
		if _, err := m.Spawn(opts); !errors.As(err, &se) {
			t.Errorf("%s: %v", name, err)
		}
	}
	off := NewManager(t.TempDir())
	var se *SpawnError
	if _, err := off.Spawn(SpawnOpts{Distro: "Ubuntu"}); !errors.As(err, &se) || !strings.Contains(se.msg, "switched on") {
		t.Fatalf("Linux panes off: %v", err)
	}
	// A host secret in the env never reaches the distro.
	if _, err := m.Spawn(SpawnOpts{Distro: "Ubuntu", Env: []string{"SOLU_TEAM_BROKER_TOKEN=t"}}); err == nil {
		t.Fatal("a host secret was sent to the distro")
	}
}

// The login-shell wrapper passes the command's words through untouched.
func TestLinuxPaneArgvKeepsWords(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	argv := linuxPaneArgv([]string{"printf", "%s|", "a b", "$HOME", "'q'", `"d"`, "x;y"})
	// A non-POSIX login shell (fish) can't run the wrapper's script: /bin/sh
	// stands in for it.
	for _, shell := range []string{"/bin/sh", "/usr/bin/fish"} {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = []string{"SHELL=" + shell, "HOME=/nonexistent", "PATH=/usr/bin:/bin"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("SHELL=%s: %v", shell, err)
		}
		if got, want := string(out), `a b|$HOME|'q'|"d"|x;y|`; got != want {
			t.Fatalf("SHELL=%s: got %q want %q", shell, got, want)
		}
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// After a host restart a Linux pane is reattached through the helper, and
// only while its agent is still alive with the same identity.
func TestLinuxPaneReattach(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	_ = agentEnd(t, b, p.Linux.AgentID)
	info := persistedFromSnapshot(p.Snapshot())

	m2 := NewManager(t.TempDir())
	m2.linux = b
	p2, err := m2.AttachExisting(&info)
	if err != nil || p2 == nil {
		t.Fatalf("reattach: %v %v", p2, err)
	}
	if p2.Pid != 0 || p2.SocketPath != "" || p2.Linux == nil || p2.linuxStop == nil {
		t.Fatalf("reattached pane %+v", p2.Linux)
	}
	_ = agentEnd(t, b, p.Linux.AgentID) // a new stream reached the same agent

	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	ag.SetAlive(false)
	m3 := NewManager(t.TempDir())
	m3.linux = b
	if p3, _ := m3.AttachExisting(&info); p3 != nil {
		t.Fatal("reattached a dead agent")
	}
	other := info
	other.ID = "pane_00000000000000ff"
	ag.SetAlive(true)
	if p4, _ := m3.AttachExisting(&other); p4 != nil {
		t.Fatal("reattached another pane's agent")
	}
	off := NewManager(t.TempDir())
	if p5, _ := off.AttachExisting(&info); p5 != nil {
		t.Fatal("reattached with Linux panes off")
	}
}

func TestLinuxPaneRespawnKeepsItsDistro(t *testing.T) {
	snap := PaneSnapshot{ID: "pane_0123456789abcdef", Cmd: linuxPaneShell, Linux: &linuxPaneRef{Distro: "Ubuntu", AgentID: strings.Repeat("a", 32)}}
	if opts := resumeSpawnOpts(snap, 80, 24); opts.Distro != "Ubuntu" {
		t.Fatalf("respawn of a Linux pane would run on Windows: %q", opts.Distro)
	}
	if opts := resumeSpawnOpts(PaneSnapshot{ID: "pane_0123456789abcdef"}, 80, 24); opts.Distro != "" {
		t.Fatal("a local pane got a distro")
	}
	// A respawn's command is never wrapped twice.
	if got := linuxPaneArgv(linuxPaneShell); strings.Join(got, "\x00") != strings.Join(linuxPaneShell, "\x00") {
		t.Fatalf("shell rewrapped: %q", got)
	}
	once := linuxPaneArgv([]string{"claude"})
	if twice := linuxPaneArgv(once); strings.Join(twice, "\x00") != strings.Join(once, "\x00") {
		t.Fatalf("command rewrapped: %q", twice)
	}
}

func TestLinuxPaneLiveCwdAndReportedPid(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu", Cwd: "/home/u"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	ag.SetCwd("/home/u/src/proj")
	if got := m.paneLiveCwd(p); got != "/home/u/src/proj" {
		t.Fatalf("live cwd %q", got)
	}
	// The agent's own pid is a Linux pid: never recorded as a local one.
	if err := WriteFrame(far, FrameAgentInfo, []byte(`{"version":"0.0.1250","pid":4242,"proto":1}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the agent info", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.agentVersion == "0.0.1250"
	})
	p.mu.Lock()
	reported := p.agentReportedPID
	p.mu.Unlock()
	if reported != 0 {
		t.Fatalf("a Linux pid %d became the pane's local agent pid", reported)
	}
}

// Linux panes stay off unless switched on, and never run anywhere but a
// Windows host.
func TestLinuxPanesAreOffByDefault(t *testing.T) {
	t.Setenv(linuxPanesEnv, "")
	if linuxPaneBackendFromEnv(linuxPaneHooks{}) != nil {
		t.Fatal("Linux panes on without the switch")
	}
	t.Setenv(linuxPanesEnv, "1")
	if runtime.GOOS != "windows" && linuxPaneBackendFromEnv(linuxPaneHooks{}) != nil {
		t.Fatal("Linux panes through a helper on a non-Windows host")
	}
}

// A Linux pane's cwd never becomes a file relay root on the host.
func TestLinuxPaneIsNoRelayRoot(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	dir := t.TempDir() // a real directory here, as a distro path could be
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu", Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	_ = agentEnd(t, b, p.Linux.AgentID)
	a := &apiServer{mgr: m, dataDir: t.TempDir()}
	if reason := a.relayFileScope().refusal(filepath.Join(dir, "x")); reason == "" {
		t.Fatal("a Linux pane's cwd became a relay file root")
	}
}

// A broken helper stream ends the pane while its agent may live on: closing
// the tile must still stop that agent through the helper (no orphan).
func TestLinuxPaneBrokenStreamStillStopsTheAgent(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	_ = far.Close() // the stream breaks; the fake agent stays alive
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pane outlived its stream")
	}
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	if err := m.Kill(p.ID); err != nil {
		t.Fatal(err)
	}
	if len(ag.Kills()) == 0 {
		t.Fatal("the orphaned agent wasn't stopped through the helper before the pane went")
	}
	waitFor(t, "its files reaped", func() bool {
		_, still := b.pair.Fake.Agent(p.Linux.AgentID)
		return !still
	})
}

func TestLinuxPaneDefaultShellRecovers(t *testing.T) {
	info := PersistedPane{ID: "pane_0123456789abcdef", Cmd: linuxPaneShell, Linux: &linuxPaneRef{Distro: "Ubuntu"}}
	if !plainShellRecovery(info) {
		t.Fatal("a Linux pane's default shell isn't recovered at boot")
	}
	info.Cmd = linuxPaneArgv([]string{"claude"})
	if plainShellRecovery(info) {
		t.Fatal("a wrapped command passed as a plain shell")
	}
}

// A close whose helper stop fails keeps the pane, so it can be retried
// instead of leaving the agent running unseen.
func TestLinuxPaneCloseKeepsTheTileWhenTheStopFails(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	_ = far.Close()
	<-p.done
	_ = b.pair.Host.Close() // the helper is gone: every helper call fails
	// An agent's or a retry loop's closes never remove it, however often.
	for i := 0; i < 3; i++ {
		if err := m.Kill(p.ID); !errors.Is(err, errLinuxStopUnconfirmed) {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	if _, ok := m.Get(p.ID); !ok {
		t.Fatal("the tile was dropped though its agent may still run")
	}
	// The person: closes without the tile's token keep it (overlapping
	// requests can't use up the warning); a wrong token too; only the token
	// from the refusal, sent after they agree, removes it.
	var stop *linuxStopError
	for i := 0; i < 2; i++ {
		err := m.KillAsPerson(p.ID, "")
		if !errors.As(err, &stop) || stop.ConfirmToken == "" {
			t.Fatalf("person close %d without a token: %v", i, err)
		}
	}
	if err := m.KillAsPerson(p.ID, strings.Repeat("0", 32)); !errors.Is(err, errLinuxStopUnconfirmed) {
		t.Fatalf("a wrong token removed the tile: %v", err)
	}
	if err := m.KillAsPerson(p.ID, stop.ConfirmToken); err != nil {
		t.Fatalf("confirmed close: %v", err)
	}
	if _, ok := m.Get(p.ID); ok {
		t.Fatal("a tile whose helper is gone can never be closed")
	}
}

// With Linux panes switched off, a Linux tile's agent can't be confirmed
// stopped: closing it never drops it silently.
func TestLinuxPaneCloseWithTheSwitchOff(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	_ = far.Close()
	<-p.done
	m.linux = nil // switched off after the pane was opened
	if err := m.Kill(p.ID); !errors.Is(err, errLinuxStopUnconfirmed) {
		t.Fatalf("close with the switch off: %v", err)
	}
	if _, ok := m.Get(p.ID); !ok {
		t.Fatal("the tile was dropped without stopping its agent")
	}
}

// A pane imported from a Linux-mode install (W5) carries only its distro
// until its first spawn: it recovers like any suspended pane, and a resume
// spawns it fresh in that distro.
func TestLinuxPaneImportedBeforeFirstSpawn(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	info := PersistedPane{ID: "pane_00000000000000aa", Cwd: "/home/u/src", Linux: &linuxPaneRef{Distro: "Ubuntu"}}
	if p, _ := m.AttachExisting(&info); p != nil {
		t.Fatal("attached a pane that has no agent")
	}
	if !plainShellRecovery(info) {
		t.Fatal("an imported shell pane isn't recovered at boot")
	}
	p, err := m.ResumeAgentPane(info)
	if err != nil {
		t.Fatalf("resume of an imported pane: %v", err)
	}
	if p.Linux == nil || p.Linux.Distro != "Ubuntu" || !helperproto.ValidAgentID(p.Linux.AgentID) || p.Pid != 0 {
		t.Fatalf("resumed pane %+v pid %d", p.Linux, p.Pid)
	}
	_ = agentEnd(t, b, p.Linux.AgentID)
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	if ag.Spawn.Cwd != "/home/u/src" {
		t.Fatalf("cwd %q", ag.Spawn.Cwd)
	}
}

// A broken helper at boot costs one bounded listing per distro, not one per
// pane, and resume then replaces nothing it can't see.
func TestLinuxPaneBootDoesNotWaitOnABrokenHelper(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	_ = agentEnd(t, b, p.Linux.AgentID)
	info := persistedFromSnapshot(p.Snapshot())

	b.mu.Lock()
	b.listErr = errors.New("wsl.exe hung")
	b.mu.Unlock()
	m2 := NewManager(t.TempDir())
	m2.linux = b
	for i := 0; i < 3; i++ {
		other := info
		other.ID = fmt.Sprintf("pane_00000000000000%02x", i)
		if got, _ := m2.AttachExisting(&other); got != nil {
			t.Fatal("attached through a broken helper")
		}
	}
	b.mu.Lock()
	lists := b.lists
	b.mu.Unlock()
	if lists != 1 {
		t.Fatalf("the broken helper was asked %d times during one boot burst", lists)
	}
	if _, err := m2.ResumeAgentPane(info); err == nil {
		t.Fatal("resume replaced a pane whose helper can't answer")
	}
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	if len(ag.Kills()) != 0 {
		t.Fatal("a live agent was killed because its helper didn't answer")
	}
}

// Resume reattaches a live agent instead of replacing it.
func TestLinuxPaneResumeReattachesALiveAgent(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	_ = agentEnd(t, b, p.Linux.AgentID)
	info := persistedFromSnapshot(p.Snapshot())
	m2 := NewManager(t.TempDir())
	m2.linux = b
	got, err := m2.ResumeAgentPane(info)
	if err != nil || got == nil || got.Linux.AgentID != p.Linux.AgentID {
		t.Fatalf("resume: %v %v", got, err)
	}
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	if len(ag.Kills()) != 0 {
		t.Fatal("resume killed a live agent instead of reattaching it")
	}
}

func TestLinuxPaneReopenAndDistroNames(t *testing.T) {
	snap := PaneSnapshot{ID: "pane_0123456789abcdef", Cmd: linuxPaneShell, Linux: &linuxPaneRef{Distro: "Ubuntu-22.04"}}
	if opts := reopenSpawnOpts(closedPaneEntry{Snap: snap}, 80, 24); opts.Distro != "Ubuntu-22.04" {
		t.Fatalf("a closed Linux pane would reopen on Windows: %q", opts.Distro)
	}
	m, _ := newLinuxPaneManager(t)
	for _, bad := range []string{"-d", "a b", "x;y", "../u", strings.Repeat("a", 65)} {
		var se *SpawnError
		if _, err := m.Spawn(SpawnOpts{Distro: bad}); !errors.As(err, &se) {
			t.Errorf("distro %q: %v", bad, err)
		}
	}
}

// A respawn the Linux pane can't take (set-role, set-context-pack) is
// refused before its live agent is stopped.
func TestLinuxPaneRespawnRefusedBeforeTheKill(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	_ = agentEnd(t, b, p.Linux.AgentID)
	a := &apiServer{mgr: m, dataDir: t.TempDir()}
	opts := resumeSpawnOpts(p.Snapshot(), 80, 24)
	opts.Role = paneRoleTaskOwner
	var se *SpawnError
	if _, err := a.respawnInPlace(p.Snapshot(), opts); !errors.As(err, &se) {
		t.Fatalf("respawn with a role: %v", err)
	}
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	if len(ag.Kills()) != 0 {
		t.Fatal("the live agent was stopped for a request that was then refused")
	}
	if got, _ := m.Get(p.ID); got != p || got.Snapshot().Status != PaneRunning {
		t.Fatal("the refused respawn changed the live pane")
	}
}

// A live agent whose stream fails to open once is not replaced: resume
// reports the failure and leaves the session alone.
func TestLinuxPaneResumeNeverReplacesALiveAgentOnAStreamError(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	_ = agentEnd(t, b, p.Linux.AgentID)
	info := persistedFromSnapshot(p.Snapshot())
	b.mu.Lock()
	b.openErr = errors.New("stream refused")
	b.mu.Unlock()
	m2 := NewManager(t.TempDir())
	m2.linux = b
	if got, err := m2.ResumeAgentPane(info); err == nil || got != nil {
		t.Fatalf("resume: %v %v", got, err)
	}
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	if ag == nil || len(ag.Kills()) != 0 {
		t.Fatal("a live agent was replaced because its stream failed once")
	}
}

// A distro repo's .solu-no-autoanswer can't be read from Windows: Linux
// panes fail closed, at spawn and on reattach.
func TestLinuxPaneNeverAutoAnswers(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu", AutoMode: "full"})
	if err != nil {
		t.Fatal(err)
	}
	_ = agentEnd(t, b, p.Linux.AgentID)
	if snap := p.Snapshot(); snap.AutoMode != "" || !snap.NeverAutoAnswer {
		t.Fatalf("spawned Linux pane: auto %q locked %v", snap.AutoMode, snap.NeverAutoAnswer)
	}
	info := persistedFromSnapshot(p.Snapshot())
	info.AutoMode, info.NeverAutoAnswer = "full", false // a record that says otherwise
	m2 := NewManager(t.TempDir())
	m2.linux = b
	p2, err := m2.AttachExisting(&info)
	if err != nil || p2 == nil {
		t.Fatalf("reattach: %v %v", p2, err)
	}
	if snap := p2.Snapshot(); snap.AutoMode != "" || !snap.NeverAutoAnswer {
		t.Fatalf("reattached Linux pane: auto %q locked %v", snap.AutoMode, snap.NeverAutoAnswer)
	}
}

// A person's Resume of a tile kept after a failed boot listing reattaches
// the still-live agent instead of killing it.
func TestLinuxPanePersonResumeReattaches(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	_ = agentEnd(t, b, p.Linux.AgentID)
	info := persistedFromSnapshot(p.Snapshot())

	m2 := NewManager(t.TempDir())
	m2.linux = b
	info.RecoveryError = "the Linux pane's helper didn't answer" // as boot keeps it
	tile := m2.SuspendExisting(&info)
	if tile == nil {
		t.Fatal("no recovery tile")
	}
	a := &apiServer{mgr: m2, dataDir: t.TempDir()}
	if !tile.tryAcquireRespawn() { // as handleResumePane does
		t.Fatal("no lease")
	}
	defer tile.releaseRespawn()
	snap := tile.Snapshot()
	got, err := a.respawnInPlace(snap, resumeSpawnOpts(snap, 80, 24))
	if err != nil || got == nil || got.Linux == nil || got.Linux.AgentID != p.Linux.AgentID {
		t.Fatalf("resume: %+v %v", got, err)
	}
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	if ag == nil || len(ag.Kills()) != 0 {
		t.Fatal("a person's Resume killed a live Linux agent")
	}
}

// A spawn request can't set its own pane id: the daemon's value wins (@secure).
func TestLinuxPaneEnvDaemonValuesWin(t *testing.T) {
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu", Env: []string{
		"SOLU_PANE_ID=pane_ffffffffffffffff", "CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN=0", "EDITOR=vim",
	}})
	if err != nil {
		t.Fatal(err)
	}
	_ = agentEnd(t, b, p.Linux.AgentID)
	ag, _ := b.pair.Fake.Agent(p.Linux.AgentID)
	env := ag.Spawn.Env
	if env["SOLU_PANE_ID"] != p.ID || env["CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN"] != "1" {
		t.Fatalf("a caller overrode the daemon's values: %v", env)
	}
	if env["EDITOR"] != "vim" {
		t.Fatalf("the caller's own entry was lost: %v", env)
	}
}

// With WSL hung, a person's close still answers (409 and its token) inside
// the app's request limit: the stop has one overall deadline.
func TestLinuxPanePersonCloseAnswersWhenWSLHangs(t *testing.T) {
	old := linuxPaneStopTimeout
	linuxPaneStopTimeout = 300 * time.Millisecond
	defer func() { linuxPaneStopTimeout = old }()
	m, b := newLinuxPaneManager(t)
	p, err := m.Spawn(SpawnOpts{Distro: "Ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	far := agentEnd(t, b, p.Linux.AgentID)
	_ = far.Close()
	<-p.done
	b.mu.Lock()
	b.killHang = true
	b.mu.Unlock()
	start := time.Now()
	var stop *linuxStopError
	if err := m.KillAsPerson(p.ID, ""); !errors.As(err, &stop) || stop.ConfirmToken == "" {
		t.Fatalf("close with WSL hung: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the close took %v with a 300ms stop deadline", took)
	}
}
