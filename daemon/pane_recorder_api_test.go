package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recCaller struct {
	name  string
	id    callerIdentity
	relay bool
}

// Every caller that is not the local person: agents (with or without Agent
// permissions, which this switch never reads), workers, untraced programs and
// the person over the relay (phone, cloud).
var recordingNonPersonCallers = []recCaller{
	{"pane agent", callerIdentity{Kind: callerPane, PaneID: "pane_agent", PID: 4242}, false},
	{"worker", callerIdentity{Kind: callerWorker, PaneID: "pane_worker", PID: 4243}, false},
	{"unverified program", callerIdentity{Kind: callerUnverified, PID: 4244}, false},
	{"unproven person refused under require", callerIdentity{Kind: callerUnverified, Unproven: true}, false},
	{"person on browser headers alone (off/report)", callerIdentity{Kind: callerHuman, Unproven: true}, false},
	{"person over the relay", callerIdentity{Kind: callerHuman, Relay: true}, true},
	{"pane agent over the relay", callerIdentity{Kind: callerPane, PaneID: "pane_agent"}, true},
}

var recordingLocalPerson = recCaller{"local person", callerIdentity{Kind: callerHuman, PID: 77, Device: "dev"}, false}

func recordingRequest(t *testing.T, c recCaller, method, path, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1:9791"
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://127.0.0.1:9791") // the app's own page
	if c.relay {
		r.Header.Set("X-Forwarded-Prefix", "/m/machine")
	}
	return r
}

func asRecordingCaller(t *testing.T, c recCaller) {
	t.Helper()
	prev := identifyCallerHook
	identifyCallerHook = func(*http.Request) callerIdentity { return c.id }
	t.Cleanup(func() { identifyCallerHook = prev })
}

func callRecording(t *testing.T, a *apiServer, c recCaller, method, route, body string, paneID string) *httptest.ResponseRecorder {
	t.Helper()
	asRecordingCaller(t, c)
	r := recordingRequest(t, c, method, "/api/panes/"+paneID+route, body)
	r.SetPathValue("id", paneID)
	w := httptest.NewRecorder()
	switch {
	case method == http.MethodGet:
		a.paneRecordingStatus(w, r)
	case method == http.MethodDelete:
		a.deletePaneRecordings(w, r)
	default:
		a.setPaneRecording(w, r)
	}
	return w
}

func TestRecordingSwitchOnIsTheLocalPersonsOnly(t *testing.T) {
	tp := newRecTestPane(t)
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
	for _, c := range recordingNonPersonCallers {
		w := callRecording(t, a, c, http.MethodPost, "/recording", `{"on":true}`, tp.p.ID)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: switch-on got %d %s", c.name, w.Code, w.Body.String())
		}
		if tp.p.recording.active.Load() != nil {
			t.Fatalf("%s switched recording on", c.name)
		}
		// Dismissing a stop notice and deleting are the person's too.
		if w := callRecording(t, a, c, http.MethodPost, "/recording", `{"dismiss":true}`, tp.p.ID); w.Code != http.StatusForbidden {
			t.Errorf("%s: dismiss got %d", c.name, w.Code)
		}
		if w := callRecording(t, a, c, http.MethodDelete, "s", ``, tp.p.ID); w.Code != http.StatusForbidden {
			t.Errorf("%s: delete got %d", c.name, w.Code)
		}
	}
	if exists(filepath.Join(tp.data, recordingsDirName)) {
		t.Fatal("a refused caller created the folder")
	}
	w := callRecording(t, a, recordingLocalPerson, http.MethodPost, "/recording", `{"on":true}`, tp.p.ID)
	if w.Code != http.StatusOK || tp.p.recording.active.Load() == nil {
		t.Fatalf("local person: %d %s", w.Code, w.Body.String())
	}
	var st paneRecordingDTO
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if !st.On || st.StartedAt == 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestRecordingSwitchOffIsAnyones(t *testing.T) {
	for _, c := range append(append([]recCaller{}, recordingNonPersonCallers...), recordingLocalPerson) {
		t.Run(c.name, func(t *testing.T) {
			tp := newRecTestPane(t)
			a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
			rec := tp.start()
			w := callRecording(t, a, c, http.MethodPost, "/recording", `{"on":false}`, tp.p.ID)
			if w.Code != http.StatusOK {
				t.Fatalf("switch-off: %d %s", w.Code, w.Body.String())
			}
			waitRecorder(t, rec)
			if rec.reason != recordingStopOff || tp.p.recording.active.Load() != nil {
				t.Fatalf("reason %q", rec.reason)
			}
			// Agents may read the status.
			if w := callRecording(t, a, c, http.MethodGet, "/recording", ``, tp.p.ID); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"on":false`) {
				t.Fatalf("status: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRecordingStateReachesThePaneListAndEveryClientsEvents(t *testing.T) {
	tp := newRecTestPane(t)
	tp.m.bus = newUIBus()
	first, second := tp.m.bus.subscribe(), tp.m.bus.subscribe()
	defer tp.m.bus.unsubscribe(first)
	defer tp.m.bus.unsubscribe(second)
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
	listed := func() *paneRecordingDTO {
		w := httptest.NewRecorder()
		a.listPanes(w, httptest.NewRequest(http.MethodGet, "/api/panes", nil))
		var panes []struct {
			ID        string            `json:"id"`
			Recording *paneRecordingDTO `json:"recording"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &panes); err != nil {
			t.Fatalf("pane list: %v %s", err, w.Body.String())
		}
		for _, p := range panes {
			if p.ID == tp.p.ID {
				return p.Recording
			}
		}
		t.Fatal("pane missing from the list")
		return nil
	}
	nextRecordingEvent := func(ch chan uiEvent) *paneRecordingDTO {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case e := <-ch:
				if e.Type == "pane-recording" && e.PaneID == tp.p.ID {
					return e.Recording
				}
			case <-deadline:
				t.Fatal("no pane-recording event")
				return nil
			}
		}
	}
	if d := listed(); d != nil {
		t.Fatalf("listed before recording: %+v", d)
	}
	rec := tp.start()
	if d := listed(); d == nil || !d.On {
		t.Fatalf("list while on: %+v", d)
	}
	for _, ch := range []chan uiEvent{first, second} {
		if d := nextRecordingEvent(ch); d == nil || !d.On {
			t.Fatalf("event while on: %+v", d)
		}
	}
	tp.m.StopRecording(tp.p, "local human")
	waitRecorder(t, rec)
	if d := listed(); d != nil && d.On {
		t.Fatalf("list after off: %+v", d)
	}
	for _, ch := range []chan uiEvent{first, second} {
		if d := nextRecordingEvent(ch); d == nil || d.On {
			t.Fatalf("event after off: %+v", d)
		}
	}
}

func TestRecordingLogLinesNameThePaneAndTransportButNoContent(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	tp := newRecTestPane(t)
	a := &apiServer{mgr: tp.m, dataDir: tp.data, aria: &Aria{}}
	if w := callRecording(t, a, recordingLocalPerson, http.MethodPost, "/recording", `{"on":true}`, tp.p.ID); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	rec := tp.p.recording.active.Load()
	tp.send(FrameData, []byte("SECRET-LOOKING-OUTPUT"))
	tp.sync()
	callRecording(t, a, recordingNonPersonCallers[0], http.MethodPost, "/recording", `{"on":false}`, tp.p.ID)
	waitRecorder(t, rec)
	out := buf.String()
	if strings.Contains(out, "SECRET-LOOKING-OUTPUT") {
		t.Fatal("recorded content reached the daemon log")
	}
	for _, want := range []string{"recording: pane " + tp.p.ID + " on by the person (local)", "recording: pane " + tp.p.ID + " switch-off by local pane", "recording: pane " + tp.p.ID + " off: " + recordingStopOff} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// Every generic file surface refuses recordings/, locally and over the relay,
// through a symlink elsewhere and through "..", with a data dir not named
// .solu (so the ".solu" name check can't be what refuses it).
func TestRecordingsRefusedOnEveryFileSurface(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	data := filepath.Join(home, "soludata")
	pane := filepath.Join(data, recordingsDirName, "pane_fixture")
	if err := os.MkdirAll(pane, 0o700); err != nil {
		t.Fatal(err)
	}
	cast := filepath.Join(pane, "20261119T000000Z.cast")
	if err := os.WriteFile(cast, []byte(`{"version":2,"width":80,"height":24,"timestamp":1}`+"\n"+`[0.1, "o", "fixture-recording-content"]`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// ST-317: the session timelines sit under the same folder and the same
	// refusals.
	sessions := filepath.Join(data, recordingsDirName, recordingSessionsDirName)
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	timeline := filepath.Join(sessions, "20261119T000000Z.jsonl")
	if err := os.WriteFile(timeline, []byte(`{"t":0,"ev":"header","app":"fixture-recording-content"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(home, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(project, "rec-alias")
	if err := os.Symlink(filepath.Join(data, recordingsDirName), link); err != nil {
		t.Fatal(err)
	}
	a := &apiServer{dataDir: data, aria: &Aria{dataDir: data}}
	mux := http.NewServeMux()
	a.routes(mux)
	authed := (&authGate{token: "owner"}).middleware(mux)
	paths := []string{
		cast,
		filepath.Join(link, "pane_fixture", "20261119T000000Z.cast"),
		filepath.Join(project, "..", "soludata", recordingsDirName, "pane_fixture", "20261119T000000Z.cast"),
		timeline,
		filepath.Join(link, recordingSessionsDirName, "20261119T000000Z.jsonl"),
	}
	dirs := []string{filepath.Join(data, recordingsDirName), pane, filepath.Join(link, "pane_fixture"), sessions}
	send := func(caller, method, target, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Host = "127.0.0.1:9791"
		r.RemoteAddr = "127.0.0.1:1234"
		if caller == "relay" {
			r.Header.Set("Authorization", "Bearer owner")
			r.Header.Set("X-Forwarded-Prefix", "/m/machine")
			r.RemoteAddr = "192.0.2.2:1234"
		}
		w := httptest.NewRecorder()
		asRecordingCaller(t, recordingLocalPerson)
		authed.ServeHTTP(w, r)
		return w
	}
	leaked := func(w *httptest.ResponseRecorder) bool {
		return strings.Contains(w.Body.String(), "fixture-recording-content") || strings.Contains(w.Body.String(), "20261119T000000Z")
	}
	for _, path := range paths {
		if credentialPathRefusal(path, data) == "" {
			t.Errorf("credentialPathRefusal allows %s", path)
		}
		for _, caller := range []string{"loopback", "relay"} {
			q := url.QueryEscape(path)
			if w := send(caller, http.MethodGet, "/api/file?path="+q, ""); w.Code < 400 || leaked(w) {
				t.Errorf("%s GET /api/file %s: %d %s", caller, path, w.Code, w.Body.String())
			}
			body := `{"path":` + strconvQuote(path) + `,"lineNo":0,"newText":"forged","expectedMtime":0}`
			if w := send(caller, http.MethodPost, "/api/file/line", body); w.Code < 400 {
				t.Errorf("%s POST /api/file/line %s: %d %s", caller, path, w.Code, w.Body.String())
			}
			if w := send(caller, http.MethodGet, "/api/git/diff?path="+q, ""); w.Code < 400 || leaked(w) {
				t.Errorf("%s GET /api/git/diff %s: %d %s", caller, path, w.Code, w.Body.String())
			}
		}
		if got := a.mcpToolPreviewFile(map[string]any{"path": path}).(map[string]any); !strings.Contains(got["content"].([]any)[0].(map[string]any)["text"].(string), "refusing") {
			t.Errorf("MCP preview_file allowed %s", path)
		}
		if got := a.aria.toolReadFile(path, 0, 0); !strings.HasPrefix(got, "error:") {
			t.Errorf("MCP read_file allowed %s: %s", path, got)
		}
	}
	for _, dir := range dirs {
		for _, caller := range []string{"loopback", "relay"} {
			q := url.QueryEscape(dir)
			if w := send(caller, http.MethodGet, "/api/dir/list?path="+q, ""); w.Code < 400 || leaked(w) {
				t.Errorf("%s GET /api/dir/list %s: %d %s", caller, dir, w.Code, w.Body.String())
			}
			if w := send(caller, http.MethodGet, "/api/files/search?root="+q+"&q=cast", ""); w.Code < 400 || leaked(w) {
				t.Errorf("%s GET /api/files/search %s: %d %s", caller, dir, w.Code, w.Body.String())
			}
		}
		if got := a.aria.toolSearchFiles(dir, "cast", "files", 10); !strings.HasPrefix(got, "error:") {
			t.Errorf("MCP search_files allowed %s: %s", dir, got)
		}
		if got := a.aria.toolGitStatus(dir, false, 1); !strings.HasPrefix(got, "error:") {
			t.Errorf("MCP git_status allowed %s: %s", dir, got)
		}
	}
	// Searching from a parent never lists a recording.
	for _, caller := range []string{"loopback", "relay"} {
		if w := send(caller, http.MethodGet, "/api/files/search?root="+url.QueryEscape(data)+"&q=.cast", ""); leaked(w) {
			t.Errorf("%s search from the data dir lists recordings: %s", caller, w.Body.String())
		}
		if w := send(caller, http.MethodGet, "/api/dir/list?path="+url.QueryEscape(filepath.Join(data, recordingsDirName, "..")), ""); leaked(w) {
			t.Errorf("%s listing the data dir shows recordings: %s", caller, w.Body.String())
		}
	}
	if raw, _ := os.ReadFile(cast); bytes.Contains(raw, []byte("forged")) {
		t.Fatal("a file surface wrote into a recording")
	}
	if raw, _ := os.ReadFile(timeline); bytes.Contains(raw, []byte("forged")) {
		t.Fatal("a file surface wrote into a timeline")
	}
}
