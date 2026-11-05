package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Removal of everything SoluTerminal writes into Claude Code's and Codex's
// global configuration. Used when the user turns the integration off and by
// `solud uninstall-cleanup` (which the Windows uninstaller runs). Only our own
// entries go: the "soluterminal" MCP server, our hook entries (matched by our
// callback path), the [mcp_servers.soluterminal] table and the marker-delimited
// AGENTS.md block. Each file is copied to backupDir before it is changed.

type agentCleanupResult struct {
	What   string `json:"what"`
	Path   string `json:"path,omitempty"`
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

// backupConfigFile copies path into backupDir before an edit. A missing
// backupDir skips the copy; a failed copy aborts the edit.
func backupConfigFile(path, backupDir string) error {
	if backupDir == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return err
	}
	name := strings.TrimPrefix(strings.ReplaceAll(filepath.ToSlash(path), "/", "_"), "_")
	return os.WriteFile(filepath.Join(backupDir, name), data, 0o600)
}

// rewriteConfigFile applies edit to path, backing it up first. Results:
// "absent", "nothing to remove", "left as is (…)" or "removed".
func rewriteConfigFile(path, backupDir string, edit func(string) (string, bool, bool)) (string, error) {
	validate := func([]byte) error { return nil }
	switch filepath.Ext(path) {
	case ".json":
		validate = validateJSONObject
	case ".toml":
		validate = validateTOML
	}
	for attempt := 0; attempt < configWriteAttempts; attempt++ {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return "absent", nil
		}
		if err != nil {
			return "", err
		}
		next, changed, ok := edit(string(data))
		if !ok {
			return "left as is (unrecognized shape)", nil
		}
		if !changed {
			return "nothing to remove", nil
		}
		if err := backupConfigFile(path, backupDir); err != nil {
			return "", fmt.Errorf("backup failed, file left unchanged: %w", err)
		}
		// Through a symlink, exclusive temp file, mode kept, re-read before
		// the rename (writeAgentConfig).
		err = writeAgentConfig(path, data, []byte(next), validate)
		if errors.Is(err, errConfigChangedMeanwhile) {
			continue
		}
		if err != nil {
			return "", err
		}
		return "removed", nil
	}
	return "", fmt.Errorf("%s kept changing while it was being updated", path)
}

// agentIntegrationOwner names the SoluTerminal install whose entries a
// removal may touch. The zero value (uninstall) removes every SoluTerminal
// entry. A daemon turning its own switch off passes its own MCP and hook URLs,
// so a second copy (a side-port test daemon, another install sharing the same
// home) never deletes the entries the main install relies on.
type agentIntegrationOwner struct {
	MCPURL  string
	HookURL string
	// MCPSocket is the socket this install's bridge entries name.
	MCPSocket string
	GOOS      string
}

func (o agentIntegrationOwner) all() bool {
	return o.MCPURL == "" && o.HookURL == "" && o.MCPSocket == ""
}

// ownsBridgeEntry reports whether a bridge entry of ours (args from
// ~/.claude.json) names this install's socket.
func (o agentIntegrationOwner) ownsBridgeEntry(args []string) bool {
	if o.all() {
		return true
	}
	return o.MCPSocket != "" && len(args) == 3 && args[1] == "--socket" && args[2] == o.MCPSocket
}

// ownsMCPURL reports whether an existing registration points at this install.
// On Windows the canonical ports are one family (see mcpTakeoverSiblingEntry).
func (o agentIntegrationOwner) ownsMCPURL(existing string) bool {
	if o.all() || strings.TrimSpace(existing) == "" || o.MCPURL == "" {
		return true
	}
	return sameLocalMcpEndpoint(existing, o.MCPURL) || mcpTakeoverSiblingEntry(o.GOOS, existing, o.MCPURL)
}

// ownsHookEntry reports whether one of our hook entries calls this install.
func (o agentIntegrationOwner) ownsHookEntry(entry any) bool {
	if !claudeHookIsOurEntry(entry) {
		return false
	}
	if o.all() || o.HookURL == "" {
		return true
	}
	own, err := url.Parse(o.HookURL)
	if err != nil {
		return true
	}
	em, _ := entry.(map[string]any)
	hs, _ := em["hooks"].([]any)
	for _, h := range hs {
		hm, _ := h.(map[string]any)
		target, _ := hm["url"].(string)
		if target == "" {
			target, _ = hm["command"].(string)
		}
		if strings.Contains(target, "127.0.0.1:"+own.Port()+"/") || strings.Contains(target, "localhost:"+own.Port()+"/") {
			return true
		}
		if o.GOOS == "windows" {
			for _, port := range []string{"8787", dualWindowsDaemonPort} {
				if strings.Contains(target, ":"+port+"/api/hooks/") {
					return true
				}
			}
		}
	}
	return false
}

// claudeSettingsRemoveHooks drops our hook entries from a settings.json body.
// Event lists we empty are removed, and so is "hooks" if we empty it; every
// other key and every other tool's hooks stay.
func claudeSettingsRemoveHooks(body string) (string, bool, bool) {
	return claudeSettingsRemoveOwnedHooks(body, agentIntegrationOwner{})
}

func claudeSettingsRemoveOwnedHooks(body string, owner agentIntegrationOwner) (string, bool, bool) {
	if strings.TrimSpace(body) == "" {
		return body, false, true
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		return body, false, false
	}
	hooks, _ := cfg["hooks"].(map[string]any)
	if hooks == nil {
		return body, false, true
	}
	changed := false
	for event, raw := range hooks {
		entries, ok := raw.([]any)
		if !ok {
			continue
		}
		kept := entries[:0:0]
		for _, entry := range entries {
			if owner.ownsHookEntry(entry) {
				changed = true
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) == len(entries) {
			continue
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if !changed {
		return body, false, true
	}
	if len(hooks) == 0 {
		delete(cfg, "hooks")
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return body, false, false
	}
	return string(out) + "\n", true, true
}

// claudeJSONRemoveServer drops mcpServers.soluterminal from ~/.claude.json.
func claudeJSONRemoveServer(body string) (string, bool, bool) {
	var cfg map[string]any
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		return body, false, false
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if _, ok := servers["soluterminal"]; !ok {
		return body, false, true
	}
	delete(servers, "soluterminal")
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return body, false, false
	}
	return string(out), true, true
}

// removeClaudeMcpServer prefers the supported CLI (it owns ~/.claude.json and
// coordinates with running sessions) and falls back to a direct edit when the
// CLI is missing or fails.
func removeClaudeMcpServer(home, backupDir string, owner agentIntegrationOwner) agentCleanupResult {
	path := filepath.Join(home, ".claude.json")
	res := agentCleanupResult{What: "Claude Code MCP server", Path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		res.Result = "absent"
		return res
	}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if why := claudeServerNotOwned(string(data), owner); why != "" {
		res.Result = why
		return res
	}
	if _, changed, ok := claudeJSONRemoveServer(string(data)); ok && !changed {
		res.Result = "nothing to remove"
		return res
	}
	if claudeBin, err := exec.LookPath("claude"); err == nil {
		if err := backupConfigFile(path, backupDir); err != nil {
			res.Error = "backup failed, file left unchanged: " + err.Error()
			return res
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, runErr := exec.CommandContext(ctx, claudeBin, "mcp", "remove", "--scope", "user", "soluterminal").CombinedOutput()
		cancel()
		if runErr == nil {
			res.Result = "removed"
			return res
		}
		_ = out // fall through to the direct edit
	}
	// The ownership check runs again on every attempt: a retry after
	// another program's save must not remove an entry that has since become
	// another install's.
	result, err := rewriteConfigFile(path, backupDir, claudeRemoveOwnedServer(owner))
	res.Result = result
	if err != nil {
		res.Error = err.Error()
	}
	return res
}

// claudeServerNotOwned says why mcpServers.soluterminal in body is not
// owner's to remove, or "" when it is (or there is nothing there).
func claudeServerNotOwned(body string, owner agentIntegrationOwner) string {
	existing := claudeJSONServerURL(body)
	if existing == "" && claudeJSONHasServer(body) {
		// A stdio entry: ours only as a bridge entry naming this install's
		// socket. Someone else's, or another install's bridge, stays.
		if !claudeEntryIsOurBridge([]byte(body)) {
			return "left as is (it isn't a SoluTerminal entry)"
		}
		if !owner.ownsBridgeEntry(claudeJSONServerArgs(body)) {
			return "left as is (it belongs to another SoluTerminal's MCP socket)"
		}
		return ""
	}
	if !owner.ownsMCPURL(existing) {
		return "left as is (it belongs to another SoluTerminal at " + existing + ")"
	}
	return ""
}

// claudeRemoveOwnedServer is claudeJSONRemoveServer for an entry owner still
// owns when the file is read; otherwise it changes nothing.
func claudeRemoveOwnedServer(owner agentIntegrationOwner) func(string) (string, bool, bool) {
	return func(body string) (string, bool, bool) {
		if claudeServerNotOwned(body, owner) != "" {
			return body, false, true
		}
		return claudeJSONRemoveServer(body)
	}
}

// claudeJSONHasServer reports whether mcpServers.soluterminal exists.
func claudeJSONHasServer(body string) bool {
	var cfg struct {
		McpServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal([]byte(body), &cfg) != nil {
		return false
	}
	_, ok := cfg.McpServers["soluterminal"]
	return ok
}

// claudeJSONServerArgs is the args of mcpServers.soluterminal.
func claudeJSONServerArgs(body string) []string {
	var cfg struct {
		McpServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal([]byte(body), &cfg) != nil {
		return nil
	}
	return cfg.McpServers["soluterminal"].Args
}

// claudeJSONServerURL is the url of mcpServers.soluterminal ("" if none).
func claudeJSONServerURL(body string) string {
	var cfg struct {
		McpServers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal([]byte(body), &cfg) != nil {
		return ""
	}
	return cfg.McpServers["soluterminal"].URL
}

// removeAgentIntegration removes all four global entries for the current user
// (uninstall). Turning the switch off uses removeAgentIntegrationFor with the
// daemon's own URLs.
func removeAgentIntegration(backupDir string) []agentCleanupResult {
	return removeAgentIntegrationFor(backupDir, agentIntegrationOwner{})
}

// removeAgentIntegrationFor removes the global entries that belong to owner.
func removeAgentIntegrationFor(backupDir string, owner agentIntegrationOwner) []agentCleanupResult {
	var results []agentCleanupResult
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return []agentCleanupResult{{What: "home directory", Error: "could not resolve the home directory"}}
	}
	add := func(what, path, result string, err error) {
		r := agentCleanupResult{What: what, Path: path, Result: result}
		if err != nil {
			r.Error = err.Error()
		}
		results = append(results, r)
	}

	results = append(results, removeClaudeMcpServer(home, backupDir, owner))
	settings := filepath.Join(home, ".claude", "settings.json")
	res, err := rewriteConfigFile(settings, backupDir, func(body string) (string, bool, bool) {
		return claudeSettingsRemoveOwnedHooks(body, owner)
	})
	add("Claude Code hooks", settings, res, err)

	if cfgPath, err := codexConfigPath(); err != nil {
		add("Codex config", "", "", err)
	} else if body, readErr := os.ReadFile(cfgPath); readErr == nil && !owner.ownsMCPURL(codexConfigMcpURL(string(body), "soluterminal")) {
		existing := codexConfigMcpURL(string(body), "soluterminal")
		add("Codex MCP server", cfgPath, "left as is (it belongs to another SoluTerminal at "+existing+")", nil)
		add("Codex AGENTS.md block", filepath.Join(filepath.Dir(cfgPath), "AGENTS.md"), "left as is (it belongs to another SoluTerminal)", nil)
	} else {
		agents := filepath.Join(filepath.Dir(cfgPath), "AGENTS.md")
		res, err := rewriteConfigFile(agents, backupDir, codexAgentsRemove)
		add("Codex AGENTS.md block", agents, res, err)
		res, err = rewriteConfigFile(cfgPath, backupDir, func(body string) (string, bool, bool) {
			next, changed := codexConfigRemoveServer(body, "soluterminal")
			return next, changed, true
		})
		add("Codex MCP server", cfgPath, res, err)
	}
	return results
}

// agentIntegrationBackupDir is a fresh, timestamped backup folder in dataDir.
func agentIntegrationBackupDir(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "agent-integration-backups", time.Now().UTC().Format("20060102-150405"))
}
