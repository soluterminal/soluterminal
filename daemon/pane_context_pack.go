package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// contextPackNone is the create-pane sentinel that explicitly opts out of a
// workspace default. Context-pack paths must be absolute or home-prefixed, so
// the sentinel cannot collide with a valid pack directory.
const contextPackNone = "none"

const (
	contextPackInjected    = "injected"
	contextPackCwd         = "cwd"
	contextPackUnsupported = "unsupported"
	contextPackMissing     = "missing"
)

// contextPackInjection is deliberately path-only. The daemon never reads pack
// contents: the selected CLI is responsible for loading the briefing files.
type contextPackInjection struct {
	Dir      string
	Briefing string
	Status   string
}

// validContextPackDir validates the local, path-only contract for a context
// pack. A pack must contain at least one regular briefing file. Only os.Stat is
// used here so sensitive pack contents never enter daemon memory, state or logs.
func validContextPackDir(raw string) (abs string, err error) {
	// Check the wire value before trimming: a trailing newline is still a shell
	// control character and must not be normalized into a valid argv path.
	if strings.ContainsAny(raw, "'\r\n\x00") {
		return "", fmt.Errorf("path contains an unsupported character")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "~") {
		return "", fmt.Errorf("path must be absolute or start with ~")
	}

	abs = filepath.Clean(expandHome(raw))
	if !filepath.IsAbs(abs) {
		return abs, fmt.Errorf("path must resolve to an absolute directory")
	}
	info, statErr := os.Stat(abs)
	if statErr != nil {
		return abs, fmt.Errorf("directory is unavailable: %w", statErr)
	}
	if !info.IsDir() {
		return abs, fmt.Errorf("path is not a directory")
	}

	hasBriefing := false
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		briefInfo, briefErr := os.Stat(filepath.Join(abs, name))
		if briefErr == nil && briefInfo.Mode().IsRegular() {
			hasBriefing = true
			break
		}
	}
	if !hasBriefing {
		return abs, fmt.Errorf("directory must contain CLAUDE.md or AGENTS.md")
	}
	return abs, nil
}

const contextPackMarker = "[solu context pack]"

func contextPackPointer(dir string) string {
	return contextPackPointerForBriefing(dir, "AGENTS.md")
}

func contextPackPointerForBriefing(dir, briefing string) string {
	return contextPackMarker + " A local context pack is attached at " + dir + ". Before your first response, read " + dir + "/" + briefing + " and follow it. If another instruction already pointed you at this directory, it is the same pack — read it once."
}

// resolveContextPack classifies the adapter to apply without reading any pack
// content. A moved/deleted pack never blocks a respawn; it becomes a visible
// missing status and the pane starts unbriefed.
func resolveContextPack(dir, cwd, kind, agent string, cmd []string) contextPackInjection {
	if strings.TrimSpace(dir) == "" {
		return contextPackInjection{}
	}
	abs, err := validContextPackDir(dir)
	if err != nil {
		if abs == "" {
			abs = filepath.Clean(expandHome(strings.TrimSpace(dir)))
		}
		return contextPackInjection{Dir: abs, Status: contextPackMissing}
	}

	cleanCwd := filepath.Clean(expandHome(strings.TrimSpace(cwd)))
	if cleanCwd == abs || strings.HasPrefix(cleanCwd, abs+string(filepath.Separator)) {
		return contextPackInjection{Dir: abs, Status: contextPackCwd}
	}

	effective := effectiveAgentKind(agent)
	if effective == agentClaude && cmdStartsClaude(cmd) {
		briefing := "AGENTS.md"
		if info, statErr := os.Stat(filepath.Join(abs, "CLAUDE.md")); statErr == nil && info.Mode().IsRegular() {
			briefing = "CLAUDE.md"
		}
		return contextPackInjection{Dir: abs, Briefing: briefing, Status: contextPackInjected}
	}
	if effective == agentCodex && cmdStartsCodex(cmd) {
		briefing := "CLAUDE.md"
		if info, statErr := os.Stat(filepath.Join(abs, "AGENTS.md")); statErr == nil && info.Mode().IsRegular() {
			briefing = "AGENTS.md"
		}
		return contextPackInjection{Dir: abs, Briefing: briefing, Status: contextPackInjected}
	}
	return contextPackInjection{Dir: abs, Status: contextPackUnsupported}
}

func withoutContextPackParagraph(value string) string {
	paragraphs := strings.Split(value, "\n\n")
	kept := paragraphs[:0]
	for _, paragraph := range paragraphs {
		if strings.HasPrefix(paragraph, contextPackMarker) {
			continue
		}
		kept = append(kept, paragraph)
	}
	return strings.Join(kept, "\n\n")
}

// withContextPack decorates a bare Claude argv. Login-shell-wrapped replay
// commands are intentionally left byte-identical; clean respawn paths rebuild
// a bare argv and re-derive the adapter.
func withContextPack(cmd []string, inj contextPackInjection) []string {
	if inj.Status != contextPackInjected || !cmdStartsClaude(cmd) {
		return cmd
	}

	out := make([]string, 0, len(cmd)+4)
	appendPromptValue := -1
	for i := 0; i < len(cmd); i++ {
		if cmd[i] == "--add-dir" && i+1 < len(cmd) && cmd[i+1] == inj.Dir {
			i++
			continue
		}
		if cmd[i] == "--append-system-prompt" && i+1 < len(cmd) {
			cleaned := withoutContextPackParagraph(cmd[i+1])
			i++
			if cleaned == "" {
				continue
			}
			out = append(out, "--append-system-prompt", cleaned)
			if appendPromptValue < 0 {
				appendPromptValue = len(out) - 1
			}
			continue
		}
		out = append(out, cmd[i])
	}

	out = append(out, "--add-dir", inj.Dir)
	// Decision rule: --add-dir is a filesystem-access grant, not a sufficiently
	// stable cross-version guarantee that Claude will discover CLAUDE.md as
	// memory. Keep the explicit pointer for both briefing names unless a manual
	// side-port E2E proves discovery on every supported Claude CLI version.
	pointer := contextPackPointerForBriefing(inj.Dir, inj.Briefing)
	if appendPromptValue >= 0 {
		out[appendPromptValue] += "\n\n" + pointer
	} else {
		out = append(out, "--append-system-prompt", pointer)
	}
	return out
}

func spawnCmdForContextPack(cmd []string, inj contextPackInjection) []string {
	return withContextPack(cmd, inj)
}

func contextPackEnv(inj contextPackInjection) []string {
	if inj.Status != contextPackInjected && inj.Status != contextPackUnsupported {
		return nil
	}
	return []string{"SOLU_CONTEXT_PACK=" + inj.Dir}
}
