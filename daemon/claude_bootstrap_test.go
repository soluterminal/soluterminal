package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestLoginShellInitSharedWithSpawn pins the contract that detection and pane
// spawn use the SAME env prelude — otherwise a binary we report "installed"
// might not be spawnable (or vice-versa). Must include the native installer's
// ~/.local/bin and the nvm hook.
func TestLoginShellInitSharedWithSpawn(t *testing.T) {
	init := loginShellInit()
	for _, want := range []string{`$HOME/.local/bin`, `.nvm/nvm.sh`} {
		if !strings.Contains(init, want) {
			t.Fatalf("loginShellInit() missing %q; got: %s", want, init)
		}
	}
	// pane spawn must consume the same init so the two never drift.
	if wrapped, ok := tryLoginShellWrap([]string{"claude"}, "", nil); ok {
		joined := strings.Join(wrapped, " ")
		if !strings.Contains(joined, `$HOME/.local/bin`) {
			t.Fatalf("tryLoginShellWrap dropped the shared init; got: %s", joined)
		}
	}
}

func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"2.1.89 (Claude Code)": "2.1.89 (Claude Code)",
		"2.1.89\nextra noise":  "2.1.89",
		"  1.0.0  \n":          "1.0.0",
		"":                     "",
		"\nleading":            "",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Fatalf("firstLine(%q)=%q, want %q", in, got, want)
		}
	}
}

// TestVersionAfterMarker: a login-shell banner printed before the fence marker
// must not leak into the parsed version (the reason detection fences output).
func TestVersionAfterMarker(t *testing.T) {
	m := claudeVersionMarker
	cases := []struct{ in, want string }{
		{m + "2.1.89 (Claude Code)", "2.1.89 (Claude Code)"},
		{"Welcome to Ubuntu 22.04\nMOTD line\n" + m + "2.1.89 (Claude Code)\nUpdate available", "2.1.89 (Claude Code)"},
		{m + "\n", ""}, // marker but empty version
		{"banner with no marker", "banner with no marker"}, // no marker → best-effort firstLine
		{"line1\nline2", "line1"},
	}
	for _, c := range cases {
		if got := versionAfterMarker(c.in, m); got != c.want {
			t.Fatalf("versionAfterMarker(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestResolveClaudeAuthed pins the sign-in signal matrix: OAuth credential file
// OR an ANTHROPIC_API_KEY in the pane env count as authed (an API-key user never
// writes a credential file and must not be nudged to "Log in" forever); darwin
// stays conservative (Keychain unreadable → installed ⇒ authed); not-installed
// is never authed regardless of the rest.
func TestResolveClaudeAuthed(t *testing.T) {
	cases := []struct {
		name                    string
		installed, cred, apiKey bool
		goos                    string
		want                    bool
	}{
		{"not installed", false, true, true, "linux", false},
		{"oauth cred", true, true, false, "linux", true},
		{"api key only", true, false, true, "linux", true},
		{"neither on linux", true, false, false, "linux", false},
		{"neither on windows", true, false, false, "windows", false},
		{"darwin conservative", true, false, false, "darwin", true},
		{"api key only windows", true, false, true, "windows", true},
	}
	for _, c := range cases {
		if got := resolveClaudeAuthed(c.installed, c.cred, c.apiKey, c.goos); got != c.want {
			t.Fatalf("%s: resolveClaudeAuthed(%v,%v,%v,%q)=%v, want %v",
				c.name, c.installed, c.cred, c.apiKey, c.goos, got, c.want)
		}
	}
}

// TestAPIKeyMarkerDoesNotPolluteVersion: the key marker is printed BEFORE the
// version fence; version parsing must be unaffected whether or not it's present.
func TestAPIKeyMarkerDoesNotPolluteVersion(t *testing.T) {
	out := claudeAPIKeyMarker + claudeVersionMarker + "2.1.89 (Claude Code)\n"
	if got := versionAfterMarker(out, claudeVersionMarker); got != "2.1.89 (Claude Code)" {
		t.Fatalf("version with key marker = %q", got)
	}
	if !strings.Contains(out, claudeAPIKeyMarker) {
		t.Fatal("key marker not detectable in probe output")
	}
	// Distinct markers — a key hit must never satisfy the version fence.
	if strings.Contains(claudeVersionMarker, claudeAPIKeyMarker) ||
		strings.Contains(claudeAPIKeyMarker, claudeVersionMarker) {
		t.Fatal("markers must not contain each other")
	}
}

// TestClaudeInstallCommandForOS: the installer spec is well-formed for the
// runtime — a spawnable argv + a disclosure string pointing at the official
// installer. (We can't assert the OTHER OS's branch from here, but we pin that
// the current platform yields a usable, transparent command.)
func TestClaudeInstallCommandForOS(t *testing.T) {
	spec := claudeInstallCommandForOS()
	if !spec.Supported {
		t.Fatalf("install not supported on test platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if len(spec.Cmd) == 0 {
		t.Fatal("install spec has empty cmd")
	}
	if spec.Display == "" {
		t.Fatal("install spec has empty display string (no disclosure)")
	}
	if runtime.GOOS == "windows" {
		if spec.Cmd[0] != "powershell.exe" || !strings.Contains(spec.Display, "install.ps1") {
			t.Fatalf("windows install spec wrong: %+v", spec)
		}
	} else {
		if spec.Cmd[0] != "bash" || !strings.Contains(spec.Display, "install.sh") {
			t.Fatalf("unix install spec wrong: %+v", spec)
		}
		// The unix command must source the shared init so the post-install shell
		// (and future panes) resolve ~/.local/bin/claude.
		joined := strings.Join(spec.Cmd, " ")
		if !strings.Contains(joined, "$HOME/.local/bin") {
			t.Fatalf("unix install cmd missing ~/.local/bin PATH wiring: %s", joined)
		}
	}
}

// TestCurrentClaudeStatusDefault: before the first probe, status is unchecked
// (UI shows "checking…", not a premature "not installed").
func TestCurrentClaudeStatusDefault(t *testing.T) {
	a := &apiServer{}
	st := a.currentClaudeStatus()
	if st.checked || st.installed || st.authed || st.version != "" {
		t.Fatalf("zero-value status should be unchecked/empty; got %+v", st)
	}
}

// --- three-way probe outcome (a failure is not an answer) ---------------

// TestClassifyClaudeProbeOutput pins the outcome mapping: version marker →
// found, absent marker → definitively absent, neither → error (the shell died
// before answering — NO verdict).
func TestClassifyClaudeProbeOutput(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantOutcome claudeProbeOutcome
		wantVersion string
	}{
		{"found with version", claudeVersionMarker + "2.1.201 (Claude Code)\n", claudeProbeFound, "2.1.201 (Claude Code)"},
		{"found, --version died before printing", claudeVersionMarker, claudeProbeFound, ""},
		{"found behind login banner", "Welcome to Ubuntu\n" + claudeVersionMarker + "2.1.201 (Claude Code)", claudeProbeFound, "2.1.201 (Claude Code)"},
		{"definitively absent", claudeAbsentMarker, claudeProbeAbsent, ""},
		{"absent behind login banner", "MOTD noise\n" + claudeAbsentMarker, claudeProbeAbsent, ""},
		{"shell died — no marker at all", "Welcome to Ubuntu\npartial noise", claudeProbeError, ""},
		{"empty output", "", claudeProbeError, ""},
		{"key marker alone is not an answer", claudeAPIKeyMarker, claudeProbeError, ""},
	}
	for _, c := range cases {
		outcome, version := classifyClaudeProbeOutput(c.in)
		if outcome != c.wantOutcome || version != c.wantVersion {
			t.Fatalf("%s: classifyClaudeProbeOutput(%q) = (%v, %q), want (%v, %q)",
				c.name, c.in, outcome, version, c.wantOutcome, c.wantVersion)
		}
	}
}

// --- absent corroboration (a completed "absent" is not trusted blind) --------

// TestCorroborateClaudeAbsent is the core regression guard for the recurring
// false "Claude Code isn't installed for WSL" banner. A login shell can complete
// and print the absent marker while claude is genuinely on disk (nvm auto-use
// lost a fork under load), OR while the binary is mid-self-update (npm removed
// it seconds ago, a claude process is still running) — neither is an uninstall.
// A contradicted absent must downgrade to a no-verdict ERROR (keep prior), while
// a truly-absent verdict and the found/error outcomes pass through untouched.
func TestCorroborateClaudeAbsent(t *testing.T) {
	yes := func() bool { return true }
	no := func() bool { return false }
	cases := []struct {
		name          string
		in            claudeProbeOutcome
		onDisk, alive func() bool
		want          claudeProbeOutcome
	}{
		{"absent contradicted by a binary on disk → error", claudeProbeAbsent, yes, no, claudeProbeError},
		{"absent contradicted by a live claude process → error", claudeProbeAbsent, no, yes, claudeProbeError},
		{"absent with no contradiction → stays absent", claudeProbeAbsent, no, no, claudeProbeAbsent},
		{"found is never second-guessed", claudeProbeFound, yes, yes, claudeProbeFound},
		{"error stays error", claudeProbeError, yes, yes, claudeProbeError},
	}
	for _, c := range cases {
		if got := corroborateClaudeAbsent(c.in, c.onDisk, c.alive); got != c.want {
			t.Fatalf("%s: corroborateClaudeAbsent(%v)=%v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestCorroborateClaudeAbsentIsLazy: the cross-checks must run ONLY for an
// absent outcome — a found/error verdict needs no corroboration and must not
// pay for a disk or /proc scan — and the process scan must be skipped entirely
// once the disk already contradicts.
func TestCorroborateClaudeAbsentIsLazy(t *testing.T) {
	called := false
	probe := func() bool { called = true; return true }
	corroborateClaudeAbsent(claudeProbeFound, probe, probe)
	corroborateClaudeAbsent(claudeProbeError, probe, probe)
	if called {
		t.Fatal("cross-check ran for a non-absent outcome")
	}
	procCalled := false
	corroborateClaudeAbsent(claudeProbeAbsent,
		func() bool { return true },
		func() bool { procCalled = true; return false })
	if procCalled {
		t.Fatal("process scan ran although the disk already contradicted the absent")
	}
}

// TestClaudeBinaryUnderHome pins the load-immune filesystem cross-check across the
// install layouts the daemon's slim PATH can't see — especially the nvm-versions
// glob, the only signal that catches the reported WSL false-absent — and that a
// dangling symlink (a genuine uninstall) is NOT mistaken for an install.
func TestClaudeBinaryUnderHome(t *testing.T) {
	mkbin := func(dir, name string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Empty home → no claude anywhere.
	if claudeBinaryUnderHome(t.TempDir()) {
		t.Fatal("empty home reported a claude binary")
	}

	// nvm-managed (the reported case): claude ONLY under ~/.nvm/versions/node/*/bin.
	nvm := t.TempDir()
	mkbin(filepath.Join(nvm, ".nvm", "versions", "node", "v24.2.0", "bin"), "claude")
	if !claudeBinaryUnderHome(nvm) {
		t.Fatal("nvm-managed claude not found by the disk cross-check")
	}

	// Native installer ~/.local/bin.
	local := t.TempDir()
	mkbin(filepath.Join(local, ".local", "bin"), "claude")
	if !claudeBinaryUnderHome(local) {
		t.Fatal("~/.local/bin/claude not found")
	}

	// asdf shim.
	asdf := t.TempDir()
	mkbin(filepath.Join(asdf, ".asdf", "shims"), "claude")
	if !claudeBinaryUnderHome(asdf) {
		t.Fatal("asdf-shimmed claude not found")
	}

	// A dangling nvm bin symlink (package uninstalled, entry left behind) must NOT
	// count — otherwise a real uninstall would be pinned to a stale "installed".
	dangle := t.TempDir()
	bindir := filepath.Join(dangle, ".nvm", "versions", "node", "v20.0.0", "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dangle, "does-not-exist"), filepath.Join(bindir, "claude")); err != nil {
		t.Fatal(err)
	}
	if claudeBinaryUnderHome(dangle) {
		t.Fatal("dangling symlink counted as an installed claude")
	}
}

// TestDetectClaudeCLISuppressesFalseAbsent is the END-TO-END guard: it drives the
// REAL detectClaudeCLI login-shell probe against a synthetic $HOME whose nvm is
// inert (a non-empty nvm.sh that never puts node on PATH — standing in for nvm
// auto-use losing a fork under load) yet which HAS a claude binary under
// ~/.nvm/versions/node/*/bin. This is exactly the reported failure: the probe
// shell completes and reports "absent", but because the binary is provably on
// disk the outcome must be claudeProbeError (keep prior) — NEVER a definitive
// absence that would banner the user to install what they are already running.
func TestDetectClaudeCLISuppressesFalseAbsent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("login-shell probe is the Unix path")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash on this host")
	}
	home := t.TempDir()
	// A claude only nvm's node bin would surface — the stripped PATH below can't.
	nvmBin := filepath.Join(home, ".nvm", "versions", "node", "v24.2.0", "bin")
	if err := os.MkdirAll(nvmBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nvmBin, "claude"),
		[]byte("#!/bin/sh\necho '2.1.201 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A non-empty but INERT nvm.sh: it passes loginShellInit's `[ -s ]` guard and
	// is sourced, yet never touches PATH — the broken-auto-use state under load.
	if err := os.WriteFile(filepath.Join(home, ".nvm", "nvm.sh"),
		[]byte(": # inert nvm for test — deliberately does not add node to PATH\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin") // strip the real nvm/claude from the probe env
	// Interactive panes may export a claude shell function. Bash imports it
	// independently of PATH, defeating this intentionally absent-CLI fixture.
	t.Setenv("BASH_FUNC_claude%%", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	outcome, _, _ := detectClaudeCLI()
	if outcome != claudeProbeError {
		t.Fatalf("false-absent not suppressed: detectClaudeCLI outcome = %v, want claudeProbeError (keep-prior)", outcome)
	}
}

// TestRefreshClaudeStatusErrorKeepsVerdict is the regression test for the
// spurious "install Claude Code on WSL" banner: a probe error (timeout under
// post-auto-update load) must NOT downgrade a confirmed install — the cached
// status is kept wholesale.
func TestRefreshClaudeStatusErrorKeepsVerdict(t *testing.T) {
	a := &apiServer{}
	a.claudeStat = claudeStatus{checked: true, installed: true, version: "2.1.201 (Claude Code)", authed: true}
	a.detectClaude = func() (claudeProbeOutcome, string, bool) { return claudeProbeError, "", false }
	a.registerIntegrationsFn = func() {}

	a.refreshClaudeStatus()
	st := a.currentClaudeStatus()
	if !st.checked || !st.installed || st.version != "2.1.201 (Claude Code)" || !st.authed {
		t.Fatalf("probe error changed the cached verdict: %+v", st)
	}
}

// A probe error before any definitive verdict keeps checked=false — the UI
// stays on "checking…" instead of asserting "not installed" from a failure.
func TestRefreshClaudeStatusErrorBeforeFirstVerdict(t *testing.T) {
	a := &apiServer{}
	a.detectClaude = func() (claudeProbeOutcome, string, bool) { return claudeProbeError, "", false }

	a.refreshClaudeStatus()
	st := a.currentClaudeStatus()
	if st.checked || st.installed {
		t.Fatalf("probe error produced a verdict: %+v", st)
	}
}

// A DEFINITIVE absence still lands — but only once it is DURABLE: a confirmed
// install flips to "not installed" after claudeAbsentConfirmProbes consecutive
// absents spanning claudeAbsentConfirmWindow (a Claude self-update leaves the
// binary genuinely absent for seconds; one correctly-observed absent sample
// must not banner a user mid-update). Then version clears with the flag.
func TestRefreshClaudeStatusAbsentOverwritesOnlyWhenDurable(t *testing.T) {
	a := &apiServer{}
	a.claudeStat = claudeStatus{checked: true, installed: true, version: "2.1.201 (Claude Code)", authed: true}
	a.detectClaude = func() (claudeProbeOutcome, string, bool) { return claudeProbeAbsent, "", false }
	a.registerIntegrationsFn = func() {}
	clock := time.Unix(1_750_000_000, 0)
	a.nowFn = func() time.Time { return clock }

	// Absents within the window: verdict must hold at installed:true throughout.
	for i := 0; i < claudeAbsentConfirmProbes; i++ {
		a.refreshClaudeStatus()
		if st := a.currentClaudeStatus(); !st.installed {
			t.Fatalf("absent #%d inside the window flipped a confirmed install: %+v", i+1, st)
		}
		clock = clock.Add(15 * time.Second) // update-window sampling cadence
	}

	// Same streak, but now past the confirmation window → the flip lands.
	clock = clock.Add(claudeAbsentConfirmWindow)
	a.refreshClaudeStatus()
	st := a.currentClaudeStatus()
	if !st.checked || st.installed || st.version != "" || st.authed {
		t.Fatalf("durable absence not applied: %+v", st)
	}
}

// A found probe between absents resets the hysteresis — repeated update windows
// (each healing before the next) must never accumulate into a false flip.
func TestRefreshClaudeStatusFoundResetsAbsentStreak(t *testing.T) {
	a := &apiServer{}
	a.claudeStat = claudeStatus{checked: true, installed: true, version: "2.1.201 (Claude Code)"}
	a.registerIntegrationsFn = func() {}
	clock := time.Unix(1_750_000_000, 0)
	a.nowFn = func() time.Time { return clock }

	outcome := claudeProbeAbsent
	a.detectClaude = func() (claudeProbeOutcome, string, bool) { return outcome, "", false }

	for cycle := 0; cycle < 3; cycle++ {
		// Two absents (short of the probe threshold)…
		outcome = claudeProbeAbsent
		for i := 0; i < claudeAbsentConfirmProbes-1; i++ {
			a.refreshClaudeStatus()
			clock = clock.Add(claudeAbsentConfirmWindow) // ample elapsed time — count must still gate
		}
		// …healed by a found before the streak completes.
		outcome = claudeProbeFound
		a.refreshClaudeStatus()
	}
	if st := a.currentClaudeStatus(); !st.installed {
		t.Fatalf("repeated healed absent episodes accumulated into a flip: %+v", st)
	}
}

// The hysteresis protects only a CONFIRMED install: on a machine with no prior
// verdict (fresh install of soluterminal, no claude anywhere) the FIRST
// definitive absent must land immediately so onboarding can guide the user.
func TestRefreshClaudeStatusFirstAbsentVerdictImmediate(t *testing.T) {
	a := &apiServer{}
	a.detectClaude = func() (claudeProbeOutcome, string, bool) { return claudeProbeAbsent, "", false }
	a.registerIntegrationsFn = func() {}

	a.refreshClaudeStatus()
	st := a.currentClaudeStatus()
	if !st.checked || st.installed {
		t.Fatalf("first-ever absent verdict was damped: %+v", st)
	}
}

// TestClaudeProbeInterval pins the adaptive ticker cadence: brisk while absent,
// brisker while an absence is suspected-but-unconfirmed, relaxed once confirmed.
func TestClaudeProbeInterval(t *testing.T) {
	a := &apiServer{}
	if got := a.claudeProbeInterval(); got != claudeProbeIntervalMissing {
		t.Fatalf("unchecked/missing interval = %v, want %v", got, claudeProbeIntervalMissing)
	}
	a.claudeStat = claudeStatus{checked: true, installed: true}
	if got := a.claudeProbeInterval(); got != claudeProbeIntervalPresent {
		t.Fatalf("present interval = %v, want %v", got, claudeProbeIntervalPresent)
	}
	a.claudeAbsentStreak = 1
	if got := a.claudeProbeInterval(); got != claudeProbeIntervalSuspect {
		t.Fatalf("suspect interval = %v, want %v", got, claudeProbeIntervalSuspect)
	}
}

// Found with an empty version (a killed `claude --version` after presence was
// proven) keeps the last known version rather than blanking the UI; a fresh
// version replaces it.
func TestRefreshClaudeStatusFoundVersionBestEffort(t *testing.T) {
	a := &apiServer{}
	a.claudeStat = claudeStatus{checked: true, installed: true, version: "2.1.200 (Claude Code)"}
	a.detectClaude = func() (claudeProbeOutcome, string, bool) { return claudeProbeFound, "", false }
	a.registerIntegrationsFn = func() {}

	a.refreshClaudeStatus()
	if st := a.currentClaudeStatus(); st.version != "2.1.200 (Claude Code)" {
		t.Fatalf("empty best-effort version blanked the cache: %+v", st)
	}

	a.detectClaude = func() (claudeProbeOutcome, string, bool) { return claudeProbeFound, "2.1.201 (Claude Code)", false }
	a.refreshClaudeStatus()
	if st := a.currentClaudeStatus(); !st.installed || st.version != "2.1.201 (Claude Code)" {
		t.Fatalf("fresh version not adopted: %+v", st)
	}
}

// An API key signs Claude in for the nudge but is not the plan login a
// background run may bill (ST-97).
func TestRefreshClaudeStatusAPIKeyIsNotPlanLogin(t *testing.T) {
	a := &apiServer{}
	a.detectClaude = func() (claudeProbeOutcome, string, bool) { return claudeProbeFound, "2.1.310 (Claude Code)", true }
	a.registerIntegrationsFn = func() {}
	a.refreshClaudeStatus()
	st := a.currentClaudeStatus()
	if !st.authed {
		t.Fatalf("an API key must count as signed in: %+v", st)
	}
	if st.planLogin != (runtime.GOOS == "darwin") {
		t.Fatalf("an API key alone is not the plan login: %+v", st)
	}
}
