package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// Session titles run on the user's own Claude Code: the daemon spawns the
// installed, signed-in `claude -p` with no tools and Haiku. It never reads the
// user's Claude login token or calls Anthropic itself; Anthropic's terms let
// an app run the unmodified Claude Code binary on the user's subscription,
// but not intermediate their session tokens.

const titleCLITimeoutSeconds = 45

var errTitleCLIUnavailable = errors.New("claude code is not installed or not signed in")

// titleRunner is the test seam for the spawned CLI.
var titleRunner = runTitleCLI

func titlePrompt(convText string) string {
	return titleSystemTask + "\n\nConversation:\n" + convText
}

// titleCLIArgs keeps the run cheap and inert: no tools, no MCP servers, no
// CLAUDE.md or plugins, no saved session.
func titleCLIArgs() []string {
	return []string{
		"-p",
		"--safe-mode",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
		"--tools", "",
		"--permission-mode", "dontAsk",
		"--no-session-persistence",
		"--output-format", "text",
		"--model", "haiku",
		"--effort", "low",
	}
}

func runTitleCLI(ctx context.Context, prompt string) (string, error) {
	bin, err := resolveClaudeBin()
	if err != nil {
		return "", errTitleCLIUnavailable
	}
	args, cleanupConfig, err := claudeConfigFileArgs(bin, titleCLIArgs())
	if err != nil {
		return "", fmt.Errorf("Claude launch configuration: %w", err)
	}
	defer cleanupConfig()
	cmd := hiddenCommand(ctx, bin, args...)
	// A neutral directory: the title needs no project files.
	cmd.Dir = os.TempDir()
	cmd.Env = providerChildEnvForBin(bin, withResolvedNodeBin(os.Environ(), resolveNodeBin))
	// The prompt goes on stdin: -p rejects a value that starts with a dash.
	cmd.Stdin = strings.NewReader(prompt)
	out := &boundedBuf{limit: 16 << 10}
	cmd.Stdout = out
	cmd.Stderr = &boundedBuf{limit: 4 << 10}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude -p: %w", err)
	}
	return string(out.buf), nil
}

// generateTitleViaCLI asks the user's Claude Code for a short title.
func generateTitleViaCLI(ctx context.Context, convText string) (string, error) {
	raw, err := titleRunner(ctx, titlePrompt(convText))
	if err != nil {
		return "", err
	}
	title := sanitizeTitle(raw)
	if title == "" {
		return "", errors.New("title: empty result")
	}
	return title, nil
}

// titleSystemTask is the session-title instruction (pane_title_cli.go runs
// it on the user's own Claude Code).
const titleSystemTask = "Generate a concise title (max 6 words, no quotes, no trailing punctuation) describing what this coding session is currently working on. Reply with ONLY the title."

// sanitizeTitle normalizes a model-authored title for display: collapse all
// whitespace/newlines to single spaces, trim, strip a single pair of
// surrounding quotes, drop trailing punctuation, and clamp to ~60 chars (on a
// rune boundary). Pure + total — never errors, "" in → "" out.
func sanitizeTitle(s string) string {
	// Collapse any run of whitespace (incl. newlines/tabs) to one space.
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimSpace(s)
	// Strip one pair of matching surrounding quotes (straight or smart).
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			s = strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	s = strings.Trim(s, "“”‘’\"'")
	// Drop a trailing sentence-ending punctuation mark.
	s = strings.TrimRight(s, ".!?,;: ")
	s = strings.TrimSpace(s)
	// Clamp to ~60 chars on a rune boundary (don't split a multibyte rune).
	const maxLen = 60
	if len(s) > maxLen {
		s = s[:maxLen]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s = strings.TrimSpace(s)
	}
	return s
}
