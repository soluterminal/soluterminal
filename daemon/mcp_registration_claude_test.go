package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ourClaudeHTTPEntry = `{"type": "http", "url": "http://127.0.0.1:8787/mcp", "headers": {"X-Soluterminal-Pane": "${SOLU_PANE_ID}"}}`

func claudeFileWith(entry string) string {
	return `{"numStartups": 5, "mcpServers": {"other": {"type": "stdio", "command": "/usr/bin/true"}, "soluterminal": ` + entry + `}, "projects": {}}`
}

func claudeEntryOf(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var cfg struct {
		McpServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return cfg.McpServers["soluterminal"]
}

func TestClaudeMigrationMovesOnlyOurExactHTTPEntry(t *testing.T) {
	want := claudeFileWith(string(claudeBridgeEntryJSON(testBridgeExe, testBridgeSocket)))
	for _, tc := range []struct {
		name    string
		entry   string
		migrate bool
	}{
		{"ours with the pane header", ourClaudeHTTPEntry, true},
		{"ours from before the pane header", `{"type": "http", "url": "http://127.0.0.1:8787/mcp"}`, true},
		{"localhost spelling", `{"type": "http", "url": "http://localhost:8787/mcp", "headers": {"X-Soluterminal-Pane": "${SOLU_PANE_ID}"}}`, true},
		{"another header", `{"type": "http", "url": "http://127.0.0.1:8787/mcp", "headers": {"Authorization": "Bearer x"}}`, false},
		{"an extra key", `{"type": "http", "url": "http://127.0.0.1:8787/mcp", "timeout": 5}`, false},
		{"another port", `{"type": "http", "url": "http://127.0.0.1:38787/mcp"}`, false},
		{"another host", `{"type": "http", "url": "http://10.0.0.2:8787/mcp"}`, false},
		{"sse", `{"type": "sse", "url": "http://127.0.0.1:8787/mcp"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useMCPRegistration(t, testBridgeSocket, testBridgeExe)
			next, changed, err := claudePlanBridgeMigration([]byte(claudeFileWith(tc.entry)), testMCPURL)
			if err != nil || changed != tc.migrate {
				t.Fatalf("changed=%t err=%v", changed, err)
			}
			if changed && string(next) != want {
				t.Fatalf("got  %s\nwant %s", next, want)
			}
		})
	}
	t.Run("socket not serving", func(t *testing.T) {
		useMCPRegistration(t, "", testBridgeExe)
		if _, changed, _ := claudePlanBridgeMigration([]byte(claudeFileWith(ourClaudeHTTPEntry)), testMCPURL); changed {
			t.Fatal("migrated without a serving socket")
		}
	})
	t.Run("binary not under its installed name", func(t *testing.T) {
		useMCPRegistration(t, testBridgeSocket, "")
		if _, changed, _ := claudePlanBridgeMigration([]byte(claudeFileWith(ourClaudeHTTPEntry)), testMCPURL); changed {
			t.Fatal("migrated without a stable binary path")
		}
	})
}

func TestEnsureClaudeMigratesOnceAndTheSwitchMovesItBack(t *testing.T) {
	backups := useMCPRegistration(t, testBridgeSocket, testBridgeExe)
	openRegistrationGates(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte(claudeFileWith(ourClaudeHTTPEntry)), 0o600); err != nil {
		t.Fatal(err)
	}
	ensureClaudeMcpRegistration(":8787")
	got, _ := os.ReadFile(path)
	if e := claudeEntryOf(t, got); e["type"] != "stdio" || e["command"] != testBridgeExe {
		t.Fatalf("not migrated: %s", got)
	}
	if !legacyAgentRegistrationPresent() {
		t.Fatal("a bridge entry doesn't count as an earlier registration")
	}
	// Idempotent: a live bridge entry stays, with no second backup.
	ensureClaudeMcpRegistration(":8787")
	if again, _ := os.ReadFile(path); string(again) != string(got) {
		t.Fatalf("second run changed it: %s", again)
	}
	if files := backupFiles(t, backups); len(files) != 1 {
		t.Fatalf("backups %v", files)
	}
	// SOLU_MCP_BRIDGE_REGISTER=0: back to HTTP, and it stays there.
	t.Setenv("SOLU_MCP_BRIDGE_REGISTER", "0")
	ensureClaudeMcpRegistration(":8787")
	back, _ := os.ReadFile(path)
	if e := claudeEntryOf(t, back); e["type"] != "http" || e["url"] != testMCPURL {
		t.Fatalf("switch didn't move it back: %s", back)
	}
	ensureClaudeMcpRegistration(":8787")
	if again, _ := os.ReadFile(path); string(again) != string(back) {
		t.Fatalf("switch run changed it again: %s", again)
	}
}

// Uninstall removes our bridge entry but not a stdio entry someone else
// wrote under the same name.
func TestUninstallRecognisesOurBridgeEntry(t *testing.T) {
	for _, tc := range []struct {
		name, entry string
		removed     bool
	}{
		{"our bridge entry", string(claudeBridgeEntryJSON(testBridgeExe, testBridgeSocket)), true},
		{"someone else's stdio entry", `{"type": "stdio", "command": "/usr/local/bin/my-proxy", "args": []}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, dataDir, _ := isolateAgentHome(t)
			path := filepath.Join(home, ".claude.json")
			writeTestFile(t, path, claudeFileWith(tc.entry))
			res := removeClaudeMcpServer(home, agentIntegrationBackupDir(dataDir), agentIntegrationOwner{MCPURL: testMCPURL, MCPSocket: testBridgeSocket})
			if res.Error != "" {
				t.Fatalf("%+v", res)
			}
			_, present := claudeEntryOf(t, []byte(readTestFile(t, path)))["type"]
			if present == tc.removed {
				t.Fatalf("removed=%t, result %q", !present, res.Result)
			}
		})
	}
}

// Turning integration off in one daemon removes only a bridge entry naming
// its own socket; another install's bridge entry stays. Uninstall (no owner)
// removes ours wherever it points.
func TestTurningOffRemovesOnlyThisInstallsBridgeEntry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		owner   agentIntegrationOwner
		removed bool
	}{
		{"this install's socket", agentIntegrationOwner{MCPURL: testMCPURL, MCPSocket: testBridgeSocket}, true},
		{"another install's socket", agentIntegrationOwner{MCPURL: "http://127.0.0.1:18890/mcp", MCPSocket: "/home/u/side/run/mcp.sock"}, false},
		{"uninstall", agentIntegrationOwner{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, dataDir, _ := isolateAgentHome(t)
			path := filepath.Join(home, ".claude.json")
			writeTestFile(t, path, claudeFileWith(string(claudeBridgeEntryJSON(testBridgeExe, testBridgeSocket))))
			res := removeClaudeMcpServer(home, agentIntegrationBackupDir(dataDir), tc.owner)
			if res.Error != "" {
				t.Fatalf("%+v", res)
			}
			_, present := claudeEntryOf(t, []byte(readTestFile(t, path)))["type"]
			if present == tc.removed {
				t.Fatalf("removed=%t, result %q", !present, res.Result)
			}
		})
	}
}

// Each attempt of the direct removal checks ownership again: an entry that
// became another install's bridge by the time of a retry stays.
func TestRemovalRechecksOwnershipOnEveryAttempt(t *testing.T) {
	owner := agentIntegrationOwner{MCPURL: testMCPURL, MCPSocket: testBridgeSocket}
	edit := claudeRemoveOwnedServer(owner)
	theirs := claudeFileWith(string(claudeBridgeEntryJSON(testBridgeExe, "/home/u/side/run/mcp.sock")))
	if out, changed, ok := edit(theirs); !ok || changed || out != theirs {
		t.Fatalf("removed another install's bridge: changed=%t ok=%t", changed, ok)
	}
	ours := claudeFileWith(string(claudeBridgeEntryJSON(testBridgeExe, testBridgeSocket)))
	if _, changed, ok := edit(ours); !ok || !changed {
		t.Fatalf("didn't remove our own bridge: changed=%t ok=%t", changed, ok)
	}
	foreign := claudeFileWith(`{"type": "stdio", "command": "/usr/local/bin/my-proxy"}`)
	if _, changed, _ := edit(foreign); changed {
		t.Fatal("removed someone else's stdio entry")
	}
}

// The HTTP fallback never overwrites a stdio entry that appeared after the
// decision to write it.
func TestClaudeDirectWriteLeavesANewStdioEntryAlone(t *testing.T) {
	useMCPRegistration(t, "", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude.json")
	theirs := `{"mcpServers": {"soluterminal": {"type": "stdio", "command": "/usr/local/bin/my-proxy"}}}`
	if err := os.WriteFile(path, []byte(theirs), 0o600); err != nil {
		t.Fatal(err)
	}
	registerClaudeMcpDirect(testMCPURL, "add", "", false, "test")
	if b, _ := os.ReadFile(path); string(b) != theirs {
		t.Fatalf("stdio entry overwritten: %s", b)
	}
}

// LOW (a): a symlinked config stays a symlink; the write goes to its target.
func TestConfigWriteFollowsASymlinkedConfig(t *testing.T) {
	useMCPRegistration(t, "", "")
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "claude.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	before := []byte(claudeFileWith(`{"type": "http", "url": "http://0.0.0.0:8787/mcp"}`))
	if err := os.WriteFile(target, before, 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".claude.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := updateClaudeConfig(link, func(data []byte) ([]byte, bool, error) {
		next, err := jsonSetNestedMember(data, "mcpServers", "soluterminal", claudeHTTPEntryJSON(testMCPURL))
		return next, err == nil, err
	}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced: %v %v", fi.Mode(), err)
	}
	got, _ := os.ReadFile(target)
	if e := claudeEntryOf(t, got); e["url"] != testMCPURL {
		t.Fatalf("target not updated: %s", got)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o640 {
		t.Fatalf("target mode %v", fi.Mode().Perm())
	}
}

// LOW (b): the temporary file is created exclusively under a fresh name; a
// file or link planted at the old fixed name is neither followed nor used.
func TestConfigWriteCreatesItsTempFileExclusively(t *testing.T) {
	useMCPRegistration(t, "", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "model = \"gpt-5\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path+".solu-tmp"); err != nil {
		t.Fatal(err)
	}
	next := body + "\n[mcp_servers.soluterminal]\nurl = \"" + testMCPURL + "\"\n"
	if err := writeCodexConfig(path, body, next); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Fatal("wrote through the planted link")
	}
	if got, _ := os.ReadFile(path); string(got) != next {
		t.Fatal("config not written")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".solu-tmp-") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

// LOW (c): a write by another program between our read and our rename is not
// lost. The write that would overwrite it is refused, and the update starts
// over from the new contents.
func TestConfigWriteDoesNotLoseAConcurrentWrite(t *testing.T) {
	useMCPRegistration(t, "", "")
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	original := claudeFileWith(`{"type": "http", "url": "http://0.0.0.0:8787/mcp"}`)
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	concurrent := strings.Replace(original, `"numStartups": 5`, `"numStartups": 6`, 1)

	// Directly: the file changed since "before", so nothing is written.
	if err := writeClaudeConfig(path, []byte(original), []byte(`{"x": 1}`)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(concurrent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeConfig(path, []byte(original), []byte(`{"x": 1}`)); !errors.Is(err, errConfigChangedMeanwhile) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != concurrent {
		t.Fatal("the concurrent write was lost")
	}

	// Through the update loop: Claude Code saves the file while we plan;
	// the result keeps its change and ours.
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	plans := 0
	if _, err := updateClaudeConfig(path, func(data []byte) ([]byte, bool, error) {
		plans++
		if plans == 1 {
			if err := os.WriteFile(path, []byte(concurrent), 0o600); err != nil {
				return nil, false, err
			}
		}
		next, err := jsonSetNestedMember(data, "mcpServers", "soluterminal", claudeHTTPEntryJSON(testMCPURL))
		return next, err == nil, err
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if plans != 2 || !strings.Contains(string(got), `"numStartups": 6`) || claudeEntryOf(t, got)["url"] != testMCPURL {
		t.Fatalf("plans=%d result %s", plans, got)
	}
}

// A retry starts from what the file holds now: an entry another program
// changed in between is left alone, and an entry the CLI path removed before
// its add failed is put back.
func TestDirectWritesRecheckTheEntryTheyDecidedOn(t *testing.T) {
	t.Run("codex fix after the url changed", func(t *testing.T) {
		useMCPRegistration(t, "", "")
		path := filepath.Join(t.TempDir(), "config.toml")
		custom := "[mcp_servers.soluterminal]\nurl = \"http://10.0.0.2:8787/mcp\"\n"
		if err := os.WriteFile(path, []byte(custom), 0o600); err != nil {
			t.Fatal(err)
		}
		registerCodexMcpDirect(path, "", testMCPURL, "fix", "http://0.0.0.0:8787/mcp")
		if b, _ := os.ReadFile(path); string(b) != custom {
			t.Fatalf("custom entry overwritten:\n%s", b)
		}
	})
	t.Run("claude after the url changed", func(t *testing.T) {
		useMCPRegistration(t, "", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		custom := `{"mcpServers": {"soluterminal": {"type": "http", "url": "http://10.0.0.2:8787/mcp"}}}`
		path := filepath.Join(home, ".claude.json")
		if err := os.WriteFile(path, []byte(custom), 0o600); err != nil {
			t.Fatal(err)
		}
		registerClaudeMcpDirect(testMCPURL, "fix", "http://0.0.0.0:8787/mcp", false, "test")
		if b, _ := os.ReadFile(path); string(b) != custom {
			t.Fatalf("custom entry overwritten: %s", b)
		}
	})
	t.Run("claude after the CLI removed the entry", func(t *testing.T) {
		useMCPRegistration(t, "", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		path := filepath.Join(home, ".claude.json")
		if err := os.WriteFile(path, []byte(`{"numStartups": 5, "mcpServers": {"other": {}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		registerClaudeMcpDirect(testMCPURL, "fix", testMCPURL, true, "claude CLI add failed")
		if e := claudeEntryOf(t, []byte(readTestFile(t, path))); e["url"] != testMCPURL {
			t.Fatalf("entry not restored: %v", e)
		}
	})
}
