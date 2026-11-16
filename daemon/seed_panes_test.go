//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSeedFile(t *testing.T, dataDir, body string, mode os.FileMode) {
	t.Helper()
	path := seedPanesPath(dataDir)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func seedDoc(cwd string, argvs ...string) string {
	var panes []string
	for i, argv := range argvs {
		panes = append(panes, fmt.Sprintf(`{"title":"take %d","cwd":%q,"argv":%s}`, i+1, cwd, argv))
	}
	return `{"panes":[` + strings.Join(panes, ",") + `]}`
}

func TestSeedPanesAllowed(t *testing.T) {
	side := decideDaemonRole("linux", "127.0.0.1:18899", "/scratch", "/home/u/.solu")
	primary := daemonRole{Primary: true}
	cases := []struct {
		name     string
		enabled  bool
		role     daemonRole
		existing int
		seeded   bool
		ok       bool
		logs     string
	}{
		{"flag off", false, side, 0, false, false, ""},
		{"flag off on the primary says nothing", false, primary, 0, false, false, ""},
		{"primary refuses with a log line", true, primary, 0, false, false, "primary daemon"},
		{"side daemon with saved panes", true, side, 2, false, false, "fresh -data"},
		{"side daemon seeded before, every pane since closed", true, side, 0, true, false, "already seeded once"},
		{"side daemon, fresh data", true, side, 0, false, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := seedPanesAllowed(tc.enabled, tc.role, tc.existing, tc.seeded)
			if ok != tc.ok || (tc.logs == "") != (why == "") || !strings.Contains(why, tc.logs) {
				t.Fatalf("seedPanesAllowed = %v, %q; want %v with %q", ok, why, tc.ok, tc.logs)
			}
		})
	}
}

// TestSeedArgvKeepsSafeFlags: ordinary flags, and the same flags with safe
// values, are not mistaken for approval bypasses.
func TestSeedArgvKeepsSafeFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--model", "claude-sonnet-5-5"},
		{"--permission-mode", "plan"},
		{"--permission-mode=acceptEdits"},
		{"-s", "workspace-write"},
		{"--sandbox=read-only"},
		{"-c", "approval_policy=on-request"},
		{"--config", "model=\"o4\""},
		{"-s"},
	} {
		if flag, bad := seedArgvBypassesApprovals(args); bad {
			t.Errorf("%q flagged as a bypass (%s)", args, flag)
		}
	}
}

func TestSeedArgvAllowedKeepsModelAndEffort(t *testing.T) {
	for _, tc := range []struct {
		agent string
		args  []string
	}{
		{"claude", nil},
		{"claude", []string{"--model", "claude-sonnet-5-5"}},
		{"claude", []string{"--model=opus", "--effort", "high"}},
		{"claude", []string{"--model", "claude-opus-5-5[1m]"}},
		{"codex", []string{"-m", "gpt-5.5-codex"}},
		{"codex", []string{"--model=o4-mini"}},
	} {
		if err := seedArgvAllowed(tc.agent, tc.args); err != nil {
			t.Errorf("%s %q refused: %v", tc.agent, tc.args, err)
		}
	}
}

func TestLoadSeedPanesAcceptsTheDocumentedShape(t *testing.T) {
	data, cwd := t.TempDir(), t.TempDir()
	writeSeedFile(t, data, seedDoc(cwd, `["claude","--model","claude-sonnet-5-5"]`, `["codex"]`), 0o600)
	specs, err := loadSeedPanes(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Argv[2] != "claude-sonnet-5-5" || specs[1].Argv[0] != "codex" || specs[0].Cwd != cwd || specs[0].Title != "take 1" {
		t.Fatalf("specs = %+v", specs)
	}
}

func TestLoadSeedPanesRefuses(t *testing.T) {
	cwd := t.TempDir()
	many := make([]string, seedPanesMax+1)
	for i := range many {
		many[i] = `["claude"]`
	}
	cases := []struct {
		name, body, want string
		mode             os.FileMode
	}{
		{"a shell", seedDoc(cwd, `["bash","-c","echo hi"]`), `argv[0] must be "claude" or "codex"`, 0o600},
		{"a path to claude", seedDoc(cwd, `["/usr/bin/claude"]`), "argv[0]", 0o600},
		{"a shell string", seedDoc(cwd, `["claude --model x; rm -rf ~"]`), "argv[0]", 0o600},
		{"empty argv", seedDoc(cwd, `[]`), "argv is empty", 0o600},
		{"codex app-server", seedDoc(cwd, `["codex","app-server"]`), "not allowed", 0o600},
		{"codex exec", seedDoc(cwd, `["codex","exec","fix it"]`), "not allowed", 0o600},
		{"a starting prompt", seedDoc(cwd, `["claude","do X"]`), "not allowed", 0o600},
		{"codex -a never", seedDoc(cwd, `["codex","-a","never"]`), "approvals or sandbox", 0o600},
		{"codex --ask-for-approval=never", seedDoc(cwd, `["codex","--ask-for-approval=never"]`), "approvals or sandbox", 0o600},
		{"codex attached -s", seedDoc(cwd, `["codex","-sdanger-full-access"]`), "not allowed", 0o600},
		{"codex attached -c", seedDoc(cwd, `["codex","-capproval_policy=never"]`), "not allowed", 0o600},
		{"codex profile", seedDoc(cwd, `["codex","-p","yolo"]`), "not allowed", 0o600},
		{"codex -m attached with =", seedDoc(cwd, `["codex","-m=gpt"]`), "not allowed", 0o600},
		{"claude settings json", seedDoc(cwd, `["claude","--settings","{\"permissions\":{\"defaultMode\":\"bypassPermissions\"}}"]`), "not allowed", 0o600},
		{"claude allowed tools", seedDoc(cwd, `["claude","--allowedTools","Bash"]`), "not allowed", 0o600},
		{"claude effort on codex", seedDoc(cwd, `["codex","--effort","high"]`), "not allowed", 0o600},
		{"model given a path", seedDoc(cwd, `["claude","--model","/etc/passwd"]`), "plain model", 0o600},
		{"model without a value", seedDoc(cwd, `["claude","--model"]`), "needs a value", 0o600},
		{"a control character", seedDoc(cwd, `["claude","a\nb"]`), "control characters", 0o600},
		{"claude skip permissions", seedDoc(cwd, `["claude","--dangerously-skip-permissions"]`), "approvals or sandbox", 0o600},
		{"claude skip permissions with a value", seedDoc(cwd, `["claude","--dangerously-skip-permissions=true"]`), "approvals or sandbox", 0o600},
		{"claude allow skip permissions", seedDoc(cwd, `["claude","--allow-dangerously-skip-permissions"]`), "approvals or sandbox", 0o600},
		{"claude bypass mode, separate", seedDoc(cwd, `["claude","--permission-mode","bypassPermissions"]`), "approvals or sandbox", 0o600},
		{"claude bypass mode, joined", seedDoc(cwd, `["claude","--permission-mode=bypassPermissions"]`), "approvals or sandbox", 0o600},
		{"codex yolo", seedDoc(cwd, `["codex","--yolo"]`), "approvals or sandbox", 0o600},
		{"codex bypass", seedDoc(cwd, `["codex","--dangerously-bypass-approvals-and-sandbox"]`), "approvals or sandbox", 0o600},
		{"codex full auto", seedDoc(cwd, `["codex","--full-auto"]`), "approvals or sandbox", 0o600},
		{"codex -s danger", seedDoc(cwd, `["codex","-s","danger-full-access"]`), "approvals or sandbox", 0o600},
		{"codex --sandbox=danger", seedDoc(cwd, `["codex","--sandbox=danger-full-access"]`), "approvals or sandbox", 0o600},
		{"codex -c sandbox_mode", seedDoc(cwd, `["codex","-c","sandbox_mode=\"danger-full-access\""]`), "approvals or sandbox", 0o600},
		{"codex --config approval never", seedDoc(cwd, `["codex","--config","approval_policy = 'never'"]`), "approvals or sandbox", 0o600},
		{"codex --config= approval never", seedDoc(cwd, `["codex","--config=approval_policy=never"]`), "approvals or sandbox", 0o600},
		{"relative cwd", seedDoc("work", `["claude"]`), "absolute", 0o600},
		{"missing cwd", seedDoc(filepath.Join(cwd, "nope"), `["claude"]`), "existing directory", 0o600},
		{"unknown field", `{"panes":[{"cwd":"` + cwd + `","argv":["claude"],"prompt":"go"}]}`, "unknown field", 0o600},
		{"trailing data", seedDoc(cwd, `["claude"]`) + `{}`, "trailing", 0o600},
		{"a stray closing bracket", seedDoc(cwd, `["claude"]`) + `]`, "trailing", 0o600},
		{"no panes", `{"panes":[]}`, "no panes", 0o600},
		{"too many panes", seedDoc(cwd, many...), "at most", 0o600},
		{"group-writable file", seedDoc(cwd, `["claude"]`), "writable by group or others", 0o620},
		{"world-writable file", seedDoc(cwd, `["claude"]`), "writable by group or others", 0o602},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := t.TempDir()
			writeSeedFile(t, data, tc.body, tc.mode)
			if _, err := loadSeedPanes(data); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	t.Run("a symlink", func(t *testing.T) {
		data, elsewhere := t.TempDir(), t.TempDir()
		writeSeedFile(t, elsewhere, seedDoc(cwd, `["claude"]`), 0o600)
		if err := os.Symlink(seedPanesPath(elsewhere), seedPanesPath(data)); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSeedPanes(data); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("err = %v, want a symlink refusal", err)
		}
	})
	t.Run("no file", func(t *testing.T) {
		if _, err := loadSeedPanes(t.TempDir()); !os.IsNotExist(err) {
			t.Fatalf("err = %v, want not-exist", err)
		}
	})
}

// TestSeedPanesAtBoot: a side daemon on a fresh data dir starts exactly the
// listed panes, marked as seeded, with the argv and cwd from the file; the
// primary daemon, a data dir with panes, or one bad entry starts none.
func TestSeedPanesAtBoot(t *testing.T) {
	side := decideDaemonRole("linux", "127.0.0.1:18899", "/scratch", "/home/u/.solu")
	run := func(t *testing.T, role daemonRole, body string, existing bool) []SpawnOpts {
		t.Helper()
		data := t.TempDir()
		if body != "" {
			writeSeedFile(t, data, body, 0o600)
		}
		panes, err := NewPaneStore(data)
		if err != nil {
			t.Fatal(err)
		}
		var spawned []SpawnOpts
		a := &apiServer{dataDir: data, mgr: NewManager(data), panes: panes}
		if existing {
			if err := panes.Put(transactionTestPane("pane_old")); err != nil {
				t.Fatal(err)
			}
		}
		a.spawnPane = func(o SpawnOpts) (*Pane, error) {
			spawned = append(spawned, o)
			return transactionTestPane(fmt.Sprintf("pane_seed_%d", len(spawned))), nil
		}
		a.seedPanesAtBoot(true, role)
		return spawned
	}
	cwd := t.TempDir()
	good := seedDoc(cwd, `["claude","--model","claude-sonnet-5-5"]`, `["codex"]`)

	got := run(t, side, good, false)
	if len(got) != 2 {
		t.Fatalf("side daemon seeded %d panes, want 2", len(got))
	}
	if strings.Join(got[0].Cmd, " ") != "claude --model claude-sonnet-5-5" || got[0].Cwd != cwd || got[0].Title != "take 1" || got[1].Agent != agentCodex {
		t.Fatalf("seeded opts = %+v", got)
	}
	for _, o := range got {
		if o.Origin == nil || o.Origin.Source != seedPaneOriginSrc || o.Origin.Worker != nil || o.Origin.TeamKind != "" || o.Role != "" || o.NeverAutoAnswer || o.AutoMode != "" {
			t.Fatalf("seeded pane metadata = %+v origin %+v", o, o.Origin)
		}
	}
	if n := len(run(t, daemonRole{Primary: true}, good, false)); n != 0 {
		t.Fatalf("primary daemon seeded %d panes", n)
	}
	if n := len(run(t, side, good, true)); n != 0 {
		t.Fatalf("side daemon with saved panes seeded %d panes", n)
	}
	if n := len(run(t, side, seedDoc(cwd, `["claude"]`, `["bash"]`), false)); n != 0 {
		t.Fatalf("a file with one bad entry seeded %d panes", n)
	}
	if n := len(run(t, side, "", false)); n != 0 {
		t.Fatalf("no file seeded %d panes", n)
	}

	// A data dir is seeded at most once: the marker written before the first
	// spawn stops a later boot even when every seeded pane was closed.
	data := t.TempDir()
	writeSeedFile(t, data, good, 0o600)
	var spawned int
	a := &apiServer{dataDir: data, mgr: NewManager(data)}
	a.spawnPane = func(SpawnOpts) (*Pane, error) {
		spawned++
		return transactionTestPane(fmt.Sprintf("p%d", spawned)), nil
	}
	a.seedPanesAtBoot(true, side)
	if spawned != 2 {
		t.Fatalf("first boot seeded %d panes, want 2", spawned)
	}
	if _, err := os.Stat(filepath.Join(data, seedPanesDoneName)); err != nil {
		t.Fatalf("no %s after seeding: %v", seedPanesDoneName, err)
	}
	a.seedPanesAtBoot(true, side) // no saved panes (no store), marker present
	if spawned != 2 {
		t.Fatalf("a second boot on a seeded -data seeded again (%d spawns)", spawned)
	}
}
