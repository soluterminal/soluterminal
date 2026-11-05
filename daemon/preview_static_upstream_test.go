package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPreviewIsBackendPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/api", true},
		{"/api/health", true},
		{"/preview/abc123/api/health", true},
		{"/preview/abc123/api", true},
		{"/", false},
		{"/assets/app.js", false},
		{"/apian/thing", false},
	}
	for _, tc := range cases {
		if got := previewIsBackendPath(tc.path); got != tc.want {
			t.Errorf("previewIsBackendPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestMountedPreviewBackendUnavailable(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<title>Preview</title>"), 0600); err != nil {
		t.Fatal(err)
	}
	static := &previewStaticServer{root: root, spa: true}
	server := httptest.NewServer(http.HandlerFunc(static.serveHTTP))
	defer server.Close()
	for _, targetBase := range []string{"", "/preview/example"} {
		mounted := httptest.NewServer(previewMountedProxyHandler(serverPort(t, server.URL), "/preview/example", targetBase))
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req, _ := http.NewRequest(method, mounted.URL+"/preview/example/api/health", nil)
			req.Header.Set("Origin", mounted.URL)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "preview backend unavailable") {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
		}
		mounted.Close()
	}
}

func TestPreviewDoesNotForwardAuthorityMarker(t *testing.T) {
	var seen atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(true)
		if r.Header.Get("X-Soluterminal-Preview-Proxy") != "" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected preview credentials")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	mounted := httptest.NewServer(previewMountedProxyHandler(serverPort(t, upstream.URL), "/preview/example"))
	defer mounted.Close()
	req, _ := http.NewRequest(http.MethodPost, mounted.URL+"/preview/example/api/health", nil)
	req.Header.Set("X-Soluterminal-Preview-Proxy", "retired-marker")
	req.Header.Set("Authorization", "Bearer test-master")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !seen.Load() || resp.StatusCode != http.StatusNoContent {
		t.Fatal("preview transport unavailable")
	}
}
