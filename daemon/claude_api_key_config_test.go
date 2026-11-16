package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeClaudeCfgFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeStoredAPIKeyIn(t *testing.T) {
	cases := []struct {
		name     string
		config   string // .claude.json body; "" = no file
		settings string // settings.json body; "" = no file
		want     bool
	}{
		{"no files", "", "", false},
		{"console login key", `{"numStartups":3,"primaryApiKey":"sk-ant-test"}`, "", true},
		{"logged out key", `{"primaryApiKey":null}`, "", false},
		{"empty key", `{"primaryApiKey":"  "}`, "", false},
		{"non-string key", `{"primaryApiKey":{"x":1}}`, "", false},
		{"oauth only", `{"oauthAccount":{"emailAddress":"a@example.com"}}`, "", false},
		{"invalid config", `{"primaryApiKey":`, "", false},
		{"api key helper", "", `{"apiKeyHelper":"~/bin/key.sh"}`, true},
		{"empty helper", "", `{"apiKeyHelper":""}`, false},
		{"settings env key", "", `{"env":{"ANTHROPIC_API_KEY":"sk-ant-test"}}`, true},
		{"settings env other", "", `{"env":{"FOO":"bar"}}`, false},
		{"other-case key name", `{"primaryapikey":"sk-ant-test"}`, `{"ApiKeyHelper":"x","env":{"anthropic_api_key":"y"}}`, false},
		{"non-object env", "", `{"env":"ANTHROPIC_API_KEY"}`, false},
		{"approved env key only", `{"customApiKeyResponses":{"approved":["abc"],"rejected":[]}}`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := filepath.Join(dir, ".claude.json")
			settings := filepath.Join(dir, "settings.json")
			if c.config != "" {
				writeClaudeCfgFixture(t, cfg, c.config)
			}
			if c.settings != "" {
				writeClaudeCfgFixture(t, settings, c.settings)
			}
			if got := claudeStoredAPIKeyIn(cfg, []string{settings, ""}); got != c.want {
				t.Fatalf("claudeStoredAPIKeyIn = %v, want %v", got, c.want)
			}
		})
	}
}

// A managed settings file (second entry) counts like the user's own.
func TestClaudeStoredAPIKeyInManagedSettings(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "managed-settings.json")
	writeClaudeCfgFixture(t, managed, `{"apiKeyHelper":"/opt/key-helper"}`)
	if !claudeStoredAPIKeyIn(filepath.Join(dir, ".claude.json"), []string{filepath.Join(dir, "settings.json"), managed}) {
		t.Fatal("managed apiKeyHelper not detected")
	}
}

// noManagedClaudeSettings keeps a test off this machine's real managed
// settings file, which may set an apiKeyHelper.
func noManagedClaudeSettings(t *testing.T) {
	t.Helper()
	prev := claudeManagedSettingsFiles
	claudeManagedSettingsFiles = func() []string { return nil }
	t.Cleanup(func() { claudeManagedSettingsFiles = prev })
}

func TestClaudeManagedSettingsPaths(t *testing.T) {
	env := map[string]string{"ProgramFiles": `C:\Program Files`, "CLAUDE_CODE_MANAGED_SETTINGS_PATH": "/x/managed.json"}
	get := func(k string) string { return env[k] }
	if got := claudeManagedSettingsPaths("linux", get); len(got) != 2 || got[0] != "/etc/claude-code/managed-settings.json" || got[1] != "/x/managed.json" {
		t.Fatalf("linux paths = %v", got)
	}
	if got := claudeManagedSettingsPaths("windows", get); len(got) != 2 || got[0] != filepath.Join(`C:\Program Files`, "ClaudeCode", "managed-settings.json") {
		t.Fatalf("windows paths = %v", got)
	}
}

func TestClaudeStoredAPIKeyConfiguredUsesClaudeConfigDir(t *testing.T) {
	noManagedClaudeSettings(t)
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if claudeStoredAPIKeyConfigured() {
		t.Fatal("empty config dir reported a stored key")
	}
	writeClaudeCfgFixture(t, filepath.Join(dir, ".claude.json"), `{"primaryApiKey":"sk-ant-test"}`)
	if !claudeStoredAPIKeyConfigured() {
		t.Fatal("primaryApiKey in $CLAUDE_CONFIG_DIR/.claude.json not detected")
	}
}

func TestClaudeStoredAPIKeyConfiguredUsesHomeConfig(t *testing.T) {
	noManagedClaudeSettings(t)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writeClaudeCfgFixture(t, filepath.Join(home, ".claude", "settings.json"), `{"apiKeyHelper":"key.sh"}`)
	if !claudeStoredAPIKeyConfigured() {
		t.Fatal("apiKeyHelper in ~/.claude/settings.json not detected")
	}
	if err := os.Remove(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	writeClaudeCfgFixture(t, filepath.Join(home, ".claude.json"), `{"primaryApiKey":"sk-ant-test"}`)
	if !claudeStoredAPIKeyConfigured() {
		t.Fatal("primaryApiKey in ~/.claude.json not detected")
	}
}

func TestReadClaudeConfigFileSkipsOversized(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxClaudeConfigBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var v map[string]any
	if readClaudeConfigFile(path, &v) {
		t.Fatal("oversized file was read")
	}
}
