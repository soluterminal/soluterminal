package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The parts of a report's "App and system details" that describe the computer
// and the pane, beyond versions. Each is a short, fixed-form value a person can
// read in the form's preview.

// feedbackInWSL is isInsideWSL, replaceable in tests.
var feedbackInWSL = isInsideWSL

// feedbackOS names the operating system as a person reads it. A daemon in WSL
// runs Linux inside Windows, and the computer is a Windows one, so it says both.
func feedbackOS() string {
	switch runtime.GOOS {
	case "linux":
		if feedbackInWSL() {
			return "Windows (WSL)"
		}
		return "Linux"
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	}
	return runtime.GOOS
}

// feedbackWindowsBuild is the Windows version and build ("10.0.26100.4652"),
// or "" off Windows or when it can't be read. Read once per daemon, on the
// first report, never at start-up.
var feedbackWindowsBuild = func() string {
	feedbackWindowsBuildOnce.Do(func() { feedbackWindowsBuildValue = feedbackWindowsBuildProbe() })
	return feedbackWindowsBuildValue
}

var (
	feedbackWindowsBuildOnce  sync.Once
	feedbackWindowsBuildValue string
)

var feedbackWindowsVerRE = regexp.MustCompile(`\[Version ([0-9]+(?:\.[0-9]+){1,3})\]`)

// feedbackParseWindowsVer reads "Microsoft Windows [Version 10.0.26100.4652]",
// the output of cmd's ver, and keeps only the number.
func feedbackParseWindowsVer(out string) string {
	if m := feedbackWindowsVerRE.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// feedbackCmdExe is the one cmd.exe the WSL probe may run. Never looked up on
// PATH: in WSL that carries Windows folders a user can write to (@secure).
var feedbackCmdExe = "/mnt/c/Windows/System32/cmd.exe"

// feedbackWSLWindowsVer asks Windows itself through WSL interop. Bounded, so a
// report never waits long on it; any failure leaves the row out.
func feedbackWSLWindowsVer() string {
	if fi, err := os.Lstat(feedbackCmdExe); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, feedbackCmdExe, "/c", "ver")
	applyNoWindow(cmd)
	// Started from a Windows folder, cmd doesn't warn about a UNC path.
	cmd.Dir = filepath.Dir(feedbackCmdExe)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return feedbackParseWindowsVer(string(out))
}

// feedbackPaneKind describes the pane whose text a report carries: its type
// as the app names it ("Terminal", or "Focus" for a native pane) and the agent
// it was started with, if any. Muse is a team-only runtime, so it shows as
// "Other". A shell pane where an agent was started by hand names no agent.
func (a *apiServer) feedbackPaneKind(paneID string) (kind, agent string) {
	if a.mgr == nil || paneID == "" {
		return "", ""
	}
	p, ok := a.mgr.Get(paneID)
	if !ok {
		return "", ""
	}
	p.mu.Lock()
	k, ag, cmd := p.Kind, strings.TrimSpace(p.Agent), p.Cmd
	p.mu.Unlock()
	kind = "Terminal"
	if k == "native" {
		kind = "Focus"
	}
	switch {
	case ag == agentCodex || cmdRunsCodex(cmd):
		agent = "Codex"
	case ag == agentMuse || cmdStartsMuse(cmd):
		agent = "Other"
	case k == "native" || cmdRunsClaude(cmd):
		agent = "Claude Code"
	}
	return kind, agent
}
