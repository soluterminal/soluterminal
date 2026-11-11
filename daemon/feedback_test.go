package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func boolPtr(b bool) *bool { return &b }

func postDaemonFeedback(t *testing.T, a *apiServer, req feedbackReq) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/api/feedback", bytes.NewReader(body))
	w := httptest.NewRecorder()
	a.handleFeedback(w, r)
	return w
}

func TestDaemonFeedbackValidation(t *testing.T) {
	t.Parallel()
	a := &apiServer{}
	if w := postDaemonFeedback(t, a, feedbackReq{Type: "rant", Title: "x", Body: "y", IncludeSnapshot: boolPtr(false)}); w.Code != http.StatusBadRequest {
		t.Fatalf("bad type: status = %d", w.Code)
	}
	if w := postDaemonFeedback(t, a, feedbackReq{Type: "bug", Title: "t", Body: "  ", IncludeSnapshot: boolPtr(false)}); w.Code != http.StatusBadRequest {
		t.Fatalf("empty body: status = %d", w.Code)
	}
}

func TestDaemonFeedbackNotPaired(t *testing.T) {
	t.Parallel()
	a := &apiServer{} // no cloud creds
	w := postDaemonFeedback(t, a, feedbackReq{Type: "bug", Title: "t", Body: "b", IncludeSnapshot: boolPtr(false)})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestDaemonFeedbackForwardsRedacted(t *testing.T) {
	var got feedbackSubmission
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
			t.Errorf("cloud auth = %q, want Bearer tok", auth)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"number":7,"url":"https://x/issues/7"}`)
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	if err := SaveClaim(dataDir, &Claim{Token: "machine-token", MachineID: "machine-1", MachineName: "Dev Laptop"}); err != nil {
		t.Fatalf("save claim: %v", err)
	}
	a := &apiServer{ver: "0.0.770", backendID: "wsl-x", shellVersion: "0.0.771", shellPlatform: "linux", dataDir: dataDir}
	a.cloudBase = srv.URL
	a.cloudToken = "tok"

	w := postDaemonFeedback(t, a, feedbackReq{
		Type:            "bug",
		Title:           "garble",
		Body:            "leak sk-ant-api03-AbCdEf0123456789ZyXwVu here",
		IncludeSnapshot: boolPtr(false),
		ClientVersion:   "0.0.770",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OK     bool   `json:"ok"`
		Number int    `json:"number"`
		URL    string `json:"url"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.OK || resp.Number != 7 {
		t.Fatalf("unexpected resp: %+v", resp)
	}

	// The forwarded submission must be normalized and ALREADY redacted — a
	// secret must never leave the daemon, even before the cloud's second pass.
	if got.Channel != "inapp" || got.Type != "bug" {
		t.Fatalf("forwarded channel/type wrong: %+v", got)
	}
	if got.Context.AppVersion != "" || got.Context.DaemonVersion != "0.0.770" || got.Context.OS == "" {
		t.Fatalf("context not gathered server-side: %+v", got.Context)
	}
	if got.Context.ShellVersion != "0.0.771" || got.Context.ShellPlatform != "linux" {
		t.Fatalf("shell context not gathered server-side: %+v", got.Context)
	}
	if got.Context.MachineID != "machine-1" {
		t.Fatalf("machine ID not gathered from claim: %+v", got.Context)
	}
	if got.Context.CaptureKind != feedbackCaptureNone {
		t.Fatalf("capture kind = %q, want %q", got.Context.CaptureKind, feedbackCaptureNone)
	}
	if got.Context.SnapshotRequested == nil || *got.Context.SnapshotRequested {
		t.Fatalf("snapshot requested = %v, want false", got.Context.SnapshotRequested)
	}
	if got.Context.LogsRequested == nil || *got.Context.LogsRequested {
		t.Fatalf("logs requested = %v, want false", got.Context.LogsRequested)
	}
	if strings.Contains(got.Body, "sk-ant-api03-AbCdEf0123456789ZyXwVu") {
		t.Fatalf("secret leaked off the daemon: %q", got.Body)
	}
	if !strings.Contains(got.Body, "[redacted: token]") {
		t.Fatalf("expected redaction in forwarded body: %q", got.Body)
	}
}

// A report the cloud queued (GitHub refused it) still succeeds for the user,
// and the web says it will be filed shortly.
func TestDaemonFeedbackRelaysQueued(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"ok":true,"queued":true}`)
	}))
	defer srv.Close()
	a := &apiServer{ver: "0.0.770"}
	a.cloudBase = srv.URL
	a.cloudToken = "tok"
	w := postDaemonFeedback(t, a, feedbackReq{Type: "bug", Title: "garble", Body: "something is wrong", IncludeSnapshot: boolPtr(false)})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"queued":true`) {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestDaemonFeedbackCaptureStatus(t *testing.T) {
	var got feedbackSubmission
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	mgr := NewManager(t.TempDir())
	p := &Pane{ID: "pane_1", buffer: NewRingBuffer(1 << 20)}
	_, _ = p.buffer.Write([]byte("visible terminal text\n"))
	mgr.panes[p.ID] = p
	a := &apiServer{mgr: mgr}
	a.cloudBase = srv.URL
	a.cloudToken = "tok"

	w := postDaemonFeedback(t, a, feedbackReq{
		Type:            "bug",
		Title:           "pane broke",
		Body:            "details",
		PaneID:          "pane_1",
		IncludeSnapshot: boolPtr(true),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if got.Context.CaptureKind != feedbackCapturePaneText {
		t.Fatalf("capture kind = %q, want %q", got.Context.CaptureKind, feedbackCapturePaneText)
	}
	if got.Context.SnapshotRequested == nil || !*got.Context.SnapshotRequested {
		t.Fatalf("snapshot requested = %v, want true", got.Context.SnapshotRequested)
	}
	if got.Context.SnapshotCaptured == nil || !*got.Context.SnapshotCaptured {
		t.Fatalf("snapshot captured = %v, want true", got.Context.SnapshotCaptured)
	}
	if !strings.Contains(got.Snapshot, "visible terminal text") {
		t.Fatalf("snapshot missing pane text: %q", got.Snapshot)
	}
}

func TestDaemonFeedbackCloudDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := &apiServer{}
	a.cloudBase = srv.URL
	a.cloudToken = "tok"
	w := postDaemonFeedback(t, a, feedbackReq{Type: "idea", Title: "t", Body: "b", IncludeSnapshot: boolPtr(false)})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestFeedbackPaneSnapshotRedactsAndStrips(t *testing.T) {
	t.Parallel()
	mgr := NewManager("")
	p := &Pane{ID: "pane_1", buffer: NewRingBuffer(1 << 20)}
	_, _ = p.buffer.Write([]byte("\x1b[31mfirst line\x1b[0m\ntoken sk-ant-api03-AbCdEf0123456789ZyXwVu\nlast line\n"))
	mgr.panes[p.ID] = p
	a := &apiServer{mgr: mgr}

	snap := a.feedbackPaneSnapshot("pane_1")
	if strings.Contains(snap, "\x1b[") {
		t.Fatalf("ANSI not stripped: %q", snap)
	}
	if strings.Contains(snap, "sk-ant-api03-AbCdEf0123456789ZyXwVu") {
		t.Fatalf("secret not redacted in snapshot: %q", snap)
	}
	if !strings.Contains(snap, "first line") || !strings.Contains(snap, "last line") {
		t.Fatalf("snapshot lost content: %q", snap)
	}

	// Missing pane yields empty, never an error.
	if got := a.feedbackPaneSnapshot("nope"); got != "" {
		t.Fatalf("missing pane snapshot = %q, want empty", got)
	}
}

func TestFeedbackTailLines(t *testing.T) {
	t.Parallel()
	in := "a\nb\nc\nd\ne\n\n\n"
	if got := feedbackTailLines(in, 2); got != "d\ne" {
		t.Fatalf("tail(2) = %q, want %q", got, "d\ne")
	}
	if got := feedbackTailLines(in, 100); got != "a\nb\nc\nd\ne" {
		t.Fatalf("tail(100) = %q", got)
	}
	if got := feedbackTailLines("", 5); got != "" {
		t.Fatalf("tail of empty = %q", got)
	}
}

func TestFeedbackClampRuneBoundary(t *testing.T) {
	t.Parallel()
	// "é" is 2 bytes (0xC3 0xA9). Clamping to 3 bytes must not split it.
	s := "aé" + "b" // bytes: a(1) é(2) b(1) = 4 bytes
	if got := feedbackClamp(s, 3); got != "aé" {
		t.Fatalf("clamp split a rune: got %q (% x)", got, got)
	}
	if got := feedbackClamp("short", 100); got != "short" {
		t.Fatalf("clamp shortened unnecessarily: %q", got)
	}
}

// A request that does not say whether to include the snapshot sends neither
// pane text nor logs.
func TestDaemonFeedbackSnapshotDefaultsOff(t *testing.T) {
	var got feedbackSubmission
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	mgr := NewManager(t.TempDir())
	p := &Pane{ID: "pane_1", buffer: NewRingBuffer(1 << 20)}
	_, _ = p.buffer.Write([]byte("private terminal text\n"))
	mgr.panes[p.ID] = p
	a := &apiServer{mgr: mgr}
	a.cloudBase = srv.URL
	a.cloudToken = "tok"

	for name, body := range map[string]string{
		"omitted": `{"type":"bug","title":"t","body":"b","paneId":"pane_1"}`,
		"null":    `{"type":"bug","title":"t","body":"b","paneId":"pane_1","includeSnapshot":null}`,
	} {
		got = feedbackSubmission{}
		w := httptest.NewRecorder()
		a.handleFeedback(w, httptest.NewRequest("POST", "/api/feedback", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body=%s", name, w.Code, w.Body.String())
		}
		if got.Snapshot != "" || got.Logs != "" {
			t.Fatalf("%s: sent pane text or logs by default: snapshot=%q logs=%q", name, got.Snapshot, got.Logs)
		}
		if got.Context.CaptureKind != feedbackCaptureNone {
			t.Fatalf("%s: capture kind = %q, want %q", name, got.Context.CaptureKind, feedbackCaptureNone)
		}
		if got.Context.SnapshotRequested == nil || *got.Context.SnapshotRequested ||
			got.Context.LogsRequested == nil || *got.Context.LogsRequested {
			t.Fatalf("%s: requested flags = %v / %v, want false", name, got.Context.SnapshotRequested, got.Context.LogsRequested)
		}
	}
}

func TestDaemonFeedbackSharedRulesBeforeTransport(t *testing.T) {
	var got feedbackSubmission
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"ok":true,"number":7}`)
	}))
	defer srv.Close()
	feedbackPairingCodes.remember("WARMFK", time.Now().Add(time.Minute))
	a := &apiServer{cloudBase: srv.URL, cloudToken: "fake-machine-token"}
	w := postDaemonFeedback(t, a, feedbackReq{Type: "bug", Title: "Contact fake@example.invalid", Body: "I saw WARMFK and password=fake;tail", IncludeSnapshot: boolPtr(false)})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if got.Title != "Contact [redacted: email]" || got.Body != "I saw [redacted: code] and password=[redacted: token]" {
		t.Fatal("shared privacy rules must run before transport")
	}
}

func TestDaemonFeedbackReplySharedRulesBeforeTransport(t *testing.T) {
	var got struct {
		Number int64  `json:"number"`
		Text   string `json:"text"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	feedbackPairingCodes.remember("FARMWK", time.Now().Add(time.Minute))
	a := &apiServer{cloudBase: srv.URL, cloudToken: "fake-machine-token"}
	payload := `{"number":7,"text":"I saw FARMWK and password=fake&tail"}`
	r := httptest.NewRequest("POST", "/api/feedback/reply", strings.NewReader(payload))
	w := httptest.NewRecorder()
	a.handleFeedbackReply(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if got.Number != 7 || got.Text != "I saw [redacted: code] and password=[redacted: token]" {
		t.Fatal("reply must be redacted on the device before transport")
	}
}

// A refusal the cloud wrote for the person reaches the web client as it is: a
// reply to a closed report is a 409 with the sentence, not a 502.
func TestDaemonFeedbackReplyPassesTheCloudRefusalThrough(t *testing.T) {
	const closed = "This report is closed, so it can't take replies. Send a follow-up instead."
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantBody   string
	}{
		{"closed report", http.StatusConflict, closed, http.StatusConflict, closed},
		{"someone else's report", http.StatusForbidden, "not your report", http.StatusForbidden, "not your report"},
		{"service trouble", http.StatusBadGateway, "could not read the report", http.StatusBadGateway, "could not save the reply right now"},
		{"this computer's credential", http.StatusUnauthorized, "unauthenticated", http.StatusBadGateway, "could not save the reply right now"},
		{"a proxy's page", http.StatusConflict, "<html><body>409</body></html>", http.StatusBadGateway, "could not save the reply right now"},
		{"no sentence", http.StatusConflict, "", http.StatusBadGateway, "could not save the reply right now"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tc.body, tc.status)
			}))
			defer srv.Close()
			a := &apiServer{cloudBase: srv.URL, cloudToken: "tok"}
			w := httptest.NewRecorder()
			a.handleFeedbackReply(w, httptest.NewRequest("POST", "/api/feedback/reply", strings.NewReader(`{"number":7,"text":"more"}`)))
			if got := strings.TrimSpace(w.Body.String()); w.Code != tc.wantStatus || got != tc.wantBody {
				t.Fatalf("answer = %d %q, want %d %q", w.Code, got, tc.wantStatus, tc.wantBody)
			}
		})
	}
}

func TestDaemonFeedbackThreadPassesTheCloudRefusalThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not your report", http.StatusForbidden)
	}))
	defer srv.Close()
	a := &apiServer{cloudBase: srv.URL, cloudToken: "tok"}
	w := httptest.NewRecorder()
	a.handleFeedbackThread(w, httptest.NewRequest("GET", "/api/feedback/thread?number=7", nil))
	if got := strings.TrimSpace(w.Body.String()); w.Code != http.StatusForbidden || got != "not your report" {
		t.Fatalf("answer = %d %q, want the cloud's 403", w.Code, got)
	}
}
