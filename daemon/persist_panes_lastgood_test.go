package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const twoPanesJSON = `[
  {"id":"pane_a","cwd":"/work/a","cmd":["bash"],"title":"a","createdAt":"2026-11-01T00:00:00Z","cols":80,"rows":24,"socketPath":"/data/agents/pane_a.sock","agentPid":101},
  {"id":"pane_b","cwd":"/work/b","cmd":["bash"],"title":"b","createdAt":"2026-11-02T00:00:00Z","cols":80,"rows":24,"socketPath":"/data/agents/pane_b.sock","agentPid":102}
]`

// seedPaneStoreWithLastGood writes a healthy panes.json and starts a store
// once, which is what leaves the last-good copy behind in real use.
func seedPaneStoreWithLastGood(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "panes.json")
	if err := os.WriteFile(path, []byte(twoPanesJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewPaneStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reason, _ := s.BootProblem(); reason != "" {
		t.Fatalf("a healthy panes.json reported boot problem %q", reason)
	}
	got, err := os.ReadFile(path + ".last-good")
	if err != nil {
		t.Fatalf("a healthy start must leave a last-good copy: %v", err)
	}
	if !bytes.Equal(got, []byte(twoPanesJSON)) {
		t.Fatalf("last-good copy differs from the loaded panes.json")
	}
	return dir, path
}

func paneIDs(list []PersistedPane) []string {
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, p.ID)
	}
	return out
}

func assertRestoredTwoPanes(t *testing.T, dir, path, wantReason string) {
	t.Helper()
	s, err := NewPaneStore(dir)
	if err != nil {
		t.Fatalf("NewPaneStore: %v", err)
	}
	reason, restored := s.BootProblem()
	if reason != wantReason || restored != 2 {
		t.Fatalf("BootProblem = (%q, %d), want (%q, 2)", reason, restored, wantReason)
	}
	if ids := paneIDs(s.List()); len(ids) != 2 || ids[0] != "pane_a" || ids[1] != "pane_b" {
		t.Fatalf("restored panes = %v, want [pane_a pane_b]", ids)
	}
	// The restored list is written back, so the following start is normal.
	next, err := NewPaneStore(dir)
	if err != nil {
		t.Fatalf("second NewPaneStore: %v", err)
	}
	if reason, _ := next.BootProblem(); reason != "" {
		t.Fatalf("start after a restore still reports %q", reason)
	}
	if ids := paneIDs(next.List()); len(ids) != 2 {
		t.Fatalf("panes after the restore was written back = %v", ids)
	}
	if b, err := os.ReadFile(path); err != nil || len(b) == 0 {
		t.Fatalf("panes.json was not rewritten after the restore: %v (%d bytes)", err, len(b))
	}
}

func TestPaneStoreEmptyFileRestoresLastGood(t *testing.T) {
	t.Parallel()
	dir, path := seedPaneStoreWithLastGood(t)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	assertRestoredTwoPanes(t, dir, path, "empty")
}

func TestPaneStoreTruncatedFileRestoresLastGood(t *testing.T) {
	t.Parallel()
	dir, path := seedPaneStoreWithLastGood(t)
	torn := []byte(twoPanesJSON)[:len(twoPanesJSON)/2]
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	assertRestoredTwoPanes(t, dir, path, "invalid")
	if got, err := os.ReadFile(path + ".corrupt"); err != nil || !bytes.Equal(got, torn) {
		t.Fatalf("the torn file must be kept as panes.json.corrupt: %v", err)
	}
}

func TestPaneStoreInvalidJSONRestoresLastGood(t *testing.T) {
	t.Parallel()
	for name, content := range map[string][]byte{
		"not json":     []byte("{not valid json"),
		"wrong shape":  []byte(`{"id":"pane_a"}`),
		"zeroed block": make([]byte, 4096),
	} {
		content := content
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, path := seedPaneStoreWithLastGood(t)
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			assertRestoredTwoPanes(t, dir, path, "invalid")
		})
	}
}

func TestPaneStoreMissingFileNextToLastGoodRestores(t *testing.T) {
	t.Parallel()
	dir, path := seedPaneStoreWithLastGood(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	assertRestoredTwoPanes(t, dir, path, "missing")
}

// A fresh install has neither file. It must start exactly as before: no
// boot problem, no panes, and nothing written until the first pane exists.
func TestPaneStoreFreshInstallIsNotABootProblem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := NewPaneStore(dir)
	if err != nil {
		t.Fatalf("NewPaneStore on a fresh install: %v", err)
	}
	if reason, restored := s.BootProblem(); reason != "" || restored != 0 {
		t.Fatalf("fresh install BootProblem = (%q, %d), want none", reason, restored)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("fresh install has %d panes", len(got))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("fresh install wrote %d file(s) before any pane existed", len(entries))
	}
}

// A saved "no panes" list is a real state (every pane was closed), not a
// damaged file: it must not bring closed panes back from the copy.
func TestPaneStoreSavedEmptyListIsNotABootProblem(t *testing.T) {
	t.Parallel()
	for _, content := range []string{"[]", "null"} {
		dir, path := seedPaneStoreWithLastGood(t)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := NewPaneStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if reason, _ := s.BootProblem(); reason != "" {
			t.Fatalf("%s reported boot problem %q", content, reason)
		}
		if got := s.List(); len(got) != 0 {
			t.Fatalf("%s restored %d closed pane(s)", content, len(got))
		}
	}
}

// With no usable copy the daemon still starts with no panes, and still
// reports the problem so startup leaves sockets and agents alone.
func TestPaneStoreUnusableFileWithoutLastGoodStartsEmpty(t *testing.T) {
	t.Parallel()
	for wantReason, content := range map[string][]byte{"empty": nil, "invalid": []byte("{not valid json")} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "panes.json"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := NewPaneStore(dir)
		if err != nil {
			t.Fatalf("%s: NewPaneStore: %v", wantReason, err)
		}
		if reason, restored := s.BootProblem(); reason != wantReason || restored != 0 {
			t.Fatalf("BootProblem = (%q, %d), want (%q, 0)", reason, restored, wantReason)
		}
		if got := s.List(); len(got) != 0 {
			t.Fatalf("%s: expected no panes, got %d", wantReason, len(got))
		}
	}
}

// A damaged last-good copy is treated like no copy.
func TestPaneStoreDamagedLastGoodStartsEmpty(t *testing.T) {
	t.Parallel()
	dir, path := seedPaneStoreWithLastGood(t)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".last-good", []byte("[{"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewPaneStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reason, restored := s.BootProblem(); reason != "empty" || restored != 0 {
		t.Fatalf("BootProblem = (%q, %d), want (empty, 0)", reason, restored)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("expected no panes, got %d", len(got))
	}
}

// A restore that cannot be written back (disk full, read-only data dir) must
// still start the daemon with the restored panes: NewPaneStore's error is fatal.
func TestPaneStoreRestoreSurvivesFailedWriteBack(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir, path := seedPaneStoreWithLastGood(t)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	s, err := NewPaneStore(dir)
	if err != nil {
		t.Fatalf("a failed write-back stopped startup: %v", err)
	}
	if reason, restored := s.BootProblem(); reason != "empty" || restored != 2 {
		t.Fatalf("BootProblem = (%q, %d), want (empty, 2)", reason, restored)
	}
	if ids := paneIDs(s.List()); len(ids) != 2 {
		t.Fatalf("restored panes = %v", ids)
	}
}

// The copy follows saves, but only takes a version that has been the
// current file for a while, and never a damaged one.
func TestPaneStoreLastGoodFollowsSettledSaves(t *testing.T) {
	t.Parallel()
	dir, path := seedPaneStoreWithLastGood(t)
	lastGood := path + ".last-good"
	s, err := NewPaneStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	read := func(p string) []byte {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	remove := func(id string) {
		t.Helper()
		if err := s.Remove(id); err != nil {
			t.Fatal(err)
		}
	}
	boot := read(lastGood)

	// First save after start: the copy already holds the start version.
	remove("pane_a")
	afterFirst := read(path)
	if !bytes.Equal(read(lastGood), boot) {
		t.Fatal("first save changed the last-good copy away from the start version")
	}
	// A save moments later must not copy the version written just before it.
	clock = clock.Add(5 * time.Second)
	remove("pane_b")
	afterSecond := read(path)
	if !bytes.Equal(read(lastGood), boot) {
		t.Fatal("a save 5s after the previous one refreshed the last-good copy")
	}
	// Once the current file has settled, the next save keeps it as the copy.
	clock = clock.Add(paneLastGoodMinAge)
	remove("pane_missing")
	if !bytes.Equal(read(lastGood), afterSecond) {
		t.Fatalf("settled version was not kept: last-good = %s", read(lastGood))
	}
	if bytes.Equal(afterFirst, afterSecond) {
		t.Fatal("test setup: the two saves wrote identical content")
	}
	// Constant saves closer together than the settle time still refresh the
	// copy once it is older than the staleness bound.
	if err := os.WriteFile(lastGood, boot, 0o600); err != nil {
		t.Fatal(err)
	}
	for elapsed := time.Duration(0); elapsed < paneLastGoodMaxStale; elapsed += 10 * time.Second {
		clock = clock.Add(10 * time.Second)
		remove("pane_missing")
	}
	if !bytes.Equal(read(lastGood), read(path)) {
		t.Fatal("constant saves never refreshed the last-good copy")
	}
	// A current file damaged behind the daemon's back never replaces the copy.
	good := read(lastGood)
	if err := os.WriteFile(path, []byte("[{"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(paneLastGoodMaxStale)
	remove("pane_missing")
	if !bytes.Equal(read(lastGood), good) {
		t.Fatal("a damaged panes.json overwrote the last-good copy")
	}
}

// startFakeAgent runs a process whose command line reads as this daemon's
// pane agent for socket, so the reaper sees it as one of its own.
func startFakeAgent(t *testing.T, socket string) (alive func() bool) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	hold, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &exec.Cmd{
		Path:  sh,
		Args:  []string{"solud-test", "-c", "read _", "sh", "agent", "--socket", socket},
		Stdin: hold,
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	hold.Close()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		release.Close()
		_ = cmd.Process.Kill()
		<-done
	})
	return func() bool {
		select {
		case <-done:
			return false
		case <-time.After(300 * time.Millisecond):
			return true
		}
	}
}

func bootCleanupFixture(t *testing.T) (dataDir, socket string, alive func() bool) {
	t.Helper()
	dataDir = t.TempDir()
	agents := filepath.Join(dataDir, "agents")
	if err := os.MkdirAll(agents, 0o700); err != nil {
		t.Fatal(err)
	}
	socket = filepath.Join(agents, "pane_live.sock")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dataDir, socket, startFakeAgent(t, socket)
}

// The start that found panes.json unusable has no trustworthy pane list, so
// a live session with no pane must keep its socket and its process.
func TestBootCleanupLeavesSessionsAloneWhenPanesFileWasUnusable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the agent reaper reads Linux procfs")
	}
	for _, problem := range []string{"empty", "invalid", "missing"} {
		dataDir, socket, alive := bootCleanupFixture(t)
		cleanOrphansAtBoot(NewManager(dataDir), dataDir, nil, nil, problem, 0)
		if _, err := os.Stat(socket); err != nil {
			t.Fatalf("%s: the socket of a live session was swept: %v", problem, err)
		}
		if !alive() {
			t.Fatalf("%s: the live agent process was reaped", problem)
		}
	}
}

// Control: on a normal start (fresh install included) the same unowned
// socket and process are orphans and are removed, as before.
func TestBootCleanupStillRemovesOrphansOnANormalStart(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the agent reaper reads Linux procfs")
	}
	dataDir, socket, alive := bootCleanupFixture(t)
	store, err := NewPaneStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	problem, restored := store.BootProblem()
	cleanOrphansAtBoot(NewManager(dataDir), dataDir, nil, nil, problem, restored)
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("orphan socket survived a normal start: %v", err)
	}
	if alive() {
		t.Fatal("orphan agent process survived a normal start")
	}
}
