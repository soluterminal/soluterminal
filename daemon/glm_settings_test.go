package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeGlmSettingsKey(t *testing.T, dir, key string) {
	t.Helper()
	b, _ := json.Marshal(glmSettings{Key: key})
	if err := os.WriteFile(filepath.Join(dir, "glm_settings.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

func countKey(env []string, key string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			n++
		}
	}
	return n
}

func TestGlmSettingsDefaults(t *testing.T) {
	g := newGlmSettingsStore(t.TempDir()).Snapshot()
	if g.BaseURL != glmDefaultBaseURL {
		t.Fatalf("baseURL = %q", g.BaseURL)
	}
	if g.OpusModel != glmDefaultModel || g.SonnetModel != glmDefaultModel {
		t.Fatalf("models = %q/%q", g.OpusModel, g.SonnetModel)
	}
	if g.HaikuModel != glmDefaultHaikuModel {
		t.Fatalf("haiku = %q", g.HaikuModel)
	}
	if g.AutoCompactWindow != glmDefaultAutoCompactWindow {
		t.Fatalf("window = %d", g.AutoCompactWindow)
	}
}

func TestResolveKeyFromStore(t *testing.T) {
	dir := t.TempDir()
	writeGlmSettingsKey(t, dir, "store-key-123")
	st := newGlmSettingsStore(dir)
	got, err := st.resolveKey()
	if err != nil || got != "store-key-123" {
		t.Fatalf("resolveKey = %q, %v", got, err)
	}
	if !st.KeyConfigured() {
		t.Fatal("KeyConfigured should be true")
	}
}

func TestResolveKeyFromEnvFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, ".config", "glm")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "env"), []byte("# a comment\n\nZAI_API_KEY=file-key-456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := newGlmSettingsStore(t.TempDir()) // no store Key → falls back to the env file
	got, err := st.resolveKey()
	if err != nil || got != "file-key-456" {
		t.Fatalf("resolveKey = %q, %v", got, err)
	}
}

func TestResolveKeyMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := newGlmSettingsStore(t.TempDir())
	if _, err := st.resolveKey(); err == nil {
		t.Fatal("expected missing-key error")
	}
	if st.KeyConfigured() {
		t.Fatal("KeyConfigured should be false")
	}
}

func TestResolveKeyPlaceholderIgnored(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, ".config", "glm")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "env"), []byte("ZAI_API_KEY=PASTE_YOUR_ZAI_KEY_HERE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if newGlmSettingsStore(t.TempDir()).KeyConfigured() {
		t.Fatal("placeholder must not count as configured")
	}
}

// TestAgentGlmEnvBuild reproduces the agent's env assembly for a glm pane and
// asserts the two invariants the adversarial review flagged: the GLM token wins
// (no stale ANTHROPIC_API_KEY survives to shadow it) and the GLM
// CLAUDE_CODE_AUTO_COMPACT_WINDOW is NOT lost to the CLAUDE_CODE_* strip.
func TestAgentGlmEnvBuild(t *testing.T) {
	dir := t.TempDir()
	writeGlmSettingsKey(t, dir, "tok-789")

	// A hostile inherited env: stale Anthropic creds + a stale auto-compact.
	inherited := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=stale-anthropic",
		"ANTHROPIC_BASE_URL=https://api.anthropic.com",
		"CLAUDE_CODE_SESSION_ID=should-be-stripped",
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW=999",
	}
	// Mirror agent.go: strip CLAUDE_CODE_*, then (glm) scrub conflicts, then append.
	env := stripInheritedClaudeEnv(inherited)
	env = filterOutEnvKeys(env, glmConflictEnvKeys)
	glmEnv, err := providerEnvVars(dir)
	if err != nil {
		t.Fatalf("providerEnvVars: %v", err)
	}
	env = append(env, glmEnv...)

	m := envMap(env)
	if m["ANTHROPIC_AUTH_TOKEN"] != "tok-789" {
		t.Fatalf("token = %q, want tok-789", m["ANTHROPIC_AUTH_TOKEN"])
	}
	if _, ok := m["ANTHROPIC_API_KEY"]; ok {
		t.Fatal("stale ANTHROPIC_API_KEY must be scrubbed so it can't shadow the token")
	}
	if m["ANTHROPIC_BASE_URL"] != glmDefaultBaseURL {
		t.Fatalf("baseURL = %q, want %q", m["ANTHROPIC_BASE_URL"], glmDefaultBaseURL)
	}
	if m["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] != "1000000" {
		t.Fatalf("auto-compact = %q (must survive the strip AND be the GLM value)", m["CLAUDE_CODE_AUTO_COMPACT_WINDOW"])
	}
	if n := countKey(env, "ANTHROPIC_AUTH_TOKEN"); n != 1 {
		t.Fatalf("ANTHROPIC_AUTH_TOKEN occurs %d times, want exactly 1", n)
	}
	for _, kv := range env {
		if strings.Contains(kv, "tok-789") && !strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") {
			t.Fatalf("token leaked into a non-AUTH_TOKEN entry: %q", kv)
		}
	}
}

func TestGlmWrapPrefixNonSecret(t *testing.T) {
	dir := t.TempDir()
	writeGlmSettingsKey(t, dir, "secret-tok")
	st := newGlmSettingsStore(dir)

	pfx := glmWrapPrefix("glm", st)
	if !strings.HasPrefix(pfx, "unset ANTHROPIC_API_KEY;") {
		t.Fatalf("prefix must start by unsetting API key: %q", pfx)
	}
	if !strings.Contains(pfx, "ANTHROPIC_BASE_URL=") || !strings.Contains(pfx, "CLAUDE_CODE_AUTO_COMPACT_WINDOW=") {
		t.Fatalf("prefix missing non-secret exports: %q", pfx)
	}
	if strings.Contains(pfx, "secret-tok") || strings.Contains(pfx, "ANTHROPIC_AUTH_TOKEN") {
		t.Fatalf("prefix must NOT contain the token: %q", pfx)
	}
	if glmWrapPrefix("", st) != "" || glmWrapPrefix("glm", nil) != "" {
		t.Fatal("non-glm/nil store must yield an empty prefix")
	}
}

func TestTryLoginShellWrapGlmPrefixNoTokenLeak(t *testing.T) {
	dir := t.TempDir()
	writeGlmSettingsKey(t, dir, "secret-tok")
	st := newGlmSettingsStore(dir)
	wrapped, ok := tryLoginShellWrap([]string{"claude"}, "glm", st)
	if !ok {
		t.Skip("bash not on PATH in this environment")
	}
	joined := strings.Join(wrapped, " ")
	if !strings.Contains(joined, "unset ANTHROPIC_API_KEY") {
		t.Fatalf("wrap missing the glm non-secret prefix: %q", joined)
	}
	if strings.Contains(joined, "secret-tok") {
		t.Fatalf("token leaked into the wrap argv (readable via /proc/cmdline): %q", joined)
	}
}

func TestPersistedPaneProviderRoundTrip(t *testing.T) {
	pp := persistedFromSnapshot(PaneSnapshot{ID: "p1", Provider: "glm"})
	if pp.Provider != "glm" {
		t.Fatalf("persistedFromSnapshot Provider = %q", pp.Provider)
	}
	b, _ := json.Marshal(pp)
	var back PersistedPane
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Provider != "glm" {
		t.Fatalf("round-trip Provider = %q", back.Provider)
	}
	// A legacy panes.json entry without "provider" must unmarshal to "" (Anthropic).
	var legacy PersistedPane
	if err := json.Unmarshal([]byte(`{"id":"p2","cwd":"/tmp","title":"t","createdAt":"2020-01-01T00:00:00Z","cols":80,"rows":24,"socketPath":"/x","agentPid":1}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Provider != "" {
		t.Fatalf("legacy entry Provider = %q, want empty", legacy.Provider)
	}
}
