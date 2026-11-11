package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocalPreviewDrainSurvivesStablePollAndRemainsBounded(t *testing.T) {
	u, _, _, _ := rollbackFixture(t, "1.0.0", "1.0.0", []byte("unused"))
	u.manifestURL = "https://updates.test/releases/download/v1.0.0/update.json"
	u.inflightCount = func() int { return 2 }
	path, sum := writeLocalBuild(t, t.TempDir(), 0o755)
	candidate := localUpdateCandidate{path: path, sha256: sum, version: "1.0.0.1"}
	apply := func() error {
		return u.ApplyLocal(context.Background(), func(int) { t.Fatal("unexpected swap") }, candidate)
	}
	var busy *updateBusyError
	if err := apply(); !errors.As(err, &busy) || !u.WorkflowDrainPending() {
		t.Fatalf("local candidate must hold admission without a Stable update: %v", err)
	}
	u.mu.Lock()
	u.drainStart = time.Now().Add(-4 * time.Minute)
	start := u.drainStart
	u.mu.Unlock()
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !u.WorkflowDrainPending() || !u.drainStart.Equal(start) {
		t.Fatal("Stable poll reset the local Preview drain")
	}
	if err := apply(); !errors.As(err, &busy) || !u.drainStart.Equal(start) {
		t.Fatalf("same candidate must retain its countdown: %v", err)
	}
	u.mu.Lock()
	u.drainStart = time.Now().Add(-u.drainWindow - time.Second)
	u.mu.Unlock()
	var deferred *updateDeferredError
	if err := apply(); !errors.As(err, &deferred) || u.WorkflowDrainPending() {
		t.Fatalf("expired local drain must defer and reopen admission: %v", err)
	}
}

// serveUnpausedControl points the updater at a local, signed, unpaused
// control file. Without it ApplyLocal fetches the real one from GitHub, and
// an outage there (or a real operator pause) fails these tests closed before
// the checks they are about ever run.
func serveUnpausedControl(t *testing.T) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(UpdateControl{Schema: 1, Paused: false})
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, body)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update-control.json":
			w.Write(body)
		case "/update-control.json.sig":
			w.Write(sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("SOLU_UPDATE_MANIFEST_PUBKEY", base64.StdEncoding.EncodeToString(pub))
	t.Setenv("SOLU_UPDATE_CONTROL_URL", srv.URL+"/update-control.json")
}

func TestLocalPreviewChangedCandidateAndTerminalErrorClearOldDrain(t *testing.T) {
	serveUnpausedControl(t)
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.restartGate = new(atomic.Bool)
	inflight := 1
	u.inflightCount = func() int { return inflight }
	path, sum := writeLocalBuild(t, t.TempDir(), 0o755)
	candidate := localUpdateCandidate{path: path, sha256: sum, version: "1.0.0.1"}
	var busy *updateBusyError
	if err := u.ApplyLocal(context.Background(), func(int) {}, candidate); !errors.As(err, &busy) {
		t.Fatal(err)
	}
	u.drainStart = time.Now().Add(-4 * time.Minute)
	oldStart := u.drainStart
	candidate.sha256 = strings.Repeat("0", 64)
	if err := u.ApplyLocal(context.Background(), func(int) {}, candidate); !errors.As(err, &busy) || !u.drainStart.After(oldStart) {
		t.Fatalf("different content must get its own drain: %v", err)
	}
	inflight = 0
	u.executable = func() (string, error) { return filepath.Join(t.TempDir(), "solud"), nil }
	if err := u.ApplyLocal(context.Background(), func(int) { t.Fatal("unexpected swap") }, candidate); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("invalid candidate must still be rejected: %v", err)
	}
	if u.WorkflowDrainPending() || u.localDrainCandidateID != "" {
		t.Fatal("terminal candidate error retained local drain ownership")
	}
}

func writeLocalBuild(t *testing.T, dataDir string, mode os.FileMode) (string, string) {
	t.Helper()
	dir := localBuildsDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "solud-0.0.1168.3")
	body := []byte("#!/bin/sh\necho fake\n")
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return path, hex.EncodeToString(sum[:])
}

// A local build goes through the same guarded swap as a release: the busy
// gate, version ordering and hash verification all apply.
func TestApplyLocalUsesTheGuardedSwap(t *testing.T) {
	serveUnpausedControl(t)
	data := t.TempDir()
	path, sum := writeLocalBuild(t, data, 0o755)
	bin := filepath.Join(t.TempDir(), "solud")

	u := NewUpdater("0.0.1168.5", "", "", nil, nil)
	u.restartGate = new(atomic.Bool)
	u.executable = func() (string, error) { return bin, nil }

	u.busyCheck = func() *updateBusyError {
		return &updateBusyError{cause: updateBusyCauseCollaboration, reason: "pane p1 mid-turn", paneID: "p1"}
	}
	var busy *updateBusyError
	if err := u.ApplyLocal(context.Background(), func(int) {}, localUpdateCandidate{path: path, sha256: sum, version: "0.0.1168.9"}); !errors.As(err, &busy) {
		t.Fatalf("busy daemon: ApplyLocal = %v, want the busy gate", err)
	}
	u.busyCheck = nil

	err := u.ApplyLocal(context.Background(), func(int) {}, localUpdateCandidate{path: path, sha256: sum, version: "0.0.1168.3"})
	if err == nil || !strings.Contains(err.Error(), "not newer") {
		t.Fatalf("older local build: %v, want refusal", err)
	}

	err = u.ApplyLocal(context.Background(), func(int) {}, localUpdateCandidate{path: path, sha256: strings.Repeat("0", 64), version: "0.0.1168.3", allowDowngrade: true})
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("tampered build: %v, want a hash refusal", err)
	}
	if _, statErr := os.Stat(bin + ".new"); !os.IsNotExist(statErr) {
		t.Fatal("rejected build was left staged next to the daemon binary")
	}
}

func applyLocalRequest(t *testing.T, a *apiServer, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/update/apply-local", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50123"
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Solu-Local-Build", "1")
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	a.updateApplyLocal(rec, req)
	return rec
}

func TestApplyLocalHandlerOnlyAcceptsOwnerLocalBuilds(t *testing.T) {
	serveUnpausedControl(t)
	data := t.TempDir()
	path, sum := writeLocalBuild(t, data, 0o755)
	gate := new(atomic.Bool)
	gate.Store(true) // a held restart reservation proves a request got through to the updater
	u := NewUpdater("0.0.1168", "", "", nil, nil)
	u.restartGate = gate
	a := &apiServer{updater: u, dataDir: data}
	good := `{"path":"` + path + `","sha256":"` + sum + `","version":"0.0.1168.3"}`

	cases := []struct {
		name   string
		body   string
		mutate func(*http.Request)
		want   int
	}{
		{"over the cloud relay", good, func(r *http.Request) { r.Header.Set("X-Forwarded-Prefix", "/m/mach_1") }, 403},
		{"from another machine", good, func(r *http.Request) { r.RemoteAddr = "192.168.1.9:4000" }, 403},
		{"rebinding host", good, func(r *http.Request) { r.Host = "evil.example:8787" }, 403},
		{"without the local-build header", good, func(r *http.Request) { r.Header.Del("X-Solu-Local-Build") }, 400},
		{"outside local-builds", strings.Replace(good, path, "/usr/bin/true", 1), nil, 400},
		{"bad version", strings.Replace(good, "0.0.1168.3", "dev", 1), nil, 400},
		{"unknown field", strings.Replace(good, "}", `,"force":true}`, 1), nil, 400},
	}
	for _, c := range cases {
		if rec := applyLocalRequest(t, a, c.body, c.mutate); rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
	if rec := applyLocalRequest(t, a, good, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"busy":true`) {
		t.Fatalf("valid request did not reach the guarded updater: %d %s", rec.Code, rec.Body.String())
	}

	if err := os.Chmod(path, 0o775); err != nil {
		t.Fatal(err)
	}
	if rec := applyLocalRequest(t, a, good, nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "writable by other users") {
		t.Fatalf("group-writable build accepted: %d %s", rec.Code, rec.Body.String())
	}
}

// The build is re-checked on the opened file: swapping it for a symlink (or
// FIFO/device) after validation must not be copied.
func TestCopyLocalBuildRefusesSwappedFile(t *testing.T) {
	data := t.TempDir()
	path, sum := writeLocalBuild(t, data, 0o755)
	outside := filepath.Join(t.TempDir(), "other")
	os.WriteFile(outside, []byte("#!/bin/sh\necho fake\n"), 0o755)
	os.Remove(path)
	if err := os.Symlink(outside, path); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := copyVerifiedLocalBuild(path, sum, filepath.Join(t.TempDir(), "solud.new")); err == nil {
		t.Fatal("copied a build that was swapped for a symlink after validation")
	}
}
