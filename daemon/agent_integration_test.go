package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// isolateAgentHome points HOME (and so ~/.claude*, ~/.codex) at a temp dir,
// hides any real claude/codex CLI, and installs a fresh consent store.
func isolateAgentHome(t *testing.T) (home, dataDir string, store *agentIntegrationStore) {
	t.Helper()
	home = t.TempDir()
	dataDir = filepath.Join(home, ".solu")
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SOLU_SKIP_MCP_REGISTER", "")
	t.Setenv("SOLU_SKIP_HOOK_REGISTER", "")
	store = newAgentIntegrationStore(dataDir)
	prev := activeAgentIntegration.Load()
	activeAgentIntegration.Store(store)
	t.Cleanup(func() { activeAgentIntegration.Store(prev) })
	return home, dataDir, store
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAgentIntegrationEffectiveAndNeedsAnswer(t *testing.T) {
	cases := []struct {
		rec       agentIntegrationRecord
		effective string
		ask       bool
	}{
		{agentIntegrationRecord{}, agentIntegrationAsk, true},
		{agentIntegrationRecord{OperatorDefault: true}, agentIntegrationOn, false},
		{agentIntegrationRecord{State: "on"}, agentIntegrationOn, false},
		{agentIntegrationRecord{State: "off", Source: "user"}, agentIntegrationOff, false},
		{agentIntegrationRecord{State: "off", Source: "user", OperatorDefault: true}, agentIntegrationOff, false},
		{agentIntegrationRecord{State: "off", Source: "uninstall"}, agentIntegrationOff, true},
	}
	for _, c := range cases {
		if got := c.rec.effective(); got != c.effective {
			t.Errorf("%+v effective = %q, want %q", c.rec, got, c.effective)
		}
		if got := c.rec.needsAnswer(); got != c.ask {
			t.Errorf("%+v needsAnswer = %v, want %v", c.rec, got, c.ask)
		}
	}
}

func TestBootAgentIntegrationRecordsExistingRegistrationOnce(t *testing.T) {
	_, _, store := isolateAgentHome(t)
	legacy := true
	if got := bootAgentIntegration(store, "0.0.0.0:8787", false, true, func() bool { return legacy }); got != agentIntegrationOn {
		t.Fatalf("existing registration should keep it on, got %q", got)
	}
	rec := store.load()
	if rec.State != "on" || rec.Source != agentIntegrationSourceExisting || !rec.NoticePending {
		t.Fatalf("record: %+v", rec)
	}
	if rec.MCPURL != "http://127.0.0.1:8787/mcp" || rec.HookURL != "http://127.0.0.1:8787/api/hooks/tool-call" {
		t.Fatalf("urls: %+v", rec)
	}
	// A later boot never re-derives the choice from the files.
	legacy = false
	if got := bootAgentIntegration(store, "0.0.0.0:8787", false, true, func() bool { return legacy }); got != agentIntegrationOn {
		t.Fatalf("recorded choice must persist, got %q", got)
	}
}

func TestBootAgentIntegrationFreshInstallAsksAndOperatorDefaultsOn(t *testing.T) {
	_, _, store := isolateAgentHome(t)
	if got := bootAgentIntegration(store, "127.0.0.1:8787", false, true, func() bool { return false }); got != agentIntegrationAsk {
		t.Fatalf("fresh install should ask, got %q", got)
	}
	if agentIntegrationGlobalOn() {
		t.Fatal("nothing global may be written before the user answers")
	}
	if got := bootAgentIntegration(store, "127.0.0.1:8787", true, true, func() bool { return true }); got != agentIntegrationOn {
		t.Fatalf("operator installs default on, got %q", got)
	}
	if rec := store.load(); rec.State != "" {
		t.Fatalf("an operator default is not a recorded choice: %+v", rec)
	}
}

// A team build booting on a data dir whose cached verdict says not operator
// (a copied ~/.solu, an old backup) must not drop the operator default, nor
// record the existing registration as a choice, until the cloud answers.
func TestBootAgentIntegrationStaleNonOperatorVerdictWaitsForFreshAnswer(t *testing.T) {
	_, _, store := isolateAgentHome(t)
	legacy := func() bool { return true }
	if got := bootAgentIntegration(store, "127.0.0.1:8787", true, true, legacy); got != agentIntegrationOn {
		t.Fatalf("team boot: %q", got)
	}
	// Next boot reads a cached non-operator verdict (not fresh).
	if got := bootAgentIntegration(store, "127.0.0.1:8787", false, false, legacy); got != agentIntegrationOn {
		t.Fatalf("stale non-operator verdict changed the integration: %q", got)
	}
	if rec := store.load(); !rec.OperatorDefault || rec.State != "" || rec.NoticePending {
		t.Fatalf("stale verdict rewrote the record: %+v", rec)
	}

	// The cloud then confirms team: nothing changes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settleAgentIntegrationVerdict(ctx, store, "127.0.0.1:8787", func() (bool, bool, bool) { return true, true, true }, legacy)
	if rec := store.load(); !rec.OperatorDefault || rec.State != "" {
		t.Fatalf("fresh team answer: %+v", rec)
	}

	// A fresh non-operator answer lowers it, and only then records the
	// existing registration as the user's choice.
	settleAgentIntegrationVerdict(ctx, store, "127.0.0.1:8787", func() (bool, bool, bool) { return false, true, true }, legacy)
	rec := store.load()
	if rec.OperatorDefault || rec.State != agentIntegrationOn || rec.Source != agentIntegrationSourceExisting || !rec.NoticePending {
		t.Fatalf("fresh non-operator answer: %+v", rec)
	}
}

// A boot that started in ask on a stale non-operator verdict writes the
// global entries as soon as the cloud confirms operator, not on the next boot;
// a settle that does not turn the integration on writes nothing.
func TestSettleAgentIntegrationAppliesWhenTurningOn(t *testing.T) {
	_, _, store := isolateAgentHome(t)
	applied := 0
	prevApply := applyAgentIntegrationOn
	applyAgentIntegrationOn = func(string) { applied++ }
	t.Cleanup(func() { applyAgentIntegrationOn = prevApply })

	if got := bootAgentIntegration(store, "127.0.0.1:8787", false, false, nil); got != agentIntegrationAsk {
		t.Fatalf("boot: %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settleAgentIntegrationVerdict(ctx, store, "127.0.0.1:8787", func() (bool, bool, bool) { return true, true, true }, nil)
	if applied != 1 {
		t.Fatalf("turning on must apply the registrations once, applied=%d", applied)
	}
	settleAgentIntegrationVerdict(ctx, store, "127.0.0.1:8787", func() (bool, bool, bool) { return true, true, true }, nil)
	settleAgentIntegrationVerdict(ctx, store, "127.0.0.1:8787", func() (bool, bool, bool) { return false, true, true }, nil)
	if applied != 1 {
		t.Fatalf("no change, or on to ask, must not apply: applied=%d", applied)
	}
}

// Without a recorded operator default (a public install), a cached verdict
// behaves as before: nothing to protect.
func TestBootAgentIntegrationStaleVerdictOnPublicInstallUnchanged(t *testing.T) {
	_, _, store := isolateAgentHome(t)
	if got := bootAgentIntegration(store, "127.0.0.1:8787", false, false, func() bool { return false }); got != agentIntegrationAsk {
		t.Fatalf("public install must ask, got %q", got)
	}
	// A stale cached operator verdict still raises it at once.
	if got := bootAgentIntegration(store, "127.0.0.1:8787", true, false, func() bool { return false }); got != agentIntegrationOn {
		t.Fatalf("operator verdict must raise the default, got %q", got)
	}
}

// The settle loop waits while the verdict is unknown or not fresh, and stops
// with the daemon.
func TestSettleAgentIntegrationVerdictWaitsForFresh(t *testing.T) {
	_, _, store := isolateAgentHome(t)
	bootAgentIntegration(store, "127.0.0.1:8787", true, true, nil)
	prev := agentIntegrationSettleEvery
	agentIntegrationSettleEvery = time.Millisecond
	t.Cleanup(func() { agentIntegrationSettleEvery = prev })

	var calls atomic.Int32
	verdict := func() (bool, bool, bool) {
		switch calls.Add(1) {
		case 1:
			return false, false, false // cache not loaded
		case 2, 3:
			return false, true, false // loaded from disk
		}
		return false, true, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settleAgentIntegrationVerdict(ctx, store, "127.0.0.1:8787", verdict, nil)
	if n := calls.Load(); n != 4 {
		t.Fatalf("verdict polled %d times, want 4", n)
	}
	if rec := store.load(); rec.OperatorDefault {
		t.Fatalf("fresh non-operator answer must lower the default: %+v", rec)
	}

	bootAgentIntegration(store, "127.0.0.1:8787", true, true, nil)
	stopped, stop := context.WithCancel(context.Background())
	stop()
	settleAgentIntegrationVerdict(stopped, store, "127.0.0.1:8787", func() (bool, bool, bool) { return false, true, false }, nil)
	if rec := store.load(); !rec.OperatorDefault {
		t.Fatalf("a cancelled settle must not lower the default: %+v", rec)
	}
}

func TestAgentIntegrationGlobalOnWithoutStoreKeepsOldBehaviour(t *testing.T) {
	prev := activeAgentIntegration.Load()
	activeAgentIntegration.Store(nil)
	defer activeAgentIntegration.Store(prev)
	if !agentIntegrationGlobalOn() {
		t.Fatal("no store (tests, CLI helpers) must mean on")
	}
}

func TestHooksAreNotWrittenWithoutConsent(t *testing.T) {
	home, _, store := isolateAgentHome(t)
	bootAgentIntegration(store, "127.0.0.1:8787", false, true, func() bool { return false })
	ensureClaudeHooksRegistered("127.0.0.1:8787")
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("hooks written without consent: %v", err)
	}
	if _, err := store.update(func(r *agentIntegrationRecord) { r.State = agentIntegrationOn }); err != nil {
		t.Fatal(err)
	}
	ensureClaudeHooksRegistered("127.0.0.1:8787")
	body := readTestFile(t, filepath.Join(home, ".claude", "settings.json"))
	for _, event := range claudeHookEvents {
		if !strings.Contains(body, event) {
			t.Fatalf("missing %s after turning on:\n%s", event, body)
		}
	}
}

const testClaudeJSON = `{
  "numStartups": 12,
  "mcpServers": {
    "github": {"type": "http", "url": "https://example.com/mcp"},
    "soluterminal": {"type": "http", "url": "http://127.0.0.1:8787/mcp", "headers": {"X-Soluterminal-Pane": "${SOLU_PANE_ID}"}}
  }
}`

func testClaudeSettings() string {
	ours := claudeHookSpecs("http://127.0.0.1:8787/api/hooks/tool-call", "$SOLU_PANE_ID", "${SOLU_PANE_ID}")
	other := map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "my-linter"}}}
	cfg := map[string]any{
		"model": "opus",
		"hooks": map[string]any{
			"PreToolUse":   []any{other, ours["PreToolUse"]},
			"PostToolUse":  []any{ours["PostToolUse"]},
			"SessionStart": []any{ours["SessionStart"]},
			"Stop":         []any{other},
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return string(b)
}

const testCodexConfig = `model = "gpt-5"

[mcp_servers.docs]
url = "https://docs.example.com/mcp"

[mcp_servers.soluterminal]
url = "http://127.0.0.1:8787/mcp"

[mcp_servers.soluterminal.env_http_headers]
X-Soluterminal-Pane = "SOLU_PANE_ID"
`

func writeAgentFixtures(t *testing.T, home string) {
	writeTestFile(t, filepath.Join(home, ".claude.json"), testClaudeJSON)
	writeTestFile(t, filepath.Join(home, ".claude", "settings.json"), testClaudeSettings())
	writeTestFile(t, filepath.Join(home, ".codex", "config.toml"), testCodexConfig)
	writeTestFile(t, filepath.Join(home, ".codex", "AGENTS.md"), "My own notes.\n\n"+legacyCodexAgentsBlock()+"\n")
}

func TestRemoveAgentIntegrationRemovesOnlyOurEntries(t *testing.T) {
	home, dataDir, _ := isolateAgentHome(t)
	writeAgentFixtures(t, home)
	backup := agentIntegrationBackupDir(dataDir)

	for _, r := range removeAgentIntegration(backup) {
		if r.Error != "" || r.Result != "removed" {
			t.Fatalf("%s: %+v", r.What, r)
		}
	}

	var claude map[string]any
	if err := json.Unmarshal([]byte(readTestFile(t, filepath.Join(home, ".claude.json"))), &claude); err != nil {
		t.Fatal(err)
	}
	servers := claude["mcpServers"].(map[string]any)
	if _, ok := servers["soluterminal"]; ok {
		t.Fatal("soluterminal server left in ~/.claude.json")
	}
	if _, ok := servers["github"]; !ok || claude["numStartups"] == nil {
		t.Fatalf("other Claude config lost: %v", claude)
	}

	var settings map[string]any
	if err := json.Unmarshal([]byte(readTestFile(t, filepath.Join(home, ".claude", "settings.json"))), &settings); err != nil {
		t.Fatal(err)
	}
	hooks := settings["hooks"].(map[string]any)
	if settings["model"] != "opus" {
		t.Fatal("other settings lost")
	}
	if pre := hooks["PreToolUse"].([]any); len(pre) != 1 || claudeHookIsOurEntry(pre[0]) {
		t.Fatalf("PreToolUse should keep only the user's hook: %v", pre)
	}
	if _, ok := hooks["PostToolUse"]; ok {
		t.Fatal("emptied PostToolUse should be dropped")
	}
	if _, ok := hooks["Stop"]; !ok {
		t.Fatal("the user's Stop hook was removed")
	}

	codex := readTestFile(t, filepath.Join(home, ".codex", "config.toml"))
	if strings.Contains(codex, "soluterminal") || !strings.Contains(codex, "[mcp_servers.docs]") || !strings.Contains(codex, `model = "gpt-5"`) {
		t.Fatalf("codex config:\n%s", codex)
	}
	if agents := readTestFile(t, filepath.Join(home, ".codex", "AGENTS.md")); agents != "My own notes.\n" {
		t.Fatalf("AGENTS.md: %q", agents)
	}

	entries, err := os.ReadDir(backup)
	if err != nil || len(entries) != 4 {
		t.Fatalf("expected 4 backups in %s, got %d (%v)", backup, len(entries), err)
	}

	// Running again finds nothing.
	for _, r := range removeAgentIntegration(agentIntegrationBackupDir(dataDir)) {
		if r.Error != "" || r.Result != "nothing to remove" {
			t.Fatalf("second run %s: %+v", r.What, r)
		}
	}
}

func TestClaudeSettingsRemoveHooksLeavesUnreadableFilesAlone(t *testing.T) {
	if _, changed, ok := claudeSettingsRemoveHooks("{not json"); ok || changed {
		t.Fatal("unparseable settings must be left alone")
	}
	if got, changed, ok := claudeSettingsRemoveHooks(`{"model":"x"}`); !ok || changed || got != `{"model":"x"}` {
		t.Fatalf("no hooks: %q %v %v", got, changed, ok)
	}
}

func TestUninstallCleanupCommandRecordsTheUninstall(t *testing.T) {
	home, dataDir, store := isolateAgentHome(t)
	writeAgentFixtures(t, home)
	if _, err := store.update(func(r *agentIntegrationRecord) {
		r.State, r.Source = agentIntegrationOn, agentIntegrationSourceExisting
	}); err != nil {
		t.Fatal(err)
	}
	if code := runUninstallCleanupCommand([]string{"--data-dir", dataDir}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	rec := store.load()
	if rec.effective() != agentIntegrationOff || !rec.needsAnswer() || rec.Source != agentIntegrationSourceUninstall {
		t.Fatalf("after uninstall: %+v", rec)
	}
	if strings.Contains(readTestFile(t, filepath.Join(home, ".claude.json")), "soluterminal") {
		t.Fatal("cleanup left the Claude server")
	}
	// A user's own "off" stays theirs.
	if _, err := store.update(func(r *agentIntegrationRecord) { r.State, r.Source = agentIntegrationOff, agentIntegrationSourceUser }); err != nil {
		t.Fatal(err)
	}
	runUninstallCleanupCommand([]string{"--data-dir=" + dataDir})
	if rec := store.load(); rec.Source != agentIntegrationSourceUser {
		t.Fatalf("user choice overwritten: %+v", rec)
	}
}

func TestAgentIntegrationAPI(t *testing.T) {
	home, dataDir, store := isolateAgentHome(t)
	bootAgentIntegration(store, "127.0.0.1:8787", false, true, func() bool { return false })
	a := &apiServer{addr: "127.0.0.1:8787", dataDir: dataDir}

	applied := 0
	prevApply := applyAgentIntegrationOn
	applyAgentIntegrationOn = func(string) { applied++ }
	defer func() { applyAgentIntegrationOn = prevApply }()

	get := httptest.NewRecorder()
	a.handleGetAgentIntegration(get, httptest.NewRequest(http.MethodGet, "/api/settings/agent-integration", nil))
	var got agentIntegrationResponse
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Effective != agentIntegrationAsk || !got.NeedsAnswer || len(got.Changes) != 3 {
		t.Fatalf("fresh GET: %+v", got)
	}

	post := func(body, prefix string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/settings/agent-integration", strings.NewReader(body))
		if prefix != "" {
			req.Header.Set("X-Forwarded-Prefix", prefix)
		}
		w := httptest.NewRecorder()
		a.handleSetAgentIntegration(w, req)
		return w
	}
	if w := post(`{"state":"on"}`, "/m/abc"); w.Code != http.StatusForbidden {
		t.Fatalf("relayed change must be refused, got %d", w.Code)
	}
	if w := post(`{"state":"maybe"}`, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad state must be rejected, got %d", w.Code)
	}
	if w := post(`{"state":"on"}`, ""); w.Code != http.StatusOK || applied != 1 || store.load().effective() != agentIntegrationOn {
		t.Fatalf("turn on: %d applied=%d %+v", w.Code, applied, store.load())
	}

	writeAgentFixtures(t, home)
	w := post(`{"state":"off"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("turn off: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Effective != agentIntegrationOff || got.NeedsAnswer || len(got.Cleanup) != 4 {
		t.Fatalf("turn off response: %+v", got)
	}
	if strings.Contains(readTestFile(t, filepath.Join(home, ".claude", "settings.json")), "/api/hooks/tool-call") {
		t.Fatal("turning off left our hooks")
	}
	// Off survives a restart and is never re-added.
	bootAgentIntegration(store, "127.0.0.1:8787", false, true, func() bool { return true })
	if agentIntegrationGlobalOn() {
		t.Fatal("off must survive a restart")
	}

	if _, err := store.update(func(r *agentIntegrationRecord) { r.NoticePending = true }); err != nil {
		t.Fatal(err)
	}
	ack := httptest.NewRecorder()
	a.handleAckAgentIntegrationNotice(ack, httptest.NewRequest(http.MethodPost, "/api/settings/agent-integration/notice-ack", nil))
	if ack.Code != http.StatusOK || store.load().NoticePending {
		t.Fatalf("ack: %d %+v", ack.Code, store.load())
	}
}
