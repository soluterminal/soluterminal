package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// useMCPRegistration gives the registration writers a backup directory and
// a decided socket state: socket "" means not serving; exe is the binary a
// live bridge entry must name. It returns the backup directory.
func useMCPRegistration(t *testing.T, socket, exe string) string {
	t.Helper()
	prevReg, prevExe := mcpSocketReg, mcpBridgeExecutable
	mcpSocketReg = &mcpSocketRegistration{ready: make(chan struct{})}
	mcpBridgeExecutable = func() string { return exe }
	t.Cleanup(func() { mcpSocketReg, mcpBridgeExecutable = prevReg, prevExe })
	data := t.TempDir()
	setMCPRegistrationDataDir(data)
	setMCPSocketServing(socket)
	return filepath.Join(data, "state", "mcp-registration")
}

// openRegistrationGates lets ensure*Registration run in a test even where the
// runner sets SOLU_SKIP_MCP_REGISTER=1 (the remote profiles do) or another
// test left an integration choice behind.
func openRegistrationGates(t *testing.T) {
	t.Helper()
	t.Setenv("SOLU_SKIP_MCP_REGISTER", "")
	prev := activeAgentIntegration.Load()
	activeAgentIntegration.Store(nil)
	t.Cleanup(func() { activeAgentIntegration.Store(prev) })
}

func backupFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

const (
	testBridgeExe    = "/home/u/.local/share/soluterminal/solud"
	testBridgeSocket = "/home/u/.solu/run/mcp.sock"
	testMCPURL       = "http://127.0.0.1:8787/mcp"
)

// codexAfterOldDaemon is config.toml as solud 0.0.1235.8 left it when it found
// a bridge table and no codex CLI: a second soluterminal table appended (and
// its tool sub-tables), which Codex refuses to load.
const codexAfterOldDaemon = `model = "gpt-5"
# keep this comment

[mcp_servers.other]
command = "/usr/bin/true"

[mcp_servers.soluterminal]
command = "` + testBridgeExe + `"
args = ["mcp-bridge", "--socket", "` + testBridgeSocket + `"]
tool_timeout_sec = 900

[mcp_servers.soluterminal.tools.list_panes]
approval_mode = "approve"

[mcp_servers.soluterminal]
url = "http://127.0.0.1:8787/mcp"
env_http_headers = { "X-Soluterminal-Pane" = "SOLU_PANE_ID" }

[mcp_servers.soluterminal.tools.get_pane_state]
approval_mode = "approve"
`

func TestCodexPlanRepairsTheOldDaemonsDuplicateTable(t *testing.T) {
	useMCPRegistration(t, "", "")
	if validateTOML([]byte(codexAfterOldDaemon)) == nil {
		t.Fatal("fixture should be invalid TOML")
	}
	next, entry, note, changed := codexPlanRegistration(codexAfterOldDaemon, testMCPURL)
	if !changed || !strings.Contains(note, "repaired 2") {
		t.Fatalf("changed=%t note=%q", changed, note)
	}
	if err := validateTOML([]byte(next)); err != nil {
		t.Fatalf("repaired config doesn't parse: %v\n%s", err, next)
	}
	if n := len(codexSoluterminalTables(strings.Split(next, "\n"))); n != 1 {
		t.Fatalf("%d soluterminal tables after repair:\n%s", n, next)
	}
	var cfg struct {
		McpServers map[string]map[string]any `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal([]byte(next), &cfg); err != nil {
		t.Fatal(err)
	}
	st := cfg.McpServers["soluterminal"]
	if st["url"] != testMCPURL || st["command"] != nil || st["tool_timeout_sec"] != int64(900) {
		t.Fatalf("kept table = %v", st)
	}
	tools, _ := st["tools"].(map[string]any)
	if tools["list_panes"] == nil || tools["get_pane_state"] == nil {
		t.Fatalf("tool sub-tables lost: %v", tools)
	}
	// Everything that isn't a soluterminal table is byte-identical.
	for _, keep := range []string{"model = \"gpt-5\"\n# keep this comment\n\n[mcp_servers.other]\ncommand = \"/usr/bin/true\"\n"} {
		if !strings.HasPrefix(next, keep) {
			t.Fatalf("other content changed:\n%s", next)
		}
	}
	if !strings.Contains(entry, "mcp-bridge") || strings.Contains(entry, "model") || strings.Contains(entry, "mcp_servers.other") {
		t.Fatalf("backup entry = %q", entry)
	}
	// Idempotent.
	if again, _, _, changed := codexPlanRegistration(next, testMCPURL); changed {
		t.Fatalf("second plan changed it again:\n%s", again)
	}
}

func TestCodexPlanBridgeEntry(t *testing.T) {
	bridge := "model = \"gpt-5\"\n\n[mcp_servers.soluterminal]\ncommand = \"" + testBridgeExe + "\"\nargs = [\"mcp-bridge\", \"--socket\", \"" + testBridgeSocket + "\"]\ntool_timeout_sec = 900\n\n[mcp_servers.after]\nurl = \"http://x\"\n"
	t.Run("live bridge entry stays", func(t *testing.T) {
		useMCPRegistration(t, testBridgeSocket, testBridgeExe)
		if _, _, _, changed := codexPlanRegistration(bridge, testMCPURL); changed {
			t.Fatal("changed a live bridge entry")
		}
	})
	for name, reg := range map[string][2]string{
		"socket not serving": {"", testBridgeExe},
		"another socket":     {"/elsewhere/run/mcp.sock", testBridgeExe},
		"another binary":     {testBridgeSocket, "/opt/solud"},
	} {
		t.Run(name+": back to HTTP", func(t *testing.T) {
			useMCPRegistration(t, reg[0], reg[1])
			next, _, _, changed := codexPlanRegistration(bridge, testMCPURL)
			if !changed {
				t.Fatal("unchanged")
			}
			want := "model = \"gpt-5\"\n\n[mcp_servers.soluterminal]\nurl = \"" + testMCPURL + "\"\n" + codexMcpPaneHeaderLine + "\ntool_timeout_sec = 900\n\n[mcp_servers.after]\nurl = \"http://x\"\n"
			if next != want {
				t.Fatalf("got\n%s\nwant\n%s", next, want)
			}
		})
	}
	t.Run("someone else's stdio entry stays", func(t *testing.T) {
		useMCPRegistration(t, "", "")
		foreign := "[mcp_servers.soluterminal]\ncommand = \"/usr/local/bin/my-proxy\"\nargs = [\"--x\"]\n"
		if _, _, _, changed := codexPlanRegistration(foreign, testMCPURL); changed {
			t.Fatal("changed a foreign entry")
		}
		dup := foreign + "\n" + foreign
		if _, _, _, changed := codexPlanRegistration(dup, testMCPURL); changed {
			t.Fatal("touched duplicate tables that aren't ours")
		}
	})
}

func TestCodexConfigWriteKeepsModeBacksUpAndRefusesUnparseable(t *testing.T) {
	backups := useMCPRegistration(t, "", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(codexAfterOldDaemon), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	next, _, _, _ := codexPlanRegistration(codexAfterOldDaemon, testMCPURL)
	if err := writeCodexConfig(path, codexAfterOldDaemon, next); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != next {
		t.Fatal("not written")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	files := backupFiles(t, backups)
	if len(files) != 1 {
		t.Fatalf("backups %v", files)
	}
	fi, _ := os.Stat(files[0])
	b, _ := os.ReadFile(files[0])
	if fi.Mode().Perm() != 0o600 || !strings.Contains(string(b), "mcp-bridge") || strings.Contains(string(b), "model") {
		t.Fatalf("backup %v:\n%s", fi.Mode().Perm(), b)
	}

	// A result that wouldn't parse leaves the file exactly as it was.
	before, _ := os.ReadFile(path)
	if err := writeCodexConfig(path, string(before), string(before)+"\n[mcp_servers.other]\nx = 1\n"); err == nil {
		t.Fatal("wrote unparseable TOML")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("file changed")
	}
	if _, err := os.Stat(path + ".solu-tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}

// A config that is already invalid for reasons of its own is never rewritten.
func TestEnsureCodexLeavesAnUnparseableConfigAlone(t *testing.T) {
	openRegistrationGates(t)
	useMCPRegistration(t, "", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	body := "[mcp_servers.x]\na = 1\n[mcp_servers.x]\na = 2\n\n[mcp_servers.soluterminal]\ncommand = \"" + testBridgeExe + "\"\nargs = [\"mcp-bridge\", \"--socket\", \"" + testBridgeSocket + "\"]\n"
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ensureCodexMcpRegistration(":8787")
	if got, _ := os.ReadFile(path); string(got) != body {
		t.Fatalf("config changed:\n%s", got)
	}
}

// Someone else's stdio soluterminal entry: no second table is appended next
// to it (the bug that breaks Codex when an older daemon meets a bridge table).
func TestEnsureCodexNeverAppendsBesideAnExistingTable(t *testing.T) {
	openRegistrationGates(t)
	useMCPRegistration(t, "", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	body := "[mcp_servers.soluterminal]\ncommand = \"/usr/local/bin/my-proxy\"\nargs = [\"--x\"]\n"
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ensureCodexMcpRegistration(":8787")
	if got, _ := os.ReadFile(path); string(got) != body {
		t.Fatalf("config changed:\n%s", got)
	}
}

func TestJSONSetNestedMemberChangesOnlyThatMember(t *testing.T) {
	val := []byte(`{"type":"http"}`)
	for _, tc := range []struct{ in, want string }{
		{`{"a": 1, "mcpServers": {"x": {"k": [1, 2]}, "soluterminal": {"type": "stdio", "command": "c"}}, "z": "s"}`,
			`{"a": 1, "mcpServers": {"x": {"k": [1, 2]}, "soluterminal": {"type":"http"}}, "z": "s"}`},
		{"{\n  \"mcpServers\": {\n    \"x\": {}\n  }\n}\n", "{\n  \"mcpServers\": {\n    \"x\": {}, \"soluterminal\": {\"type\":\"http\"}\n  }\n}\n"},
		{`{"mcpServers": {}}`, `{"mcpServers": {"soluterminal": {"type":"http"}}}`},
		{`{"a": 1}`, `{"a": 1, "mcpServers": {"soluterminal": {"type":"http"}}}`},
		{`{}`, `{"mcpServers": {"soluterminal": {"type":"http"}}}`},
	} {
		got, err := jsonSetNestedMember([]byte(tc.in), "mcpServers", "soluterminal", val)
		if err != nil || string(got) != tc.want {
			t.Fatalf("%s:\ngot  %s (%v)\nwant %s", tc.in, got, err, tc.want)
		}
		if validateJSONObject(got) != nil {
			t.Fatalf("invalid JSON %s", got)
		}
	}
}

func TestClaudePlanBridgeEntry(t *testing.T) {
	bridge := `{"numStartups": 5, "mcpServers": {"other": {"type": "stdio", "command": "/usr/bin/true"}, "soluterminal": {"type": "stdio", "command": "` + testBridgeExe + `", "args": ["mcp-bridge", "--socket", "` + testBridgeSocket + `"], "env": {}}}, "projects": {}}`
	t.Run("live bridge entry stays", func(t *testing.T) {
		useMCPRegistration(t, testBridgeSocket, testBridgeExe)
		if _, handled, changed, err := claudePlanBridgeEntry([]byte(bridge), testMCPURL); err != nil || !handled || changed {
			t.Fatalf("handled=%t changed=%t err=%v", handled, changed, err)
		}
	})
	t.Run("socket not serving: back to HTTP, nothing else changes", func(t *testing.T) {
		useMCPRegistration(t, "", testBridgeExe)
		next, handled, changed, err := claudePlanBridgeEntry([]byte(bridge), testMCPURL)
		if err != nil || !handled || !changed {
			t.Fatalf("handled=%t changed=%t err=%v", handled, changed, err)
		}
		want := `{"numStartups": 5, "mcpServers": {"other": {"type": "stdio", "command": "/usr/bin/true"}, "soluterminal": ` + string(claudeHTTPEntryJSON(testMCPURL)) + `}, "projects": {}}`
		if string(next) != want {
			t.Fatalf("got  %s\nwant %s", next, want)
		}
	})
	t.Run("someone else's stdio entry stays", func(t *testing.T) {
		useMCPRegistration(t, "", "")
		foreign := `{"mcpServers": {"soluterminal": {"type": "stdio", "command": "/usr/local/bin/my-proxy", "args": ["mcp-bridge"]}}}`
		if _, handled, changed, _ := claudePlanBridgeEntry([]byte(foreign), testMCPURL); !handled || changed {
			t.Fatalf("handled=%t changed=%t", handled, changed)
		}
	})
	t.Run("an HTTP entry is the URL policy's", func(t *testing.T) {
		useMCPRegistration(t, "", "")
		http := `{"mcpServers": {"soluterminal": {"type": "http", "url": "` + testMCPURL + `"}}}`
		if _, handled, _, _ := claudePlanBridgeEntry([]byte(http), testMCPURL); handled {
			t.Fatal("handled an HTTP entry")
		}
	})
}

func TestEnsureClaudeRestoresABridgeEntryOnce(t *testing.T) {
	openRegistrationGates(t)
	backups := useMCPRegistration(t, "", testBridgeExe)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	body := `{"numStartups": 5, "mcpServers": {"soluterminal": {"type": "stdio", "command": "` + testBridgeExe + `", "args": ["mcp-bridge", "--socket", "` + testBridgeSocket + `"]}}, "projects": {}}`
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ensureClaudeMcpRegistration(":8787")
	got, _ := os.ReadFile(path)
	if url, hasHeader, _ := currentClaudeMcpURL(); url != testMCPURL || !hasHeader {
		t.Fatalf("not restored: %s", got)
	}
	if !strings.HasPrefix(string(got), `{"numStartups": 5, "mcpServers": {"soluterminal": `) || !strings.HasSuffix(string(got), `}, "projects": {}}`) {
		t.Fatalf("other bytes changed: %s", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if files := backupFiles(t, backups); len(files) != 1 {
		t.Fatalf("backups %v", files)
	}
	// Idempotent: the HTTP entry it wrote is kept, with no second backup.
	ensureClaudeMcpRegistration(":8787")
	if again, _ := os.ReadFile(path); string(again) != string(got) {
		t.Fatalf("second run changed it: %s", again)
	}
	if files := backupFiles(t, backups); len(files) != 1 {
		t.Fatalf("backups %v", files)
	}
}
