package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// claudeTranscriptOnDisk must report on-disk existence via a cheap stat — true
// when the transcript exists (so `claude --resume` would find a conversation),
// false for a brand-new/phantom id whose transcript was never written. The
// resumeReady badge gates on this per pane per poll, so it must stay O(1);
// callers that would destroy or refuse something on a "no" use
// claudeTranscriptResumable instead.
func TestClaudeTranscriptOnDisk(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	cwd := "/home/u/proj"
	enc := claudeProjectDirName(cwd)
	projDir := filepath.Join(cfg, "projects", enc)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "11111111-2222-3333-4444-555555555555"
	tp := filepath.Join(projDir, sid+".jsonl")
	if err := os.WriteFile(tp, []byte(`{"type":"summary"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !claudeTranscriptOnDisk(cwd, sid, "") {
		t.Fatal("cwd-derived stat: existing transcript should be found")
	}
	if !claudeTranscriptOnDisk("", sid, tp) {
		t.Fatal("knownPath stat: existing transcript should be found even without cwd")
	}
	if claudeTranscriptOnDisk(cwd, "99999999-0000-0000-0000-000000000000", "") {
		t.Fatal("nonexistent id must be reported absent (→ start fresh, not doomed --resume)")
	}
	if claudeTranscriptOnDisk(cwd, "  ", "") {
		t.Fatal("blank id must be absent")
	}
	if claudeTranscriptOnDisk("", "", "/no/such/file.jsonl") {
		t.Fatal("dangling knownPath + empty id must be absent")
	}
}

// The regression that silently reset a user's ~736k-token session, reproduced
// on any host: when the real project dir is NOT named the way
// claudeProjectDirName guesses, the cwd-encoded stat misses. On Windows it
// missed EVERY time (the encoder was POSIX-only), and Spawn read that miss as
// proof of absence and erased the conversation. The id scan must find the
// transcript regardless of what the directory is called.
func TestClaudeTranscriptResumableSurvivesEncodingMismatch(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	// What Claude Code names the project dir for C:\Users\u\proj on Windows —
	// and nothing claudeProjectDirName produces on this POSIX test host, which
	// is exactly the shape of the bug.
	projDir := filepath.Join(cfg, "projects", "C--Users-u-proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !claudeTranscriptResumable(`C:\Users\u\proj`, sid, "") {
		t.Fatal("transcript is on disk but wasn't found — Spawn would erase the conversation")
	}

	// The other half of the split, pinned deliberately: the cheap stat does NOT
	// find it, and must not learn to. It runs per pane on every /api/panes poll,
	// where a directory walk is the computeState GC death-spiral. Being wrong
	// there only dims a badge; being wrong in Spawn costs a conversation.
	if claudeTranscriptOnDisk(`C:\Users\u\proj`, sid, "") {
		t.Fatal("the poll-path stat grew a directory walk — keep it O(1)")
	}
}

// "We couldn't look" must never be reported as "it isn't there" — the same
// asymmetry codexRolloutOnDisk documents. A doomed --resume that claude rejects
// is recoverable; an erased session id is not.
func TestClaudeTranscriptResumableFailsOpenWhenItCannotLook(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the tree can't be made unreadable")
	}
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	root := filepath.Join(cfg, "projects")
	if err := os.MkdirAll(filepath.Join(root, "-home-u-proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	if !claudeTranscriptResumable("/home/u/proj", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "") {
		t.Fatal("an unreadable projects tree must fail open, not abandon the conversation")
	}
}

// ...but the fail-open must not swallow the case the guard exists for: a clean
// enumeration that finds nothing still means start fresh, because a doomed
// --resume kills the pane.
func TestClaudeTranscriptResumableStillReportsGenuineAbsence(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "projects", "-home-u-proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if claudeTranscriptResumable("/home/u/proj", "99999999-0000-0000-0000-000000000000", "") {
		t.Fatal("a clean enumeration that found nothing must report absence")
	}
}

func TestClaudeProjectDirNamePerPlatform(t *testing.T) {
	if got, want := claudeProjectDirNameFor("windows", `C:\Users\u\proj`), "C--Users-u-proj"; got != want {
		t.Errorf("windows: got %q, want %q", got, want)
	}
	if got, want := claudeProjectDirNameFor("windows", `C:\Users\u\.config\app`), "C--Users-u--config-app"; got != want {
		t.Errorf("windows dotted: got %q, want %q", got, want)
	}
	if got, want := claudeProjectDirNameFor("linux", "/home/u/proj"), "-home-u-proj"; got != want {
		t.Errorf("linux: got %q, want %q", got, want)
	}
	// A POSIX path containing ':' or '\' keeps them — only Windows collapses.
	if got, want := claudeProjectDirNameFor("linux", "/home/u/a:b"), "-home-u-a:b"; got != want {
		t.Errorf("linux colon: got %q, want %q", got, want)
	}
}

// stripResumeArgv turns a doomed `claude --resume <id>` into a fresh `claude` so a
// terminal pane with an unresumable id starts a new session instead of crashing.
func TestStripResumeArgv(t *testing.T) {
	got := stripResumeArgv([]string{"claude", "--resume", "sess-abc"})
	if len(got) != 1 || got[0] != "claude" {
		t.Fatalf("got %#v, want [claude]", got)
	}
	got = stripResumeArgv([]string{"claude", "--resume", "sess-abc", "--flag"})
	if len(got) != 2 || got[0] != "claude" || got[1] != "--flag" {
		t.Fatalf("got %#v, want [claude --flag]", got)
	}
	got = stripResumeArgv([]string{"claude"})
	if len(got) != 1 || got[0] != "claude" {
		t.Fatalf("no --resume should be unchanged, got %#v", got)
	}
}

// The persisted/workspace form wraps the CLI inside one bash -lc string
// (`… 'claude' '--resume' '<id>'; exec bash -i`). stripResumeArgv must strip
// THAT form too: workspace inline defs respawn with it, and leaving the baked
// `--resume` behind launches the doomed resume the fresh-start downgrade just
// decided against — the dead-bash masquerade.
func TestStripResumeArgvWrappedForm(t *testing.T) {
	wrapped := "export PATH=\"$HOME/.local/bin:$PATH\"; true; 'claude' '--resume' '413e0bd6-7e1b-439c-8764-da01886b346d'; exec /usr/bin/bash -i"
	got := stripResumeArgv([]string{"/usr/bin/bash", "-lc", wrapped})
	if len(got) != 3 {
		t.Fatalf("shape changed: %#v", got)
	}
	want := "export PATH=\"$HOME/.local/bin:$PATH\"; true; 'claude'; exec /usr/bin/bash -i"
	if got[2] != want {
		t.Fatalf("wrapped --resume not stripped:\n got %q\nwant %q", got[2], want)
	}
	// An element with no resume marker is untouched.
	if out := stripResumeArgv([]string{"/usr/bin/bash", "-lc", "'claude'; exec bash -i"}); out[2] != "'claude'; exec bash -i" {
		t.Fatalf("clean wrapped cmd mutated: %q", out[2])
	}
	// A marker with no closing quote is left alone rather than corrupted.
	if out := stripResumeArgv([]string{"bash", "-lc", "'claude' '--resume' 'dangling"}); out[2] != "'claude' '--resume' 'dangling" {
		t.Fatalf("unterminated quote corrupted: %q", out[2])
	}
}

// resumeThreadIDFromCodexCmd must parse BOTH the bare argv and the login-shell-
// wrapped form, and reject non-canonical ids from the wrapped path (argv
// parsing stays as conservative as /proc discovery).
func TestResumeThreadIDFromCodexCmdWrappedForm(t *testing.T) {
	const thread = "019f5697-154c-7a92-8b61-ad2f9b68476d"
	if got := resumeThreadIDFromCodexCmd([]string{"codex", "resume", thread}); got != thread {
		t.Fatalf("bare form: got %q", got)
	}
	wrapped := []string{"/usr/bin/bash", "-lc", "export PATH=…; true; exec 'codex' 'resume' '" + thread + "'"}
	if got := resumeThreadIDFromCodexCmd(wrapped); got != thread {
		t.Fatalf("wrapped form: got %q", got)
	}
	junk := []string{"/usr/bin/bash", "-lc", "exec 'codex' 'resume' 'not-a-uuid'"}
	if got := resumeThreadIDFromCodexCmd(junk); got != "" {
		t.Fatalf("non-canonical wrapped id must be rejected, got %q", got)
	}
	if got := resumeThreadIDFromCodexCmd([]string{"/usr/bin/bash", "-lc", "'claude' '--resume' '" + thread + "'"}); got != "" {
		t.Fatalf("claude resume must not parse as codex thread, got %q", got)
	}
}

// The interactive Codex TUI must get the same `; exec bash -i` shell fallback
// as the Claude TUI — historically Codex was `exec`'d directly, so quitting the
// TUI killed the whole pane while Claude fell back to a shell. The headless
// `codex app-server` pipe agent keeps plain exec (and its protocol-safe
// non-login shell) — a trailing interactive bash would zombie the pane instead
// of letting it exit as resumable.
func TestLoginShellWrapCodexTUIFallsBackToShell(t *testing.T) {
	const thread = "019f5697-154c-7a92-8b61-ad2f9b68476d"
	wrapped, ok := tryLoginShellWrap([]string{"codex", "resume", thread}, "", nil)
	if !ok {
		t.Skip("bash not available to wrap (tryLoginShellWrap returned false)")
	}
	tail := wrapped[len(wrapped)-1]
	if !strings.Contains(tail, "'codex' 'resume' '"+thread+"'") {
		t.Fatalf("wrap lost the resume argv: %q", tail)
	}
	if !strings.Contains(tail, "; exec ") || !strings.HasSuffix(tail, " -i") {
		t.Fatalf("codex TUI wrap must end in the interactive shell fallback: %q", tail)
	}
	if got := resumeThreadIDFromCodexCmd(wrapped); got != thread {
		t.Fatalf("new wrap shape must stay parseable: got %q", got)
	}

	appServer, ok := tryLoginShellWrap([]string{"codex", "app-server", "--stdio"}, "", nil)
	if !ok {
		t.Skip("bash not available to wrap")
	}
	if appServer[1] != "-c" {
		t.Fatalf("app-server must keep the protocol-safe non-login shell, got %q", appServer[1])
	}
	if strings.HasSuffix(appServer[len(appServer)-1], " -i") {
		t.Fatalf("app-server must NOT get the interactive fallback: %q", appServer[len(appServer)-1])
	}
}

// runningSessionOwner backs the duplicate-resume guard: only a RUNNING pane
// with the same session id — and a different pane id — claims a session.
// Suspended/exited panes never block (resuming one of two suspended twins is
// legitimate), and a same-id respawn (resume click, native switch) is excluded
// so it can't refuse its own session.
func TestRunningSessionOwner(t *testing.T) {
	mgr := NewManager(t.TempDir())
	register := func(p *Pane) {
		mgr.mu.Lock()
		mgr.panes[p.ID] = p
		mgr.mu.Unlock()
	}
	register(&Pane{ID: "pane_live", Status: PaneRunning, ClaudeSessionID: "sess-dup"})
	register(&Pane{ID: "pane_susp", Status: PaneExited, Resumable: true, ClaudeSessionID: "sess-parked"})
	register(&Pane{ID: "codex_live", Status: PaneRunning, Agent: agentCodex, CodexThreadID: "thread-dup"})

	if got := mgr.runningSessionOwner(agentClaude, "sess-dup", ""); got != "pane_live" {
		t.Fatalf("running owner: got %q, want pane_live", got)
	}
	if got := mgr.runningSessionOwner(agentClaude, "sess-dup", "pane_live"); got != "" {
		t.Fatalf("same-pane respawn must be excluded, got %q", got)
	}
	if got := mgr.runningSessionOwner(agentClaude, "sess-parked", ""); got != "" {
		t.Fatalf("a suspended pane must not claim its session, got %q", got)
	}
	if got := mgr.runningSessionOwner(agentClaude, "sess-unknown", ""); got != "" {
		t.Fatalf("unknown session: got %q, want \"\"", got)
	}
	if got := mgr.runningSessionOwner(agentClaude, "  ", ""); got != "" {
		t.Fatalf("blank session id must never match, got %q", got)
	}
	if got := mgr.runningSessionOwner(agentCodex, "thread-dup", ""); got != "codex_live" {
		t.Fatalf("Codex thread owner: got %q, want codex_live", got)
	}
	if got := mgr.runningSessionOwner(agentClaude, "thread-dup", ""); got != "" {
		t.Fatalf("Claude lookup crossed into Codex thread ownership: %q", got)
	}
}

// spawnResumeID picks the provider-specific continuity id from the
// corresponding terminal resume argv.
func TestSpawnResumeID(t *testing.T) {
	if got := spawnResumeID(SpawnOpts{Kind: "", Cmd: []string{"claude", "--resume", "sess-t"}}); got != "sess-t" {
		t.Fatalf("terminal: got %q, want sess-t", got)
	}
	if got := spawnResumeID(SpawnOpts{Kind: "", Cmd: []string{"bash"}}); got != "" {
		t.Fatalf("terminal no-resume: got %q, want \"\"", got)
	}
	if got := spawnResumeID(SpawnOpts{Agent: agentCodex, Cmd: []string{"codex", "resume", "thread-t"}}); got != "thread-t" {
		t.Fatalf("terminal Codex: got %q, want thread-t", got)
	}
}

func TestClearSpawnResumeCodexStartsFreshWithoutTouchingClaudeState(t *testing.T) {
	opts := SpawnOpts{
		Agent: agentCodex, Cmd: []string{"codex", "resume", "thread-1"},
		CodexThreadID: "thread-1", ClaudeSessionID: "unrelated-legacy-value",
	}
	clearSpawnResume(&opts)
	if opts.CodexThreadID != "" || len(opts.Cmd) != 1 || opts.Cmd[0] != "codex" {
		t.Fatalf("Codex resume not cleared safely: %+v", opts)
	}
	if opts.ClaudeSessionID != "unrelated-legacy-value" {
		t.Fatalf("Codex clear mutated Claude state: %q", opts.ClaudeSessionID)
	}
}
