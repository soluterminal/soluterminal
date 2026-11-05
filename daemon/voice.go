package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"soluterminal-shared/voiceclean"
)

// transcribe forwards a browser-uploaded audio blob to Groq's Whisper
// endpoint and returns {text}. The browser does MediaRecorder → POST to
// us; we re-wrap the audio as a multipart file and hand it to Groq with
// our server-side GROQ_API_KEY.
const groqTranscribeURL = "https://api.groq.com/openai/v1/audio/transcriptions"

// whisper-large-v3-turbo — Groq's fastest Whisper variant, ~3x faster
// than whisper-large-v3 (~300ms vs ~1s end-to-end) with a marginal
// accuracy dip on jargon that the cloud-side Haiku cleanup usually
// fixes. Briefly reverted to large-v3 on 2026-04-23 after a suspected
// hallucination was later traced to a bad mic setting — not the model.
// If stock-phrase hallucinations appear again, check mic/audio first
// before blaming the model; Haiku can't catch them downstream.
const groqModel = "whisper-large-v3-turbo"
const maxAudioBytes = 25 * 1024 * 1024 // Entire direct multipart body, not duration.
const cloudVoiceBodyBytes = 4 * 1024 * 1024

var groqClient = &http.Client{
	Timeout:   60 * time.Second,
	Transport: sharedHTTPTransport,
}
var cloudProxyClient = &http.Client{
	// Cloud path layers Whisper + Claude cleanup; needs a more generous
	// timeout than direct-to-Groq.
	Timeout:   120 * time.Second,
	Transport: sharedHTTPTransport,
}

func (a *apiServer) transcribeAudio(w http.ResponseWriter, r *http.Request) {
	// Preferred path: proxy to the cloud with this machine's pairing
	// token. The cloud runs its own Whisper key + per-user daily quota,
	// so paired users get voice for free and we don't need a key in
	// ~/.solu/env. Kicks in whenever --relay is set and a claim exists.
	localOK, _ := a.localCleanupAvailable()
	if base, tok := a.cloudCreds(); base != "" && tok != "" {
		if localOK {
			a.transcribeViaCloudCleanLocally(w, r)
			return
		}
		a.proxyTranscribeToCloud(w, r)
		return
	}

	apiKey := strings.TrimSpace(os.Getenv("GROQ_API_KEY"))
	if apiKey == "" {
		// The web turns this code into a "Connect this computer" action.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":   "voice_not_connected",
			"message": "Voice needs this computer connected to your free account.",
		})
		return
	}

	startT := time.Now()
	defer func() {
		log.Printf("voice[daemon]: total=%dms", time.Since(startT).Milliseconds())
	}()

	r.Body = http.MaxBytesReader(w, r.Body, maxAudioBytes)
	if err := r.ParseMultipartForm(maxAudioBytes); err != nil {
		voiceMultipartError(w, err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, hdr, err := r.FormFile("audio")
	if err != nil {
		http.Error(w, "missing 'audio' file field", 400)
		return
	}
	defer file.Close()

	filename := hdr.Filename
	if filename == "" {
		filename = "audio.webm"
	}
	audio, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "read upload: "+err.Error(), 400)
		return
	}
	// Optional language hint from the client. Leaving blank lets Whisper
	// auto-detect, which is what we want by default.
	lang := voiceclean.NormalizeLanguage(r.FormValue("language"))

	groqStart := time.Now()
	gr := transcribeWithVocabulary(r.Context(), apiKey, filename, audio, lang, a.voiceWords().list())
	log.Printf("voice[daemon]: groq=%dms status=%d", time.Since(groqStart).Milliseconds(), gr.status)
	if gr.err != nil {
		http.Error(w, "groq request failed: "+gr.err.Error(), 502)
		return
	}
	if gr.status < 200 || gr.status >= 300 {
		// Surface Groq's error body so the client sees "API key invalid",
		// "file too large", etc. Capped to avoid dumping huge payloads.
		msg := string(gr.raw)
		if len(msg) > 500 {
			msg = msg[:500]
		}
		http.Error(w, fmt.Sprintf("groq %d: %s", gr.status, msg), 502)
		return
	}
	if gr.parseErr != nil {
		http.Error(w, "parse groq response: "+gr.parseErr.Error(), 502)
		return
	}
	groqResp := struct{ Text string }{Text: gr.text}
	text := strings.TrimSpace(groqResp.Text)
	if !localOK {
		out := gr.verbose.Diagnostics(lang)
		out["text"], out["cleanup"] = text, "disabled"
		writeJSON(w, 200, out)
		return
	}
	// With the user's own Groq key, cleanup can still run on their Claude
	// login: deferred (the client asks /api/voice/cleanup next) or inline.
	if r.FormValue("defer_cleanup") == "1" {
		out := gr.verbose.Diagnostics(lang)
		out["text"], out["cleanup"] = text, "deferred"
		writeJSON(w, 200, out)
		return
	}
	out := gr.verbose.Diagnostics(lang)
	for key, value := range a.cleanLocallyOnly(r, text, r.FormValue("context")) {
		out[key] = value
	}
	if out["text"] != text {
		out["rawText"] = text
	}
	writeJSON(w, 200, out)
}

// voiceCleanup is the raw-then-patch polish call. It runs on the user's own
// Claude login when that's available (voice_local.go) and falls back to the
// cloud's cleanup (operator keys, counted against the daily allowance). On an
// unpaired daemon with no local Claude there is no cleanup provider, so it
// returns 503 and the client keeps the raw text.
func (a *apiServer) voiceCleanup(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), 400)
		return
	}
	if ok, _ := a.localCleanupAvailable(); ok {
		var req struct {
			Text    string `json:"text"`
			Context string `json:"context"`
			// "writing" for everyday prose (Shinoque); anything else is the
			// terminal prompt.
			Mode     string `json:"mode"`
			Language string `json:"language"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), 400)
			return
		}
		raw := strings.TrimSpace(req.Text)
		profile := voiceclean.ParseProfile(req.Mode)
		if voiceclean.IsNoSpeech(raw) || len(raw) < voiceclean.MinCleanupBytesFor(profile) {
			writeJSON(w, 200, map[string]string{"text": raw, "cleanup": "skipped", "mode": string(profile)})
			return
		}
		out := a.localCleaner().CleanWithLanguage(r.Context(), profile, raw, voiceclean.TrimContextFor(profile, req.Context), req.Language)
		if out.Status != "failed" {
			writeJSON(w, 200, map[string]string{"text": out.Text, "cleanup": out.Status, "by": "local", "mode": string(profile), "requestedLanguage": voiceclean.RequestedLanguage(req.Language), "cleanupProvider": out.Provider, "rawText": raw})
			return
		}
		log.Printf("voice[daemon]: local cleanup failed detailLen=%d; using the cloud", len(out.Detail))
	}
	a.proxyCleanupToCloud(w, r, cloudCleanupBody(body, a.trustedDevices.policy() == remoteAccessApproved))
}

// cloudCleanupBody is what may leave this computer for the cloud fallback. A
// writing (prose) request's context is the user's open conversation, which is
// only ever shared with their own Claude login, so it is dropped here. With
// only approved devices allowed (private), terminal context is dropped too:
// the person has chosen that the cloud should not see their sessions.
func cloudCleanupBody(body []byte, private bool) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	mode, _ := m["mode"].(string)
	if !private && voiceclean.ParseProfile(mode) != voiceclean.ProfileWriting {
		return body
	}
	if _, has := m["context"]; !has {
		return body
	}
	delete(m, "context")
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// proxyCleanupToCloud forwards a {text, context} body to the cloud's
// /api/voice/cleanup and tags a successful answer with by:"cloud".
func (a *apiServer) proxyCleanupToCloud(w http.ResponseWriter, r *http.Request, body []byte) {
	cloudBase, cloudToken := a.cloudCreds()
	if cloudBase == "" || cloudToken == "" {
		http.Error(w, "voice cleanup requires cloud pairing (unpaired daemon does no cleanup)", 503)
		return
	}
	status, header, payload, err := a.cloudCleanupCall(r.Context(), cloudBase, cloudToken, body)
	if err != nil {
		http.Error(w, "cloud cleanup failed: "+err.Error(), 502)
		return
	}
	for _, h := range []string{"Content-Type", "X-Cleanup-Remaining", "Retry-After"} {
		if v := header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if status == 200 {
		if tagged, ok := tagCleanupBy(payload, "cloud"); ok {
			payload = tagged
			w.Header().Set("Content-Type", "application/json")
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func (a *apiServer) cloudCleanupCall(ctx context.Context, cloudBase, cloudToken string, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", cloudBase+"/api/voice/cleanup", bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cloudToken)
	resp, err := cloudProxyClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, payload, nil
}

// tagCleanupBy adds "by" to a JSON object body; ok is false for anything
// that isn't a JSON object, which is then passed through untouched.
func tagCleanupBy(payload []byte, by string) ([]byte, bool) {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil || m == nil {
		return payload, false
	}
	m["by"] = by
	out, err := json.Marshal(m)
	if err != nil {
		return payload, false
	}
	return out, true
}

// proxyTranscribeToCloud streams the browser's multipart upload straight
// to the cloud's /api/transcribe using this machine's pairing token.
// The cloud enforces the per-user daily quota and runs the Claude
// cleanup pass, so we just pipe bytes through and mirror the response.
//
// Streaming the body (rather than buffering) keeps memory flat for 10MB
// audio uploads on memory-tight hosts. We preserve the upstream status
// code and rate-limit headers so the browser's existing error handling
// (429 toast, X-Transcribe-Remaining hint) works transparently.
func (a *apiServer) proxyTranscribeToCloud(w http.ResponseWriter, r *http.Request) {
	cloudBase, cloudToken := a.cloudCreds()
	url := cloudBase + "/api/transcribe"
	var bodyReader io.Reader = r.Body
	contentType := r.Header.Get("Content-Type")
	contentLength := r.ContentLength
	words := a.voiceWords().formValue()
	// With "Only devices I approve" on, the terminal snapshot stays on this
	// computer, as it does for the cleanup calls (cloudCleanupBody).
	private := a.trustedDevices.policy() == remoteAccessApproved
	if words != "" || private {
		// The user's own words go with the audio; the cloud adds them to the
		// fixed name list it sends to Whisper.
		r.Body = http.MaxBytesReader(w, r.Body, maxAudioBytes)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			voiceMultipartError(w, err)
			return
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()
		set := map[string]string{}
		if words != "" {
			set["vocabulary"] = words
		}
		var drop []string
		if private {
			drop = []string{"context"}
		}
		buf, ct, err := rebuildMultipart(r.MultipartForm, set, drop)
		if err != nil {
			http.Error(w, "multipart rebuild: "+err.Error(), 500)
			return
		}
		if buf.Len() > cloudVoiceBodyBytes {
			http.Error(w, "recording part upload is too large", 413)
			return
		}
		bodyReader, contentType, contentLength = buf, ct, int64(buf.Len())
	}
	req, err := http.NewRequestWithContext(r.Context(), "POST", url, bodyReader)
	if err != nil {
		http.Error(w, "build request: "+err.Error(), 500)
		return
	}
	// Forward the multipart Content-Type (carries the boundary) plus
	// Content-Length so the cloud can MaxBytesReader it.
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.ContentLength = contentLength
	req.Header.Set("Authorization", "Bearer "+cloudToken)

	proxyStart := time.Now()
	resp, err := cloudProxyClient.Do(req)
	log.Printf("voice[daemon→cloud]: roundtrip=%dms", time.Since(proxyStart).Milliseconds())
	if err != nil {
		http.Error(w, "cloud transcribe failed: "+err.Error(), 502)
		return
	}
	defer resp.Body.Close()

	// Mirror rate-limit / content-type headers back to the browser so
	// the existing client UX (quota warning toast, remaining count)
	// keeps working without client-side changes.
	for _, h := range []string{"Content-Type", "X-Transcribe-Remaining", "X-Transcribe-Limit", "Retry-After"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func voiceMultipartError(w http.ResponseWriter, err error) {
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		http.Error(w, "recording part upload is too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "invalid multipart upload", http.StatusBadRequest)
}
