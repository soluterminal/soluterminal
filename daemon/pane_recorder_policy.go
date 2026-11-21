package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Pane recorder policy: every number and list the security owner ruled on
// (launch/roles/security/notes/reviews/pane-recorder-rules-2026-11-19.md and
// its amendment pane-recorder-rules-amendment-2026-11-20.md) lives here, so a
// changed ruling is a one-line change.
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

	// Deletion age and total cap are the person's settings
	// (pane_recorder_settings.go), always present and always within these
	// bounds; out-of-bounds values are refused, never clamped. Raising one is
	// the person's only (the switch-on gate); lowering one is anyone's.
	recordingDeleteAfterDaysDefault = 7
	recordingDeleteAfterDaysMin     = 1
	recordingDeleteAfterDaysMax     = 30
	recordingCapMBDefault           = 2000
	recordingCapMBMin               = 500
	recordingCapMBMax               = 10000
	// recordingSweepEvery is how often the deletion sweep runs, at daemon start
	// and on the host storage sampler's existing ticker, recording on or off.
	recordingSweepEvery = time.Hour
	// recordingFileCap starts a new file once one reaches it. Not a setting.
	recordingFileCap int64 = 500_000_000

	// The time limit switches a recording off after this many hours on. The
	// person picks it at switch-on; there is never an unlimited choice.
	recordingHoursDefault = 8
	recordingHoursMin     = 1
	recordingHoursMax     = 24
	// recordingPolicyCheckEvery re-checks the rules while recording. A rule
	// stop truncates the file back to the last passing check, so nothing
	// written after it survives.
	recordingPolicyCheckEvery = 5 * time.Second

	// recordingQueueBytes bounds output waiting for the disk. Past it, output
	// is dropped and counted, and an "m" event records the gap: the pane's
	// read loop never waits on the disk.
	recordingQueueBytes = 4 << 20
)

// Stop and refusal reasons, shown on the pane as "recording stopped: <reason>".
const (
	recordingStopOff        = "switched off"
	recordingStopClosed     = "pane closed"
	recordingStopReplaced   = "pane restarted or resumed"
	recordingStopDiskCap    = "disk cap"
	recordingStopFolder     = "recordings folder refused"
	recordingStopWriteError = "write failed"
	recordingStopHandle     = "pane handle on the do-not-record list"
	recordingStopCwd        = "folder"
	recordingStopShell      = "plain shell"
	recordingStopHardFolder = "pane folder is never recorded"
	recordingStopCwdUnknown = "pane folder can't be read"
	recordingStopDeleted    = "recordings deleted"
)

// recordingStopTimeLimit is the stop reason at a recording's time limit.
func recordingStopTimeLimit(hours int) string { return fmt.Sprintf("%d-hour limit", hours) }

// recordingRule is a code for one of the rules a pane is checked against.
// Codes, never paths or handles, are what the status route, the switch-on
// request and the daemon log carry.
type recordingRule string

const (
	// Defaults the person may override for one switch-on, in the start
	// dialog only (amendment 2026-11-20 section 2).
	recordingRuleHandle recordingRule = "handle"
	recordingRuleShell  recordingRule = "shell"
	// The folder default is overridden per root (Addendum C 1, Addendum D e):
	// folder:<root>, never a path. recordingRuleFolder is the family, the
	// stop and skip code when one of them refuses.
	recordingRuleFolder recordingRule = "folder"
	// Hard rules: no override touches them (section 1.9; an unreadable shell
	// or agent folder counts as a hard folder, Addendum B).
	recordingRuleHardFolder recordingRule = "hard_folder"
	recordingRuleCwdUnknown recordingRule = "cwd_unknown"
)

// recordingFolderRoots are the folder default's roots, each overridden on
// its own: a switch-on names every root the pane is in, and moving into
// another one stops the recording. Home-relative unless absolute.
var recordingFolderRoots = []struct {
	code recordingRule
	path string
}{
	{"folder:launch_security", "wsl-projects/launch/roles/security"},
	{"folder:launch_deploy", "wsl-projects/launch/deploy"},
	{"folder:config", ".config"},
	{"folder:etc", "/etc"},
}

// recordingOverridable is every rule a switch-on may override.
var recordingOverridable = func() map[recordingRule]bool {
	m := map[recordingRule]bool{recordingRuleHandle: true, recordingRuleShell: true}
	for _, r := range recordingFolderRoots {
		m[r.code] = true
	}
	return m
}()

// recordingRuleOrder is the order rules are listed in (log, status, clients).
var recordingRuleOrder = func() []recordingRule {
	out := []recordingRule{recordingRuleHandle}
	for _, r := range recordingFolderRoots {
		out = append(out, r.code)
	}
	return append(out, recordingRuleShell)
}()

// recordingRuleFamily is "folder" for every folder:<root> code.
func recordingRuleFamily(r recordingRule) recordingRule {
	if strings.HasPrefix(string(r), string(recordingRuleFolder)+":") {
		return recordingRuleFolder
	}
	return r
}

// recordingRuleStop maps a rule to the stop reason the person sees.
var recordingRuleStop = map[recordingRule]string{
	recordingRuleHandle:     recordingStopHandle,
	recordingRuleShell:      recordingStopShell,
	recordingRuleFolder:     recordingStopCwd,
	recordingRuleHardFolder: recordingStopHardFolder,
	recordingRuleCwdUnknown: recordingStopCwdUnknown,
}

// recordingStopText is the stop reason for a rule (any folder root: "folder").
func recordingStopText(r recordingRule) string { return recordingRuleStop[recordingRuleFamily(r)] }

// recordingRefusedHandles and recordingRefusedHandlePrefixes are the handle
// default (f)1, overridable.
var (
	recordingRefusedHandles        = []string{"secure", "secure2", "ops", "release-lead"}
	recordingRefusedHandlePrefixes = []string{"release-", "publish-", "gate-"}
)

// recordingHardDirs are the folders never recorded, whatever the person
// overrides: keys and tokens live there. /etc/solu-* is matched by
// recordingUnderEtcSolu. Paths relative to home are joined to it.
func recordingHardDirs(home, dataDir string) []string {
	dirs := []string{dataDir}
	if home != "" {
		for _, rel := range []string{".ssh", ".config/launch-secrets", ".codex"} {
			dirs = append(dirs, filepath.Join(home, rel))
		}
	}
	return dirs
}

// recordingDefaultDirs is the folder default (f)2: refused unless the person
// overrides each root at switch-on.
func recordingDefaultDirs(home string) []string {
	var dirs []string
	for _, r := range recordingFolderRoots {
		if p := recordingRootPath(r.path, home); p != "" {
			dirs = append(dirs, p)
		}
	}
	return dirs
}

func recordingRootPath(path, home string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, path)
}

// recordingEtcSoluPrefix: any /etc entry whose name starts with it is a hard
// folder (/etc/solu-*). recordingEtcDir is /etc (a test seam).
const recordingEtcSoluPrefix = "solu-"

var recordingEtcDir = "/etc"

// recordingAgentNames are the processes that make a pane an agent pane (C1):
// a live one anywhere in the pane's process tree, the pane's own process
// included. Codex installed through npm runs as node with argv[1] "codex"
// (Addendum B 4).
var recordingAgentNames = map[string]bool{"claude": true, "codex": true}

// recordingPlatformSupported is false on a Windows-side daemon until the
// security owner has reviewed Windows file permissions.
func recordingPlatformSupported() bool { return runtime.GOOS != "windows" }

// recordingPaneFacts is what the rules read about a pane.
type recordingPaneFacts struct {
	Handle string
	// Agent is what the pane was started as (recordingPaneAgent): claude,
	// codex, muse or "" for a shell. The rules don't read it; C1 decides by
	// the live process tree (AgentRunning).
	Agent string
	// Cwds are the current folders of the pane's own shell and, while one
	// runs, its agent process; never other descendants (C2). CwdUnknown is
	// set when one of them can't be read, or on a platform without /proc.
	Cwds       []string
	CwdUnknown bool
	// AgentRunning is whether a claude or codex process is live in the
	// pane's process tree (C1). Without one the pane is a plain shell.
	AgentRunning bool
}

// recordingVerdict is how the rules read a pane: a hard rule that refuses it
// whatever is overridden, and the defaults that refuse it unless overridden.
type recordingVerdict struct {
	Hard     recordingRule
	Defaults []recordingRule
}

// recordingCheck reads the rules for a pane.
func recordingCheck(f recordingPaneFacts, home, dataDir string) recordingVerdict {
	var v recordingVerdict
	if f.CwdUnknown {
		v.Hard = recordingRuleCwdUnknown
	} else {
		for _, cwd := range f.Cwds {
			if cwd != "" && (recordingPathUnder(cwd, recordingHardDirs(home, dataDir)) || recordingUnderEtcSolu(cwd)) {
				v.Hard = recordingRuleHardFolder
				break
			}
		}
	}
	if recordingHandleRefused(f.Handle) {
		v.Defaults = append(v.Defaults, recordingRuleHandle)
	}
	if !f.CwdUnknown {
		for _, root := range recordingFolderRoots {
			p := recordingRootPath(root.path, home)
			if p == "" {
				continue
			}
			for _, cwd := range f.Cwds {
				if cwd != "" && recordingPathUnder(cwd, []string{p}) {
					v.Defaults = append(v.Defaults, root.code)
					break
				}
			}
		}
	}
	if !f.AgentRunning {
		v.Defaults = append(v.Defaults, recordingRuleShell)
	}
	return v
}

// refusal returns the rule that refuses the pane given the switch-on's
// overrides, or "" when it may be recorded. A hard rule comes first.
func (v recordingVerdict) refusal(overrides map[recordingRule]bool) recordingRule {
	if v.Hard != "" {
		return v.Hard
	}
	for _, d := range v.Defaults {
		if !overrides[d] {
			return d
		}
	}
	return ""
}

// recordingHandleRefused is the handle default (f)1.
func recordingHandleRefused(handle string) bool {
	handle = strings.ToLower(strings.TrimSpace(handle))
	for _, h := range recordingRefusedHandles {
		if handle == h {
			return true
		}
	}
	for _, prefix := range recordingRefusedHandlePrefixes {
		if strings.HasPrefix(handle, prefix) {
			return true
		}
	}
	return false
}

// recordingUnderEtcSolu reports whether path, or what it resolves to, is
// /etc/solu-* or inside one, including where an /etc/solu-* entry is a
// symlink to a folder elsewhere (a process's cwd reads as the target).
func recordingUnderEtcSolu(path string) bool {
	candidates := []string{filepath.Clean(path)}
	if resolved := resolveExistingAncestor(path); resolved != "" {
		candidates = append(candidates, filepath.Clean(resolved))
	}
	for _, c := range candidates {
		rest, ok := strings.CutPrefix(c, recordingEtcDir+string(filepath.Separator))
		if ok && strings.HasPrefix(rest, recordingEtcSoluPrefix) {
			return true
		}
	}
	entries, _ := filepath.Glob(filepath.Join(recordingEtcDir, recordingEtcSoluPrefix+"*"))
	return len(entries) > 0 && recordingPathUnder(path, entries)
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
