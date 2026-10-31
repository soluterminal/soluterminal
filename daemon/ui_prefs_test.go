package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func uiPrefsRequest(a *apiServer, method, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	a.handleUIPrefs(w, httptest.NewRequest(method, "/api/settings/ui", strings.NewReader(body)))
	return w
}

func TestUIPrefsPersistence(t *testing.T) {
	dir := t.TempDir()
	a := &apiServer{uiPrefs: newUIPrefsStore(dir)}
	if got := uiPrefsRequest(a, "GET", ""); got.Code != 200 || !strings.Contains(got.Body.String(), `"prefs":null`) {
		t.Fatalf("new user: %d %s", got.Code, got.Body.String())
	}
	body := `{"sidebar":{"order":[],"hidden":[],"open":[],"workspacesCollapsed":true}}`
	if got := uiPrefsRequest(a, "PUT", body); got.Code != 200 {
		t.Fatalf("save: %d %s", got.Code, got.Body.String())
	}
	// A fresh server instance reads the durable copy, preserving explicit empty
	// arrays (all sections shown / all sections collapsed) versus absent defaults.
	a = &apiServer{uiPrefs: newUIPrefsStore(dir)}
	got := uiPrefsRequest(a, "GET", "")
	var reply struct {
		Prefs json.RawMessage `json:"prefs"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if string(reply.Prefs) != body {
		t.Fatalf("persisted = %s, want %s", reply.Prefs, body)
	}
	info, err := os.Stat(a.uiPrefs.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file info = %v, %v", info, err)
	}
	// Reset is a saved choice too, distinguishable from no daemon copy.
	if got := uiPrefsRequest(a, "PUT", `{}`); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if got := uiPrefsRequest(a, "GET", ""); !strings.Contains(got.Body.String(), `"prefs":{}`) {
		t.Fatal(got.Body.String())
	}
}

func TestUIPrefsValidationAndSizeCap(t *testing.T) {
	a := &apiServer{uiPrefs: newUIPrefsStore(t.TempDir())}
	valid := `{"sidebar":{"hidden":[]}}`
	uiPrefsRequest(a, "PUT", valid)
	for _, body := range []string{
		`{"Sidebar":null}`, `{"sidebar":{"Hidden":null}}`,
		`null`, `[]`, ``, `{} {}`, `{"other":true}`, `{"sidebar":null}`,
		`{"sidebar":{"hidden":"mcp"}}`, `{"sidebar":{"hidden":[1]}}`,
		`{"sidebar":{"order":null}}`, `{"sidebar":{"hidden":null}}`,
		`{"sidebar":{"open":true}}`, `{"sidebar":{"workspacesCollapsed":"yes"}}`,
		`{"sidebar":{"workspacesCollapsed":null}}`, `{"sidebar":{"unknown":1}}`,
		`{"sidebar":{"order":["../file"]}}`, `{"sidebar":{"hidden":[""]}}`,
		`{"sidebar":{"hidden":["` + strings.Repeat("a", 65) + `"]}}`,
		`{"sidebar":{"open":[` + strings.Repeat(`"files",`, 64) + `"files"]}}`,
	} {
		t.Run(body, func(t *testing.T) {
			if got := uiPrefsRequest(a, "PUT", body); got.Code != 400 {
				t.Fatalf("status = %d, %s", got.Code, got.Body.String())
			}
		})
	}
	// Even a tiny valid JSON object with oversized trailing whitespace is capped.
	if got := uiPrefsRequest(a, "PUT", `{}`+strings.Repeat(" ", uiPrefsMaxBody)); got.Code != 413 {
		t.Fatalf("size cap: %d", got.Code)
	}
	if got := uiPrefsRequest(a, "GET", ""); !strings.Contains(got.Body.String(), valid) {
		t.Fatalf("invalid PUT changed prefs: %s", got.Body.String())
	}
}

func TestUIPrefsRequiresDaemonAuthentication(t *testing.T) {
	a := &apiServer{uiPrefs: newUIPrefsStore(t.TempDir())}
	mux := http.NewServeMux()
	a.routes(mux)
	handler := (&authGate{token: "secret"}).middleware(mux)
	for _, method := range []string{"GET", "PUT"} {
		for _, authed := range []bool{false, true} {
			req := httptest.NewRequest(method, "http://daemon.example/api/settings/ui", strings.NewReader(`{}`))
			req.RemoteAddr = "192.0.2.1:1234"
			if authed {
				req.Header.Set("Authorization", "Bearer secret")
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			want := 401
			if authed {
				want = 200
			}
			if w.Code != want {
				t.Fatalf("%s authed=%v: %d, %s", method, authed, w.Code, w.Body.String())
			}
		}
	}
}

func TestUIPrefsFailedWritePreservesLayout(t *testing.T) {
	a := &apiServer{uiPrefs: newUIPrefsStore(t.TempDir())}
	uiPrefsRequest(a, "PUT", `{"sidebar":{"hidden":[]}}`)
	// A path inside a missing directory fails without altering the saved copy.
	original := a.uiPrefs.path
	a.uiPrefs.path = original + "/missing/ui.json"
	if got := uiPrefsRequest(a, "PUT", `{}`); got.Code != 500 {
		t.Fatalf("status = %d", got.Code)
	}
	a.uiPrefs.path = original
	if got := uiPrefsRequest(a, "GET", ""); !strings.Contains(got.Body.String(), `"hidden":[]`) {
		t.Fatal(got.Body.String())
	}
}
