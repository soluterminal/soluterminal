package main

import (
	"encoding/json"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Per-pane Claude Code and Codex config for Linux panes (ST-253 M3, W3 PR 4;
// @secure ruling 8.6a.6). A Linux pane's agent reaches this host only through
// its helper's in-distro sockets:
//
//   - MCP: a stdio server, `<helper bin> mcp-bridge --socket <runtimeDir>/mcp.sock`;
//   - hooks: command hooks, `<helper bin> hook-forward --socket <runtimeDir>/hooks.sock`.
//
// The argv carries socket paths and event names only: no URL, token, claim,
// host secret or pane id. The helper names the pane from the kernel
// (attestation), never from anything the agent says. Nothing is written to
// the Linux ~/.claude or ~/.codex. Applied only while fwd is enabled: with it
// off, the helper serves no sockets and the flags would only show a broken
// server.

// linuxPaneAgentPaths are the in-distro paths the injected config names.
type linuxPaneAgentPaths struct {
	Bin, MCPSocket, HooksSocket string
}

// linuxPaneAgentPathsFor builds them from the helper's placed binary and its
// runtime dir, or reports false when either is unusable in a hook command
// (not absolute, or a character the shell would treat specially).
func linuxPaneAgentPathsFor(bin, runtimeDir string) (linuxPaneAgentPaths, bool) {
	if !linuxPaneAgentPathOK(bin) || !linuxPaneAgentPathOK(runtimeDir) {
		return linuxPaneAgentPaths{}, false
	}
	return linuxPaneAgentPaths{
		Bin:         bin,
		MCPSocket:   path.Join(runtimeDir, helperFwdMCPSocket),
		HooksSocket: path.Join(runtimeDir, helperFwdHooksSocket),
	}, true
}

func linuxPaneAgentPathOK(p string) bool {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p || len(p) > 4096 {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || strings.ContainsRune("'\"`$\\!*?[]{}()<>|&;#~ \t", r) {
			return false
		}
	}
	return true
}

// linuxPaneAgentCmd rewrites a Linux pane's command when it starts Claude Code
// or Codex directly (by base name); anything else is returned as it is.
func linuxPaneAgentCmd(p linuxPaneAgentPaths, cmd []string) []string {
	if len(cmd) == 0 {
		return cmd
	}
	switch path.Base(cmd[0]) {
	case "claude":
		return append([]string{cmd[0]}, linuxClaudeInjectArgs(p, cmd[1:])...)
	case "codex":
		return append([]string{cmd[0]}, linuxCodexInjectArgs(p, cmd[1:])...)
	}
	return cmd
}

// linuxClaudeInjectArgs adds the stdio MCP server and command hooks to a
// Claude Code argv (without the binary), as claudeInjectArgs does for a local
// pane over HTTP.
func linuxClaudeInjectArgs(p linuxPaneAgentPaths, args []string) []string {
	if len(args) > 0 && claudeSkipInjectSubcommands[args[0]] {
		return args
	}
	opts := argsBeforeDoubleDash(args)
	if cliArgsHasAny(opts, "-v", "--version", "-h", "--help", "--strict-mcp-config") {
		return args
	}
	var out []string
	if !cliArgsHasAny(opts, "--mcp-config") {
		cfg := map[string]any{"mcpServers": map[string]any{"soluterminal": map[string]any{
			"type":    "stdio",
			"command": p.Bin,
			"args":    []any{mcpBridgeCommand, "--socket", p.MCPSocket},
		}}}
		b, _ := json.Marshal(cfg)
		out = append(out, "--mcp-config", string(b))
	}
	if !cliArgsHasAny(opts, "--settings") {
		hook := map[string]any{
			"matcher": "",
			"hooks": []any{map[string]any{
				"type":    "command",
				"command": "'" + p.Bin + "' " + hookForwardCommand + " --socket '" + p.HooksSocket + "'",
				"timeout": 5,
			}},
		}
		hooks := map[string]any{}
		for _, event := range claudeHookEvents {
			hooks[event] = []any{hook}
		}
		b, _ := json.Marshal(map[string]any{
			"hooks":       hooks,
			"permissions": map[string]any{"allow": claudeSoluReadOnlyAllowRules()},
		})
		out = append(out, "--settings", string(b))
	}
	return insertBeforeDoubleDash(args, out)
}

// linuxCodexInjectArgs adds the stdio MCP server to a Codex argv (without the
// binary), as codexInjectArgs does for a local pane over HTTP.
func linuxCodexInjectArgs(p linuxPaneAgentPaths, args []string) []string {
	if len(args) > 0 && codexSkipInjectSubcommands[args[0]] {
		return args
	}
	opts := argsBeforeDoubleDash(args)
	if cliArgsHasAny(opts, "-V", "--version", "-h", "--help") {
		return args
	}
	for _, a := range opts {
		if strings.HasPrefix(a, "mcp_servers.soluterminal.") {
			return args
		}
	}
	var out []string
	out = append(out,
		"-c", "mcp_servers.soluterminal.command="+strconv.Quote(p.Bin),
		"-c", `mcp_servers.soluterminal.args=["`+mcpBridgeCommand+`","--socket",`+strconv.Quote(p.MCPSocket)+`]`,
		"-c", "mcp_servers.soluterminal.tool_timeout_sec="+strconv.Itoa(codexSoluToolTimeoutSec),
	)
	for _, t := range soluReadOnlyMCPTools {
		out = append(out, "-c", "mcp_servers.soluterminal.tools."+t+`.approval_mode="approve"`)
	}
	return insertBeforeDoubleDash(args, out)
}

// argsBeforeDoubleDash is the options part of an argv: everything before
// the first "--". What follows it is positional (a prompt) and never a flag.
func argsBeforeDoubleDash(args []string) []string {
	if i := slices.Index(args, "--"); i >= 0 {
		return args[:i]
	}
	return args
}

// insertBeforeDoubleDash adds extra to args as options: before the first
// "--" if there is one, else at the end.
func insertBeforeDoubleDash(args, extra []string) []string {
	i := slices.Index(args, "--")
	if i < 0 {
		return append(append([]string(nil), args...), extra...)
	}
	out := append([]string(nil), args[:i]...)
	out = append(out, extra...)
	return append(out, args[i:]...)
}
