package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreatePaneGoldenResponses(t *testing.T) {
	pack := makeContextPack(t, "AGENTS.md")
	tests := []struct {
		name           string
		body           string
		mgr            *Manager
		emptyWorkspace bool
		wantStatus     int
		wantBody       string
		wantBodyPrefix string
		wantSpawn      bool
		wantPack       string
		oversized      bool
		wantCmd        []string
	}{
		{name: "malformed JSON", body: `{oops`, wantStatus: http.StatusBadRequest, wantBody: "invalid json\n"},
		{name: "empty body", body: ``, wantStatus: http.StatusBadRequest, wantBody: "invalid json\n"},
		{name: "trailing data remains lenient", body: `{}garbage`, wantStatus: http.StatusCreated, wantSpawn: true},
		{name: "unknown field remains lenient", body: `{"unknown":true}`, wantStatus: http.StatusCreated, wantSpawn: true},
		{name: "workspace store unavailable", body: `{"workspace":"x"}`, wantStatus: http.StatusInternalServerError, wantBody: "workspace store not initialized\n"},
		{name: "workspace not found", body: `{"workspace":"x"}`, emptyWorkspace: true, wantStatus: http.StatusNotFound, wantBody: "workspace not found: x\n"},
		{name: "unknown kind", body: `{"kind":"weird"}`, wantStatus: http.StatusBadRequest, wantBody: "unknown pane kind: weird\n"},
		{name: "new Focus pane refused", body: `{"kind":"native"}`, wantStatus: http.StatusBadRequest, wantBody: "Focus view was removed: open a terminal pane instead\n"},
		{name: "saved Focus pane restores as a terminal", body: `{"kind":"native","restore":true,"cmd":["claude"],"claudeSessionId":"sess-1"}`, wantStatus: http.StatusCreated, wantSpawn: true, wantCmd: []string{"claude", "--resume", "sess-1"}},
		{name: "saved Focus pane without a conversation asks for it", body: `{"kind":"native","restore":true,"cmd":["claude"]}`, wantStatus: http.StatusBadRequest, wantBody: "saved conversation ID is missing; choose the original conversation\n"},
		{name: "unknown agent", body: `{"agent":"gemini"}`, wantStatus: http.StatusBadRequest, wantBody: "unknown agent: gemini\n"},
		{name: "Claude agent with Codex command", body: `{"agent":"claude","cmd":["codex"]}`, wantStatus: http.StatusBadRequest, wantBody: "agent is claude but command starts Codex\n"},
		{name: "Claude agent with Muse command", body: `{"agent":"claude","cmd":["muse"]}`, wantStatus: http.StatusBadRequest, wantBody: "agent is claude but command starts Muse\n"},
		{name: "Codex agent without Codex command", body: `{"agent":"codex","cmd":["claude"]}`, wantStatus: http.StatusBadRequest, wantBody: "agent is codex but command does not start Codex\n"},
		{name: "Muse agent without Muse command", body: `{"agent":"muse","cmd":["claude"]}`, wantStatus: http.StatusBadRequest, wantBody: "agent is muse but command does not start Muse\n"},
		{name: "unknown provider", body: `{"provider":"aws"}`, wantStatus: http.StatusBadRequest, wantBody: "unknown provider: aws\n"},
		{name: "provider on Codex pane", body: `{"agent":"codex","cmd":["codex"],"provider":"glm"}`, wantStatus: http.StatusBadRequest, wantBody: "provider is only supported for Claude panes\n"},
		{name: "GLM provider without key", body: `{"provider":"glm"}`, mgr: &Manager{}, wantStatus: http.StatusBadRequest, wantBody: "GLM provider selected but no Z.ai key configured — set ZAI_API_KEY in ~/.config/glm/env\n"},
		{name: "unknown effort", body: `{"effort":"turbo"}`, wantStatus: http.StatusBadRequest, wantBody: "unknown effort: turbo\n"},
		{name: "invalid model", body: `{"model":"model with spaces"}`, wantStatus: http.StatusBadRequest, wantBody: "invalid model: model with spaces\n"},
		{name: "missing context pack", body: `{"cmd":["claude"],"contextPack":"/does/not/exist"}`, wantStatus: http.StatusBadRequest, wantBodyPrefix: "invalid context pack:"},
		{name: "valid context pack", body: `{"cmd":["claude"],"contextPack":"` + pack + `"}`, wantStatus: http.StatusCreated, wantSpawn: true, wantPack: pack},
		{name: "context pack opt-out", body: `{"cmd":["claude"],"contextPack":"none"}`, wantStatus: http.StatusCreated, wantSpawn: true},
		{name: "success", body: `{}`, wantStatus: http.StatusCreated, wantSpawn: true},
		// This pins the helper's new direct-handler cap. Before the refactor, only
		// the global 32MB middleware bounded createPane request bodies.
		{name: "oversized body", wantStatus: http.StatusBadRequest, wantBody: "invalid json\n", oversized: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := tt.body
			if tt.oversized {
				body = `{"title":"` + strings.Repeat("x", 1<<20) + `"}`
			}
			var ws *WorkspaceStore
			if tt.emptyWorkspace {
				var err error
				ws, err = NewWorkspaceStore(t.TempDir())
				if err != nil {
					t.Fatalf("NewWorkspaceStore: %v", err)
				}
			}
			spawned := false
			var spawnOpts SpawnOpts
			api := &apiServer{
				mgr: tt.mgr,
				ws:  ws,
				spawnPane: func(opts SpawnOpts) (*Pane, error) {
					spawned = true
					spawnOpts = opts
					return transactionTestPane("golden-created"), nil
				},
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/panes", strings.NewReader(body))

			api.createPane(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if spawned != tt.wantSpawn {
				t.Fatalf("spawned = %v, want %v", spawned, tt.wantSpawn)
			}
			if tt.wantStatus == http.StatusCreated {
				if tt.wantCmd != nil {
					if spawnOpts.Kind != "" || strings.Join(spawnOpts.Cmd, " ") != strings.Join(tt.wantCmd, " ") {
						t.Fatalf("spawn kind=%q cmd=%q, want terminal %q", spawnOpts.Kind, spawnOpts.Cmd, tt.wantCmd)
					}
				}
				if spawnOpts.ContextPack != tt.wantPack {
					t.Fatalf("spawn context pack = %q, want %q", spawnOpts.ContextPack, tt.wantPack)
				}
				if got := rec.Header().Get("Content-Type"); got != "application/json" {
					t.Fatalf("Content-Type = %q, want application/json", got)
				}
				var pane paneDTO
				if err := json.Unmarshal(rec.Body.Bytes(), &pane); err != nil {
					t.Fatalf("success body is not a pane DTO: %v; body=%q", err, rec.Body.String())
				}
				if pane.ID != "golden-created" {
					t.Fatalf("pane ID = %q, want golden-created", pane.ID)
				}
				return
			}
			if got := rec.Body.String(); tt.wantBodyPrefix != "" && !strings.HasPrefix(got, tt.wantBodyPrefix) {
				t.Fatalf("body = %q, want prefix %q", got, tt.wantBodyPrefix)
			} else if tt.wantBodyPrefix == "" && got != tt.wantBody {
				t.Fatalf("body = %q, want %q", got, tt.wantBody)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Fatalf("Content-Type = %q, want text/plain; charset=utf-8", got)
			}
		})
	}
}
