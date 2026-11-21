//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"soluterminal/helperproto"

	"golang.org/x/sys/unix"
)

// helperAgentStartTimeout bounds how long spawn waits for a new agent's
// socket to accept a connection.
const helperAgentStartTimeout = 10 * time.Second

// helperAgentRecord is agents/<agentId>.json.
type helperAgentRecord struct {
	PaneID     string `json:"paneId"`
	Pid        int    `json:"pid"`
	StartedAt  string `json:"startedAt"`
	StartTicks uint64 `json:"startTicks"`
}

// linuxHelper implements helperproto.Helper and helperproto.AgentRegistry
// over the runtime dir: one detached `solud agent` per pane.
type linuxHelper struct {
	version string
	exe     string // this solud, which runs each agent
	uid     int
	user    string
	home    string
	getenv  func(string) string
	logf    func(format string, args ...any)

	mu         sync.Mutex // runtime dir file changes: spawn's record, reap
	runtimeDir string     // set by Hello
}

func (h *linuxHelper) Hello(_ context.Context, a helperproto.HelloArgs) (helperproto.HelloResult, error) {
	dir, err := helperRuntimeDir(h.getenv, h.home)
	if err != nil {
		return helperproto.HelloResult{}, helperproto.Errorf(helperproto.CodeUnsafeRuntimeDir, "%v", err)
	}
	if err := prepareHelperRuntimeDir(dir, h.uid); err != nil {
		return helperproto.HelloResult{}, err
	}
	h.mu.Lock()
	h.runtimeDir = dir
	h.mu.Unlock()
	var uts syscall.Utsname
	_ = syscall.Uname(&uts)
	return helperproto.HelloResult{
		Proto:         helperproto.Proto,
		HelperVersion: h.version,
		Pid:           os.Getpid(),
		UID:           h.uid,
		User:          h.user,
		Home:          h.home,
		Arch:          utsString(uts.Machine[:]),
		Kernel:        utsString(uts.Release[:]),
		Networking:    wslNetworkingMode(),
		RuntimeDir:    dir,
	}, nil
}

func (h *linuxHelper) dir() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runtimeDir
}

// helperAgentArgv is the agent command line: today's Linux agent start,
// without --data-dir or --owner-retirement-pane (helper-protocol-v1, spawn).
func helperAgentArgv(sock, diagDir string, a helperproto.SpawnArgs) ([]string, error) {
	argv := []string{"agent",
		"--socket", sock,
		"--cwd", a.Cwd,
		"--cols", strconv.Itoa(a.Cols),
		"--rows", strconv.Itoa(a.Rows),
		"--diagnostics-dir", diagDir,
		"--capped-log",
	}
	if a.NoPty {
		argv = append(argv, "--no-pty")
	}
	if len(a.Env) > 0 {
		keys := make([]string, 0, len(a.Env))
		for k, v := range a.Env {
			// The agent's --env wire is comma-joined, as today's host spawn.
			if strings.Contains(v, ",") {
				return nil, helperproto.Errorf(helperproto.CodeBadArgs, "spawn: env %s has a comma", k)
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		env := make([]string, 0, len(keys))
		for _, k := range keys {
			env = append(env, k+"="+a.Env[k])
		}
		argv = append(argv, "--env", joinEnv(env))
	}
	argv = append(argv, "--")
	return append(argv, a.Argv...), nil
}

func (h *linuxHelper) Spawn(ctx context.Context, a helperproto.SpawnArgs) (helperproto.SpawnResult, error) {
	var r helperproto.SpawnResult
	dir := h.dir()
	if err := checkHelperRuntimeDir(dir, h.uid); err != nil {
		return r, err
	}
	id, err := helperproto.NewAgentID()
	if err != nil {
		return r, err
	}
	sock, recordPath, logPath, _ := helperAgentPaths(dir, id)
	diagDir := helperAgentDiagDir(dir, id)
	argv, err := helperAgentArgv(sock, diagDir, a)
	if err != nil {
		return r, err
	}
	if err := os.Mkdir(diagDir, 0o700); err != nil {
		return r, helperproto.Errorf(helperproto.CodeSpawnFailed, "diagnostics dir: %v", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag, 0o600)
	if err != nil {
		_ = os.Remove(diagDir)
		return r, helperproto.Errorf(helperproto.CodeSpawnFailed, "startup log: %v", err)
	}
	cmd := exec.Command(h.exe, argv...)
	// argv[0] is exactly "solud", whatever the file is called, so the
	// identity check reads `solud agent --socket <sock>` (helper-protocol-v1).
	cmd.Args[0] = "solud"
	// Its own session, so it outlives the helper and wsl.exe (Check 1).
	// The agent catches SIGHUP itself (--capped-log); nothing here sets
	// SIG_IGN, which would be inherited by every pane process.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// GOTRACEBACK=single for the agent itself (@secure): a user's all/system/
	// crash can't widen its crash report or dump core. The agent removes it
	// from its child's env under --capped-log.
	cmd.Env = append(filterOutEnvKeys(os.Environ(), []string{"GOTRACEBACK"}), "GOTRACEBACK=single")
	cmd.Stderr = logFile
	startErr := cmd.Start()
	_ = logFile.Close()
	if startErr != nil {
		_ = h.removeAgentFiles(dir, id)
		return r, helperproto.Errorf(helperproto.CodeSpawnFailed, "start agent: %v", startErr)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }() // reap while the helper lives

	pid := cmd.Process.Pid
	ticks, ok := mcpProcessStartTimePlatform(pid)
	if !ok {
		_ = cmd.Process.Kill()
		_ = h.removeAgentFiles(dir, id)
		return r, helperproto.Errorf(helperproto.CodeSpawnFailed, "agent exited at once")
	}
	rec := helperAgentRecord{PaneID: a.PaneID, Pid: pid, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), StartTicks: ticks}
	if err := h.writeRecord(recordPath, rec); err != nil {
		_ = cmd.Process.Kill()
		_ = h.removeAgentFiles(dir, id)
		return r, helperproto.Errorf(helperproto.CodeSpawnFailed, "record: %v", err)
	}
	if err := waitHelperAgentReady(ctx, sock, pid, exited); err != nil {
		_ = cmd.Process.Kill()
		<-exited
		h.keepFailedStartupLog(dir, id, logPath)
		_ = h.removeAgentFiles(dir, id)
		return r, helperproto.Errorf(helperproto.CodeSpawnFailed, "agent did not start: %v", err)
	}
	return helperproto.SpawnResult{AgentID: id, Pid: pid, StartTicks: ticks}, nil
}

// waitHelperAgentReady waits until the agent greets a client. The socket
// accepts connections from the moment it binds, before the pane's child has
// started, so a dial alone proves nothing. The agent's accept loop, which
// sends FrameDims first to every client, runs only after the child started;
// a child that fails to start ends the agent, and the read sees EOF.
func waitHelperAgentReady(ctx context.Context, sock string, pid int, exited <-chan struct{}) error {
	start := time.Now()
	deadline := time.NewTimer(helperAgentStartTimeout)
	defer deadline.Stop()
	for {
		c, err := net.DialTimeout("unix", sock, time.Second)
		if err == nil && !unixPeerIs(c, pid, os.Getuid()) {
			_ = c.Close()
			return errors.New("the socket's peer is not the agent")
		}
		if err == nil {
			_ = c.SetReadDeadline(start.Add(helperAgentStartTimeout))
			var hdr [5]byte
			_, rerr := io.ReadFull(c, hdr[:])
			_ = c.Close()
			if rerr == nil {
				return nil
			}
		}
		select {
		case <-exited:
			return errors.New("agent exited")
		case <-deadline.C:
			return errors.New("timed out")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// keepFailedStartupLog keeps a failed agent's startup log in the distro as
// agents/failed-<agentId>.log, for a person debugging there, and prunes all
// but the newest helperFailedLogsKept. Its contents never leave the distro:
// the helper's stderr reaches the host's log, so only a fixed line goes
// there (@secure, Agent output).
func (h *linuxHelper) keepFailedStartupLog(dir, agentID, logPath string) {
	kept := filepath.Join(dir, "agents", "failed-"+agentID+".log")
	if err := os.Rename(logPath, kept); err != nil {
		return
	}
	h.logf("agent %s did not start; its startup log is agents/failed-%s.log in the distro", agentID, agentID)
	old, _ := filepath.Glob(filepath.Join(dir, "agents", "failed-*.log"))
	if len(old) <= helperFailedLogsKept {
		return
	}
	sort.Slice(old, func(i, j int) bool { return modTime(old[i]).Before(modTime(old[j])) })
	for _, p := range old[:len(old)-helperFailedLogsKept] {
		_ = os.Remove(p)
	}
}

// helperFailedLogsKept bounds the failed startup logs kept in the distro.
const helperFailedLogsKept = 8

func modTime(path string) time.Time {
	fi, err := os.Lstat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func (h *linuxHelper) writeRecord(path string, rec helperAgentRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

func (h *linuxHelper) readRecord(dir, agentID string) (helperAgentRecord, bool) {
	var rec helperAgentRecord
	_, recordPath, _, err := helperAgentPaths(dir, agentID)
	if err != nil {
		return rec, false
	}
	f, err := os.OpenFile(recordPath, os.O_RDONLY|noFollowFlag, 0)
	if err != nil {
		return rec, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil || json.Unmarshal(b, &rec) != nil || !helperproto.ValidPaneID(rec.PaneID) {
		return rec, false
	}
	return rec, true
}

// alive reports whether the record's process identity holds: the pid's
// start time equals the recorded startTicks, its cmdline is this agent's
// `solud agent --socket <runtimeDir>/agents/<agentId>.sock`, and it runs as
// the helper's uid. A record without startTicks is never alive.
func (h *linuxHelper) alive(dir, agentID string, rec helperAgentRecord) bool {
	sock, _, _, err := helperAgentPaths(dir, agentID)
	if err != nil {
		return false
	}
	return helperAgentIdentityHolds(rec.Pid, rec.StartTicks, sock, h.uid)
}

func helperAgentIdentityHolds(pid int, startTicks uint64, sock string, uid int) bool {
	if pid <= 0 || startTicks == 0 {
		return false
	}
	if t, ok := mcpProcessStartTimePlatform(pid); !ok || t != startTicks {
		return false
	}
	fi, err := os.Lstat("/proc/" + strconv.Itoa(pid))
	if err != nil {
		return false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != uid {
		return false
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	if !helperAgentCmdlineMatches(raw, sock) {
		return false
	}
	// The pid can't have been reused between the reads if its start time
	// is still the recorded one.
	t, ok := mcpProcessStartTimePlatform(pid)
	return ok && t == startTicks
}

// helperAgentCmdlineMatches checks a NUL-separated /proc cmdline against the
// exact prefix the helper starts: `solud agent --socket <sock>`. Stricter
// than parseAgentCmdline, which takes "agent" and "--socket" from anywhere,
// including a pane's own argv after "--".
func helperAgentCmdlineMatches(raw []byte, sock string) bool {
	args := strings.Split(string(raw), "\x00")
	return len(args) >= 4 && args[0] == "solud" &&
		args[1] == "agent" && args[2] == "--socket" && args[3] == sock
}

func (h *linuxHelper) agentIDs(dir string) []string {
	entries, err := os.ReadDir(filepath.Join(dir, "agents"))
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if ok && e.Type().IsRegular() && helperproto.ValidAgentID(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (h *linuxHelper) List(_ context.Context, a helperproto.ListArgs) (helperproto.ListResult, error) {
	dir := h.dir()
	r := helperproto.ListResult{Agents: []helperproto.AgentEntry{}}
	var last string
	for _, id := range h.agentIDs(dir) {
		if id <= a.After {
			continue
		}
		rec, ok := h.readRecord(dir, id)
		if !ok {
			continue // a record that doesn't parse is skipped, like a bad id
		}
		if len(r.Agents) == a.Limit {
			r.Next = last
			break
		}
		r.Agents = append(r.Agents, helperproto.AgentEntry{
			AgentID: id, PaneID: rec.PaneID, Pid: rec.Pid, StartTicks: rec.StartTicks,
			Alive: h.alive(dir, id, rec),
		})
		last = id
	}
	return r, nil
}

// liveAgent returns the record of an agent whose identity holds.
func (h *linuxHelper) liveAgent(agentID string) (helperAgentRecord, error) {
	dir := h.dir()
	rec, ok := h.readRecord(dir, agentID)
	if !ok || !h.alive(dir, agentID, rec) {
		return rec, helperproto.Errorf(helperproto.CodeNotFound, "no running agent %s", agentID)
	}
	return rec, nil
}

func (h *linuxHelper) Cwd(_ context.Context, agentID string) (string, error) {
	rec, err := h.liveAgent(agentID)
	if err != nil {
		return "", err
	}
	return liveCwdOfPids(rec.Pid, descendantPids(rec.Pid)), nil
}

// Kill binds a pidfd to the recorded pid first, then checks the identity,
// then signals through the pidfd: a pid that exits and is reused after the
// check can't receive the signal (as killLegacyPaneAgent does).
func (h *linuxHelper) Kill(_ context.Context, a helperproto.KillArgs) error {
	dir := h.dir()
	notFound := helperproto.Errorf(helperproto.CodeNotFound, "no running agent %s", a.AgentID)
	rec, ok := h.readRecord(dir, a.AgentID)
	if !ok || rec.Pid <= 0 {
		return notFound
	}
	fd, err := unix.PidfdOpen(rec.Pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return notFound
	}
	if err != nil {
		return helperproto.Errorf(helperproto.CodeInternal, "pidfd: %v", err)
	}
	defer unix.Close(fd)
	if !h.alive(dir, a.AgentID, rec) {
		return notFound
	}
	if a.How == helperproto.KillTerm {
		// The agent forwards SIGTERM to its pane's process group itself.
		if err := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			return helperproto.Errorf(helperproto.CodeInternal, "signal: %v", err)
		}
		return nil
	}
	// Force: the agent and every process under it. A --no-pty child has
	// its own process group and no terminal to hang up, so killing the
	// agent alone would leave it running while list says the pane is gone.
	tree := pinDescendants(rec.Pid)
	defer tree.close()
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return helperproto.Errorf(helperproto.CodeInternal, "signal: %v", err)
	}
	tree.kill()
	return nil
}

// pinnedTree holds pidfds for an agent's descendants, bound while the agent
// still lives, so the later SIGKILL can't reach a reused pid.
type pinnedTree []int

// pinDescendants opens a pidfd for each descendant of root, then keeps only
// those still under root in a fresh scan: a pid that exited and was reused
// between the scans leaves its pidfd on the dead process, which ESRCH skips.
func pinDescendants(root int) pinnedTree {
	var fds pinnedTree
	pinned := map[int]int{}
	for _, pid := range scanProcessTree().descendantPids(root) {
		if pid == root {
			continue
		}
		if fd, err := unix.PidfdOpen(pid, 0); err == nil {
			pinned[pid] = fd
		}
	}
	still := map[int]bool{}
	for _, pid := range scanProcessTree().descendantPids(root) {
		still[pid] = true
	}
	for pid, fd := range pinned {
		if still[pid] {
			fds = append(fds, fd)
		} else {
			_ = unix.Close(fd)
		}
	}
	return fds
}

func (t pinnedTree) kill() {
	for _, fd := range t {
		_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
	}
}

func (t pinnedTree) close() {
	for _, fd := range t {
		_ = unix.Close(fd)
	}
}

func (h *linuxHelper) Reap(_ context.Context, agentID string) error {
	dir := h.dir()
	rec, ok := h.readRecord(dir, agentID)
	if ok && h.alive(dir, agentID, rec) {
		return helperproto.Errorf(helperproto.CodeBusy, "agent %s is still running", agentID)
	}
	if err := h.removeAgentFiles(dir, agentID); err != nil {
		return helperproto.Errorf(helperproto.CodeInternal, "reap: %v", err)
	}
	return nil
}

// removeAgentFiles removes an agent's socket, record, startup log and
// diagnostics dir. Files already gone are fine; any other failure is
// returned, so reap can be retried. The record goes last: while any file
// remains, list still names the agent.
func (h *linuxHelper) removeAgentFiles(dir, agentID string) error {
	sock, recordPath, logPath, err := helperAgentPaths(dir, agentID)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var errs []error
	for _, p := range []string{sock, logPath, recordPath + ".tmp"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := os.RemoveAll(helperAgentDiagDir(dir, agentID)); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		if err := os.Remove(recordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func helperAgentDiagDir(dir, agentID string) string {
	return filepath.Join(dir, "agents", agentID+".d")
}

// Drain needs nothing beyond the session's barrier: every ServeAgent has
// returned and closed its socket connection by the time it runs.
func (h *linuxHelper) Drain(context.Context) error { return nil }

// ServeAgent splices one agent stream to that agent's unix socket, after
// checking that the process on the other end of the socket is the recorded
// agent (SO_PEERCRED pid and uid), so a socket path swapped after the
// identity check can't receive the pane's input.
func (h *linuxHelper) ServeAgent(agentID string, c net.Conn) {
	rec, err := h.liveAgent(agentID)
	if err != nil {
		return
	}
	sock, _, _, err := helperAgentPaths(h.dir(), agentID)
	if err != nil {
		return
	}
	s, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		h.logf("agent %s: dial: %v", agentID, err)
		return
	}
	if !unixPeerIs(s, rec.Pid, h.uid) || !h.alive(h.dir(), agentID, rec) {
		_ = s.Close()
		h.logf("agent %s: the socket's peer is not the recorded agent; refused", agentID)
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(s, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, s); done <- struct{}{} }()
	<-done
	_ = s.Close()
	_ = c.Close()
	_ = c.SetDeadline(time.Unix(1, 0))
	<-done
}

// AgentByPid implements helperproto.AgentRegistry. It returns only an agent
// whose process identity holds.
func (h *linuxHelper) AgentByPid(pid int) (helperproto.AgentInfo, bool) {
	dir := h.dir()
	if dir == "" || pid <= 0 {
		return helperproto.AgentInfo{}, false
	}
	for _, id := range h.agentIDs(dir) {
		rec, ok := h.readRecord(dir, id)
		if ok && rec.Pid == pid && h.alive(dir, id, rec) {
			return helperproto.AgentInfo{AgentID: id, PaneID: rec.PaneID, Pid: pid, StartTicks: rec.StartTicks}, true
		}
	}
	return helperproto.AgentInfo{}, false
}

// unixPeerIs reports whether the peer of a unix socket connection is pid,
// running as uid (SO_PEERCRED, fixed by the kernel at connect).
func unixPeerIs(c net.Conn, pid, uid int) bool {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil || cred == nil {
		return false
	}
	return int(cred.Pid) == pid && int(cred.Uid) == uid
}

func utsString(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// wslNetworkingMode asks `wslinfo --networking-mode` (WSL 2.0+), and says
// unknown when it isn't there or answers anything else.
func wslNetworkingMode() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wslinfo", "--networking-mode").Output()
	if err != nil {
		return helperproto.NetworkingUnknown
	}
	switch strings.TrimSpace(string(out)) {
	case "nat":
		return helperproto.NetworkingNAT
	case "mirrored":
		return helperproto.NetworkingMirrored
	}
	return helperproto.NetworkingUnknown
}
