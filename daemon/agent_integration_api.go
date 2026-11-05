package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// HTTP surface for the agent-integration choice (agent_integration.go).
// Changing it is LOCAL-ONLY, like the privacy settings: it edits files in the
// user's home directory, so it is never accepted over the cloud relay. The
// Dual-mode router mirrors POSTs to the Windows backend (router.go) so both
// backends follow one choice.

const agentIntegrationMaxRequestBody = 1 << 10

type agentIntegrationChange struct {
	What string `json:"what"`
	Path string `json:"path"`
}

type agentIntegrationResponse struct {
	State           string                   `json:"state"`
	Source          string                   `json:"source,omitempty"`
	Effective       string                   `json:"effective"`
	NeedsAnswer     bool                     `json:"needsAnswer"`
	NoticePending   bool                     `json:"noticePending"`
	OperatorDefault bool                     `json:"operatorDefault"`
	Changes         []agentIntegrationChange `json:"changes"`
	Cleanup         []agentCleanupResult     `json:"cleanup,omitempty"`
}

// agentIntegrationChanges lists exactly what "on" writes, for the Settings
// "What this changes" detail.
func agentIntegrationChanges() []agentIntegrationChange {
	home, _ := os.UserHomeDir()
	changes := []agentIntegrationChange{
		{What: "Claude Code: SoluTerminal's tools (MCP server \"soluterminal\")", Path: filepath.Join(home, ".claude.json")},
		{What: "Claude Code: hooks that tell SoluTerminal which session a pane runs", Path: filepath.Join(home, ".claude", "settings.json")},
	}
	if cfgPath, err := codexConfigPath(); err == nil {
		changes = append(changes,
			agentIntegrationChange{What: "Codex: SoluTerminal's tools (MCP server \"soluterminal\")", Path: cfgPath},
			agentIntegrationChange{What: "Codex: a short marked note in your global instructions", Path: filepath.Join(filepath.Dir(cfgPath), "AGENTS.md")},
		)
	}
	return changes
}

func agentIntegrationSnapshot(rec agentIntegrationRecord) agentIntegrationResponse {
	return agentIntegrationResponse{
		State:           rec.State,
		Source:          rec.Source,
		Effective:       rec.effective(),
		NeedsAnswer:     rec.needsAnswer(),
		NoticePending:   rec.NoticePending,
		OperatorDefault: rec.OperatorDefault,
		Changes:         agentIntegrationChanges(),
	}
}

func (a *apiServer) agentIntegrationStore() *agentIntegrationStore {
	return activeAgentIntegration.Load()
}

func (a *apiServer) handleGetAgentIntegration(w http.ResponseWriter, _ *http.Request) {
	s := a.agentIntegrationStore()
	if s == nil {
		http.Error(w, "agent integration settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, agentIntegrationSnapshot(s.load()))
}

// handleSetAgentIntegration turns the integration on (writing the four
// entries now) or off (removing them now, with backups in the data dir).
func (a *apiServer) handleSetAgentIntegration(w http.ResponseWriter, r *http.Request) {
	if requestViaRelay(r) {
		http.Error(w, "this setting can only be changed on the computer itself", http.StatusForbidden)
		return
	}
	s := a.agentIntegrationStore()
	if s == nil {
		http.Error(w, "agent integration settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, agentIntegrationMaxRequestBody)).Decode(&body); err != nil ||
		(body.State != agentIntegrationOn && body.State != agentIntegrationOff) {
		http.Error(w, `body must set "state" to "on" or "off"`, http.StatusBadRequest)
		return
	}
	rec, err := s.update(func(rec *agentIntegrationRecord) {
		rec.State = body.State
		rec.Source = agentIntegrationSourceUser
		rec.DecidedAtMs = time.Now().UnixMilli()
		rec.NoticePending = false
	})
	if err != nil {
		http.Error(w, "could not save the setting", http.StatusInternalServerError)
		return
	}
	resp := agentIntegrationSnapshot(rec)
	if body.State == agentIntegrationOn {
		applyAgentIntegrationOn(a.addr)
	} else {
		resp.Cleanup = removeAgentIntegrationFor(agentIntegrationBackupDir(a.dataDir), agentIntegrationOwner{
			MCPURL:  claudeMcpDesiredURL(a.addr),
			HookURL: claudeHookURL(a.addr),
			// A bridge entry has no URL; it names this daemon's socket.
			MCPSocket: filepath.Join(a.dataDir, mcpSocketDirName, mcpSocketName),
			GOOS:      runtime.GOOS,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *apiServer) handleAckAgentIntegrationNotice(w http.ResponseWriter, r *http.Request) {
	if requestViaRelay(r) {
		http.Error(w, "this setting can only be changed on the computer itself", http.StatusForbidden)
		return
	}
	s := a.agentIntegrationStore()
	if s == nil {
		http.Error(w, "agent integration settings are unavailable", http.StatusServiceUnavailable)
		return
	}
	rec, err := s.update(func(rec *agentIntegrationRecord) { rec.NoticePending = false })
	if err != nil {
		http.Error(w, "could not save the setting", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, agentIntegrationSnapshot(rec))
}

// applyAgentIntegrationOn writes the four entries now instead of waiting for
// the next restart. Each writer is idempotent and serialized internally.
var applyAgentIntegrationOn = func(addr string) {
	ensureClaudeMcpRegistration(addr)
	ensureClaudeHooksRegistered(addr)
	ensureCodexMcpRegistration(addr)
}
