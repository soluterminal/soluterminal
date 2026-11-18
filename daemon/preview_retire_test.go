package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetiredPreviewTargetsOnlyMatchVerifiedSpawnProcesses(t *testing.T) {
	dir := t.TempDir()
	registry := `[
		{"id":"pv_live","mode":"spawn","pid":101,"processStartIdentity":"linux:500"},
		{"id":"pv_recycled","mode":"spawn","pid":102,"processStartIdentity":"linux:600"},
		{"id":"pv_gone","mode":"spawn","pid":103,"processStartIdentity":"linux:700"},
		{"id":"pv_noident","mode":"spawn","pid":104},
		{"id":"pv_static","mode":"static","pid":105,"processStartIdentity":"linux:800"},
		{"id":"pv_adopt","mode":"adopt","pid":106,"processStartIdentity":"linux:900"},
		null
	]`
	if err := os.WriteFile(filepath.Join(dir, "previews.json"), []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	live := map[int]string{101: "linux:500", 102: "linux:999", 104: "linux:1", 105: "linux:800", 106: "linux:900"}
	got := retiredPreviewTargets(dir, func(pid int) string { return live[pid] })
	if len(got) != 1 || got[0].ID != "pv_live" || got[0].PID != 101 {
		t.Fatalf("targets = %+v, want only the verified live spawn preview", got)
	}
}

func TestRetiredPreviewTargetsToleratesMissingOrBadRegistry(t *testing.T) {
	never := func(int) string { t.Fatal("identity probed without a registry entry"); return "" }
	if got := retiredPreviewTargets(t.TempDir(), never); got != nil {
		t.Fatalf("missing registry: %+v", got)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "previews.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := retiredPreviewTargets(dir, never); got != nil {
		t.Fatalf("corrupt registry: %+v", got)
	}
	if n := stopRetiredPreviewProcesses(t.TempDir()); n != 0 {
		t.Fatalf("stopped %d with no registry", n)
	}
}

func TestStopRetiredPreviewRechecksIdentityBeforeEachSignal(t *testing.T) {
	e := retiredPreviewEntry{ID: "pv_x", Mode: "spawn", PID: os.Getpid(), ProcessStartIdentity: "linux:1"}
	var terms, kills, pidKills int
	ops := retiredPreviewOps{
		descendants: func(int) []int { return []int{e.PID} },
		terminate:   func(*os.Process) error { terms++; return nil },
		kill:        func(*os.Process) error { kills++; return nil },
		killPid:     func(int) error { pidKills++; return nil },
	}

	// The pid was reused before its turn: nothing is signalled.
	ops.identity = func(int) string { return "linux:2" }
	if ok, _ := stopRetiredPreview(e, ops, ""); ok || terms != 0 || kills != 0 || pidKills != 0 {
		t.Fatalf("reused pid signalled: terms=%d kills=%d pidKills=%d", terms, kills, pidKills)
	}

	// The server exits on SIGTERM and its pid is reused at once: no SIGKILL.
	ops.identity = func(int) string {
		if terms > 0 {
			return "linux:other"
		}
		return "linux:1"
	}
	if ok, left := stopRetiredPreview(e, ops, ""); !ok || len(left) != 0 || terms != 1 || kills != 0 || pidKills != 0 {
		t.Fatalf("after a clean exit: terms=%d kills=%d pidKills=%d", terms, kills, pidKills)
	}
}

func TestStopRetiredPreviewKillsDescendantsThatOutliveTheLeader(t *testing.T) {
	const leader, worker, replaced, gone = 201, 202, 203, 204
	e := retiredPreviewEntry{ID: "pv_x", Mode: "spawn", PID: leader, ProcessStartIdentity: "linux:1"}
	termed := false
	live := func(pid int) string {
		switch pid {
		case leader:
			if termed {
				return "" // the leader exits on SIGTERM
			}
			return "linux:1"
		case worker:
			return "linux:2" // ignores SIGTERM and lives on
		case replaced:
			if termed {
				return "linux:99" // exited; its pid now belongs to someone else
			}
			return "linux:3"
		}
		return "" // gone: already exited before the snapshot was read
	}
	var kills int
	var pidKills []int
	ops := retiredPreviewOps{
		identity:    live,
		descendants: func(int) []int { return []int{leader, worker, replaced, gone} },
		terminate:   func(*os.Process) error { termed = true; return nil },
		kill:        func(*os.Process) error { kills++; return nil },
		killPid:     func(pid int) error { pidKills = append(pidKills, pid); return nil },
	}
	ok, left := stopRetiredPreview(e, ops, "")
	if !ok {
		t.Fatal("verified leader not stopped")
	}
	// The worker ignores the kill too (its identity still matches): it is
	// reported under its own identity so it stays on record.
	if len(left) != 1 || left[0].PID != worker || left[0].ProcessStartIdentity != "linux:2" || left[0].Mode != "spawn" {
		t.Fatalf("survivors = %+v, want only the worker", left)
	}
	if kills != 0 {
		t.Fatalf("tree kill on an exited leader: %d", kills)
	}
	if len(pidKills) != 1 || pidKills[0] != worker {
		t.Fatalf("descendant kills = %v, want only the surviving worker %d", pidKills, worker)
	}
}

// After the stop pass, the registry is retired so a later boot never probes
// its pids again, and the previews' folders go. A symlinked folder loses only
// the link, and the rest of the data folder is untouched.
func TestRetireRemovedPreviewsRetiresRegistryAndFolders(t *testing.T) {
	dir := t.TempDir()
	reg := `[{"id":"pv_live","mode":"spawn","pid":301,"processStartIdentity":"linux:5"}]`
	if err := os.WriteFile(filepath.Join(dir, "previews.json"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "preview-cache", "pv_live", "dist"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preview-cache", "pv_live", "dist", "index.html"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "preview-logs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schedules.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}

	alive := true
	ops := retiredPreviewOps{
		identity: func(pid int) string {
			if pid == 301 && alive {
				return "linux:5"
			}
			return ""
		},
		descendants: func(int) []int { return nil },
		terminate:   func(*os.Process) error { alive = false; return nil },
		kill:        func(*os.Process) error { return nil },
		killPid:     func(int) error { return nil },
	}
	if n := retireRemovedPreviews(dir, ops); n != 1 {
		t.Fatalf("stopped %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "previews.json")); !os.IsNotExist(err) {
		t.Fatalf("previews.json still in place: %v", err)
	}
	// The retired copy is the record written before the stop: the server it
	// named, kept for reference.
	raw, err := os.ReadFile(filepath.Join(dir, "previews.json.retired"))
	if err != nil || !strings.Contains(string(raw), `"pv_live"`) || !strings.Contains(string(raw), `"pid": 301`) {
		t.Fatalf("retired registry = %q (%v), want the server's record kept", raw, err)
	}
	for _, name := range []string{"preview-cache", "preview-logs"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s still there: %v", name, err)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(outside, "keep.txt")); err != nil || string(raw) != "mine" {
		t.Fatalf("a symlinked folder's target was touched: %q %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "schedules.json")); err != nil {
		t.Fatalf("an unrelated data file went: %v", err)
	}

	// A later boot finds no registry and probes nothing.
	ops.identity = func(int) string { t.Fatal("a later boot probed a pid"); return "" }
	if n := retireRemovedPreviews(dir, ops); n != 0 {
		t.Fatalf("later boot stopped %d", n)
	}
}

// A server that is still running keeps the registry and its folders, so the
// next boot tries again.
func TestRetireRemovedPreviewsKeepsRegistryWhileAServerRuns(t *testing.T) {
	dir := t.TempDir()
	reg := `[{"id":"pv_stuck","mode":"spawn","pid":302,"processStartIdentity":"linux:6"}]`
	if err := os.WriteFile(filepath.Join(dir, "previews.json"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "preview-logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	shortRetireWaits(t)
	ops := retiredPreviewOps{
		identity:    func(int) string { return "linux:6" }, // ignores every signal
		descendants: func(int) []int { return nil },
		terminate:   func(*os.Process) error { return nil },
		kill:        func(*os.Process) error { return nil },
		killPid:     func(int) error { return nil },
	}
	retireRemovedPreviews(dir, ops)
	if _, err := os.Stat(filepath.Join(dir, "previews.json")); err != nil {
		t.Fatalf("registry retired while its server still runs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "preview-logs")); err != nil {
		t.Fatalf("preview-logs removed while its server still runs: %v", err)
	}
}

// shortRetireWaits makes the stop and settle waits brief for a test.
func shortRetireWaits(t *testing.T) {
	t.Helper()
	grace, settle := retiredPreviewStopGrace, retiredPreviewSettle
	retiredPreviewStopGrace, retiredPreviewSettle = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { retiredPreviewStopGrace, retiredPreviewSettle = grace, settle })
}

// A registry that can't be parsed is left as it is, and the folders stay:
// it may still name a running server.
func TestRetireRemovedPreviewsLeavesAnUnreadableRegistry(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "previews.json"), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "preview-cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	never := retiredPreviewOps{identity: func(int) string { t.Fatal("probed a pid from a torn registry"); return "" }}
	retireRemovedPreviews(dir, never)
	if raw, err := os.ReadFile(filepath.Join(dir, "previews.json")); err != nil || string(raw) != "{torn" {
		t.Fatalf("torn registry changed: %q %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "preview-cache")); err != nil {
		t.Fatalf("preview-cache removed despite an unreadable registry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "previews.json.retired")); !os.IsNotExist(err) {
		t.Fatalf("an unreadable registry was retired: %v", err)
	}
}

// The server exits but a descendant outlives its kill: the registry is
// rewritten to hold the descendant under its own identity (0600), the
// folders stay, and the next boot stops it and then retires everything.
func TestRetireRemovedPreviewsKeepsASurvivingDescendantOnRecord(t *testing.T) {
	shortRetireWaits(t)
	dir := t.TempDir()
	reg := `[{"id":"pv_x","mode":"spawn","pid":401,"processStartIdentity":"linux:1"}]`
	if err := os.WriteFile(filepath.Join(dir, "previews.json"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "preview-logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	leaderUp, workerUp := true, true
	ops := retiredPreviewOps{
		identity: func(pid int) string {
			switch {
			case pid == 401 && leaderUp:
				return "linux:1"
			case pid == 402 && workerUp:
				return "linux:2"
			}
			return ""
		},
		descendants: func(int) []int { return []int{401, 402} },
		terminate:   func(*os.Process) error { leaderUp = false; return nil },
		kill:        func(*os.Process) error { return nil },
		killPid:     func(int) error { return nil }, // the worker survives this boot's kill
	}
	if n := retireRemovedPreviews(dir, ops); n != 1 {
		t.Fatalf("stopped %d, want 1", n)
	}
	entries, err := readRetiredPreviewRegistry(dir)
	if err != nil || len(entries) != 1 || entries[0].PID != 402 || entries[0].ProcessStartIdentity != "linux:2" || !entries[0].Child {
		t.Fatalf("registry after the pass = %+v (%v), want only the surviving worker, as a child", entries, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "previews.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("rewritten registry mode = %v (%v), want 0600", fi, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "preview-logs")); err != nil {
		t.Fatalf("preview-logs removed while a process lives: %v", err)
	}

	// Next boot: the worker is retried by its own pid (never as a group
	// leader) and dies, so everything is retired.
	ops.terminate = func(*os.Process) error { t.Fatal("a recorded child was stopped as a server"); return nil }
	ops.killPid = func(pid int) error {
		if pid == 402 {
			workerUp = false
		}
		return nil
	}
	retireRemovedPreviews(dir, ops)
	if _, err := os.Stat(filepath.Join(dir, "previews.json")); !os.IsNotExist(err) {
		t.Fatalf("registry not retired once nothing runs: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "preview-logs")); !os.IsNotExist(err) {
		t.Fatalf("preview-logs still there: %v", err)
	}
}

// Every descendant is on record before the first signal, so a server that
// exits at once can't take its tree's only trace with it.
func TestRetireRemovedPreviewsRecordsDescendantsBeforeSignalling(t *testing.T) {
	shortRetireWaits(t)
	dir := t.TempDir()
	reg := `[{"id":"pv_y","mode":"spawn","pid":501,"processStartIdentity":"linux:1"}]`
	if err := os.WriteFile(filepath.Join(dir, "previews.json"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	up := map[int]bool{501: true, 502: true}
	ids := map[int]string{501: "linux:1", 502: "linux:2"}
	var atSignal []*retiredPreviewEntry
	ops := retiredPreviewOps{
		identity: func(pid int) string {
			if up[pid] {
				return ids[pid]
			}
			return ""
		},
		descendants: func(int) []int { return []int{501, 502} },
		terminate: func(*os.Process) error {
			atSignal, _ = readRetiredPreviewRegistry(dir)
			up[501], up[502] = false, false
			return nil
		},
		kill:    func(*os.Process) error { return nil },
		killPid: func(int) error { return nil },
		bootID:  func() string { return "boot-a" },
	}
	retireRemovedPreviews(dir, ops)
	if len(atSignal) != 2 || atSignal[1].PID != 502 || !atSignal[1].Child || atSignal[1].BootID != "boot-a" || atSignal[0].BootID != "boot-a" {
		t.Fatalf("registry at the first signal = %+v, want the server and its child, with this boot's id", atSignal)
	}
	if _, err := os.Stat(filepath.Join(dir, "previews.json.retired")); err != nil {
		t.Fatalf("registry not retired once both were gone: %v", err)
	}
}

// If the registry can't be updated first, nothing is signalled.
func TestRetireRemovedPreviewsSignalsNothingWhenItCannotRecordFirst(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only folder")
	}
	dir := t.TempDir()
	reg := `[{"id":"pv_z","mode":"spawn","pid":601,"processStartIdentity":"linux:1"}]`
	if err := os.WriteFile(filepath.Join(dir, "previews.json"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	signalled := false
	ops := retiredPreviewOps{
		identity:    func(int) string { return "linux:1" },
		descendants: func(int) []int { return []int{601, 602} },
		terminate:   func(*os.Process) error { signalled = true; return nil },
		kill:        func(*os.Process) error { signalled = true; return nil },
		killPid:     func(int) error { signalled = true; return nil },
	}
	if n := retireRemovedPreviews(dir, ops); n != 0 || signalled {
		t.Fatalf("stopped %d, signalled %v, with an unwritable registry", n, signalled)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "previews.json")); string(raw) != reg {
		t.Fatalf("registry changed: %q", raw)
	}
}

// An entry recorded under another boot names no live process: it is never
// probed or signalled, and the registry is retired.
func TestRetireRemovedPreviewsSkipsEntriesFromAnotherBoot(t *testing.T) {
	dir := t.TempDir()
	reg := `[{"id":"pv_w/702","mode":"spawn","pid":702,"processStartIdentity":"linux:2","child":true,"bootId":"boot-old"}]`
	if err := os.WriteFile(filepath.Join(dir, "previews.json"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := retiredPreviewOps{
		identity: func(int) string { t.Fatal("probed an entry from another boot"); return "" },
		killPid:  func(int) error { t.Fatal("signalled an entry from another boot"); return nil },
		bootID:   func() string { return "boot-new" },
	}
	retireRemovedPreviews(dir, ops)
	if _, err := os.Stat(filepath.Join(dir, "previews.json.retired")); err != nil {
		t.Fatalf("registry not retired: %v", err)
	}
}
