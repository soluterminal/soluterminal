package main

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	testLinuxBin = "/home/u/.local/lib/soluterminal/helper/solud-0.0.1250"
	testLinuxRun = "/run/user/1000/soluterminal-helper"
)

var paneIDInArgv = regexp.MustCompile(`pane_[0-9a-f]{16}`)

func testLinuxPaths(t *testing.T) linuxPaneAgentPaths {
	t.Helper()
	p, ok := linuxPaneAgentPathsFor(testLinuxBin, testLinuxRun)
	if !ok {
		t.Fatal("paths refused")
	}
	return p
}

// flagValue returns the value after flag in argv.
func linuxFlagValue(t *testing.T, argv []string, flag string) string {
	t.Helper()
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("%s missing from %q", flag, argv)
	}
	return argv[i+1]
}

// The injected config names the helper binary and socket paths only: no URL,
// no pane id, no token, nothing that points at this host (8.6a.6).
func assertNoHostOrIdentity(t *testing.T, argv []string) {
	t.Helper()
	all := strings.Join(argv, "\x00")
	for _, bad := range []string{"http", "127.0.0.1", "localhost", "SOLU_PANE_ID", "X-Soluterminal", "token", "Bearer"} {
		if strings.Contains(all, bad) {
			t.Errorf("injected argv contains %q", bad)
		}
	}
	if paneIDInArgv.MatchString(all) {
		t.Errorf("injected argv names a pane id")
	}
}

func TestLinuxClaudeInjectArgs(t *testing.T) {
	p := testLinuxPaths(t)
	argv := linuxPaneAgentCmd(p, []string{"claude", "--model", "opus"})
	if argv[0] != "claude" || argv[1] != "--model" || argv[2] != "opus" {
		t.Fatalf("user args changed: %q", argv)
	}
	var mcp struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(linuxFlagValue(t, argv, "--mcp-config")), &mcp); err != nil {
		t.Fatal(err)
	}
	s := mcp.MCPServers["soluterminal"]
	if s.Type != "stdio" || s.Command != testLinuxBin || !slices.Equal(s.Args, []string{"mcp-bridge", "--socket", testLinuxRun + "/mcp.sock"}) {
		t.Fatalf("mcp server %+v", s)
	}
	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(linuxFlagValue(t, argv, "--settings")), &settings); err != nil {
		t.Fatal(err)
	}
	want := "'" + testLinuxBin + "' hook-forward --socket '" + testLinuxRun + "/hooks.sock'"
	for _, ev := range []string{"PreToolUse", "PostToolUse", "SessionStart"} {
		hs := settings.Hooks[ev]
		if len(hs) != 1 || len(hs[0].Hooks) != 1 || hs[0].Hooks[0].Type != "command" || hs[0].Hooks[0].Command != want {
			t.Fatalf("%s hook %+v", ev, hs)
		}
	}
	if !slices.Contains(settings.Permissions.Allow, "mcp__soluterminal__list_panes") || slices.Contains(settings.Permissions.Allow, "mcp__soluterminal__send_input") {
		t.Fatalf("allow rules %q", settings.Permissions.Allow)
	}
	assertNoHostOrIdentity(t, argv)
}

func TestLinuxCodexInjectArgs(t *testing.T) {
	p := testLinuxPaths(t)
	argv := linuxPaneAgentCmd(p, []string{"/usr/local/bin/codex", "--model", "gpt"})
	all := strings.Join(argv, "\n")
	for _, want := range []string{
		`mcp_servers.soluterminal.command="` + testLinuxBin + `"`,
		`mcp_servers.soluterminal.args=["mcp-bridge","--socket","` + testLinuxRun + `/mcp.sock"]`,
		`mcp_servers.soluterminal.tool_timeout_sec=900`,
		`mcp_servers.soluterminal.tools.list_panes.approval_mode="approve"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %s", want)
		}
	}
	if argv[0] != "/usr/local/bin/codex" || argv[1] != "--model" {
		t.Fatalf("user args changed: %q", argv[:3])
	}
	assertNoHostOrIdentity(t, argv)
}

func TestLinuxPaneAgentCmdLeavesOthersAlone(t *testing.T) {
	p := testLinuxPaths(t)
	for _, cmd := range [][]string{
		nil,
		{"bash"},
		{"/bin/sh", "-c", "claude"},
		{"claude", "mcp", "list"},
		{"claude", "--version"},
		{"claude", "--strict-mcp-config"},
		{"codex", "login"},
		{"codex", "-c", "mcp_servers.soluterminal.url=x"},
	} {
		if got := linuxPaneAgentCmd(p, cmd); !slices.Equal(got, cmd) {
			t.Errorf("%q rewritten to %q", cmd, got)
		}
	}
	// A user's own --mcp-config or --settings is kept; only the other is added.
	got := linuxPaneAgentCmd(p, []string{"claude", "--mcp-config", "mine.json"})
	if linuxFlagValue(t, got, "--mcp-config") != "mine.json" || !slices.Contains(got, "--settings") {
		t.Fatalf("own --mcp-config: %q", got)
	}
}

func TestLinuxPaneAgentPathsRefuseUnsafe(t *testing.T) {
	for _, bad := range [][2]string{
		{"relative/solud", testLinuxRun},
		{testLinuxBin, "run/x"},
		{"/home/u/my dir/solud", testLinuxRun},
		{"/home/u/o'brien/solud", testLinuxRun},
		{testLinuxBin, "/run/$(id)"},
		{testLinuxBin, "/run/a/../b"},
		{"/home/u/solud\n", testLinuxRun},
		{testLinuxBin, "/run/`x`"},
	} {
		if _, ok := linuxPaneAgentPathsFor(bad[0], bad[1]); ok {
			t.Errorf("accepted %q %q", bad[0], bad[1])
		}
	}
}

// Injected flags go before a "--": what follows it is a prompt, kept as is,
// and a flag-like word in it neither moves nor suppresses the injection.
func TestLinuxInjectArgsBeforeDoubleDash(t *testing.T) {
	p := testLinuxPaths(t)
	got := linuxPaneAgentCmd(p, []string{"claude", "--model", "opus", "--", "-literal", "--settings"})
	dd := slices.Index(got, "--")
	if dd < 0 || !slices.Equal(got[dd:], []string{"--", "-literal", "--settings"}) {
		t.Fatalf("prompt changed: %q", got)
	}
	if i := slices.Index(got, "--mcp-config"); i < 0 || i > dd {
		t.Fatalf("--mcp-config not before --: %q", got)
	}
	if i := slices.Index(got, "--settings"); i < 0 || i > dd {
		t.Fatalf("--settings not before --: %q", got)
	}
	cx := linuxPaneAgentCmd(p, []string{"codex", "--", "-x", "--version"})
	dd = slices.Index(cx, "--")
	if dd < 0 || !slices.Equal(cx[dd:], []string{"--", "-x", "--version"}) || slices.Index(cx, "-c") > dd || slices.Index(cx, "-c") < 0 {
		t.Fatalf("codex: %q", cx)
	}
}
