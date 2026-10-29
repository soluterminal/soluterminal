package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// countFlag returns how many times flag appears as its own argv element.
func countFlag(argv []string, flag string) int {
	n := 0
	for _, el := range argv {
		if el == flag {
			n++
		}
	}
	return n
}

// appendedSystemPrompt returns the single --append-system-prompt value in argv.
func appendedSystemPrompt(t *testing.T, argv []string) string {
	t.Helper()
	if got := countFlag(argv, "--append-system-prompt"); got != 1 {
		t.Fatalf("--append-system-prompt count = %d, want 1 (argv=%v)", got, argv)
	}
	for i, el := range argv {
		if el == "--append-system-prompt" {
			if i+1 >= len(argv) {
				t.Fatalf("--append-system-prompt has no value (argv=%v)", argv)
			}
			return argv[i+1]
		}
	}
	return ""
}

// TestOrchestratorBriefTextConstraints pins the invariants the spawn plumbing
// depends on: the marker prefix (stale-copy replacement), no ASCII apostrophe
// (it rides a single-quoted bash -lc string), no headless-argv trio, no
// wrapped-resume marker, and the delegation vocabulary the role exists for.
func TestOrchestratorBriefTextConstraints(t *testing.T) {
	if !strings.HasPrefix(orchestratorBrief, orchestratorBriefMarker) {
		t.Fatalf("brief must start with orchestratorBriefMarker %q", orchestratorBriefMarker)
	}
	if strings.ContainsRune(orchestratorBrief, '\'') {
		t.Fatal("brief contains an ASCII single quote — use the typographic ’ so the bash -lc wrap stays clean")
	}
	if isHeadlessClaudeCmd([]string{"claude", "--append-system-prompt", orchestratorBrief}) {
		t.Fatal("brief text makes a terminal claude argv look like the headless stream-json invocation")
	}
	if strings.Contains(orchestratorBrief, "'--resume' '") {
		t.Fatal("brief contains the wrapped --resume marker resumeSessionIDFromCmd scans for")
	}
	for _, phrase := range []string{"Work directly", "ask_pane", "wait_for_pane", "read_pane_tail", "create_schedule", "set_pane_flag", "scripts/solu-review", "at most two review rounds", "The user decides what merges"} {
		if !strings.Contains(orchestratorBrief, phrase) {
			t.Fatalf("brief never mentions %q", phrase)
		}
	}
	for _, stale := range []string{"add_todo", "update_todo", "todo list", "Use create_workflow_task", "Do not investigate before filing", "pipeline’s scout", "agent_resume_task", "depends_on"} {
		if strings.Contains(orchestratorBrief, stale) {
			t.Fatalf("retired delegation policy remains: %q", stale)
		}
	}
	// The pre-router brief was 2,445 bytes; keep this revision within ~1.43x so
	// the role remains a brief rather than becoming another instruction set.
	// Growth since the router revision is limited to the session-start digest
	// vocabulary and the shared completion-handoff contract. Raise this only
	// for the same kind of cross-transport product contract.
	if got, max := len(orchestratorBrief), 4000; got > max {
		t.Fatalf("brief is %d bytes, want at most %d", got, max)
	}
}

func TestWithOrchestratorBriefTerminalArgv(t *testing.T) {
	t.Run("bare claude gets the brief", func(t *testing.T) {
		got := withOrchestratorBrief([]string{"claude"})
		want := []string{"claude", "--append-system-prompt", orchestratorBrief}
		if !slices.Equal(got, want) {
			t.Fatalf("argv = %v, want %v", got, want)
		}
	})

	t.Run("resume flags stay ahead of the brief", func(t *testing.T) {
		got := withOrchestratorBrief([]string{"claude", "--resume", "s1"})
		want := []string{"claude", "--resume", "s1", "--append-system-prompt", orchestratorBrief}
		if !slices.Equal(got, want) {
			t.Fatalf("argv = %v, want %v", got, want)
		}
		if sid := resumeSessionIDFromCmd(got); sid != "s1" {
			t.Fatalf("resumeSessionIDFromCmd = %q, want s1", sid)
		}
		stripped := stripResumeArgv(got)
		if slices.Contains(stripped, "--resume") || slices.Contains(stripped, "s1") {
			t.Fatalf("stripResumeArgv left the resume behind: %v", stripped)
		}
		if appendedSystemPrompt(t, stripped) != orchestratorBrief {
			t.Fatal("stripResumeArgv dropped or mangled the brief")
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		once := withOrchestratorBrief([]string{"claude"})
		twice := withOrchestratorBrief(once)
		if !slices.Equal(once, twice) {
			t.Fatalf("second application changed the argv: %v vs %v", once, twice)
		}
		if got := countFlag(twice, "--append-system-prompt"); got != 1 {
			t.Fatalf("--append-system-prompt count = %d, want 1", got)
		}
	})

	t.Run("stale brief is replaced", func(t *testing.T) {
		stale := orchestratorBriefMarker + " (old)"
		got := withOrchestratorBrief([]string{"claude", "--append-system-prompt", stale})
		if appendedSystemPrompt(t, got) != orchestratorBrief {
			t.Fatalf("stale brief was not replaced: %v", got)
		}
	})

	// The stale pair can sit anywhere in the argv — a respawn rebuilds the flags
	// in its own order — so replacement must be position-independent and must not
	// disturb the flags around it.
	t.Run("stale brief between other flags is replaced in place", func(t *testing.T) {
		stale := orchestratorBriefMarker + " (old)"
		got := withOrchestratorBrief([]string{
			"claude", "--model", "foo", "--append-system-prompt", stale, "--resume", "s3",
		})
		if appendedSystemPrompt(t, got) != orchestratorBrief {
			t.Fatalf("interleaved stale brief was not replaced: %v", got)
		}
		if slices.Contains(got, stale) {
			t.Fatalf("stale brief value survived: %v", got)
		}
		want := []string{"claude", "--model", "foo", "--resume", "s3", "--append-system-prompt", orchestratorBrief}
		if !slices.Equal(got, want) {
			t.Fatalf("argv = %v, want %v", got, want)
		}
		if sid := resumeSessionIDFromCmd(got); sid != "s3" {
			t.Fatalf("resumeSessionIDFromCmd = %q, want s3", sid)
		}
	})

	t.Run("user system prompt is preserved", func(t *testing.T) {
		got := withOrchestratorBrief([]string{"claude", "--append-system-prompt", "be brief"})
		if !slices.Contains(got, "be brief") {
			t.Fatalf("user --append-system-prompt was dropped: %v", got)
		}
		if !slices.Contains(got, orchestratorBrief) {
			t.Fatalf("orchestrator brief was not added: %v", got)
		}
		if n := countFlag(got, "--append-system-prompt"); n != 2 {
			t.Fatalf("--append-system-prompt count = %d, want 2 (user + orchestrator)", n)
		}
	})

	t.Run("absolute claude path is decorated", func(t *testing.T) {
		got := withOrchestratorBrief([]string{"/home/u/.local/bin/claude"})
		if appendedSystemPrompt(t, got) != orchestratorBrief {
			t.Fatalf("absolute claude was not decorated: %v", got)
		}
	})

	t.Run("wrapped and non-claude commands are untouched", func(t *testing.T) {
		wrapped := []string{"bash", "-lc", "init; 'claude' '--resume' 'x'; exec bash -i"}
		if got := withOrchestratorBrief(wrapped); !slices.Equal(got, wrapped) {
			t.Fatalf("wrapped cmd changed: %v", got)
		}
		codex := []string{"codex"}
		if got := withOrchestratorBrief(codex); !slices.Equal(got, codex) {
			t.Fatalf("codex cmd changed: %v", got)
		}
	})
}

func TestWithOrchestratorBriefSurvivesLoginShellWrap(t *testing.T) {
	wrapped, ok := tryLoginShellWrap(withOrchestratorBrief([]string{"claude", "--resume", "s2"}), "", nil)
	if !ok {
		t.Skip("no bash on PATH — login-shell wrap unavailable")
	}
	joined := strings.Join(wrapped, " ")
	if !strings.Contains(joined, "'--append-system-prompt' '") {
		t.Fatalf("wrapped cmd lost the system-prompt flag: %q", joined)
	}
	if !strings.Contains(joined, orchestratorBriefMarker) {
		t.Fatalf("wrapped cmd lost the brief: %q", joined)
	}
	if !strings.HasSuffix(joined, " -i") {
		t.Fatalf("wrapped cmd lost its interactive `exec bash -i` tail: %q", joined)
	}
	if isHeadlessClaudeCmd(wrapped) {
		t.Fatalf("wrapped terminal claude reads as the headless invocation: %q", joined)
	}
	if sid := resumeSessionIDFromCmd(wrapped); sid != "s2" {
		t.Fatalf("resumeSessionIDFromCmd = %q, want s2", sid)
	}
}

func TestNativeClaudeArgvRoleConcatenatesNarrationAndBrief(t *testing.T) {
	both := nativeClaudeArgvRole("sess", true, "", false, "", paneRoleOrchestrator)
	value := appendedSystemPrompt(t, both)
	if !strings.HasPrefix(value, nativeNarrationPrompt) {
		t.Fatal("concatenated prompt does not lead with the narration prompt")
	}
	if idx := strings.Index(value, orchestratorBriefMarker); idx <= 0 {
		t.Fatalf("orchestrator brief missing or not after the narration prompt (idx=%d)", idx)
	}
	if !strings.HasSuffix(value, orchestratorBrief) {
		t.Fatal("concatenated prompt does not end with the orchestrator brief")
	}

	narrateOnly := nativeClaudeArgvRole("sess", true, "", false, "", "")
	if got := appendedSystemPrompt(t, narrateOnly); got != nativeNarrationPrompt {
		t.Fatal("role-less native argv should carry exactly the narration prompt")
	}
	roleOnly := nativeClaudeArgvRole("sess", false, "", false, "", paneRoleOrchestrator)
	if got := appendedSystemPrompt(t, roleOnly); got != orchestratorBrief {
		t.Fatal("narration-less orchestrator argv should carry exactly the brief")
	}
	plain := nativeClaudeArgvRole("sess", false, "", false, "", "")
	if n := countFlag(plain, "--append-system-prompt"); n != 0 {
		t.Fatalf("plain native argv carries %d --append-system-prompt flags, want 0", n)
	}
	if got, want := nativeClaudeArgv("sess", true, "high", true, "claude-fable-5"),
		nativeClaudeArgvRole("sess", true, "high", true, "claude-fable-5", ""); !slices.Equal(got, want) {
		t.Fatal("nativeClaudeArgv wrapper diverged from the role-less nativeClaudeArgvRole")
	}
	for name, argv := range map[string][]string{
		"both": both, "narrate": narrateOnly, "role": roleOnly, "plain": plain,
	} {
		if !isHeadlessClaudeCmd(argv) {
			t.Fatalf("%s native argv no longer reads as headless", name)
		}
	}
}

func TestSystemPromptAppend(t *testing.T) {
	tests := []struct {
		name    string
		narrate bool
		role    string
		want    string
	}{
		{name: "neither", want: ""},
		{name: "narration only", narrate: true, want: nativeNarrationPrompt},
		{name: "role only", role: paneRoleOrchestrator, want: orchestratorBrief},
		{name: "both", narrate: true, role: paneRoleOrchestrator, want: nativeNarrationPrompt + "\n\n" + orchestratorBrief},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := systemPromptAppend(tt.narrate, tt.role); got != tt.want {
				t.Fatalf("systemPromptAppend(%v, %q) length = %d, want %d", tt.narrate, tt.role, len(got), len(tt.want))
			}
		})
	}
}

// TestSpawnCmdForRole pins the kind/role→argv decision Manager.Spawn delegates
// to. The decoration itself is covered above; what this adds is that the WIRING
// discriminates correctly — native argvs (already decorated by
// nativeClaudeArgvRole) and role-less panes must come back untouched, or a Focus
// orchestrator would carry the brief twice.
func TestSpawnCmdForRole(t *testing.T) {
	nativeArgv := nativeClaudeArgvRole("s", true, "", false, "", paneRoleOrchestrator)
	tests := []struct {
		name string
		kind string
		role string
		cmd  []string
		want []string
	}{
		{name: "terminal orchestrator is decorated", role: paneRoleOrchestrator,
			cmd:  []string{"claude"},
			want: []string{"claude", "--append-system-prompt", orchestratorBrief}},
		{name: "terminal orchestrator keeps resume ahead of the brief", role: paneRoleOrchestrator,
			cmd:  []string{"claude", "--resume", "s"},
			want: []string{"claude", "--resume", "s", "--append-system-prompt", orchestratorBrief}},
		{name: "non-claude terminal cmd is untouched", role: paneRoleOrchestrator,
			cmd: []string{"bash"}, want: []string{"bash"}},
		{name: "role-less claude is untouched",
			cmd: []string{"claude"}, want: []string{"claude"}},
		{name: "native argv is left to nativeClaudeArgvRole", kind: "native", role: paneRoleOrchestrator,
			cmd: nativeArgv, want: nativeArgv},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := spawnCmdForRole(tt.kind, tt.role, agentClaude, tt.cmd)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("spawnCmdForRole(%q, %q, %v) = %v, want %v", tt.kind, tt.role, tt.cmd, got, tt.want)
			}
		})
	}
	// The native pass-through must not have doubled the flag.
	if n := countFlag(spawnCmdForRole("native", paneRoleOrchestrator, agentClaude, nativeArgv), "--append-system-prompt"); n != 1 {
		t.Fatalf("native argv carries %d --append-system-prompt flags after spawnCmdForRole, want 1", n)
	}
}

// TestNativeClaudeSpawnArgvCarriesRole pins the OTHER half of the Spawn wiring:
// the native branch builds its argv from opts, so the pane's Role has to reach
// the brief without any other opts field being disturbed.
func TestNativeClaudeSpawnArgvCarriesRole(t *testing.T) {
	opts := SpawnOpts{ClaudeSessionID: "s", Effort: "high", Model: "claude-fable-5", Role: paneRoleOrchestrator}
	argv := nativeClaudeSpawnArgv(opts, true, false)
	if got, want := appendedSystemPrompt(t, argv), nativeNarrationPrompt+"\n\n"+orchestratorBrief; got != want {
		t.Fatalf("appended system prompt length = %d, want %d (narration + brief)", len(got), len(want))
	}
	for _, pair := range [][2]string{{"--effort", "high"}, {"--model", "claude-fable-5"}, {"--resume", "s"}} {
		i := slices.Index(argv, pair[0])
		if i < 0 || i+1 >= len(argv) || argv[i+1] != pair[1] {
			t.Fatalf("argv lost %s %s: %v", pair[0], pair[1], argv)
		}
	}
	if !isHeadlessClaudeCmd(argv) {
		t.Fatalf("orchestrator native argv no longer reads as headless: %v", argv)
	}

	plain := opts
	plain.Role = ""
	if got, want := nativeClaudeSpawnArgv(plain, true, false),
		nativeClaudeArgv("s", true, "high", false, "claude-fable-5"); !slices.Equal(got, want) {
		t.Fatalf("role-less nativeClaudeSpawnArgv diverged from nativeClaudeArgv:\n got=%v\nwant=%v", got, want)
	}
}

func TestPaneRoleRoundTripsPersistRestartResume(t *testing.T) {
	if !validPaneRole("") || !validPaneRole(paneRoleOrchestrator) || validPaneRole("conductor") {
		t.Fatal("validPaneRole accepts exactly \"\" and orchestrator")
	}

	entry := persistedFromSnapshot(PaneSnapshot{ID: "p1", Role: paneRoleOrchestrator})
	if entry.Role != paneRoleOrchestrator {
		t.Fatalf("persistedFromSnapshot Role = %q, want orchestrator", entry.Role)
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back PersistedPane
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Role != paneRoleOrchestrator {
		t.Fatalf("round-tripped Role = %q, want orchestrator", back.Role)
	}
	var legacy PersistedPane
	if err := json.Unmarshal([]byte(`{"id":"old","cwd":"/tmp","title":"claude"}`), &legacy); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if legacy.Role != "" {
		t.Fatalf("legacy entry Role = %q, want empty", legacy.Role)
	}
	if plain, err := json.Marshal(persistedFromSnapshot(PaneSnapshot{ID: "p2"})); err != nil {
		t.Fatalf("marshal plain: %v", err)
	} else if strings.Contains(string(plain), `"role"`) {
		t.Fatalf("plain pane persisted a role key: %s", plain)
	}

	defRaw, err := json.Marshal(PaneDef{ID: "p1", Role: paneRoleOrchestrator})
	if err != nil {
		t.Fatalf("marshal def: %v", err)
	}
	var def PaneDef
	if err := json.Unmarshal(defRaw, &def); err != nil {
		t.Fatalf("unmarshal def: %v", err)
	}
	if def.Role != paneRoleOrchestrator {
		t.Fatalf("PaneDef Role = %q, want orchestrator", def.Role)
	}
	if plainDef, err := json.Marshal(PaneDef{ID: "p2"}); err != nil {
		t.Fatalf("marshal plain def: %v", err)
	} else if strings.Contains(string(plainDef), `"role"`) {
		t.Fatalf("plain PaneDef emitted a role key: %s", plainDef)
	}

	mgr := NewManager(t.TempDir())
	sp := mgr.SuspendExisting(&PersistedPane{
		ID: "pane_r", ClaudeSessionID: "sess-r", Role: paneRoleOrchestrator, Cmd: []string{"claude"},
	})
	if sp == nil {
		t.Fatal("SuspendExisting returned nil")
	}
	snap := sp.Snapshot()
	if snap.Role != paneRoleOrchestrator {
		t.Fatalf("suspended snapshot Role = %q, want orchestrator", snap.Role)
	}
	resumed := resumeSpawnOpts(snap, 0, 0)
	if resumed.Role != paneRoleOrchestrator {
		t.Fatalf("resumeSpawnOpts Role = %q, want orchestrator", resumed.Role)
	}
	if want := []string{"claude", "--resume", "sess-r"}; !slices.Equal(resumed.Cmd, want) {
		t.Fatalf("resumeSpawnOpts Cmd = %v, want the bare %v that Spawn re-decorates", resumed.Cmd, want)
	}
	if got := switchSpawnOpts(snap, "native", 0, 0).Role; got != paneRoleOrchestrator {
		t.Fatalf("switchSpawnOpts(→native) Role = %q, want orchestrator", got)
	}
	nativeSnap := snap
	nativeSnap.Kind = "native"
	if got := switchSpawnOpts(nativeSnap, "", 0, 0).Role; got != paneRoleOrchestrator {
		t.Fatalf("switchSpawnOpts(→terminal) Role = %q, want orchestrator", got)
	}

	codex := mgr.SuspendExisting(&PersistedPane{
		ID: "pane_codex_r", Agent: agentCodex, CodexThreadID: "thr-1",
		Role: paneRoleOrchestrator, Cmd: []string{"codex"},
	})
	if codex == nil {
		t.Fatal("SuspendExisting Codex returned nil")
	}
	codexSnap := codex.Snapshot()
	if opts := resumeSpawnOpts(codexSnap, 0, 0); opts.Agent != agentCodex || opts.Role != paneRoleOrchestrator || !slices.Equal(opts.Cmd, []string{"codex", "resume", "thr-1"}) {
		t.Fatalf("Codex resume opts = %+v", opts)
	}
	toNative := switchSpawnOpts(codexSnap, "native", 0, 0)
	toTerminal := switchSpawnOpts(PaneSnapshot{Agent: toNative.Agent, Role: toNative.Role, Kind: "native", CodexThreadID: toNative.CodexThreadID}, "", 0, 0)
	if toNative.Agent != agentCodex || toNative.Role != paneRoleOrchestrator || toTerminal.Agent != agentCodex || toTerminal.Role != paneRoleOrchestrator {
		t.Fatalf("Codex switch fidelity native=%+v terminal=%+v", toNative, toTerminal)
	}
}

func TestCreatePaneRoleValidationAndPassthrough(t *testing.T) {
	t.Setenv("SOLU_NATIVE_RENDERER", "1") // covers native-pane roles while Focus can still be re-enabled
	wrappedDefRestore := `{"role":"orchestrator","cmd":["/usr/bin/bash","-lc","init; 'claude' '--resume' 'x'; exec /usr/bin/bash -i"]}`
	wrappedCodexRestore := `{"role":"orchestrator","agent":"codex","cmd":["/usr/bin/bash","-lc","init; 'codex' 'resume' '019f5697-154c-7a92-8b61-ad2f9b68476d'; exec /usr/bin/bash -i"]}`
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantBody   string
		wantRole   string
		wantAgent  string
	}{
		{name: "unknown role", body: `{"role":"conductor","cmd":["claude"]}`,
			wantStatus: http.StatusBadRequest, wantBody: "unknown role: conductor\n"},
		{name: "role on a Codex pane", body: `{"role":"orchestrator","agent":"codex","cmd":["codex"]}`,
			wantStatus: http.StatusCreated, wantRole: paneRoleOrchestrator, wantAgent: agentCodex},
		// Muse and a NATIVE non-Claude agent are the paths the first review found
		// untested: each carries a cmd that starts its own agent, so the agent/cmd
		// checks upstream pass and the ROLE check is demonstrably what rejects it.
		{name: "role on a Muse pane", body: `{"role":"orchestrator","agent":"muse","cmd":["muse"]}`,
			wantStatus: http.StatusBadRequest, wantBody: "Muse Code has no MCP client"},
		{name: "role on a native Muse pane", body: `{"role":"orchestrator","kind":"native","agent":"muse","cmd":["muse"]}`,
			wantStatus: http.StatusBadRequest, wantBody: "Muse Code has no MCP client"},
		{name: "role on a native Codex pane", body: `{"role":"orchestrator","kind":"native","agent":"codex"}`,
			wantStatus: http.StatusCreated, wantRole: paneRoleOrchestrator, wantAgent: agentCodex},
		{name: "role on a shell pane", body: `{"role":"orchestrator","cmd":["bash"]}`,
			wantStatus: http.StatusBadRequest, wantBody: "orchestrator role requires a claude or codex command"},
		{name: "role without a command", body: `{"role":"orchestrator"}`,
			wantStatus: http.StatusBadRequest, wantBody: "orchestrator role requires a claude or codex command"},
		{name: "terminal claude", body: `{"role":"orchestrator","cmd":["claude"]}`,
			wantStatus: http.StatusCreated, wantRole: paneRoleOrchestrator},
		{name: "terminal claude resume", body: `{"role":"orchestrator","cmd":["claude","--resume","abc"]}`,
			wantStatus: http.StatusCreated, wantRole: paneRoleOrchestrator},
		{name: "native claude", body: `{"role":"orchestrator","kind":"native"}`,
			wantStatus: http.StatusCreated, wantRole: paneRoleOrchestrator},
		{name: "workspace def restore of a wrapped cmd", body: wrappedDefRestore,
			wantStatus: http.StatusCreated, wantRole: paneRoleOrchestrator},
		{name: "workspace def restore of a wrapped Codex cmd", body: wrappedCodexRestore,
			wantStatus: http.StatusCreated, wantRole: paneRoleOrchestrator, wantAgent: agentCodex},
		{name: "no role", body: `{"cmd":["claude"]}`, wantStatus: http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got SpawnOpts
			spawned := false
			api := &apiServer{orchestratorHostProbe: func(agent string) orchestratorHostAvailability {
				return orchestratorHostAvailability{Agent: agent, Label: orchestratorHostLabel(agent), Supported: orchestratorHostSupported(agent), Installed: true, Available: true}
			}, spawnPane: func(opts SpawnOpts) (*Pane, error) {
				got, spawned = opts, true
				p := transactionTestPane("role-created")
				p.Role = opts.Role
				return p, nil
			}}
			rec := httptest.NewRecorder()
			api.createPane(rec, httptest.NewRequest(http.MethodPost, "/api/panes", strings.NewReader(tt.body)))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus != http.StatusCreated {
				if spawned {
					t.Fatal("spawn called for a rejected role request")
				}
				if body := rec.Body.String(); !strings.Contains(body, tt.wantBody) {
					t.Fatalf("body = %q, want substring %q", body, tt.wantBody)
				}
				return
			}
			if !spawned {
				t.Fatal("spawn was not called")
			}
			if got.Role != tt.wantRole {
				t.Fatalf("SpawnOpts.Role = %q, want %q", got.Role, tt.wantRole)
			}
			if got.Agent != tt.wantAgent {
				t.Fatalf("SpawnOpts.Agent = %q, want %q", got.Agent, tt.wantAgent)
			}
			var dto paneDTO
			if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
				t.Fatalf("response is not a pane DTO: %v", err)
			}
			if dto.Role != tt.wantRole {
				t.Fatalf("pane DTO role = %q, want %q", dto.Role, tt.wantRole)
			}
			if tt.wantRole == "" && strings.Contains(rec.Body.String(), `"role"`) {
				t.Fatalf("plain pane DTO carries a role key: %s", rec.Body.String())
			}
		})
	}

	t.Run("rejects an unavailable Codex host with remediation", func(t *testing.T) {
		spawned := false
		api := &apiServer{
			orchestratorHostProbe: func(agent string) orchestratorHostAvailability {
				return orchestratorHostAvailability{
					Agent: agent, Label: orchestratorHostLabel(agent), Supported: true,
					Installed: true, Available: false, Reason: "not registered — run codex mcp add soluterminal",
				}
			},
			spawnPane: func(opts SpawnOpts) (*Pane, error) {
				spawned = true
				return transactionTestPane("should-not-spawn"), nil
			},
		}
		rec := httptest.NewRecorder()
		api.createPane(rec, httptest.NewRequest(http.MethodPost, "/api/panes", strings.NewReader(`{"role":"orchestrator","agent":"codex","cmd":["codex"]}`)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "codex mcp add soluterminal") {
			t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
		}
		if spawned {
			t.Fatal("unavailable Codex host spawned a pane")
		}
	})
}
