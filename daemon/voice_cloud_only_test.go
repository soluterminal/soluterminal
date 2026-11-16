package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// ST-180: a cloud-only request (another product relaying a person's
// dictation) never runs on the person's Claude login.

func postCloudOnlyCleanup(a *apiServer, extra map[string]any) *httptest.ResponseRecorder {
	req := map[string]any{"text": rawDictation, "mode": "writing", "cloudOnly": true}
	for k, v := range extra {
		req[k] = v
	}
	body, _ := json.Marshal(req)
	rec := httptest.NewRecorder()
	a.voiceCleanup(rec, httptest.NewRequest("POST", "/api/voice/cleanup", bytes.NewReader(body)))
	return rec
}

func TestVoiceLocalAdvertisesCloudOnly(t *testing.T) {
	a, _ := newVoiceAPI(t, "ok")
	if !a.voiceLocalSnapshot().CloudOnly {
		t.Fatal("GET /api/voice/local must advertise cloudOnly")
	}
}

func TestVoiceCleanupCloudOnlySkipsLocalAndDropsContext(t *testing.T) {
	a, dir := newVoiceAPI(t, "ok")
	var hits int32
	var saw map[string]any
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewDecoder(r.Body).Decode(&saw)
		writeJSON(w, 200, map[string]string{"text": "Cloud cleaned.", "cleanup": "ok", "mode": "writing"})
	}))
	defer cloud.Close()
	a.cloudBase, a.cloudToken = cloud.URL, "tok"

	rec := postCloudOnlyCleanup(a, map[string]any{"context": "private conversation"})
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got["by"] != "cloud" || got["text"] != "Cloud cleaned." || hits != 1 {
		t.Fatalf("%d %v hits=%d", rec.Code, got, hits)
	}
	if n := countFiles(t, dir, "args"); n != 0 {
		t.Fatalf("local Claude ran %d times for a cloud-only request", n)
	}
	if _, has := saw["context"]; has {
		t.Fatalf("context reached the cloud: %v", saw)
	}
}

func TestVoiceCleanupCloudOnlyUnpairedRefuses(t *testing.T) {
	a, dir := newVoiceAPI(t, "ok")
	rec := postCloudOnlyCleanup(a, nil)
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 503 || got["error"] != "plan_cleanup_not_for_relay" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := countFiles(t, dir, "args"); n != 0 {
		t.Fatalf("local Claude ran %d times", n)
	}
}

func TestVoiceCleanupCloudOnlyFalseStillRunsLocally(t *testing.T) {
	a, _ := newVoiceAPI(t, "ok")
	rec := postCloudOnlyCleanup(a, map[string]any{"cloudOnly": false})
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got["by"] != "local" {
		t.Fatalf("%d %v", rec.Code, got)
	}
}

// assertPrewarmNoop sends a prewarm through h and checks no Claude process
// starts (Prewarm runs in a goroutine, so it waits), then proves the wait is
// long enough: a plain local prewarm does start one.
func assertPrewarmNoop(t *testing.T, a *apiServer, dir string, h http.Handler, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/voice/prewarm", bytes.NewReader([]byte(body))))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d", rec.Code)
	}
	time.Sleep(500 * time.Millisecond)
	if n := countFiles(t, dir, "args"); n != 0 {
		t.Fatalf("prewarm started Claude %d times", n)
	}
	a.handleVoicePrewarm(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/voice/prewarm", bytes.NewReader([]byte(`{"mode":"writing"}`))))
	voiceWaitFor(t, "control prewarm", func() bool { return countFiles(t, dir, "args") > 0 })
}

func TestVoicePrewarmCloudOnlyIsNoop(t *testing.T) {
	a, dir := newVoiceAPI(t, "ok")
	assertPrewarmNoop(t, a, dir, http.HandlerFunc(a.handleVoicePrewarm), `{"mode":"writing","cloudOnly":true}`)
}

func TestTranscribeCloudOnlyNeverCleansLocallyAndDropsContext(t *testing.T) {
	a, dir := newVoiceAPI(t, "ok")
	var cloudCalls int32
	var cloudFields map[string][]string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&cloudCalls, 1)
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			cloudFields = r.MultipartForm.Value
		}
		writeJSON(w, 200, map[string]string{"text": "Cloud transcribed and cleaned.", "cleanup": "ok"})
	}))
	defer cloud.Close()
	a.cloudBase, a.cloudToken = cloud.URL, "tok"

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("audio", "audio.webm")
	_, _ = fw.Write([]byte("AUDIO"))
	_ = mw.WriteField("context", "private conversation")
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/api/transcribe?cloudOnly=1", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.transcribeAudio(rec, req)
	if rec.Code != 200 || cloudCalls != 1 {
		t.Fatalf("%d %s calls=%d", rec.Code, rec.Body.String(), cloudCalls)
	}
	if _, sent := cloudFields["context"]; sent {
		t.Fatal("context reached the cloud")
	}
	if n := countFiles(t, dir, "args"); n != 0 {
		t.Fatalf("local Claude ran %d times", n)
	}
}

// @secure's condition: the daemon forces cloud-only itself for a request it
// can identify as relayed, even when the caller forgets the flag.

func postPlainCleanup(h http.Handler) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"text": rawDictation, "mode": "writing", "context": "private conversation"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/voice/cleanup", bytes.NewReader(body)))
	return rec
}

func cloudCleanupStub(t *testing.T, a *apiServer) (*int32, *map[string]any) {
	t.Helper()
	var hits int32
	var saw map[string]any
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewDecoder(r.Body).Decode(&saw)
		writeJSON(w, 200, map[string]string{"text": "Cloud cleaned.", "cleanup": "ok", "mode": "writing"})
	}))
	t.Cleanup(cloud.Close)
	a.cloudBase, a.cloudToken = cloud.URL, "tok"
	return &hits, &saw
}

func TestVoiceCleanupRelayedWithoutFlagNeverRunsLocally(t *testing.T) {
	a, dir := newVoiceAPI(t, "ok")
	hits, saw := cloudCleanupStub(t, a)
	rec := postPlainCleanup(markRelayTransport(http.HandlerFunc(a.voiceCleanup)))
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got["by"] != "cloud" || *hits != 1 {
		t.Fatalf("%d %v hits=%d", rec.Code, got, *hits)
	}
	if n := countFiles(t, dir, "args"); n != 0 {
		t.Fatalf("local Claude ran %d times for a relayed request", n)
	}
	if _, has := (*saw)["context"]; has {
		t.Fatalf("context reached the cloud: %v", *saw)
	}
}

func TestVoiceCleanupRelayedUnpairedRefuses(t *testing.T) {
	a, dir := newVoiceAPI(t, "ok")
	rec := postPlainCleanup(markRelayTransport(http.HandlerFunc(a.voiceCleanup)))
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 503 || got["error"] != "plan_cleanup_not_for_relay" || countFiles(t, dir, "args") != 0 {
		t.Fatalf("%d %s spawned=%d", rec.Code, rec.Body.String(), countFiles(t, dir, "args"))
	}
}

func TestVoiceCleanupFromShinoqueCoreWithoutFlagNeverRunsLocally(t *testing.T) {
	a, dir := newVoiceAPI(t, "ok")
	hits, _ := cloudCleanupStub(t, a)
	a.shinoqueTokens = &shinoqueTokenCache{}
	a.shinoqueTokens.pushedKey.store("/home/x/.shinoque", "47800", "key", 4242, "/opt/shinoque/core")
	prev := shinoqueRelayKeyPeer
	shinoqueRelayKeyPeer = func(*http.Request) (int, string, bool) { return 4242, "/opt/shinoque/core", true }
	t.Cleanup(func() { shinoqueRelayKeyPeer = prev })

	rec := postPlainCleanup(http.HandlerFunc(a.voiceCleanup))
	if rec.Code != 200 || *hits != 1 || countFiles(t, dir, "args") != 0 {
		t.Fatalf("%d %s hits=%d spawned=%d", rec.Code, rec.Body.String(), *hits, countFiles(t, dir, "args"))
	}

	// Another local caller still uses the local clean-up, including one that
	// got Shinoque's pid after it exited (a different executable).
	for _, peer := range []struct {
		pid int
		exe string
	}{{4243, "/opt/shinoque/core"}, {4242, "/usr/bin/other"}} {
		shinoqueRelayKeyPeer = func(*http.Request) (int, string, bool) { return peer.pid, peer.exe, true }
		rec = postPlainCleanup(http.HandlerFunc(a.voiceCleanup))
		var got map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if rec.Code != 200 || got["by"] != "local" {
			t.Fatalf("other caller %+v: %d %v", peer, rec.Code, got)
		}
	}
}

func TestVoicePrewarmRelayedIsNoop(t *testing.T) {
	a, dir := newVoiceAPI(t, "ok")
	assertPrewarmNoop(t, a, dir, markRelayTransport(http.HandlerFunc(a.handleVoicePrewarm)), `{"mode":"writing"}`)
}
