//go:build !windows

package main

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// descendantPids walks /proc to find all descendants of root (inclusive).
func descendantPids(root int) []int {
	return snapshotProcessTree().descendantPids(root)
}

type processTreeSnapshot struct {
	children map[int][]int
}

// Process-tree cache. snapshotProcessTree reads every /proc/<pid>/stat, which
// is expensive on a long-uptime host with hundreds of processes. computeState
// calls it (via HasCodex/ClaudeDescendant) for every pane on every 1-2s ticker
// sweep — ~100 full /proc scans/sec across 36 panes × 3 tickers, which alone
// burned ~50% of a core. Process membership doesn't change meaningfully
// sub-second, so a short TTL lets one scan serve every caller in the window.
var (
	procTreeMu       sync.Mutex
	procTreeCache    processTreeSnapshot
	procTreeCachedAt time.Time
)

const procTreeTTL = time.Second

func snapshotProcessTree() processTreeSnapshot {
	return snapshotProcessTreeMaxAge(procTreeTTL)
}

func snapshotProcessTreeMaxAge(maxAge time.Duration) processTreeSnapshot {
	procTreeMu.Lock()
	defer procTreeMu.Unlock()
	if procTreeCache.children != nil && time.Since(procTreeCachedAt) < maxAge {
		return procTreeCache
	}
	snap := scanProcessTree()
	procTreeCache = snap
	procTreeCachedAt = time.Now()
	return snap
}

// scanProcessTree does the actual /proc walk. Callers should use the cached
// snapshotProcessTree unless they specifically need an uncached read.
func scanProcessTree() processTreeSnapshot {
	parents := map[int]int{} // pid -> ppid
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return processTreeSnapshot{}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ppid := readPpid(pid)
		if ppid > 0 {
			parents[pid] = ppid
		}
	}

	children := map[int][]int{}
	for pid, ppid := range parents {
		children[ppid] = append(children[ppid], pid)
	}
	return processTreeSnapshot{children: children}
}

func (s processTreeSnapshot) descendantPids(root int) []int {
	out := []int{root}
	if len(s.children) == 0 {
		return out
	}
	// A /proc scan is not atomic, so PID reuse mid-scan can stitch a cycle;
	// the visited set keeps the walk bounded by the process count.
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range s.children[cur] {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out
}

func readPpid(pid int) int {
	if ppid, ok := darwinReadPpid(pid); ok {
		return ppid
	}
	f, _ := statFieldsAfterComm(pid)
	if len(f) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(f[1])
	return ppid
}

// statFieldsAfterComm returns the whitespace-separated fields of
// /proc/<pid>/stat that follow the `(comm)` token. Index 0 is `state`,
// 1 is `ppid`, 2 is `pgrp`, 3 is `session`, 4 is `tty_nr`, 5 is
// `tpgid`, etc. (see proc(5)). Parsing past comm is fiddly because
// the executable name can contain spaces and parens — we find the
// LAST ')' and split what follows, which is robust against pathological
// comms like "weird ) name".
func statFieldsAfterComm(pid int) ([]string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil, false
	}
	s := string(data)
	rp := strings.LastIndex(s, ")")
	if rp < 0 || rp+1 >= len(s) {
		return nil, false
	}
	return strings.Fields(s[rp+1:]), true
}

// readTpgid returns the foreground process-group id of pid's controlling
// terminal — i.e. the pgid that `tcgetpgrp(fd)` would return inside that
// process. -1 means "no controlling tty" (kernel sentinel). 0 means
// "unreadable / no answer", treated the same as -1 by callers.
func readTpgid(pid int) int {
	f, ok := statFieldsAfterComm(pid)
	if !ok || len(f) < 6 {
		return 0
	}
	tpgid, _ := strconv.Atoi(f[5])
	return tpgid
}

// paneForegroundCmd reports the pane's *currently-running* foreground
// command — distinct from the pane's spawn argv. ok=false when the pane's
// root pid IS the foreground (e.g. bash sitting at its prompt, or a TUI
// like claude/codex/vim in its event loop with no transient child). In
// that case there is nothing actionable for "command long-running"
// purposes; treating PTY byte activity as "busy" gives false positives on
// any shell with a chatty prompt (starship, OSC title updates, etc.).
//
// When ok=true, fgPid is the leader of the foreground process group and
// cmd is its argv (or comm if argv is empty). Callers should restart any
// per-command timer when fgPid changes — one command finishing and
// another starting is two distinct runs even within one busy window.
func paneForegroundCmd(rootPid int) (fgPid int, cmd string, ok bool) {
	if rootPid <= 0 {
		return 0, "", false
	}
	tpgid := readTpgid(rootPid)
	if tpgid <= 0 || tpgid == rootPid {
		return 0, "", false
	}
	cmd = readCmdline(tpgid)
	if cmd == "" {
		// argv is empty for some short-lived helpers and kernel threads;
		// the canonical fallback is /proc/<pid>/comm (the binary name as
		// known to the scheduler), which exec'd processes always have.
		if comm, err := os.ReadFile("/proc/" + strconv.Itoa(tpgid) + "/comm"); err == nil {
			cmd = strings.TrimSpace(string(comm))
		}
	}
	fgPid = tpgid
	ok = true
	return
}

// readHarnessCmdline returns a descendant's argv for managed-harness
// detection. Managed CLI harnesses are Unix/WSL-only, so the Windows build
// stubs this to empty. Unix just reuses the /proc/<pid>/cmdline reader.
func readHarnessCmdline(pid int) string {
	return readCmdline(pid)
}

// readProcEnviron parses /proc/<pid>/environ (NUL-separated KEY=VALUE entries)
// into a map. ok=false on any read error (process exited, permission). Values
// are kept but callers only ever key on presence; nothing here is logged.
func readProcEnviron(pid int) (map[string]string, bool) {
	if env, ok := darwinReadEnviron(pid); ok {
		return env, true
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, false
	}
	out := make(map[string]string, 64)
	for _, kv := range strings.Split(string(b), "\x00") {
		if kv == "" {
			continue
		}
		if i := strings.IndexByte(kv, '='); i >= 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out, true
}
