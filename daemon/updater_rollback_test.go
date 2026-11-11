package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func rollbackFixture(t *testing.T, current, target string, candidate []byte) (*Updater, *updateRollbackStore, string, staticRoundTripper) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("disposable capability script fixture is unix-only")
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOLU_UPDATE_MANIFEST_PUBKEY", base64.StdEncoding.EncodeToString(pub))
	t.Setenv("INVOCATION_ID", "test-systemd")
	dir := t.TempDir()
	bin := filepath.Join(dir, "solud")
	if err := os.WriteFile(bin, []byte("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	store, _ := newUpdateRollbackStore(dataDir)
	hash := sha256.Sum256(candidate)
	manifest, _ := json.Marshal(Manifest{
		Version: target,
		Flavor:  "public",
		Platforms: map[string]ManifestPlatform{
			platformKeyCurrent(): {URL: "https://cdn.test/solud", SHA256: hex.EncodeToString(hash[:])},
		},
	})
	base := "https://updates.test/releases/download/v" + target + "/"
	rt := staticRoundTripper{
		base + "update.json":     manifest,
		base + "update.json.sig": []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest))),
		"https://cdn.test/solud": candidate,
	}
	u := NewUpdater(current, "", "", nil, nil)
	u.manifestURL = "https://updates.test/releases/latest/download/update.json"
	u.httpClient = &http.Client{Transport: rt}
	u.restartGate = new(atomic.Bool)
	u.executable = func() (string, error) { return bin, nil }
	u.rollback = store
	u.dataDir = dataDir
	u.commitSleepFn = func(time.Duration) {}
	return u, store, bin, rt
}

func capabilityScript(version string, readers string) []byte {
	return []byte("#!/bin/sh\nprintf '%s\\n' '{\"version\":\"" + version + "\",\"rollbackHold\":1,\"stateReaders\":" + readers + "}'\n")
}

func TestRollbackVerifiedPinnedManifestPersistsHoldAndMarker(t *testing.T) {
	u, store, bin, _ := rollbackFixture(t, "2.0.0", "1.0.0", capabilityScript("1.0.0", `{"workflow":2,"compute":3}`))
	if err := u.Rollback(context.Background(), rollbackRequest{TargetVersion: "1.0.0"}, func(int) {}); err != nil {
		t.Fatal(err)
	}
	snapshot := store.Snapshot()
	if snapshot.Hold == nil || snapshot.Hold.Version != "2.0.0" || snapshot.Hold.FromVersion != "1.0.0" {
		t.Fatalf("hold = %+v", snapshot.Hold)
	}
	duration := time.Duration(snapshot.Hold.ExpiresAtMs-snapshot.Hold.CreatedAtMs) * time.Millisecond
	if duration != defaultRollbackHoldDuration || snapshot.LastAttempt == nil || snapshot.LastAttempt.State != "swapped" {
		t.Fatalf("state = %+v, duration=%s", snapshot, duration)
	}
	marker, err := os.ReadFile(bin + ".version")
	if err != nil || strings.TrimSpace(string(marker)) != "1.0.0" {
		t.Fatalf("marker = %q, err=%v", marker, err)
	}
	if u.verifiedManifest != nil {
		t.Fatal("rollback poisoned the latest-manifest cache")
	}
}

func TestRollbackRejectsWrongManifestAndSHAWithoutHold(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Updater, staticRoundTripper)
	}{
		{"bad signature", func(_ *Updater, rt staticRoundTripper) {
			base := "https://updates.test/releases/download/v1.0.0/"
			rt[base+"update.json.sig"] = []byte(base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)))
		}},
		{"sha mismatch", func(_ *Updater, rt staticRoundTripper) { rt["https://cdn.test/solud"] = []byte("corrupt") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, store, bin, rt := rollbackFixture(t, "2.0.0", "1.0.0", capabilityScript("1.0.0", `{}`))
			tc.edit(u, rt)
			if err := u.Rollback(context.Background(), rollbackRequest{TargetVersion: "1.0.0"}, func(int) {}); err == nil {
				t.Fatal("corrupt rollback was accepted")
			}
			if store.Snapshot().Hold != nil {
				t.Fatal("hold was written before artifact verification")
			}
			if got, err := os.ReadFile(bin); err != nil || string(got) != "current" {
				t.Fatalf("corrupt rollback changed installed binary: %q, %v", got, err)
			}
			if _, err := os.Stat(bin + ".rollback"); !os.IsNotExist(err) {
				t.Fatalf("corrupt rollback left staged candidate: %v", err)
			}
		})
	}
}

func TestPinnedRollbackManifestVersionCoherence(t *testing.T) {
	u, store, bin, rt := rollbackFixture(t, "2.0.0", "1.0.0", capabilityScript("1.0.0", `{}`))
	pub, priv, _ := ed25519.GenerateKey(nil)
	t.Setenv("SOLU_UPDATE_MANIFEST_PUBKEY", base64.StdEncoding.EncodeToString(pub))
	base := "https://updates.test/releases/download/v1.0.0/"
	body := []byte(`{"flavor":"public","version":"1.0.1","platforms":{"` + platformKeyCurrent() + `":{"url":"https://cdn.test/solud","sha256":"00"}}}`)
	rt[base+"update.json"] = body
	rt[base+"update.json.sig"] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, body)))
	if _, err := u.fetchPinnedManifest(context.Background(), "1.0.0"); err == nil || !strings.Contains(err.Error(), "requested version") {
		t.Fatalf("coherence error = %v", err)
	}
	if err := u.Rollback(context.Background(), rollbackRequest{TargetVersion: "1.0.0"}, func(int) {}); err == nil || !strings.Contains(err.Error(), "requested version") {
		t.Fatalf("rollback coherence error = %v", err)
	}
	if store.Snapshot().Hold != nil {
		t.Fatal("wrong-version manifest wrote a hold")
	}
	if got, _ := os.ReadFile(bin); string(got) != "current" {
		t.Fatalf("wrong-version manifest swapped binary: %q", got)
	}
}

func TestRollbackHoldSurvivesRestartAndBlocksApplyIncludingForce(t *testing.T) {
	dir := t.TempDir()
	store, _ := newUpdateRollbackStore(dir)
	now := time.Now()
	if err := store.SetHold(rollbackHold{Version: "2.0.0"}, now); err != nil {
		t.Fatal(err)
	}
	fresh, _ := newUpdateRollbackStore(dir)
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.rollback = fresh
	u.restartGate = new(atomic.Bool)
	u.manifest = &Manifest{Version: "2.0.0", Platforms: map[string]ManifestPlatform{platformKeyCurrent(): {URL: "x", SHA256: "x"}}}
	u.httpClient = &http.Client{Transport: staticRoundTripper{}}
	statusRec := httptest.NewRecorder()
	(&apiServer{updater: u}).updateStatus(statusRec, httptest.NewRequest(http.MethodGet, "/api/update/status", nil))
	if statusRec.Code != http.StatusOK || !strings.Contains(statusRec.Body.String(), `"holdVersion":"2.0.0"`) {
		t.Fatalf("fresh daemon status omitted persisted hold: %d %s", statusRec.Code, statusRec.Body.String())
	}
	for _, force := range []bool{false, true} {
		err := u.Apply(context.Background(), func(int) {}, force)
		var held *updateHeldError
		if !errors.As(err, &held) {
			t.Fatalf("force=%v error=%v, want held", force, err)
		}
	}
	rec := httptest.NewRecorder()
	(&apiServer{updater: u}).updateApply(rec, httptest.NewRequest(http.MethodPost, "/api/update/apply?force=1", nil))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"update_held"`) {
		t.Fatalf("held response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestRollbackDeclinesIncompatibleStateAndFailedProbe(t *testing.T) {
	for _, tc := range []struct {
		name      string
		candidate []byte
		marker    bool
	}{
		{"reader", capabilityScript("1.0.0", `{"workflow":2}`), true},
		{"probe", []byte("#!/bin/sh\nexit 1\n"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, store, bin, _ := rollbackFixture(t, "2.0.0", "1.0.0", tc.candidate)
			if tc.marker {
				path := filepath.Join(u.dataDir, "workflow", "version.json")
				_ = os.MkdirAll(filepath.Dir(path), 0o700)
				_ = os.WriteFile(path, []byte(`{"minReaderVersion":3}`), 0o600)
			}
			err := u.Rollback(context.Background(), rollbackRequest{TargetVersion: "1.0.0"}, func(int) {})
			var incompatible *rollbackIncompatibleError
			if !errors.As(err, &incompatible) {
				t.Fatalf("error = %v", err)
			}
			if tc.marker && incompatible.Store != "workflow" {
				t.Fatalf("store = %q", incompatible.Store)
			}
			if store.Snapshot().Hold != nil || store.Snapshot().LastAttempt == nil || store.Snapshot().LastAttempt.State != "declined" {
				t.Fatalf("state = %+v", store.Snapshot())
			}
			if _, err := os.Stat(bin + ".rollback"); !os.IsNotExist(err) {
				t.Fatalf("candidate remains: %v", err)
			}
			if got, err := os.ReadFile(bin); err != nil || string(got) != "current" {
				t.Fatalf("declined rollback changed installed binary: %q, %v", got, err)
			}
		})
	}
}

func TestRollbackBusyForceAndFailedSuccessorRecovery(t *testing.T) {
	u, store, bin, _ := rollbackFixture(t, "2.0.0", "1.0.0", capabilityScript("1.0.0", `{}`))
	u.busyCheck = func() *updateBusyError {
		return &updateBusyError{cause: updateBusyCauseCollaboration, reason: "pane busy", paneID: "p1"}
	}
	err := u.Rollback(context.Background(), rollbackRequest{TargetVersion: "1.0.0"}, func(int) {})
	var busy *updateBusyError
	if !errors.As(err, &busy) || busy.cause != updateBusyCauseCollaboration {
		t.Fatalf("busy error = %v", err)
	}
	u.busyCheck = nil
	t.Setenv("INVOCATION_ID", "")
	u.spawnSuccessorFn = func(string, string, string) (*os.Process, error) { return nil, nil }
	u.waitSuccessorBootFn = func(string, time.Duration) bool { return false }
	err = u.Rollback(context.Background(), rollbackRequest{TargetVersion: "1.0.0", Force: true}, func(int) {})
	if err == nil || store.Snapshot().LastAttempt == nil || store.Snapshot().LastAttempt.State != "failed" || store.Snapshot().Hold == nil {
		t.Fatalf("failed rollback = %v, state=%+v", err, store.Snapshot())
	}
	body, readErr := os.ReadFile(bin)
	if readErr != nil || string(body) != "current" {
		t.Fatalf("original binary not restored: %q, %v", body, readErr)
	}
	marker, _ := os.ReadFile(bin + ".version")
	if strings.TrimSpace(string(marker)) != "2.0.0" {
		t.Fatalf("marker = %q", marker)
	}
	if err2 := u.Rollback(context.Background(), rollbackRequest{TargetVersion: "1.0.0", Force: true}, func(int) {}); err2 == nil {
		t.Fatal("second injected failure unexpectedly succeeded")
	} else if errors.As(err2, &busy) {
		t.Fatalf("second rollback remained wedged behind restart gate: %v", err2)
	}
}

func TestRollbackWorkflowGateAndForceScope(t *testing.T) {
	u := NewUpdater("2.0.0", "", "", nil, nil)
	u.restartGate = new(atomic.Bool)
	u.rollback, _ = newUpdateRollbackStore(t.TempDir())
	u.inflightCount = func() int { return 2 }
	err := u.Rollback(context.Background(), rollbackRequest{TargetVersion: "1.0.0"}, func(int) {})
	var busy *updateBusyError
	if !errors.As(err, &busy) || busy.cause != updateBusyCauseWorkflow || busy.inflight != 2 {
		t.Fatalf("workflow error = %#v (%v)", busy, err)
	}
	err = u.Rollback(context.Background(), rollbackRequest{TargetVersion: "1.0.0", Force: true}, func(int) {})
	if errors.As(err, &busy) || err == nil {
		t.Fatalf("force did not pass workload gate: %v", err)
	}
}

func TestHeldReleaseCheckExpiryAndOperatorFloorPrecedence(t *testing.T) {
	disableControlSignature(t)
	current, latest := "1.0.0", "2.0.0"
	candidate := []byte("next")
	digest := sha256.Sum256(candidate)
	manifest := []byte(`{"flavor":"public","version":"2.0.0","platforms":{"` + platformKeyCurrent() + `":{"url":"https://cdn.test/solud","sha256":"` + hex.EncodeToString(digest[:]) + `"}}}`)
	base := "https://updates.test/releases/download/v2.0.0/"
	controlURL := "https://updates.test/releases/download/control/update-control.json"
	newUpdater := func(t *testing.T, store *updateRollbackStore, control []byte) *Updater {
		u := NewUpdater(current, "", "", nil, nil)
		u.rollback = store
		u.manifestURL = "https://updates.test/releases/latest/download/update.json"
		rt := staticRoundTripper{
			u.manifestURL: []byte(`{"version":"2.0.0"}`), base + "update.json": manifest,
			"https://cdn.test/solud": candidate,
		}
		if control != nil {
			rt[controlURL] = control
		}
		u.httpClient = &http.Client{Transport: rt}
		return u
	}

	t.Run("active", func(t *testing.T) {
		dir := t.TempDir()
		store, _ := newUpdateRollbackStore(dir)
		_ = store.SetHold(rollbackHold{Version: latest}, time.Now())
		fresh, _ := newUpdateRollbackStore(dir)
		st, err := newUpdater(t, fresh, nil).Check(context.Background())
		if err != nil || st.Available || st.HoldVersion != latest || st.HoldExpiresMs == 0 {
			t.Fatalf("held status = %+v, err=%v", st, err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		dir := t.TempDir()
		past := time.Now().Add(-2 * time.Hour)
		raw := `{"schema":1,"history":[],"hold":{"version":"2.0.0","fromVersion":"1.0.0","reason":"test","createdAtMs":` +
			fmt.Sprint(past.Add(-time.Hour).UnixMilli()) + `,"expiresAtMs":` + fmt.Sprint(past.UnixMilli()) + `}}`
		_ = os.WriteFile(filepath.Join(dir, "update_rollback.json"), []byte(raw), 0o600)
		store, _ := newUpdateRollbackStore(dir)
		u := newUpdater(t, store, nil)
		u.restartGate = new(atomic.Bool)
		st, err := u.Check(context.Background())
		if err != nil || !st.Available || st.HoldVersion != "" || store.Snapshot().Hold != nil {
			t.Fatalf("expired status = %+v store=%+v err=%v", st, store.Snapshot(), err)
		}
		u.executable = func() (string, error) { return "", errors.New("later executable guard") }
		err = u.Apply(context.Background(), func(int) {}, true)
		var held *updateHeldError
		if errors.As(err, &held) || err == nil || !strings.Contains(err.Error(), "later executable guard") {
			t.Fatalf("expired hold still blocked apply: %v", err)
		}
	})

	t.Run("operator floor", func(t *testing.T) {
		store, _ := newUpdateRollbackStore(t.TempDir())
		_ = store.SetHold(rollbackHold{Version: latest}, time.Now())
		u := newUpdater(t, store, []byte(`{"schema":1,"min_daemon_version":"1.5.0"}`))
		u.restartGate = new(atomic.Bool)
		t.Setenv("INVOCATION_ID", "test-systemd")
		st, err := u.Check(context.Background())
		if err != nil || !st.Available || !st.Required || st.HoldVersion != "" {
			t.Fatalf("floor status = %+v, err=%v", st, err)
		}
		if store.Snapshot().Hold != nil || store.Snapshot().HoldReleaseReason != "operator minimum version floor" {
			t.Fatalf("floor did not durably release hold: %+v", store.Snapshot())
		}
		bin := filepath.Join(t.TempDir(), "solud")
		if err := os.WriteFile(bin, []byte("current"), 0o755); err != nil {
			t.Fatal(err)
		}
		u.executable = func() (string, error) { return bin, nil }
		u.commitSleepFn = func(time.Duration) {}
		err = u.Apply(context.Background(), func(int) {}, true)
		var held *updateHeldError
		if errors.As(err, &held) || err != nil {
			t.Fatalf("operator floor did not apply held release: %v", err)
		}
		if got, readErr := os.ReadFile(bin); readErr != nil || string(got) != string(candidate) {
			t.Fatalf("mandatory release was not installed: %q, %v", got, readErr)
		}
	})
}

func TestUpdateRollbackAPIBusyBodiesMatchForwardApply(t *testing.T) {
	u := NewUpdater("2.0.0", "", "", nil, nil)
	u.rollback, _ = newUpdateRollbackStore(t.TempDir())
	u.restartGate = new(atomic.Bool)
	u.busyCheck = func() *updateBusyError {
		return &updateBusyError{cause: updateBusyCauseCollaboration, reason: "pane busy", paneID: "p1"}
	}
	a := &apiServer{updater: u, rollback: u.rollback}

	forward := httptest.NewRecorder()
	a.updateApply(forward, httptest.NewRequest(http.MethodPost, "/api/update/apply", nil))
	rollback := httptest.NewRecorder()
	a.updateRollback(rollback, httptest.NewRequest(http.MethodPost, "/api/update/rollback",
		bytes.NewBufferString(`{"targetVersion":"1.0.0","confirm":true}`)))
	if forward.Code != http.StatusConflict || rollback.Code != http.StatusConflict || forward.Body.String() != rollback.Body.String() {
		t.Fatalf("busy bodies differ: forward=%d %s rollback=%d %s", forward.Code, forward.Body.String(), rollback.Code, rollback.Body.String())
	}
}

func TestUpdateRollbackAPIRequiresConfirmationAndReturnsTypedIncompatibility(t *testing.T) {
	u, store, _, _ := rollbackFixture(t, "2.0.0", "1.0.0", capabilityScript("1.0.0", `{"workflow":2}`))
	marker := filepath.Join(u.dataDir, "workflow", "version.json")
	_ = os.MkdirAll(filepath.Dir(marker), 0o700)
	_ = os.WriteFile(marker, []byte(`{"minReaderVersion":3}`), 0o600)
	a := &apiServer{updater: u, rollback: store}

	missingConfirm := httptest.NewRecorder()
	a.updateRollback(missingConfirm, httptest.NewRequest(http.MethodPost, "/api/update/rollback", bytes.NewBufferString(`{"targetVersion":"1.0.0"}`)))
	if missingConfirm.Code != http.StatusBadRequest {
		t.Fatalf("missing confirm status = %d", missingConfirm.Code)
	}

	declined := httptest.NewRecorder()
	a.updateRollback(declined, httptest.NewRequest(http.MethodPost, "/api/update/rollback", bytes.NewBufferString(`{"targetVersion":"1.0.0","confirm":true}`)))
	if declined.Code != http.StatusConflict || !strings.Contains(declined.Body.String(), `"code":"rollback_incompatible"`) || !strings.Contains(declined.Body.String(), `"store":"workflow"`) {
		t.Fatalf("decline = %d %s", declined.Code, declined.Body.String())
	}

	store.RecordRan("1.0.0", time.Now().Add(-time.Hour))
	store.RecordRan("2.0.0", time.Now())
	targets := httptest.NewRecorder()
	a.updateRollbackTargets(targets, httptest.NewRequest(http.MethodGet, "/api/update/rollback", nil))
	if targets.Code != http.StatusOK || !strings.Contains(targets.Body.String(), `"version":"1.0.0"`) || strings.Contains(targets.Body.String(), `"version":"2.0.0"`) {
		t.Fatalf("targets = %d %s", targets.Code, targets.Body.String())
	}
}

func TestHealthAdvertisesActiveRollbackHoldForWSLShellReuse(t *testing.T) {
	store, err := newUpdateRollbackStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetHold(rollbackHold{Version: "2.0.0"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	u := NewUpdater("1.0.0", "", "", nil, nil)
	u.rollback = store
	rec := httptest.NewRecorder()
	(&apiServer{ver: "1.0.0", updater: u, startT: time.Now()}).health(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["updateHoldVersion"] != "2.0.0" {
		t.Fatalf("health updateHoldVersion = %#v, want held bundled release", body["updateHoldVersion"])
	}
}
