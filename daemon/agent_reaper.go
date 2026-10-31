package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Orphaned/duplicate agent reaping.
//
// Agent sockets are FILESYSTEM unix sockets and the agents are Setsid-detached
// so they outlive a daemon restart. The resume/restore paths os.Remove a
// socket FILE to let a fresh agent rebind — but that unlinks the path of a
// still-ALIVE old agent (which never re-creates it), leaving a process that is
// alive but unreachable by `dialAgent`. On the next restart AttachExisting reads
// "can't dial" as "agent dead", suspends the pane, and the orphan lingers
// FOREVER — nothing reaps the PROCESS (the socket sweep only deletes files). A
// resume then binds yet another agent on the same path. Repeated across daemon
// restarts this leaks dozens of bash+claude(node) trees (observed: 85 agents for
// ~48 panes), a real chunk of WSL memory. This reaper kills agent process trees
// no live pane owns; kill-before-replace (callers) stops new orphans at the source.

// procAgent identifies one discovered `solud agent` process by pid + the socket
// it was told to serve. Kept tiny so the reap decision is a pure, testable function.
type procAgent struct {
	pid    int
	socket string
}

// agentsToReap returns the pids to kill: every discovered agent whose pid is NOT
// owned by a RUNNING pane. Pure (no /proc, no signals) so it's unit-testable;
// reapOrphanAgents wraps it with discovery + the actual kill.
func agentsToReap(agents []procAgent, ownedRunningPids map[int]bool) []int {
	var out []int
	for _, a := range agents {
		if a.pid <= 0 || ownedRunningPids[a.pid] {
			continue
		}
		out = append(out, a.pid)
	}
	return out
}

// killAgentTree SIGKILLs an agent process AND all its descendants (the bash -lc
// wrapper + claude + its node subtree). A single process-group kill misses part
// of the tree because a --no-pty agent puts its child in its OWN process group
// (pipeChildSysProcAttr), so we walk descendants instead. Idempotent — signaling
// an already-dead pid is a harmless no-op.
func killAgentTree(pid int) {
	if pid <= 0 {
		return
	}
	if scope := ownedScopeForPID(pid); scope != nil {
		_, _ = stopProcessScopeIdentity(*scope)
		return
	}
	for _, d := range descendantPids(pid) { // inclusive of pid itself
		_ = hardKillPid(d)
	}
}

// reapDetachedTree SIGKILLs every pid from a pre-kill descendant snapshot that
// is still alive. A managed harness client is spawned DETACHED (its own
// session) by launch.mjs, so the pane child's process-group teardown never
// signals it; once its wrapper dies it is reparented to init, a descendant walk
// can no longer reach it, and its DSH child keeps the session's flock — so the
// next /resume answers an opaque "Internal error". The caller must snapshot the
// tree BEFORE the kill and call this immediately after. Idempotent; PIDs are
// not recycled within this millisecond window (Linux allocates them
// monotonically until wraparound), the same assumption killAgentTree makes.
func reapDetachedTree(pids []int) {
	for _, pid := range pids {
		if pid > 0 && processAlive(pid) {
			_ = hardKillPid(pid)
		}
	}
}

// agentPidOwnsSocket reports whether pid is a live `solud agent --socket
// <socketPath> …` process — i.e. genuinely the agent serving that exact socket.
// Identity-checking the cmdline (not just pidAlive) guards every kill against a
// recycled pid. Linux-only (/proc); false elsewhere.
func agentPidOwnsSocket(pid int, socketPath string) bool {
	if pid <= 0 || socketPath == "" || runtime.GOOS != "linux" {
		return false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	isAgent, ownsSocket := parseAgentCmdline(raw)
	return isAgent && ownsSocket == socketPath
}

// liveOrphanRebindWait bounds how long the restart reattach loop waits for a
// live agent to re-bind a socket FILE that was unlinked out from under it. The
// agent's socket self-heal watchdog (agent.go) re-creates the path on a ~1s
// tick; waiting a few seconds lets us RECONNECT to the still-running session
// (preserving the in-flight turn) instead of killing+respawning it. A pre-fix
// agent has no watchdog, so this times out and we fall back to kill+respawn.
const liveOrphanRebindWait = 4 * time.Second

// waitForAgentSocket polls until pid's socket FILE reappears AND pid still owns
// that socket, or `within` elapses. Returns true when the socket is back and
// the same agent re-bound it (so AttachExisting can dial it). Linux-only via
// agentPidOwnsSocket; returns false elsewhere.
func waitForAgentSocket(pid int, socketPath string, within time.Duration) bool {
	if pid <= 0 || socketPath == "" {
		return false
	}
	deadline := time.Now().Add(within)
	for {
		if _, err := os.Stat(socketPath); err == nil && agentPidOwnsSocket(pid, socketPath) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// parseAgentCmdline pulls the "is this our `solud agent`?" + served-socket-path
// out of a NUL-separated /proc/<pid>/cmdline. Returns (isOurAgent, socketPath).
// Split out so the parsing is unit-testable without /proc.
func parseAgentCmdline(raw []byte) (isAgent bool, socket string) {
	args := bytes.Split(raw, []byte{0})
	if len(args) < 2 || !bytes.Contains(args[0], []byte("solud")) {
		return false, ""
	}
	var sawAgent bool
	for i, a := range args {
		switch string(a) {
		case "agent":
			sawAgent = true
		case "--socket":
			if i+1 < len(args) {
				socket = string(args[i+1])
			}
		}
	}
	return sawAgent && socket != "", socket
}

// reapOrphanAgents kills every `solud agent` process tree that no RUNNING pane
// owns. MUST run AFTER the restart restore loop so reattached agents are already
// registered (their pid is the pane's live Pid) and therefore spared. A suspended
// pane (Status != running) is intentionally NOT an owner — its agent, if somehow
// still alive, is an orphan we want gone. Linux-only (reads /proc); no-op elsewhere.
func reapOrphanAgents(mgr *Manager, agentsDir string, retained ...map[string]ownerRetainedHistory) {
	requireNormalBootEngine("agent_reaper.reapOrphanAgents")
	if runtime.GOOS != "linux" {
		return
	}
	// CONFINE the reap to THIS daemon's own agents — those whose socket lives in
	// our <dataDir>/agents. The /proc scan is machine-wide, so WITHOUT this scope
	// the reaper kills agents belonging to a DIFFERENT daemon (a second install, or
	// a dev/test daemon on another port + dataDir): that daemon's panes aren't in
	// our manager, so every one of its agents reads as "unowned" and gets SIGKILLed
	// out from under it. (This is exactly how a throwaway test daemon on an empty
	// dataDir wiped a live daemon's whole agent fleet.) The socket path is the
	// daemon identity the agent already carries on its argv, and a daemon never has
	// agents outside its own agentsDir (sockets are always <dataDir>/agents/pane_*.sock),
	// so matching agentsDir confines the reap to agents we are actually responsible for.
	wantDir := filepath.Clean(agentsDir)
	owned := map[int]bool{}
	for _, p := range mgr.List() {
		if p.Status == PaneRunning && p.Pid > 0 {
			owned[p.Pid] = true
		}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	self := os.Getpid()
	var found []procAgent
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		// Only OUR agents (socket under agentsDir) are reap candidates; a foreign
		// daemon's agent is skipped even though its cmdline matches parseAgentCmdline.
		if ok, sock := parseAgentCmdline(raw); ok && socketUnderDir(sock, wantDir) {
			protected := false
			for _, histories := range retained {
				for _, history := range histories {
					if !history.RestoreEnabled && history.State.Phase != "retired" && history.Pane.SocketPath == sock {
						protected = true
					}
				}
			}
			if !protected {
				found = append(found, procAgent{pid: pid, socket: sock})
			}
		}
	}
	reap := agentsToReap(found, owned)
	for _, pid := range reap {
		killAgentTree(pid)
	}
	if len(reap) > 0 {
		log.Printf("reaped %d orphaned agent process tree(s) under %s (no live pane owns them)", len(reap), wantDir)
	}
}

// socketUnderDir reports whether an agent socket path lives DIRECTLY in dir — i.e.
// it is one of THIS daemon's <dataDir>/agents/pane_*.sock. reapOrphanAgents uses it
// to confine the machine-wide /proc scan to the calling daemon's own agents, so a
// second daemon (different dataDir) never has its agents reaped. An empty dir
// matches nothing (fail safe: reap none rather than reap everything).
func socketUnderDir(sock, dir string) bool {
	if dir == "" || sock == "" {
		return false
	}
	return filepath.Dir(filepath.Clean(sock)) == filepath.Clean(dir)
}

// procHarnessClient is one discovered managed terminal harness client: its pid
// and the SoluTerminal pane it was launched for (SOLU_PANE_ID from its env, ""
// for a user's out-of-pane session).
type procHarnessClient struct {
	pid    int
	paneID string
	// dataDir is the client's SOLU_HARNESS_DATA_DIR — the data dir of the
	// daemon that launched it (launch.mjs sets it for every provider). "" = an
	// untagged client, which no daemon may reap.
	dataDir string
}

// harnessClientBelongsTo reports whether a managed harness client was launched
// by the daemon that owns dataDir. The /proc scan is machine-wide, and pane
// ids only mean something to the daemon that created them: without this a
// second daemon (a side-port test build, a dev instance with its own -data)
// saw every client of the main daemon as "pane gone" and SIGKILLed them all
// at boot, interrupting live DeepSeek/Pi sessions.
func harnessClientBelongsTo(c procHarnessClient, dataDir string) bool {
	clientDir := strings.TrimSpace(c.dataDir)
	if clientDir == "" || strings.TrimSpace(dataDir) == "" {
		// No ownership tag (a client launched before launch.mjs set it for its
		// provider): we cannot prove it is ours, so never kill it.
		return false
	}
	return filepath.Clean(clientDir) == filepath.Clean(dataDir)
}

// harnessClientsToReap decides which managed harness clients are orphans.
// runningPanes maps each RUNNING pane id to whether its agent pid is known
// (true) or not yet published (false). A client is reaped when its pane is
// gone, or when the pane lives but the client is not a descendant of the
// pane's CURRENT agent — a predecessor respawn left it behind. A client with no
// pane id is a user's out-of-pane session and is never touched; a running pane
// whose pid is unknown spares its clients. Pure so it is unit-testable without
// /proc.
func harnessClientsToReap(clients []procHarnessClient, runningPanes map[string]bool, ownedPids map[int]bool) []int {
	var out []int
	for _, c := range clients {
		if c.pid <= 0 || strings.TrimSpace(c.paneID) == "" {
			continue // unmanaged or out-of-pane: never reap
		}
		pidKnown, running := runningPanes[c.paneID]
		if !running {
			out = append(out, c.pid) // its pane no longer exists
			continue
		}
		if !pidKnown {
			continue // pane is live but its agent pid is not published yet — spare
		}
		if !ownedPids[c.pid] {
			out = append(out, c.pid) // a previous agent was replaced; this client is orphaned
		}
	}
	return out
}

// reapOrphanHarnessClients kills managed terminal harness clients (DeepSeek's
// deepseek-terminal.mjs, Pi's approval extension) left behind by a previous
// pane agent. launch.mjs spawns the client DETACHED, so a pane restart's
// process-group teardown misses it; the kernel reparents it to init and its
// harness child keeps the session's cross-process flock — every later /resume
// then answers an opaque "Internal error". Runs at boot after the restore loop
// (like reapOrphanAgents): a reattached pane's own client is already a
// descendant of its live agent and is spared. Confined to clients carrying
// SOLU_PANE_ID, so a user's own `deepseek` outside SoluTerminal is never
// touched. Linux-only (/proc); no-op elsewhere.
func reapOrphanHarnessClients(mgr *Manager, dataDir string, retained ...map[string]ownerRetainedHistory) {
	if runtime.GOOS != "linux" || mgr == nil {
		return
	}
	tree := snapshotProcessTreeMaxAge(0)
	runningPanes := map[string]bool{}
	for _, histories := range retained {
		for id, history := range histories {
			if !history.RestoreEnabled && history.State.Phase != "retired" {
				runningPanes[id] = true
			}
		}
	}
	owned := map[int]bool{}
	for _, p := range mgr.List() {
		if p.Status != PaneRunning {
			continue
		}
		known := p.Pid > 0
		runningPanes[p.ID] = known
		if known {
			for _, pid := range tree.descendantPids(p.Pid) {
				owned[pid] = true
			}
		}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	self := os.Getpid()
	var clients []procHarnessClient
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		cmd := readHarnessCmdline(pid)
		if !isManagedDeepseekCmdline(cmd) && !isManagedPiCmdline(cmd) {
			continue
		}
		env, ok := readProcEnviron(pid)
		if !ok {
			continue
		}
		c := procHarnessClient{pid: pid, paneID: strings.TrimSpace(env["SOLU_PANE_ID"]), dataDir: env["SOLU_HARNESS_DATA_DIR"]}
		if !harnessClientBelongsTo(c, dataDir) {
			continue // another daemon's client — its pane ids are not ours to judge
		}
		clients = append(clients, c)
	}
	reap := harnessClientsToReap(clients, runningPanes, owned)
	for _, pid := range reap {
		killAgentTree(pid)
	}
	if len(reap) > 0 {
		log.Printf("reaped %d orphaned managed-harness client tree(s) (no live pane owns them)", len(reap))
	}
}
