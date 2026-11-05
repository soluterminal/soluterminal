package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"soluterminal-shared/voiceclean"
)

// transcribeViaCloudCleanLocally is the one-shot transcribe when cleanup can
// run on the user's Claude login: the cloud only transcribes (defer_cleanup,
// and without the terminal context, which stays on this machine), then the
// daemon cleans locally. If the local run fails, the cloud cleans instead.
func (a *apiServer) transcribeViaCloudCleanLocally(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAudioBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		voiceMultipartError(w, err)
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	deferred := r.FormValue("defer_cleanup") == "1"
	profile := voiceclean.ParseProfile(r.FormValue("mode"))
	termContext := voiceclean.TrimContextFor(profile, r.FormValue("context"))
	if !deferred {
		// Start Claude while the cloud transcribes.
		go a.localCleaner().Prewarm(newCleanVariant(profile, termContext))
	}

	set := map[string]string{"defer_cleanup": "1"}
	if words := a.voiceWords().formValue(); words != "" {
		set["vocabulary"] = words
	}
	body, contentType, err := rebuildMultipart(r.MultipartForm, set, []string{"context"})
	if err != nil {
		http.Error(w, "multipart rebuild: "+err.Error(), 500)
		return
	}
	if body.Len() > cloudVoiceBodyBytes {
		http.Error(w, "recording part upload is too large", 413)
		return
	}
	cloudBase, cloudToken := a.cloudCreds()
	req, err := http.NewRequestWithContext(r.Context(), "POST", cloudBase+"/api/transcribe", body)
	if err != nil {
		http.Error(w, "build request: "+err.Error(), 500)
		return
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+cloudToken)
	resp, err := cloudProxyClient.Do(req)
	if err != nil {
		http.Error(w, "cloud transcribe failed: "+err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "X-Transcribe-Remaining", "X-Transcribe-Limit", "Retry-After"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	var m map[string]any
	if resp.StatusCode != 200 || json.Unmarshal(payload, &m) != nil || m == nil {
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(payload)
		return
	}

	m["requestedLanguage"] = voiceclean.RequestedLanguage(r.FormValue("language"))
	if deferred {
		writeJSON(w, 200, m)
		return
	}
	text, _ := m["text"].(string)
	text = strings.TrimSpace(text)
	if noSpeech, _ := m["noSpeech"].(bool); noSpeech || voiceclean.IsNoSpeech(text) || len(text) < voiceclean.MinCleanupBytesFor(profile) {
		m["cleanup"] = "skipped"
		writeJSON(w, 200, m)
		return
	}
	out := a.localCleaner().CleanWithLanguage(r.Context(), profile, text, termContext, r.FormValue("language"))
	if out.Status != "failed" {
		m["text"], m["cleanup"], m["by"] = out.Text, out.Status, "local"
		m["cleanupProvider"] = out.Provider
		if out.Text != text {
			m["rawText"] = text
		}
		writeJSON(w, 200, m)
		return
	}
	log.Printf("voice[daemon]: local cleanup failed detailLen=%d; using the cloud", len(out.Detail))
	cleanBody, _ := json.Marshal(map[string]string{"text": text, "context": termContext, "mode": string(profile), "language": voiceclean.NormalizeLanguage(r.FormValue("language"))})
	cleanBody = cloudCleanupBody(cleanBody, a.trustedDevices.policy() == remoteAccessApproved)
	status, header, cloudPayload, err := a.cloudCleanupCall(r.Context(), cloudBase, cloudToken, cleanBody)
	var cleaned struct {
		Text     string `json:"text"`
		Cleanup  string `json:"cleanup"`
		Provider string `json:"cleanupProvider"`
	}
	if err == nil && status == 200 && json.Unmarshal(cloudPayload, &cleaned) == nil && cleaned.Text != "" {
		m["text"], m["cleanup"], m["by"] = cleaned.Text, cleaned.Cleanup, "cloud"
		m["cleanupProvider"] = cleaned.Provider
		if cleaned.Text != text {
			m["rawText"] = text
		}
		if v := header.Get("X-Cleanup-Remaining"); v != "" {
			w.Header().Set("X-Cleanup-Remaining", v)
		}
	} else {
		m["cleanup"] = "failed"
	}
	writeJSON(w, 200, m)
}

// cleanLocallyOnly cleans a transcript from the user's own Groq key on their
// Claude login. There is no cloud to fall back to on this path.
func (a *apiServer) cleanLocallyOnly(r *http.Request, text, termContext string) map[string]string {
	profile := voiceclean.ParseProfile(r.FormValue("mode"))
	if voiceclean.IsNoSpeech(text) || len(text) < voiceclean.MinCleanupBytesFor(profile) {
		return map[string]string{"text": text, "cleanup": "skipped"}
	}
	out := a.localCleaner().CleanWithLanguage(r.Context(), profile, text, voiceclean.TrimContextFor(profile, termContext), r.FormValue("language"))
	if out.Status == "failed" {
		return map[string]string{"text": text, "cleanup": "failed"}
	}
	return map[string]string{"text": out.Text, "cleanup": out.Status, "by": "local", "cleanupProvider": out.Provider}
}

// rebuildMultipart re-encodes a parsed upload with some fields set and some
// dropped. Files keep their field name, file name and content type.
func rebuildMultipart(form *multipart.Form, set map[string]string, drop []string) (*bytes.Buffer, string, error) {
	skip := map[string]bool{}
	for _, k := range drop {
		skip[k] = true
	}
	for k := range set {
		skip[k] = true
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, vals := range form.Value {
		if skip[k] {
			continue
		}
		for _, v := range vals {
			if err := mw.WriteField(k, v); err != nil {
				return nil, "", err
			}
		}
	}
	for k, v := range set {
		if err := mw.WriteField(k, v); err != nil {
			return nil, "", err
		}
	}
	for field, files := range form.File {
		for _, fh := range files {
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, fh.Filename))
			ct := fh.Header.Get("Content-Type")
			if ct == "" {
				ct = "application/octet-stream"
			}
			h.Set("Content-Type", ct)
			part, err := mw.CreatePart(h)
			if err != nil {
				return nil, "", err
			}
			f, err := fh.Open()
			if err != nil {
				return nil, "", err
			}
			_, err = io.Copy(part, f)
			_ = f.Close()
			if err != nil {
				return nil, "", err
			}
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return &buf, mw.FormDataContentType(), nil
}
