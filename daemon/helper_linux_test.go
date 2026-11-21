//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"soluterminal/helperproto"
)

// shortTempDir is under /tmp directly: agent socket paths must fit sun_path.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "w2h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestHelperRuntimeDirChoice(t *testing.T) {
	env := map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}
	got, err := helperRuntimeDir(func(k string) string { return env[k] }, "/home/u")
	if err != nil || got != "/run/user/1000/soluterminal-helper" {
		t.Fatalf("with XDG: %q, %v", got, err)
	}
	env["XDG_RUNTIME_DIR"] = "relative"
	got, err = helperRuntimeDir(func(k string) string { return env[k] }, "/home/u")
	if err != nil || got != "/home/u/.local/state/soluterminal/helper" {
		t.Fatalf("fallback: %q, %v", got, err)
	}
	if _, err := helperRuntimeDir(func(string) string { return "" }, ""); err == nil {
		t.Fatal("no home must fail")
	}
}

func TestHelperRuntimeDirRefusals(t *testing.T) {
	uid := os.Getuid()
	unsafe := func(t *testing.T, err error) {
		t.Helper()
		if helperproto.ErrorCode(err) != helperproto.CodeUnsafeRuntimeDir {
			t.Fatalf("want unsafe_runtime_dir, got %v", err)
		}
	}
	t.Run("fresh dir is 0700 and passes", func(t *testing.T) {
		dir := filepath.Join(shortTempDir(t), "rt")
		if err := prepareHelperRuntimeDir(dir, uid); err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{dir, filepath.Join(dir, "agents")} {
			if fi, _ := os.Lstat(d); fi.Mode().Perm() != 0o700 {
				t.Fatalf("%s mode %v", d, fi.Mode())
			}
		}
	})
	t.Run("symlinked agents", func(t *testing.T) {
		base := shortTempDir(t)
		dir := filepath.Join(base, "rt")
		_ = os.Mkdir(dir, 0o700)
		_ = os.Mkdir(filepath.Join(base, "elsewhere"), 0o700)
		_ = os.Symlink(filepath.Join(base, "elsewhere"), filepath.Join(dir, "agents"))
		unsafe(t, prepareHelperRuntimeDir(dir, uid))
	})
	t.Run("symlinked root", func(t *testing.T) {
		base := shortTempDir(t)
		_ = os.MkdirAll(filepath.Join(base, "real", "agents"), 0o700)
		_ = os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "rt"))
		unsafe(t, checkHelperRuntimeDir(filepath.Join(base, "rt"), uid))
	})
	t.Run("group-readable", func(t *testing.T) {
		dir := filepath.Join(shortTempDir(t), "rt")
		_ = prepareHelperRuntimeDir(dir, uid)
		_ = os.Chmod(filepath.Join(dir, "agents"), 0o750)
		unsafe(t, checkHelperRuntimeDir(dir, uid))
	})
	t.Run("other owner", func(t *testing.T) {
		dir := filepath.Join(shortTempDir(t), "rt")
		_ = prepareHelperRuntimeDir(dir, uid)
		unsafe(t, checkHelperRuntimeDir(dir, uid+1))
	})
	t.Run("too long for a socket", func(t *testing.T) {
		dir := filepath.Join(shortTempDir(t), strings.Repeat("d", 60))
		unsafe(t, prepareHelperRuntimeDir(dir, uid))
	})
}

func TestHelperAgentArgv(t *testing.T) {
	a := helperproto.SpawnArgs{
		PaneID: "pane_0123456789abcdef", Argv: []string{"bash", "-l"}, Cwd: "/home/u",
		Env: map[string]string{"SOLU_PANE_ID": "pane_0123456789abcdef", "A": "1"}, Cols: 100, Rows: 30, NoPty: true,
	}
	got, err := helperAgentArgv("/rt/agents/x.sock", "/rt/agents/x.d", a)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"agent", "--socket", "/rt/agents/x.sock", "--cwd", "/home/u", "--cols", "100", "--rows", "30",
		"--diagnostics-dir", "/rt/agents/x.d", "--capped-log", "--no-pty",
		"--env", "A=1,SOLU_PANE_ID=pane_0123456789abcdef", "--", "bash", "-l"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
	for _, flag := range []string{"--data-dir", "--owner-retirement-pane", "--provider"} {
		if strings.Contains(strings.Join(got, " "), flag) {
			t.Fatalf("v1 must not pass %s", flag)
		}
	}
	a.Env["NO_PROXY"] = "a,b"
	if _, err := helperAgentArgv("/s", "/d", a); helperproto.ErrorCode(err) != helperproto.CodeBadArgs {
		t.Fatalf("comma in env: %v", err)
	}
}

func TestHelperAgentCmdlineMatches(t *testing.T) {
	sock := "/rt/agents/" + strings.Repeat("a", 32) + ".sock"
	nul := func(args ...string) []byte { return []byte(strings.Join(args, "\x00") + "\x00") }
	for _, c := range []struct {
		raw  []byte
		want bool
	}{
		{nul("solud", "agent", "--socket", sock, "--cwd", "/"), true},
		{nul("solud", "agent", "--socket", sock+"X"), false},
		{nul("/home/u/.local/lib/soluterminal/helper/solud-0.0.1250", "agent", "--socket", sock), false},
		{nul("/usr/bin/sleep", "agent", "--socket", sock), false},
		{nul("/x/solud", "agent", "--socket", "/other.sock"), false},
		{nul("/x/solud", "agent", "--cwd", "/", "--socket", sock), false},
		{nul("solud", "--", "agent", "--socket", sock), false},
		{nul("/solud-dir/sh", "agent", "--socket", sock), false},
		{nil, false},
	} {
		if got := helperAgentCmdlineMatches(c.raw, sock); got != c.want {
			t.Errorf("%q: %v", c.raw, got)
		}
	}
}

func TestCappedLogWriter(t *testing.T) {
	var buf bytes.Buffer
	c := newCappedLogWriter(&buf, 1024)
	line := []byte(strings.Repeat("x", 99) + "\n")
	for i := 0; i < 20; i++ {
		_, _ = c.Write(line)
	}
	_, _ = c.Write([]byte("short\n")) // after the cap, even short records drop
	_ = c.Close()
	out := buf.String()
	if len(out) > 1024 {
		t.Fatalf("wrote %d bytes past a 1024 cap", len(out))
	}
	if strings.Contains(out, "short") || !strings.Contains(out, "bytes dropped") {
		t.Fatalf("tail: %q", out[len(out)-80:])
	}
	if strings.Count(out, "\n")-1 != (1024-cappedLogNoteRoom)/100 {
		t.Fatalf("records cut or lost: %q", out)
	}
}

func TestCappedOOMDiagnostics(t *testing.T) {
	t.Setenv("W2_SECRET_MARKER", "do-not-capture-7f3a")
	diag := filepath.Join(shortTempDir(t), "x.d")
	if err := os.Mkdir(diag, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := captureCappedOOMDiagnostics(diag, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("capture dir mode %v", fi.Mode())
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 3 {
		t.Fatalf("files: %v", files)
	}
	for _, f := range files {
		info, _ := f.Info()
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", f.Name(), info.Mode())
		}
		b, _ := os.ReadFile(filepath.Join(dir, f.Name()))
		if bytes.Contains(b, []byte("do-not-capture-7f3a")) {
			t.Fatalf("%s holds an env value", f.Name())
		}
	}
	// Fill the dir to just under 1 MiB: the next capture is cut to fit.
	used, _ := diagnosticsDirSize(diag)
	filler := make([]byte, agentDiagnosticsCap-used-200)
	if err := os.WriteFile(filepath.Join(diag, "filler"), filler, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = captureCappedOOMDiagnostics(diag, "agent")
	if total, _ := diagnosticsDirSize(diag); total > agentDiagnosticsCap {
		t.Fatalf("diagnostics grew to %d bytes", total)
	}
	if _, err := captureCappedOOMDiagnostics(diag, "agent"); err == nil {
		t.Fatal("a spent budget must refuse a capture")
	}
	link := filepath.Join(filepath.Dir(diag), "link.d")
	_ = os.Symlink(diag, link)
	if _, err := captureCappedOOMDiagnostics(link, "agent"); err == nil {
		t.Fatal("a symlinked diagnostics dir must be refused")
	}
}

func TestHelperNeverSignalsAStranger(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "rt")
	if err := prepareHelperRuntimeDir(dir, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	h := &linuxHelper{uid: os.Getuid(), runtimeDir: dir, logf: t.Logf}
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	ticks, _ := mcpProcessStartTimePlatform(sleeper.Process.Pid)
	id := strings.Repeat("c", 32)
	_, recordPath, _, _ := helperAgentPaths(dir, id)
	for name, rec := range map[string]helperAgentRecord{
		"cmdline":     {PaneID: "pane_0123456789abcdef", Pid: sleeper.Process.Pid, StartTicks: ticks},
		"start time":  {PaneID: "pane_0123456789abcdef", Pid: sleeper.Process.Pid, StartTicks: ticks + 1},
		"no identity": {PaneID: "pane_0123456789abcdef", Pid: sleeper.Process.Pid},
	} {
		_ = os.Remove(recordPath)
		if err := h.writeRecord(recordPath, rec); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if err := h.Kill(ctx, helperproto.KillArgs{AgentID: id, How: helperproto.KillKill}); helperproto.ErrorCode(err) != helperproto.CodeNotFound {
			t.Fatalf("%s: kill = %v", name, err)
		}
		page, _ := h.List(ctx, helperproto.ListArgs{Limit: 128})
		if len(page.Agents) != 1 || page.Agents[0].Alive {
			t.Fatalf("%s: list = %+v", name, page)
		}
		if _, ok := h.AgentByPid(sleeper.Process.Pid); ok {
			t.Fatalf("%s: AgentByPid matched a stranger", name)
		}
	}
	if err := syscall.Kill(sleeper.Process.Pid, 0); err != nil {
		t.Fatalf("the stranger was signalled: %v", err)
	}
}

// helperProc is a `solud helper` subprocess (this test binary, run through a
// symlink named like solud so the identity check recognises its agents).
type helperProc struct {
	cmd    *exec.Cmd
	host   *helperproto.HostSession
	stderr *helperStderrBuffer
	exited chan error
}

type helperStderrBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *helperStderrBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *helperStderrBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type pipeRWC struct {
	io.Reader
	io.WriteCloser
}

func startHelperProc(t *testing.T, bin, home, xdg string) *helperProc {
	t.Helper()
	cmd := exec.Command(bin, "helper", "--proto", "1")
	cmd.Env = []string{"HOME=" + home, "XDG_RUNTIME_DIR=" + xdg, "PATH=/usr/bin:/bin", "GOTRACEBACK=all"}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	p := &helperProc{cmd: cmd, stderr: &helperStderrBuffer{}, exited: make(chan error, 1)}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.exited <- cmd.Wait() }()
	host, err := helperproto.NewHostSession(pipeRWC{stdout, stdin}, helperproto.HostOptions{Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	p.host = host
	t.Cleanup(func() {
		_ = host.Close()
		_ = cmd.Process.Kill()
		<-p.exited
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := host.Hello(ctx, helperproto.HelloArgs{Proto: 1, HostVersion: version, HostPid: os.Getpid(), Distro: "test"}, os.Getuid()); err != nil {
		t.Fatalf("hello: %v (helper stderr: %s)", err, p.stderr.String())
	}
	return p
}

func (p *helperProc) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-p.exited:
		p.exited <- err
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("helper did not exit; stderr: %s", p.stderr.String())
		return nil
	}
}

func procEnviron(pid int) []string {
	b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	return strings.Split(string(b), "\x00")
}

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestHelperEndToEnd(t *testing.T) {
	base := shortTempDir(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(base, "solud-test")
	if err := os.Symlink(self, bin); err != nil {
		t.Fatal(err)
	}
	home, xdg := filepath.Join(base, "home"), filepath.Join(base, "run")
	for _, d := range []string{home, xdg, filepath.Join(home, "work")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runtimeDir := filepath.Join(xdg, "soluterminal-helper")
	// Detached agents outlive every helper: kill them all at the end.
	t.Cleanup(func() {
		recs, _ := filepath.Glob(filepath.Join(runtimeDir, "agents", "*.json"))
		for _, r := range recs {
			var rec helperAgentRecord
			if b, err := os.ReadFile(r); err == nil && json.Unmarshal(b, &rec) == nil && rec.Pid > 0 {
				_ = syscall.Kill(-rec.Pid, syscall.SIGKILL)
				_ = syscall.Kill(rec.Pid, syscall.SIGKILL)
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h1 := startHelperProc(t, bin, home, xdg)
	sp, err := h1.host.Spawn(ctx, helperproto.SpawnArgs{
		PaneID: "pane_0123456789abcdef",
		Argv:   []string{"/bin/sh", "-c", "cd work && sleep 300; :"},
		Cwd:    home, Cols: 80, Rows: 24,
		Env: map[string]string{"SOLU_PANE_ID": "pane_0123456789abcdef"},
	})
	if err != nil {
		t.Fatalf("spawn: %v (helper stderr: %s)", err, h1.stderr.String())
	}
	if sp.StartTicks == 0 || !helperproto.ValidAgentID(sp.AgentID) {
		t.Fatalf("spawn result %+v", sp)
	}

	// Files: socket 0600, log 0600, diagnostics 0700.
	sock, _, logPath, _ := helperAgentPaths(runtimeDir, sp.AgentID)
	for path, want := range map[string]os.FileMode{sock: 0o600, logPath: 0o600, helperAgentDiagDir(runtimeDir, sp.AgentID): 0o700} {
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", path, fi, err)
		}
	}
	// GOTRACEBACK=single reaches the agent only; the pane's process is clean.
	if env := procEnviron(sp.Pid); !hasEnv(env, "GOTRACEBACK=single") {
		t.Fatalf("agent env lacks GOTRACEBACK=single")
	}
	deadline := time.Now().Add(10 * time.Second)
	var cwd string
	for time.Now().Before(deadline) {
		if cwd, _ = h1.host.Cwd(ctx, sp.AgentID); cwd == filepath.Join(home, "work") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if cwd != filepath.Join(home, "work") {
		t.Fatalf("cwd = %q", cwd)
	}
	for _, pid := range descendantPids(sp.Pid) {
		if pid == sp.Pid {
			continue
		}
		if hasEnv(procEnviron(pid), "GOTRACEBACK=single") {
			t.Fatalf("pane process %d inherited GOTRACEBACK", pid)
		}
		if procIgnoresSIGHUP(t, pid) {
			t.Fatalf("pane process %d ignores SIGHUP: closing its pane would leak it", pid)
		}
	}

	// The agent stream speaks agentproto: the agent greets every client.
	conn, err := h1.host.OpenAgent(sp.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		t.Fatalf("agent stream: %v", err)
	}
	_ = conn.Close()

	// The helper goes away (stdin EOF); the agent survives it.
	_ = h1.host.Close()
	if err := h1.wait(t); err != nil {
		t.Fatalf("helper exit on EOF: %v (stderr: %s)", err, h1.stderr.String())
	}
	if !helperAgentIdentityHolds(sp.Pid, sp.StartTicks, sock, os.Getuid()) {
		t.Fatal("the agent died with its helper")
	}

	// A new helper reattaches with list and reaches the same agent.
	h2 := startHelperProc(t, bin, home, xdg)
	all, err := h2.host.ListAll(ctx)
	if err != nil || len(all) != 1 || all[0].AgentID != sp.AgentID || !all[0].Alive || all[0].StartTicks != sp.StartTicks {
		t.Fatalf("list after restart: %+v, %v", all, err)
	}
	conn, err = h2.host.OpenAgent(sp.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, hdr); err != nil {
		t.Fatalf("reattached stream: %v", err)
	}

	// Kill, then reap: busy while the identity holds, gone after.
	if err := h2.host.Reap(ctx, sp.AgentID); helperproto.ErrorCode(err) != helperproto.CodeBusy {
		t.Fatalf("reap of a live agent: %v", err)
	}
	// SIGKILL: the agent dies at once, and the pane's shell gets the PTY's
	// hangup, which it must not be ignoring.
	if err := h2.host.Kill(ctx, sp.AgentID, helperproto.KillKill); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && helperAgentIdentityHolds(sp.Pid, sp.StartTicks, sock, os.Getuid()) {
		time.Sleep(50 * time.Millisecond)
	}
	if err := h2.host.Reap(ctx, sp.AgentID); err != nil {
		var tree []string
		for _, pid := range descendantPids(sp.Pid) {
			st, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
			tree = append(tree, string(st))
		}
		logb, _ := os.ReadFile(logPath)
		t.Fatalf("reap: %v\nagent tree: %q\nagent log: %q\nhelper stderr: %s", err, tree, logb, h2.stderr.String())
	}
	if left, _ := filepath.Glob(filepath.Join(runtimeDir, "agents", sp.AgentID+"*")); len(left) != 0 {
		t.Fatalf("reap left %v", left)
	}

	// drain is the barrier; then only shutdown works, and the helper exits 0.
	if err := h2.host.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h2.host.Spawn(ctx, helperproto.SpawnArgs{PaneID: "pane_0123456789abcdef", Argv: []string{"sh"}, Cols: 80, Rows: 24}); helperproto.ErrorCode(err) != helperproto.CodeBusy {
		t.Fatalf("spawn after drain: %v", err)
	}
	if err := h2.host.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h2.wait(t); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 0 {
			t.Fatalf("helper exit after shutdown: %v", err)
		}
	}
}

func TestHelperRefusesAnUnsafeRuntimeDirAndExits(t *testing.T) {
	base := shortTempDir(t)
	self, _ := os.Executable()
	bin := filepath.Join(base, "solud-test")
	_ = os.Symlink(self, bin)
	xdg := filepath.Join(base, "run")
	_ = os.MkdirAll(filepath.Join(base, "elsewhere"), 0o700)
	_ = os.MkdirAll(filepath.Join(xdg, "soluterminal-helper"), 0o700)
	_ = os.Symlink(filepath.Join(base, "elsewhere"), filepath.Join(xdg, "soluterminal-helper", "agents"))

	cmd := exec.Command(bin, "helper", "--proto", "1")
	cmd.Env = []string{"HOME=" + base, "XDG_RUNTIME_DIR=" + xdg, "PATH=/usr/bin:/bin"}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }() // Wait runs below
	host, err := helperproto.NewHostSession(pipeRWC{stdout, stdin}, helperproto.HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = host.Hello(ctx, helperproto.HelloArgs{Proto: 1, HostVersion: version}, os.Getuid())
	if helperproto.ErrorCode(err) != helperproto.CodeUnsafeRuntimeDir {
		t.Fatalf("hello: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 3 {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper kept running after refusing its runtime dir")
	}
}

// procIgnoresSIGHUP reads SigIgn from /proc/<pid>/status (SIGHUP is bit 0).
func procIgnoresSIGHUP(t *testing.T, pid int) bool {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "SigIgn:"); ok {
			mask, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			return err == nil && mask&1 != 0
		}
	}
	return false
}

// A pane whose program can't start fails the spawn (the agent never greets),
// and its startup log stays in the distro: only a fixed line reaches the
// helper's stderr, which the host logs.
func TestHelperSpawnFailureStaysInTheDistro(t *testing.T) {
	base := shortTempDir(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(base, "solud-test")
	if err := os.Symlink(self, bin); err != nil {
		t.Fatal(err)
	}
	home, xdg := filepath.Join(base, "home"), filepath.Join(base, "run")
	for _, d := range []string{home, xdg} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runtimeDir := filepath.Join(xdg, "soluterminal-helper")
	h := startHelperProc(t, bin, home, xdg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const marker = "no-such-binary-w2-7c1e"
	_, err = h.host.Spawn(ctx, helperproto.SpawnArgs{
		PaneID: "pane_0123456789abcdef", Argv: []string{"/nonexistent/" + marker},
		Cwd: home, Cols: 80, Rows: 24,
	})
	if helperproto.ErrorCode(err) != helperproto.CodeSpawnFailed {
		t.Fatalf("spawn of a missing program: %v", err)
	}
	if recs, _ := filepath.Glob(filepath.Join(runtimeDir, "agents", "*.json")); len(recs) != 0 {
		t.Fatalf("a failed spawn left records: %v", recs)
	}
	logs, _ := filepath.Glob(filepath.Join(runtimeDir, "agents", "failed-*.log"))
	if len(logs) != 1 {
		t.Fatalf("failed startup logs: %v", logs)
	}
	if fi, _ := os.Lstat(logs[0]); fi.Mode().Perm() != 0o600 {
		t.Fatalf("failed log mode %v", fi.Mode())
	}
	if b, _ := os.ReadFile(logs[0]); !strings.Contains(string(b), marker) {
		t.Fatalf("the startup error should be kept in the distro, got %q", b)
	}
	// The host only ever sees the fixed line, not the agent's error text.
	if strings.Contains(h.stderr.String(), marker) || strings.Contains(err.Error(), marker) {
		t.Fatalf("the startup log left the distro: stderr %q, reply %q", h.stderr.String(), err)
	}
}

func TestUnixPeerIs(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			time.Sleep(time.Second)
		}
	}()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !unixPeerIs(c, os.Getpid(), os.Getuid()) {
		t.Fatal("own process not recognised as the peer")
	}
	if unixPeerIs(c, os.Getpid()+1, os.Getuid()) || unixPeerIs(c, os.Getpid(), os.Getuid()+1) {
		t.Fatal("a wrong pid or uid passed the peer check")
	}
}

func TestHelperReapReportsCleanupFailure(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "rt")
	if err := prepareHelperRuntimeDir(dir, os.Getuid()); err != nil {
		t.Fatal(err)
	}
	h := &linuxHelper{uid: os.Getuid(), runtimeDir: dir, logf: t.Logf}
	id := strings.Repeat("d", 32)
	_, recordPath, logPath, _ := helperAgentPaths(dir, id)
	if err := h.writeRecord(recordPath, helperAgentRecord{PaneID: "pane_0123456789abcdef", Pid: 1}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(logPath, []byte("x"), 0o600)
	agents := filepath.Join(dir, "agents")
	_ = os.Chmod(agents, 0o500) // removals now fail
	t.Cleanup(func() { _ = os.Chmod(agents, 0o700) })
	if err := h.Reap(context.Background(), id); helperproto.ErrorCode(err) != helperproto.CodeInternal {
		t.Fatalf("reap with a failing removal: %v", err)
	}
	_ = os.Chmod(agents, 0o700)
	if err := h.Reap(context.Background(), id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(agents, id+"*")); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
	if err := h.Reap(context.Background(), id); err != nil {
		t.Fatalf("reap of nothing: %v", err)
	}
}

// A --no-pty child has its own process group and no terminal to hang up:
// a force kill must take it down with its agent.
func TestHelperForceKillTakesThePipeChild(t *testing.T) {
	base := shortTempDir(t)
	self, _ := os.Executable()
	bin := filepath.Join(base, "solud-test")
	if err := os.Symlink(self, bin); err != nil {
		t.Fatal(err)
	}
	home, xdg := filepath.Join(base, "home"), filepath.Join(base, "run")
	for _, d := range []string{home, xdg} {
		_ = os.MkdirAll(d, 0o700)
	}
	h := startHelperProc(t, bin, home, xdg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sp, err := h.host.Spawn(ctx, helperproto.SpawnArgs{
		PaneID: "pane_0123456789abcdef", Argv: []string{"sleep", "300"}, Cwd: home, Cols: 80, Rows: 24, NoPty: true,
	})
	if err != nil {
		t.Fatalf("spawn: %v (stderr %s)", err, h.stderr.String())
	}
	var child int
	for _, pid := range scanProcessTree().descendantPids(sp.Pid) {
		if pid != sp.Pid {
			child = pid
		}
	}
	if child == 0 {
		t.Fatal("no pipe child found")
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL); _ = syscall.Kill(sp.Pid, syscall.SIGKILL) })
	if err := h.host.Kill(ctx, sp.AgentID, helperproto.KillKill); err != nil {
		t.Fatal(err)
	}
	// Dead means gone (ESRCH), already reaped (no /proc entry), or a zombie
	// waiting for init: one check for the wait and the verdict, so init
	// reaping between two separate checks can't read as a survivor.
	dead := func() bool {
		if err := syscall.Kill(child, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		st, err := os.ReadFile("/proc/" + strconv.Itoa(child) + "/stat")
		return err != nil || bytes.Contains(st, []byte(") Z "))
	}
	deadline := time.Now().Add(5 * time.Second)
	for !dead() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !dead() {
		t.Fatalf("the pipe child %d survived a force kill", child)
	}
}
