package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Regression tests for the first independent review of the recorder.

func TestRecordingPersonGateRefusesUnprovenAndForeignOrigins(t *testing.T) {
	tp := newRecTestPane(t)
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
	unproven := recCaller{"person on browser headers alone", callerIdentity{Kind: callerHuman, Unproven: true}, false}
	if w := callRecording(t, a, unproven, http.MethodPost, "/recording", `{"on":true}`, tp.p.ID); w.Code != http.StatusForbidden {
		t.Fatalf("unproven person: %d %s", w.Code, w.Body.String())
	}
	for name, origin := range map[string]string{"no origin": "", "another site": "http://evil.example", "https": "https://127.0.0.1:9791"} {
		asRecordingCaller(t, recordingLocalPerson)
		r := recordingRequest(t, recordingLocalPerson, http.MethodPost, "/api/panes/"+tp.p.ID+"/recording", `{"on":true}`)
		if origin == "" {
			r.Header.Del("Origin")
		} else {
			r.Header.Set("Origin", origin)
		}
		r.SetPathValue("id", tp.p.ID)
		w := newRecorder()
		a.setPaneRecording(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	r := recordingRequest(t, recordingLocalPerson, http.MethodPost, "/api/panes/"+tp.p.ID+"/recording", `{"on":true}`)
	r.RemoteAddr = "192.0.2.7:4444" // not this computer
	r.SetPathValue("id", tp.p.ID)
	w := newRecorder()
	a.setPaneRecording(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("remote address: %d %s", w.Code, w.Body.String())
	}
	if tp.p.recording.active.Load() != nil {
		t.Fatal("a refused request switched recording on")
	}
}

func TestDeletePaneRecordingsNeverFollowsASymlinkedRecordingsFolder(t *testing.T) {
	data := t.TempDir()
	outside := t.TempDir()
	victimDir := filepath.Join(outside, "pane_x")
	if err := os.MkdirAll(victimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	victims := []string{filepath.Join(victimDir, "keep.cast"), filepath.Join(victimDir, "notes.txt")}
	for _, v := range victims {
		if err := os.WriteFile(v, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(data, recordingsDirName)); err != nil {
		t.Fatal(err)
	}
	hub := newRecordingHub(data)
	if _, err := hub.deletePane("pane_x"); err == nil {
		t.Fatal("deleted through a symlinked recordings/")
	}
	for _, v := range victims {
		if !exists(v) {
			t.Fatalf("%s was deleted", v)
		}
	}
	// In a real pane folder, only the recorder's .cast files go.
	if err := os.Remove(filepath.Join(data, recordingsDirName)); err != nil {
		t.Fatal(err)
	}
	keep := writeRecordingFixture(t, hub, "pane_y", "notes.txt", 3, recordingTestNow())
	gone := writeRecordingFixture(t, hub, "pane_y", "20261119T000000Z.cast", 3, recordingTestNow())
	if n, err := hub.deletePane("pane_y"); err != nil || n != 1 {
		t.Fatalf("deleted %d: %v", n, err)
	}
	if exists(gone) || !exists(keep) {
		t.Fatalf("cast %v, other %v", exists(gone), exists(keep))
	}
}

func TestRecordingRecountKeepsUnflushedReservations(t *testing.T) {
	hub := newRecordingHub(t.TempDir())
	hub.totalCap = 100
	// An open file can't be deleted for room, so only the account decides.
	hub.open[writeRecordingFixture(t, hub, "pane_a", "20261119T000000Z.cast", 40, recordingTestNow())] = true
	if !hub.reserve(50) { // 90: 50 still in a writer's buffer
		t.Fatal("refused under the cap")
	}
	hub.sweep() // recounts from disk
	if hub.total != 90 {
		t.Fatalf("recount forgot reserved bytes: total %d", hub.total)
	}
	if hub.reserve(20) {
		t.Fatal("reserved past the cap after a recount")
	}
	hub.written(50)
	hub.unreserve(0)
	if hub.pending != 0 {
		t.Fatalf("pending %d", hub.pending)
	}
}

// An (f) rule that holds when the writer stops for another reason (here the
// disk cap) still cuts back to the last passing check.
func TestRecordingCapStopStillCutsUncheckedOutput(t *testing.T) {
	tp := newRecTestPane(t)
	tp.m.recordingHub().totalCap = 1200
	rec := tp.start()
	tp.send(FrameData, []byte("CHECKED-OUTPUT"))
	tp.sync()
	n := tp.checks.Load()
	for tp.checks.Load() < n+2 {
		waitBriefly()
	}
	hold := make(chan struct{})
	tp.hold.Store(&hold)
	<-tp.blocked
	refused := recordingPaneFacts{Agent: "claude", Cwd: "/work/project", LiveCwds: []string{"/etc"}, AgentRunning: true}
	tp.facts.Store(&refused)
	for i := 0; i < 40; i++ {
		tp.send(FrameData, []byte(strings.Repeat("U", 50)+"UNCHECKED"))
	}
	tp.sync()
	waitRecorder(t, rec)
	close(hold)
	if rec.reason != recordingStopCwd {
		t.Fatalf("reason %q", rec.reason)
	}
	var out string
	for _, f := range castFiles(t, tp.data, tp.p.ID) {
		_, ev := readCast(t, f)
		out += outputOf(ev)
	}
	if !strings.Contains(out, "CHECKED-OUTPUT") || strings.Contains(out, "UNCHECKED") {
		t.Fatalf("files hold %q", out)
	}
}

func TestTurnSnapshotsNeverStoreRecordings(t *testing.T) {
	repo, git := hardeningRepo(t)
	data := filepath.Join(repo, "soludata")
	cast := filepath.Join(data, recordingsDirName, "pane_z", "20261119T000000Z.cast")
	if err := os.MkdirAll(filepath.Dir(cast), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cast, []byte("fixture-recording-content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ok.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "ok.txt")
	git("commit", "-qm", "base")
	s, err := gitTurnSnapshotter{dataDir: data}.snapshot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if s.secretSkipped == 0 {
		t.Fatal("the recording was not skipped")
	}
	out, _ := exec.Command("git", "-C", repo, "ls-tree", "-r", "--name-only", s.tree).Output()
	if strings.Contains(string(out), recordingsDirName) {
		t.Fatalf("snapshot tree holds a recording:\n%s", out)
	}
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func waitBriefly() { time.Sleep(5 * time.Millisecond) }

func recordingTestNow() time.Time { return time.Now() }

// Round 2: a pane that closes before the next check keeps only what the last
// passing check covered.
func TestRecordingCloseKeepsOnlyCheckedOutput(t *testing.T) {
	tp := newRecTestPane(t)
	rec := tp.start()
	tp.send(FrameData, []byte("CHECKED-OUTPUT"))
	tp.sync()
	n := tp.checks.Load()
	for tp.checks.Load() < n+2 {
		waitBriefly()
	}
	hold := make(chan struct{})
	tp.hold.Store(&hold)
	<-tp.blocked
	tp.send(FrameData, []byte("UNCHECKED-TAIL"))
	tp.sync()
	tp.server.Close()
	<-tp.p.done
	waitRecorder(t, rec)
	close(hold)
	_, ev := readCast(t, castFiles(t, tp.data, tp.p.ID)[0])
	if out := outputOf(ev); !strings.Contains(out, "CHECKED-OUTPUT") || strings.Contains(out, "UNCHECKED") {
		t.Fatalf("after close the file holds %q", out)
	}
	if m := marks(ev); len(m) == 0 || m[len(m)-1] != "stopped: "+recordingStopClosed {
		t.Fatalf("marks %v", m)
	}
}

// Round 2: a change made while the switch-on creates the file is seen.
func TestRecordingSwitchOnRechecksAfterActivation(t *testing.T) {
	tp := newRecTestPane(t)
	var calls int
	prev := recordingFactsOf
	recordingFactsOf = func(*Pane) recordingPaneFacts {
		calls++
		f := allowedRecordingFacts()
		if calls > 1 { // the handle became a refused one during the start
			f.Handle = "ops"
		}
		return f
	}
	t.Cleanup(func() { recordingFactsOf = prev })
	if _, err := tp.m.StartRecording(tp.p, "local"); err != nil {
		t.Fatal(err)
	}
	if rec := tp.p.recording.active.Load(); rec != nil {
		t.Fatal("recording left on after a refused change during the start")
	}
	if d := tp.p.Recording(); d != nil && d.On {
		t.Fatalf("status %+v", d)
	}
}

// Round 2: on Linux a live process whose folder can't be read fails closed;
// an exited one is skipped.
func TestRecordingLiveFoldersFailClosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		if _, unknown := recordingLiveCwds([]int{os.Getpid()}); !unknown {
			t.Fatal("folders read without /proc")
		}
		return
	}
	cwds, unknown := recordingLiveCwds([]int{os.Getpid(), 1 << 30})
	if unknown || len(cwds) != 1 {
		t.Fatalf("own process: %v %v", cwds, unknown)
	}
	if os.Geteuid() != 0 {
		if _, err := os.Readlink("/proc/1/cwd"); err != nil {
			if _, unknown := recordingLiveCwds([]int{os.Getpid(), 1}); !unknown {
				t.Fatal("an unreadable live process's folder was ignored")
			}
		}
	}
}

// Round 2: agent git status on a worktree holding the data dir never lists
// recordings.
func TestAgentGitStatusNeverListsRecordings(t *testing.T) {
	repo, git := hardeningRepo(t)
	data := filepath.Join(repo, "soludata")
	cast := filepath.Join(data, recordingsDirName, "pane_z", "20261119T000000Z.cast")
	if err := os.MkdirAll(filepath.Dir(cast), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cast, []byte("fixture-recording-content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "other.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ok.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "ok.txt")
	git("commit", "-qm", "base")
	// Even a recording someone committed is never listed.
	git("add", "-f", cast)
	out, err := hardenedGitStatus(repo, true, 1, data)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), recordingsDirName) || strings.Contains(string(raw), "20261119T000000Z") {
		t.Fatalf("git status lists recordings: %s", raw)
	}
}
