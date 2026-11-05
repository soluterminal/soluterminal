package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Self-registration of the daemon's MCP server with the Codex CLI, mirroring
// ensureClaudeMcpRegistration: panes running `codex` get the soluterminal
// tools without the user hand-editing ~/.codex/config.toml. Same policy —
// missing entry → add, bind-address entry → fix, anything else → keep — and
// the same 8787-only + SOLU_SKIP_MCP_REGISTER gates. Config edits go through
// `codex mcp add/remove` (the supported interface) when the CLI is
// resolvable, with a narrow line-surgery fallback for PATHs without it
// (systemd shells; codex commonly lives under nvm).
//
// Muse deliberately has no counterpart here: Muse Code (0.1.x) has no MCP
// client at all — its extension surface is skills/plugins — so there is
// nothing to register. Muse panes are still fully drivable as MCP *targets*
// (send_input / read_pane_tail); they just cannot call soluterminal tools
// themselves until Meta ships MCP support.

var codexMcpRegMu sync.Mutex

// Probed against codex-cli 0.146.0 on 2026-08-09, following the probe-first
// culture documented in runtime_codex.go: (1) streamable_http
// mcp_servers entries accept http_headers (a static map) and env_http_headers
// (header name → environment-variable name), verified with `codex -c ... mcp
// get --json`; (2) env_http_headers resolves from the Codex process environment
// at request time — with SOLU_PANE_ID set, a local listener captured
// `x-soluterminal-pane: probe-pane-42` on the initialize POST from
// codex-mcp-client/0.146.0, so one global config still gives per-pane values;
// (3) an unset variable is non-fatal — `codex doctor` reports "header env var X
// is not set" as an optional issue and leaves the server enabled, so out-of-app
// sessions simply send no header (and explicit pane_id wins in workflow_mcp.go);
// (4) `codex mcp add` has no flag for either header map (only
// --bearer-token-env-var), so this assignment requires careful line surgery.
const codexMcpPaneHeaderLine = `env_http_headers = { "` + claudeMcpPaneHeaderName + `" = "SOLU_PANE_ID" }`

const (
	codexPaneHeaderMissing = "missing"
	codexPaneHeaderOurs    = "ours"
	codexPaneHeaderForeign = "foreign"
)

func codexConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("no home dir")
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

var tomlTableRe = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*(?:#.*)?$`)
var tomlURLRe = regexp.MustCompile(`^\s*url\s*=\s*"([^"]*)"\s*(?:#.*)?$`)
var tomlCommandRe = regexp.MustCompile(`^\s*command\s*=\s*"([^"]*)"\s*(?:#.*)?$`)

// codexServerTableName extracts NAME from a [mcp_servers.NAME] header,
// tolerating the quoted form. Sub-tables (mcp_servers.NAME.env) return
// false — they belong to a server but are not one.
func codexServerTableName(header string) (string, bool) {
	h := strings.TrimSpace(header)
	rest, ok := strings.CutPrefix(h, "mcp_servers.")
	if !ok {
		return "", false
	}
	if strings.HasPrefix(rest, `"`) && strings.HasSuffix(rest, `"`) && len(rest) > 2 {
		inner := rest[1 : len(rest)-1]
		if strings.Contains(inner, `"`) {
			return "", false
		}
		return inner, true
	}
	if rest == "" || strings.ContainsAny(rest, `."`) {
		return "", false
	}
	return rest, true
}

// codexMcpServers lists the MCP servers the Codex CLI is configured with
// (~/.codex/config.toml) for the sidebar inventory — source "codex". Codex
// has no project scoping; one global config. url entries type as "http" so
// the sidebar's TCP liveness probe covers them like Claude's.
func codexMcpServers() []McpServerInfo {
	path, err := codexConfigPath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []McpServerInfo
	cur := -1
	for _, line := range strings.Split(string(data), "\n") {
		if m := tomlTableRe.FindStringSubmatch(line); m != nil {
			cur = -1
			if name, ok := codexServerTableName(m[1]); ok {
				out = append(out, McpServerInfo{Name: name, Source: "codex"})
				cur = len(out) - 1
			}
			continue
		}
		if cur < 0 {
			continue
		}
		if m := tomlURLRe.FindStringSubmatch(line); m != nil {
			out[cur].URL = m[1]
			out[cur].Type = "http"
		} else if m := tomlCommandRe.FindStringSubmatch(line); m != nil {
			out[cur].Command = m[1]
			out[cur].Type = "stdio"
		}
	}
	return out
}

// tomlTableIs reports whether a [header] names exactly mcp_servers.<name>,
// tolerating the quoted form mcp_servers."<name>". Sub-tables
// (mcp_servers.<name>.env) do NOT match — they are different tables and
// their keys must never be mistaken for the server's own.
func tomlTableIs(header, name string) bool {
	h := strings.TrimSpace(header)
	return h == "mcp_servers."+name || h == `mcp_servers."`+name+`"`
}

// codexConfigMcpURL extracts the url of [mcp_servers.<name>] from a
// config.toml body ("" when the section or key is absent). Line-oriented on
// purpose: we own neither the file nor a TOML dependency, and this reads
// only the one shape `codex mcp add --url` writes.
func codexConfigMcpURL(body, name string) string {
	inSection := false
	for _, line := range strings.Split(body, "\n") {
		if m := tomlTableRe.FindStringSubmatch(line); m != nil {
			inSection = tomlTableIs(m[1], name)
			continue
		}
		if !inSection {
			continue
		}
		if m := tomlURLRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// codexConfigFixMcpURL replaces the url line inside [mcp_servers.<name>],
// leaving every other byte untouched. Second return is false when no url
// line was found to replace.
func codexConfigFixMcpURL(body, name, desired string) (string, bool) {
	lines := strings.Split(body, "\n")
	inSection := false
	for i, line := range lines {
		if m := tomlTableRe.FindStringSubmatch(line); m != nil {
			inSection = tomlTableIs(m[1], name)
			continue
		}
		if !inSection {
			continue
		}
		if tomlURLRe.MatchString(line) {
			lines[i] = `url = "` + desired + `"`
			return strings.Join(lines, "\n"), true
		}
	}
	return body, false
}

// codexConfigPaneHeaderState classifies only the soluterminal server's
// env_http_headers. An assignment we do not recognize, or any sub-table form,
// is user-authored territory and must be left alone.
func codexConfigPaneHeaderState(body string) string {
	inSection := false
	ours := false
	foreign := false
	for _, line := range strings.Split(body, "\n") {
		if m := tomlTableRe.FindStringSubmatch(line); m != nil {
			header := strings.TrimSpace(m[1])
			if header == "mcp_servers.soluterminal.env_http_headers" || header == `mcp_servers."soluterminal".env_http_headers` {
				foreign = true
			}
			inSection = tomlTableIs(header, "soluterminal")
			continue
		}
		if !inSection {
			continue
		}
		trimmed := strings.TrimSpace(line)
		key, _, assignment := strings.Cut(trimmed, "=")
		if !assignment || strings.TrimSpace(key) != "env_http_headers" {
			continue
		}
		if strings.Contains(line, claudeMcpPaneHeaderName) {
			ours = true
		} else {
			foreign = true
		}
	}
	if foreign {
		return codexPaneHeaderForeign
	}
	if ours {
		return codexPaneHeaderOurs
	}
	return codexPaneHeaderMissing
}

// codexConfigInsertPaneHeader inserts immediately after soluterminal's url
// assignment and preserves every other byte.
func codexConfigInsertPaneHeader(body string) (string, bool) {
	lines := strings.Split(body, "\n")
	inSection := false
	for i, line := range lines {
		if m := tomlTableRe.FindStringSubmatch(line); m != nil {
			inSection = tomlTableIs(m[1], "soluterminal")
			continue
		}
		if inSection && tomlURLRe.MatchString(line) {
			lines = append(lines, "")
			copy(lines[i+2:], lines[i+1:])
			lines[i+1] = codexMcpPaneHeaderLine
			return strings.Join(lines, "\n"), true
		}
	}
	return body, false
}

// ensureCodexMcpRegistration runs once at daemon startup on its own
// goroutine, next to its Claude sibling.
func ensureCodexMcpRegistration(listenAddr string) {
	requireNormalBootEngine("codex_mcp_register.ensureCodexMcpRegistration")
	if os.Getenv("SOLU_SKIP_MCP_REGISTER") == "1" {
		return
	}
	if !shouldSelfRegister(listenAddr) {
		return
	}
	// Opt-in (agent_integration.go): without consent nothing global is written,
	// neither the MCP server nor the AGENTS.md block.
	if !agentIntegrationGlobalOn() {
		return
	}
	codexMcpRegMu.Lock()
	defer codexMcpRegMu.Unlock()

	cfgPath, err := codexConfigPath()
	if err != nil {
		return
	}
	body, readErr := os.ReadFile(cfgPath)
	haveCfg := readErr == nil
	desired := claudeMcpDesiredURL(listenAddr)
	existing := ""
	hasTable := false
	if haveCfg {
		// Repair first: duplicate soluterminal tables an older daemon left,
		// or a bridge entry this daemon can't serve. Then the URL policy
		// below sees the result.
		note := ""
		changed, err := updateCodexConfig(cfgPath, func(b string) (string, bool) {
			next, _, n, change := codexPlanRegistration(b, desired)
			note = n
			return next, change
		})
		if err != nil {
			log.Printf("codex-mcp: %v", err)
		} else if changed {
			log.Printf("codex-mcp: %s in %s", note, cfgPath)
		}
		if b, err := os.ReadFile(cfgPath); err == nil {
			body = b
		}
		existing = codexConfigMcpURL(string(body), "soluterminal")
		hasTable = len(codexSoluterminalTables(strings.Split(string(body), "\n"))) > 0
	}
	codexBin, binErr := resolveCodexBin()
	if !haveCfg && binErr != nil {
		// No config and no CLI means Codex is not installed on this machine.
		// Do not create either global file in that case.
		return
	}

	syncCodexAgentsMd(filepath.Join(filepath.Dir(cfgPath), "AGENTS.md"))

	action := claudeMcpAction(existing) // same URL policy: add / fix / keep
	if existing == "" && hasTable {
		// A soluterminal table with no url is a live bridge entry or one
		// someone else wrote: never add a second table next to it.
		action = "keep"
	}
	if action == "keep" && mcpTakeoverSiblingEntry(runtime.GOOS, existing, desired) {
		log.Printf("codex-mcp: re-pointing soluterminal registration %q → %q (this Windows daemon's port changed with the backend mode)", existing, desired)
		action = "fix"
	}
	if action != "keep" && existing != desired {
		if binErr != nil {
			// A config proves Codex is present; heal it directly when the CLI is
			// unavailable to this daemon's non-login PATH.
			registerCodexMcpDirect(cfgPath, string(body), desired, action, existing)
		} else {
			// CLI path. A missing config is fine here: `codex mcp add` creates it
			// with the right schema.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if action == "fix" {
				removeCmd := hiddenCommand(ctx, codexBin, "mcp", "remove", "soluterminal")
				removeCmd.Env = providerChildEnvForBin(codexBin, os.Environ())
				_ = removeCmd.Run()
			}
			addCmd := hiddenCommand(ctx, codexBin,
				"mcp", "add", "soluterminal", "--url", desired,
			)
			addCmd.Env = providerChildEnvForBin(codexBin, os.Environ())
			out, cmdErr := addCmd.CombinedOutput()
			cancel()
			if cmdErr != nil {
				log.Printf("codex-mcp: could not register %s: %v outputLen=%d", desired, cmdErr, len(out))
			} else {
				switch action {
				case "fix":
					log.Printf("codex-mcp: fixed soluterminal registration %q → %q (bind addresses aren't dialable)", existing, desired)
				default:
					log.Printf("codex-mcp: registered soluterminal MCP server for Codex at %s", desired)
				}
			}
		}
	}

	// Re-read because `codex mcp add` or the direct fallback may have just
	// rewritten the section. This pass also upgrades an already-correct URL.
	ensureCodexMcpPaneHeader(cfgPath, desired)
	ensureCodexMcpDefaults(cfgPath, desired)
}

// codexConfigEnsureDefaults adds, inside our own [mcp_servers.soluterminal]
// table only, a tool_timeout_sec long enough for ask_pane and an
// approval_mode = "approve" sub-table for each read-only tool. Anything the
// user already set (a timeout, any approval_mode for a tool) is left alone.
func codexConfigEnsureDefaults(body string) (string, bool) {
	lines := strings.Split(body, "\n")
	inSection := false
	urlLine, hasTimeout := -1, false
	present := map[string]bool{}
	for i, line := range lines {
		if m := tomlTableRe.FindStringSubmatch(line); m != nil {
			h := strings.TrimSpace(m[1])
			inSection = tomlTableIs(h, "soluterminal")
			for _, prefix := range []string{"mcp_servers.soluterminal.tools.", `mcp_servers."soluterminal".tools.`} {
				if strings.HasPrefix(h, prefix) {
					present[strings.Trim(strings.TrimPrefix(h, prefix), `"`)] = true
				}
			}
			continue
		}
		if !inSection {
			continue
		}
		if tomlURLRe.MatchString(line) {
			urlLine = i
		}
		if key, _, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.TrimSpace(key) == "tool_timeout_sec" {
			hasTimeout = true
		}
	}
	if urlLine < 0 {
		return body, false
	}
	changed := false
	if !hasTimeout {
		lines = append(lines, "")
		copy(lines[urlLine+2:], lines[urlLine+1:])
		lines[urlLine+1] = "tool_timeout_sec = " + strconv.Itoa(codexSoluToolTimeoutSec)
		changed = true
	}
	next := strings.Join(lines, "\n")
	for _, t := range soluReadOnlyMCPTools {
		if present[t] {
			continue
		}
		if next != "" && !strings.HasSuffix(next, "\n") {
			next += "\n"
		}
		next += "\n[mcp_servers.soluterminal.tools." + t + "]\napproval_mode = \"approve\"\n"
		changed = true
	}
	return next, changed
}

func ensureCodexMcpDefaults(path, desired string) {
	changed, err := updateCodexConfig(path, func(body string) (string, bool) {
		if existing := codexConfigMcpURL(body, "soluterminal"); !sameLocalMcpEndpoint(existing, desired) {
			return body, false
		}
		return codexConfigEnsureDefaults(body)
	})
	if err != nil {
		log.Printf("codex-mcp: %v", err)
	} else if changed {
		log.Printf("codex-mcp: pre-approved read-only soluterminal tools and set tool_timeout_sec in %s", path)
	}
}

func ensureCodexMcpPaneHeader(path, desired string) {
	changed, err := updateCodexConfig(path, func(body string) (string, bool) {
		existing := codexConfigMcpURL(body, "soluterminal")
		if claudeMcpAction(existing) != "keep" || !sameLocalMcpEndpoint(existing, desired) {
			return body, false
		}
		switch codexConfigPaneHeaderState(body) {
		case codexPaneHeaderOurs:
			return body, false
		case codexPaneHeaderForeign:
			log.Printf("codex-mcp: soluterminal entry has an env_http_headers we did not write in %s (leaving as is)", path)
			return body, false
		}
		return codexConfigInsertPaneHeader(body)
	})
	if err != nil {
		log.Printf("codex-mcp: %v", err)
	} else if changed {
		log.Printf("codex-mcp: added per-pane env_http_headers attribution in %s", path)
	}
}

// registerCodexMcpDirect is the no-CLI fallback: fix rewrites only the url
// line of the existing section; add appends a fresh section at EOF. Atomic
// replace either way.
func registerCodexMcpDirect(path, _, desired, action, existing string) {
	changed, err := updateCodexConfig(path, func(body string) (string, bool) {
		// The caller decided from an earlier read. If the entry is no longer
		// the one it saw (another program changed it meanwhile), write
		// nothing; the next start decides again. (A second table is never
		// added: see the default case.)
		if codexConfigMcpURL(body, "soluterminal") != existing {
			return body, false
		}
		switch action {
		case "fix":
			fixed, ok := codexConfigFixMcpURL(body, "soluterminal", desired)
			if !ok {
				// Section exists in some shape we don't understand — leave
				// it alone rather than risk mangling a hand-written config.
				log.Printf("codex-mcp: entry %q not in a shape we can heal in %s (leaving as is)", existing, path)
				return body, false
			}
			return fixed, true
		default:
			if len(codexSoluterminalTables(strings.Split(body, "\n"))) > 0 {
				return body, false // never a second table
			}
			next := body
			if next != "" && !strings.HasSuffix(next, "\n") {
				next += "\n"
			}
			return next + "\n[mcp_servers.soluterminal]\nurl = \"" + desired + "\"\n" + codexMcpPaneHeaderLine + "\n", true
		}
	})
	if err != nil {
		log.Printf("codex-mcp: %v", err)
		return
	}
	if !changed {
		return
	}
	switch action {
	case "fix":
		log.Printf("codex-mcp: fixed soluterminal registration %q → %q (direct write; codex CLI not found)", existing, desired)
	default:
		log.Printf("codex-mcp: registered soluterminal MCP server at %s (direct write; codex CLI not found)", desired)
	}
}
