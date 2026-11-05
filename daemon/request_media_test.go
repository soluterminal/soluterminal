package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestMediaObservation(t *testing.T) {
	old := log.Writer()
	var output bytes.Buffer
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(old) })
	for _, tc := range []struct {
		path, contentType, body string
		observed                bool
	}{
		{"/api/file/line", "text/plain", `{}`, true},
		{"/api/hooks/tool-call", "application/x-www-form-urlencoded", `x=y`, true},
		{"/api/settings/ui", "multipart/form-data; boundary=test", `test`, true},
		{"/api/update/check", "", "", false},
		{"/api/file/line", "application/json", `{}`, false},
		{"/api/transcribe", "multipart/form-data; boundary=test", `test`, false},
		{"/api/panes/x/paste-image", "image/png", `test`, false},
		{"/api/panes/x/paste-file", "application/octet-stream", `test`, false},
		{"/api/proxy/41004/", "text/plain", `test`, false},
	} {
		output.Reset()
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:41001"+tc.path+"?private=never-log", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.contentType)
		r.Header.Set("Origin", "http://127.0.0.1:41001")
		r.Header.Set("User-Agent", "never-log-client-value")
		pattern := "POST /media-observation-test" + tc.path
		requestMediaCounts.Delete(pattern + "|browser")
		observeRequestMedia(r, pattern)
		if (output.Len() != 0) != tc.observed {
			t.Errorf("%s %s log=%q", tc.path, tc.contentType, output.String())
		}
		if strings.Contains(output.String(), "never-log") {
			t.Fatal("logged request values")
		}
	}
	// The observer remains non-blocking, including all browser-simple types.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/observe/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:41001/api/observe/example", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	requestMediaObserver(mux).ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatal("observer blocked request")
	}
}
