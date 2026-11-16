package main

import (
	"os"
	"path/filepath"
	"testing"
)

func validCodexRolloutMeta(thread string) []byte {
	return []byte(`{"type":"session_meta","payload":{"id":"` + thread + `","session_id":"` + thread + `"}}` + "\n")
}

// codexRolloutOnDiskAt gates a Codex resume the way claudeTranscriptOnDisk
// gates a Claude one, but with deliberately asymmetric error semantics: "no"
// abandons a conversation, so "no" requires a full clean enumeration that
// found nothing; any inability to look fails OPEN (resume proceeds and the
// provider reports its own error).
func TestCodexRolloutOnDiskAt(t *testing.T) {
	t.Parallel()
	const thread = "019f5443-20e7-7e62-a771-5e2c0560171b"
	root := t.TempDir()
	day := filepath.Join(root, "2026", "07", "12")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(day, "rollout-2026-07-12T10-58-33-"+thread+".jsonl")
	if err := os.WriteFile(rollout, validCodexRolloutMeta(thread), 0o644); err != nil {
		t.Fatal(err)
	}

	if !codexRolloutOnDiskAt([]string{root}, thread) {
		t.Fatal("existing rollout should be found")
	}
	if !codexRolloutVerifiedOnDiskAt([]string{root}, thread) {
		t.Fatal("strict fleet verification should find the exact rollout")
	}
	if !codexRolloutOnDiskAt([]string{root}, "019F5443-20E7-7E62-A771-5E2C0560171B") {
		t.Fatal("thread id lookup must be case-insensitive (ids canonicalize to lowercase)")
	}
	if codexRolloutOnDiskAt([]string{root}, "019f0000-0000-7000-8000-000000000000") {
		t.Fatal("cleanly enumerated tree without the rollout must report absent (→ fresh start)")
	}

	// Fail-open cases: no enumerable root at all.
	if !codexRolloutOnDiskAt(nil, thread) {
		t.Fatal("no roots: must fail open (cannot prove absence)")
	}
	if codexRolloutVerifiedOnDiskAt(nil, thread) {
		t.Fatal("strict fleet verification must fail closed when no root is enumerable")
	}
	if !codexRolloutOnDiskAt([]string{filepath.Join(root, "no-such-dir")}, thread) {
		t.Fatal("missing sessions dir alongside no other root: must fail open")
	}
	// A missing root next to a clean empty root: the clean enumeration wins.
	empty := t.TempDir()
	if codexRolloutOnDiskAt([]string{filepath.Join(root, "no-such-dir"), empty}, "019f0000-0000-7000-8000-000000000000") {
		t.Fatal("one clean enumeration that found nothing must report absent")
	}

	// Non-canonical id: nothing a walk could ever match — fail open so the
	// provider surfaces its own error instead of a silent fresh start.
	if !codexRolloutOnDiskAt([]string{root}, "not-a-thread-id") {
		t.Fatal("non-canonical id must fail open")
	}
	if codexRolloutVerifiedOnDiskAt([]string{root}, "not-a-thread-id") {
		t.Fatal("strict fleet verification accepted a non-canonical thread id")
	}

	// Subagent rollouts still match by filename only when the id matches; a
	// different rollout in the same tree must not shadow absence detection.
	if codexRolloutOnDiskAt([]string{root}, "aaaaaaaa-bbbb-7ccc-8ddd-eeeeeeeeeeee") {
		t.Fatal("unrelated rollout must not count as a hit")
	}
}

func TestCodexRolloutVerifiedOnDiskValidatesReadableSessionMetadata(t *testing.T) {
	const thread = "019f5443-20e7-7e62-a771-5e2c0560171b"
	const other = "aaaaaaaa-bbbb-7ccc-8ddd-eeeeeeeeeeee"
	for _, tt := range []struct {
		name    string
		content []byte
		mode    os.FileMode
	}{
		{name: "truncated", content: []byte("{\n"), mode: 0o600},
		{name: "missing metadata", content: []byte("{}\n"), mode: 0o600},
		{name: "mismatched metadata", content: validCodexRolloutMeta(other), mode: 0o600},
		{name: "unreadable", content: validCodexRolloutMeta(thread), mode: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			rollout := filepath.Join(root, "rollout-2026-07-12T10-58-33-"+thread+".jsonl")
			if err := os.WriteFile(rollout, tt.content, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(rollout, tt.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(rollout, 0o600) })
			if codexRolloutVerifiedOnDiskAt([]string{root}, thread) {
				t.Fatalf("strict fleet verification accepted %s rollout", tt.name)
			}
			if !codexRolloutOnDiskAt([]string{root}, thread) {
				t.Fatalf("non-strict resume helper changed behavior for %s rollout", tt.name)
			}
		})
	}
}

// The Spawn-path integration: a Codex resume whose rollout is gone must be
// downgraded to a fresh invocation (id cleared, argv stripped) with a
// fresh-start notice, exactly like the Claude transcript guard.
func TestClearSpawnResumeCodexStripsWrappedArgv(t *testing.T) {
	t.Parallel()
	const thread = "019f5443-20e7-7e62-a771-5e2c0560171b"
	wrapped := "export PATH=\"$HOME/.local/bin:$PATH\"; true; exec 'codex' 'resume' '" + thread + "'"
	opts := SpawnOpts{
		Agent:         agentCodex,
		CodexThreadID: thread,
		Cmd:           []string{"/usr/bin/bash", "-lc", wrapped},
	}
	clearSpawnResume(&opts)
	if opts.CodexThreadID != "" {
		t.Fatalf("thread id not cleared: %q", opts.CodexThreadID)
	}
	joined := opts.Cmd[2]
	if want := "export PATH=\"$HOME/.local/bin:$PATH\"; true; exec 'codex'"; joined != want {
		t.Fatalf("wrapped resume not stripped:\n got %q\nwant %q", joined, want)
	}

	bare := SpawnOpts{Agent: agentCodex, CodexThreadID: thread, Cmd: []string{"codex", "resume", thread}}
	clearSpawnResume(&bare)
	if len(bare.Cmd) != 1 || bare.Cmd[0] != "codex" {
		t.Fatalf("bare resume not stripped: %#v", bare.Cmd)
	}
}
