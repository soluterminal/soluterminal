package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func strPtr(s string) *string { return &s }

// feedbackCloudStub records the raw body of every report the daemon forwards.
func feedbackCloudStub(t *testing.T) (*httptest.Server, *[]byte) {
	t.Helper()
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"number":3}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &raw
}

type feedbackPreviewResp struct {
	Title    string                `json:"title"`
	Body     string                `json:"body"`
	Snapshot string                `json:"snapshot"`
	Logs     string                `json:"logs"`
	Context  feedbackSubmitContext `json:"context"`
}

func postFeedbackPreview(t *testing.T, a *apiServer, req feedbackReq) feedbackPreviewResp {
	t.Helper()
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	a.handleFeedbackPreview(w, httptest.NewRequest("POST", "/api/feedback/preview", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", w.Code, w.Body.String())
	}
	var out feedbackPreviewResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	return out
}

func feedbackTestServer(t *testing.T, cloud string) *apiServer {
	t.Helper()
	dataDir := t.TempDir()
	if err := SaveClaim(dataDir, &Claim{Token: "machine-token", MachineID: "machine-1", MachineName: "Dev Laptop"}); err != nil {
		t.Fatalf("save claim: %v", err)
	}
	mgr := NewManager(t.TempDir())
	p := &Pane{ID: "pane_1", buffer: NewRingBuffer(1 << 20)}
	_, _ = p.buffer.Write([]byte("first line\nkey sk-ant-api03-AbCdEf0123456789ZyXwVu\nprivate line\nlast line\n"))
	mgr.panes[p.ID] = p
	a := &apiServer{ver: "0.0.1241", backendID: "wsl-x", shellPlatform: "windows", dataDir: dataDir, mgr: mgr}
	if cloud != "" {
		a.cloudBase = cloud
		a.cloudToken = "tok"
	}
	return a
}

// Test 4: the preview is what the cloud receives, field for field, when the
// form sends the previewed text back unchanged.
func TestFeedbackPreviewEqualsSent(t *testing.T) {
	srv, raw := feedbackCloudStub(t)
	a := feedbackTestServer(t, srv.URL)

	prev := postFeedbackPreview(t, a, feedbackReq{
		Title: "", Body: "it broke\nmore", PaneID: "pane_1",
		IncludeSnapshot: boolPtr(true), IncludeLogs: boolPtr(false), ClientVersion: "0.0.1241",
	})
	if strings.Contains(prev.Snapshot, "sk-ant-api03") || !strings.Contains(prev.Snapshot, "last line") {
		t.Fatalf("preview snapshot not redacted or lost text: %q", prev.Snapshot)
	}
	if prev.Title != "it broke" {
		t.Fatalf("empty title should become the first line, got %q", prev.Title)
	}
	if prev.Context.MachineID != "machine-1" || prev.Context.PaneID != "pane_1" {
		t.Fatalf("preview details: %+v", prev.Context)
	}

	w := postDaemonFeedback(t, a, feedbackReq{
		Type: "bug", Title: "", Body: "it broke\nmore", PaneID: "pane_1",
		IncludeSnapshot: boolPtr(true), IncludeLogs: boolPtr(false),
		Snapshot: strPtr(prev.Snapshot), ClientVersion: "0.0.1241",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("submit status = %d, body=%s", w.Code, w.Body.String())
	}
	var got feedbackSubmission
	if err := json.Unmarshal(*raw, &got); err != nil {
		t.Fatalf("decode forwarded: %v", err)
	}
	if got.Snapshot != prev.Snapshot || got.Title != prev.Title || got.Body != prev.Body || got.Logs != "" {
		t.Fatalf("sent differs from preview:\nsent %+v\npreview %+v", got, prev)
	}
	sentCtx := got.Context
	sentCtx.SubmittedAt = ""
	if sj, pj := feedbackJSON(t, sentCtx), feedbackJSON(t, prev.Context); sj != pj {
		t.Fatalf("details differ:\nsent    %s\npreview %s", sj, pj)
	}
}

// Test 5: a line the user deleted from the preview is not sent, and the
// daemon doesn't capture the pane again behind the preview's back.
func TestFeedbackDeletedPreviewLineAbsent(t *testing.T) {
	srv, raw := feedbackCloudStub(t)
	a := feedbackTestServer(t, srv.URL)
	prev := postFeedbackPreview(t, a, feedbackReq{PaneID: "pane_1", IncludeSnapshot: boolPtr(true)})
	edited := strings.Replace(prev.Snapshot, "private line\n", "", 1)
	if edited == prev.Snapshot {
		t.Fatalf("fixture: line not in preview %q", prev.Snapshot)
	}
	w := postDaemonFeedback(t, a, feedbackReq{
		Type: "bug", Body: "b", PaneID: "pane_1",
		IncludeSnapshot: boolPtr(true), IncludeLogs: boolPtr(true),
		Snapshot: strPtr(edited), Logs: strPtr("only this log line"),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if bytes.Contains(*raw, []byte("private line")) {
		t.Fatalf("deleted line was sent: %s", *raw)
	}
	var got feedbackSubmission
	_ = json.Unmarshal(*raw, &got)
	if got.Logs != "only this log line" {
		t.Fatalf("logs = %q, want the previewed text", got.Logs)
	}
}

// Test 3: unticked rows send nothing, even when a client sends their text;
// pane text and logs are ticked separately.
func TestFeedbackUntickedRowsSendNothing(t *testing.T) {
	srv, raw := feedbackCloudStub(t)
	a := feedbackTestServer(t, srv.URL)
	w := postDaemonFeedback(t, a, feedbackReq{
		Type: "bug", Body: "b", PaneID: "pane_1",
		IncludeSnapshot: boolPtr(false), IncludeLogs: boolPtr(false),
		Snapshot: strPtr("pane secret text"), Logs: strPtr("log secret text"),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	for _, leak := range []string{"pane secret text", "log secret text", "first line", "pane_1"} {
		if bytes.Contains(*raw, []byte(leak)) {
			t.Fatalf("unticked content %q sent: %s", leak, *raw)
		}
	}

	w = postDaemonFeedback(t, a, feedbackReq{
		Type: "bug", Body: "b", PaneID: "pane_1",
		IncludeSnapshot: boolPtr(false), IncludeLogs: boolPtr(true), Logs: strPtr("a log line"),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got feedbackSubmission
	_ = json.Unmarshal(*raw, &got)
	if got.Snapshot != "" || got.Logs != "a log line" || got.Context.CaptureKind != feedbackCaptureLogsOnly {
		t.Fatalf("logs-only report: snapshot=%q logs=%q kind=%q", got.Snapshot, got.Logs, got.Context.CaptureKind)
	}
}

// Screenshots stay on the computer for every account until private storage
// exists, and the computer's name is never sent.
func TestFeedbackNoScreenshotsNoComputerName(t *testing.T) {
	srv, raw := feedbackCloudStub(t)
	a := feedbackTestServer(t, srv.URL)
	body := `{"type":"bug","title":"t","body":"b","includeSnapshot":false,` +
		`"attachments":[{"name":"s.png","mime":"image/png","data":"iVBORw0KGgo="}]}`
	w := httptest.NewRecorder()
	a.handleFeedback(w, httptest.NewRequest("POST", "/api/feedback", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	for _, leak := range []string{"attachments", "iVBORw0KGgo", "Dev Laptop", "machineName"} {
		if bytes.Contains(*raw, []byte(leak)) {
			t.Fatalf("%q forwarded: %s", leak, *raw)
		}
	}
}

// The preview needs no cloud connection: signed-out and unpaired computers
// build Copy report / Email it from it.
func TestFeedbackPreviewWorksUnpaired(t *testing.T) {
	a := feedbackTestServer(t, "")
	prev := postFeedbackPreview(t, a, feedbackReq{Body: "token=hunter2 here", ClientVersion: "0.0.1241"})
	if strings.Contains(prev.Body, "hunter2") {
		t.Fatalf("body not redacted: %q", prev.Body)
	}
	if prev.Snapshot != "" || prev.Logs != "" || prev.Context.PaneID != "" {
		t.Fatalf("unticked preview carries pane text or logs: %+v", prev)
	}
	if prev.Context.DaemonVersion != "0.0.1241" || prev.Context.ClientVersion != "0.0.1241" {
		t.Fatalf("versions missing: %+v", prev.Context)
	}
}

func TestFeedbackTextBoundsAfterRedaction(t *testing.T) {
	srv, raw := feedbackCloudStub(t)
	a := feedbackTestServer(t, srv.URL)
	for _, tc := range []struct {
		name, title, body, wantTitle, wantBody string
	}{
		{
			name:      "body quoted value",
			title:     "report",
			body:      `{"password":"` + strings.Repeat("fake value ", feedbackMaxBody/10) + `","safe":true}`,
			wantTitle: "report", wantBody: `{"password":"[redacted: token]","safe":true}`,
		},
		{
			name:      "title quoted value",
			title:     `{"password":"` + strings.Repeat("fake value ", feedbackMaxTitle/10) + `","safe":true}`,
			body:      "report",
			wantTitle: `{"password":"[redacted: token]","safe":true}`, wantBody: "report",
		},
		{
			name:      "derived title quoted value",
			body:      `{"password":"` + strings.Repeat("fake value ", feedbackMaxTitle/10) + `","safe":true}`,
			wantTitle: `{"password":"[redacted: token]","safe":true}`,
			wantBody:  `{"password":"[redacted: token]","safe":true}`,
		},
		{
			name:      "title copied from removed body context",
			title:     "fake+key/material+line",
			body:      "What I was trying to do: " + strings.Repeat("-", 5) + "BEGIN OPENSSH PRIVATE KEY" + strings.Repeat("-", 5) + "\nfake+key/material+line",
			wantTitle: "What I was trying to do: [redacted: private key]",
			wantBody:  "What I was trying to do: [redacted: private key]",
		},
		{
			name:  "safe text remains bounded",
			title: strings.Repeat("safe ", feedbackMaxTitle), body: strings.Repeat("safe ", feedbackMaxBody),
			wantTitle: strings.Repeat("safe ", feedbackMaxTitle/5),
			wantBody:  strings.Repeat("safe ", feedbackMaxBody/5) + "sa",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := feedbackReq{Type: "bug", Title: tc.title, Body: tc.body}
			prev := postFeedbackPreview(t, a, req)
			if prev.Title != tc.wantTitle || prev.Body != tc.wantBody {
				t.Fatalf("preview text differs: title=%q body=%q", prev.Title, prev.Body)
			}
			w := postDaemonFeedback(t, a, req)
			if w.Code != http.StatusOK {
				t.Fatalf("submit status = %d, body=%s", w.Code, w.Body.String())
			}
			var got feedbackSubmission
			if err := json.Unmarshal(*raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.Title != prev.Title || got.Body != prev.Body {
				t.Fatal("submitted text differs from preview")
			}
			if len(got.Title) > feedbackMaxTitle || len(got.Body) > feedbackMaxBody {
				t.Fatal("submitted text exceeds bounds")
			}
		})
	}
}

// The redaction is idempotent over the bounds, so a re-sent preview is
// unchanged even when it was cut to size.
func TestFeedbackBoundsIdempotent(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", feedbackMaxSnapshotBytes) + "\npassword=hunter2\n"
	once := feedbackBoundSnapshot(long)
	if feedbackBoundSnapshot(once) != once {
		t.Fatalf("snapshot bound not idempotent")
	}
	if len(once) > feedbackMaxSnapshotBytes || strings.Contains(once, "hunter2") {
		t.Fatalf("snapshot bound: len=%d", len(once))
	}
	wide := feedbackBoundSnapshot(strings.Repeat("é", feedbackMaxSnapshotBytes))
	if !utf8.ValidString(wide) || feedbackBoundSnapshot(wide) != wide {
		t.Fatalf("multi-byte cut split a character or isn't idempotent")
	}
	logs := feedbackBoundLogs(strings.Repeat("y", 2*crashRecentLogBytes) + " token=abc")
	if feedbackBoundLogs(logs) != logs || strings.Contains(logs, "abc") {
		t.Fatalf("log bound not idempotent or not redacted")
	}
}

// Replies are redacted on the device before they reach the cloud.
func TestFeedbackReplyRedacted(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	a := &apiServer{}
	a.cloudBase = srv.URL
	a.cloudToken = "tok"
	w := httptest.NewRecorder()
	a.handleFeedbackReply(w, httptest.NewRequest("POST", "/api/feedback/reply",
		strings.NewReader(`{"number":4,"text":"here: sk-ant-api03-AbCdEf0123456789ZyXwVu"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got["number"] != float64(4) || strings.Contains(got["text"].(string), "sk-ant-api03") {
		t.Fatalf("reply forwarded = %v", got)
	}
}

func feedbackJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The form says email addresses are removed: they are, in the preview and
// in what is sent, and a second pass changes nothing.
func TestFeedbackEmailsRedacted(t *testing.T) {
	srv, raw := feedbackCloudStub(t)
	a := feedbackTestServer(t, srv.URL)
	body := "mail alice@example.com or bob.smith+tag@mail.example.co.uk"
	prev := postFeedbackPreview(t, a, feedbackReq{Body: body})
	if strings.Contains(prev.Body, "@") || strings.Count(prev.Body, "[redacted: email]") != 2 {
		t.Fatalf("preview body = %q", prev.Body)
	}
	w := postDaemonFeedback(t, a, feedbackReq{
		Type: "bug", Body: body, IncludeSnapshot: boolPtr(false),
		IncludeLogs: boolPtr(true), Logs: strPtr("login as carol@example.org"),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	for _, leak := range []string{"alice@example.com", "bob.smith", "carol@example.org", "example.org"} {
		if bytes.Contains(*raw, []byte(leak)) {
			t.Fatalf("%q sent: %s", leak, *raw)
		}
	}
	long := strings.Repeat("a", 40) + "@example.com"
	once := redactFeedback(long)
	if once != "[redacted: email]" || redactFeedback(once) != once {
		t.Fatalf("long local part: %q", once)
	}
}

// ST-166: the follow-up link passes through to the cloud, which checks it; a
// new report carries no followUpOf at all.
func TestFeedbackFollowUpOfPassesThrough(t *testing.T) {
	srv, raw := feedbackCloudStub(t)
	a := feedbackTestServer(t, srv.URL)
	for _, tc := range []struct {
		followUpOf int64
		want       string
	}{{38, `"followUpOf":38`}, {0, ""}, {-4, ""}} {
		w := postDaemonFeedback(t, a, feedbackReq{Type: "bug", Body: "still broken", IncludeSnapshot: boolPtr(false), FollowUpOf: tc.followUpOf})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
		}
		got := string(*raw)
		if tc.want != "" && !strings.Contains(got, tc.want) {
			t.Fatalf("followUpOf %d not forwarded: %s", tc.followUpOf, got)
		}
		if tc.want == "" && strings.Contains(got, "followUpOf") {
			t.Fatalf("followUpOf %d forwarded: %s", tc.followUpOf, got)
		}
	}
}
