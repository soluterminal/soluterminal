package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Claude Code keeps an API key of its own in two places that never write a
// credential file, so the first-run "signed in" check has to look for them
// (ST-97):
//   - a key made at Claude's own login ("Anthropic Console account") is saved as
//     primaryApiKey in .claude.json (macOS saves it in the Keychain instead);
//   - an apiKeyHelper command, or an ANTHROPIC_API_KEY in the settings env,
//     in Claude's user or managed settings.
//
// Only presence is checked. The decoder below keeps no part of any value: a
// presentSetting records whether the JSON value is a non-empty string, nothing
// more, so no key text is held, logged or returned.

// maxClaudeConfigBytes bounds a config read. .claude.json grows with project
// history; a file past this is skipped (no verdict from it) rather than read.
const maxClaudeConfigBytes = 32 << 20

// presentSetting is true when a JSON value is a non-empty string. It never
// stores the string.
type presentSetting bool

func (p *presentSetting) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) != nil {
		*p = false
		return nil
	}
	*p = presentSetting(strings.TrimSpace(s) != "")
	return nil
}

// claudeConfigJSONPathFor is Claude Code's .claude.json for a Claude dir:
// inside $CLAUDE_CONFIG_DIR when set, otherwise in the home directory.
func claudeConfigJSONPathFor(claudeDir string) string {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); v != "" {
		return filepath.Join(claudeDir, ".claude.json")
	}
	if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		return filepath.Join(home, ".claude.json")
	}
	return filepath.Join(filepath.Dir(claudeDir), ".claude.json")
}

// claudeManagedSettingsPaths are the machine-wide managed settings files
// Claude Code reads on this OS: the fixed per-OS file, plus any named by
// CLAUDE_CODE_MANAGED_SETTINGS_PATH or CLAUDE_CODE_REMOTE_SETTINGS_PATH.
func claudeManagedSettingsPaths(goos string, getenv func(string) string) []string {
	var paths []string
	switch goos {
	case "windows":
		if root := getenv("ProgramFiles"); root != "" {
			paths = append(paths, filepath.Join(root, "ClaudeCode", "managed-settings.json"))
		}
	case "darwin":
		paths = append(paths, "/Library/Application Support/ClaudeCode/managed-settings.json")
	default:
		paths = append(paths, "/etc/claude-code/managed-settings.json")
	}
	for _, key := range []string{"CLAUDE_CODE_MANAGED_SETTINGS_PATH", "CLAUDE_CODE_REMOTE_SETTINGS_PATH"} {
		if path := getenv(key); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

// claudeManagedSettingsFiles is a test seam: tests must not read this
// machine's real managed settings.
var claudeManagedSettingsFiles = func() []string { return claudeManagedSettingsPaths(runtime.GOOS, os.Getenv) }

// claudeStoredAPIKeyConfigured reports whether Claude Code has an API key of
// its own: a /login-managed primaryApiKey, an apiKeyHelper, or an
// ANTHROPIC_API_KEY in its settings env. Errors and unreadable files count as
// "no".
func claudeStoredAPIKeyConfigured() bool {
	dir, err := resolveClaudeDir()
	if err != nil {
		return false
	}
	return claudeStoredAPIKeyIn(claudeConfigJSONPathFor(dir), append([]string{filepath.Join(dir, "settings.json")}, claudeManagedSettingsFiles()...))
}

func claudeStoredAPIKeyIn(configJSON string, settingsFiles []string) bool {
	// Fields are matched by exact name, as Claude Code reads them; a struct
	// would also accept other spellings, since encoding/json ignores case.
	var cfg map[string]presentSetting
	if readClaudeConfigFile(configJSON, &cfg) && bool(cfg["primaryApiKey"]) {
		return true
	}
	for _, path := range settingsFiles {
		if path == "" {
			continue
		}
		var settings map[string]json.RawMessage
		if !readClaudeConfigFile(path, &settings) {
			continue
		}
		if settingPresent(settings["apiKeyHelper"]) {
			return true
		}
		var env map[string]presentSetting
		if raw := settings["env"]; len(raw) > 0 && json.Unmarshal(raw, &env) == nil && bool(env["ANTHROPIC_API_KEY"]) {
			return true
		}
	}
	return false
}

// settingPresent reports a raw JSON value that is a non-empty string.
func settingPresent(raw json.RawMessage) bool {
	var p presentSetting
	return len(raw) > 0 && p.UnmarshalJSON(raw) == nil && bool(p)
}

// readClaudeConfigFile decodes a bounded JSON file into v; false when the file is
// missing, not a regular file (a pipe would block the read), oversized or not
// JSON.
func readClaudeConfigFile(path string, v any) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxClaudeConfigBytes {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}
