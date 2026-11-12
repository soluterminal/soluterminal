package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// No test asks Windows for its version: that would start cmd.exe under WSL.
func init() { feedbackWindowsBuild = func() string { return "" } }

func TestFeedbackParseWindowsVer(t *testing.T) {
	for in, want := range map[string]string{
		"\r\nMicrosoft Windows [Version 10.0.26100.4652]\r\n":                                                     "10.0.26100.4652",
		"Microsoft Windows [Version 10.0.19045]":                                                                  "10.0.19045",
		"'\\\\wsl.localhost\\Ubuntu' UNC paths are not supported.\r\nMicrosoft Windows [Version 10.0.22631.5039]": "10.0.22631.5039",
		"":                    "",
		"[Version name@host]": "",
	} {
		if got := feedbackParseWindowsVer(in); got != want {
			t.Errorf("feedbackParseWindowsVer(%q) = %q, want %q", in, got, want)
		}
	}
}

// The WSL probe runs only the fixed cmd.exe, a regular file, from its own
// folder; a cmd.exe on PATH is never run (@secure).
func TestFeedbackWSLWindowsVerNeverUsesPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts stand in for cmd.exe")
	}
	defer func(p string) { feedbackCmdExe = p }(feedbackCmdExe)
	onPath, fixed := t.TempDir(), t.TempDir()
	ran := filepath.Join(t.TempDir(), "ran")
	fake := func(dir, script string) string {
		p := filepath.Join(dir, "cmd.exe")
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	fake(onPath, "#!/bin/sh\ntouch "+ran+"\necho 'Microsoft Windows [Version 6.6.6]'\n")
	t.Setenv("PATH", onPath+string(os.PathListSeparator)+os.Getenv("PATH"))

	feedbackCmdExe = filepath.Join(fixed, "missing", "cmd.exe")
	if got := feedbackWSLWindowsVer(); got != "" {
		t.Fatalf("missing cmd.exe gave %q", got)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the cmd.exe on PATH was run")
	}

	real := fake(fixed, "#!/bin/sh\n[ \"$#\" = 3 ] && [ \"$1\" = /d ] && [ \"$2\" = /c ] && [ \"$3\" = ver ] || exit 1\necho \"Microsoft Windows [Version 10.0.26100.4652] in $(pwd)\"\n")
	link := filepath.Join(t.TempDir(), "cmd.exe")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	feedbackCmdExe = link
	if got := feedbackWSLWindowsVer(); got != "" {
		t.Fatalf("a symlinked cmd.exe gave %q", got)
	}

	feedbackCmdExe = real
	if got := feedbackWSLWindowsVer(); got != "10.0.26100.4652" {
		t.Fatalf("fixed cmd.exe gave %q", got)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the cmd.exe on PATH was run")
	}
}

func TestFeedbackOSNamesWSLAsWindows(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the WSL case is a Linux daemon")
	}
	defer func(f func() bool) { feedbackInWSL = f }(feedbackInWSL)
	feedbackInWSL = func() bool { return true }
	if got := feedbackOS(); got != "Windows (WSL)" {
		t.Fatalf("in WSL: %q", got)
	}
	feedbackInWSL = func() bool { return false }
	if got := feedbackOS(); got != "Linux" {
		t.Fatalf("plain Linux: %q", got)
	}
}

// ST-165: no row repeats another, and the pane's type and agent go only with
// its text.
func TestFeedbackContextNoRepeatsAndPaneKind(t *testing.T) {
	defer func(f func() string) { feedbackWindowsBuild = f }(feedbackWindowsBuild)
	feedbackWindowsBuild = func() string { return "10.0.26100.4652" }
	a := feedbackTestServer(t, "")
	codex := &Pane{ID: "pane_2", buffer: NewRingBuffer(1 << 10), Kind: "native", Agent: agentCodex}
	shell := &Pane{ID: "pane_3", buffer: NewRingBuffer(1 << 10), Cmd: []string{"bash", "-l"}}
	claude := &Pane{ID: "pane_4", buffer: NewRingBuffer(1 << 10), Cmd: []string{"claude", "--resume"}}
	muse := &Pane{ID: "pane_5", buffer: NewRingBuffer(1 << 10), Agent: agentMuse}
	for _, p := range []*Pane{codex, shell, claude, muse} {
		a.mgr.panes[p.ID] = p
	}

	off := postFeedbackPreview(t, a, feedbackReq{Type: "bug", Body: "x", PaneID: "pane_2", IncludeSnapshot: boolPtr(false), IncludeLogs: boolPtr(false)})
	c := off.Context
	if c.AppVersion != "" || c.Platform != "" {
		t.Fatalf("repeated rows still sent: app %q platform %q", c.AppVersion, c.Platform)
	}
	if c.DaemonVersion != "0.0.1241" || c.ShellPlatform != "windows" || c.WindowsBuild != "10.0.26100.4652" {
		t.Fatalf("context = %+v", c)
	}
	if c.PaneID != "" || c.PaneKind != "" || c.PaneAgent != "" {
		t.Fatalf("pane described without its text: %+v", c)
	}
	raw, _ := json.Marshal(c)
	for _, key := range []string{`"paneKind"`, `"paneAgent"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("%s sent without pane text: %s", key, raw)
		}
	}

	for id, want := range map[string][2]string{
		"pane_2": {"Focus", "Codex"},
		"pane_3": {"Terminal", ""},
		"pane_4": {"Terminal", "Claude Code"},
		"pane_5": {"Terminal", "Other"},
		"gone":   {"", ""},
	} {
		on := postFeedbackPreview(t, a, feedbackReq{Type: "bug", Body: "x", PaneID: id, IncludeSnapshot: boolPtr(true), IncludeLogs: boolPtr(false)})
		if got := [2]string{on.Context.PaneKind, on.Context.PaneAgent}; got != want {
			t.Errorf("%s: kind/agent = %q, want %q", id, got, want)
		}
	}
}
