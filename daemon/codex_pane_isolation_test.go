package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCodexPaneNeedsNoDaemon(t *testing.T) {
	for _, tc := range []struct {
		cmd  []string
		want bool
	}{
		{[]string{"codex"}, true},
		{[]string{"codex", "resume", "thread-id"}, true},
		{[]string{"codex", "fork", "thread-id"}, true},
		{[]string{"codex", "--model", "gpt-6-sol", "Fix this"}, true},
		{[]string{"codex", "--cd", "review"}, true},
		{[]string{"codex", "-C", "exec", "Fix this"}, true},
		{[]string{"codex", "--profile", "help"}, true},
		{[]string{"codex", "-p", "mcp", "resume", "review"}, true},
		{[]string{"codex", "--profile=help", "--cd=review"}, true},
		{[]string{"codex", "-phelp", "-Creview"}, true},
		{[]string{"codex", "resume", "review"}, true},
		{[]string{"codex", "fork", "help"}, true},
		{[]string{"codex", "--", "help"}, true},
		{[]string{"codex", "--", "--remote"}, true},
		{[]string{"codex", "Fix this"}, true},
		{[]string{"codex", "exec", "Fix this"}, false},
		{[]string{"codex", "--model", "gpt-6-sol", "exec", "Fix this"}, false},
		{[]string{"codex", "--profile", "help", "exec", "Fix this"}, false},
		{[]string{"codex", "-C", "review", "login"}, false},
		{[]string{"codex", "app-server", "--stdio"}, false},
		{[]string{"codex", "--help"}, false},
		{[]string{"codex", "--version"}, false},
		{[]string{"codex", "-V"}, false},
		{[]string{"codex", "--remote", "unix:///tmp/pane.sock"}, false},
		{[]string{"codex", "--profile", "help", "--remote=unix:///tmp/pane.sock"}, false},
		{[]string{"codex", "resume", "--no-daemon"}, false},
		{[]string{"claude"}, false},
	} {
		if got := codexPaneNeedsNoDaemon(tc.cmd); got != tc.want {
			t.Errorf("codexPaneNeedsNoDaemon(%q) = %t, want %t", tc.cmd, got, tc.want)
		}
	}
}

func TestCodexPaneLaunchCommand(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nif [ \"$1\" = --help ]; then echo --no-daemon; fi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := []string{bin, "resume", "thread-id"}
	if got, want := codexPaneLaunchCommand(cmd), []string{bin, "--no-daemon", "resume", "thread-id"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("launch = %q, want %q", got, want)
	}
	if got := codexPaneLaunchCommand(cmd); !reflect.DeepEqual(cmd, []string{bin, "resume", "thread-id"}) || len(got) != 4 {
		t.Fatal("launch mutated persisted command")
	}
	// Provider configuration and approval isolation must reach the same
	// actual child while the saved resume command stays provider-independent.
	brokerFlags := []string{"-c", `model_provider="solu_team_broker"`}
	child := routeChildCommand(codexPaneLaunchCommand(cmd), brokerFlags)
	wantChild := []string{bin, "-c", `model_provider="solu_team_broker"`, "--no-daemon", "resume", "thread-id"}
	if !reflect.DeepEqual(child, wantChild) || !reflect.DeepEqual(cmd, []string{bin, "resume", "thread-id"}) {
		t.Fatalf("broker child = %q, canonical command = %q", child, cmd)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nif [ \"$1\" = --help ]; then echo old-version; fi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := codexPaneLaunchCommand(cmd); !reflect.DeepEqual(got, cmd) {
		t.Fatalf("unsupported CLI launch = %q, want %q", got, cmd)
	}
}

func TestCodexPaneShellFunction(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "codex")
	writeCLI := func(supported bool) {
		t.Helper()
		help := "old-version"
		if supported {
			help = "--no-daemon"
		}
		script := "#!/bin/sh\nif [ \"$1\" = --help ]; then echo " + help + "; exit; fi\nprintf '%s\\n' \"$@\"\n"
		if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeCLI(true)
	baseEnv := []string{"SOLU_PANE_ID=pane_test", "PATH=" + binDir + ":/usr/bin:/bin", "HOME=" + t.TempDir()}
	for _, tc := range []struct {
		args string
		want string
	}{
		{"codex", "--no-daemon"},
		{"codex resume thread-id", "--no-daemon\nresume\nthread-id"},
		{"codex --cd review", "--no-daemon\n--cd\nreview"},
		{"codex -C exec 'Fix this'", "--no-daemon\n-C\nexec\nFix this"},
		{"codex --profile help", "--no-daemon\n--profile\nhelp"},
		{"codex -p mcp resume review", "--no-daemon\n-p\nmcp\nresume\nreview"},
		{"codex --profile=help --cd=review", "--no-daemon\n--profile=help\n--cd=review"},
		{"codex -phelp -Creview", "--no-daemon\n-phelp\n-Creview"},
		{"codex resume review", "--no-daemon\nresume\nreview"},
		{"codex fork help", "--no-daemon\nfork\nhelp"},
		{"codex -- help", "--no-daemon\n--\nhelp"},
		{"codex -- --remote", "--no-daemon\n--\n--remote"},
		{"codex exec prompt", "exec\nprompt"},
		{"codex --model gpt-6-sol exec prompt", "--model\ngpt-6-sol\nexec\nprompt"},
		{"codex --profile help exec prompt", "--profile\nhelp\nexec\nprompt"},
		{"codex -C review login", "-C\nreview\nlogin"},
		{"codex app-server --stdio", "app-server\n--stdio"},
		{"codex --version", "--version"},
		{"codex --remote unix:///tmp/pane.sock", "--remote\nunix:///tmp/pane.sock"},
		{"codex --profile help --remote=unix:///tmp/pane.sock", "--profile\nhelp\n--remote=unix:///tmp/pane.sock"},
		{"codex --no-daemon resume thread-id", "--no-daemon\nresume\nthread-id"},
	} {
		cmd := exec.Command(bash, "-c", tc.args)
		cmd.Env = codexPaneShellEnv(bash, baseEnv)
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != tc.want {
			t.Errorf("%q = %q, %v; want %q", tc.args, out, err, tc.want)
		}
	}
	writeCLI(false)
	cmd := exec.Command(bash, "-c", "codex resume thread-id")
	cmd.Env = codexPaneShellEnv(bash, baseEnv)
	if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "resume\nthread-id" {
		t.Errorf("older Codex = %q, %v", out, err)
	}
	if got := codexPaneShellEnv(bash, []string{"PATH=/usr/bin"}); len(got) != 1 {
		t.Fatalf("outside a pane: %q", got)
	}
	if got := codexPaneShellEnv("/bin/zsh", baseEnv); len(got) != len(baseEnv) {
		t.Fatalf("another shell: %q", got)
	}
}
