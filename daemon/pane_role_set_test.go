package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func postSetRole(api *apiServer, id, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/panes/"+id+"/role", strings.NewReader(body))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	api.handleSetRole(rec, req)
	return rec
}

func TestSetPaneRoleValidationAndGuards(t *testing.T) {
	t.Run("invalid role", func(t *testing.T) {
		p := switchTestPane("role_bad", "", []string{"claude"}, "")
		api, spawnOpts, killed := resumeTestServer(t, p)
		rec := postSetRole(api, p.ID, `{"role":"bogus"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown role: bogus") {
			t.Fatalf("response = %d %q, want 400 unknown role", rec.Code, rec.Body.String())
		}
		if len(*spawnOpts) != 0 || len(*killed) != 0 {
			t.Fatal("invalid role must not respawn")
		}
	})

	t.Run("unknown pane", func(t *testing.T) {
		p := switchTestPane("role_other", "", []string{"claude"}, "")
		api, spawnOpts, _ := resumeTestServer(t, p)
		rec := postSetRole(api, "missing", `{"role":"orchestrator"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if len(*spawnOpts) != 0 {
			t.Fatal("unknown pane must not respawn")
		}
	})

	t.Run("explicit unknown agent is unsupported", func(t *testing.T) {
		p := switchTestPane("role_unknown", "", []string{"gemini"}, "")
		p.Agent = "gemini"
		api, spawnOpts, killed := resumeTestServer(t, p)
		rec := postSetRole(api, p.ID, `{"role":"orchestrator"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not available") {
			t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
		}
		if len(*spawnOpts) != 0 || len(*killed) != 0 {
			t.Fatal("unknown-agent role request respawned")
		}
	})

	t.Run("same role is a no-op", func(t *testing.T) {
		p := switchTestPane("role_same", "", []string{"claude"}, "session")
		p.Role = paneRoleOrchestrator
		api, spawnOpts, killed := resumeTestServer(t, p)
		rec := postSetRole(api, p.ID, `{"role":"orchestrator"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		if len(*spawnOpts) != 0 || len(*killed) != 0 {
			t.Fatal("same-role request must not respawn")
		}
	})

	t.Run("same role with dirty identity is still a no-op", func(t *testing.T) {
		p := switchTestPane("role_same_dirty", "", []string{"claude"}, "")
		p.Role, p.Agent = paneRoleOrchestrator, agentCodex
		api, spawnOpts, killed := resumeTestServer(t, p)
		rec := postSetRole(api, p.ID, `{"role":"orchestrator"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		if len(*spawnOpts) != 0 || len(*killed) != 0 {
			t.Fatal("same-role dirty-identity request must not respawn")
		}
	})

	t.Run("busy", func(t *testing.T) {
		p := switchTestPane("role_busy", "", []string{"claude"}, "session")
		markPaneWorking(p)
		api, spawnOpts, killed := resumeTestServer(t, p)
		rec := postSetRole(api, p.ID, `{"role":"orchestrator"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "agent is busy") {
			t.Fatalf("response = %d %q, want 409 busy", rec.Code, rec.Body.String())
		}
		if len(*spawnOpts) != 0 || len(*killed) != 0 {
			t.Fatal("busy pane must not respawn")
		}
	})

	t.Run("exited", func(t *testing.T) {
		p := switchTestPane("role_exited", "", []string{"claude"}, "session")
		p.Status = PaneExited
		api, spawnOpts, killed := resumeTestServer(t, p)
		rec := postSetRole(api, p.ID, `{"role":"orchestrator"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "use resume") {
			t.Fatalf("response = %d %q, want 409 exited", rec.Code, rec.Body.String())
		}
		if len(*spawnOpts) != 0 || len(*killed) != 0 {
			t.Fatal("exited pane must not respawn")
		}
	})

	t.Run("respawn already in progress", func(t *testing.T) {
		p := switchTestPane("role_locked", "", []string{"claude"}, "")
		p.respawning.Store(true)
		api, spawnOpts, killed := resumeTestServer(t, p)
		rec := postSetRole(api, p.ID, `{"role":"orchestrator"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "respawn already in progress") {
			t.Fatalf("response = %d %q, want 409 in progress", rec.Code, rec.Body.String())
		}
		if len(*spawnOpts) != 0 || len(*killed) != 0 {
			t.Fatal("locked pane must not respawn")
		}
	})

	t.Run("session without transcript", func(t *testing.T) {
		p := switchTestPane("role_no_tx", "", []string{"claude"}, "missing-session")
		api, spawnOpts, killed := resumeTestServer(t, p)
		rec := postSetRole(api, p.ID, `{"role":"orchestrator"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no on-disk transcript") {
			t.Fatalf("response = %d %q, want 409 transcript", rec.Code, rec.Body.String())
		}
		if len(*spawnOpts) != 0 || len(*killed) != 0 {
			t.Fatal("unresumable pane must not respawn")
		}
	})

	t.Run("kill switch", func(t *testing.T) {
		t.Setenv("SOLU_PANE_ROLE", "0")
		p := switchTestPane("role_disabled", "", []string{"claude"}, "")
		api, spawnOpts, killed := resumeTestServer(t, p)
		rec := postSetRole(api, p.ID, `{"role":"orchestrator"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "SOLU_PANE_ROLE") {
			t.Fatalf("response = %d %q, want 400 disabled", rec.Code, rec.Body.String())
		}
		if len(*spawnOpts) != 0 || len(*killed) != 0 {
			t.Fatal("disabled endpoint must not respawn")
		}
	})
}

func TestSetPaneRoleRespawnRecipes(t *testing.T) {
	tests := []struct {
		name        string
		pane        func(*testing.T) *Pane
		body        string
		wantKind    string
		wantCmd     []string
		wantRole    string
		wantSession string
	}{
		{
			name: "terminal conversation resumes as orchestrator",
			pane: func(t *testing.T) *Pane {
				p := switchTestPane("role_resume", "", []string{"claude"}, "sess-resume")
				p.ClaudeTranscriptPath = writeTempTranscript(t)
				return p
			},
			body:    `{"role":"orchestrator","cols":132,"rows":41}`,
			wantCmd: []string{"claude", "--resume", "sess-resume"}, wantRole: paneRoleOrchestrator, wantSession: "sess-resume",
		},
		{
			name: "session-less claude starts fresh",
			pane: func(*testing.T) *Pane {
				return switchTestPane("role_fresh", "", []string{"claude"}, "")
			},
			body:    `{"role":"orchestrator"}`,
			wantCmd: []string{"claude"}, wantRole: paneRoleOrchestrator,
		},
		{
			name: "shell starts fresh claude",
			pane: func(*testing.T) *Pane {
				return switchTestPane("role_shell", "", []string{"bash"}, "")
			},
			body:    `{"role":"orchestrator"}`,
			wantCmd: []string{"claude"}, wantRole: paneRoleOrchestrator,
		},
		{
			name: "orchestrator converts back",
			pane: func(t *testing.T) *Pane {
				p := switchTestPane("role_remove", "", []string{"claude"}, "sess-plain")
				p.ClaudeTranscriptPath = writeTempTranscript(t)
				p.Role = paneRoleOrchestrator
				return p
			},
			body:    `{"role":""}`,
			wantCmd: []string{"claude", "--resume", "sess-plain"}, wantSession: "sess-plain",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.pane(t)
			api, spawnOpts, killed := resumeTestServer(t, p)
			rec := postSetRole(api, p.ID, tt.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			if len(*spawnOpts) != 1 || len(*killed) != 1 || (*killed)[0] != p.ID {
				t.Fatalf("spawns=%d killed=%v, want one each", len(*spawnOpts), *killed)
			}
			o := (*spawnOpts)[0]
			if o.Kind != tt.wantKind || o.Role != tt.wantRole || o.ClaudeSessionID != tt.wantSession {
				t.Fatalf("opts = %+v, want kind=%q role=%q session=%q", o, tt.wantKind, tt.wantRole, tt.wantSession)
			}
			if strings.Join(o.Cmd, "\x00") != strings.Join(tt.wantCmd, "\x00") {
				t.Fatalf("cmd = %#v, want %#v", o.Cmd, tt.wantCmd)
			}
			if tt.name == "terminal conversation resumes as orchestrator" && (o.Cols != 132 || o.Rows != 41 || o.DimsUnverified) {
				t.Fatalf("live dimensions not carried: cols=%d rows=%d unverified=%v", o.Cols, o.Rows, o.DimsUnverified)
			}
			var dto paneDTO
			if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if dto.Role != tt.wantRole {
				t.Fatalf("response role = %q, want %q", dto.Role, tt.wantRole)
			}
			if tt.name == "terminal conversation resumes as orchestrator" {
				converted, ok := api.mgr.Get(p.ID)
				if !ok {
					t.Fatal("converted pane missing from manager")
				}
				convertedSnap := converted.Snapshot()
				if got := persistedFromSnapshot(convertedSnap).Role; got != paneRoleOrchestrator {
					t.Fatalf("converted role not persisted: %q", got)
				}
				if got := resumeSpawnOpts(convertedSnap, 0, 0).Role; got != paneRoleOrchestrator {
					t.Fatalf("converted role not carried through resume: %q", got)
				}
			}
		})
	}
}

func TestSetPaneRoleCodexRecipes(t *testing.T) {
	const thread = "019f5697-154c-7a92-8b61-ad2f9b68476d"
	codeHome := t.TempDir()
	t.Setenv("CODEX_HOME", codeHome)
	rolloutDir := filepath.Join(codeHome, "sessions", "2026", "08", "24")
	if err := os.MkdirAll(rolloutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rolloutDir, "rollout-2026-08-24T00-00-00-"+thread+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		pane     func() *Pane
		role     string
		wantKind string
		wantCmd  []string
		wantID   string
	}{
		{name: "terminal resume", pane: func() *Pane {
			p := switchTestPane("codex_role_terminal", "", []string{"codex"}, "")
			p.Agent, p.CodexThreadID = agentCodex, thread
			return p
		}, role: paneRoleOrchestrator, wantCmd: []string{"codex", "resume", thread}, wantID: thread},
		{name: "fresh terminal", pane: func() *Pane {
			p := switchTestPane("codex_role_fresh", "", []string{"codex"}, "")
			p.Agent = agentCodex
			return p
		}, role: paneRoleOrchestrator, wantCmd: []string{"codex"}},
		{name: "remove role", pane: func() *Pane {
			p := switchTestPane("codex_role_remove", "", []string{"codex"}, "")
			p.Agent, p.CodexThreadID, p.Role = agentCodex, thread, paneRoleOrchestrator
			return p
		}, wantCmd: []string{"codex", "resume", thread}, wantID: thread},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.pane()
			api, spawns, _ := resumeTestServer(t, p)
			api.orchestratorHostProbe = func(agent string) orchestratorHostAvailability {
				return orchestratorHostAvailability{Agent: agent, Label: "Codex", Supported: true, Installed: true, Available: true}
			}
			rec := postSetRole(api, p.ID, `{"role":"`+tt.role+`"}`)
			if rec.Code != http.StatusOK || len(*spawns) != 1 {
				t.Fatalf("status=%d body=%q spawns=%d", rec.Code, rec.Body.String(), len(*spawns))
			}
			o := (*spawns)[0]
			if o.Agent != agentCodex || o.Kind != tt.wantKind || o.Role != tt.role || o.CodexThreadID != tt.wantID || !slices.Equal(o.Cmd, tt.wantCmd) {
				t.Fatalf("spawn opts = %+v want kind=%q role=%q cmd=%#v id=%q", o, tt.wantKind, tt.role, tt.wantCmd, tt.wantID)
			}
		})
	}

	t.Run("missing rollout is refused without respawn", func(t *testing.T) {
		const missingThread = "129f5697-154c-7a92-8b61-ad2f9b68476d"
		p := switchTestPane("codex_role_missing_rollout", "", []string{"codex"}, "")
		p.Agent, p.CodexThreadID = agentCodex, missingThread
		api, spawns, killed := resumeTestServer(t, p)
		api.orchestratorHostProbe = func(agent string) orchestratorHostAvailability {
			return orchestratorHostAvailability{Agent: agent, Label: "Codex", Supported: true, Installed: true, Available: true}
		}
		rec := postSetRole(api, p.ID, `{"role":"orchestrator"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no on-disk Codex rollout") {
			t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
		}
		if len(*spawns) != 0 || len(*killed) != 0 {
			t.Fatal("unresumable Codex role change mutated the pane")
		}
	})

	t.Run("uncorroborated Codex claim follows the Claude command", func(t *testing.T) {
		p := switchTestPane("role_uncorroborated", "", []string{"claude"}, "")
		p.Agent = agentCodex
		api, spawns, _ := resumeTestServer(t, p)
		rec := postSetRole(api, p.ID, `{"role":"orchestrator"}`)
		if rec.Code != http.StatusOK || len(*spawns) != 1 {
			t.Fatalf("status=%d body=%q spawns=%d", rec.Code, rec.Body.String(), len(*spawns))
		}
		got := (*spawns)[0]
		if got.Agent != "" || got.CodexThreadID != "" || got.Role != paneRoleOrchestrator || !slices.Equal(got.Cmd, []string{"claude"}) {
			t.Fatalf("healed spawn opts = %+v", got)
		}
	})

	t.Run("corroborated Codex identity inside a Claude-spawned wrapper resumes Codex", func(t *testing.T) {
		p := switchTestPane("role_corroborated", "", []string{"bash", "-lc", "init; 'claude' '--append-system-prompt' 'old'; exec bash -i"}, "claude-session")
		p.Agent, p.CodexThreadID = agentCodex, thread
		api, spawns, _ := resumeTestServer(t, p)
		api.orchestratorHostProbe = func(agent string) orchestratorHostAvailability {
			return orchestratorHostAvailability{Agent: agent, Label: "Codex", Supported: true, Installed: true, Available: true}
		}
		rec := postSetRole(api, p.ID, `{"role":"orchestrator"}`)
		if rec.Code != http.StatusOK || len(*spawns) != 1 {
			t.Fatalf("status=%d body=%q spawns=%d", rec.Code, rec.Body.String(), len(*spawns))
		}
		got := (*spawns)[0]
		if got.Agent != agentCodex || got.CodexThreadID != thread || got.ClaudeSessionID != "claude-session" || got.Role != paneRoleOrchestrator || !slices.Equal(got.Cmd, []string{"codex", "resume", thread}) {
			t.Fatalf("corroborated spawn opts = %+v", got)
		}
	})

	t.Run("explicit Codex identity discovered in a shell can convert", func(t *testing.T) {
		p := switchTestPane("codex_role_discovered", "", []string{"bash"}, "")
		p.Agent = agentCodex
		api, spawns, _ := resumeTestServer(t, p)
		api.orchestratorHostProbe = func(agent string) orchestratorHostAvailability {
			return orchestratorHostAvailability{Agent: agent, Label: "Codex", Supported: true, Installed: true, Available: true}
		}
		rec := postSetRole(api, p.ID, `{"role":"orchestrator"}`)
		if rec.Code != http.StatusOK || len(*spawns) != 1 {
			t.Fatalf("status=%d body=%q spawns=%d", rec.Code, rec.Body.String(), len(*spawns))
		}
		if got := (*spawns)[0]; got.Agent != agentCodex || !slices.Equal(got.Cmd, []string{"codex"}) {
			t.Fatalf("discovered Codex spawn opts = %+v", got)
		}
	})
}
