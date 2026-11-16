package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func scratchCodexHome(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// codexDevInstructionsForTest returns the decoded developer_instructions an
// argv sets, failing when it sets none or more than one.
func codexDevInstructionsForTest(t *testing.T, args []string) string {
	t.Helper()
	var found []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" && strings.HasPrefix(args[i+1], codexDeveloperInstructionsKey) {
			found = append(found, codexOverrideText(strings.TrimPrefix(args[i+1], codexDeveloperInstructionsKey)))
		}
	}
	if len(found) != 1 {
		t.Fatalf("argv sets developer_instructions %d times: %q", len(found), args)
	}
	return found[0]
}

func TestCodexPaneInstructionsTeam(t *testing.T) {
	home := scratchCodexHome(t)
	got := withCodexPaneInstructions([]string{"resume", "t1"}, true, "", home)
	if !reflect.DeepEqual(got[:2], []string{"resume", "t1"}) {
		t.Fatalf("argv order changed: %q", got)
	}
	if text := codexDevInstructionsForTest(t, got); text != mcpDirectServerInstructions {
		t.Fatalf("team pane instructions = %q", text)
	}
	withPack := codexDevInstructionsForTest(t, withCodexPaneInstructions(nil, true, "/pack", home))
	if withPack != mcpDirectServerInstructions+"\n\n"+codexAgentsContextPackLine {
		t.Fatalf("team pane with a pack = %q", withPack)
	}
}

// The orchestrator brief rides the same override; Codex keeps only the last
// one, so both are merged into a single value, ours first.
func TestCodexPaneInstructionsMergeTheOrchestratorBrief(t *testing.T) {
	home := scratchCodexHome(t)
	cmd := withCodexOrchestratorBrief([]string{"codex", "--no-alt-screen"})
	got := withCodexPaneInstructionsCmd(cmd, true, "/pack", home)
	text := codexDevInstructionsForTest(t, got)
	want := mcpDirectServerInstructions + "\n\n" + codexAgentsContextPackLine + "\n\n" + orchestratorBrief
	if text != want {
		t.Fatalf("merged instructions = %q", text)
	}
	if got[0] != "codex" || got[1] != "--no-alt-screen" {
		t.Fatalf("argv = %q", got)
	}
	// Idempotent: a second pass does not repeat anything.
	if again := codexDevInstructionsForTest(t, withCodexPaneInstructionsCmd(got, true, "/pack", home)); again != want {
		t.Fatalf("second pass = %q", again)
	}
	// A quoted override (the old context-pack form) and --config= decode too.
	for _, args := range [][]string{
		{"-c", `developer_instructions="keep \"this\""`},
		{"--config=developer_instructions=keep \"this\""},
		{`-cdeveloper_instructions="keep \"this\""`},
		{"-c=developer_instructions=keep \"this\""},
	} {
		text := codexDevInstructionsForTest(t, withCodexPaneInstructions(args, false, "/pack", home))
		if !strings.HasSuffix(text, "\n\nkeep \"this\"") || !strings.HasPrefix(text, codexAgentsContextPackLine) {
			t.Fatalf("%q merged to %q", args, text)
		}
	}
}

// A -c override replaces the user's own developer_instructions, so a Codex
// whose config.toml sets them is left alone.
func TestCodexPaneInstructionsRespectTheUsersOwn(t *testing.T) {
	home := scratchCodexHome(t)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"x\"\ndeveloper_instructions = \"mine\"\n\n[mcp_servers.a]\nurl = \"u\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"resume", "t1"}
	if got := withCodexPaneInstructions(args, true, "/pack", home); !reflect.DeepEqual(got, args) {
		t.Fatalf("user's developer_instructions overridden: %q", got)
	}
	// A profile's instructions count when that profile runs: chosen on the
	// command line, or the config's default profile.
	other := scratchCodexHome(t)
	if err := os.WriteFile(filepath.Join(other, "config.toml"), []byte("[profiles.work]\ndeveloper_instructions = \"p\"\n\n[profiles.\"play\"]\nmodel = \"m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, own := range [][]string{{"--profile", "work"}, {"-p", "work"}, {"-pwork"}, {"--profile=work", "resume", "t1"}} {
		if got := withCodexPaneInstructions(own, true, "", other); !reflect.DeepEqual(got, own) {
			t.Errorf("profile work's instructions overridden: %q", got)
		}
	}
	for _, mine := range [][]string{args, {"--profile", "play"}, {"--", "--profile", "work"}} {
		if got := withCodexPaneInstructions(mine, true, "", other); len(got) != len(mine)+2 {
			t.Errorf("%q: profile without instructions blocked the pane instructions: %q", mine, got)
		}
	}
	dflt := scratchCodexHome(t)
	if err := os.WriteFile(filepath.Join(dflt, "config.toml"), []byte("profile = \"work\"\n[profiles.work]\ndeveloper_instructions = \"p\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := withCodexPaneInstructions(args, true, "", dflt); !reflect.DeepEqual(got, args) {
		t.Errorf("default profile's instructions overridden: %q", got)
	}
}

// Everything after "--" is the prompt; the override goes before it.
func TestCodexPaneInstructionsStopAtDoubleDash(t *testing.T) {
	home := scratchCodexHome(t)
	args := []string{"--model", "x", "--", "-c", "developer_instructions=not an option"}
	got := withCodexPaneInstructions(args, true, "", home)
	want := append([]string{"--model", "x", "-c", codexDeveloperInstructionsKey + tomlBasicString(mcpDirectServerInstructions)}, args[2:]...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestCodexPaneInstructionsSkipManagementCommands(t *testing.T) {
	home := scratchCodexHome(t)
	for _, args := range [][]string{{"login"}, {"mcp", "list"}, {"--version"}, {"app-server"}, {"-h"}} {
		if got := withCodexPaneInstructions(args, true, "/pack", home); !reflect.DeepEqual(got, args) {
			t.Errorf("%q must be untouched, got %q", args, got)
		}
	}
	for _, cmd := range [][]string{{"bash"}, {"claude"}, {"codex", "app-server"}} {
		if got := withCodexPaneInstructionsCmd(cmd, true, "/pack", home); !reflect.DeepEqual(got, cmd) {
			t.Errorf("%q must be untouched, got %q", cmd, got)
		}
	}
}

func TestTOMLBasicStringRoundTrips(t *testing.T) {
	for _, s := range []string{mcpDirectServerInstructions, orchestratorBrief, codexAgentsContextPackLine, "a\"b\\c\n\td\x01—é"} {
		q := tomlBasicString(s)
		if !strings.HasPrefix(q, `"`) || strings.ContainsAny(q[1:len(q)-1], "\n\r\x01") {
			t.Fatalf("not a one-line basic string: %q", q)
		}
		if got := codexOverrideText(q); got != s {
			t.Fatalf("round trip = %q, want %q", got, s)
		}
	}
}

// The retired engine and the global file are gone from what agents are told.
func TestPaneInstructionsDropTheRetiredEngine(t *testing.T) {
	for name, text := range map[string]string{"mcp": mcpDirectServerInstructions, "orchestrator": orchestratorBrief} {
		if strings.Contains(text, "workflow task engine") || strings.Contains(text, "workflow tasks") {
			t.Errorf("%s instructions still mention the retired engine: %q", name, text)
		}
	}
	if strings.Contains(mcpDirectServerInstructions, ".Keep") {
		t.Error("missing space before Keep")
	}
}
