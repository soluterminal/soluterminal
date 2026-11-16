package main

import (
	"reflect"
	"testing"
)

const museUUID = "11111111-2222-4333-8444-555555555555"

func TestResumeSessionIDFromMuseCmd(t *testing.T) {
	t.Parallel()
	if got := resumeSessionIDFromMuseCmd([]string{"muse", "resume", museUUID}); got != museUUID {
		t.Errorf("bare argv: %q", got)
	}
	// Picker and --last carry no durable identity.
	if got := resumeSessionIDFromMuseCmd([]string{"muse", "resume"}); got != "" {
		t.Errorf("picker argv leaked an id: %q", got)
	}
	if got := resumeSessionIDFromMuseCmd([]string{"muse", "resume", "--last"}); got != "" {
		t.Errorf("--last treated as id: %q", got)
	}
	wrapped := []string{"bash", "-lc", "exec 'muse' 'resume' '" + museUUID + "'"}
	if got := resumeSessionIDFromMuseCmd(wrapped); got != museUUID {
		t.Errorf("wrapped argv: %q", got)
	}
	if got := resumeSessionIDFromMuseCmd([]string{"codex", "resume", museUUID}); got != "" {
		t.Errorf("codex argv parsed as muse: %q", got)
	}
}

func TestStripMuseResumeArgv(t *testing.T) {
	t.Parallel()
	if got := stripMuseResumeArgv([]string{"muse", "resume", museUUID}); !reflect.DeepEqual(got, []string{"muse"}) {
		t.Errorf("strip = %v", got)
	}
	keep := []string{"muse"}
	if got := stripMuseResumeArgv(keep); !reflect.DeepEqual(got, keep) {
		t.Errorf("fresh argv changed: %v", got)
	}
}

func TestSpawnResumeIDMuse(t *testing.T) {
	t.Parallel()
	if got := spawnResumeID(SpawnOpts{Agent: agentMuse, MuseSessionID: museUUID}); got != museUUID {
		t.Errorf("explicit id: %q", got)
	}
	if got := spawnResumeID(SpawnOpts{Agent: agentMuse, Cmd: []string{"muse", "resume", museUUID}}); got != museUUID {
		t.Errorf("argv id: %q", got)
	}
	// A muse spawn must never read the Claude/Codex ids.
	if got := spawnResumeID(SpawnOpts{Agent: agentMuse, ClaudeSessionID: "c", CodexThreadID: museUUID}); got != "" {
		t.Errorf("crossed providers: %q", got)
	}
}

// A dead terminal muse pane with a known session resumes as
// `muse resume <id>` — never `claude --resume` (the pre-fix conversion bug).
func TestResumeSpawnOptsMuseTerminal(t *testing.T) {
	t.Parallel()
	snap := PaneSnapshot{ID: "p1", Agent: agentMuse, MuseSessionID: museUUID}
	opts := resumeSpawnOpts(snap, 0, 0)
	want := []string{"muse", "resume", museUUID}
	if !reflect.DeepEqual(opts.Cmd, want) {
		t.Fatalf("resume argv = %v, want %v", opts.Cmd, want)
	}
	if opts.MuseSessionID != museUUID {
		t.Fatalf("MuseSessionID dropped: %q", opts.MuseSessionID)
	}
}

// The boot-reseed twin: a persisted muse pane that lost its MuseSessionID
// field but kept the resume argv gets the id back (mirrors Codex).
func TestMuseSessionIDPersistsRoundTrip(t *testing.T) {
	t.Parallel()
	pp := persistedFromSnapshot(PaneSnapshot{ID: "p-muse", Agent: agentMuse, MuseSessionID: museUUID})
	if pp.MuseSessionID != museUUID {
		t.Fatalf("persisted MuseSessionID = %q", pp.MuseSessionID)
	}
}
