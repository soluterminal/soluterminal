package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	claudeMcpPaneHeaderName  = "X-Soluterminal-Pane"
	claudeMcpPaneHeaderValue = "${SOLU_PANE_ID}"
	claudeMcpPaneHeader      = claudeMcpPaneHeaderName + ": " + claudeMcpPaneHeaderValue
)

// Self-registration of the daemon's MCP server with Claude Code, so panes
// running `claude` get the soluterminal tools (list_ports, open_browser,
// schedules, …) without the user ever hand-typing a URL. Hand-typing
// is exactly how one install ended up registered at http://0.0.0.0:8787/mcp
// — a BIND address pasted from the README's --addr example — which fails to
// connect and silently strips every soluterminal tool from every session.
//
// Policy, deliberately conservative:
//   - entry missing            → add it (product default: tools available;
//     per-project disabling stays available inside Claude Code)
//   - entry on a bind address  → fix it (0.0.0.0 / :: hosts can't be dialed;
//     nobody means them)
//   - entry on our loopback endpoint without the pane header → re-add it;
//     localhost and 127.0.0.1 are equivalent for this upgrade
//   - any other existing entry → leave it alone, even if the port differs
//     from ours: multi-daemon setups (WSL + Windows) share one config, and
//     the other daemon's registration is not ours to steal.
//
// Best-effort by design: no `claude` CLI → skip silently; config edits go
// through `claude mcp add/remove` (the supported interface) rather than
// rewriting ~/.claude.json ourselves. A long-running Claude session can
// still clobber the fix by saving its stale in-memory config — we re-heal
// on the next daemon start, which updates make frequent.

// claudeMcpAction decides what to do about an existing registration URL.
// Returns "add" (no usable entry), "fix" (bind-address entry), or "keep".
// A keep result may still be promoted to fix by the pane-header upgrade check.
// Pure — unit-tested in claude_mcp_register_test.go.
func claudeMcpAction(existingURL string) string {
	existingURL = strings.TrimSpace(existingURL)
	if existingURL == "" {
		return "add"
	}
	u, err := url.Parse(existingURL)
	if err != nil || u.Host == "" {
		return "fix"
	}
	host := u.Hostname()
	if host == "0.0.0.0" || host == "::" || host == "" {
		return "fix"
	}
	return "keep"
}

// claudeMcpDesiredURL is the connect address for THIS daemon's /mcp
// endpoint: always loopback + our listen port, regardless of what host the
// daemon binds (binding 0.0.0.0 for Tailscale/LAN doesn't change how a
// local `claude` should dial us).
func claudeMcpDesiredURL(listenAddr string) string {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" {
		port = "8787"
	}
	return "http://127.0.0.1:" + port + "/mcp"
}

// dualWindowsDaemonPort is where the desktop app runs the Windows-native
// daemon in Dual mode (desktop/src-tauri daemon::DUAL_WIN_ADDR): WSL's
// localhost forwarding owns Windows' 127.0.0.1:8787 once the WSL daemon is
// up, so the Windows backend listens here instead. It is canonical like 8787.
const dualWindowsDaemonPort = "38787"

func canonicalDaemonPort(port string) bool {
	return port == "8787" || port == dualWindowsDaemonPort
}

// mcpTakeoverSiblingEntry reports whether this daemon should re-point an
// existing registration that the keep-policy would otherwise leave alone.
// Only on a Windows host, where the canonical daemons are one family: the
// Windows backend moves between :8787 (Windows-only) and :38787 (Dual), and
// in Dual WSL's forward answers :8787. An entry on the OTHER canonical
// loopback port is therefore a leftover from the previous mode that would
// send this machine's Windows sessions to the wrong daemon. On Linux (the WSL
// daemon) the keep-policy is unchanged. Pure — goos is injected for tests.
func mcpTakeoverSiblingEntry(goos, existing, desired string) bool {
	if goos != "windows" || existing == "" || existing == desired {
		return false
	}
	eu, err := url.Parse(strings.TrimSpace(existing))
	if err != nil {
		return false
	}
	du, err := url.Parse(desired)
	if err != nil {
		return false
	}
	if host := eu.Hostname(); host != "127.0.0.1" && host != "localhost" {
		return false
	}
	return canonicalDaemonPort(eu.Port()) && eu.Port() != du.Port() && strings.TrimSuffix(eu.Path, "/") == strings.TrimSuffix(du.Path, "/")
}

// shouldSelfRegister: only a daemon on a canonical port (8787, or 38787 for
// Dual's Windows backend) owns the shared registration. Dev, sandbox, and secondary daemons always run on
// other ports — a throwaway daemon that adopts the entry and then exits
// leaves the user WORSE off than the broken entry did (this exact hijack
// happened in pre-release testing: a sandbox on :18890 "fixed" the entry to
// itself and died). A user who permanently runs their only daemon on a
// custom port registers once by hand and the keep-policy then respects it.
func shouldSelfRegister(listenAddr string) bool {
	_, port, err := net.SplitHostPort(listenAddr)
	return err == nil && canonicalDaemonPort(port)
}

// currentClaudeMcpURL reads the soluterminal entry from ~/.claude.json.
// Read-only and tolerant: any parse trouble reads as "no usable entry",
// letting the add path (via the CLI, which owns the schema) sort it out.
// Third return is false when the config file itself doesn't exist.
func currentClaudeMcpURL() (string, bool, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, false
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return "", false, false
	}
	var cfg struct {
		McpServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", false, true
	}
	server := cfg.McpServers["soluterminal"]
	_, hasPaneHeader := server.Headers[claudeMcpPaneHeaderName]
	return server.URL, hasPaneHeader, true
}

// claudeMcpNeedsHeaderUpgrade upgrades only our desired-URL registration when
// it is otherwise healthy but lacks automatic pane attribution.
func claudeMcpNeedsHeaderUpgrade(action, existing, desired string, hasPaneHeader bool) bool {
	return action == "keep" && sameLocalMcpEndpoint(existing, desired) && !hasPaneHeader
}

// sameLocalMcpEndpoint treats localhost and 127.0.0.1 as equivalent while
// preserving the conservative policy for different ports and remote hosts.
func sameLocalMcpEndpoint(existing, desired string) bool {
	a, errA := url.Parse(existing)
	b, errB := url.Parse(desired)
	if errA != nil || errB != nil || a.Scheme != b.Scheme || a.Port() != b.Port() || a.EscapedPath() != b.EscapedPath() {
		return false
	}
	isLoopback := func(host string) bool { return host == "localhost" || host == "127.0.0.1" }
	return isLoopback(a.Hostname()) && isLoopback(b.Hostname())
}

// claudeMcpAddArgs keeps Claude's variadic --header flag after the required
// positionals so it cannot consume the server name and URL.
func claudeMcpAddArgs(desired string) []string {
	return []string{"mcp", "add", "--transport", "http", "--scope", "user", "soluterminal", desired, "--header", claudeMcpPaneHeader}
}

// claudeMcpRegMu serializes registration attempts: the boot goroutine and a
// post-install registerClaudeIntegrations can overlap, and both end in a
// read-modify-write of the same config.
var claudeMcpRegMu sync.Mutex

// ensureClaudeMcpRegistration is THE ONLY writer of the soluterminal entry in
// Claude Code's config (boot goroutine + post-install re-register). A second,
// older writer used to rewrite ~/.claude.json with the raw --addr value —
// re-breaking the entry to http://0.0.0.0:8787/mcp on every boot of a
// LAN-bound daemon, in a fight with this file's repair. It is gone; every
// path now funnels here and only the loopback-normalized URL is ever written.
func ensureClaudeMcpRegistration(listenAddr string) {
	if os.Getenv("SOLU_SKIP_MCP_REGISTER") == "1" {
		return
	}
	if !shouldSelfRegister(listenAddr) {
		return
	}
	// Opt-in (agent_integration.go): without consent nothing global is written.
	if !agentIntegrationGlobalOn() {
		return
	}
	claudeMcpRegMu.Lock()
	defer claudeMcpRegMu.Unlock()
	existing, hasPaneHeader, haveCfg := currentClaudeMcpURL()
	action := claudeMcpAction(existing)
	desired := claudeMcpDesiredURL(listenAddr)
	headerUpgrade := claudeMcpNeedsHeaderUpgrade(action, existing, desired, hasPaneHeader)
	if headerUpgrade {
		action = "fix"
	}
	if action == "keep" && mcpTakeoverSiblingEntry(runtime.GOOS, existing, desired) {
		log.Printf("claude-mcp: re-pointing soluterminal registration %q → %q (this Windows daemon's port changed with the backend mode)", existing, desired)
		action = "fix"
	}
	if action == "keep" {
		return
	}
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		// No claude CLI on THIS process's PATH — common under systemd and
		// other non-login shells even when Claude Code is installed. If a
		// config exists, heal/add the entry by direct write; if not, this
		// machine has no Claude Code and there is nothing to register with.
		if haveCfg {
			registerClaudeMcpDirect(desired, action, existing, headerUpgrade, "claude CLI not on PATH")
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if action == "fix" {
		// `claude mcp add` refuses to overwrite an existing name; remove
		// first. Failure is fine (racing session, already gone) — the add
		// below is what matters.
		_ = exec.CommandContext(ctx, claudeBin, "mcp", "remove", "--scope", "user", "soluterminal").Run()
	}
	out, err := exec.CommandContext(ctx, claudeBin, claudeMcpAddArgs(desired)...).CombinedOutput()
	if err != nil {
		log.Printf("claude-mcp: could not register %s: %v (%s)", desired, err, strings.TrimSpace(string(out)))
		// A CLI without --header support must not leave a fresh install
		// unregistered or a previously healthy registration deleted.
		if haveCfg {
			registerClaudeMcpDirect(desired, action, existing, headerUpgrade, "claude CLI add failed")
		}
		return
	}
	switch action {
	case "fix":
		if headerUpgrade {
			log.Printf("claude-mcp: added pane-attribution header to existing soluterminal registration %s", desired)
		} else {
			log.Printf("claude-mcp: fixed soluterminal registration %q → %q (bind addresses aren't dialable)", existing, desired)
		}
	default:
		log.Printf("claude-mcp: registered soluterminal MCP server for Claude Code at %s", desired)
	}
}

// registerClaudeMcpDirect is the no-CLI fallback: read ~/.claude.json, set
// mcpServers.soluterminal to the loopback-normalized URL, atomic-replace.
// Handles add/fix plus pane-header upgrades of our own local desired endpoint.
// A parse failure aborts rather than overwriting a file we couldn't read; the
// CLI path (which owns the schema) heals it on a later boot.
func registerClaudeMcpDirect(desired, action, existing string, headerUpgrade bool, reason string) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	path := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("claude-mcp: could not parse %s: %v (not overwriting)", path, err)
		return
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		cfg["mcpServers"] = servers
	}
	servers["soluterminal"] = map[string]any{
		"type": "http",
		"url":  desired,
		"headers": map[string]any{
			claudeMcpPaneHeaderName: claudeMcpPaneHeaderValue,
		},
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".solu-tmp"
	if err := os.WriteFile(tmp, out, 0600); err != nil {
		log.Printf("claude-mcp: could not write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		log.Printf("claude-mcp: could not replace %s: %v", path, err)
		return
	}
	switch action {
	case "fix":
		if headerUpgrade {
			log.Printf("claude-mcp: added pane-attribution header to existing soluterminal registration %s (direct write; %s)", desired, reason)
		} else {
			log.Printf("claude-mcp: fixed soluterminal registration %q → %q (direct write; %s)", existing, desired, reason)
		}
	default:
		log.Printf("claude-mcp: registered soluterminal MCP server at %s (direct write; %s)", desired, reason)
	}
}
