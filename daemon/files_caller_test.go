package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileEditorCaller(t *testing.T) {
	previous := identifyCallerHook
	t.Cleanup(func() { identifyCallerHook = previous })
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name string
		id   callerIdentity
		want int
	}{
		{"desktop", callerIdentity{Kind: callerHuman}, http.StatusOK},
		{"phone", callerIdentity{Kind: callerHuman, Relay: true}, http.StatusOK},
		{"code frame", callerIdentity{Kind: callerHuman, Device: "approved-test-device"}, http.StatusOK},
		{"program", callerIdentity{Kind: callerUnverified}, http.StatusForbidden},
		{"pane", callerIdentity{Kind: callerPane, PaneID: "test-pane"}, http.StatusForbidden},
		{"worker", callerIdentity{Kind: callerWorker}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identifyCallerHook = func(*http.Request) callerIdentity { return tc.id }
			path := filepath.Join(t.TempDir(), "example.txt")
			if err := os.WriteFile(path, []byte("before\n"), 0600); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(writeLineReq{Path: path, NewText: "after"})
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:41001/api/file/line", strings.NewReader(string(body)))
			r.Header.Set("Origin", "http://127.0.0.1:41001")
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			r.Header.Set("Authorization", "Bearer test-master")
			w := httptest.NewRecorder()
			(&apiServer{}).writeFileLine(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := "before\n"
			if tc.want == http.StatusOK {
				want = "after\n"
			}
			if string(content) != want {
				t.Fatalf("file=%q, want %q", content, want)
			}
		})
	}
}
