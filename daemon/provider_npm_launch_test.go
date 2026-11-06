package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNPMProviderShimRecognition(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name, body, target string
		node               bool
	}{
		{"codex.cmd", npmCodexNodeShim, "node_modules/@openai/codex/bin/codex.js", true},
		{"claude.cmd", npmClaudeNativeShim, "node_modules/@anthropic-ai/claude-code/bin/claude.exe", false},
		{"claude.cmd", strings.ReplaceAll(npmCodexNodeShim, `node_modules\@openai\codex\bin\codex.js`, `node_modules\@anthropic-ai\claude-code\cli.js`), "node_modules/@anthropic-ai/claude-code/cli.js", true},
		{"codex.cmd", "set ROUTE=custom\r\n" + npmCodexNodeShim, "", false},
		{"codex.cmd", strings.ReplaceAll(npmCodexNodeShim, "codex.js", "other.js"), "", false},
		{"codex.cmd", strings.Repeat("x", 16385), "", false},
		{"other.cmd", npmCodexNodeShim, "", false},
	} {
		bin := filepath.Join(root, tc.name)
		if err := os.WriteFile(bin, []byte(strings.ReplaceAll(tc.body, "\n", "\r\n")), 0600); err != nil {
			t.Fatal(err)
		}
		target, node := npmProviderShimEntrypoint(bin)
		want := ""
		if tc.target != "" {
			want = filepath.Join(root, filepath.FromSlash(tc.target))
		}
		if target != want || node != tc.node {
			t.Fatalf("%s: target %q node %t, want %q %t", tc.name, target, node, want, tc.node)
		}
	}
}

func TestNPMProviderPreservesLookupError(t *testing.T) {
	c := &exec.Cmd{Path: "codex.cmd", Args: []string{"codex.cmd"}, Err: exec.ErrDot}
	if err := prepareProviderExec(c); err != nil || c.Err != exec.ErrDot || c.Path != "codex.cmd" {
		t.Fatalf("lost LookPath security check: %v", err)
	}
}
