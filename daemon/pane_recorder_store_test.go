package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRecordingFixture(t *testing.T, hub *recordingHub, paneID, name string, size int, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(hub.root(), paneID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hub.root(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Repeat("z", size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func TestRecordingSweepDeletesSevenDaysAfterLastWrite(t *testing.T) {
	hub := newRecordingHub(t.TempDir())
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	hub.now = func() time.Time { return now }
	old := writeRecordingFixture(t, hub, "pane_a", "20261110T000000Z.cast", 10, now.Add(-hub.maxAge-time.Minute))
	fresh := writeRecordingFixture(t, hub, "pane_a", "20261118T000000Z.cast", 10, now.Add(-hub.maxAge+time.Hour))
	openOld := writeRecordingFixture(t, hub, "pane_b", "20261101T000000Z.cast", 10, now.Add(-30*24*time.Hour))
	hub.open[openOld] = true
	gone := writeRecordingFixture(t, hub, "pane_c", "20261101T000000Z.cast", 10, now.Add(-8*24*time.Hour))
	stranger := writeRecordingFixture(t, hub, "pane_a", "notes.txt", 10, now.Add(-30*24*time.Hour))
	if n := hub.sweep(); n != 2 {
		t.Fatalf("deleted %d, want 2", n)
	}
	if exists(old) || !exists(fresh) || !exists(openOld) || exists(gone) || exists(filepath.Dir(gone)) {
		t.Fatalf("old=%v fresh=%v open=%v gone=%v", exists(old), exists(fresh), exists(openOld), exists(gone))
	}
	if !exists(stranger) {
		t.Fatal("the sweep deleted a file the recorder didn't write")
	}
	if hub.total != 20 {
		t.Fatalf("total %d", hub.total)
	}
	// Hourly: not again within the hour, again after it.
	later := writeRecordingFixture(t, hub, "pane_a", "20261101T000001Z.cast", 10, now.Add(-30*24*time.Hour))
	now = now.Add(30 * time.Minute)
	hub.sweepIfDue()
	if !exists(later) {
		t.Fatal("swept again within the hour")
	}
	now = now.Add(31 * time.Minute)
	hub.sweepIfDue()
	if exists(later) {
		t.Fatal("hourly sweep did not run")
	}
}

func TestRecordingSweepCreatesNothing(t *testing.T) {
	data := t.TempDir()
	hub := newRecordingHub(data)
	hub.sweep()
	hub.sweepIfDue()
	if exists(filepath.Join(data, recordingsDirName)) {
		t.Fatal("the sweep created recordings/")
	}
}

func TestRecordingSweepDoesNotFollowSymlinks(t *testing.T) {
	data := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "old.cast")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(victim, old, old)
	if err := os.Symlink(outside, filepath.Join(data, recordingsDirName)); err != nil {
		t.Fatal(err)
	}
	hub := newRecordingHub(data)
	hub.sweep()
	if !exists(victim) {
		t.Fatal("the sweep followed a symlinked recordings/")
	}
	if err := os.Remove(filepath.Join(data, recordingsDirName)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(data, recordingsDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(data, recordingsDirName, "pane_x")); err != nil {
		t.Fatal(err)
	}
	hub.sweep()
	if !exists(victim) {
		t.Fatal("the sweep followed a symlinked pane folder")
	}
}

func TestRecordingCapDeletesOldestFinishedFilesFirst(t *testing.T) {
	hub := newRecordingHub(t.TempDir())
	hub.totalCap = 100
	now := time.Now()
	oldest := writeRecordingFixture(t, hub, "pane_a", "20261119T010000Z.cast", 30, now.Add(-3*time.Hour))
	middle := writeRecordingFixture(t, hub, "pane_b", "20261119T020000Z.cast", 30, now.Add(-2*time.Hour))
	newest := writeRecordingFixture(t, hub, "pane_a", "20261119T030000Z.cast", 30, now.Add(-1*time.Hour))
	// Not the recorder's: never deleted for room, never counted.
	other := writeRecordingFixture(t, hub, "pane_a", "notes.txt", 30, now.Add(-9*time.Hour))
	if !hub.reserve(10) { // 90 + 10 fits
		t.Fatal("refused under the cap")
	}
	if !hub.reserve(25) { // 125: the oldest must go
		t.Fatal("refused when deleting the oldest makes room")
	}
	if exists(oldest) || !exists(middle) || !exists(newest) || !exists(other) {
		t.Fatalf("oldest=%v middle=%v newest=%v other=%v", exists(oldest), exists(middle), exists(newest), exists(other))
	}
	hub.open[middle] = true
	hub.open[newest] = true
	if hub.reserve(200) {
		t.Fatal("reserved past the cap with only open files left")
	}
	if !exists(middle) || !exists(newest) {
		t.Fatal("an open file was deleted for room")
	}
}

func TestRecordingStopsAtTheDiskCap(t *testing.T) {
	tp := newRecTestPane(t)
	hub := tp.m.recordingHub()
	hub.totalCap = 600
	rec := tp.start()
	for i := 0; i < 40; i++ {
		tp.send(FrameData, []byte(strings.Repeat("c", 40)))
	}
	tp.sync()
	waitRecorder(t, rec)
	if rec.reason != recordingStopDiskCap {
		t.Fatalf("reason %q", rec.reason)
	}
	var onDisk int64
	for _, f := range castFiles(t, tp.data, tp.p.ID) {
		info, _ := os.Stat(f)
		onDisk += info.Size()
	}
	if onDisk > hub.totalCap {
		t.Fatalf("wrote %d past the %d cap", onDisk, hub.totalCap)
	}
	if d := tp.p.Recording(); d == nil || d.StoppedReason != recordingStopDiskCap {
		t.Fatalf("notice %+v", d)
	}
	// The person dismisses the notice.
	tp.m.DismissRecordingNotice(tp.p)
	if tp.p.Recording() != nil {
		t.Fatal("notice survived dismissal")
	}
}

func TestRecordingRollsToANewFileAtTheFileCap(t *testing.T) {
	tp := newRecTestPane(t)
	tp.m.recordingHub().fileCap = 300
	rec := tp.start()
	want := ""
	for i := 0; i < 12; i++ {
		chunk := strings.Repeat(string(rune('a'+i)), 60)
		tp.send(FrameData, []byte(chunk))
		want += chunk
	}
	tp.sync()
	tp.m.StopRecording(tp.p, "local human")
	waitRecorder(t, rec)
	files := castFiles(t, tp.data, tp.p.ID)
	if len(files) < 3 {
		t.Fatalf("files %v", files)
	}
	got := ""
	for _, f := range files {
		header, events := readCast(t, f)
		if len(header) != 4 || header["width"] != float64(80) {
			t.Fatalf("%s header %v", f, header)
		}
		if info, _ := os.Stat(f); info.Size() > 300+200 {
			t.Fatalf("%s is %d bytes", f, info.Size())
		}
		got += outputOf(events)
	}
	if got != want {
		t.Fatalf("rolled files hold %q", got)
	}
}

func TestDeletePaneRecordings(t *testing.T) {
	tp := newRecTestPane(t)
	rec := tp.start()
	tp.send(FrameData, []byte("to delete"))
	tp.sync()
	n, err := tp.m.DeletePaneRecordings(tp.p, "local")
	if err != nil || n != 1 {
		t.Fatalf("deleted %d: %v", n, err)
	}
	waitRecorder(t, rec)
	if exists(filepath.Join(tp.data, recordingsDirName, tp.p.ID)) {
		t.Fatal("pane folder survived")
	}
	if tp.p.recording.active.Load() != nil || tp.p.Recording() != nil {
		t.Fatal("recording state survived the delete")
	}
	if hub := tp.m.recordingHub(); hub.total != 0 {
		t.Fatalf("total %d", hub.total)
	}
	if _, err := tp.m.recordingHub().paneDir("../escape"); err == nil {
		t.Fatal("a pane id with .. names a folder")
	}
}
