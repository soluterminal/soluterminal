package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMuseRejectsOrchestratorRole(t *testing.T) {
	p := switchTestPane("muse_role", "", []string{"muse"}, "")
	p.Agent = agentMuse
	api, spawnOpts, killed := resumeTestServer(t, p)
	req := httptest.NewRequest(http.MethodPost, "/api/panes/muse_role/role", strings.NewReader(`{"role":"orchestrator"}`))
	req.SetPathValue("id", p.ID)
	rec := httptest.NewRecorder()
	api.handleSetRole(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Muse") {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(*spawnOpts) != 0 || len(*killed) != 0 {
		t.Fatal("Muse role request mutated pane")
	}
}
