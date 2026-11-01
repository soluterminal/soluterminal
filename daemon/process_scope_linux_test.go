//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func ownedScopeFixture(t *testing.T, cmd *exec.Cmd) *ownedProcessScope {
	return ownedScopeFixtureMode(t, cmd, true)
}

func ownedScopeFixtureMode(t *testing.T, cmd *exec.Cmd, contained bool) *ownedProcessScope {
	t.Helper()
	applyDetached(cmd)
	scope, err := prepareOwnedCommand(cmd, contained)
	if err != nil || scope == nil {
		if os.Getenv("SOLU_REQUIRE_PROCESS_SCOPE_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skipf("kernel scope integration requires delegated nsdelegate cgroup v2: %v", err)
	}
	t.Cleanup(func() {
		_, _ = stopOwnedProcessScope(scope, true)
		if scope.resource != nil {
			scope.resource.Close()
		}
		commandProcessScopes.Delete(cmd)
		_ = os.Remove(scope.recordPath(false))
		_ = os.Remove(scope.recordPath(true))
		_ = os.Remove(filepath.Join(scope.root, "locks", scope.identity.ID))
	})
	return scope
}

func TestOwnedProcessScopeDelegatesNonSystemdWSLLauncher(t *testing.T) {
	if os.Getenv("SOLU_TEST_NON_SYSTEMD_SCOPE") != "1" {
		t.Skip("run this test binary through wsl.exe to exercise the desktop launch context")
	}
	before, err := processScopeParent()
	if err != nil || before != "/sys/fs/cgroup/non-systemd" {
		t.Fatalf("probe did not start in the desktop WSL launch context: %q: %v", before, err)
	}
	t.Setenv("SOLU_REQUIRE_PROCESS_SCOPE_TESTS", "1")
	cmd := exec.Command("sh", "-c", "setsid sleep 60 </dev/null >/dev/null 2>&1 &")
	scope := ownedScopeFixture(t, cmd)
	after, err := processScopeParent()
	if err != nil || after == before || !processScopeParentWritable(after) {
		t.Fatalf("daemon did not acquire delegated ownership: %q: %v", after, err)
	}
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	if err := waitOwnedCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if gone, err := stopOwnedProcessScope(scope, true); err != nil || !gone {
		t.Fatalf("desktop-launched scope left detached children: gone=%v: %v", gone, err)
	}
}

func TestOwnedProcessScopeTerminalPreservesNamespaceAndStopsDetachedChildren(t *testing.T) {
	parentMap, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(t.TempDir(), "uid_map")
	cmd := exec.Command("sh", "-c", `cat /proc/self/uid_map > "$1"; setsid sleep 60 </dev/null >/dev/null 2>&1 & echo $! > "$1.child"`, "sh", result)
	scope := ownedScopeFixtureMode(t, cmd, false)
	if processScopeHasContainment(scope.identity) {
		t.Fatal("ordinary terminal falsely advertised migration containment")
	}
	if err := claimTaskProcessScope(scope.identity, "wft_fixture", "inv_fixture"); err == nil {
		t.Fatal("ordinary terminal scope admitted as workflow shutdown proof")
	}
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	if err := waitOwnedCommand(cmd); err != nil {
		t.Fatal(err)
	}
	childMap, err := os.ReadFile(result)
	if err != nil || string(childMap) != string(parentMap) {
		t.Fatalf("ordinary terminal lost its original user namespace: %q vs %q: %v", childMap, parentMap, err)
	}
	childPIDText, err := os.ReadFile(result + ".child")
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(childPIDText)))
	if err != nil {
		t.Fatal(err)
	}
	if ownedScopeForPID(childPID) != nil {
		t.Fatal("descendant acquired enclosing pane scope authority")
	}
	if state := retirementStopProcessGroup(retirementProcessIdentity{Scope: &scope.identity}); state != retirementProcessUnknown {
		t.Fatalf("uncontained scope invented whole-tree proof: %s", state)
	}
	if err := killChildTree(cmd.Process); err != nil {
		t.Fatal(err)
	}
	record, err := scope.readRecord()
	if err != nil || record.Phase != "closed" {
		t.Fatalf("terminal teardown left detached descendants: %+v %v", record, err)
	}
}

func TestOwnedProcessScopeCancellationPreservesSibling(t *testing.T) {
	first, sibling := exec.Command("sleep", "60"), exec.Command("sleep", "60")
	ownedScopeFixture(t, first)
	siblingScope := ownedScopeFixture(t, sibling)
	for _, cmd := range []*exec.Cmd{first, sibling} {
		if err := startWithoutCoreDumps(cmd); err != nil {
			t.Fatal(err)
		}
	}
	if err := killChildTree(first.Process); err != nil {
		t.Fatal(err)
	}
	_ = waitOwnedCommand(first)
	dir, err := openLinuxProcessScope(siblingScope.identity)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := processScopeEmpty(dir)
	dir.Close()
	if empty || err != nil {
		t.Fatalf("cancellation touched sibling: empty=%v %v", empty, err)
	}
	_ = killChildTree(sibling.Process)
	_ = waitOwnedCommand(sibling)
}

func TestOwnedProcessScopeLegacyPaneCannotClaimRecycledPID(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	scope := ownedScopeFixture(t, cmd)
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = killChildTree(cmd.Process); _ = waitOwnedCommand(cmd) }()
	done := make(chan struct{})
	close(done)
	// The old pane's PID now belongs to an unrelated, newly scoped process.
	legacy := &Pane{ID: "pane_old", Pid: cmd.Process.Pid, done: done, SocketPath: "/not-the-current-agent.sock"}
	if err := legacy.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := killLegacyPaneAgent(cmd.Process.Pid, legacy.SocketPath); err == nil {
		t.Fatal("PID-only legacy fallback accepted an unrelated process")
	}
	dir, err := openLinuxProcessScope(scope.identity)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := processScopeEmpty(dir)
	dir.Close()
	if err != nil || empty {
		t.Fatalf("legacy pane stopped the unrelated new scope: %v %v", empty, err)
	}
}

func TestOwnedProcessScopeContextCancellationStopsDetachedDescendant(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `setsid sleep 60 </dev/null >/dev/null 2>&1 & wait`)
	scope := ownedScopeFixture(t, cmd)
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = waitOwnedCommand(cmd)
	record, err := scope.readRecord()
	if err != nil || record.Phase != "closed" {
		t.Fatalf("context left descendants: %+v %v", record, err)
	}
}

func TestOwnedProcessScopeWSLInterop(t *testing.T) {
	bin := "/mnt/c/Windows/System32/cmd.exe"
	if !isInsideWSL() {
		t.Skip("WSL only")
	}
	cmd := exec.Command(bin, "/d", "/c", "exit", "0")
	ownedScopeFixture(t, cmd)
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	if err := waitOwnedCommand(cmd); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedProcessScopeNestedHelper(t *testing.T) {
	if os.Getenv("SOLU_TEST_NESTED_SCOPE") != "1" {
		return
	}
	cmd := exec.Command("sh", "-c", `setsid sleep 60 </dev/null >/dev/null 2>&1 &`)
	applyDetached(cmd)
	scope, err := prepareOwnedCommand(cmd, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	if err := waitOwnedCommand(cmd); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(scope.identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("SOLU_TEST_NESTED_RESULT"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedProcessScopeNestedCleanupRetainsChildProof(t *testing.T) {
	result := filepath.Join(t.TempDir(), "nested.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestOwnedProcessScopeNestedHelper$")
	cmd.Env = append(os.Environ(), "SOLU_TEST_NESTED_SCOPE=1", "SOLU_TEST_NESTED_RESULT="+result)
	parent := ownedScopeFixture(t, cmd)
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	if err := waitOwnedCommand(cmd); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	var child processScopeIdentity
	if err := json.Unmarshal(raw, &child); err != nil {
		t.Fatal(err)
	}
	childScope := &ownedProcessScope{identity: child, root: parent.root}
	t.Cleanup(func() {
		_, _ = stopOwnedProcessScope(childScope, true)
		_ = os.Remove(childScope.recordPath(false))
		_ = os.Remove(childScope.recordPath(true))
		_ = os.Remove(filepath.Join(childScope.root, "locks", child.ID))
	})
	if gone, err := stopOwnedProcessScope(parent, true); err != nil || !gone {
		t.Fatalf("parent stop failed: %v %v", gone, err)
	}
	if _, err := os.Stat(parent.identity.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nested cgroups left behind: %v", err)
	}
	if gone, err := stopOwnedProcessScope(childScope, true); err != nil || !gone {
		t.Fatalf("child lost shutdown proof: %v %v", gone, err)
	}
}

func TestOwnedProcessScopePaneCloseUsesPersistedScope(t *testing.T) {
	cmd := exec.Command("sh", "-c", `setsid sleep 60 </dev/null >/dev/null 2>&1 &`)
	scope := ownedScopeFixture(t, cmd)
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	if err := waitOwnedCommand(cmd); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(PersistedPane{ID: "pane_scope", ProcessScope: &scope.identity})
	if err != nil {
		t.Fatal(err)
	}
	var stored PersistedPane
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	pane := &Pane{ID: stored.ID, processScope: stored.ProcessScope, done: done}
	if err := pane.Kill(); err != nil {
		t.Fatal(err)
	}
	record, err := scope.readRecord()
	if err != nil || record.Phase != "closed" {
		t.Fatalf("pane close left descendants: %+v %v", record, err)
	}
}

func TestOwnedProcessScopeDetachedChildOutlivesParentAndIsStopped(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("/bin/sh", "-c", `setsid sh -c 'while :; do echo alive >> "$1/payload"; sleep 0.02; done' sh "$1" </dev/null >/dev/null 2>&1 & echo $! > "$1/child"`, "sh", dir)
	scope := ownedScopeFixture(t, cmd)
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if b, _ := os.ReadFile(filepath.Join(dir, "payload")); len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached child never wrote")
		}
		time.Sleep(10 * time.Millisecond)
	}
	dirFD, err := openLinuxProcessScope(scope.identity)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := processScopeEmpty(dirFD)
	dirFD.Close()
	if err != nil || empty {
		t.Fatalf("parent exit hid detached child: empty=%v err=%v", empty, err)
	}
	// The shared kill helper must use the original scope even after Wait and PID
	// disappearance. It must not reduce shutdown to kill(-old-parent-PID).
	if err := killChildTree(cmd.Process); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "payload"))
	time.Sleep(60 * time.Millisecond)
	after, _ := os.ReadFile(filepath.Join(dir, "payload"))
	if len(after) != len(before) {
		t.Fatal("detached child survived explicit tree stop")
	}
	record, err := scope.readRecord()
	if err != nil || record.Phase != "closed" {
		t.Fatalf("durable closure missing: %+v %v", record, err)
	}
}

func TestOwnedProcessScopeRejectsMigrationOutsideNamespace(t *testing.T) {
	parent, err := processScopeParent()
	if err != nil {
		t.Skip(err)
	}
	output := filepath.Join(t.TempDir(), "result")
	cmd := exec.Command("/bin/sh", "-c", `if (echo $$ > "$1/cgroup.procs") 2>/dev/null; then echo escaped; else echo contained; fi > "$2"`, "sh", parent, output)
	ownedScopeFixture(t, cmd)
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil || strings.TrimSpace(string(data)) != "contained" {
		t.Fatalf("scope migration escaped: %s %v", data, err)
	}
}

func TestOwnedProcessScopeStopBeforeStartFencesLateExecution(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	cmd := exec.Command("/bin/sh", "-c", `echo ran > "$1"`, "sh", marker)
	scope := ownedScopeFixture(t, cmd)
	if gone, err := stopOwnedProcessScope(scope, true); err != nil || !gone {
		t.Fatalf("prepared stop: %v %v", gone, err)
	}
	if err := startWithoutCoreDumps(cmd); err == nil {
		_ = cmd.Wait()
		t.Fatal("closed scope started user code")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late command executed: %v", err)
	}
}

func TestOwnedProcessScopeRecoveryDoesNotNeedLeader(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", `setsid sleep 60 </dev/null >/dev/null 2>&1 &`)
	scope := ownedScopeFixture(t, cmd)
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// A later daemon has only this persisted scope identity and state directory.
	recovered := &ownedProcessScope{identity: scope.identity, root: scope.root}
	if gone, err := stopOwnedProcessScope(recovered, true); err != nil || !gone {
		t.Fatalf("restart recovery failed: %v %v", gone, err)
	}
}

func TestOwnedProcessScopeMissingKernelPathIsNotProof(t *testing.T) {
	cmd := exec.Command("/bin/true")
	scope := ownedScopeFixture(t, cmd)
	// Removing an empty, prepared group simulates lost/renamed kernel evidence.
	// Without a closed receipt it must stay uncertain, never invent completion.
	if err := os.Remove(scope.identity.Path); err != nil {
		t.Fatal(err)
	}
	if gone, err := stopOwnedProcessScope(scope, true); err == nil || gone {
		t.Fatalf("missing path invented closure: %v %v", gone, err)
	}
}

func TestOwnedProcessScopeWorkerArchiveReapsBeforeCredentialRelease(t *testing.T) {
	cmd := exec.Command("sh", "-c", `setsid sleep 60 </dev/null >/dev/null 2>&1 &`)
	scope := ownedScopeFixtureMode(t, cmd, false)
	if err := startWithoutCoreDumps(cmd); err != nil {
		t.Fatal(err)
	}
	if err := waitOwnedCommand(cmd); err != nil {
		t.Fatal(err)
	}
	a, p, now := archiveFixture(t)
	p.mu.Lock()
	p.Pid = cmd.Process.Pid
	p.processScope = &scope.identity
	p.mu.Unlock()
	released := false
	q := a.workers()
	q.mu.Lock()
	q.requests[0].release = func() {
		record, err := scope.readRecord()
		if err != nil || record.Phase != "closed" {
			t.Fatalf("credentials released before descendants stopped: %+v %v", record, err)
		}
		released = true
	}
	q.mu.Unlock()
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	a.tryArchiveWorker(p, now)
	if !released || len(a.archivedWorkerSummaries()) != 1 {
		t.Fatal("scoped worker was not archived/released")
	}
}
