package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// credentialPathRefusal is the common boundary for every generic file surface.
// The rules describe ownership of credential material, including atomic-write
// siblings, instead of enumerating only today's exact filenames.
func credentialPathRefusal(path, dataDir string) string {
	const refusal = "refusing to access credential material"
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	home, _ := os.UserHomeDir()
	paths := []string{path, resolveExistingAncestor(path)}
	for _, candidate := range paths {
		if candidate == "" {
			continue
		}
		if credentialOwnedPath(candidate, dataDir, home) {
			return refusal
		}
	}
	return ""
}

// EvalSymlinks on a missing atomic-write sibling fails. Resolve its nearest
// existing ancestor so a symlinked credential directory remains protected.
func resolveExistingAncestor(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	var tail []string
	for dir := filepath.Clean(abs); ; dir = filepath.Dir(dir) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved
		}
		if filepath.Dir(dir) == dir {
			return abs
		}
		tail = append(tail, filepath.Base(dir))
	}
}

func credentialOwnedPath(path, dataDir, home string) bool {
	within := func(root string) (string, bool) {
		if root == "" {
			return "", false
		}
		root = resolveExistingAncestor(root)
		if root == "" {
			return "", false
		}
		candidate := filepath.Clean(path)
		root = filepath.Clean(root)
		if runtime.GOOS == "windows" {
			candidate, root = strings.ToLower(candidate), strings.ToLower(root)
		}
		rel, err := filepath.Rel(root, candidate)
		return rel, err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if rel, ok := within(dataDir); ok {
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) > 0 && parts[0] != "." {
			switch strings.ToLower(parts[0]) {
			case "harness-accounts", "harness-launchers", "harness-state", "codex", "codex-live", "muse", "glm", "api-keys":
				return true
			case recordingsDirName:
				// Pane recordings (pane_recorder.go): raw terminal output that
				// no file surface may read, list or write, locally or remotely.
				return true
			case "workflow":
				// Only the Claude lane credentials live here; task worktrees, task
				// state and health files under workflow/ must stay readable (diffs,
				// previews and the Tasks UI read them through these surfaces).
				if len(parts) > 1 && strings.ToLower(parts[1]) == "creds" {
					return true
				}
			}
		}
		if filepath.Dir(rel) == "." {
			name := strings.ToLower(filepath.Base(rel))
			for _, prefix := range []string{"token", "claim.json", "cloud_revoke_pending.json", "env", "workflow_secrets.json", "model-gateway", "jev-bridge", "claude_creds", "claude_team", "claude_oauth_sessions.json", "claude_autoswitch_history.json", "codex_team", "codex_creds", "codex_autoswitch_history.json", "codex_live_auth.json", "muse_team", "glm_settings.json"} {
				if strings.HasPrefix(name, prefix) || strings.HasPrefix(name, "."+prefix) {
					return true
				}
			}
		}
	}
	for _, spec := range []struct{ root, prefix string }{
		{filepath.Join(home, ".claude"), ".credentials"},
		{os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials"},
		{filepath.Join(home, ".codex"), "auth.json"},
		{os.Getenv("CODEX_HOME"), "auth.json"},
		{filepath.Join(home, ".config", "muse"), "auth.json"},
		{filepath.Join(home, ".config", "glm"), "env"},
		{filepath.Join(home, ".zcode", "cli"), "config.json"},
		{os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json"},
	} {
		if rel, ok := within(spec.root); ok && filepath.Dir(rel) == "." {
			name := strings.ToLower(filepath.Base(rel))
			if strings.HasPrefix(name, spec.prefix) || strings.HasPrefix(name, "."+spec.prefix) {
				return true
			}
		}
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		if rel, ok := within(filepath.Join(xdg, "muse")); ok && filepath.Dir(rel) == "." {
			name := strings.ToLower(filepath.Base(rel))
			if strings.HasPrefix(name, "auth.json") || strings.HasPrefix(name, ".auth.json") {
				return true
			}
		}
	}
	if rel, ok := within(home); ok && filepath.Dir(rel) == "." && strings.HasPrefix(strings.ToLower(filepath.Base(rel)), ".claude.json") {
		return true
	}
	if _, ok := within(filepath.Join(home, ".claude", "state")); ok {
		return true
	}
	if configDir := os.Getenv("CLAUDE_CONFIG_DIR"); configDir != "" {
		if _, ok := within(filepath.Join(configDir, "state")); ok {
			return true
		}
	}
	for _, env := range []string{"MUSE_AUTH_PATH"} {
		if custom := os.Getenv(env); filepath.IsAbs(custom) {
			name := strings.ToLower(filepath.Base(custom))
			if rel, ok := within(filepath.Dir(custom)); ok && filepath.Dir(rel) == "." {
				base := strings.ToLower(filepath.Base(rel))
				if strings.HasPrefix(base, name) || strings.HasPrefix(base, "."+name) {
					return true
				}
			}
		}
	}
	return false
}
