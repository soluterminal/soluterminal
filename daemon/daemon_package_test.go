package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain keeps the package hermetic.
//
// Daemon-spawned shells (panes, steward sessions, and publish.sh's test gate)
// export SOLU_RELEASE_REPO, SOLU_RELEASES_REPO, and the workflow resource-cap
// variables. The source-repo resolvers (releaseCutRepoPath, releaseLedgerRepoKey,
// declareReleaseCut, releaseReadinessProber.RepoPath, and compute) and the
// resolve…Override cap resolvers read those variables first, so host values silently
// replace test fixture repos and caps. On 2026-08-15 that made publish.sh refuse
// v0.0.1001 after release tests inspected the real repository.
//
// This cannot live in per-test t.Setenv calls: the affected
// TestReleaseCutAPI* and TestReleaseReadiness* tests use t.Parallel, where
// t.Setenv panics. Tests that deliberately exercise these overrides are
// non-parallel and set them explicitly after this package-wide scrub.
//
// The suite also protects the enclosing repository's administrative link. On
// 2026-08-23, agent-generated test code running in a review worktree overwrote
// its .git gitdir pointer with /tmp/fake_git. A package-wide snapshot catches
// and restores that mutation even when the responsible test was generated at
// runtime rather than present in tracked source.
var (
	daemonPackageRequestedTimeout string
	daemonPackageActiveTimeout    string
)

// daemonPackageDefaultParallelism mirrors scripts/daemon-test-gate.sh's
// -parallel policy for trees that do not carry the script.
const daemonPackageDefaultParallelism = 8

func daemonPackageCanonicalParallelism() (int, error) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(sourceFile) {
		return 0, fmt.Errorf("resolve canonical daemon test gate from source path %q", sourceFile)
	}
	gatePath := filepath.Join(filepath.Dir(sourceFile), "..", "scripts", "daemon-test-gate.sh")
	data, err := os.ReadFile(gatePath)
	if errors.Is(err, os.ErrNotExist) {
		// A source tree without the repository's test gate (the public export)
		// uses the gate's policy as of this writing.
		return daemonPackageDefaultParallelism, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read canonical daemon test gate: %w", err)
	}
	match := regexp.MustCompile(`-parallel=([0-9]+)`).FindSubmatch(data)
	if match == nil {
		return 0, errors.New("canonical daemon test gate has no -parallel policy")
	}
	parallelism, err := strconv.Atoi(string(match[1]))
	if err != nil {
		return 0, fmt.Errorf("parse canonical daemon test parallelism %q: %w", match[1], err)
	}
	if parallelism < 1 {
		return 0, fmt.Errorf("canonical daemon test parallelism must be positive, got %d", parallelism)
	}
	return parallelism, nil
}

func daemonPackageParallelismExplicit(args []string) bool {
	for _, arg := range args {
		if arg == "-parallel" || arg == "-test.parallel" ||
			strings.HasPrefix(arg, "-parallel=") || strings.HasPrefix(arg, "-test.parallel=") {
			return true
		}
	}
	return false
}

func TestDaemonPackagePreservesRequestedTimeout(t *testing.T) {
	timeoutFlag := flag.Lookup("test.timeout")
	if timeoutFlag == nil {
		t.Fatal("test.timeout flag is unavailable")
	}
	if _, err := time.ParseDuration(timeoutFlag.Value.String()); err != nil {
		t.Fatalf("parse active timeout: %v", err)
	}
	if timeoutFlag.Value.String() != daemonPackageActiveTimeout {
		t.Fatalf("active timeout changed after TestMain from %s to %s", daemonPackageActiveTimeout, timeoutFlag.Value.String())
	}
	requested, err := time.ParseDuration(daemonPackageRequestedTimeout)
	if err != nil {
		t.Fatalf("parse requested timeout: %v", err)
	}
	if requested == 10*time.Minute {
		if timeoutFlag.Value.String() != (15 * time.Minute).String() {
			t.Fatalf("implicit Go timeout was not extended: requested=%s active=%s", daemonPackageRequestedTimeout, timeoutFlag.Value.String())
		}
	} else if timeoutFlag.Value.String() != daemonPackageRequestedTimeout {
		t.Fatalf("TestMain changed explicit timeout from %s to %s", daemonPackageRequestedTimeout, timeoutFlag.Value.String())
	}
}

func TestDaemonPackageUsesControlledParallelismHeadroom(t *testing.T) {
	if daemonPackageParallelismExplicit(os.Args[1:]) {
		t.Skip("explicit -parallel override is authoritative; gate default not asserted")
	}
	parallelFlag := flag.Lookup("test.parallel")
	if parallelFlag == nil {
		t.Fatal("test.parallel flag is unavailable")
	}
	var got int
	if _, err := fmt.Sscanf(parallelFlag.Value.String(), "%d", &got); err != nil {
		t.Fatalf("parse active test parallelism: %v", err)
	}
	canonical, err := daemonPackageCanonicalParallelism()
	if err != nil {
		t.Fatal(err)
	}
	if got < canonical {
		t.Fatalf("daemon package parallelism = %d, want canonical gate default %d", got, canonical)
	}
}

func TestDaemonPackageExplicitLowerParallelismOverrideIsValid(t *testing.T) {
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestDaemonPackageUsesControlledParallelismHeadroom$",
		"-test.parallel=2",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("explicit lower test parallelism was rejected: %v\n%s", err, output)
	}
}

func TestDaemonPackageCanonicalParallelismIsWorkingDirectoryIndependent(t *testing.T) {
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestDaemonPackageUsesControlledParallelismHeadroom$",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("canonical daemon parallelism depends on the process working directory: %v\n%s", err, output)
	}
}

func TestDaemonPackageParallelismExplicitOverrideDetection(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "default invocation", args: []string{"-test.timeout=25m"}},
		{name: "go test equals", args: []string{"-test.parallel=3"}, want: true},
		{name: "go test split", args: []string{"-test.parallel", "3"}, want: true},
		{name: "source spelling", args: []string{"-parallel=3"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonPackageParallelismExplicit(tc.args); got != tc.want {
				t.Fatalf("explicit = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMain(m *testing.M) {
	// Full boot/restart fixture: isolated HOME, data directory and side port.
	if len(os.Args) >= 2 && os.Args[1] == "policy-test-daemon" {
		identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
		os.Args = append(os.Args[:1], os.Args[2:]...)
		main()
		os.Exit(0)
	}
	// Isolated persistence tests launch the real pane transport with this binary.
	if len(os.Args) >= 2 && os.Args[1] == "agent" {
		os.Exit(runAgent())
	}
	// Exercise the production fixed-recipe helper in subprocess fixtures.
	if len(os.Args) >= 2 && os.Args[1] == capabilityCommand {
		os.Exit(runCapabilityCommand(os.Args[2:], os.Stdout))
	}
	// Handler tests act as the person at the app; caller_authority_test.go
	// switches the real identification back on.
	identifyCallerHook = func(*http.Request) callerIdentity { return callerIdentity{Kind: callerHuman} }
	// Task schedules are created all over the suite on hosts with no Claude
	// CLI; schedules_task_claude_test.go exercises the real check.
	scheduleTaskRunnable = func() error { return nil }
	// Generated-storage producers must never allocate fixture roots beneath the
	// invoking user's real cache. Keep one process-stable cache base so every
	// managed fixture is bounded to this test process and discarded afterward.
	generatedCacheRoot, generatedCacheErr := os.MkdirTemp("", "soluterminal-daemon-test-cache-")
	if generatedCacheErr != nil {
		fmt.Fprintln(os.Stderr, "FAIL: create generated-storage test cache: "+generatedCacheErr.Error())
		os.Exit(2)
	}
	if err := os.Setenv("XDG_CACHE_HOME", generatedCacheRoot); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: configure generated-storage test cache: "+err.Error())
		_ = os.RemoveAll(generatedCacheRoot)
		os.Exit(2)
	}
	// Git status views and the daemon-written global git config files stay
	// in this test process, never the real daemon's data directory.
	setGitViewDir(filepath.Join(generatedCacheRoot, "data"))
	// Fixtures can enable browser acceptance and construct an engine, which
	// launches boot cleanup. Never let ordinary verification discover/delete
	// the real daemon's live acceptance sandboxes. Tests supply fake endpoints;
	// only the explicitly opted-in live-browser test keeps host discovery.
	os.Unsetenv("SOLU_BROWSER_ACCEPT")
	if os.Getenv("SOLU_BROWSER_ACCEPT_LIVE") != "1" {
		os.Unsetenv("SOLU_SOLUBROWSE_MCP_URL")
		os.Unsetenv("SOLU_SOLUBROWSE_MCP_TOKEN")
	}
	os.Unsetenv("SOLU_RELEASE_REPO")
	// Fixtures assert an immediate Enter on the first evaluation. Pacing
	// tests opt back in per Aria via productionAutoFirePacing.
	productionAutoFirePacing = autoFirePacing{
		minAge:     ariaAutoFireMinAge,
		minGap:     ariaAutoFireMinGap,
		settle:     ariaAutoRepeatSettle,
		unrendered: ariaAutoUnrenderedHold,
		verify:     ariaAutoFireVerifyAfter,
	}
	ariaAutoFireMinAge, ariaAutoFireMinGap, ariaAutoRepeatSettle, ariaAutoUnrenderedHold = 0, 0, 0, 0
	ariaAutoFireVerifyAfter = nil
	os.Unsetenv("SOLU_RELEASES_REPO")
	os.Unsetenv("SOLU_WORKFLOW_MAX_HEAVY")
	os.Unsetenv("SOLU_WORKFLOW_MAX_CONCURRENT")
	os.Unsetenv("SOLU_WORKFLOW_MIN_FREE_MB")
	os.Unsetenv("SOLU_WORKFLOW_ADAPTIVE_HEAVY")
	os.Unsetenv("SOLU_WORKFLOW_HOST_PROBE")
	if !flag.Parsed() {
		flag.Parse()
	}
	if timeoutFlag := flag.Lookup("test.timeout"); timeoutFlag != nil {
		daemonPackageRequestedTimeout = timeoutFlag.Value.String()
		// Go supplies its implicit default to the test binary as though it were
		// an explicit flag. Keep the package-level safety margin here as well as
		// in workflow.json so a frozen verifier command from before the config
		// update cannot still trip the observed 10-minute alarm. Deliberately
		// preserve every non-default timeout chosen by a caller.
		if requested, err := time.ParseDuration(daemonPackageRequestedTimeout); err == nil && requested == 10*time.Minute {
			_ = flag.Set("test.timeout", (15 * time.Minute).String())
		}
		daemonPackageActiveTimeout = timeoutFlag.Value.String()
	}
	// Follow the canonical gate's test-parallelism default for direct package
	// invocations too. The gate is the sole owner of that policy and its 25m
	// package alarm; an explicit caller override remains authoritative.
	if parallelFlag := flag.Lookup("test.parallel"); parallelFlag != nil && !daemonPackageParallelismExplicit(os.Args[1:]) {
		var current int
		canonical, err := daemonPackageCanonicalParallelism()
		if err != nil {
			fmt.Fprintln(os.Stderr, "FAIL: configure daemon test parallelism: "+err.Error())
			os.Exit(2)
		}
		if _, err := fmt.Sscanf(parallelFlag.Value.String(), "%d", &current); err == nil && current < canonical {
			if err := parallelFlag.Value.Set(fmt.Sprint(canonical)); err != nil {
				fmt.Fprintln(os.Stderr, "FAIL: configure daemon test parallelism: "+err.Error())
				os.Exit(2)
			}
		}
	}
	// Low-disk relief (reached from any fixture whose disk probe reports free
	// space below the floor) trims the engine's shared Go build cache and reaps
	// managed dirs under its temp root. Keep every engine this binary builds
	// off the host's real shared cache, which concurrent verification rows are
	// compiling against, and off the real /tmp, whose dirs other processes own.
	// Tests that exercise relief inject their own cache resolver and temp root.
	hermeticTmpRoot, err := os.MkdirTemp("", "soluterminal-daemon-test-tmproot-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: create hermetic engine temp root: "+err.Error())
		os.Exit(2)
	}
	guard := snapshotEnclosingGitLink()
	code := m.Run()
	removeStandInSleeper()
	_ = os.RemoveAll(hermeticTmpRoot)
	if guard != nil {
		if msg := guard.check(); msg != "" {
			fmt.Fprintln(os.Stderr, "FAIL: test suite mutated the enclosing repo's .git link: "+msg)
			code = 1
		}
	}
	// Engine cleanup can finish a cache write just as the suite returns. Retry
	// this test-owned root briefly so a concurrent final mkdir does not make
	// the whole package fail after every test has passed.
	var cacheCleanupErr error
	for attempt := 0; attempt < 5; attempt++ {
		cacheCleanupErr = os.RemoveAll(generatedCacheRoot)
		if cacheCleanupErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cacheCleanupErr; err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: remove generated-storage test cache: "+err.Error())
		code = 1
	}
	os.Exit(code)
}

// The fallback must track the gate wherever the gate exists.
func TestDaemonPackageDefaultParallelismMatchesGate(t *testing.T) {
	_, sourceFile, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(sourceFile), "..", "scripts", "daemon-test-gate.sh"))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("no repository test gate in this tree")
	}
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`-parallel=([0-9]+)`).FindSubmatch(data)
	if match == nil || string(match[1]) != strconv.Itoa(daemonPackageDefaultParallelism) {
		t.Fatalf("gate parallelism %q, fallback %d: update daemonPackageDefaultParallelism", match, daemonPackageDefaultParallelism)
	}
}
