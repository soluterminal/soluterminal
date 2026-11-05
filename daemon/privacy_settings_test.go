package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func withActivePrivacy(t *testing.T, p *privacySettings) {
	t.Helper()
	prev := activePrivacy.Load()
	activePrivacy.Store(p)
	t.Cleanup(func() { activePrivacy.Store(prev) })
}

func TestPrivacyUsageUploadDefaultsByTier(t *testing.T) {
	t.Setenv(usageUploadDisabledEnv, "")
	team := false
	p := newPrivacySettings(t.TempDir(), func() bool { return team })
	if p.usageUploadAllowed() {
		t.Fatal("public install must not upload usage by default")
	}
	team = true
	if !p.usageUploadAllowed() {
		t.Fatal("team install keeps uploading usage by default")
	}
	off := false
	if err := p.update(&off, nil); err != nil {
		t.Fatal(err)
	}
	if p.usageUploadAllowed() {
		t.Fatal("a team member's explicit off must win")
	}
}

func TestPrivacyChoicesPersistAndEnvForcesOff(t *testing.T) {
	dir := t.TempDir()
	p := newPrivacySettings(dir, func() bool { return false })
	on, off := true, false
	if err := p.update(&on, &off); err != nil {
		t.Fatal(err)
	}
	reloaded := newPrivacySettings(dir, func() bool { return false })
	if !reloaded.usageUploadAllowed() || reloaded.crashReportsAllowed() {
		t.Fatalf("choices not persisted: usage=%v crash=%v", reloaded.usageUploadAllowed(), reloaded.crashReportsAllowed())
	}
	t.Setenv(usageUploadDisabledEnv, "1")
	if reloaded.usageUploadAllowed() {
		t.Fatal("SOLU_DISABLE_USAGE_UPLOAD must force usage off")
	}
	fresh := newPrivacySettings(t.TempDir(), nil)
	if !fresh.crashReportsAllowed() {
		t.Fatal("crash reports default on")
	}
	t.Setenv(crashReportsDisabledEnv, "1")
	if fresh.crashReportsAllowed() {
		t.Fatal("SOLU_DISABLE_CRASH_REPORTS must force crash reports off")
	}
}

func TestPrivacySettingsAPI(t *testing.T) {
	a := &apiServer{privacy: newPrivacySettings(t.TempDir(), func() bool { return false })}

	rec := httptest.NewRecorder()
	a.handleGetPrivacySettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings/privacy", nil))
	var got privacySettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.UsageUpload || !got.CrashReports {
		t.Fatalf("defaults: %+v", got)
	}

	put := func(body string, prefix string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/settings/privacy", strings.NewReader(body))
		if prefix != "" {
			req.Header.Set("X-Forwarded-Prefix", prefix)
		}
		w := httptest.NewRecorder()
		a.handlePutPrivacySettings(w, req)
		return w
	}
	if w := put(`{"usageUpload":true}`, "/m/abc"); w.Code != http.StatusForbidden {
		t.Fatalf("relay PUT must be refused, got %d", w.Code)
	}
	if w := put(`{}`, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("empty PUT must be rejected, got %d", w.Code)
	}
	w := put(`{"usageUpload":true,"crashReports":false}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.UsageUpload || got.CrashReports {
		t.Fatalf("after PUT: %+v", got)
	}
}

// A public install with usage statistics off reads past new turns without
// sending them, so a later opt-in never uploads the backlog.
func TestCCUsageScanSkipsUploadWhenUsageOff(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	projects := filepath.Join(home, ".claude", "projects", "-tmp-demo")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	turn := func(n string) string {
		return `{"type":"assistant","timestamp":"` + time.Now().UTC().Format(time.RFC3339) +
			`","sessionId":"s1","cwd":"/tmp/demo","requestId":"req_` + n + `","message":{"id":"msg_` + n +
			`","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	}
	transcript := filepath.Join(projects, "s1.jsonl")
	if err := os.WriteFile(transcript, []byte(turn("1")), 0o600); err != nil {
		t.Fatal(err)
	}

	withActivePrivacy(t, newPrivacySettings(t.TempDir(), func() bool { return false }))
	dataDir := t.TempDir()
	cursors := newCursorStore(dataDir)
	ledger := newInAppLedger(dataDir)
	client := &http.Client{Timeout: 5 * time.Second}

	if usageUploadAllowedNow() {
		t.Fatal("precondition: usage off")
	}
	// What the scanner's run() does when usage is off.
	scanOnce(t.Context(), cursors, ledger, client, "", "", nil)
	if posts.Load() != 0 {
		t.Fatalf("usage off must not POST, got %d", posts.Load())
	}
	// Opting in afterwards sends nothing from before.
	scanOnce(t.Context(), cursors, ledger, client, srv.URL, "tok", nil)
	if posts.Load() != 0 {
		t.Fatalf("backlog must not be sent after opting in, got %d posts", posts.Load())
	}
	// A turn after opting in is sent.
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(turn("2"))
	_ = f.Close()
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(transcript, future, future)
	scanOnce(t.Context(), cursors, ledger, client, srv.URL, "tok", nil)
	if posts.Load() != 1 {
		t.Fatalf("new turn after opting in must be sent once, got %d posts", posts.Load())
	}
}

func TestCrashReportRespectsPrivacy(t *testing.T) {
	var bodies []crashReportPayload
	done := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p crashReportPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		bodies = append(bodies, p)
		w.WriteHeader(http.StatusNoContent)
		done <- struct{}{}
	}))
	defer srv.Close()

	team := false
	p := newPrivacySettings(t.TempDir(), func() bool { return team })
	withActivePrivacy(t, p)
	sink := &crashSink{lastSent: map[string]time.Time{}}
	sink.setCreds(srv.URL, "tok")

	off := false
	if err := p.update(nil, &off); err != nil {
		t.Fatal(err)
	}
	sink.report("site-off", "error", "boom", "")
	select {
	case <-done:
		t.Fatal("crash report sent while switched off")
	case <-time.After(150 * time.Millisecond):
	}

	on := true
	if err := p.update(nil, &on); err != nil {
		t.Fatal(err)
	}
	prevRing := globalLogRing
	globalLogRing = newLogRing(logRingSize)
	t.Cleanup(func() { globalLogRing = prevRing })
	globalLogRing.add("pane title secret-client-project")
	sink.report("site-on", "error", "open ~/clients/acme/plan.md: denied", "")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("crash report not sent")
	}
	got := bodies[0]
	if got.RecentLogs != "" {
		t.Fatalf("public install must not attach the log tail, got %q", got.RecentLogs)
	}
	if strings.Contains(got.Message, "acme") || !strings.Contains(got.Message, "[path]") {
		t.Fatalf("user path not scrubbed: %q", got.Message)
	}
}

func TestScrubUserPathsKeepsAppPaths(t *testing.T) {
	in := "goroutine 1 [running]:\nsoluterminal/grid.(*Grid).Resize(...)\n\tsoluterminal/grid/grid.go:42 +0x1a\n" +
		"open ~/projects/acme/secret.txt: no such file; cfg ~/.solu/settings.json; route /api/settings/privacy; win C:\\Users\\bob\\work\\x.txt"
	out := scrubUserPaths(in)
	for _, keep := range []string{"soluterminal/grid/grid.go:42", "soluterminal/grid.(*Grid).Resize", "~/.solu/settings.json", "/api/settings/privacy"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("lost %q in %q", keep, out)
		}
	}
	for _, gone := range []string{"acme", "bob"} {
		if strings.Contains(out, gone) {
			t.Fatalf("leaked %q in %q", gone, out)
		}
	}
}

func TestScrubUserPathsRedactsGoPaths(t *testing.T) {
	for _, path := range []string{
		"/home/x/client/secret.go",
		"/Users/x/client/secret.go",
		`C:\Users\x\client\secret.go`,
		"~/client/secret.go",
		"/build/src/daemon/grid/grid.go",
	} {
		t.Run(path, func(t *testing.T) {
			in := "open " + path + ": denied\n\t" + path + ":42 +0x1a\n"
			want := "open [path]: denied\n\t[path]:42 +0x1a\n"
			if got := scrubUserPaths(in); got != want {
				t.Fatalf("scrubUserPaths() = %q, want %q", got, want)
			}
		})
	}
}

func TestCrashReportScrubsGoPaths(t *testing.T) {
	t.Setenv(crashReportsDisabledEnv, "")
	for _, team := range []bool{false, true} {
		name := "public"
		if team {
			name = "team"
		}
		t.Run(name, func(t *testing.T) {
			withActivePrivacy(t, newPrivacySettings(t.TempDir(), func() bool { return team }))
			prevRing := globalLogRing
			globalLogRing = newLogRing(logRingSize)
			t.Cleanup(func() { globalLogRing = prevRing })
			globalLogRing.add("open /home/x/client/secret.go: denied")
			reports := make(chan crashReportPayload, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var p crashReportPayload
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				reports <- p
			}))
			defer srv.Close()
			sink := &crashSink{lastSent: map[string]time.Time{}}
			sink.setCreds(srv.URL, "tok")
			sink.report("go-path", "panic", "open /home/x/client/secret.go: denied",
				"\t/home/x/client/secret.go:42 +0x1a\n\tsoluterminal/grid/grid.go:7 +0x2\n")
			select {
			case got := <-reports:
				if got.Message != "open [path]: denied" || got.Stack != "\t[path]:42 +0x1a\n\tsoluterminal/grid/grid.go:7 +0x2\n" {
					t.Fatalf("unexpected message/stack: %q / %q", got.Message, got.Stack)
				}
				wantLogs := ""
				if team {
					wantLogs = "open [path]: denied"
				}
				if got.RecentLogs != wantLogs {
					t.Fatalf("recent logs = %q, want %q", got.RecentLogs, wantLogs)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("crash report not sent")
			}
		})
	}
}

func TestGoTracebackLinesDropsOtherLogLines(t *testing.T) {
	tail := "2026/10/21 pane 3 title: client-acme deploy\n" +
		"panic: runtime error: index out of range\n\ngoroutine 7 [running]:\nmain.(*x).y(...)\n\tsoluterminal/x.go:10 +0x1\n\n" +
		"2026/10/21 typed: rm -rf build\n"
	out := goTracebackLines(tail)
	if strings.Contains(out, "acme") || strings.Contains(out, "rm -rf") {
		t.Fatalf("kept non-traceback lines: %q", out)
	}
	if !strings.Contains(out, "panic: runtime error") || !strings.Contains(out, "soluterminal/x.go:10") {
		t.Fatalf("lost traceback: %q", out)
	}
}

func TestFeedbackStatusExplainsWhyUnavailable(t *testing.T) {
	a := &apiServer{entitlements: newTeamEntitlements(t.TempDir(), func() (string, string) { return "", "" })}
	rec := httptest.NewRecorder()
	a.handleFeedbackStatus(rec, httptest.NewRequest(http.MethodGet, "/api/feedback/status", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["available"] != false || got["reason"] != "signed_out" {
		t.Fatalf("no account: %v", got)
	}
	a.entitlements.store(entitlementRecord{Tier: tierAccount, Source: "account", CheckedAt: time.Now()})
	rec = httptest.NewRecorder()
	a.handleFeedbackStatus(rec, httptest.NewRequest(http.MethodGet, "/api/feedback/status", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["available"] != false || got["reason"] != "unpaired" {
		t.Fatalf("account, unpaired: %v", got)
	}
}
