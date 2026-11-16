package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
)

// Instructions for the Codex sessions SoluTerminal starts.
//
// They used to live in a marked block in the global ~/.codex/AGENTS.md, which
// every Codex session on the computer reads, including sessions in unrelated
// repositories and terminals. They now reach only Codex processes that
// SoluTerminal launches (a pane's own `codex`, the live bridge, and `codex`
// typed in a pane's shell through the pane shim), as a per-process
// `-c developer_instructions=…` override, the same transport the orchestrator
// brief uses (pane_role_host.go). The MCP server's own instructions are not
// enough on their own: Codex does not put them in the model's prompt (checked
// with `codex debug prompt-input` on codex-cli 0.164.0).
//
// The block an earlier version wrote is removed from ~/.codex/AGENTS.md on the
// next registration pass (codex_agents_md.go).

// codexPanePolicyTeam selects the team brief (mcpDirectServerInstructions).
// Public installs only get the context-pack note, and only when a pack is
// attached.
var codexPanePolicyTeam atomic.Bool

// codexPanePolicyEnv tells the pane shim (a separate process) that the team
// brief applies. Spawn sets it on team installs.
const codexPanePolicyEnv = "SOLU_CODEX_PANE_POLICY"

const codexAgentsContextPackLine = "Context packs: if the environment variable SOLU_CONTEXT_PACK is set, it names a local directory holding a briefing for this session — before your first response, read its AGENTS.md (or CLAUDE.md if AGENTS.md is absent) and follow it, in addition to any AGENTS.md discovered from the working directory. If another instruction already pointed you at that same directory, it is the same pack — read it once."

// codexUserDeveloperInstructionsRe and codexProfileKeyRe read the two keys
// that matter here from config.toml without a TOML parser.
var (
	codexUserDeveloperInstructionsRe = regexp.MustCompile(`^\s*developer_instructions\s*=`)
	codexProfileKeyRe                = regexp.MustCompile(`^\s*profile\s*=\s*["']([^"']*)["']`)
)

// codexArgProfile is the profile a Codex argv selects (--profile/-p), "" for
// none. Parsing stops at "--".
func codexArgProfile(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if (a == "--profile" || a == "-p") && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--profile="); ok {
			return v
		}
		if v, ok := strings.CutPrefix(a, "-p"); ok && v != "" && !strings.HasPrefix(v, "-") {
			return strings.TrimPrefix(v, "=")
		}
	}
	return ""
}

// codexUserSetsDeveloperInstructions reports whether the user's own Codex
// config gives this session developer_instructions: at the top level, or in
// the profile the session runs (argProfile, else the config's default
// `profile`). A -c override replaces that value outright, so SoluTerminal
// leaves such a session alone rather than silently drop the user's own
// instructions. codexHome is the pane's CODEX_HOME ("" for ~/.codex).
func codexUserSetsDeveloperInstructions(codexHome, argProfile string) bool {
	path := ""
	if h := strings.TrimSpace(codexHome); h != "" {
		path = filepath.Join(h, "config.toml")
	} else if p, err := codexConfigPath(); err == nil {
		path = p
	}
	if path == "" {
		return false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	table, defaultProfile := "", ""
	profilesWithOwn := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		if m := tomlTableRe.FindStringSubmatch(line); m != nil {
			table = strings.TrimSpace(m[1])
			continue
		}
		if table == "" {
			if codexUserDeveloperInstructionsRe.MatchString(line) {
				return true
			}
			if m := codexProfileKeyRe.FindStringSubmatch(line); m != nil {
				defaultProfile = m[1]
			}
			continue
		}
		if name, ok := strings.CutPrefix(table, "profiles."); ok && codexUserDeveloperInstructionsRe.MatchString(line) {
			profilesWithOwn[strings.Trim(name, `"'`)] = true
		}
	}
	active := argProfile
	if active == "" {
		active = defaultProfile
	}
	return active != "" && profilesWithOwn[active]
}

// codexPaneInstructions is what a SoluTerminal Codex session is told, before
// any instructions its own argv already carries (an orchestrator brief).
func codexPaneInstructions(team bool, contextPack string) []string {
	var parts []string
	if team {
		parts = append(parts, mcpDirectServerInstructions)
	}
	if strings.TrimSpace(contextPack) != "" {
		parts = append(parts, codexAgentsContextPackLine)
	}
	return parts
}

// withCodexPaneInstructions adds the pane instructions to a Codex argv
// (without the binary). A developer_instructions override already in the argv
// (the orchestrator brief) is kept and merged after ours into one override,
// since Codex keeps only the last. With no override in the argv and one in the
// user's config.toml (top level or the session's profile), the argv is left
// as is. The override goes before a "--".
func withCodexPaneInstructions(args []string, team bool, contextPack, codexHome string) []string {
	if len(args) > 0 && codexSkipInjectSubcommands[args[0]] {
		return args
	}
	if cliArgsHasAny(args, "-V", "--version", "-h", "--help") {
		return args
	}
	parts := codexPaneInstructions(team, contextPack)
	if len(parts) == 0 {
		return args
	}
	// Options end at "--"; everything after it is the prompt and stays put,
	// after the override.
	opts, rest := args, []string(nil)
	if k := slices.Index(args, "--"); k >= 0 {
		opts, rest = args[:k], args[k:]
	}
	out := make([]string, 0, len(args)+2)
	var existing []string
	for i := 0; i < len(opts); i++ {
		a := opts[i]
		if (a == "-c" || a == "--config") && i+1 < len(opts) && strings.HasPrefix(opts[i+1], codexDeveloperInstructionsKey) {
			existing = append(existing, codexOverrideText(strings.TrimPrefix(opts[i+1], codexDeveloperInstructionsKey)))
			i++
			continue
		}
		if v, ok := codexInlineDeveloperInstructions(a); ok {
			existing = append(existing, codexOverrideText(v))
			continue
		}
		out = append(out, a)
	}
	if len(existing) == 0 && codexUserSetsDeveloperInstructions(codexHome, codexArgProfile(opts)) {
		return args
	}
	ours := len(parts)
	for _, e := range existing {
		// A command decorated before (a respawn) already carries our parts;
		// take them out so they are not repeated.
		for _, p := range parts[:ours] {
			e = strings.ReplaceAll(e, p, "")
		}
		if e = strings.TrimSpace(e); e != "" && !slices.Contains(parts, e) {
			parts = append(parts, e)
		}
	}
	out = append(out, "-c", codexDeveloperInstructionsKey+tomlBasicString(strings.Join(parts, "\n\n")))
	return append(out, rest...)
}

// codexInlineDeveloperInstructions reads the one-argument forms of a
// developer_instructions override: --config=…, -c=… and the compact -c….
func codexInlineDeveloperInstructions(a string) (string, bool) {
	for _, prefix := range []string{"--config=", "-c=", "-c"} {
		if v, ok := strings.CutPrefix(a, prefix+codexDeveloperInstructionsKey); ok {
			return v, true
		}
	}
	return "", false
}

// withCodexPaneInstructionsCmd is withCodexPaneInstructions for a full pane
// command (binary first); anything that does not start Codex is untouched.
func withCodexPaneInstructionsCmd(cmd []string, team bool, contextPack, codexHome string) []string {
	if !cmdStartsCodex(cmd) || (len(cmd) >= 2 && cmd[1] == "app-server") {
		return cmd
	}
	return append([]string{cmd[0]}, withCodexPaneInstructions(cmd[1:], team, contextPack, codexHome)...)
}

// codexOverrideText decodes a -c value the way Codex does: TOML when it parses
// as a string, the raw text otherwise.
func codexOverrideText(v string) string {
	switch {
	case strings.HasPrefix(v, `"`):
		if s, err := strconv.Unquote(v); err == nil {
			return s
		}
	case len(v) >= 2 && strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") && !strings.Contains(v[1:len(v)-1], "'"):
		return v[1 : len(v)-1]
	}
	return v
}

// tomlBasicString quotes s as a TOML basic string, so Codex reads the -c value
// back as exactly s whatever it starts with.
func tomlBasicString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
