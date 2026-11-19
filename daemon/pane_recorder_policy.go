package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Pane recorder policy: every number and list the security owner ruled on
// (launch/roles/security/notes/reviews/pane-recorder-rules-2026-11-19.md) lives
// here, so a changed ruling is a one-line change.
const (
	// recordingsDirName is the folder under the data dir that holds every
	// recording, one subfolder per pane id. credentialOwnedPath refuses it on
	// every generic file surface.
	recordingsDirName = "recordings"
	// recordingDirMode and recordingFileMode are set explicitly, whatever the
	// umask. A folder that is group- or world-accessible is refused.
	recordingDirMode  os.FileMode = 0o700
	recordingFileMode os.FileMode = 0o600
	// recordingFileTimeLayout names each file by its UTC start time.
	recordingFileTimeLayout = "20060102T150405Z"

	// recordingMaxAge deletes a file this long after its last write.
	recordingMaxAge = 7 * 24 * time.Hour
	// recordingSweepEvery is how often the deletion sweep runs, at daemon start
	// and on the host storage sampler's existing ticker, recording on or off.
	recordingSweepEvery = time.Hour
	// recordingTotalCap bounds every file under recordings/ together.
	recordingTotalCap int64 = 2_000_000_000
	// recordingFileCap starts a new file once one reaches it.
	recordingFileCap int64 = 500_000_000

	// recordingMaxOn switches a recording off after this long on.
	recordingMaxOn = 8 * time.Hour
	// recordingPolicyCheckEvery re-checks the (f) refusals while recording. An
	// (f) stop truncates the file back to the last passing check, so nothing
	// written after it survives.
	recordingPolicyCheckEvery = 5 * time.Second

	// recordingQueueBytes bounds output waiting for the disk. Past it, output
	// is dropped and counted, and an "m" event records the gap: the pane's
	// read loop never waits on the disk.
	recordingQueueBytes = 4 << 20
)

// Stop and refusal reasons. The (f) ones name the rule that stopped it.
const (
	recordingStopOff        = "switched off"
	recordingStopClosed     = "pane closed"
	recordingStopReplaced   = "pane restarted or resumed"
	recordingStopMaxOn      = "8-hour limit"
	recordingStopDiskCap    = "disk cap"
	recordingStopFolder     = "recordings folder refused"
	recordingStopWriteError = "write failed"
	recordingStopHandle     = "pane handle not allowed (rule f1)"
	recordingStopCwd        = "pane folder not allowed (rule f2)"
	recordingStopShell      = "not an agent pane (rule f3)"
	recordingStopDeleted    = "recordings deleted"
)

// recordingRefusedHandles and recordingRefusedHandlePrefixes are (f)1.
var (
	recordingRefusedHandles        = []string{"secure", "secure2", "ops", "release-lead"}
	recordingRefusedHandlePrefixes = []string{"release-", "publish-", "gate-"}
)

// recordingRefusedDirs is (f)2: a pane whose cwd or live cwd is under one of
// these is never recorded. Paths relative to home are joined to it.
func recordingRefusedDirs(home, dataDir string) []string {
	dirs := []string{"/etc", dataDir}
	if home != "" {
		for _, rel := range []string{"wsl-projects/launch/roles/security", "wsl-projects/launch/deploy", ".ssh", ".config"} {
			dirs = append(dirs, filepath.Join(home, rel))
		}
	}
	return dirs
}

// recordingAgents is (f)3: only these agents' panes are recorded. A plain
// shell, or an agent pane whose agent has exited to its shell, is refused.
var recordingAgents = map[string]bool{"claude": true, "codex": true}

// recordingPlatformSupported is false on a Windows-side daemon until the
// security owner has reviewed Windows file permissions.
func recordingPlatformSupported() bool { return runtime.GOOS != "windows" }

// recordingPaneFacts is what the (f) rules read about a pane.
type recordingPaneFacts struct {
	Handle string
	Agent  string
	Cwd    string
	// LiveCwds are the current folders of every process in the pane;
	// CwdUnknown is set when they can't be read on this platform.
	LiveCwds   []string
	CwdUnknown bool
	// AgentRunning is whether the pane's agent process is running, from the
	// pane's process tree. A shell left behind by an exited agent is false.
	AgentRunning bool
}

// recordingRefusal returns the (f) stop reason for a pane, or "" when it may
// be recorded.
func recordingRefusal(f recordingPaneFacts, home, dataDir string) string {
	handle := strings.ToLower(strings.TrimSpace(f.Handle))
	for _, h := range recordingRefusedHandles {
		if handle == h {
			return recordingStopHandle
		}
	}
	for _, prefix := range recordingRefusedHandlePrefixes {
		if strings.HasPrefix(handle, prefix) {
			return recordingStopHandle
		}
	}
	if f.CwdUnknown {
		return recordingStopCwd
	}
	refused := recordingRefusedDirs(home, dataDir)
	for _, cwd := range append([]string{f.Cwd}, f.LiveCwds...) {
		if cwd != "" && recordingPathUnder(cwd, refused) {
			return recordingStopCwd
		}
	}
	if !recordingAgents[strings.ToLower(f.Agent)] || !f.AgentRunning {
		return recordingStopShell
	}
	return ""
}

// recordingPathUnder reports whether path, or what it resolves to, is inside
// any of roots (also compared resolved).
func recordingPathUnder(path string, roots []string) bool {
	candidates := []string{filepath.Clean(path)}
	if resolved := resolveExistingAncestor(path); resolved != "" {
		candidates = append(candidates, filepath.Clean(resolved))
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		rootForms := []string{filepath.Clean(root)}
		if resolved := resolveExistingAncestor(root); resolved != "" {
			rootForms = append(rootForms, filepath.Clean(resolved))
		}
		for _, c := range candidates {
			for _, r := range rootForms {
				if c == r || strings.HasPrefix(c, r+string(filepath.Separator)) {
					return true
				}
			}
		}
	}
	return false
}
