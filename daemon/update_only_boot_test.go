package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// updateOnlyBootCalls is every function update_only_boot.go may call. The
// update-only boot must never reach the normal boot sequence or any engine
// (scheduler, release train, panes, agent cleanup, registrations,
// relay, pairing, check-in, update poll, memory guard), so a new call here is
// a deliberate, reviewed change: add it to this list with care.
var updateOnlyBootCalls = map[string]bool{
	// standard library
	"context.Background": true, "context.WithTimeout": true, "errors.New": true, "errors.Is": true,
	"flag.NewFlagSet": true, "fmt.Errorf": true, "log.Printf": true, "log.Print": true,
	"net.Listen": true, "os.Getenv": true, "os.Unsetenv": true, "os.UserHomeDir": true,
	"signal.NotifyContext": true, "strings.TrimSpace": true, "json.NewEncoder": true,
	"make": true, "stop": true, "cancel": true, "h": true,
	"fs.StringVar": true, "fs.String": true, "fs.Bool": true, "fs.Parse": true, "fs.NArg": true, "fs.Arg": true,
	"ln.Addr": true, "srv.Serve": true, "srv.Shutdown": true, "w.Header": true, "w.WriteHeader": true,
	"http.HandlerFunc": true, "ctx.Done": true, "defaultDataDir": true,
	// chained calls: json.NewEncoder(w).Encode, w.Header().Set, ln.Addr().String
	"?.Encode": true, "?.Set": true, "?.String": true,
	// this file
	"parseUpdateOnlyFlags": true, "runUpdateOnlyBootContext": true, "newUpdateOnlyHandler": true,
	"serveUpdateOnly": true, "updateOnlyRouter": true, "writeUpdateOnlyRefusal": true,
	"updateOnlyDaemon": true, "updaterHasWhatItNeeds": true, "u.cloudCreds": true, "u.channelSnapshot": true,
	// what the update needs
	"readShellInfo": true, "claimDataDirForBoot": true, "lock.Release": true, "loadEnvFile": true,
	"daemonListenerNetwork": true, "isInsideWSL": true, "describeListenConflict": true,
	"NewUpdater": true, "installCohortID": true, "newUpdateRollbackStore": true,
	"loadClaimSafe": true, "httpsBaseFromRelayURL": true, "newTeamEntitlements": true,
	"newUpdateChannelStore": true, "entitlementChannelEvidence": true,
	"updater.SetCloudTeamUpdates": true, "updater.SetCloudCreds": true, "entitlements.refreshMachine": true,
	"loadOrCreateToken": true, "gate.middleware": true, "maxRequestBodyMiddleware": true,
	"logMiddleware": true, "writeJSON": true, "a.liveHealthIdentity": true, "a.updateApplyShell": true,
	// Observational compatibility counters; no services or boot work.
	"observeRequestMedia": true,
}

// The structural guard: every call in update_only_boot.go is on the list.
func TestUpdateOnlyBootCallsOnlyWhatTheUpdateNeeds(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "update_only_boot.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			if x, ok := fn.X.(*ast.Ident); ok {
				name = x.Name + "." + fn.Sel.Name
			} else {
				name = "?." + fn.Sel.Name
			}
		case *ast.FuncLit:
			return true
		default:
			name = "?"
		}
		seen[name] = true
		return true
	})
	var unexpected []string
	for name := range seen {
		if !updateOnlyBootCalls[name] {
			unexpected = append(unexpected, name)
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Fatalf("update_only_boot.go calls %v, which are not on updateOnlyBootCalls. The update-only boot may start only what the installer update needs.", unexpected)
	}
}

func updateOnlySnapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(root, path)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	return out
}

func updateOnlyChangedPaths(before, after map[string]string) []string {
	var out []string
	for path, sum := range after {
		if before[path] != sum {
			out = append(out, path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			out = append(out, path+" (deleted)")
		}
	}
	sort.Strings(out)
	return out
}

func runInUpdateOnlyTestProcess(t *testing.T) bool {
	t.Helper()
	// Keep the complete process-wide engine assertion, but give this boot its
	// own process. Earlier package tests may leave scheduler/manager goroutines
	// running; those say nothing about what this boot starts. Starting with the
	// real marker also exercises constructor guards after the env is scrubbed.
	if os.Getenv("SOLU_TEST_UPDATE_ONLY_CASE") != t.Name() {
		cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v", "-test.timeout=30s")
		cmd.Env = append(os.Environ(), "SOLU_TEST_UPDATE_ONLY_CASE="+t.Name(), updateOnlyEnv+"=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated update-only boot: %v\n%s", err, out)
		}
		return false
	}
	if !updateOnlyDaemon() {
		t.Fatal("child must start in update-only mode")
	}
	return true
}

// Private test files extend this list for engines absent from the public build.
var updateOnlyForbiddenEngines = []string{
	"(*ScheduleStore)", "startActivityWatcher", "(*relaySupervisor).run", "StartBackgroundPoll",
	"ensureClaudeMcpRegistration", "ensureCodexMcpRegistration", "ensureClaudeHooksRegistered",
	"(*PortRegistry)", "(*teamEntitlements).run", "startInstallCheckin", "(*pairingManager)",
	"releaseTrain", "memguard", "(*Manager).", "reapOrphanAgents",
}

// An update-only daemon, booted for real under a scratch HOME with saved
// panes, schedules, settings, a claim and Claude/Codex config present:
//   - it answers only the four update routes and refuses everything else;
//   - it starts none of the normal engines (no goroutine of theirs runs);
//   - the exact set of files it writes is the update's own state.
func TestUpdateOnlyBootWritesOnlyTheUpdatesOwnState(t *testing.T) {
	if !runInUpdateOnlyTestProcess(t) {
		return
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SOLU_DURABLE_LOG_DIR", "off")
	// The data-dir lock is taken as on any boot (an inherited successor flag
	// or lock override would skip it and make the write set vary).
	t.Setenv("SOLU_SUCCESSOR_OF_UPDATE", "")
	t.Setenv("SOLU_DATA_DIR_LOCK", "")
	// The boot loads the seeded env file into this process's environment;
	// register the key so t.Setenv restores it afterwards, then clear it so
	// the file really is what sets it.
	t.Setenv("SOLU_EXAMPLE", "")
	_ = os.Unsetenv("SOLU_EXAMPLE")
	prevLock := dataDirLockHeld
	t.Cleanup(func() { dataDirLockHeld = prevLock })
	data := filepath.Join(home, ".solu")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}

	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/entitlements" && r.Header.Get("Authorization") == "Bearer machine-tok" {
			_, _ = w.Write([]byte(`{"team":true,"tier":"team"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer cloud.Close()
	manifest := httptest.NewServer(http.NotFoundHandler())
	defer manifest.Close()
	t.Setenv("SOLU_UPDATE_MANIFEST_URL", manifest.URL+"/update.json")

	seed := map[string]string{
		".solu/panes.json":           `[{"id":"p1","title":"bash","cmd":["bash"]}]`,
		".solu/schedules.json":       `[{"id":"s1","cron":"* * * * *","enabled":true}]`,
		".solu/update_settings.json": `{"autoApply":"when-idle","shellAutoApply":"when-idle"}`,
		".solu/env":                  "SOLU_EXAMPLE=1\n",
		".claude/settings.json":      `{"hooks":{}}`,
		".claude.json":               `{"mcpServers":{}}`,
		".codex/config.toml":         "model = \"x\"\n",
	}
	for rel, body := range seed {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveClaim(data, &Claim{RelayURL: "ws://x/relay", Token: "machine-tok", MachineID: "m", MachineName: "n", ClaimedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	// Existing update state: the auth token, a valid entitlements cache and a
	// team channel decision. The boot must not rotate the token, must leave a
	// valid cache, and must not switch the channel unless the verdict says so.
	const existingToken = "existing-auth-token-0123456789abcdef"
	if err := os.WriteFile(filepath.Join(data, "token"), []byte(existingToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seeded := newTeamEntitlements(data, func() (string, string) { return "", "" })
	seeded.store(recordFromVerdict(cloudVerdict{Team: true, Tier: "team"}, "machine", "", time.Now().Add(-time.Hour)))
	channelSeed := `{"channel":"team","reason":"account","decidedAt":"2026-10-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(data, updateChannelFile), []byte(channelSeed), 0o600); err != nil {
		t.Fatal(err)
	}
	before := updateOnlySnapshotTree(t, home)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	exited := make(chan int, 1)
	relay := strings.Replace(cloud.URL, "http://", "ws://", 1) + "/relay"
	go func() {
		exited <- runUpdateOnlyBootContext(ctx, []string{
			"--addr", "127.0.0.1:0", "--data", data, "--relay", relay,
			"--backend-id", "host", "--shell-version", "0.0.1225", "--shell-platform", "windows-amd64",
		}, ready)
	}()
	var addr string
	select {
	case addr = <-ready:
	case code := <-exited:
		t.Fatalf("update-only boot exited early with %d", code)
	case <-time.After(10 * time.Second):
		t.Fatal("update-only boot did not start listening")
	}
	base := "http://" + addr

	do := func(method, path string) (int, map[string]any) {
		req, _ := http.NewRequest(method, base+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}

	code, health := do("GET", "/api/health?identity=1")
	if code != 200 || health["updateOnly"] != true || health["updaterReady"] != true || health["backendID"] != "host" {
		t.Fatalf("health = %d %v", code, health)
	}
	for key, value := range health {
		switch value.(type) {
		case bool, float64:
		case string:
			if key != "status" && key != "version" && key != "backendID" {
				t.Fatalf("health carries a string field %q", key)
			}
		default:
			t.Fatalf("health field %q has type %T", key, value)
		}
	}
	if code, _ := do("GET", "/api/update/status"); code != 200 {
		t.Fatalf("status = %d", code)
	}
	if code, _ := do("POST", "/api/update/check"); code != 200 {
		t.Fatalf("check = %d", code)
	}
	if code, body := do("POST", "/api/update/apply-shell?silent=1"); code != 400 || body["code"] != "update_only_attended" {
		t.Fatalf("silent apply-shell = %d %v", code, body)
	}
	if code, body := do("POST", "/api/update/apply-shell?force=1"); code != 400 || body["code"] != "update_only_attended" {
		t.Fatalf("forced apply-shell = %d %v", code, body)
	}
	// No manifest was served, so the attended update has nothing to install.
	if code, _ := do("POST", "/api/update/apply-shell"); code == 200 {
		t.Fatal("apply-shell launched something with no manifest")
	}
	for _, probe := range [][2]string{
		{"GET", "/"}, {"GET", "/api/panes"}, {"GET", "/api/health/full"}, {"POST", "/api/update/apply"},
		{"GET", "/api/schedules"}, {"POST", "/api/cloud/pair/start"}, {"GET", "/mcp"}, {"POST", "/api/update/status"},
		{"GET", "/api/update/check"}, {"GET", "/debug/pprof/"}, {"GET", "/ws"},
	} {
		code, body := do(probe[0], probe[1])
		if code != http.StatusNotFound || body["code"] != "update_only" {
			t.Fatalf("%s %s = %d %v, want the update-only refusal", probe[0], probe[1], code, body)
		}
	}

	// None of the normal engines runs in this process because of this boot.
	buf := make([]byte, 1<<22)
	stacks := string(buf[:runtime.Stack(buf, true)])
	for _, engine := range updateOnlyForbiddenEngines {
		if strings.Contains(stacks, engine) {
			t.Fatalf("an engine is running under the update-only boot: %s", engine)
		}
	}

	cancel()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("update-only boot did not stop")
	}

	// The env file was loaded into this process (restored by t.Setenv).
	if os.Getenv("SOLU_EXAMPLE") != "1" {
		t.Fatal("the data dir's env file was not loaded")
	}

	// The existing token is byte-identical: never rotated.
	if b, _ := os.ReadFile(filepath.Join(data, "token")); string(b) != existingToken+"\n" {
		t.Fatalf("the auth token changed: %q", b)
	}
	// The entitlements cache is still a valid team verdict.
	var cache entitlementRecord
	if b, err := os.ReadFile(filepath.Join(data, entitlementsFile)); err != nil || json.Unmarshal(b, &cache) != nil {
		t.Fatalf("the entitlements cache is unreadable: %v", err)
	}
	if !cache.Team || cache.Tier != "team" || cache.CheckedAt.IsZero() {
		t.Fatalf("the entitlements cache lost its verdict: %+v", cache)
	}
	// The channel stays team: the (team) verdict does not call for a switch.
	var channel updateChannelRecord
	if b, err := os.ReadFile(filepath.Join(data, updateChannelFile)); err != nil || json.Unmarshal(b, &channel) != nil {
		t.Fatalf("the channel decision is unreadable: %v", err)
	}
	if channel.Channel != "team" {
		t.Fatalf("the channel switched to %q", channel.Channel)
	}

	// The exact set of files the update-only boot wrote.
	got := updateOnlyChangedPaths(before, updateOnlySnapshotTree(t, home))
	want := updateOnlyBootWrites
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("update-only boot wrote %v, want exactly %v", got, want)
	}
}

// updateOnlyBootWrites is the whole set of files an update-only boot may
// change (relative to HOME) on an install with existing update state, for a
// check that found no installer update:
//   - .solu/.daemon.lock: the data-dir lock every daemon takes, which keeps a
//     second daemon off the same data dir;
//   - .solu/entitlements.json: the one machine entitlements check refreshes
//     the cache (still a valid verdict, asserted above);
//   - .solu/update_channel.json: the updater's channel decision, kept on the
//     same channel (asserted above).
//
// The existing token is not among them (never rotated). On a fresh install
// .solu/token is created, as any daemon does. The installer staging dir
// (.solu/shell-update/) and its launch log are added only when an installer
// is actually downloaded.
var updateOnlyBootWrites = []string{
	".solu/.daemon.lock",
	".solu/entitlements.json",
	".solu/update_channel.json",
}

// A restart path in update-only mode exits instead (spawnSuccessor), and the
// marker counts only from the start environment.
func TestUpdateOnlyMarkerIsReadFromTheStartEnvironmentOnly(t *testing.T) {
	if !runInUpdateOnlyTestProcess(t) {
		return
	}
	prev := bootUpdateOnlyEnv
	t.Cleanup(func() { bootUpdateOnlyEnv = prev })
	bootUpdateOnlyEnv = ""
	t.Setenv(updateOnlyEnv, "1")
	if updateOnlyDaemon() {
		t.Fatal("a marker set after start must not count")
	}
	bootUpdateOnlyEnv = "1"
	if !updateOnlyDaemon() {
		t.Fatal("the start marker must count")
	}
}

func TestUpdateOnlyFlagsRefuseClaimAndStrayArguments(t *testing.T) {
	if _, err := parseUpdateOnlyFlags([]string{"--claim"}); err == nil {
		t.Fatal("--claim must be refused")
	}
	if _, err := parseUpdateOnlyFlags([]string{"--addr", "127.0.0.1:1", "agent"}); err == nil {
		t.Fatal("a stray argument must be refused")
	}
	cfg, err := parseUpdateOnlyFlags([]string{"--addr", "127.0.0.1:1", "--relay", "wss://c/relay", "--backend-id", "wsl", "--shell-version", "1", "--shell-platform", "windows-amd64", "--relay-token", "x"})
	if err != nil || cfg.addr != "127.0.0.1:1" || cfg.backendID != "wsl" || cfg.relayURL != "wss://c/relay" {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
}

// The successor path exits in update-only mode instead of starting another
// daemon. Run in a child process, since it exits.
func TestUpdateOnlySuccessorPathExitsInstead(t *testing.T) {
	if os.Getenv("SOLU_TEST_UPDATE_ONLY_SUCCESSOR") == "1" {
		bootUpdateOnlyEnv = "1"
		_, _ = spawnSuccessor(filepath.Join(t.TempDir(), "solud"), "0.0.1", "marker")
		os.Exit(3) // reached only if spawnSuccessor did not exit
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUpdateOnlySuccessorPathExitsInstead$")
	cmd.Env = append(os.Environ(), "SOLU_TEST_UPDATE_ONLY_SUCCESSOR=1", "SOLU_DURABLE_LOG_DIR=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the successor path did not exit cleanly: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "refusing to start a successor") {
		t.Fatalf("no refusal logged:\n%s", out)
	}
}

// updaterReady means an update check can run: always on the public channel,
// and on the team channel only once the updater has its cloud credentials.
func TestUpdaterReadyMeansTheUpdaterHasWhatItNeeds(t *testing.T) {
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.mu.Lock()
	u.channel = "team"
	u.mu.Unlock()
	if updaterHasWhatItNeeds(u) {
		t.Fatal("a team-channel updater without cloud credentials is not ready")
	}
	u.SetCloudCreds("https://cloud.example", "machine-tok")
	if !updaterHasWhatItNeeds(u) {
		t.Fatal("with its credentials it is ready")
	}
	pub := NewUpdater("1.0.0", "", "", nil, nil)
	pub.mu.Lock()
	pub.channel = "public"
	pub.mu.Unlock()
	if !updaterHasWhatItNeeds(pub) {
		t.Fatal("the public channel needs no cloud credentials")
	}
	if updaterHasWhatItNeeds(nil) {
		t.Fatal("no updater is not ready")
	}
}
