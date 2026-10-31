package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func touch(path string, ts int64) recentFileTouch {
	return recentFileTouch{Path: path, Tool: "Edit", Ts: ts}
}

func TestRecentFilesRingDedupOrderCap(t *testing.T) {
	t.Parallel()
	r := newRecentFilesRing()
	r.max = 3

	r.record(touch("/r/a.go", 1))
	r.record(touch("/r/b.go", 2))
	r.record(touch("/r/a.go", 3)) // re-touch a -> moves to newest

	got := r.recent(10)
	if len(got) != 2 {
		t.Fatalf("want 2 distinct files, got %d (%v)", len(got), got)
	}
	if got[0].Path != "/r/a.go" || got[1].Path != "/r/b.go" {
		t.Fatalf("newest-first dedup wrong: %v", []string{got[0].Path, got[1].Path})
	}
	if got[0].Ts != 3 {
		t.Fatalf("re-touch should carry the new ts, got %d", got[0].Ts)
	}

	// Cap: pushing past max drops the oldest.
	r.record(touch("/r/c.go", 4))
	r.record(touch("/r/d.go", 5)) // now a,b,c,d -> capped to c? no: dedup keeps order b,a,c,d -> cap 3 = a,c,d
	got = r.recent(10)
	if len(got) != 3 {
		t.Fatalf("ring should cap at 3, got %d (%v)", len(got), got)
	}
	if got[0].Path != "/r/d.go" {
		t.Fatalf("newest should be d.go, got %s", got[0].Path)
	}
	// b.go was the oldest distinct entry and must have been evicted.
	for _, it := range got {
		if it.Path == "/r/b.go" {
			t.Fatalf("oldest entry b.go should have been evicted: %v", got)
		}
	}

	// limit is honored.
	if n := len(r.recent(2)); n != 2 {
		t.Fatalf("recent(2) returned %d", n)
	}
}

func TestHookEditedPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tool string
		fp   string
		cwd  string
		want string
		ok   bool
	}{
		{"Edit", "/abs/main.go", "/cwd", "/abs/main.go", true},
		{"Write", "sub/new.go", "/home/me/proj", "/home/me/proj/sub/new.go", true},
		{"MultiEdit", "./x.ts", "/p", "/p/x.ts", true},
		{"NotebookEdit", "/n/foo.ipynb", "", "/n/foo.ipynb", true},
		{"Read", "/abs/main.go", "/cwd", "", false}, // viewing isn't "touching"
		{"Bash", "/abs/main.go", "/cwd", "", false}, // not a file-edit tool
		{"Write", "rel.go", "", "", false},          // relative with no cwd -> can't resolve
		{"Edit", "", "/cwd", "", false},             // missing file_path
	}
	for _, tt := range tests {
		got, ok := hookEditedPath(tt.tool, map[string]any{"file_path": tt.fp}, tt.cwd)
		if ok != tt.ok || got != tt.want {
			t.Fatalf("hookEditedPath(%q,%q,%q) = (%q,%v), want (%q,%v)",
				tt.tool, tt.fp, tt.cwd, got, ok, tt.want, tt.ok)
		}
	}
}

// The endpoint must surface agent-touched paths locally, but never reveal
// secret/credential file paths over the cloud relay.
func TestRecentFilesEndpointRelayHidesSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(relayFileRootsEnv, "")
	t.Setenv(relayFileConfinementEnv, "")
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_CONFIG_HOME", "MUSE_AUTH_PATH"} {
		t.Setenv(key, "")
	}

	ring := newRecentFilesRing()
	repo := filepath.Join(home, "repo")
	mainPath := filepath.Join(repo, "src", "main.go")
	envPath := filepath.Join(repo, ".env")
	outsidePath := filepath.Join(home, "other", "notes.md")
	// Stop gitTopLevel at the fixture's repository, even if a host ancestor
	// (such as /tmp) has a .git entry that would also admit outsidePath.
	for _, dir := range []string{filepath.Join(repo, ".git"), filepath.Dir(mainPath), filepath.Dir(outsidePath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{mainPath, envPath, outsidePath} {
		if err := os.WriteFile(path, []byte("fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ring.record(touch(mainPath, 1))
	ring.record(touch(envPath, 2))
	ring.record(touch(outsidePath, 3))
	a := &apiServer{
		recentFiles:          ring,
		dataDir:              filepath.Join(home, ".solu"),
		relayPaneDirsForTest: []string{repo},
	}

	decode := func(req *http.Request) []recentFileTouch {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleRecentFiles(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
		}
		var resp recentFilesResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad json: %v (%q)", err, rec.Body.String())
		}
		return resp.Files
	}

	// Local: both files visible (operator can open their own .env).
	local := decode(httptest.NewRequest("GET", "/api/recent-files", nil))
	if len(local) != 3 {
		t.Fatalf("local should list all files, got %#v", local)
	}

	// Over relay: the .env path is omitted; the source file remains.
	relayReq := httptest.NewRequest("GET", "/api/recent-files", nil)
	relayReq.Header.Set("X-Forwarded-Prefix", "/m/mach_1")
	relay := decode(relayReq)
	for _, f := range relay {
		if f.Path == envPath {
			t.Fatalf("secret path leaked over relay: %#v", relay)
		}
		if f.Path == outsidePath {
			t.Fatalf("path outside the relay roots leaked: %#v", relay)
		}
	}
	var sawMain bool
	for _, f := range relay {
		if f.Path == mainPath {
			sawMain = true
		}
	}
	if !sawMain {
		t.Fatalf("non-secret file should still appear over relay: %#v", relay)
	}
}

// nil ring (never constructed) degrades to an empty list, not a panic/500.
func TestRecentFilesEndpointNilRing(t *testing.T) {
	t.Parallel()
	a := &apiServer{}
	rec := httptest.NewRecorder()
	a.handleRecentFiles(rec, httptest.NewRequest("GET", "/api/recent-files", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("nil-ring code=%d, want 200", rec.Code)
	}
}
