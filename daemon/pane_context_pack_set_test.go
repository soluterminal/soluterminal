package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postSetContextPack(api *apiServer, id, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/panes/"+id+"/context-pack", strings.NewReader(body))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	api.handleSetContextPack(rec, req)
	return rec
}

func TestSetPaneContextPackValidationAndGuards(t *testing.T) {
	t.Run("invalid directory", func(t *testing.T) {
		p := switchTestPane("pack_invalid", "", []string{"claude"}, "")
		api, spawned, killed := resumeTestServer(t, p)
		rec := postSetContextPack(api, p.ID, `{"contextPack":"/does/not/exist"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid context pack") {
			t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
		}
		if len(*spawned) != 0 || len(*killed) != 0 {
			t.Fatal("invalid request respawned")
		}
	})

	t.Run("unknown pane", func(t *testing.T) {
		dir := makeContextPack(t, "AGENTS.md")
		p := switchTestPane("pack_other", "", []string{"claude"}, "")
		api, spawned, _ := resumeTestServer(t, p)
		rec := postSetContextPack(api, "missing", `{"contextPack":"`+dir+`"}`)
		if rec.Code != http.StatusNotFound || len(*spawned) != 0 {
			t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
		}
	})

	for _, tc := range []struct {
		name  string
		kind  string
		agent string
		cmd   []string
	}{
		{"muse", "", agentMuse, []string{"muse"}},
		{"bash", "", "", []string{"bash"}},
	} {
		t.Run(tc.name+" unsupported", func(t *testing.T) {
			dir := makeContextPack(t, "AGENTS.md")
			p := switchTestPane("pack_unsupported", tc.kind, tc.cmd, "")
			p.Agent = tc.agent
			api, spawned, killed := resumeTestServer(t, p)
			rec := postSetContextPack(api, p.ID, `{"contextPack":"`+dir+`"}`)
			if rec.Code != http.StatusConflict || len(*spawned) != 0 || len(*killed) != 0 {
				t.Fatalf("response = %d %q spawned=%d killed=%d", rec.Code, rec.Body.String(), len(*spawned), len(*killed))
			}
		})
	}

	t.Run("same value no-op", func(t *testing.T) {
		dir := makeContextPack(t, "CLAUDE.md")
		p := switchTestPane("pack_same", "", []string{"claude"}, "")
		p.ContextPack = dir
		api, spawned, killed := resumeTestServer(t, p)
		rec := postSetContextPack(api, p.ID, `{"contextPack":"`+dir+`"}`)
		if rec.Code != http.StatusOK || len(*spawned) != 0 || len(*killed) != 0 {
			t.Fatalf("response = %d %q spawned=%d killed=%d", rec.Code, rec.Body.String(), len(*spawned), len(*killed))
		}
	})

	t.Run("busy", func(t *testing.T) {
		dir := makeContextPack(t, "CLAUDE.md")
		p := switchTestPane("pack_busy", "", []string{"claude"}, "session")
		markPaneWorking(p)
		api, spawned, killed := resumeTestServer(t, p)
		rec := postSetContextPack(api, p.ID, `{"contextPack":"`+dir+`"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "agent is busy") {
			t.Fatalf("response = %d %q, want 409 busy", rec.Code, rec.Body.String())
		}
		if len(*spawned) != 0 || len(*killed) != 0 {
			t.Fatal("busy pane respawned")
		}
	})

	t.Run("exited", func(t *testing.T) {
		dir := makeContextPack(t, "CLAUDE.md")
		p := switchTestPane("pack_exited", "", []string{"claude"}, "")
		p.Status = PaneExited
		api, spawned, killed := resumeTestServer(t, p)
		rec := postSetContextPack(api, p.ID, `{"contextPack":"`+dir+`"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "use resume") || len(*spawned) != 0 || len(*killed) != 0 {
			t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("respawn already in progress", func(t *testing.T) {
		dir := makeContextPack(t, "CLAUDE.md")
		p := switchTestPane("pack_locked", "", []string{"claude"}, "")
		p.respawning.Store(true)
		api, spawned, killed := resumeTestServer(t, p)
		rec := postSetContextPack(api, p.ID, `{"contextPack":"`+dir+`"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "respawn already in progress") {
			t.Fatalf("response = %d %q, want 409 in progress", rec.Code, rec.Body.String())
		}
		if len(*spawned) != 0 || len(*killed) != 0 {
			t.Fatal("locked pane respawned")
		}
	})

	t.Run("session without transcript", func(t *testing.T) {
		dir := makeContextPack(t, "CLAUDE.md")
		p := switchTestPane("pack_no_tx", "", []string{"claude"}, "missing-session")
		api, spawned, killed := resumeTestServer(t, p)
		rec := postSetContextPack(api, p.ID, `{"contextPack":"`+dir+`"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no on-disk transcript") {
			t.Fatalf("response = %d %q, want 409 transcript", rec.Code, rec.Body.String())
		}
		if len(*spawned) != 0 || len(*killed) != 0 {
			t.Fatal("unresumable pane respawned")
		}
	})

	t.Run("kill switch", func(t *testing.T) {
		t.Setenv("SOLU_PANE_CONTEXT_PACK", "0")
		dir := makeContextPack(t, "CLAUDE.md")
		p := switchTestPane("pack_disabled", "", []string{"claude"}, "")
		api, spawned, killed := resumeTestServer(t, p)
		rec := postSetContextPack(api, p.ID, `{"contextPack":"`+dir+`"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "SOLU_PANE_CONTEXT_PACK") {
			t.Fatalf("response = %d %q, want 400 disabled", rec.Code, rec.Body.String())
		}
		if len(*spawned) != 0 || len(*killed) != 0 {
			t.Fatal("disabled endpoint respawned")
		}
	})
}

func TestSetPaneContextPackRespawnRecipes(t *testing.T) {
	pack := makeContextPack(t, "AGENTS.md")
	tests := []struct {
		name        string
		pane        func(*testing.T) *Pane
		wantKind    string
		wantAgent   string
		wantCmd     []string
		wantSession string
		wantThread  string
	}{
		{
			name: "claude conversation resumes",
			pane: func(t *testing.T) *Pane {
				p := switchTestPane("pack_resume", "", []string{"claude"}, "session")
				p.ClaudeTranscriptPath = writeTempTranscript(t)
				return p
			},
			wantCmd: []string{"claude", "--resume", "session"}, wantSession: "session",
		},
		{
			name: "sessionless claude starts fresh",
			pane: func(*testing.T) *Pane {
				return switchTestPane("pack_fresh", "", []string{"claude"}, "")
			},
			wantCmd: []string{"claude"},
		},
		{
			name: "codex thread resume kept",
			pane: func(*testing.T) *Pane {
				p := switchTestPane("pack_codex", "", []string{"codex"}, "")
				p.Agent = agentCodex
				p.CodexThreadID = "thread-1"
				return p
			},
			wantAgent: agentCodex, wantCmd: []string{"codex", "resume", "thread-1"}, wantThread: "thread-1",
		},
		{
			name: "detach starts sessionless claude fresh",
			pane: func(t *testing.T) *Pane {
				p := switchTestPane("pack_detach", "", []string{"claude"}, "")
				p.ContextPack = pack
				p.ContextPackStatus = contextPackInjected
				return p
			},
			wantCmd: []string{"claude"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.pane(t)
			api, spawned, killed := resumeTestServer(t, p)
			requestedPack := pack
			if tc.name == "detach starts sessionless claude fresh" {
				requestedPack = ""
			}
			rec := postSetContextPack(api, p.ID, `{"contextPack":"`+requestedPack+`","cols":132,"rows":41}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
			}
			if len(*spawned) != 1 || len(*killed) != 1 {
				t.Fatalf("spawned=%d killed=%d", len(*spawned), len(*killed))
			}
			o := (*spawned)[0]
			if o.ContextPack != requestedPack || o.Kind != tc.wantKind || o.Agent != tc.wantAgent || o.ClaudeSessionID != tc.wantSession || o.CodexThreadID != tc.wantThread {
				t.Fatalf("opts = %+v", o)
			}
			if strings.Join(o.Cmd, "\x00") != strings.Join(tc.wantCmd, "\x00") {
				t.Fatalf("cmd = %#v, want %#v", o.Cmd, tc.wantCmd)
			}
		})
	}
}
