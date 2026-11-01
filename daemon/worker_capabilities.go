package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

var errWorkerQuota = errors.New("no qualifying account has fresh quota headroom")

type workerCapabilityError struct{ Needs []string }

func (e *workerCapabilityError) Error() string {
	return "no provider qualifies for required capability: " + strings.Join(e.Needs, ", ")
}
func workerNeeds(args map[string]any, cwd string) ([]string, error) {
	var needs []string
	if raw, ok := args["needs"]; ok {
		switch values := raw.(type) {
		case []any:
			for _, v := range values {
				s, ok := v.(string)
				if !ok {
					return nil, errors.New("needs must be an array of capability names")
				}
				needs = append(needs, s)
			}
		case []string:
			needs = append(needs, values...)
		default:
			return nil, errors.New("needs must be an array of capability names")
		}
	}
	if len(needs) > 16 {
		return nil, errors.New("needs supports at most 16 capabilities")
	}
	if guardedWorkerFolder(cwd) {
		needs = append(needs, "role-guard")
	}
	seen := map[string]bool{}
	out := []string{}
	for _, name := range needs {
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 64 {
			return nil, errors.New("invalid capability name")
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}
func requiredWorkerCapabilities(guarded bool, extra ...[]string) []string {
	var needs []string
	for _, items := range extra {
		needs = append(needs, items...)
	}
	if guarded {
		needs = append(needs, "role-guard")
	}
	return needs
}
func (c workerAccount) qualifies(needs []string) bool {
	for _, need := range needs {
		if !c.Capabilities[need] {
			return false
		}
	}
	return true
}
func filterWorkerCapabilities(accounts []workerAccount, needs []string) ([]workerAccount, error) {
	out := []workerAccount{}
	for _, c := range accounts {
		if c.qualifies(needs) {
			out = append(out, c)
		}
	}
	if len(accounts) > 0 && len(out) == 0 && len(needs) > 0 {
		return nil, &workerCapabilityError{Needs: needs}
	}
	return out, nil
}

type workerCodexConfig struct {
	Features struct {
		Hooks bool `toml:"hooks"`
	} `toml:"features"`
	Hooks struct {
		PreToolUse []struct {
			Matcher string `toml:"matcher"`
			Hooks   []struct {
				Type    string `toml:"type"`
				Command string `toml:"command"`
			} `toml:"hooks"`
		} `toml:"PreToolUse"`
	} `toml:"hooks"`
	Servers map[string]struct {
		Enabled *bool  `toml:"enabled"`
		Command string `toml:"command"`
		URL     string `toml:"url"`
	} `toml:"mcp_servers"`
}

func readWorkerCodexConfig(path string) (workerCodexConfig, bool) {
	var cfg workerCodexConfig
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 4<<20 || toml.Unmarshal(raw, &cfg) != nil {
		return cfg, false
	}
	return cfg, true
}
func codexRoleGuard(cwd string) bool {
	cfg, ok := readWorkerCodexConfig(filepath.Join(cwd, ".codex", "config.toml"))
	if !ok || !cfg.Features.Hooks {
		return false
	}
	for _, group := range cfg.Hooks.PreToolUse {
		if group.Matcher != "" && group.Matcher != "*" {
			continue
		}
		for _, hook := range group.Hooks {
			if hook.Type == "command" && strings.TrimSpace(hook.Command) != "" {
				return true
			}
		}
	}
	return false
}
func claudeRoleGuard(cwd string) bool {
	raw, err := os.ReadFile(filepath.Join(cwd, ".claude", "settings.json"))
	if err != nil || len(raw) > 4<<20 {
		return false
	}
	var cfg struct {
		Hooks map[string][]struct {
			Matcher string                           `json:"matcher"`
			Hooks   []struct{ Type, Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return false
	}
	for _, group := range cfg.Hooks["PreToolUse"] {
		if group.Matcher != "" && group.Matcher != "*" {
			continue
		}
		for _, hook := range group.Hooks {
			if hook.Type == "command" && strings.TrimSpace(hook.Command) != "" {
				return true
			}
		}
	}
	return false
}
func workerProviderCapabilities(cwd string) map[string]map[string]bool {
	cwd = expandHome(strings.TrimSpace(cwd))
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	if path, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = path
	}
	caps := map[string]map[string]bool{"claude": {"role-guard": claudeRoleGuard(cwd)}, "codex": {"role-guard": codexRoleGuard(cwd)}}
	for _, server := range listMCPServersForCwd(cwd) {
		if server.Name == "solubrowse" && server.Source != "codex" && (server.Command != "" || server.URL != "") {
			caps["claude"]["browser"] = true
		}
	}
	// Project tables override the global table, including enabled=false.
	if home, err := resolveCodexHome(); err == nil {
		for _, path := range []string{filepath.Join(home, "config.toml"), filepath.Join(cwd, ".codex", "config.toml")} {
			cfg, ok := readWorkerCodexConfig(path)
			if !ok {
				continue
			}
			if server, found := cfg.Servers["solubrowse"]; found {
				caps["codex"]["browser"] = (server.Enabled == nil || *server.Enabled) && (server.Command != "" || server.URL != "")
			}
		}
	}
	return caps
}
func workerCapabilityRequirements(args map[string]any) ([]string, map[string]map[string]bool, error) {
	cwd := mcpArgString(args, "cwd")
	needs, err := workerNeeds(args, cwd)
	if err != nil {
		return nil, nil, err
	}
	caps := workerProviderCapabilities(cwd)
	qualified := false
	for provider, available := range caps {
		if (workerAccount{Provider: provider, Capabilities: available}).qualifies(needs) {
			qualified = true
		}
	}
	if !qualified {
		return needs, nil, &workerCapabilityError{Needs: needs}
	}
	return needs, caps, nil
}
func (a *apiServer) workerCandidates(args map[string]any, now time.Time) ([]workerAccount, []string, error) {
	needs, caps, err := workerCapabilityRequirements(args)
	if err != nil {
		return nil, needs, err
	}
	if quotaWorkerAccounts == nil {
		return nil, needs, fmt.Errorf("account usage data unavailable in this build")
	}
	accounts := quotaWorkerAccounts(a, now)
	out := []workerAccount{}
	for _, c := range accounts {
		c.Capabilities = caps[c.Provider]
		if c.qualifies(needs) {
			out = append(out, c)
		}
	}
	return out, needs, nil
}
