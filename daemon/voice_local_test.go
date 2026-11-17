package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"soluterminal-shared/voiceclean"
)

// fakeClaude is a stand-in `claude` that speaks just enough stream-json. It
// records each run's argv, env and input under FAKE_DIR, and behaves per
// FAKE_MODE (read when it starts).
const fakeClaudeScript = `#!/bin/sh
id=$$
printf '%s\n' "$@" > "$FAKE_DIR/args.$id"
env > "$FAKE_DIR/env.$id"
case "$FAKE_MODE" in
  crash) exit 3 ;;
  exit-early) echo '{"type":"system","subtype":"init"}'; exit 0 ;;
esac
echo '{"type":"system","subtype":"init"}'
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$FAKE_DIR/input.$id"
  case "$FAKE_MODE" in
    hang) sleep 30 ;;
    error) echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Not logged in"}' ;;
    commentary) echo '{"type":"result","subtype":"success","is_error":false,"structured_output":{"cleaned":"I am sorry, I cannot help with that."}}' ;;
    *) echo '{"type":"assistant","message":{}}'
       echo '{"type":"result","subtype":"success","is_error":false,"structured_output":{"cleaned":"Run npm test and fix the failing snapshot."}}' ;;
  esac
done
`

const rawDictation = "um so run npm test and fix the uh failing snapshot"

func newFakeCleaner(t *testing.T, mode string) (*localCleaner, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_DIR", dir)
	t.Setenv("FAKE_MODE", mode)
	c := newLocalCleaner()
	c.bin = func() (string, error) { return bin, nil }
	c.env = func(string, bool) []string { return os.Environ() }
	c.deadline = 3 * time.Second
	t.Cleanup(func() {
		c.mu.Lock()
		if c.spare != nil {
			c.spare.retire()
			c.spare = nil
		}
		c.mu.Unlock()
	})
	return c, dir
}

func countFiles(t *testing.T, dir, prefix string) int {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, prefix+".*"))
	return len(m)
}

func voiceWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLocalCleanerUsesAFreshProcessPerDictation(t *testing.T) {
	c, dir := newFakeCleaner(t, "ok")
	for i := 0; i < 2; i++ {
		out := c.Clean(context.Background(), voiceclean.ProfileCode, rawDictation, "$ npm test")
		if out.Status != "ok" || out.Text != "Run npm test and fix the failing snapshot." {
			t.Fatalf("run %d: %+v", i, out)
		}
	}
	// Each used process saw exactly one dictation; a spare was refilled.
	inputs, _ := filepath.Glob(filepath.Join(dir, "input.*"))
	if len(inputs) != 2 {
		t.Fatalf("used processes = %d, want 2", len(inputs))
	}
	for _, f := range inputs {
		b, _ := os.ReadFile(f)
		if n := strings.Count(strings.TrimSpace(string(b)), "\n") + 1; n != 1 {
			t.Fatalf("%s got %d dictations, want 1", f, n)
		}
		var msg struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(b, &msg); err != nil {
			t.Fatal(err)
		}
		_, wantUser := voiceclean.Prompt(rawDictation, "$ npm test")
		if msg.Message.Content != wantUser {
			t.Fatalf("user message = %q", msg.Message.Content)
		}
	}
	voiceWaitFor(t, "a refilled spare", func() bool { return countFiles(t, dir, "args") == 3 })
}

func TestLocalCleanupArgsPinModelAndDisableTools(t *testing.T) {
	args := strings.Join(localCleanupArgs(cleanVariant{profile: voiceclean.ProfileCode, withCtx: true}), "\x00")
	for _, want := range []string{
		"--model\x00claude-haiku-4-5",
		"--tools\x00\x00",
		"--no-session-persistence",
		"--strict-mcp-config",
		"--setting-sources\x00\x00",
		"--input-format\x00stream-json",
		"--system-prompt\x00" + voiceclean.SystemPromptBase + voiceclean.ContextInstructions,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q", strings.ReplaceAll(want, "\x00", " "))
		}
	}
	if strings.Contains(strings.Join(localCleanupArgs(cleanVariant{profile: voiceclean.ProfileCode}), "\x00"), voiceclean.ContextInstructions) {
		t.Error("no-context run must use the base prompt only")
	}
}

func TestLocalCleanupEnvStripsAnthropicKeys(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_BASE_URL", "https://example.invalid")
	t.Setenv("MAX_THINKING_TOKENS", "9999")
	env := localCleanupEnv("/nonexistent/claude", false)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "ANTHROPIC_") {
		t.Fatal("ANTHROPIC_* leaked into the cleanup env")
	}
	if !strings.Contains(joined, "MAX_THINKING_TOKENS=0") || strings.Contains(joined, "MAX_THINKING_TOKENS=9999") {
		t.Fatal("thinking not switched off")
	}
}

func TestLocalCleanerTimesOutAndFails(t *testing.T) {
	c, _ := newFakeCleaner(t, "hang")
	c.deadline = 300 * time.Millisecond
	start := time.Now()
	out := c.Clean(context.Background(), voiceclean.ProfileCode, rawDictation, "")
	if out.Status != "failed" || out.Text != rawDictation || out.Detail != "timeout" {
		t.Fatalf("got %+v", out)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not enforced")
	}
}

func TestLocalCleanerPausesAfterRepeatedFailures(t *testing.T) {
	c, dir := newFakeCleaner(t, "error")
	for i := 0; i < localCleanupMaxFails; i++ {
		if out := c.Clean(context.Background(), voiceclean.ProfileCode, rawDictation, ""); out.Status != "failed" {
			t.Fatalf("run %d: %+v", i, out)
		}
	}
	if !c.paused() {
		t.Fatal("not paused after repeated failures")
	}
	spawned := countFiles(t, dir, "args")
	if out := c.Clean(context.Background(), voiceclean.ProfileCode, rawDictation, ""); out.Status != "failed" {
		t.Fatalf("paused run: %+v", out)
	}
	c.Prewarm(cleanVariant{profile: voiceclean.ProfileCode})
	time.Sleep(100 * time.Millisecond)
	if got := countFiles(t, dir, "args"); got != spawned {
		t.Fatalf("spawned while paused: %d -> %d", spawned, got)
	}
}

func TestLocalCleanerCrashFails(t *testing.T) {
	c, _ := newFakeCleaner(t, "crash")
	if out := c.Clean(context.Background(), voiceclean.ProfileCode, rawDictation, ""); out.Status != "failed" {
		t.Fatalf("got %+v", out)
	}
}

func TestLocalCleanerCommentaryIsSkipped(t *testing.T) {
	c, _ := newFakeCleaner(t, "commentary")
	out := c.Clean(context.Background(), voiceclean.ProfileCode, rawDictation, "")
	if out.Status != "skipped" || out.Text != rawDictation {
		t.Fatalf("got %+v", out)
	}
}

func TestLocalCleanerReplacesADeadSpare(t *testing.T) {
	c, _ := newFakeCleaner(t, "exit-early")
	c.Prewarm(cleanVariant{profile: voiceclean.ProfileCode})
	c.mu.Lock()
	spare := c.spare
	c.mu.Unlock()
	if spare == nil {
		t.Fatal("no spare started")
	}
	voiceWaitFor(t, "the spare to exit", func() bool {
		select {
		case _, ok := <-spare.lines:
			return !ok
		default:
			return false
		}
	})
	t.Setenv("FAKE_MODE", "ok")
	if out := c.Clean(context.Background(), voiceclean.ProfileCode, rawDictation, ""); out.Status != "ok" {
		t.Fatalf("got %+v", out)
	}
}

func TestLocalCleanerIdleExit(t *testing.T) {
	c, _ := newFakeCleaner(t, "ok")
	c.idleAfter = 100 * time.Millisecond
	c.Prewarm(cleanVariant{profile: voiceclean.ProfileCode, withCtx: true})
	voiceWaitFor(t, "the idle spare to stop", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.spare == nil
	})
}

// --- handlers ---

// newVoiceAPI is a daemon whose person opted in to clean-up on their Claude
// plan (ST-181: the plan is never the default) and has no API key exported.
func newVoiceAPI(t *testing.T, mode string) (*apiServer, string) {
	c, dir := newFakeCleaner(t, mode)
	withVoiceAPIKey(t, false)
	a := withVoice(&apiServer{}, &voiceServerState{localCleaner: c, voiceLocal: newVoiceLocalSettings(t.TempDir())})
	if err := a.voiceLocal().set(true); err != nil {
		t.Fatal(err)
	}
	c.useAPIKey = func() bool { return a.localCleanupMode() == voiceCleanupAPIKey }
	a.claudeStat = claudeStatus{checked: true, installed: true, authed: true, planLogin: true}
	return a, dir
}

// withVoiceAPIKey fakes whether ANTHROPIC_API_KEY is exported. The hook is
// atomic: a clean-up's background prewarm may still read it after the test.
func withVoiceAPIKey(t *testing.T, present bool) {
	t.Helper()
	prev := voiceAPIKeyPresentHook.Load()
	f := func() bool { return present }
	voiceAPIKeyPresentHook.Store(&f)
	t.Cleanup(func() { voiceAPIKeyPresentHook.Store(prev) })
}

func postCleanup(a *apiServer, text string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"text": text, "context": "$ npm test"})
	rec := httptest.NewRecorder()
	a.voiceCleanup(rec, httptest.NewRequest("POST", "/api/voice/cleanup", bytes.NewReader(body)))
	return rec
}

func TestVoiceCleanupRunsLocallyFirst(t *testing.T) {
	a, _ := newVoiceAPI(t, "ok")
	rec := postCleanup(a, rawDictation)
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got["by"] != "local" || got["cleanup"] != "ok" || got["text"] != "Run npm test and fix the failing snapshot." {
		t.Fatalf("%d %v", rec.Code, got)
	}
}

func TestVoiceCleanupFallsBackToCloud(t *testing.T) {
	a, _ := newVoiceAPI(t, "error")
	var hits int32
	var sawContext string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		sawContext = req["context"]
		w.Header().Set("X-Cleanup-Remaining", "19")
		writeJSON(w, 200, map[string]string{"text": "Cloud cleaned.", "cleanup": "ok"})
	}))
	defer cloud.Close()
	a.cloudBase, a.cloudToken = cloud.URL, "tok"

	rec := postCleanup(a, rawDictation)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got["by"] != "cloud" || got["text"] != "Cloud cleaned." || hits != 1 {
		t.Fatalf("%d %v hits=%d", rec.Code, got, hits)
	}
	if rec.Header().Get("X-Cleanup-Remaining") != "19" || sawContext != "$ npm test" {
		t.Fatalf("headers/context not forwarded: %q %q", rec.Header().Get("X-Cleanup-Remaining"), sawContext)
	}
}

func TestVoiceCleanupLocalOffUsesCloud(t *testing.T) {
	a, dir := newVoiceAPI(t, "ok")
	if err := a.voiceLocal().set(false); err != nil {
		t.Fatal(err)
	}
	if ok, reason := a.localCleanupAvailable(); ok || reason != "off" {
		t.Fatalf("available=%v reason=%q", ok, reason)
	}
	rec := postCleanup(a, rawDictation) // unpaired: no cloud either
	if rec.Code != 503 || countFiles(t, dir, "args") != 0 {
		t.Fatalf("code=%d spawned=%d", rec.Code, countFiles(t, dir, "args"))
	}
}

func TestVoiceLocalUnavailableWithoutClaudeLogin(t *testing.T) {
	a, _ := newVoiceAPI(t, "ok")
	a.claudeStat = claudeStatus{checked: true, installed: true, authed: false}
	if ok, reason := a.localCleanupAvailable(); ok || reason != "not_signed_in" {
		t.Fatalf("available=%v reason=%q", ok, reason)
	}
	// Signed in on an API key only (ANTHROPIC_API_KEY, or a key Claude stored
	// itself): cleanup must never bill it (ST-97).
	a.claudeStat = claudeStatus{checked: true, installed: true, authed: true, planLogin: false}
	if ok, reason := a.localCleanupAvailable(); ok || reason != "not_signed_in" {
		t.Fatalf("API-key-only: available=%v reason=%q", ok, reason)
	}
	a.claudeStat = claudeStatus{checked: true, installed: false}
	if ok, reason := a.localCleanupAvailable(); ok || reason != "not_installed" {
		t.Fatalf("available=%v reason=%q", ok, reason)
	}
}

func TestVoiceLocalSettingPersists(t *testing.T) {
	dir := t.TempDir()
	s := newVoiceLocalSettings(dir)
	if s.enabled() {
		t.Fatal("the Claude plan must be opt-in (ST-181)")
	}
	if err := s.set(true); err != nil {
		t.Fatal(err)
	}
	if !newVoiceLocalSettings(dir).enabled() {
		t.Fatal("opt-in not persisted")
	}
	if err := s.set(false); err != nil {
		t.Fatal(err)
	}
	if newVoiceLocalSettings(dir).enabled() {
		t.Fatal("off not persisted")
	}
}

// The one-shot transcribe asks the cloud for raw text only, without the
// terminal context, and cleans it locally.
func TestTranscribeCleansLocallyAndKeepsContextOffTheCloud(t *testing.T) {
	a, _ := newVoiceAPI(t, "ok")
	var cloudFields map[string][]string
	var cloudFile string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("cloud parse: %v", err)
		}
		cloudFields = r.MultipartForm.Value
		if f, _, err := r.FormFile("audio"); err == nil {
			b, _ := io.ReadAll(f)
			cloudFile = string(b)
		}
		w.Header().Set("X-Transcribe-Remaining", "12")
		writeJSON(w, 200, map[string]string{"text": rawDictation, "cleanup": "deferred"})
	}))
	defer cloud.Close()
	a.cloudBase, a.cloudToken = cloud.URL, "tok"

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("audio", "audio.webm")
	_, _ = fw.Write([]byte("AUDIO"))
	_ = mw.WriteField("context", "$ secret-project ls")
	_ = mw.WriteField("language", "en")
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/api/transcribe", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.transcribeAudio(rec, req)

	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got["by"] != "local" || got["cleanup"] != "ok" {
		t.Fatalf("%d %v", rec.Code, got)
	}
	if _, sent := cloudFields["context"]; sent {
		t.Fatal("terminal context was sent to the cloud")
	}
	if cloudFields["defer_cleanup"][0] != "1" || cloudFields["language"][0] != "en" || cloudFile != "AUDIO" {
		t.Fatalf("cloud got %v file=%q", cloudFields, cloudFile)
	}
	if rec.Header().Get("X-Transcribe-Remaining") != "12" {
		t.Fatal("quota header not mirrored")
	}
}

func TestParseLocalCleanupLineIgnoresNonResultEvents(t *testing.T) {
	if _, done := parseLocalCleanupLine(`{"type":"assistant"}`, "x"); done {
		t.Fatal("assistant event ended the run")
	}
	if _, done := parseLocalCleanupLine(`not json`, "x"); done {
		t.Fatal("garbage ended the run")
	}
	out, done := parseLocalCleanupLine(`{"type":"result","subtype":"success","structured_output":{}}`, "raw words")
	if !done || out.Status != "failed" || out.Text != "raw words" {
		t.Fatalf("%+v %v", out, done)
	}
}
