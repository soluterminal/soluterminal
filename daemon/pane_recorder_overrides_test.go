package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ST-317 (amendment 2026-11-20 sections 1 to 4, Addendum B): the rules as
// defaults the person overrides per switch-on, the hard folders, the time
// limit, the settings bounds and the workspace switch. Synthetic panes only.

func startWith(t *testing.T, tp *recTestPane, f recordingPaneFacts, opts recordingStartOptions) (*paneRecorder, error) {
	t.Helper()
	tp.facts.Store(&f)
	if _, err := tp.m.StartRecording(tp.p, "local", opts); err != nil {
		return nil, err
	}
	return tp.p.recording.active.Load(), nil
}

func stopAndWait(t *testing.T, tp *recTestPane, rec *paneRecorder) {
	t.Helper()
	tp.m.StopRecording(tp.p, "test")
	waitRecorder(t, rec)
}

// Each section 2 default is refused without its override and allowed with it.
func TestRecordingDefaultsRefuseWithoutOverrideAndAllowWithIt(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, tc := range []struct {
		rule  recordingRule
		facts recordingPaneFacts
	}{
		{recordingRuleHandle, recordingPaneFacts{Handle: "release-lead", Cwds: []string{"/work"}, AgentRunning: true}},
		{recordingRuleHandle, recordingPaneFacts{Handle: "gate-main", Cwds: []string{"/work"}, AgentRunning: true}},
		{recordingRuleShell, recordingPaneFacts{Cwds: []string{"/work"}}},
		{"folder:etc", recordingPaneFacts{Cwds: []string{"/etc/ssl"}, AgentRunning: true}},
		{"folder:config", recordingPaneFacts{Cwds: []string{"/work", filepath.Join(home, ".config", "app")}, AgentRunning: true}},
		{"folder:launch_security", recordingPaneFacts{Cwds: []string{filepath.Join(home, "wsl-projects/launch/roles/security")}, AgentRunning: true}},
		{"folder:launch_deploy", recordingPaneFacts{Cwds: []string{filepath.Join(home, "wsl-projects/launch/deploy/x")}, AgentRunning: true}},
	} {
		t.Run(string(tc.rule)+" "+strings.Join(tc.facts.Cwds, ","), func(t *testing.T) {
			tp := newRecTestPane(t)
			_, err := startWith(t, tp, tc.facts, recordingStartOptions{})
			var refused recordingRefusedError
			if !errors.As(err, &refused) || refused.rule != tc.rule {
				t.Fatalf("without override: %v", err)
			}
			rec, err := startWith(t, tp, tc.facts, recordingStartOptions{Overrides: []string{string(tc.rule)}})
			if err != nil || rec == nil {
				t.Fatalf("with override: %v", err)
			}
			if d := tp.p.Recording(); len(d.Overrides) != 1 || d.Overrides[0] != string(tc.rule) || d.Hours != recordingHoursDefault {
				t.Fatalf("dto %+v", d)
			}
			// The overridden default no longer stops it on the 5 s check.
			n := tp.checks.Load()
			for tp.checks.Load() < n+3 {
				time.Sleep(5 * time.Millisecond)
			}
			if tp.p.recording.active.Load() != rec {
				t.Fatalf("stopped despite the override: %q", rec.reason)
			}
			stopAndWait(t, tp, rec)
			// Not remembered: the next switch-on without it is refused again.
			if _, err := startWith(t, tp, tc.facts, recordingStartOptions{}); !errors.As(err, &refused) {
				t.Fatalf("override carried to the next switch-on: %v", err)
			}
		})
	}
}

// The hard folders refuse switch-on and stop a running recording even with
// every override chosen, on the shell's folder and on the agent's.
func TestRecordingHardFoldersIgnoreEveryOverride(t *testing.T) {
	home, _ := os.UserHomeDir()
	all := recordingStartOptions{Overrides: []string{"handle", "shell", "folder:config", "folder:etc", "folder:launch_security", "folder:launch_deploy"}}
	// recordingFactsOf is one seam: each case gets its own pane, and its data
	// dir is the hard one.
	for _, hard := range append(recordingHardDirs(home, ""), "<data>", "/etc/solu-gate", "/etc/solu-release/keys") {
		if hard == "" {
			continue
		}
		for _, sub := range []bool{false, true} {
			tp := newRecTestPane(t)
			dir := hard
			if dir == "<data>" {
				dir = tp.data
			}
			cwds := []string{dir}
			if sub {
				cwds = []string{"/work", filepath.Join(dir, "sub")}
			}
			facts := recordingPaneFacts{Handle: "secure", Cwds: cwds}
			_, err := startWith(t, tp, facts, all)
			var refused recordingRefusedError
			if !errors.As(err, &refused) || refused.rule != recordingRuleHardFolder {
				t.Errorf("%v: switch-on with every override: %v", cwds, err)
			}
		}
	}
	tp0 := newRecTestPane(t)
	unknown := recordingPaneFacts{Cwds: []string{"/work"}, CwdUnknown: true, AgentRunning: true}
	if _, err := startWith(t, tp0, unknown, all); err == nil || !strings.Contains(err.Error(), recordingStopCwdUnknown) {
		t.Errorf("unreadable folder: %v", err)
	}
	// Running with every override, the agent moves into ~/.ssh: it stops.
	tp := newRecTestPane(t)
	rec, err := startWith(t, tp, recordingPaneFacts{Handle: "ops", Cwds: []string{"/etc"}}, all)
	if err != nil {
		t.Fatal(err)
	}
	moved := recordingPaneFacts{Handle: "ops", Cwds: []string{"/etc", filepath.Join(home, ".ssh")}}
	tp.facts.Store(&moved)
	waitRecorder(t, rec)
	if rec.reason != recordingStopHardFolder {
		t.Fatalf("reason %q", rec.reason)
	}
}

// An override the pane doesn't need is dropped: not on the recording, not in
// the log. An unknown code and a time limit outside 1 to 24 hours are refused.
func TestRecordingOverrideAndTimeLimitBounds(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	tp := newRecTestPane(t)
	for _, opts := range []recordingStartOptions{
		{Hours: -1}, {Hours: recordingHoursMax + 1}, {Hours: 1000}, {Overrides: []string{"hard_folder"}},
		{Overrides: []string{"cwd_unknown"}}, {Overrides: []string{"everything"}},
	} {
		var bad errRecordingBadRequest
		if _, err := startWith(t, tp, allowedRecordingFacts(), opts); !errors.As(err, &bad) || recordingStartStatus(err) != http.StatusBadRequest {
			t.Errorf("%+v: %v", opts, err)
		}
	}
	rec, err := startWith(t, tp, allowedRecordingFacts(), recordingStartOptions{Hours: recordingHoursMax, Overrides: []string{"shell", "handle"}})
	if err != nil {
		t.Fatal(err)
	}
	if d := tp.p.Recording(); d.Hours != 24 || len(d.Overrides) != 0 {
		t.Fatalf("unneeded overrides kept: %+v", d)
	}
	stopAndWait(t, tp, rec)
	if !strings.Contains(buf.String(), "hours=24 overrides=none") {
		t.Fatalf("log:\n%s", buf.String())
	}
}

// The log names the overridden defaults as codes, never the handle, a folder
// or output.
func TestRecordingOverrideLogNamesCodesOnly(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	tp := newRecTestPane(t)
	facts := recordingPaneFacts{Handle: "release-HANDLEMARK", Cwds: []string{"/etc/FOLDERMARK"}}
	rec, err := startWith(t, tp, facts, recordingStartOptions{Hours: 3, Overrides: []string{"shell", "folder:etc", "handle"}})
	if err != nil {
		t.Fatal(err)
	}
	tp.send(FrameData, []byte("OUTPUTMARK"))
	tp.sync()
	stopAndWait(t, tp, rec)
	out := buf.String()
	if !strings.Contains(out, "recording: pane "+tp.p.ID+" on by the person (local) hours=3 overrides=handle,folder:etc,shell") {
		t.Fatalf("log:\n%s", out)
	}
	for _, leak := range []string{"HANDLEMARK", "FOLDERMARK", "OUTPUTMARK", "/etc"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log holds %q:\n%s", leak, out)
		}
	}
}

// The time limit is the person's choice at switch-on.
func TestRecordingStopsAtTheChosenTimeLimit(t *testing.T) {
	tp := newRecTestPane(t)
	recordingHourUnit = 20 * time.Millisecond
	rec, err := startWith(t, tp, allowedRecordingFacts(), recordingStartOptions{Hours: 2})
	if err != nil {
		t.Fatal(err)
	}
	waitRecorder(t, rec)
	if d := tp.p.Recording(); rec.reason != "2-hour limit" || d.StoppedReason != "2-hour limit" {
		t.Fatalf("reason %q dto %+v", rec.reason, d)
	}
}

// C1: when the agent exits, the pane is a plain shell. Not overridden, the
// recording stops with "plain shell" (the cut is in
// TestRecordingPolicyStopCutsBackToTheLastPassingCheck); overridden, it goes on.
func TestRecordingAgentExitStopsUnlessShellOverridden(t *testing.T) {
	tp := newRecTestPane(t)
	rec, err := startWith(t, tp, allowedRecordingFacts(), recordingStartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	shell := recordingPaneFacts{Cwds: []string{"/work/project"}}
	tp.facts.Store(&shell)
	waitRecorder(t, rec)
	if d := tp.p.Recording(); rec.reason != recordingStopShell || "recording stopped: "+d.StoppedReason != "recording stopped: plain shell" {
		t.Fatalf("reason %q", rec.reason)
	}
	// A shell pane switched on with the shell override keeps recording
	// through an agent starting and exiting.
	rec, err = startWith(t, tp, shell, recordingStartOptions{Overrides: []string{"shell"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []recordingPaneFacts{allowedRecordingFacts(), shell} {
		f := f
		tp.facts.Store(&f)
		n := tp.checks.Load()
		for tp.checks.Load() < n+2 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if tp.p.recording.active.Load() != rec {
		t.Fatalf("stopped: %q", rec.reason)
	}
	stopAndWait(t, tp, rec)
}

// A handle override survives a rename into the handle list; without it the
// rename stops the recording at once.
func TestRecordingHandleOverrideSurvivesARename(t *testing.T) {
	tp := newRecTestPane(t)
	rec, err := startWith(t, tp, recordingPaneFacts{Handle: "gate-x", Cwds: []string{"/work"}, AgentRunning: true}, recordingStartOptions{Overrides: []string{"handle"}})
	if err != nil {
		t.Fatal(err)
	}
	tp.p.SetIdentity(&PaneIdentity{Handle: "secure"})
	time.Sleep(30 * time.Millisecond)
	if tp.p.recording.active.Load() != rec {
		t.Fatalf("stopped: %q", rec.reason)
	}
	stopAndWait(t, tp, rec)
}

// The status route's check is codes only: no path, folder name or handle.
func TestRecordingStatusCheckIsCodesOnly(t *testing.T) {
	tp := newRecTestPane(t)
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
	home, _ := os.UserHomeDir()
	f := recordingPaneFacts{Handle: "publish-HANDLEMARK", Cwds: []string{filepath.Join(home, ".config", "FOLDERMARK"), filepath.Join(home, ".ssh", "KEYMARK")}}
	tp.facts.Store(&f)
	w := callRecording(t, a, recordingNonPersonCallers[0], http.MethodGet, "/recording", ``, tp.p.ID)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatal(body)
	}
	var st paneRecordingDTO
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || st.Check == nil {
		t.Fatalf("%v %s", err, body)
	}
	if st.Check.Hard != "hard_folder" || strings.Join(st.Check.Defaults, ",") != "handle,folder:config,shell" {
		t.Fatalf("check %+v", st.Check)
	}
	for _, leak := range []string{"HANDLEMARK", "FOLDERMARK", "KEYMARK", "/", "publish"} {
		if strings.Contains(body, leak) {
			t.Fatalf("status holds %q: %s", leak, body)
		}
	}
	tp.p.mu.Lock()
	tp.p.Status = PaneExited
	tp.p.mu.Unlock()
	if c := tp.m.RecordingCheck(tp.p); c.Unavailable != "not_running" || c.Hard != "" {
		t.Fatalf("exited pane check %+v", c)
	}
	tp.p.mu.Lock()
	tp.p.Status = PaneRunning
	tp.p.mu.Unlock()
}

// Every caller and transport except the local person is refused switch-on
// with any override, through both switch routes.
func TestRecordingOverridesAreTheLocalPersonsOnly(t *testing.T) {
	tp := newRecTestPane(t)
	ws, _ := NewWorkspaceStore(t.TempDir())
	if err := ws.Put(&Workspace{Name: "Rec", PaneIds: []string{tp.p.ID}, Layout: LayoutNode{"type": "pane", "paneId": tp.p.ID}}); err != nil {
		t.Fatal(err)
	}
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}, ws: ws}
	shell := recordingPaneFacts{Cwds: []string{"/work"}}
	tp.facts.Store(&shell)
	for _, c := range recordingNonPersonCallers {
		for _, body := range []string{`{"on":true,"override":["shell"]}`, `{"on":true,"hours":24,"override":["shell","handle","folder:config"]}`} {
			if w := callRecording(t, a, c, http.MethodPost, "/recording", body, tp.p.ID); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: %d %s", c.name, body, w.Code, w.Body.String())
			}
		}
		if w := callWorkspaceRecording(t, a, c, `{"workspace":"Rec","on":true,"panes":[{"id":"`+tp.p.ID+`","override":["shell"]}]}`); w.Code != http.StatusForbidden {
			t.Errorf("%s workspace: %d %s", c.name, w.Code, w.Body.String())
		}
	}
	if tp.p.recording.active.Load() != nil || exists(filepath.Join(tp.data, recordingsDirName)) {
		t.Fatal("a refused caller switched recording on")
	}
	w := callRecording(t, a, recordingLocalPerson, http.MethodPost, "/recording", `{"on":true,"override":["shell"]}`, tp.p.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("local person: %d %s", w.Code, w.Body.String())
	}
	rec := tp.p.recording.active.Load()
	stopAndWait(t, tp, rec)
}

// The switch request is strict: unknown fields, duplicate keys, trailing data
// and terms sent with off are refused.
func TestRecordingSwitchRequestIsStrict(t *testing.T) {
	tp := newRecTestPane(t)
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
	for _, body := range []string{
		`{"on":true,"why":"x"}`, `{"on":true,"on":true}`, `{"on":true,"override":["shell"],"override":[]}`,
		`{"on":true} {"on":true}`, `{"on":false,"hours":3}`, `{"on":false,"override":["shell"]}`, `{"on":true,"hours":"8"}`,
		`{"on":true,"hours":25}`, `{"on":true,"override":["hard_folder"]}`, `{"on":true,"override":["folder"]}`,
		`{"on":true,"override":["folder:/etc"]}`,
	} {
		if w := callRecording(t, a, recordingLocalPerson, http.MethodPost, "/recording", body, tp.p.ID); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if tp.p.recording.active.Load() != nil {
		t.Fatal("a refused request switched recording on")
	}
	// The older app's request still works, with the defaults.
	if w := callRecording(t, a, recordingLocalPerson, http.MethodPost, "/recording", `{"on":true}`, tp.p.ID); w.Code != http.StatusOK {
		t.Fatalf("v1 request: %d %s", w.Code, w.Body.String())
	}
	stopAndWait(t, tp, tp.p.recording.active.Load())
}

func TestRejectDuplicateJSONKeys(t *testing.T) {
	for raw, dup := range map[string]bool{
		`{"a":1,"b":2}`:                     false,
		`{"a":1,"a":2}`:                     true,
		`{"a":{"x":1},"b":{"x":1}}`:         false,
		`{"a":{"x":1,"x":2}}`:               true,
		`{"a":[{"x":1},{"x":1}]}`:           false,
		`{"a":[{"x":1,"x":1}]}`:             true,
		`{"a":[1,"a",2],"b":"a"}`:           false,
		`{"panes":[{"id":"a"}],"panes":[]}`: true,
		`{"on":true,"On":false}`:            true,
		"{\"hours\":1,\"hour\u017f\":2}":    true,
	} {
		if err := rejectDuplicateJSONKeys([]byte(raw)); (err != nil) != dup {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

// --- record every pane in this workspace ----------------------------------

// addRecTestPane adds another synthetic pane to tp's manager.
func addRecTestPane(t *testing.T, tp *recTestPane, id string) *Pane {
	t.Helper()
	client, server := net.Pipe()
	p := &Pane{ID: id, Status: PaneRunning, agentConn: client, Agent: "claude", Cwd: "/work/project",
		Cols: 80, Rows: 24, buffer: NewRingBuffer(1 << 20), subs: map[int64]*subEntry{}, done: make(chan struct{})}
	tp.m.mu.Lock()
	tp.m.panes[id] = p
	tp.m.mu.Unlock()
	go func() {
		for {
			if _, _, err := ReadFrame(server); err != nil {
				return
			}
		}
	}()
	go p.readLoop(tp.m)
	t.Cleanup(func() { server.Close(); <-p.done })
	return p
}

func callWorkspaceRecording(t *testing.T, a *apiServer, c recCaller, body string) *httptest.ResponseRecorder {
	t.Helper()
	asRecordingCaller(t, c)
	r := recordingRequest(t, c, http.MethodPost, "/api/recording/workspace", body)
	w := httptest.NewRecorder()
	a.setWorkspaceRecording(w, r)
	return w
}

// One switch over the confirmed panes: refused panes are skipped with a code,
// a pane the person didn't confirm (opened after the confirm) is not added,
// each recorded pane has its own recording, and off stops them all.
func TestRecordingWorkspaceSwitch(t *testing.T) {
	tp := newRecTestPane(t)
	shell := addRecTestPane(t, tp, "pane_shell")
	secure := addRecTestPane(t, tp, "pane_secure")
	late := addRecTestPane(t, tp, "pane_late")
	ssh := addRecTestPane(t, tp, "pane_ssh")
	home, _ := os.UserHomeDir()
	var mu sync.Mutex
	facts := map[string]recordingPaneFacts{
		tp.p.ID:   allowedRecordingFacts(),
		shell.ID:  {Cwds: []string{"/work"}},
		secure.ID: {Handle: "secure", Cwds: []string{"/work"}, AgentRunning: true},
		late.ID:   allowedRecordingFacts(),
		ssh.ID:    {Cwds: []string{filepath.Join(home, ".ssh")}, AgentRunning: true},
	}
	recordingFactsOf = func(p *Pane) recordingPaneFacts {
		mu.Lock()
		defer mu.Unlock()
		return facts[p.ID]
	}
	ws, _ := NewWorkspaceStore(t.TempDir())
	ids := []string{tp.p.ID, shell.ID, secure.ID, late.ID, ssh.ID}
	if err := ws.Put(&Workspace{Name: "Rec", PaneIds: ids, Layout: LayoutNode{"type": "pane", "paneId": tp.p.ID}}); err != nil {
		t.Fatal(err)
	}
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}, ws: ws}
	body := `{"workspace":"Rec","on":true,"hours":4,"panes":[` +
		`{"id":"` + tp.p.ID + `"},{"id":"` + shell.ID + `","override":["shell"]},{"id":"` + secure.ID + `"},` +
		`{"id":"` + ssh.ID + `","override":["handle","shell","folder:config"]},{"id":"pane_elsewhere"}]}`
	w := callWorkspaceRecording(t, a, recordingLocalPerson, body)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var resp workspaceRecordingResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if strings.Join(resp.Recorded, ",") != tp.p.ID+","+shell.ID {
		t.Fatalf("recorded %v", resp.Recorded)
	}
	skipped := map[string]string{}
	for _, s := range resp.Skipped {
		skipped[s.ID] = s.Reason
	}
	want := map[string]string{secure.ID: "handle", late.ID: "not_confirmed", ssh.ID: "hard_folder", "pane_elsewhere": "not_in_workspace"}
	if len(skipped) != len(want) {
		t.Fatalf("skipped %v", skipped)
	}
	for id, reason := range want {
		if skipped[id] != reason {
			t.Fatalf("skipped %v, want %v", skipped, want)
		}
	}
	recs := map[string]*paneRecorder{}
	for _, p := range []*Pane{tp.p, shell} {
		rec := p.recording.active.Load()
		if rec == nil || rec.terms.hours != 4 {
			t.Fatalf("%s not recorded with 4 h", p.ID)
		}
		recs[p.ID] = rec
		if len(castFiles(t, tp.data, p.ID)) != 1 {
			t.Fatalf("%s has no file of its own", p.ID)
		}
	}
	if late.recording.active.Load() != nil || secure.recording.active.Load() != nil || ssh.recording.active.Load() != nil {
		t.Fatal("a skipped pane is recording")
	}
	// Off is anyone's, and stops every recording pane in the workspace.
	w = callWorkspaceRecording(t, a, recordingNonPersonCallers[0], `{"workspace":"Rec","on":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("off: %d %s", w.Code, w.Body.String())
	}
	for id, rec := range recs {
		waitRecorder(t, rec)
		if rec.reason != recordingStopOff {
			t.Fatalf("%s: %q", id, rec.reason)
		}
	}
}

func TestRecordingWorkspaceRequestIsStrict(t *testing.T) {
	tp := newRecTestPane(t)
	ws, _ := NewWorkspaceStore(t.TempDir())
	_ = ws.Put(&Workspace{Name: "Rec", PaneIds: []string{tp.p.ID}, Layout: LayoutNode{"type": "pane", "paneId": tp.p.ID}})
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}, ws: ws}
	id := tp.p.ID
	for _, body := range []string{
		`{"workspace":"Rec"}`, `{"on":true}`, `{"workspace":"Rec","on":true,"panes":[{"id":"` + id + `"}],"x":1}`,
		`{"workspace":"Rec","on":true,"hours":0.5,"panes":[{"id":"` + id + `"}]}`,
		`{"workspace":"Rec","on":true,"hours":25,"panes":[{"id":"` + id + `"}]}`,
		`{"workspace":"Rec","on":true,"panes":[{"id":"` + id + `","override":["hard_folder"]}]}`,
		`{"workspace":"Rec","on":true,"panes":[{"id":"` + id + `"},{"id":"` + id + `"}]}`,
		`{"workspace":"Rec","on":false,"panes":[{"id":"` + id + `"}]}`,
	} {
		if w := callWorkspaceRecording(t, a, recordingLocalPerson, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := callWorkspaceRecording(t, a, recordingLocalPerson, `{"workspace":"Nope","on":true,"panes":[]}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown workspace: %d", w.Code)
	}
	if tp.p.recording.active.Load() != nil {
		t.Fatal("a refused request switched recording on")
	}
}

// --- settings ----------------------------------------------------------------

func callRecordingSettings(t *testing.T, a *apiServer, c recCaller, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	asRecordingCaller(t, c)
	r := recordingRequest(t, c, method, "/api/settings/recording", body)
	w := httptest.NewRecorder()
	if method == http.MethodGet {
		a.getRecordingSettings(w, r)
	} else {
		a.putRecordingSettings(w, r)
	}
	return w
}

func recordingSettingsOf(t *testing.T, w *httptest.ResponseRecorder) recordingSettings {
	t.Helper()
	var d recordingSettingsDTO
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("%v %s", err, w.Body.String())
	}
	return d.recordingSettings
}

// Out of bounds is refused, never clamped; there's no unlimited value.
func TestRecordingSettingsBounds(t *testing.T) {
	tp := newRecTestPane(t)
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
	if got := recordingSettingsOf(t, callRecordingSettings(t, a, recordingNonPersonCallers[0], http.MethodGet, ``)); got != defaultRecordingSettings() || got.DeleteAfterDays != 7 || got.CapMB != 2000 {
		t.Fatalf("defaults %+v", got)
	}
	for _, body := range []string{
		`{"deleteAfterDays":0}`, `{"deleteAfterDays":31}`, `{"deleteAfterDays":-1}`, `{"capMB":499}`, `{"capMB":10001}`,
		`{"capMB":0}`, `{}`, `{"deleteAfterDays":7,"x":1}`, `{"capMB":1000,"capMB":900}`, `{"deleteAfterDays":1.5}`,
	} {
		if w := callRecordingSettings(t, a, recordingLocalPerson, http.MethodPut, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	for _, body := range []string{`{"deleteAfterDays":1}`, `{"deleteAfterDays":30}`, `{"capMB":500}`, `{"capMB":10000}`} {
		if w := callRecordingSettings(t, a, recordingLocalPerson, http.MethodPut, body); w.Code != http.StatusOK {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	info, err := os.Stat(filepath.Join(tp.data, recordingSettingsFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings file: %v %v", info, err)
	}
	// A new hub (a daemon restart) reads the saved values.
	if got := newRecordingHub(tp.data).settings(); got.DeleteAfterDays != 30 || got.CapMB != 10000 {
		t.Fatalf("reloaded %+v", got)
	}
	// A damaged or out-of-bounds file gives the defaults.
	_ = os.WriteFile(filepath.Join(tp.data, recordingSettingsFile), []byte(`{"deleteAfterDays":9999,"capMB":1}`), 0o600)
	if got := newRecordingHub(tp.data).settings(); got != defaultRecordingSettings() {
		t.Fatalf("out-of-bounds file %+v", got)
	}
}

// Raising the age or the cap is the local person's only; lowering is anyone's.
func TestRecordingSettingsRaiseIsThePersonsLowerIsAnyones(t *testing.T) {
	for _, c := range recordingNonPersonCallers {
		t.Run(c.name, func(t *testing.T) {
			tp := newRecTestPane(t)
			a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
			for _, body := range []string{`{"deleteAfterDays":8}`, `{"capMB":2001}`, `{"deleteAfterDays":3,"capMB":5000}`} {
				if w := callRecordingSettings(t, a, c, http.MethodPut, body); w.Code != http.StatusForbidden {
					t.Errorf("raise %s: %d %s", body, w.Code, w.Body.String())
				}
			}
			if got := tp.m.recordingHub().settings(); got != defaultRecordingSettings() {
				t.Fatalf("a refused raise changed %+v", got)
			}
			for _, body := range []string{`{"deleteAfterDays":3}`, `{"capMB":1000}`, `{"deleteAfterDays":3,"capMB":1000}`} {
				if w := callRecordingSettings(t, a, c, http.MethodPut, body); w.Code != http.StatusOK {
					t.Errorf("lower %s: %d %s", body, w.Code, w.Body.String())
				}
			}
			if got := tp.m.recordingHub().settings(); got.DeleteAfterDays != 3 || got.CapMB != 1000 {
				t.Fatalf("lowered %+v", got)
			}
		})
	}
	tp := newRecTestPane(t)
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
	if w := callRecordingSettings(t, a, recordingLocalPerson, http.MethodPut, `{"deleteAfterDays":30,"capMB":10000}`); w.Code != http.StatusOK {
		t.Fatalf("person raise: %d %s", w.Code, w.Body.String())
	}
}

// A lowered age or cap applies at once: the sweep deletes the files past the
// new age, then the oldest finished files until the total fits.
func TestRecordingLoweredSettingsDeleteAtOnce(t *testing.T) {
	data := t.TempDir()
	hub := newRecordingHub(data)
	now := time.Now()
	old := writeRecordingFixture(t, hub, "pane_a", "20261101T000000Z.cast", 10, now.Add(-3*24*time.Hour))
	// Sparse files of the sizes the cap reads, so the test holds no 400 MB.
	sparse := func(pane, name string, size int64, mtime time.Time) string {
		path := writeRecordingFixture(t, hub, pane, name, 0, mtime)
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return path
	}
	mid := sparse("pane_a", "20261110T000000Z.cast", 400_000_000, now.Add(-36*time.Hour))
	newer := sparse("pane_b", "20261111T000000Z.cast", 400_000_000, now.Add(-24*time.Hour))
	newest := writeRecordingFixture(t, hub, "pane_b", "20261112T000000Z.cast", 100, now.Add(-time.Hour))
	hub.sweep()
	if !exists(old) || !exists(mid) {
		t.Fatal("default sweep deleted a fresh file")
	}
	if err := hub.applySettings(recordingSettings{DeleteAfterDays: 2, CapMB: 500}); err != nil {
		t.Fatal(err)
	}
	if exists(old) || exists(mid) || !exists(newer) || !exists(newest) {
		t.Fatalf("old=%v mid=%v newer=%v newest=%v", exists(old), exists(mid), exists(newer), exists(newest))
	}
	if hub.total != 400_000_100 {
		t.Fatalf("total %d", hub.total)
	}
}

// A settings change reads, checks and saves under one lock: an agent's
// partial update can never put back a cap another request just lowered.
func TestRecordingSettingsChangesNeverRaiseThroughARace(t *testing.T) {
	tp := newRecTestPane(t)
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
	agent := recordingNonPersonCallers[0]
	asRecordingCaller(t, agent)
	put := func(body string) {
		r := recordingRequest(t, agent, http.MethodPut, "/api/settings/recording", body)
		a.putRecordingSettings(httptest.NewRecorder(), r)
	}
	for round := 0; round < 50; round++ {
		if err := tp.m.recordingHub().applySettings(defaultRecordingSettings()); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); put(`{"capMB":1000}`) }()
		go func() { defer wg.Done(); put(`{"deleteAfterDays":7}`) }()
		wg.Wait()
		if got := tp.m.recordingHub().settings(); got.CapMB != 1000 {
			t.Fatalf("round %d: the lowered cap was put back: %+v", round, got)
		}
	}
}

// An /etc/solu-* entry that is a symlink to a folder elsewhere: a pane whose
// cwd reads as that target is in a hard folder.
func TestRecordingHardEtcSoluSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	target := filepath.Join(root, "keys-elsewhere")
	for _, d := range []string{etc, target} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, filepath.Join(etc, "solu-gate")); err != nil {
		t.Fatal(err)
	}
	prev := recordingEtcDir
	recordingEtcDir = etc
	t.Cleanup(func() { recordingEtcDir = prev })
	for _, cwd := range []string{target, filepath.Join(target, "sub"), filepath.Join(etc, "solu-gate", "x")} {
		if v := recordingCheck(recordingPaneFacts{Cwds: []string{cwd}, AgentRunning: true}, "", ""); v.Hard != recordingRuleHardFolder {
			t.Errorf("%s: %+v", cwd, v)
		}
	}
	if v := recordingCheck(recordingPaneFacts{Cwds: []string{filepath.Join(root, "other")}, AgentRunning: true}, "", ""); v.Hard != "" {
		t.Errorf("unrelated folder: %+v", v)
	}
}

// A confirmed pane that is gone from the daemon while still listed in the
// saved workspace is answered as skipped, not left out.
func TestRecordingWorkspaceAnswersEveryConfirmedPane(t *testing.T) {
	tp := newRecTestPane(t)
	ws, _ := NewWorkspaceStore(t.TempDir())
	_ = ws.Put(&Workspace{Name: "Rec", PaneIds: []string{tp.p.ID, "pane_gone"}, Layout: LayoutNode{"type": "pane", "paneId": tp.p.ID}})
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}, ws: ws}
	w := callWorkspaceRecording(t, a, recordingLocalPerson, `{"workspace":"Rec","on":true,"panes":[{"id":"`+tp.p.ID+`"},{"id":"pane_gone"}]}`)
	var resp workspaceRecordingResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Recorded) != 1 || len(resp.Skipped) != 1 || resp.Skipped[0] != (workspaceRecordingSkip{ID: "pane_gone", Reason: "not_running"}) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	stopAndWait(t, tp, tp.p.recording.active.Load())
}

// A switch-on names the pane process the person confirmed; a pane restarted
// since (same id, new process) is refused, on both switch routes.
func TestRecordingSwitchOnRefusesAPaneThatRestarted(t *testing.T) {
	tp := newRecTestPane(t)
	tp.p.mu.Lock()
	tp.p.Pid = 4100
	tp.p.mu.Unlock()
	ws, _ := NewWorkspaceStore(t.TempDir())
	_ = ws.Put(&Workspace{Name: "Rec", PaneIds: []string{tp.p.ID}, Layout: LayoutNode{"type": "pane", "paneId": tp.p.ID}})
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}, ws: ws}
	if w := callRecording(t, a, recordingLocalPerson, http.MethodPost, "/recording", `{"on":true,"pid":4099}`, tp.p.ID); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "restarted") {
		t.Fatalf("single: %d %s", w.Code, w.Body.String())
	}
	w := callWorkspaceRecording(t, a, recordingLocalPerson, `{"workspace":"Rec","on":true,"panes":[{"id":"`+tp.p.ID+`","pid":4099}]}`)
	if !strings.Contains(w.Body.String(), `"reason":"pane_changed"`) || tp.p.recording.active.Load() != nil {
		t.Fatalf("workspace: %d %s", w.Code, w.Body.String())
	}
	if w := callRecording(t, a, recordingLocalPerson, http.MethodPost, "/recording", `{"on":true,"pid":4100}`, tp.p.ID); w.Code != http.StatusOK {
		t.Fatalf("same process: %d %s", w.Code, w.Body.String())
	}
	stopAndWait(t, tp, tp.p.recording.active.Load())
}

// Addendum C 1: a folder override binds to the roots named at switch-on.
// Moving into another root stops it with "folder"; launch-secrets inside
// ~/.config stays hard even with folder:config.
func TestRecordingFolderOverrideBindsToItsRoots(t *testing.T) {
	home, _ := os.UserHomeDir()
	deploy := filepath.Join(home, "wsl-projects/launch/deploy")
	tp := newRecTestPane(t)
	rec, err := startWith(t, tp, recordingPaneFacts{Cwds: []string{deploy}, AgentRunning: true}, recordingStartOptions{Overrides: []string{"folder:launch_deploy"}})
	if err != nil {
		t.Fatal(err)
	}
	moved := recordingPaneFacts{Cwds: []string{deploy, filepath.Join(home, ".config", "gh")}, AgentRunning: true}
	tp.facts.Store(&moved)
	waitRecorder(t, rec)
	if rec.reason != "folder" {
		t.Fatalf("reason %q", rec.reason)
	}
	if d := tp.p.Recording(); "recording stopped: "+d.StoppedReason != "recording stopped: folder" {
		t.Fatalf("notice %+v", d)
	}
	// The wrong root's override doesn't admit the pane.
	if _, err := startWith(t, tp, recordingPaneFacts{Cwds: []string{deploy}, AgentRunning: true}, recordingStartOptions{Overrides: []string{"folder:config"}}); err == nil {
		t.Fatal("a folder:config override admitted a launch/deploy pane")
	}
	secrets := filepath.Join(home, ".config", "launch-secrets", "x")
	var refused recordingRefusedError
	if _, err := startWith(t, tp, recordingPaneFacts{Cwds: []string{secrets}, AgentRunning: true}, recordingStartOptions{Overrides: []string{"folder:config"}}); !errors.As(err, &refused) || refused.rule != recordingRuleHardFolder {
		t.Fatalf("launch-secrets with folder:config: %v", err)
	}
}

func agentPutSettings(t *testing.T, a *apiServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	return callRecordingSettings(t, a, recordingNonPersonCallers[0], http.MethodPut, body)
}

// Addenda C 2 and D a: an agent's lowering applies to recordings made from
// then on; a file finished before keeps the age it had. The person's
// lowering applies at once.
func TestRecordingLoweredAgeKeepsOlderFilesUnlessThePersonLowers(t *testing.T) {
	tp := newRecTestPane(t)
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
	hub := tp.m.recordingHub()
	now := time.Now()
	old := writeRecordingFixture(t, hub, "pane_a", "20261117T000000Z.cast", 10, now.Add(-3*24*time.Hour))
	if w := agentPutSettings(t, a, `{"deleteAfterDays":1}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	hub.sweep()
	if !exists(old) {
		t.Fatal("an agent's lowering deleted a 3-day-old recording")
	}
	// A recording started and finished after the lowering keeps 1 day.
	later := writeRecordingFixture(t, hub, "pane_a", now.Add(time.Second).UTC().Format(recordingFileTimeLayout)+".cast", 10, now.Add(time.Second))
	hub.now = func() time.Time { return now.Add(2 * 24 * time.Hour) }
	hub.sweep()
	if exists(later) || !exists(old) {
		t.Fatalf("later=%v old=%v", exists(later), exists(old))
	}
	// The history survives a restart.
	if _, history := loadRecordingSettings(tp.data); len(history) != 1 || history[0].By != "agent" || history[0].DeleteAfterDays != 7 {
		t.Fatalf("history %+v", history)
	}
	// The person's lowering applies at once and collapses the history (a
	// raise to 2 days first, so there is something to lower).
	if err := hub.changeSettings(recordingSettings{DeleteAfterDays: 2, CapMB: 2000}, true, ""); err != nil {
		t.Fatal(err)
	}
	if !exists(old) {
		t.Fatal("a raise deleted a recording")
	}
	if err := hub.changeSettings(recordingSettings{DeleteAfterDays: 1, CapMB: 2000}, true, ""); err != nil {
		t.Fatal(err)
	}
	hub.mu.Lock()
	n := len(hub.history)
	hub.mu.Unlock()
	if n != 0 {
		t.Fatalf("history kept %d after the person's lowering", n)
	}
	if exists(old) {
		t.Fatal("the person's lowering didn't apply at once")
	}
}

// A raise (the person's) applies to older files too.
func TestRecordingRaisedAgeKeepsOlderFilesLonger(t *testing.T) {
	data := t.TempDir()
	hub := newRecordingHub(data)
	now := time.Now()
	hub.now = func() time.Time { return now }
	f := writeRecordingFixture(t, hub, "pane_a", "20261101T000000Z.cast", 10, now.Add(-6*24*time.Hour))
	if err := hub.changeSettings(recordingSettings{DeleteAfterDays: 3, CapMB: 2000}, false, "agent"); err != nil {
		t.Fatal(err)
	}
	if err := hub.changeSettings(recordingSettings{DeleteAfterDays: 10, CapMB: 2000}, true, ""); err != nil {
		t.Fatal(err)
	}
	if !exists(f) {
		t.Fatal("a 6-day-old file was deleted under its 7 days or a 10-day age")
	}
	hub.now = func() time.Time { return now.Add(3 * 24 * time.Hour) }
	hub.sweep()
	if !exists(f) {
		t.Fatal("a 9-day-old file was deleted after the person raised the age to 10")
	}
}

// Addendum D b: after an agent lowers the cap, older files are deleted for
// the cap only while the total is over the cap they had; a new recording that
// can't fit is refused and the notice names the lowering.
func TestRecordingLoweredCapProtectsOlderFiles(t *testing.T) {
	data := t.TempDir()
	hub := newRecordingHub(data)
	now := time.Now()
	sparse := func(name string, size int64, mtime time.Time) string {
		path := writeRecordingFixture(t, hub, "pane_a", name, 0, mtime)
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(path, mtime, mtime)
		return path
	}
	a := sparse("20261118T000000Z.cast", 600_000_000, now.Add(-2*time.Hour))
	b := sparse("20261119T000000Z.cast", 600_000_000, now.Add(-time.Hour))
	if err := hub.changeSettings(recordingSettings{DeleteAfterDays: 7, CapMB: 1000}, false, "agent"); err != nil {
		t.Fatal(err)
	}
	if !exists(a) || !exists(b) {
		t.Fatal("an agent's cap lowering deleted older recordings")
	}
	if hub.reserve(1000) {
		t.Fatal("a new write fit over the lowered cap")
	}
	if !exists(a) || !exists(b) {
		t.Fatal("making room deleted protected recordings")
	}
	if r := hub.capStopReason(); !strings.HasPrefix(r, "disk cap (lowered by an agent at ") {
		t.Fatalf("reason %q", r)
	}
	// Over the cap they had (2 GB), protected files go oldest first; a newer
	// file is held to the lowered cap.
	c := sparse(now.Add(time.Second).UTC().Format(recordingFileTimeLayout)+".cast", 900_000_000, now.Add(time.Second))
	hub.mu.Lock()
	hub.recountLocked()
	hub.mu.Unlock()
	hub.sweep()
	if exists(a) || !exists(b) || exists(c) {
		t.Fatalf("a=%v b=%v c=%v", exists(a), exists(b), exists(c))
	}
}

// The age history holds at most 16 entries.
func TestRecordingSettingsHistoryIsBounded(t *testing.T) {
	data := t.TempDir()
	hub := newRecordingHub(data)
	if err := hub.applySettings(recordingSettings{DeleteAfterDays: 30, CapMB: 2000}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 1; i <= 20; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		hub.now = func() time.Time { return at }
		writeRecordingFixture(t, hub, "pane_a", at.UTC().Format(recordingFileTimeLayout)+".cast", 1, at.Add(-time.Second))
		next := recordingSettings{DeleteAfterDays: 30 - i, CapMB: 2000}
		if err := hub.changeSettings(next, false, "agent"); err != nil {
			t.Fatal(err)
		}
	}
	if _, history := loadRecordingSettings(data); len(history) > recordingSettingsHistoryMax {
		t.Fatalf("history %d entries", len(history))
	}
}

// Item 3 of Addendum C: the daemon logs settings that differ from the
// defaults when it loads them.
func TestRecordingSettingsStartupLogLine(t *testing.T) {
	data := t.TempDir()
	_ = os.WriteFile(filepath.Join(data, recordingSettingsFile), []byte(`{"deleteAfterDays":3,"capMB":2000}`), 0o600)
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	newRecordingHub(data)
	if !strings.Contains(buf.String(), "recording: settings loaded: deleteAfterDays=3 capMB=2000") {
		t.Fatalf("log:\n%s", buf.String())
	}
}

// Addendum E 3: a recording in progress when an agent lowers the age keeps
// the longer age of those in force when it started and when it finished.
func TestRecordingInProgressKeepsTheLongerAgeAfterAnAgentLowers(t *testing.T) {
	data := t.TempDir()
	hub := newRecordingHub(data)
	now := time.Now()
	hub.now = func() time.Time { return now }
	started := now.Add(-2 * time.Hour)
	// The recording is on disk, being written, when the lowering lands; it
	// finishes an hour later.
	f := writeRecordingFixture(t, hub, "pane_a", started.UTC().Format(recordingFileTimeLayout)+".cast", 10, now.Add(-time.Minute))
	if err := hub.changeSettings(recordingSettings{DeleteAfterDays: 1, CapMB: 2000}, false, "agent"); err != nil {
		t.Fatal(err)
	}
	finished := now.Add(time.Hour)
	if err := os.Chtimes(f, finished, finished); err != nil {
		t.Fatal(err)
	}
	hub.now = func() time.Time { return now.Add(3 * 24 * time.Hour) }
	hub.sweep()
	if !exists(f) {
		t.Fatal("a recording started under 7 days lost them to an agent's lowering mid-recording")
	}
	hub.now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
	hub.sweep()
	if exists(f) {
		t.Fatal("kept past its 7 days")
	}
	// The person's lowering still applies at once.
	g := writeRecordingFixture(t, hub, "pane_a", started.Add(time.Second).UTC().Format(recordingFileTimeLayout)+".cast", 10, now.Add(time.Hour))
	if err := hub.changeSettings(recordingSettings{DeleteAfterDays: 2, CapMB: 2000}, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := hub.changeSettings(recordingSettings{DeleteAfterDays: 1, CapMB: 2000}, true, ""); err != nil {
		t.Fatal(err)
	}
	if exists(g) {
		t.Fatal("the person's lowering didn't apply at once")
	}
}

// Addendum E 1: the quiet test that decides a lowering's reach and the
// answering gate agree for every caller kind and transport.
func TestRecordingPersonChecksAgree(t *testing.T) {
	tp := newRecTestPane(t)
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
	type tc struct {
		name   string
		c      recCaller
		origin string
		remote string
		want   bool
	}
	cases := []tc{{name: recordingLocalPerson.name, c: recordingLocalPerson, want: true}}
	for _, c := range recordingNonPersonCallers {
		cases = append(cases, tc{name: c.name, c: c})
	}
	cases = append(cases,
		tc{name: "local person, other origin", c: recordingLocalPerson, origin: "http://evil.example"},
		tc{name: "local person, no origin", c: recordingLocalPerson, origin: "-"},
		tc{name: "local person, another computer", c: recordingLocalPerson, remote: "192.0.2.7:4444"},
		tc{name: "program", c: recCaller{"program", callerIdentity{Kind: callerUnverified, PID: 9}, false}},
	)
	for _, k := range cases {
		asRecordingCaller(t, k.c)
		r := recordingRequest(t, k.c, http.MethodPut, "/api/settings/recording", `{}`)
		switch k.origin {
		case "":
		case "-":
			r.Header.Del("Origin")
		default:
			r.Header.Set("Origin", k.origin)
		}
		if k.remote != "" {
			r.RemoteAddr = k.remote
		}
		quiet := a.recordingPersonQuiet(r)
		admitted := a.admitRecordingPerson(httptest.NewRecorder(), r, "test")
		if quiet != admitted || quiet != k.want {
			t.Errorf("%s: quiet %v, admit %v, want %v", k.name, quiet, admitted, k.want)
		}
	}
}

// Addendum E 3 across a roll-over: a part after the per-file cap is named by
// the recording's start, so it keeps the protection the first part has.
func TestRecordingPartsKeepTheRecordingStart(t *testing.T) {
	data := t.TempDir()
	hub := newRecordingHub(data)
	now := time.Now()
	hub.now = func() time.Time { return now }
	started := now.Add(-2 * time.Hour)
	base := started.UTC().Format(recordingFileTimeLayout)
	first := writeRecordingFixture(t, hub, "pane_a", base+".cast", 10, now.Add(-time.Minute))
	if err := hub.changeSettings(recordingSettings{DeleteAfterDays: 1, CapMB: 2000}, false, "agent"); err != nil {
		t.Fatal(err)
	}
	// The roll-over part, created after the lowering, is named by the start.
	f, path, err := hub.createPartFile(filepath.Dir(first), base, 2)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	hub.closed(path)
	if filepath.Base(path) != base+"_000002.cast" {
		t.Fatalf("part named %s", filepath.Base(path))
	}
	later := now.Add(time.Hour)
	_ = os.Chtimes(path, later, later)
	hub.now = func() time.Time { return now.Add(3 * 24 * time.Hour) }
	hub.sweep()
	if !exists(path) || !exists(first) {
		t.Fatalf("part=%v first=%v: a part lost the recording's 7 days", exists(path), exists(first))
	}
	if s, ok := recordingFileStart(path); !ok || !s.Equal(started.Truncate(time.Second).UTC()) {
		t.Fatalf("start %v %v", s, ok)
	}
}

// The person's change applies per field: lowering the cap leaves the age an
// agent's earlier lowering protects, and only the field the person lowered
// applies to older files at once.
func TestRecordingPersonChangeAppliesPerField(t *testing.T) {
	data := t.TempDir()
	hub := newRecordingHub(data)
	now := time.Now()
	hub.now = func() time.Time { return now }
	old := writeRecordingFixture(t, hub, "pane_a", now.Add(-3*24*time.Hour).UTC().Format(recordingFileTimeLayout)+".cast", 10, now.Add(-3*24*time.Hour))
	if err := hub.changeSettings(recordingSettings{DeleteAfterDays: 1, CapMB: 2000}, false, "agent"); err != nil {
		t.Fatal(err)
	}
	// The person lowers only the cap, then raises the age to 2 and lowers the
	// cap again: the 3-day-old file keeps its 7 days through both.
	for _, s := range []recordingSettings{{DeleteAfterDays: 1, CapMB: 1900}, {DeleteAfterDays: 2, CapMB: 1800}} {
		if err := hub.changeSettings(s, true, ""); err != nil {
			t.Fatal(err)
		}
		if !exists(old) {
			t.Fatalf("%+v: the person's change to another field deleted a protected recording", s)
		}
	}
	// The person lowers the age itself: it applies at once.
	if err := hub.changeSettings(recordingSettings{DeleteAfterDays: 1, CapMB: 1800}, true, ""); err != nil {
		t.Fatal(err)
	}
	if exists(old) {
		t.Fatal("the person's age lowering didn't apply at once")
	}
}

// The page sends the values it read; if they changed in between (a raise in
// another window), the daemon answers 409 and changes nothing.
func TestRecordingSettingsExpectedValues(t *testing.T) {
	tp := newRecTestPane(t)
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
	// Another window raises the age to 14 after this page read 7.
	if w := callRecordingSettings(t, a, recordingLocalPerson, http.MethodPut, `{"deleteAfterDays":14}`); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	// This page, having read 7, saves 10 as a raise: refused, nothing changes.
	w := callRecordingSettings(t, a, recordingLocalPerson, http.MethodPut, `{"deleteAfterDays":10,"expectDeleteAfterDays":7,"expectCapMB":2000}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale expected values: %d %s", w.Code, w.Body.String())
	}
	if got := tp.m.recordingHub().settings(); got.DeleteAfterDays != 14 {
		t.Fatalf("changed on a refused save: %+v", got)
	}
	// Re-read: with the stored values it applies.
	if w := callRecordingSettings(t, a, recordingLocalPerson, http.MethodPut, `{"deleteAfterDays":10,"expectDeleteAfterDays":14,"expectCapMB":2000}`); w.Code != http.StatusOK {
		t.Fatalf("current expected values: %d %s", w.Code, w.Body.String())
	}
	// A stale cap alone refuses too; strict decoding still holds.
	for body, code := range map[string]int{
		`{"capMB":1500,"expectCapMB":1999}`:                    http.StatusConflict,
		`{"capMB":1500,"expectCapMB":2000,"expectCapMB":2000}`: http.StatusBadRequest,
		`{"capMB":1500,"expectCapMb2":2000}`:                   http.StatusBadRequest,
		`{"expectDeleteAfterDays":10}`:                         http.StatusBadRequest,
	} {
		if w := callRecordingSettings(t, a, recordingLocalPerson, http.MethodPut, body); w.Code != code {
			t.Errorf("%s: %d, want %d (%s)", body, w.Code, code, w.Body.String())
		}
	}
}
