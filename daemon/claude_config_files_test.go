package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestClaudeConfigFilesPreserveInjection(t *testing.T) {
	original := claudeInjectArgs(testInjectionRecord(), "pane_private_header", nil)
	before := append([]string(nil), original...)
	args, cleanup, err := fileClaudeJSONArgs(original)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var dir string
	for _, flag := range []string{"--settings", "--mcp-config"} {
		path := injectFlagValue(args, flag)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != injectFlagValue(original, flag) {
			t.Fatalf("%s lost hooks, permissions or MCP header", flag)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("file permissions = %o", info.Mode().Perm())
		}
		dir = filepath.Dir(path)
	}
	if !reflect.DeepEqual(original, before) {
		t.Fatal("mutated original argv")
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("config directory remains after cleanup: %v", err)
	}
}

func TestClaudeConfigFilesUserFlagsAndIsolation(t *testing.T) {
	args := []string{"--settings=mine.json", "--mcp-config", "user.json", "--strict-mcp-config", "--settings", "not valid JSON", "--", "--settings", `{ "prompt": true }`}
	got, cleanup, err := fileClaudeJSONArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !reflect.DeepEqual(got, args) {
		t.Fatalf("changed user flags/paths or prompt: %v", got)
	}
	// The engine's empty strict MCP config also needs to survive batch parsing.
	const emptyMCPConfig = `{"mcpServers":{}}`
	got, cleanupStrict, err := fileClaudeJSONArgs([]string{"--mcp-config=" + emptyMCPConfig, "--strict-mcp-config"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupStrict()
	path := strings.TrimPrefix(got[0], "--mcp-config=")
	b, err := os.ReadFile(path)
	if err != nil || string(b) != emptyMCPConfig || got[1] != "--strict-mcp-config" {
		t.Fatalf("strict MCP config changed: %q, %v", b, err)
	}
}

func TestClaudeConfigFilesNativeUnchanged(t *testing.T) {
	args := claudeInjectArgs(testInjectionRecord(), "p1", nil)
	bins := []string{"claude.exe", "codex.cmd"}
	if runtime.GOOS != "windows" {
		bins = append(bins, "claude", "claude.cmd")
	}
	for _, bin := range bins {
		got, cleanup, err := claudeConfigFileArgs(bin, args)
		cleanup()
		if err != nil || !reflect.DeepEqual(got, args) {
			t.Fatalf("%s: changed native argv: %v", bin, err)
		}
	}
}

func TestClaudeConfigFilesFailClosed(t *testing.T) {
	badTemp := filepath.Join(t.TempDir(), "missing", "temp")
	t.Setenv("TMPDIR", badTemp)
	t.Setenv("TMP", badTemp)
	t.Setenv("TEMP", badTemp)
	args, cleanup, err := fileClaudeJSONArgs([]string{"--settings", `{"hooks":{}}`})
	cleanup()
	if err == nil || args != nil {
		t.Fatal("could continue without the requested configuration")
	}
}
