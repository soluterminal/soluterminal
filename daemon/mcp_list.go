package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// McpServerInfo is what the sidebar renders. Lowercase JSON to match the
// rest of the API surface. Responses carry the short display form of URL,
// Command and Args (see mcpServersForDisplay).
type McpServerInfo struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`              // "http" | "sse" | "stdio"
	Source  string   `json:"source"`            // "user" | "project" | ".mcp.json"
	URL     string   `json:"url,omitempty"`     // http/sse
	Command string   `json:"command,omitempty"` // stdio
	Args    []string `json:"args,omitempty"`    // stdio
	// Liveness from a TCP connect probe. "alive" / "dead" for http/sse;
	// empty for stdio (we can't tell without spawning, and spawning just
	// to probe is too heavy for a sidebar tick).
	Status string `json:"status,omitempty"`
}

// listMCPServersForCwd returns the effective set of MCP servers Claude
// Code would see if launched from cwd. Three sources, in increasing
// precedence so name collisions are resolved in the same order Claude
// Code itself uses:
//
//  1. ~/.claude.json → mcpServers           (source: "user")
//  2. ~/.claude.json → projects[ANCESTOR_OF_CWD].mcpServers
//     (source: "project")
//  3. .mcp.json walking up from cwd         (source: ".mcp.json")
//
// plus the Codex CLI's global servers (~/.codex/config.toml) as separate
// "codex" rows — a different consumer, never merged into Claude precedence.
//
// Missing or malformed config is not an error — empty list, log once.
//
// The result holds url, command and args exactly as configured. It is for
// use inside the daemon; pass it through mcpServersForDisplay before it is
// written to any response.
func listMCPServersForCwd(cwd string) []McpServerInfo {
	out := map[string]McpServerInfo{}

	cfg := readClaudeJSON()
	if cfg != nil {
		if user, ok := cfg["mcpServers"].(map[string]any); ok {
			for name, entry := range user {
				if info, ok := parseServerEntry(name, entry, "user"); ok {
					out[name] = info
				}
			}
		}
		if projs, ok := cfg["projects"].(map[string]any); ok {
			// Match by exact cwd plus any ancestor — Claude Code keys by
			// exact dir, but a pane in a subdir should still see the
			// project's servers.
			for _, ancestor := range ancestorPaths(cwd) {
				proj, ok := projs[ancestor].(map[string]any)
				if !ok {
					continue
				}
				servers, ok := proj["mcpServers"].(map[string]any)
				if !ok {
					continue
				}
				for name, entry := range servers {
					if info, ok := parseServerEntry(name, entry, "project"); ok {
						out[name] = info // overrides "user"
					}
				}
			}
		}
	}

	// .mcp.json walks: project-shared config files. Walk up so a deeper
	// pane still finds the repo-root .mcp.json.
	for _, ancestor := range ancestorPaths(cwd) {
		f := filepath.Join(ancestor, ".mcp.json")
		stat, err := os.Stat(f)
		if err != nil || stat.IsDir() {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			log.Printf("mcp: skipping malformed %s: %v", f, err)
			continue
		}
		servers, ok := doc["mcpServers"].(map[string]any)
		if !ok {
			continue
		}
		for name, entry := range servers {
			if info, ok := parseServerEntry(name, entry, ".mcp.json"); ok {
				out[name] = info // overrides user + project
			}
		}
	}

	list := make([]McpServerInfo, 0, len(out))
	for _, v := range out {
		list = append(list, v)
	}
	// Codex's servers ride along as their own rows (source "codex") rather
	// than joining the Claude-precedence map: same-named entries in both
	// configs are different consumers of possibly different servers, and
	// collapsing them would hide one.
	list = append(list, codexMcpServers()...)
	sort.Slice(list, func(i, j int) bool {
		if list[i].Name != list[j].Name {
			return list[i].Name < list[j].Name
		}
		return list[i].Source < list[j].Source
	})
	return list
}

// readClaudeJSON parses ~/.claude.json. Returns nil for any failure
// (missing file, parse error) — the caller treats nil as "no entries".
func readClaudeJSON() map[string]any {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	path := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("mcp: skipping malformed %s: %v", path, err)
		return nil
	}
	return cfg
}

// ancestorPaths yields cwd, parent, grandparent… stopping at "/". The
// caller cares about ordering only insofar as later entries override
// earlier ones (handled by the map upsert), so we yield deepest-first
// (most specific first) and let the loop assign in any order — final
// result is a set, not a list.
func ancestorPaths(cwd string) []string {
	cwd = filepath.Clean(cwd)
	if cwd == "" || cwd == "." {
		return nil
	}
	out := []string{}
	for {
		out = append(out, cwd)
		parent := filepath.Dir(cwd)
		if parent == cwd {
			break
		}
		cwd = parent
	}
	return out
}

// parseServerEntry turns a raw Claude-Code config value into a typed
// McpServerInfo. Returns (_, false) for entries we can't make sense of
// — a defensive parse, since the file is user-editable.
func parseServerEntry(name string, raw any, source string) (McpServerInfo, bool) {
	m, ok := raw.(map[string]any)
	if !ok {
		return McpServerInfo{}, false
	}
	t, _ := m["type"].(string)
	if t == "" {
		// Claude Code defaults missing type to stdio when "command" is
		// present. Mirror that so the UI doesn't show a blank badge.
		if _, hasCmd := m["command"]; hasCmd {
			t = "stdio"
		} else if _, hasURL := m["url"]; hasURL {
			t = "http"
		}
	}
	info := McpServerInfo{
		Name:   name,
		Type:   t,
		Source: source,
	}
	if u, ok := m["url"].(string); ok {
		info.URL = u
	}
	if c, ok := m["command"].(string); ok {
		info.Command = c
	}
	if a, ok := m["args"].([]any); ok {
		args := make([]string, 0, len(a))
		for _, x := range a {
			if s, ok := x.(string); ok {
				args = append(args, s)
			}
		}
		info.Args = args
	}
	return info, true
}

// probeServers mutates the slice in place, setting Status to "alive" or
// "dead" for http/sse entries based on a 500 ms TCP connect. We deliberately
// don't speak HTTP — some MCP servers reject HEAD or require auth, and
// any HTTP response would be a "server is listening" signal that plain
// TCP already gives us for free. Stdio entries are skipped (can't probe
// without spawning). Parallelized: total latency is max(probe) ≈ 500 ms
// regardless of fan-out, so the sidebar stays responsive even with a
// dozen servers configured.
func probeServers(list []McpServerInfo) {
	const timeout = 500 * time.Millisecond
	var wg sync.WaitGroup
	for i := range list {
		if list[i].Type != "http" && list[i].Type != "sse" {
			continue
		}
		if list[i].URL == "" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			list[i].Status = probeURL(list[i].URL, timeout)
		}(i)
	}
	wg.Wait()
}

func probeURL(raw string, timeout time.Duration) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "dead"
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		// url.Host is authority-only; fill in the scheme default so
		// e.g. "example.com" → "example.com:443" over https.
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return "dead"
	}
	_ = conn.Close()
	return "alive"
}

// HTTP handler — GET /api/mcp/servers?cwd=<absolute path>
func (a *apiServer) listMcpServers(w http.ResponseWriter, r *http.Request) {
	cwd := r.URL.Query().Get("cwd")
	if cwd == "" || !strings.HasPrefix(cwd, "/") {
		http.Error(w, "cwd query param must be a non-empty absolute path", 400)
		return
	}
	list := listMCPServersForCwd(cwd)
	probeServers(list)
	// The probe needs the configured address; everything a client receives
	// is the short display form.
	writeJSON(w, 200, mcpServersForDisplay(list))
}
