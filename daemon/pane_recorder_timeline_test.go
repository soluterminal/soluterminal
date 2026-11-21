package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ST-317 phase 1 (amendment 2026-11-20 sections 5 to 7, Addenda A and B):
// the session layout timeline and the view record route. Synthetic panes and
// fixture strings only; nothing real is recorded.

// timelineFixture: two recorded panes and an unrecorded one in a workspace
// with a fixture name, title and folder, plus a browser tile with a URL.
type timelineFixture struct {
	tp      *recTestPane
	a       *apiServer
	second  *Pane
	other   *Pane
	outside *Pane
}

const (
	tlTitleMark  = "TITLE-MARK-7731"
	tlPathMark   = "PATH-MARK-7731"
	tlWSMark     = "WORKSPACE-MARK-7731"
	tlURLMark    = "URL-MARK-7731"
	tlOutputMark = "OUTPUT-MARK-7731"
)

func newTimelineFixture(t *testing.T) *timelineFixture {
	t.Helper()
	tp := newRecTestPane(t)
	f := &timelineFixture{tp: tp}
	f.second = addRecTestPane(t, tp, "pane_second")
	f.other = addRecTestPane(t, tp, "pane_other")
	f.outside = addRecTestPane(t, tp, "pane_outside")
	for _, p := range []*Pane{tp.p, f.second, f.other, f.outside} {
		p.mu.Lock()
		p.Title = tlTitleMark + " " + p.ID
		p.Cwd = "/home/u/" + tlPathMark
		p.mu.Unlock()
	}
	f.second.SetIdentity(&PaneIdentity{Handle: "builder"})
	workspaces := []*Workspace{
		{Name: tlWSMark, PaneIds: []string{tp.p.ID, f.second.ID, f.other.ID, "browser_1"},
			Browsers: []BrowserInfoDef{{ID: "browser_1", URL: "https://example.test/" + tlURLMark}}},
		{Name: "Elsewhere " + tlWSMark, PaneIds: []string{f.outside.ID}},
	}
	tp.m.recordingWorkspaces = func() []*Workspace { return workspaces }
	f.a = &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
	return f
}

func (f *timelineFixture) start(t *testing.T, p *Pane) *paneRecorder {
	t.Helper()
	if _, err := f.tp.m.StartRecording(p, "local", recordingStartOptions{}); err != nil {
		t.Fatalf("start %s: %v", p.ID, err)
	}
	return p.recording.active.Load()
}

func timelineFiles(t *testing.T, data string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(data, recordingsDirName, recordingSessionsDirName, "*.jsonl"))
	return files
}

func readTimeline(t *testing.T, path string) (raw string, lines []map[string]any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		var l map[string]any
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("not a JSON line: %q", sc.Text())
		}
		lines = append(lines, l)
	}
	return string(b), lines
}

func linesOf(lines []map[string]any, ev string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["ev"] == ev {
			out = append(out, l)
		}
	}
	return out
}

func callView(t *testing.T, a *apiServer, c recCaller, body string) *httptest.ResponseRecorder {
	t.Helper()
	asRecordingCaller(t, c)
	r := recordingRequest(t, c, http.MethodPost, "/api/recording/view", body)
	w := httptest.NewRecorder()
	a.recordingView(w, r)
	return w
}

func (f *timelineFixture) stopAll(t *testing.T, recs ...*paneRecorder) {
	t.Helper()
	for _, p := range []*Pane{f.tp.p, f.second, f.other, f.outside} {
		f.tp.m.StopRecording(p, "test")
	}
	for _, r := range recs {
		waitRecorder(t, r)
	}
	// The session ends once nothing records.
	deadline := time.Now().Add(5 * time.Second)
	for f.tp.m.currentTimeline() != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

const viewBody = `{"state":"shown","window":{"w":1600,"h":1000},"theme":"dark","focus":"pane_rectest",` +
	`"panes":[{"id":"pane_rectest","x":0,"y":30,"w":800,"h":900,"scroll":12},{"id":"pane_second","x":800,"y":30,"w":800,"h":900,"max":false},{"id":"pane_unknown","x":1,"y":1,"w":1,"h":1}],` +
	`"sidebar":{"collapsed":false,"closedGroups":["w1","w9"]},"panel":"none",` +
	`"display":{"dpr":150,"zoom":100,"font":"jetbrains-mono","size":13,"lineHeight":120,"letterSpacing":0,"xterm":[5,5,0],"unicode":11}}`

// The whole session: header with the shared clock, a cast line per file on
// it, a pane line per pane of the recorded workspace (and none for the other
// workspace), REC marks, a stop code, a message chip as ids, counts, the view
// record with unknown ids dropped, and the end. Only allowed event types, and
// none of the fixture's title, path, workspace name, URL or output.
func TestRecordingTimelineHoldsIdsEnumsAndNumbersOnly(t *testing.T) {
	f := newTimelineFixture(t)
	recordingHourUnit = time.Hour
	if len(timelineFiles(t, f.tp.data)) != 0 {
		t.Fatal("a timeline exists before anything records")
	}
	r1 := f.start(t, f.tp.p)
	r2 := f.start(t, f.second)
	f.tp.send(FrameData, []byte(tlOutputMark))
	f.tp.sync()
	f.tp.m.recordingTimelineMessage(f.second.ID, f.tp.p.ID)
	f.tp.m.recordingTimelineMessage(f.second.ID, f.outside.ID) // sender outside the set: no "from"
	f.tp.m.recordingTimelineMessage(f.outside.ID, f.tp.p.ID)   // receiver outside the set: nothing
	if w := callView(t, f.a, recordingLocalPerson, viewBody); w.Code != http.StatusOK {
		t.Fatalf("view: %d %s", w.Code, w.Body.String())
	} else if !strings.Contains(w.Body.String(), `"w1"`) {
		t.Fatalf("no index map for the window: %s", w.Body.String())
	}
	time.Sleep(150 * time.Millisecond)
	f.stopAll(t, r1, r2)

	files := timelineFiles(t, f.tp.data)
	if len(files) != 1 {
		t.Fatalf("timelines %v", files)
	}
	info, _ := os.Stat(files[0])
	dirInfo, _ := os.Stat(filepath.Dir(files[0]))
	if info.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("modes %v %v", info.Mode(), dirInfo.Mode())
	}
	raw, lines := readTimeline(t, files[0])
	for _, mark := range []string{tlTitleMark, tlPathMark, tlWSMark, tlURLMark, tlOutputMark, "example.test", "/home/"} {
		if strings.Contains(raw, mark) {
			t.Fatalf("timeline holds %q:\n%s", mark, raw)
		}
	}
	for _, l := range lines {
		if !recordingTimelineEvents[l["ev"].(string)] {
			t.Fatalf("event type %v written", l["ev"])
		}
	}
	h := lines[0]
	if h["ev"] != tlHeader || h["v"].(float64) != 1 || h["t0"].(float64) <= 0 {
		t.Fatalf("header %v", h)
	}
	casts := linesOf(lines, tlCast)
	if len(casts) != 2 {
		t.Fatalf("casts %v", casts)
	}
	for _, c := range casts {
		if strings.Contains(c["file"].(string), "/") || !recordingFileNamePattern.MatchString(c["file"].(string)) || c["offsetMs"] == nil {
			t.Fatalf("cast %v", c)
		}
	}
	panes := map[string]map[string]any{}
	for _, l := range linesOf(lines, tlPane) {
		panes[l["pane"].(string)] = l
	}
	if _, ok := panes[f.outside.ID]; ok {
		t.Fatal("a pane of an unrecorded workspace is in the timeline")
	}
	if p := panes["browser_1"]; p == nil || p["kind"] != "browser" || p["reason"] != "browser_pane" || p["recorded"] != false {
		t.Fatalf("browser tile %v", panes["browser_1"])
	}
	if p := panes[f.other.ID]; p == nil || p["recorded"] != false || p["reason"] != "not_recorded" || p["ws"] != "w1" {
		t.Fatalf("unrecorded pane %v", panes[f.other.ID])
	}
	if p := panes[f.second.ID]; p == nil || p["handle"] != "builder" || p["agent"] != "claude" || p["kind"] != "terminal" {
		t.Fatalf("second pane %v", panes[f.second.ID])
	}
	msgs := linesOf(lines, tlMsg)
	if len(msgs) != 2 || msgs[0]["from"] != f.tp.p.ID || msgs[1]["from"] != nil {
		t.Fatalf("messages %v", msgs)
	}
	views := linesOf(lines, tlView)
	if len(views) != 1 {
		t.Fatalf("views %v", views)
	}
	v := views[0]
	if v["ws"] != "w1" || len(v["panes"].([]any)) != 2 || v["focus"] != "pane_rectest" {
		t.Fatalf("view %v", v)
	}
	if groups := v["sidebar"].(map[string]any)["closedGroups"].([]any); len(groups) != 1 || groups[0] != "w1" {
		t.Fatalf("unknown session index kept: %v", groups)
	}
	if len(linesOf(lines, tlRec)) < 4 || len(linesOf(lines, tlStop)) != 2 || linesOf(lines, tlStop)[0]["reason"] != "switched_off" {
		t.Fatalf("rec/stop %v %v", linesOf(lines, tlRec), linesOf(lines, tlStop))
	}
	if c := linesOf(lines, tlCounts); len(c) == 0 || c[0]["counts"].(map[string]any)["workspaces"].(float64) != 2 {
		t.Fatalf("counts %v", c)
	}
	if lines[len(lines)-1]["ev"] != tlEnd {
		t.Fatalf("last line %v", lines[len(lines)-1])
	}
}

// The view route is gated as switch-on, answers 409 while nothing records,
// and refuses anything outside its strict schema.
func TestRecordingViewRoute(t *testing.T) {
	f := newTimelineFixture(t)
	for _, c := range recordingNonPersonCallers {
		if w := callView(t, f.a, c, viewBody); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
	}
	if w := callView(t, f.a, recordingLocalPerson, viewBody); w.Code != http.StatusConflict {
		t.Fatalf("not recording: %d %s", w.Code, w.Body.String())
	}
	if w := callView(t, f.a, recordingLocalPerson, `{"state":"shown","away":true}`); w.Code != http.StatusConflict {
		t.Fatalf("away while nothing records: %d", w.Code)
	}
	if exists(filepath.Join(f.tp.data, recordingsDirName)) {
		t.Fatal("a refused view record created the folder")
	}
	rec := f.start(t, f.tp.p)
	for _, body := range []string{
		`{"state":"shown","window":{"w":10,"h":10},"title":"x"}`,
		`{"state":"shown","state":"shown","window":{"w":10,"h":10}}`,
		`{"state":"Shown","window":{"w":10,"h":10}}`,
		`{"state":"hidden","window":{"w":10,"h":10}}`,
		`{"state":"shown","window":{"w":10,"h":10},"theme":"solarized"}`,
		`{"state":"shown","window":{"w":10,"h":10},"panel":"Credentials"}`,
		`{"state":"shown","window":{"w":10,"h":10},"focus":"../etc"}`,
		`{"state":"shown","window":{"w":0,"h":10}}`,
		`{"state":"shown","window":{"w":10,"h":99999}}`,
		`{"state":"shown","window":{"w":10,"h":10},"panes":[{"id":"pane_rectest","x":0,"y":0,"w":1,"h":1,"scroll":20001}]}`,
		`{"state":"shown","window":{"w":10,"h":10},"panes":[{"id":"pane_rectest","x":0,"y":0,"w":1,"h":1,"scroll":-1}]}`,
		`{"state":"shown","window":{"w":10,"h":10},"panes":[{"id":"pane_rectest","x":0.5,"y":0,"w":1,"h":1}]}`,
		`{"state":"shown","window":{"w":10,"h":10},"panes":[{"id":"pane rectest","x":0,"y":0,"w":1,"h":1}]}`,
		`{"state":"shown","window":{"w":10,"h":10},"sidebar":{"collapsed":true,"closedGroups":["My workspace"]}}`,
		`{"state":"shown","window":{"w":10,"h":10},"display":{"dpr":100,"zoom":100,"font":"Fira Code","size":13,"lineHeight":120,"letterSpacing":0,"xterm":[5,5,0],"unicode":11}}`,
		`{"state":"shown","window":{"w":10,"h":10},"display":{"dpr":100,"zoom":100,"font":"other","size":13,"lineHeight":120,"letterSpacing":0,"xterm":[5,5,0],"unicode":12}}`,
		`{"state":"shown","away":true,"window":{"w":10,"h":10}}`,
		`{"state":"tray","away":true}`,
		`{"state":"shown","window":{"w":10,"h":10},"text":"typed"}`,
		`{"state":"shown","window":{"w":10,"h":10},"display":{"dpr":100,"zoom":100,"font":"other","size":13,"lineHeight":120,"letterSpacing":0,"xterm":[5],"unicode":11}}`,
		`{"state":"shown","window":{"w":10,"h":10},"display":{"dpr":100,"zoom":100,"font":"other","size":13,"lineHeight":120,"letterSpacing":0,"xterm":[5,5,0,1],"unicode":11}}`,
		`{"state":"shown","window":{"w":10,"h":10},"panes":[` + strings.Repeat(`{"id":"pane_rectest","x":0,"y":0,"w":1,"h":1},`, 120) + `{"id":"pane_rectest","x":0,"y":0,"w":1,"h":1}]}`,
	} {
		if w := callView(t, f.a, recordingLocalPerson, body); w.Code != http.StatusBadRequest {
			t.Errorf("%.90s: %d %s", body, w.Code, w.Body.String())
		}
	}
	// Only unknown panes: nothing of this view is recorded.
	if w := callView(t, f.a, recordingLocalPerson, `{"state":"shown","window":{"w":10,"h":10},"panes":[{"id":"pane_outside","x":0,"y":0,"w":1,"h":1}]}`); w.Code != http.StatusConflict {
		t.Fatalf("unrecorded workspace: %d", w.Code)
	}
	// The labelled gap is accepted while something records.
	if w := callView(t, f.a, recordingLocalPerson, `{"state":"shown","away":true}`); w.Code != http.StatusOK {
		t.Fatalf("away: %d %s", w.Code, w.Body.String())
	}
	f.stopAll(t, rec)
	_, lines := readTimeline(t, timelineFiles(t, f.tp.data)[0])
	if a := linesOf(lines, tlAway); len(a) != 1 || len(a[0]) != 3 {
		t.Fatalf("away line %v", a)
	}
}

// A burst of view records is written at most 10 per second, the latest
// winning.
func TestRecordingViewRecordsAreCoalesced(t *testing.T) {
	f := newTimelineFixture(t)
	rec := f.start(t, f.tp.p)
	start := time.Now()
	for i := 0; i < 60; i++ {
		body := strings.Replace(viewBody, `"scroll":12`, `"scroll":`+strconv.Itoa(i), 1)
		if w := callView(t, f.a, recordingLocalPerson, body); w.Code != http.StatusOK {
			t.Fatalf("%d: %d", i, w.Code)
		}
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(start)
	time.Sleep(150 * time.Millisecond)
	f.stopAll(t, rec)
	_, lines := readTimeline(t, timelineFiles(t, f.tp.data)[0])
	views := linesOf(lines, tlView)
	if max := int(elapsed/recordingViewEvery) + 2; len(views) > max || len(views) < 2 {
		t.Fatalf("%d view lines in %v (max %d)", len(views), elapsed, max)
	}
	last := views[len(views)-1]["panes"].([]any)[0].(map[string]any)
	if last["scroll"].(float64) != 59 {
		t.Fatalf("the latest record lost: %v", last)
	}
	for i := 1; i < len(views); i++ {
		if gap := views[i]["t"].(float64) - views[i-1]["t"].(float64); gap < 95 {
			t.Fatalf("two view lines %vms apart", gap)
		}
	}
}

// Deleting a pane's recordings deletes a finished timeline that no longer
// refers to any remaining recording; the sweep does the same; a timeline
// that still refers to another pane's recording stays.
func TestRecordingTimelineGoesWithItsRecordings(t *testing.T) {
	f := newTimelineFixture(t)
	r1 := f.start(t, f.tp.p)
	f.stopAll(t, r1)
	r2 := f.start(t, f.tp.p)
	r3 := f.start(t, f.second)
	f.stopAll(t, r2, r3)
	files := timelineFiles(t, f.tp.data)
	if len(files) != 2 {
		t.Fatalf("timelines %v", files)
	}
	if _, err := f.tp.m.DeletePaneRecordings(f.tp.p, "local"); err != nil {
		t.Fatal(err)
	}
	left := timelineFiles(t, f.tp.data)
	if len(left) != 1 {
		t.Fatalf("after deleting the first pane's recordings: %v", left)
	}
	_, lines := readTimeline(t, left[0])
	if len(linesOf(lines, tlCast)) != 2 {
		t.Fatal("the wrong timeline was kept")
	}
	// The other pane's file goes by age; the sweep then drops the timeline.
	for _, c := range castFiles(t, f.tp.data, f.second.ID) {
		old := time.Now().Add(-40 * 24 * time.Hour)
		_ = os.Chtimes(c, old, old)
	}
	f.tp.m.recordingHub().sweep()
	if left := timelineFiles(t, f.tp.data); len(left) != 0 {
		t.Fatalf("the sweep kept %v", left)
	}
}

// A timeline counts toward the cap like a recording, and the sweep deletes
// it by age.
func TestRecordingTimelineCountsTowardTheCapAndAge(t *testing.T) {
	f := newTimelineFixture(t)
	r := f.start(t, f.tp.p)
	f.stopAll(t, r)
	hub := f.tp.m.recordingHub()
	hub.mu.Lock()
	hub.recountLocked()
	files, _ := hub.listFilesLocked()
	hub.mu.Unlock()
	found := false
	for _, file := range files {
		if strings.HasSuffix(file.path, ".jsonl") {
			found = true
		}
	}
	if !found {
		t.Fatal("the timeline isn't counted")
	}
	for _, path := range timelineFiles(t, f.tp.data) {
		old := time.Now().Add(-40 * 24 * time.Hour)
		_ = os.Chtimes(path, old, old)
	}
	hub.sweep()
	if left := timelineFiles(t, f.tp.data); len(left) != 0 {
		t.Fatalf("aged timeline kept: %v", left)
	}
}

// A start and the last stop interleaving never deadlock, and a recording
// that starts while the previous session ends always has a timeline.
func TestRecordingTimelineStartWhileTheLastStops(t *testing.T) {
	f := newTimelineFixture(t)
	for i := 0; i < 20; i++ {
		r1 := f.start(t, f.tp.p)
		done := make(chan struct{})
		go func() {
			f.tp.m.StopRecording(f.tp.p, "test")
			close(done)
		}()
		r2 := f.start(t, f.second)
		if f.tp.m.currentTimeline() == nil {
			t.Fatalf("round %d: a recording runs without a timeline", i)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: stop hung", i)
		}
		waitRecorder(t, r1)
		f.stopAll(t, r2)
	}
}

// Pruning never reads or deletes through a symlinked recordings folder.
func TestRecordingTimelinePruneRefusesASymlinkedFolder(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	outside := filepath.Join(home, "outside")
	if err := os.MkdirAll(filepath.Join(outside, recordingSessionsDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, recordingSessionsDirName, "20261120T000000Z.jsonl")
	if err := os.WriteFile(victim, []byte(`{"t":0,"ev":"header"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(data, recordingsDirName)); err != nil {
		t.Fatal(err)
	}
	hub := newRecordingHub(data)
	hub.sweep()
	hub.mu.Lock()
	hub.pruneTimelinesLocked()
	hub.mu.Unlock()
	if !exists(victim) {
		t.Fatal("a file outside the recordings folder was deleted")
	}
}

// Stop reasons reach the timeline as codes only: the disk-cap notice that
// names a lowering's caller and time becomes "disk_cap", a folder stop "folder".
func TestRecordingTimelineStopCodes(t *testing.T) {
	for reason, code := range map[string]string{
		"disk cap": "disk_cap",
		"disk cap (lowered by an agent at 2026-11-20 10:03 UTC)": "disk_cap",
		"folder":        "folder",
		"plain shell":   "shell",
		"8-hour limit":  "time_limit",
		"switched off":  "switched_off",
		"anything else": "other",
	} {
		if got := recordingStopCode(reason); got != code {
			t.Errorf("%q: %q, want %q", reason, got, code)
		}
	}
}

// Addendum F 3: a file name's start time is used only when it is plausible:
// not after the last write, and not earlier than the last write minus the
// longest recording plus an hour.
func TestRecordingTrustedStart(t *testing.T) {
	mtime := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	name := func(at time.Time) string { return "/r/p/" + at.UTC().Format(recordingFileTimeLayout) + ".cast" }
	for _, tc := range []struct {
		start time.Time
		ok    bool
	}{
		{mtime.Add(-2 * time.Hour), true},
		{mtime, true},
		{mtime.Add(-25 * time.Hour), true},
		{mtime.Add(time.Minute), false},
		{mtime.Add(-25*time.Hour - time.Minute), false},
		{mtime.Add(-30 * 24 * time.Hour), false},
	} {
		got, ok := recordingTrustedStart(recordingFileInfo{path: name(tc.start), mtime: mtime})
		if ok != tc.ok || (ok && !got.Equal(tc.start)) {
			t.Errorf("start %v: %v %v", tc.start, got, ok)
		}
	}
	// A name far in the past can't stretch a file's protection: after an
	// agent's lowering, a file whose name claims an old start, last written
	// after the lowering, gets only the lowered age.
	data := t.TempDir()
	hub := newRecordingHub(data)
	now := time.Now()
	hub.now = func() time.Time { return now }
	other := writeRecordingFixture(t, hub, "pane_b", now.Add(-time.Hour).UTC().Format(recordingFileTimeLayout)+".cast", 1, now.Add(-time.Hour))
	if err := hub.changeSettings(recordingSettings{DeleteAfterDays: 1, CapMB: 2000}, false, "agent"); err != nil {
		t.Fatal(err)
	}
	forged := writeRecordingFixture(t, hub, "pane_a", now.Add(-20*24*time.Hour).UTC().Format(recordingFileTimeLayout)+".cast", 1, now.Add(time.Minute))
	hub.now = func() time.Time { return now.Add(2 * 24 * time.Hour) }
	hub.sweep()
	if exists(forged) {
		t.Fatal("an implausible name kept a file past the lowered age")
	}
	if !exists(other) {
		t.Fatal("a plausible older file lost its protection")
	}
}

// The view route writes no log line per record (a window sends several a
// second); refusals still answer 403.
func TestRecordingViewRouteIsQuiet(t *testing.T) {
	f := newTimelineFixture(t)
	rec := f.start(t, f.tp.p)
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	for i := 0; i < 5; i++ {
		callView(t, f.a, recordingLocalPerson, viewBody)
	}
	log.SetOutput(prev)
	if strings.Contains(buf.String(), "authority:") {
		t.Fatalf("view records logged:\n%s", buf.String())
	}
	f.stopAll(t, rec)
}
