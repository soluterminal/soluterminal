package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"soluterminal-shared/releases"
)

func withSilentShellHost(t *testing.T, supported bool) {
	t.Helper()
	prev := silentShellHost
	silentShellHost = func() bool { return supported }
	t.Cleanup(func() { silentShellHost = prev })
}

// The unattended installer update on the team channel: the daemon fetches the
// team installer from the verified team manifest exactly as for a click, but
// hands it to the courier (no installer window) instead of opening it.
func TestTeamSilentInstallerGoesThroughTheVerifiedManifestToTheCourier(t *testing.T) {
	withSilentShellHost(t, true)
	r := teamInstallerRig(t)
	var couriers []string
	r.u.launchShellSilent = func(path, _, digest string, _ *shellLaunchLog) (launchResult, error) {
		if want := sha256.Sum256(r.payload); digest != hex.EncodeToString(want[:]) {
			t.Fatalf("courier given digest %q", digest)
		}
		couriers = append(couriers, path)
		return launchResult{Method: "silent-courier-test", PID: "42"}, nil
	}
	dataDir := t.TempDir()

	r.nameTheTeamEndpoint(t)
	if st := r.check(t, releases.FlavorTeam, "3.0.0"); !st.ShellAvailable || st.ShellLatest != "3.0.0" {
		t.Fatalf("team channel did not offer the team installer: %+v", st)
	}
	path, err := r.u.ApplyShellSilent(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("ApplyShellSilent: %v", err)
	}
	want := filepath.Join(dataDir, "shell-update", teamShellInstallerName)
	if path != want || len(couriers) != 1 || couriers[0] != want || len(r.launched) != 0 {
		t.Fatalf("staged %q couriers %v installer windows %v, want one courier for %s", path, couriers, r.launched, want)
	}
	if got, _ := os.ReadFile(path); string(got) != string(r.payload) {
		t.Fatalf("staged %q", got)
	}
	r.assertOnlyTeam(t)
	if st := r.u.Status(); st.ShellLaunch == nil || st.ShellLaunch.State != "launched" || st.ShellApplying {
		t.Fatalf("status after the courier started: %+v", st.ShellLaunch)
	}

	// A second window's countdown inside the courier's wait gets the same
	// answer, not a second courier.
	again, err := r.u.ApplyShellSilent(context.Background(), dataDir)
	if err != nil || again != path || len(couriers) != 1 {
		t.Fatalf("repeat inside the window: %q %v couriers %v", again, err, couriers)
	}
	// Close to the end of its wait the courier may give up before this app
	// closes, and a second courier would install twice: refused.
	r.u.mu.Lock()
	r.u.shellSilentAt = time.Now().Add(-silentCourierReuse)
	r.u.mu.Unlock()
	if _, err := r.u.ApplyShellSilent(context.Background(), dataDir); !errors.Is(err, errSilentCourierWaiting) || len(couriers) != 1 {
		t.Fatalf("near the end of the wait: %v couriers %v", err, couriers)
	}
	// Once that courier has given up, a later quiet window starts a fresh one.
	r.u.mu.Lock()
	r.u.shellSilentAt = time.Now().Add(-silentCourierWindow)
	r.u.mu.Unlock()
	if _, err := r.u.ApplyShellSilent(context.Background(), dataDir); err != nil || len(couriers) != 2 {
		t.Fatalf("after the window: %v couriers %v", err, couriers)
	}

	// Bytes that do not match the signed digest never reach the courier.
	r.u.mu.Lock()
	r.u.shellSilentAt = time.Time{}
	r.u.mu.Unlock()
	r.cloud.files.bodies[r.team+"download/v3.0.0/"+teamShellInstallerName] = []byte("tampered installer")
	if _, err := r.u.ApplyShellSilent(context.Background(), dataDir); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("tampered installer: %v", err)
	}
	if len(couriers) != 2 {
		t.Fatalf("courier started for a tampered installer: %v", couriers)
	}
}

func silentTestUpdater(t *testing.T, current, shell string) (*Updater, *shellCountingRoundTripper, *int, *int) {
	t.Helper()
	payload := []byte("verified installer")
	sum := sha256.Sum256(payload)
	url := "https://updates.example.test/setup.exe"
	u := NewUpdater(current, "0.0.1", "windows-amd64", nil, nil)
	u.homeDir = t.TempDir()
	u.manifest = &Manifest{
		Version: current,
		Shell: &ManifestShell{Version: shell, Platforms: map[string]ManifestPlatform{
			"windows-amd64": {URL: url, SHA256: hex.EncodeToString(sum[:])},
		}},
	}
	transport := &shellCountingRoundTripper{responses: staticRoundTripper{url: payload}}
	u.httpClient = &http.Client{Transport: transport}
	u.shellDisplayPath = func(path, _ string) (string, error) { return path, nil }
	// This public-channel updater has the silent restart switched on, as a
	// published control file would (it is off by default there).
	on := false
	u.ctrlEffective = &UpdateControl{ShellAutoApplyPaused: &on}
	windows, couriers := 0, 0
	u.launchShell = func(string, string, *shellLaunchLog) (launchResult, error) {
		windows++
		return launchResult{Method: "test"}, nil
	}
	u.launchShellSilent = func(string, string, string, *shellLaunchLog) (launchResult, error) {
		couriers++
		return launchResult{Method: "silent-test"}, nil
	}
	return u, transport, &windows, &couriers
}

func TestUpdateApplyShellSilentQuery(t *testing.T) {
	withSilentShellHost(t, true)
	u, _, windows, couriers := silentTestUpdater(t, "0.0.900", "0.0.901")
	a := &apiServer{updater: u, dataDir: t.TempDir()}

	rec := httptest.NewRecorder()
	a.updateApplyShell(rec, httptest.NewRequest(http.MethodPost, "/api/update/apply-shell?silent=1", nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body["silent"] != true || *couriers != 1 || *windows != 0 {
		t.Fatalf("silent=1: %d %v couriers=%d windows=%d", rec.Code, body, *couriers, *windows)
	}

	// Only the exact query is the unattended mode; anything else is the
	// ordinary attended request and says so.
	u.mu.Lock()
	u.shellSilentAt = time.Time{}
	u.mu.Unlock()
	rec = httptest.NewRecorder()
	a.updateApplyShell(rec, httptest.NewRequest(http.MethodPost, "/api/update/apply-shell?silent=1&force=1", nil))
	body = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body["silent"] != false || *couriers != 1 || *windows != 1 {
		t.Fatalf("non-exact query: %d %v couriers=%d windows=%d", rec.Code, body, *couriers, *windows)
	}
}

// The unattended mode never overrides the downgrade refusal.
func TestSilentInstallerNeverForcesADowngrade(t *testing.T) {
	withSilentShellHost(t, true)
	u, transport, windows, couriers := silentTestUpdater(t, "0.0.932", "0.0.916")
	_, err := u.ApplyShellSilent(context.Background(), t.TempDir())
	var refusal *shellDaemonDowngradeError
	if !errors.As(err, &refusal) || transport.calls != 0 || *couriers != 0 || *windows != 0 {
		t.Fatalf("downgrade: %v downloads=%d couriers=%d windows=%d", err, transport.calls, *couriers, *windows)
	}
	if _, err := u.applyShell(context.Background(), t.TempDir(), true, true); err == nil {
		t.Fatal("silent + force accepted")
	}
}

func TestSilentInstallerRefusedWhereNoCourierCanRun(t *testing.T) {
	withSilentShellHost(t, false)
	u, transport, windows, couriers := silentTestUpdater(t, "0.0.900", "0.0.901")
	a := &apiServer{updater: u, dataDir: t.TempDir()}
	rec := httptest.NewRecorder()
	a.updateApplyShell(rec, httptest.NewRequest(http.MethodPost, "/api/update/apply-shell?silent=1", nil))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "shell_silent_unsupported") {
		t.Fatalf("unsupported host: %d %s", rec.Code, rec.Body.String())
	}
	if transport.calls != 0 || *couriers != 0 || *windows != 0 || u.shellApplying {
		t.Fatalf("unsupported host still acted: downloads=%d couriers=%d windows=%d applying=%v", transport.calls, *couriers, *windows, u.shellApplying)
	}
}

func TestSilentCourierScript(t *testing.T) {
	installer := `\\wsl.localhost\Ubuntu\home\o'brien\.solu\shell-update\setup.exe`
	script := silentCourierScript(installer, `\\wsl.localhost\Ubuntu\home\o'brien\.solu\shell-update\launch.log`, "7-9", "ABCDEF0123", silentAppProcessName, silentCourierAppExitWait)
	for _, want := range []string{
		`$installer = '\\wsl.localhost\Ubuntu\home\o''brien\.solu\shell-update\setup.exe'`,
		`$attempt = '7-9'`,
		`$appName = 'soluterminal'`,
		`$expectedSha = 'ABCDEF0123'`,
		`(Get-FileHash -Algorithm SHA256 -LiteralPath $installer -ErrorAction Stop).Hash`,
		`if ([string]::IsNullOrEmpty($actualSha)) { throw`,
		`[System.StringComparison]::OrdinalIgnoreCase`,
		`$psi.Arguments = '/S'`,
		`AddSeconds(60)`,
		// Nothing installs until every app in the session exited, and every
		// app that exited is started again, installed or not.
		`while (Get-Process -Id $appIds -ErrorAction SilentlyContinue)`,
		`foreach ($appExe in $appPaths) {`,
		`Where-Object { $_.Path -eq $appExe }) { continue }`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("courier script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "o'brien") {
		t.Fatal("unescaped apostrophe in the courier script")
	}

	line := silentCourierCommandLine(script)
	if len(line) >= 32767 {
		t.Fatalf("courier command line is %d characters; CreateProcess allows 32767", len(line))
	}
	if decodeEncodedCommand(t, line) != script {
		t.Fatal("encoded command does not decode to the courier script")
	}

	courier := `C:\Users\Zoë o'brien\.solu\shell-update\silent-update.ps1`
	cmd := silentCmdScript(courier)
	for _, r := range cmd {
		if r > 0x7f {
			t.Fatalf("cmd script is not ASCII: %q", cmd)
		}
	}
	if !strings.HasPrefix(cmd, `start "" /min powershell.exe `) || !strings.HasSuffix(cmd, "\r\n") {
		t.Fatalf("cmd script = %q", cmd)
	}
	if got := decodeEncodedCommand(t, strings.TrimSuffix(cmd, "\r\n")); got != `& 'C:\Users\Zoë o''brien\.solu\shell-update\silent-update.ps1'` {
		t.Fatalf("cmd runs %q", got)
	}
}

func decodeEncodedCommand(t *testing.T, line string) string {
	t.Helper()
	i := strings.Index(line, "-EncodedCommand ")
	if i < 0 {
		t.Fatalf("no -EncodedCommand in %q", line)
	}
	raw, err := base64.StdEncoding.DecodeString(line[i+len("-EncodedCommand "):])
	if err != nil || len(raw)%2 != 0 {
		t.Fatalf("encoded command: %v", err)
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	return string(utf16.Decode(units))
}

func TestWaitForSilentCourierReadsOnlyItsAttempt(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "launch.log")
	lines := "\ufeff" + `{"event":"courier-started","attempt":"old","pid":"1"}` + "\n" +
		`{"event":"staged","path":"x"}` + "\n"
	if err := os.WriteFile(logPath, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := waitForSilentCourier(logPath, "new", 300*time.Millisecond); !errors.Is(err, errLaunchNotObserved) {
		t.Fatalf("another attempt's line counted: %v", err)
	}
	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString("\ufeff" + `{"event":"courier-started","attempt":"new","pid":"77"}` + "\n")
	_ = f.Close()
	if pid, err := waitForSilentCourier(logPath, "new", time.Second); err != nil || pid != "77" {
		t.Fatalf("pid %q err %v", pid, err)
	}

	failed := filepath.Join(t.TempDir(), "launch.log")
	_ = os.WriteFile(failed, []byte(`{"event":"failed","attempt":"a","err":"the desktop app is not running in this session"}`+"\n"), 0o600)
	if _, err := waitForSilentCourier(failed, "a", time.Second); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("early failure: %v", err)
	}
}

// The courier itself, run by a real PowerShell (pwsh on PATH, or the binary
// SOLU_TEST_PWSH names) against a stand-in app and installer. Skipped where
// no PowerShell is installed; the script text is covered above either way.
func TestSilentCourierUnderPowerShell(t *testing.T) {
	pwsh := os.Getenv("SOLU_TEST_PWSH")
	if pwsh == "" {
		pwsh, _ = exec.LookPath("pwsh")
	}
	if pwsh == "" {
		t.Skip("no PowerShell (pwsh) available")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary for the stand-in app")
	}
	const appName = "solucourierapp"

	run := func(t *testing.T, swap bool) (dir string, log []map[string]string) {
		dir = t.TempDir()
		app := filepath.Join(dir, appName)
		raw, err := os.ReadFile(sleep)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(app, raw, 0o755); err != nil {
			t.Fatal(err)
		}
		installer := filepath.Join(dir, "setup.exe")
		verified := "#!/bin/sh\necho \"$@\" > '" + filepath.Join(dir, "installed") + "'\n"
		if err := os.WriteFile(installer, []byte(verified), 0o755); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(verified))
		if swap {
			// Replaced after the daemon verified it, while the courier waits.
			swapped := "#!/bin/sh\necho swapped > '" + filepath.Join(dir, "installed") + "'\n"
			if err := os.WriteFile(installer, []byte(swapped), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		appProc := exec.Command(app, "2")
		if err := appProc.Start(); err != nil {
			t.Fatal(err)
		}
		// Reap it as soon as it exits, as Windows would: a zombie still lists.
		go func() { _ = appProc.Wait() }()

		logPath := filepath.Join(dir, "launch.log")
		// Upper-case digest: the comparison must ignore case.
		script := silentCourierScript(installer, logPath, "t1", strings.ToUpper(hex.EncodeToString(sum[:])), appName, 20*time.Second)
		scriptPath := filepath.Join(dir, "courier.ps1")
		if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-File", scriptPath).CombinedOutput()
		log = silentCourierEntries(logPath, "t1")
		if len(log) == 0 {
			t.Fatalf("courier wrote nothing; output:\n%s", out)
		}
		return dir, log
	}
	events := func(log []map[string]string) string {
		var names []string
		for _, entry := range log {
			names = append(names, entry["event"])
		}
		return strings.Join(names, ",")
	}

	t.Run("matching digest installs silently and starts the app again", func(t *testing.T) {
		dir, log := run(t, false)
		got, err := os.ReadFile(filepath.Join(dir, "installed"))
		if err != nil || strings.TrimSpace(string(got)) != "/S" {
			t.Fatalf("installer not run with /S: %q %v (log %s)", got, err, events(log))
		}
		if e := events(log); e != "courier-started,app-exited,installer-done,relaunched" {
			t.Fatalf("events %s", e)
		}
		if event, _ := silentCourierFinal(filepath.Join(dir, "launch.log"), "t1"); event != "installer-done" {
			t.Fatalf("final %q", event)
		}
	})

	t.Run("a swapped installer installs nothing, is removed, and the app starts again", func(t *testing.T) {
		dir, log := run(t, true)
		if _, err := os.Stat(filepath.Join(dir, "installed")); !os.IsNotExist(err) {
			t.Fatalf("swapped installer ran (log %s)", events(log))
		}
		if _, err := os.Stat(filepath.Join(dir, "setup.exe")); !os.IsNotExist(err) {
			t.Fatalf("swapped installer left in place: %v", err)
		}
		if e := events(log); e != "courier-started,app-exited,sha-mismatch,failed,relaunched" {
			t.Fatalf("events %s", e)
		}
	})
}

// After the daemon answered, a courier's sha-mismatch becomes a rejected
// launch with no path to reveal or run.
func TestSilentCourierShaMismatchIsSurfaced(t *testing.T) {
	u, _, _, _ := silentTestUpdater(t, "0.0.900", "0.0.901")
	dir := t.TempDir()
	u.shellLaunchLog = newShellLaunchLog(dir)
	u.noteSilentShellLaunchLocked("0.0.901", filepath.Join(dir, "setup.exe"), "a1", time.Now())
	if st := u.Status(); st.ShellLaunch != nil {
		t.Fatalf("status before the courier reported: %+v", st.ShellLaunch)
	}
	line := `{"event":"sha-mismatch","attempt":"a1","detail":"expected AA, found BB"}` + "\n"
	if err := os.WriteFile(u.shellLaunchLog.path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	st := u.Status()
	if st.ShellLaunch == nil || st.ShellLaunch.State != "rejected" || st.ShellLaunch.StagedPath != "" ||
		!strings.Contains(st.ShellLaunch.Error, "changed after it was verified") {
		t.Fatalf("status = %+v", st.ShellLaunch)
	}
	// That courier is finished: a new request starts a new one.
	if _, reuse, err := u.recentSilentShellLaunchLocked("0.0.901", time.Now()); reuse || err != nil {
		t.Fatalf("finished courier still reused: %v %v", reuse, err)
	}

	// A courier that never reports is not read for ever.
	u.noteSilentShellLaunchLocked("0.0.901", filepath.Join(dir, "setup.exe"), "silent-forever", time.Now().Add(-silentCourierObserveFor-time.Second))
	_ = u.Status()
	if u.shellSilentAttempt != "" {
		t.Fatal("still watching a courier long past its wait")
	}
}

// shell_auto_apply_paused in the signed control file: absent means on for
// team and off for public; true and false are explicit on both channels; the
// global kill switch also stops it. Only the unattended request is refused.
func TestShellAutoApplyPausedByChannelAndControl(t *testing.T) {
	withSilentShellHost(t, true)
	yes, no := true, false
	cases := []struct {
		name    string
		channel string
		ctrl    *UpdateControl
		paused  bool
	}{
		{"team, no control file", releases.FlavorTeam, nil, false},
		{"team, field absent", releases.FlavorTeam, &UpdateControl{Schema: 1}, false},
		{"team, paused true", releases.FlavorTeam, &UpdateControl{ShellAutoApplyPaused: &yes}, true},
		{"team, paused false", releases.FlavorTeam, &UpdateControl{ShellAutoApplyPaused: &no}, false},
		{"team, global pause", releases.FlavorTeam, &UpdateControl{Paused: true, ShellAutoApplyPaused: &no}, true},
		{"public, no control file", releases.FlavorPublic, nil, true},
		{"public, field absent", releases.FlavorPublic, &UpdateControl{Schema: 1}, true},
		{"public, paused true", releases.FlavorPublic, &UpdateControl{ShellAutoApplyPaused: &yes}, true},
		{"public, paused false", releases.FlavorPublic, &UpdateControl{ShellAutoApplyPaused: &no}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, transport, windows, couriers := silentTestUpdater(t, "0.0.900", "0.0.901")
			u.channel = tc.channel
			u.ctrlEffective = tc.ctrl
			if st := u.Status(); st.ShellAutoApplyPaused != tc.paused || (tc.paused && st.ShellAutoApplyPausedReason == "") {
				t.Fatalf("status paused=%v reason=%q, want paused=%v", st.ShellAutoApplyPaused, st.ShellAutoApplyPausedReason, tc.paused)
			}
			a := &apiServer{updater: u, dataDir: t.TempDir()}
			rec := httptest.NewRecorder()
			a.updateApplyShell(rec, httptest.NewRequest(http.MethodPost, "/api/update/apply-shell?silent=1", nil))
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if tc.paused {
				if rec.Code != http.StatusConflict || body["code"] != "shell_silent_paused" || body["paused"] != true || transport.calls != 0 || *couriers != 0 {
					t.Fatalf("paused: %d %v downloads=%d couriers=%d", rec.Code, body, transport.calls, *couriers)
				}
			} else if rec.Code != http.StatusOK || body["silent"] != true || *couriers != 1 {
				t.Fatalf("on: %d %v couriers=%d", rec.Code, body, *couriers)
			}
			// A clicked installer update is never affected.
			before := *windows
			rec = httptest.NewRecorder()
			a.updateApplyShell(rec, httptest.NewRequest(http.MethodPost, "/api/update/apply-shell", nil))
			if rec.Code != http.StatusOK || *windows != before+1 {
				t.Fatalf("clicked update: %d %s windows=%d", rec.Code, rec.Body.String(), *windows)
			}
		})
	}
}

// A pause published while the installer downloads still stops the courier.
func TestShellAutoApplyPausedDuringDownload(t *testing.T) {
	withSilentShellHost(t, true)
	u, _, _, couriers := silentTestUpdater(t, "0.0.900", "0.0.901")
	yes := true
	u.shellDisplayPath = func(path, _ string) (string, error) {
		u.mu.Lock()
		u.ctrlEffective = &UpdateControl{ShellAutoApplyPaused: &yes}
		u.mu.Unlock()
		return path, nil
	}
	_, err := u.ApplyShellSilent(context.Background(), t.TempDir())
	var paused *silentShellPausedError
	if !errors.As(err, &paused) || *couriers != 0 {
		t.Fatalf("paused mid-download: %v couriers=%d", err, *couriers)
	}
}
