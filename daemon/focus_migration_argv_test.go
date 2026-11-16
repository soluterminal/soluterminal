package main

import (
	"runtime"
	"strings"
	"testing"
)

// The headless `claude -p … stream-json` argv only Focus panes ever ran. Saved
// panes from older releases still carry it, and focus_migration.go recognizes
// it (isHeadlessClaudeCmd) to reopen them as terminal panes; resumeSessionIDFromCmd
// recovers the conversation id baked into it.
func legacyFocusClaudeArgv(sessionID string) []string {
	argv := []string{
		"claude", "-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--permission-mode", "default",
		"--permission-prompt-tool", "stdio",
		"--include-partial-messages",
		"--replay-user-messages",
		"--verbose",
	}
	if sessionID != "" {
		argv = append(argv, "--resume", sessionID)
	}
	return argv
}

func TestIsHeadlessClaudeCmd(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cmd  []string
		want bool
	}{
		{"bare native argv", legacyFocusClaudeArgv(""), true},
		{"native argv with resume", legacyFocusClaudeArgv("sess-123"), true},
		{"terminal resume TUI", []string{"claude", "--resume", "abc"}, false},
		{"bare claude", []string{"claude"}, false},
		{"plain shell", []string{"/usr/bin/bash"}, false},
		{"empty", nil, false},
		{"unrelated cmd that merely mentions stream-json", []string{"echo", "stream-json"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHeadlessClaudeCmd(tc.cmd); got != tc.want {
				t.Fatalf("isHeadlessClaudeCmd(%v) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// The real on-disk corruption stores the BASH-WRAPPED native argv (claude isn't
// on the daemon's slim PATH, so tryLoginShellWrap wraps it). isHeadlessClaudeCmd
// must see through the wrap, else the Focus migration would miss it.
func TestIsHeadlessClaudeCmdDetectsBashWrappedNative(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("login shell wrapping is Unix-only")
	}
	wrapped, ok := tryLoginShellWrap(legacyFocusClaudeArgv("sess-xyz"), "", nil)
	if !ok {
		t.Skip("bash not available to wrap (tryLoginShellWrap returned false)")
	}
	if !isHeadlessClaudeCmd(wrapped) {
		t.Fatalf("isHeadlessClaudeCmd did not see through the bash-wrapped native argv: %#v", wrapped)
	}
}

// A native headless agent must be exec'd (so the agent exits when claude exits →
// pane shows exited/resumable), NOT handed the interactive-claude `; exec bash -i`
// fallback (which would leave a zombie shell the native parser can't read — and
// is what made the broken pane present a usable bash prompt).
func TestTryLoginShellWrapNativeUsesPlainExec(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("login shell wrapping is Unix-only")
	}
	wrapped, ok := tryLoginShellWrap(legacyFocusClaudeArgv("sess-1"), "", nil)
	if !ok {
		t.Skip("bash not available to wrap")
	}
	script := wrapped[2]
	if strings.HasSuffix(script, " -i") || strings.Contains(script, "; exec "+wrapped[0]+" -i") {
		t.Fatalf("native wrap script = %q, must NOT drop back to an interactive shell", script)
	}
	if !strings.Contains(script, "; exec 'claude' '-p'") {
		t.Fatalf("native wrap script = %q, want a plain `exec claude -p …`", script)
	}
}

// resumeSessionIDFromCmd recovers a pane's session id from the baked-in
// `--resume <id>` when the persisted ClaudeSessionID field was cleared — covering
// both the bare argv and the bash-wrapped single-quoted form on disk.
func TestResumeSessionIDFromCmd(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cmd  []string
		want string
	}{
		{"bare argv", []string{"claude", "-p", "--resume", "abc-123"}, "abc-123"},
		{"native bare argv", legacyFocusClaudeArgv("sess-xyz"), "sess-xyz"},
		{"no resume", legacyFocusClaudeArgv(""), ""},
		{"plain shell", []string{"bash"}, ""},
		{"empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumeSessionIDFromCmd(tc.cmd); got != tc.want {
				t.Fatalf("resumeSessionIDFromCmd(%v) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

func TestResumeSessionIDFromCmdBashWrapped(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("login shell wrapping is Unix-only")
	}
	wrapped, ok := tryLoginShellWrap(legacyFocusClaudeArgv("sess-wrapped-99"), "", nil)
	if !ok {
		t.Skip("bash not available to wrap")
	}
	if got := resumeSessionIDFromCmd(wrapped); got != "sess-wrapped-99" {
		t.Fatalf("resumeSessionIDFromCmd(wrapped) = %q, want sess-wrapped-99", got)
	}
}
