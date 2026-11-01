package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestCodexInlineTerminalCmd(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  []string
		want []string
	}{
		{"fresh", []string{"codex"}, []string{"codex", "--no-alt-screen"}},
		{"resume", []string{"codex", "resume", "thread-id"}, []string{"codex", "--no-alt-screen", "resume", "thread-id"}},
		{"fork", []string{"/usr/bin/codex", "fork", "--last"}, []string{"/usr/bin/codex", "--no-alt-screen", "fork", "--last"}},
		{"Windows command wrapper", []string{`C:\Users\alice\AppData\Roaming\npm\codex.cmd`}, []string{`C:\Users\alice\AppData\Roaming\npm\codex.cmd`, "--no-alt-screen"}},
		{"options", []string{"codex", "-m", "gpt-6-sol"}, []string{"codex", "--no-alt-screen", "-m", "gpt-6-sol"}},
		{"initial prompt", []string{"codex", "Explain this"}, []string{"codex", "--no-alt-screen", "Explain this"}},
		{"explicit", []string{"codex", "--no-alt-screen"}, nil},
		{"isolation is independent", []string{"codex", "--no-daemon", "resume", "thread"}, []string{"codex", "--no-alt-screen", "--no-daemon", "resume", "thread"}},
		{"utility after options", []string{"codex", "--model", "test", "exec", "hello"}, nil},
		{"utility after utility-named value", []string{"codex", "--profile", "help", "exec", "hello"}, nil},
		{"utility-named value", []string{"codex", "--cd", "review"}, []string{"codex", "--no-alt-screen", "--cd", "review"}},
		{"flag-named value", []string{"codex", "--profile", "--no-alt-screen"}, []string{"codex", "--no-alt-screen", "--profile", "--no-alt-screen"}},
		{"flag prompt", []string{"codex", "--", "--no-alt-screen"}, []string{"codex", "--no-alt-screen", "--", "--no-alt-screen"}},
		{"remote transport", []string{"codex", "--remote", "unix:///tmp/pane.sock"}, nil},
		{"attached remote transport", []string{"codex", "--remote=unix:///tmp/pane.sock"}, nil},
		{"help", []string{"codex", "--help"}, nil},
		{"version", []string{"codex", "-V"}, nil},
		{"headless", []string{"codex", "exec", "hello"}, nil},
		{"app server", []string{"codex", "app-server"}, nil},
		{"shell", []string{"bash"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(tc.cmd)
			if got := codexInlineTerminalCmd(tc.cmd); !slices.Equal(got, tc.want) {
				t.Fatalf("codexInlineTerminalCmd(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
			if !slices.Equal(tc.cmd, before) {
				t.Fatalf("codexInlineTerminalCmd mutated pane command: %q", tc.cmd)
			}
		})
	}
}

func TestCodexInlineAndIsolationCombinedLaunch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Unix CLI")
	}
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho --no-daemon\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	canonical := []string{bin, "resume", "thread"}
	child := routeChildCommand(codexPaneLaunchCommand(codexInlineTerminalCmd(canonical)), []string{"-c", "model_provider=team"})
	want := []string{bin, "-c", "model_provider=team", "--no-daemon", "--no-alt-screen", "resume", "thread"}
	if !slices.Equal(child, want) || !slices.Equal(canonical, []string{bin, "resume", "thread"}) {
		t.Fatalf("combined launch = %q, canonical = %q; want %q", child, canonical, want)
	}
}

func TestCodexInlineAndIsolationBashStartup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Bash environment import")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	bin := t.TempDir()
	capture := filepath.Join(t.TempDir(), "args")
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nif [ \"$1\" = --help ]; then echo --no-daemon; exit; fi\nprintf '%s\\n' \"$*\" > \"$CAPTURE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(t.TempDir(), "bashrc")
	if err := os.WriteFile(rc, []byte("PATH="+shellSingleQuote(bin)+":$PATH\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := codexPaneShellEnv(bash, []string{"SOLU_PANE_ID=pane_test", "HOME=" + t.TempDir(), "PATH=/usr/bin:/bin", "CAPTURE=" + capture, "BASH_FUNC_codex%%=() { echo obsolete; }", codexInlineBashEnv})
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, "BASH_FUNC_codex%%=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d competing Codex shell functions", count)
	}
	for _, tc := range []struct{ script, want string }{
		{"codex resume thread", "--no-daemon --no-alt-screen resume thread\n"},
		{"codex --no-alt-screen resume thread", "--no-daemon --no-alt-screen resume thread\n"},
		{"codex --no-daemon resume thread", "--no-alt-screen --no-daemon resume thread\n"},
		{"codex --no-daemon --no-alt-screen", "--no-daemon --no-alt-screen\n"},
		{"codex --cd review", "--no-daemon --no-alt-screen --cd review\n"},
		{"codex --profile help exec prompt", "--profile help exec prompt\n"},
		{"codex --remote unix:///tmp/pane.sock", "--remote unix:///tmp/pane.sock\n"},
		{"codex -- --no-alt-screen", "--no-daemon --no-alt-screen -- --no-alt-screen\n"},
		{"codex --profile --no-alt-screen", "--no-daemon --no-alt-screen --profile --no-alt-screen\n"},
	} {
		c := exec.Command(bash, "--noprofile", "--rcfile", rc, "-ic", tc.script)
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("Bash startup: %v: %s", err, out)
		}
		got, err := os.ReadFile(capture)
		if err != nil || string(got) != tc.want {
			t.Errorf("%s = %q, err=%v; want %q", tc.script, got, err, tc.want)
		}
	}
}

func TestCodexInlineBashEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Bash function import is Unix-only")
	}
	if strings.Contains(codexInlineBashEnv, ",") {
		t.Fatal("Bash function cannot contain a comma in the pane --env wire format")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Run inside a SoluTerminal pane, the test inherits that pane's own
	// exported codex/claude functions and shim directory; Bash would import
	// the inherited function instead of the one under test.
	inherited := filterOutEnvKeys(os.Environ(), []string{"BASH_FUNC_codex%%", "BASH_FUNC_claude%%", codexInlineShimEnvKey})
	env := append(inherited, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), codexInlineBashEnv)
	for _, tc := range []struct {
		name, shellFlag, script, want string
	}{
		{"interactive", "-ic", "codex; codex resume thread; codex exec print; codex --no-alt-screen", "--no-alt-screen\n--no-alt-screen resume thread\nexec print\n--no-alt-screen\n"},
		{"noninteractive", "-c", "codex; codex exec print", "\nexec print\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := exec.Command(bash, "--noprofile", "--norc", tc.shellFlag, tc.script)
			c.Env = env
			got, err := c.Output()
			if err != nil {
				t.Fatalf("run bash: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("codex invocations = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodexInlinePowerShellCmd(t *testing.T) {
	for _, shell := range []string{`C:\Program Files\PowerShell\7\pwsh.exe`, `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, `/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe`} {
		original := []string{shell}
		got := codexInlinePowerShellCmd(original)
		if len(got) != 4 || got[0] != shell || got[1] != "-NoExit" || got[2] != "-Command" || got[3] != paneShellPowerShellInit {
			t.Fatalf("PowerShell launch = %#v", got)
		}
		if !slices.Equal(original, []string{shell}) {
			t.Fatalf("mutated persisted shell command: %#v", original)
		}
	}
	for _, other := range [][]string{{"bash"}, {"codex"}, {"pwsh.exe", "-NoProfile"}} {
		if got := codexInlinePowerShellCmd(other); got != nil {
			t.Fatalf("custom/non-PowerShell launch %#v was rewritten as %#v", other, got)
		}
	}
}

func TestCodexInlinePowerShellFunction(t *testing.T) {
	ps, err := exec.LookPath("pwsh")
	if err != nil {
		ps, err = exec.LookPath("powershell.exe")
	}
	interop := false
	if err != nil && runtime.GOOS == "linux" {
		// Exercise Windows PowerShell from WSL when available, without
		// altering the user's shell profile, CLI or credentials.
		ps = "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"
		_, err = os.Stat(ps)
		interop = err == nil
	}
	if err != nil {
		t.Skip("PowerShell unavailable")
	}
	// LookPath can already discover powershell.exe through WSL's imported
	// Windows PATH; interop is not limited to the explicit fallback above.
	interop = runtime.GOOS != "windows" && strings.HasSuffix(strings.ToLower(ps), ".exe")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex.ps1"), []byte("if ($args[0] -ceq '--help') { '--no-daemon'; $global:LASTEXITCODE = 0; return }\n$args -join '|'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if interop {
		path, err := runWSLPath(t.Context(), "-w", bin)
		if err != nil {
			t.Fatal(err)
		}
		bin = strings.TrimSpace(string(path))
	}
	// Never fall through to an authenticated real CLI, even if script lookup
	// differs between Windows PowerShell, pwsh and WSL UNC paths.
	quotedBin := strings.ReplaceAll(bin, "'", "''")
	prefix := "$ErrorActionPreference = 'Stop'; $env:PATH = '" + quotedBin + "'; " +
		"$fixture = Join-Path '" + quotedBin + "' 'codex.ps1'; " +
		"$selected = Get-Command codex -CommandType Application,ExternalScript -ErrorAction Stop | Select-Object -First 1; " +
		"if ($selected.Definition -cne $fixture) { throw 'Refusing to execute a non-fixture Codex' }; "
	for _, tc := range []struct{ command, want string }{
		{"codex resume thread", "--no-daemon|--no-alt-screen|resume|thread"},
		{"codex --no-alt-screen resume thread", "--no-daemon|--no-alt-screen|resume|thread"},
		{"codex --no-daemon resume thread", "--no-alt-screen|--no-daemon|resume|thread"},
		{"codex --no-daemon --no-alt-screen", "--no-daemon|--no-alt-screen"},
		{"codex --cd review", "--no-daemon|--no-alt-screen|--cd|review"},
		{"codex --profile help exec prompt", "--profile|help|exec|prompt"},
		{"codex --remote unix:///tmp/pane.sock", "--remote|unix:///tmp/pane.sock"},
		{"codex '--' '--no-alt-screen'", "--no-daemon|--no-alt-screen|--|--no-alt-screen"},
		{"codex --profile --no-alt-screen", "--no-daemon|--no-alt-screen|--profile|--no-alt-screen"},
		{"codex -V", "-V"},
		{"codex -v", "--no-daemon|--no-alt-screen|-v"},
	} {
		got, err := runTestPowerShell(t, ps, prefix+codexInlinePowerShellInit+"; "+tc.command, nil)
		if err != nil || strings.TrimSpace(string(got)) != tc.want {
			t.Errorf("%s = %q, err=%v; want %q", tc.command, got, err, tc.want)
		}
	}
}

func TestCodexInlineLoginWrapperKeepsCanonicalResumeCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("login shell wrapping is Unix-only")
	}
	const thread = "019f5697-154c-7a92-8b61-ad2f9b68476d"
	cmd := []string{"codex", "resume", thread}
	canonical, ok := tryLoginShellWrap(cmd, "", nil)
	if !ok {
		t.Skip("bash unavailable")
	}
	launch, ok := tryLoginShellWrap(codexInlineTerminalCmd(cmd), "", nil)
	if !ok {
		t.Fatal("could not construct inline login wrapper")
	}
	if got := resumeThreadIDFromCodexCmd(canonical); got != thread {
		t.Fatalf("persisted wrapper lost resume thread: %q", got)
	}
	if strings.Contains(canonical[2], "--no-alt-screen") || !strings.Contains(launch[2], "'--no-alt-screen'") {
		t.Fatalf("inline flag must appear only in launch wrapper: canonical=%q launch=%q", canonical[2], launch[2])
	}
}

func TestCodexInlineLaunchKeepsTeamBrokerFlagsAheadOfResume(t *testing.T) {
	canonical := []string{"codex", "resume", "thread"}
	flags := []string{"-c", "model_provider=team"}
	launch := routeChildCommand(codexInlineTerminalCmd(canonical), flags)
	want := []string{"codex", "-c", "model_provider=team", "--no-alt-screen", "resume", "thread"}
	if !slices.Equal(launch, want) {
		t.Fatalf("Codex team launch = %q, want %q", launch, want)
	}
	if !slices.Equal(canonical, []string{"codex", "resume", "thread"}) {
		t.Fatalf("canonical pane command changed: %q", canonical)
	}
}

func TestCodexInlineShimPreservesSelectedCLIAndExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink shim")
	}
	shimDir, err := ensureCodexInlineShim(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(shimDir, "codex")); err != nil || target == "" {
		t.Fatalf("missing Codex shim symlink: target=%q err=%v", target, err)
	}
	bin := t.TempDir()
	capture := filepath.Join(t.TempDir(), "args")
	fakeCodex := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$CAPTURE\"\nexit 23\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(fakeCodex), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(codexInlineShimEnvKey, shimDir)
	t.Setenv("CAPTURE", capture)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+bin+string(os.PathListSeparator)+shimDir)
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"resume", "thread"}, "--no-alt-screen resume thread\n"},
		{[]string{"exec", "hello"}, "exec hello\n"},
		{[]string{"--no-alt-screen"}, "--no-alt-screen\n"},
	} {
		if code := runCodexInlineShim(tc.argv); code != 23 {
			t.Fatalf("shim exit code = %d, want real Codex exit code 23", code)
		}
		got, err := os.ReadFile(capture)
		if err != nil || string(got) != tc.want {
			t.Fatalf("real Codex args = %q, err=%v, want %q", got, err, tc.want)
		}
	}
}

func TestCodexInlineZshPreservesStartupAndSurvivesPathChanges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("zsh startup is Unix-only")
	}
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh unavailable")
	}
	home := t.TempDir()
	bin := t.TempDir()
	shim := t.TempDir()
	startup := filepath.Join(t.TempDir(), "startup")
	for name, body := range map[string]string{
		".zshenv":   `print -r -- env >> "$STARTUP_LOG"`,
		".zprofile": `print -r -- profile >> "$STARTUP_LOG"`,
		".zshrc":    `print -r -- rc >> "$STARTUP_LOG"; PATH="$REAL_BIN:$PATH"`,
		".zlogin":   `print -r -- login >> "$STARTUP_LOG"`,
	} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	startEnv := []string{"HOME=" + home, "ZDOTDIR=" + home, "PATH=" + shim + string(os.PathListSeparator) + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "REAL_BIN=" + bin, "STARTUP_LOG=" + startup}
	zshEnv, err := codexInlineZshEnvVars(t.TempDir(), startEnv)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, flags, wantStartup string
	}{
		{"interactive", "-ic", "env\nrc\n"},
		{"login", "-lic", "env\nprofile\nrc\nlogin\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(startup, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			c := exec.Command(zsh, tc.flags, `codex resume thread; codex exec hello; print -r -- "ZDOTDIR=$ZDOTDIR"`)
			c.Env = append(append([]string{}, startEnv...), zshEnv...)
			got, err := c.CombinedOutput()
			if err != nil {
				t.Fatalf("zsh: %v: %s", err, got)
			}
			want := "--no-alt-screen resume thread\nexec hello\nZDOTDIR=" + home + "\n"
			if string(got) != want {
				t.Fatalf("Codex invocations = %q, want %q", got, want)
			}
			logged, err := os.ReadFile(startup)
			if err != nil || string(logged) != tc.wantStartup {
				t.Fatalf("startup order = %q, err=%v, want %q", logged, err, tc.wantStartup)
			}
		})
	}
}
