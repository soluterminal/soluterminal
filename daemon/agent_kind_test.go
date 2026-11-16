package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeAgentKindDefaultsAndInfersCodex(t *testing.T) {
	const thread = "019f5697-154c-7a92-8b61-ad2f9b68476d"
	tests := []struct {
		name     string
		raw      string
		cmd      []string
		threadID string
		want     string
		ok       bool
	}{
		{name: "legacy Claude", cmd: []string{"claude"}, want: "", ok: true},
		{name: "explicit Claude stays zero value", raw: "claude", cmd: []string{"claude"}, want: "", ok: true},
		{name: "direct Codex inferred", cmd: []string{"/usr/local/bin/codex"}, want: agentCodex, ok: true},
		{name: "Windows Codex inferred", cmd: []string{`C:\\Tools\\codex.exe`}, want: agentCodex, ok: true},
		{name: "explicit Codex", raw: "codex", cmd: []string{"codex"}, want: agentCodex, ok: true},
		{name: "unknown rejected", raw: "gemini", cmd: []string{"gemini"}, want: "", ok: false},
		// Field-loss heal: a durable Codex thread id with an omitted agent can
		// only mean the agent field was lost (every thread-id writer stamps
		// Agent=codex, and Claude adoption clears the id). The real persisted
		// shape wraps the CLI in bash -lc, so cmd sniffing alone cannot catch it.
		{name: "lost agent healed from thread id", cmd: []string{"/usr/bin/bash", "-lc", "… exec 'codex' 'resume' '" + thread + "'"}, threadID: thread, want: agentCodex, ok: true},
		{name: "explicit Claude beats thread id", raw: "claude", cmd: []string{"claude"}, threadID: thread, want: "", ok: true},
		{name: "garbage thread id does not flip Claude", cmd: []string{"claude"}, threadID: "not-a-thread", want: "", ok: true},
		{name: "direct Muse inferred", cmd: []string{"/home/u/.local/bin/muse"}, want: agentMuse, ok: true},
		{name: "Windows Muse inferred", cmd: []string{`C:\\Tools\\muse.exe`}, want: agentMuse, ok: true},
		{name: "explicit Muse", raw: "muse", cmd: []string{"muse"}, want: agentMuse, ok: true},
		{name: "muse resume argv inferred", cmd: []string{"muse", "resume", "11111111-2222-4333-8444-555555555555"}, want: agentMuse, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeAgentKind(tt.raw, tt.cmd, tt.threadID)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("normalizeAgentKind(%q, %v, %q) = (%q, %v), want (%q, %v)", tt.raw, tt.cmd, tt.threadID, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestNormalizeAgentKindThreadIDHealYieldsToUnambiguousCommand(t *testing.T) {
	const thread = "019f5697-154c-7a92-8b61-ad2f9b68476d"
	for _, cmd := range [][]string{{"claude"}, {"bash", "-lc", "exec 'claude'"}} {
		if got, ok := normalizeAgentKind("", cmd, thread); !ok || got != "" {
			t.Fatalf("normalize(%#v) = %q, %v; want Claude", cmd, got, ok)
		}
	}
	if got, _ := normalizeAgentKind("", []string{"bash"}, thread); got != agentCodex {
		t.Fatalf("shell + durable thread = %q, want codex", got)
	}
}

func TestHealPaneIdentity(t *testing.T) {
	const thread = "019f5697-154c-7a92-8b61-ad2f9b68476d"
	// (a) The legacy zero-value Claude encoding remains canonical and quiet.
	legacy, legacyNotes := healPaneIdentity(paneIdentity{Cmd: []string{"claude"}, Role: paneRoleOrchestrator})
	if legacy.Agent != "" || legacy.Role != paneRoleOrchestrator || len(legacyNotes) != 0 {
		t.Fatalf("legacy Claude orchestrator changed: %+v notes=%v", legacy, legacyNotes)
	}

	// (b) Foreground discovery can stamp Codex after a Claude-spawned wrapper
	// was recorded. Its canonical thread corroborates the newer identity.
	out, notes := healPaneIdentity(paneIdentity{
		Agent: agentCodex, Role: paneRoleOrchestrator,
		Cmd:           []string{"bash", "-lc", "init; 'claude' '--append-system-prompt' 'old'; exec bash -i"},
		CodexThreadID: thread, ClaudeSessionID: "claude-session",
	})
	if out.Agent != agentCodex || out.Role != paneRoleOrchestrator || out.CodexThreadID != thread || out.ClaudeSessionID != "claude-session" || len(notes) != 0 {
		t.Fatalf("corroborated observed pane = %+v notes=%v", out, notes)
	}

	// (c) Without a Codex conversation of its own, the Claude command wins.
	out, notes = healPaneIdentity(paneIdentity{
		Agent: agentCodex, Role: paneRoleOrchestrator, Cmd: []string{"claude"},
		ClaudeSessionID: "s",
	})
	if out.Agent != "" || out.Role != paneRoleOrchestrator || out.ClaudeSessionID != "s" || !strings.Contains(strings.Join(notes, " "), "command wins") {
		t.Fatalf("uncorroborated Codex claim = %+v notes=%v", out, notes)
	}

	// (d) Inference never discards another provider's resumable pointer.
	out, notes = healPaneIdentity(paneIdentity{Cmd: []string{"codex"}, Role: paneRoleOrchestrator, ClaudeSessionID: "stale"})
	if out.Agent != agentCodex || out.ClaudeSessionID != "stale" || out.Role != paneRoleOrchestrator {
		t.Fatalf("Codex command heal = %+v", out)
	}
	if !strings.Contains(strings.Join(notes, " "), "inferred") {
		t.Fatalf("inferred Codex identity is not marked dirty: %v", notes)
	}
	out, notes = healPaneIdentity(paneIdentity{Agent: agentMuse, Role: paneRoleOrchestrator})
	if out.Role != "" || len(notes) == 0 {
		t.Fatalf("Muse role was not cleared: %+v notes=%v", out, notes)
	}
	// (f) A canonical Codex conversation corroborates an explicit identity even
	// when the stored command is only a shell.
	out, notes = healPaneIdentity(paneIdentity{Agent: agentCodex, Cmd: []string{"bash"}, CodexThreadID: thread})
	if out.Agent != agentCodex || out.CodexThreadID != thread || len(notes) != 0 {
		t.Fatalf("shell-discovered Codex changed: %+v notes=%v", out, notes)
	}
	// (g) Unknown identities are preserved for an honest API error, while role
	// claims are removed and all conversation pointers survive.
	out, notes = healPaneIdentity(paneIdentity{Agent: "gemini", Role: paneRoleOrchestrator, ClaudeSessionID: "x"})
	if out.Agent != "gemini" || out.Role != "" || out.ClaudeSessionID != "x" || len(notes) == 0 {
		t.Fatalf("unknown identity was silently treated as Claude: %+v notes=%v", out, notes)
	}
	// (h) Codex turn state is only meaningful for a Codex identity.
	out, notes = healPaneIdentity(paneIdentity{Cmd: []string{"claude"}, CodexTurnInFlight: true})
	if out.CodexTurnInFlight || len(notes) == 0 {
		t.Fatalf("stale Codex in-flight state was not made persistably dirty: %+v notes=%v", out, notes)
	}
	// (i) Explicit Claude is normalized to the one legacy zero-value encoding.
	out, notes = healPaneIdentity(paneIdentity{Agent: agentClaude, Cmd: []string{"claude"}})
	if out.Agent != "" || len(notes) != 0 {
		t.Fatalf("explicit Claude encoding = %+v notes=%v", out, notes)
	}
	// (j) Native metadata obeys the same uncorroborated-command rule.
	out, notes = healPaneIdentity(paneIdentity{
		Kind: "native", Agent: agentCodex, Cmd: []string{"claude", "-p", "--output-format", "stream-json"},
		ClaudeSessionID: "session",
	})
	if out.Agent != "" || out.ClaudeSessionID != "session" || !strings.Contains(strings.Join(notes, " "), "command wins") {
		t.Fatalf("native uncorroborated Codex claim = %+v notes=%v", out, notes)
	}
}

func TestForegroundClaudeRetiresOtherProviderResumeRecipe(t *testing.T) {
	for _, priorAgent := range []string{agentCodex, agentMuse} {
		p := &Pane{Agent: priorAgent, Role: paneRoleOrchestrator, Cmd: []string{"bash", "-lc", "'" + priorAgent + "' 'resume' 'old'; exec bash -i"}, CodexThreadID: "old-codex", ClaudeSessionID: "old-claude", ClaudeTranscriptPath: "/old.jsonl"}
		if !p.SetForegroundClaudeIdentity() {
			t.Fatal("handoff was not recorded")
		}
		snap, _ := healPaneSnapshot(p.Snapshot())
		if snap.Agent != "" || snap.ClaudeSessionID != "" || snap.CodexThreadID != "" || agentFromCmd(snap.Cmd) != agentClaude {
			t.Fatalf("stale provider resurrected: %+v", snap)
		}
		p.ClaudeSessionID = "current-claude"
		if p.SetForegroundClaudeIdentity() {
			t.Fatal("repeat discovery cleared current Claude identity")
		}
		opts := resumeSpawnOpts(p.Snapshot(), 0, 0)
		if opts.Agent != "" || !slices.Equal(opts.Cmd, []string{"claude", "--resume", "current-claude"}) {
			t.Fatalf("wrong resume recipe: %+v", opts)
		}
	}
	// Repair a pane already handed to Claude by an older daemon without
	// discarding the authoritative Claude hook's current conversation.
	p := &Pane{Cmd: []string{"codex"}, ClaudeSessionID: "current", ClaudeTranscriptPath: "/current.jsonl"}
	if !p.SetForegroundClaudeIdentity() || p.ClaudeSessionID != "current" || p.ClaudeTranscriptPath != "/current.jsonl" {
		t.Fatal("recipe-only repair lost current identity")
	}
}

func TestLifecycleSpawnRecipesHealIdentityBeforeChoosingProvider(t *testing.T) {
	const thread = "019f5697-154c-7a92-8b61-ad2f9b68476d"
	corroborated := PaneSnapshot{
		ID: "pane_incoherent", Agent: agentCodex, Role: paneRoleOrchestrator,
		Cmd: []string{"claude"}, ClaudeSessionID: "claude-session", CodexThreadID: thread,
	}
	resumed := resumeSpawnOpts(corroborated, 0, 0)
	if resumed.Agent != agentCodex || resumed.CodexThreadID != thread || resumed.ClaudeSessionID != "claude-session" ||
		!slices.Equal(resumed.Cmd, []string{"codex", "resume", thread}) {
		t.Fatalf("corroborated resume recipe chose the wrong provider: %+v", resumed)
	}

	uncorroborated := corroborated
	uncorroborated.CodexThreadID = ""
	resumed = resumeSpawnOpts(uncorroborated, 0, 0)
	if resumed.Agent != "" || resumed.CodexThreadID != "" || resumed.ClaudeSessionID != "claude-session" ||
		!slices.Equal(resumed.Cmd, []string{"claude", "--resume", "claude-session"}) {
		t.Fatalf("uncorroborated resume recipe did not follow the command: %+v", resumed)
	}
}

func TestConversationIDForAgentNeverCrossesProviders(t *testing.T) {
	if got := conversationIDForAgent("", "claude-session", "codex-thread", "muse-session"); got != "claude-session" {
		t.Fatalf("legacy Claude conversation id = %q", got)
	}
	if got := conversationIDForAgent(agentCodex, "claude-session", "codex-thread", "muse-session"); got != "codex-thread" {
		t.Fatalf("Codex conversation id = %q", got)
	}
	if got := conversationIDForAgent(agentCodex, "claude-session", "", "muse-session"); got != "" {
		t.Fatalf("Codex fell back to another provider's id: %q", got)
	}
	if got := conversationIDForAgent(agentMuse, "claude-session", "codex-thread", "muse-session"); got != "muse-session" {
		t.Fatalf("Muse conversation id = %q", got)
	}
	if got := conversationIDForAgent(agentMuse, "claude-session", "codex-thread", ""); got != "" {
		t.Fatalf("Muse fell back to another provider's id: %q", got)
	}
}

func TestAgentKindPersistsAndLegacyClaudeStaysOmitted(t *testing.T) {
	pp := persistedFromSnapshot(PaneSnapshot{ID: "p-codex", Agent: agentCodex})
	if pp.Agent != agentCodex {
		t.Fatalf("persisted agent = %q, want codex", pp.Agent)
	}
	b, err := json.Marshal(pp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"agent":"codex"`) {
		t.Fatalf("Codex persisted JSON lost agent: %s", b)
	}

	legacy, err := json.Marshal(persistedFromSnapshot(PaneSnapshot{ID: "p-claude"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacy), `"agent"`) {
		t.Fatalf("legacy Claude JSON must keep agent omitted: %s", legacy)
	}
	explicitClaude, _ := healPaneSnapshot(PaneSnapshot{ID: "p-explicit-claude", Agent: agentClaude, Cmd: []string{"claude"}})
	explicitBytes, err := json.Marshal(persistedFromSnapshot(explicitClaude))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(explicitBytes), `"agent"`) {
		t.Fatalf("explicit Claude JSON must use the canonical omitted encoding: %s", explicitBytes)
	}

	workspaceBytes, err := json.Marshal(PaneDef{ID: "p", Agent: agentCodex})
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip PaneDef
	if err := json.Unmarshal(workspaceBytes, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.Agent != agentCodex {
		t.Fatalf("workspace PaneDef agent = %q after round trip", roundTrip.Agent)
	}
}

func TestCreatePaneRejectsContradictoryAgentRequestsBeforeSpawn(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "contradictory explicit Claude", body: `{"agent":"claude","cmd":["codex"]}`},
		{name: "contradictory explicit Codex", body: `{"agent":"codex","cmd":["claude"]}`},
		{name: "unknown agent", body: `{"agent":"gemini","cmd":["gemini"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spawned := false
			api := &apiServer{spawnPane: func(SpawnOpts) (*Pane, error) {
				spawned = true
				return transactionTestPane("must-not-spawn"), nil
			}}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/panes", strings.NewReader(tt.body))
			api.createPane(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
			}
			if spawned {
				t.Fatal("spawn called for rejected agent request")
			}
		})
	}
}

func TestCreatePaneInfersTerminalCodexAndDefaultsClaude(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantAgent string
	}{
		{name: "terminal Codex", body: `{"cmd":["codex"]}`, wantAgent: agentCodex},
		{name: "legacy Claude", body: `{"cmd":["claude"]}`, wantAgent: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got SpawnOpts
			api := &apiServer{spawnPane: func(opts SpawnOpts) (*Pane, error) {
				got = opts
				p := transactionTestPane("created")
				p.Agent = opts.Agent
				p.Kind = opts.Kind
				return p, nil
			}}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/panes", strings.NewReader(tt.body))
			api.createPane(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body=%q", rec.Code, rec.Body.String())
			}
			if got.Agent != tt.wantAgent {
				t.Fatalf("spawn Agent = %q, want %q", got.Agent, tt.wantAgent)
			}
		})
	}
}
