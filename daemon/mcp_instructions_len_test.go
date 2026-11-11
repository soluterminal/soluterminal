package main

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// Claude Code cuts MCP server instructions longer than 4096 characters
// (JavaScript string length, i.e. UTF-16 units) and appends "[truncated]", so
// anything past the cap never reaches the pane. New rules belong in AGENTS.md
// unless something else here can be shortened to make room.
func TestMCPInstructionsFitClaudeCodeCap(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{"direct": mcpDirectServerInstructions, "public": mcpPublicServerInstructions} {
		if got := len(utf16.Encode([]rune(text))); got > 4096 {
			t.Errorf("%s instructions are %d characters; Claude Code truncates past 4096", name, got)
		}
	}
	if !strings.Contains(mcpDirectServerInstructions, "no acks, FYIs or relays") {
		t.Error("direct instructions lost the coordination-cost rule")
	}
}
