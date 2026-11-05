package main

import (
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
)

// runningAgentVersion is the version of the agent CLI running in the pane,
// as the operating system shows it: the binary the pane's agent processes
// execute, never what the pane prints or what an agent says it is. The
// installed CLI is not enough: a pane started before an update keeps running
// the old build. "" when it cannot be verified. A variable for tests.
//
// Only processes in this pane's tree count, and our own pane shim (solud run
// under the agent's name, from ~/.solu/codex-inline-bin) is skipped by what
// it is, not by its name. For Codex the pane's recorded agent must be codex,
// and each other process named codex must be an npm-installed Codex binary
// owned by this user (agentProcessVersion); any one that isn't makes the
// answer "".
var runningAgentVersion = func(p *Pane, agent string) string {
	if p.Pid <= 0 {
		return ""
	}
	if agent == agentCodex {
		p.mu.Lock()
		recorded := strings.TrimSpace(p.Agent)
		p.mu.Unlock()
		if recorded != agentCodex {
			return ""
		}
	}
	version := ""
	for _, pid := range descendantPids(p.Pid) {
		if pid == p.Pid || readComm(pid) != agent {
			continue
		}
		v, ours := agentProcessVersion(agent, pid)
		if ours {
			continue
		}
		if v == "" || version != "" && v != version {
			return ""
		}
		version = v
	}
	return version
}

// agentProcessVersion is the version of the agent build process pid runs,
// or ours: the process is solud itself. Cached per pid and start time, and
// reused only while the process still runs the same file: a process keeps its
// pid and start time across exec, and an npm update deletes the binary it
// runs, so the executable's link and file identity are read every time.
func agentProcessVersion(agent string, pid int) (version string, ours bool) {
	exe, fileID, err := procExeIdentity(pid)
	if err != nil {
		return "", false
	}
	start, startOK := mcpProcessStartTime(pid)
	key := agentExeCacheKey{agent: agent, pid: pid}
	if startOK {
		agentExeCache.Lock()
		e, ok := agentExeCache.m[key]
		agentExeCache.Unlock()
		if ok && e.start == start && e.exe == exe && e.fileID == fileID {
			return e.version, e.ours
		}
	}
	procExe := fmt.Sprintf("/proc/%d/exe", pid)
	if isOwnSoludExe(procExe, exe) {
		version, ours = "", true
	} else {
		switch agent {
		case agentCodex:
			// The binary on disk must still be the one running (not
			// deleted, so its package.json describes it) and be this
			// user's.
			if !strings.HasSuffix(exe, " (deleted)") && filepath.IsAbs(exe) && agentExeOwnedByDaemon(exe) {
				version = agentVersionFromExe(agent, exe, os.ReadFile)
			}
		default:
			version = agentVersionFromExe(agent, exe, os.ReadFile)
		}
	}
	if startOK {
		agentExeCache.Lock()
		if len(agentExeCache.m) >= agentExeCacheMax {
			agentExeCache.m = map[agentExeCacheKey]agentExeCacheEntry{}
		}
		agentExeCache.m[key] = agentExeCacheEntry{start: start, exe: exe, fileID: fileID, version: version, ours: ours}
		agentExeCache.Unlock()
	}
	return version, ours
}

// procExeIdentity is what process pid runs: its /proc/<pid>/exe link (with
// " (deleted)" once the file is gone) and the identity of the file behind
// it. A variable for tests.
var procExeIdentity = func(pid int) (exe string, id exeFileID, err error) {
	procExe := fmt.Sprintf("/proc/%d/exe", pid)
	if exe, err = os.Readlink(procExe); err != nil {
		return "", exeFileID{}, err
	}
	id, _ = exeFileIdentity(procExe)
	return exe, id, nil
}

// exeFileID names a file by device and inode.
type exeFileID struct{ dev, ino uint64 }

type agentExeCacheKey struct {
	agent string
	pid   int
}

type agentExeCacheEntry struct {
	start   uint64
	exe     string
	fileID  exeFileID
	version string
	ours    bool
}

const agentExeCacheMax = 512

var agentExeCache = struct {
	sync.Mutex
	m map[agentExeCacheKey]agentExeCacheEntry
}{m: map[agentExeCacheKey]agentExeCacheEntry{}}

// agentExeOwnedByDaemon: the file at path belongs to this daemon's user. A
// variable for tests.
var agentExeOwnedByDaemon = agentExeOwnedByDaemonPlatform

// ownSoludExes are the paths a pane shim runs solud from: this binary and,
// beside it, the installed name and the update ring's parked builds. A
// variable for tests.
var ownSoludExes = func() []string {
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	dir := filepath.Dir(self)
	out := []string{self}
	for _, name := range []string{"solud", "solud.exe", "solud.old", "solud.old.1", "solud.old.2"} {
		out = append(out, filepath.Join(dir, name))
	}
	return out
}

// ownBuild is this binary's Go module and main package, read once.
var ownBuild = sync.OnceValue(func() *debug.BuildInfo {
	bi, _ := debug.ReadBuildInfo()
	return bi
})

// isOwnSoludExe reports whether a process runs solud: its executable (exe,
// as /proc/<pid>/exe links it) is one of ownSoludExes, or the binary behind
// procExe, even one replaced on disk since, carries this build's module and
// main package.
func isOwnSoludExe(procExe, exe string) bool {
	path := strings.TrimSuffix(exe, " (deleted)")
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	for _, own := range ownSoludExes() {
		if own == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(own); err == nil {
			own = real
		}
		if path == own {
			return true
		}
	}
	mine := ownBuild()
	if mine == nil || mine.Main.Path == "" {
		return false
	}
	theirs, err := buildinfo.ReadFile(procExe)
	return err == nil && theirs.Main.Path == mine.Main.Path && theirs.Path == mine.Path
}

// codexPackageNameRE and codexPackageVersionRE: the npm packages that hold the
// Codex CLI binary, and their versions, of which the x.y.z part is the build.
var (
	codexPackageNameRE    = regexp.MustCompile(`^@openai/codex(-(linux|darwin|win32)-(x64|arm64))?$`)
	codexPackageVersionRE = regexp.MustCompile(`^(\d+\.\d+\.\d+)(-(linux|darwin|win32)-(x64|arm64))?$`)
)

// agentVersionFromExe is the version of the build at exe, a process's
// /proc/<pid>/exe link: Claude's from its versioned path, Codex's from the
// nearest package.json above the binary (only while the binary is not
// deleted, so that file still describes it). The package name and version
// must match exactly; the version is normalised to x.y.z.
func agentVersionFromExe(agent, exe string, readFile func(string) ([]byte, error)) string {
	switch agent {
	case agentClaude:
		if m := claudeExeVersionRE.FindStringSubmatch(exe); m != nil {
			return m[1]
		}
	case agentCodex:
		if strings.HasSuffix(exe, " (deleted)") || !filepath.IsAbs(exe) {
			return ""
		}
		dir := filepath.Dir(exe)
		for i := 0; i < 8 && dir != filepath.Dir(dir); i, dir = i+1, filepath.Dir(dir) {
			b, err := readFile(filepath.Join(dir, "package.json"))
			if err != nil {
				continue
			}
			var pkg struct{ Name, Version string }
			if json.Unmarshal(b, &pkg) != nil || !codexPackageNameRE.MatchString(pkg.Name) {
				return ""
			}
			if m := codexPackageVersionRE.FindStringSubmatch(pkg.Version); m != nil {
				return m[1]
			}
			return ""
		}
	}
	return ""
}
