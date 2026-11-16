package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"soluterminal-shared/machineauth"
)

// revokeFor builds a pending revoke aimed at srv for a fresh token.
func revokeFor(t *testing.T, srvURL, token, machine string) *pendingCloudRevoke {
	t.Helper()
	p := cloudRevokeFromClaim(&Claim{RelayURL: "ws" + strings.TrimPrefix(srvURL, "http") + "/relay", Token: token, MachineID: machine}, "", time.Now())
	if p == nil {
		t.Fatal("no revoke record")
	}
	return p
}

// Each answer of the cloud maps to one outcome: 2xx removed, 404 unlisted
// (never called a removal), other 4xx refused and recorded, 429/5xx and a
// network error pending and kept for retry.
func TestCloudRevokeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		want     cloudRevokeOutcome
		queued   bool
		recorded bool
	}{
		{"removed", http.StatusNoContent, revokeRemoved, false, false},
		{"unlisted", http.StatusNotFound, revokeUnlisted, false, false},
		{"refused 400", http.StatusBadRequest, revokeRefused, false, true},
		{"refused 413", http.StatusRequestEntityTooLarge, revokeRefused, false, true},
		{"busy 429", http.StatusTooManyRequests, revokePending, true, false},
		{"failing 503", http.StatusServiceUnavailable, revokePending, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud, srv := newCloudRecorder(t)
			cloud.status = func(string) int { return tc.status }
			dir := t.TempDir()
			r := newCloudRevoker(dir)
			const token = "tok_outcome_secret"
			p := revokeFor(t, srv.URL, token, "mach_o")
			if err := r.queue.add(*p); err != nil {
				t.Fatal(err)
			}
			if got := r.attempt(t.Context(), *p); got != tc.want {
				t.Fatalf("outcome = %q, want %q", got, tc.want)
			}
			if got := len(r.queue.list()) == 1; got != tc.queued {
				t.Fatalf("still queued = %v, want %v", got, tc.queued)
			}
			rec := readCloudRevokeRefused(dir)
			if (rec != nil) != tc.recorded {
				t.Fatalf("refusal recorded = %v, want %v", rec != nil, tc.recorded)
			}
			if rec != nil {
				raw, _ := os.ReadFile(dir + "/" + cloudRevokeRefusedFile)
				if strings.Contains(string(raw), token) || strings.Contains(string(raw), machineauth.RevokeKey(token)) {
					t.Fatalf("refusal record holds a credential: %s", raw)
				}
				if rec.CloudBase != p.CloudBase || rec.At == "" {
					t.Fatalf("refusal record = %+v", rec)
				}
			}
		})
	}
	t.Run("network error", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		r := newCloudRevoker(t.TempDir())
		p := revokeFor(t, url, "tok_net", "mach_n")
		_ = r.queue.add(*p)
		if got := r.attempt(t.Context(), *p); got != revokePending || len(r.queue.list()) != 1 {
			t.Fatalf("outcome = %q, queued = %d; want pending and kept", got, len(r.queue.list()))
		}
	})
}

func disconnectWithRevoker(t *testing.T, srvURL string) (*apiServer, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	if err := SaveClaim(dir, &Claim{RelayURL: "ws" + strings.TrimPrefix(srvURL, "http") + "/relay", Token: "tok_d", MachineID: "mach_d"}); err != nil {
		t.Fatal(err)
	}
	a := &apiServer{dataDir: dir, cloudRevoker: newCloudRevoker(dir)}
	rec := httptest.NewRecorder()
	a.handleCloudDisconnect(rec, httptest.NewRequest(http.MethodPost, "/api/cloud/disconnect", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if c, _ := loadClaimSafe(dir); c != nil {
		t.Fatal("claim (token) still on disk after Disconnect")
	}
	return a, body
}

// Disconnect says which outcome happened and where to check by hand.
func TestDisconnectReportsTheRevokeOutcome(t *testing.T) {
	for status, want := range map[int]cloudRevokeOutcome{
		http.StatusNoContent:  revokeRemoved,
		http.StatusNotFound:   revokeUnlisted,
		http.StatusBadRequest: revokeRefused,
	} {
		cloud, srv := newCloudRecorder(t)
		cloud.status = func(string) int { return status }
		_, body := disconnectWithRevoker(t, srv.URL)
		if body["cloudRemoval"] != string(want) {
			t.Fatalf("http %d: cloudRemoval = %q, want %q", status, body["cloudRemoval"], want)
		}
		if body["machinesUrl"] != srv.URL+"/" {
			t.Fatalf("machinesUrl = %q, want the revoke's own cloud %q", body["machinesUrl"], srv.URL+"/")
		}
	}
}

// A slow cloud doesn't hold Disconnect past the wait: the answer is pending,
// the token is already gone, and the revoke stays queued.
func TestDisconnectWaitIsBounded(t *testing.T) {
	old := cloudRevokeDisconnectWait
	cloudRevokeDisconnectWait = 200 * time.Millisecond
	defer func() { cloudRevokeDisconnectWait = old }()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	a, body := disconnectWithRevoker(t, srv.URL)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Disconnect took %s", took)
	}
	if body["cloudRemoval"] != string(revokePending) {
		t.Fatalf("cloudRemoval = %q, want pending", body["cloudRemoval"])
	}
	if len(a.cloudRevoker.queue.list()) != 1 {
		t.Fatal("pending revoke was not kept for retry")
	}
}

// Pending at Disconnect, refused later by the background sender: the status
// reports it (flag, time, the revoke's own cloud) while the computer isn't
// paired; the next Disconnect starts clean.
func TestRevokeRefusedLaterShowsInStatus(t *testing.T) {
	cloud, srv := newCloudRecorder(t)
	var refuse atomic.Bool
	cloud.status = func(string) int {
		if refuse.Load() {
			return http.StatusBadRequest
		}
		return http.StatusServiceUnavailable
	}
	a, body := disconnectWithRevoker(t, srv.URL)
	if body["cloudRemoval"] != string(revokePending) {
		t.Fatalf("cloudRemoval = %q, want pending", body["cloudRemoval"])
	}
	status := func() cloudStatusResponse {
		rec := httptest.NewRecorder()
		a.handleCloudStatus(rec, httptest.NewRequest(http.MethodGet, "/api/cloud/status", nil))
		var s cloudStatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(rec.Body.String(), "tok_d") || strings.Contains(rec.Body.String(), machineauth.RevokeKey("tok_d")) {
			t.Fatalf("status leaks a credential: %s", rec.Body.String())
		}
		return s
	}
	if s := status(); s.RevokeRefused != nil {
		t.Fatalf("refused shown before any refusal: %+v", s.RevokeRefused)
	}
	refuse.Store(true)
	a.cloudRevoker.flush(t.Context())
	s := status()
	if s.RevokeRefused == nil || !s.RevokeRefused.Refused || s.RevokeRefused.At == "" || s.RevokeRefused.MachinesURL != srv.URL+"/" {
		t.Fatalf("revokeRefused = %+v", s.RevokeRefused)
	}
	if len(a.cloudRevoker.queue.list()) != 0 {
		t.Fatal("refused revoke still queued")
	}
	// Pairing and disconnecting again starts clean.
	cloud.status = func(string) int { return http.StatusNoContent }
	if err := SaveClaim(a.dataDir, &Claim{RelayURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/relay", Token: "tok_e", MachineID: "mach_e"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.handleCloudDisconnect(rec, httptest.NewRequest(http.MethodPost, "/api/cloud/disconnect", nil))
	if s := status(); s.RevokeRefused != nil {
		t.Fatalf("old refusal shown after a later successful Disconnect: %+v", s.RevokeRefused)
	}
}

// With the background sender running, Disconnect's own attempt is the only
// send: the cloud sees one revoke and Disconnect reports "removed" (a second
// send would get 404 and read as "unlisted" after a real removal).
func TestDisconnectSendsOnceWithTheSenderRunning(t *testing.T) {
	cloud, srv := newCloudRecorder(t)
	var calls atomic.Int32
	cloud.status = func(string) int {
		if calls.Add(1) == 1 {
			time.Sleep(100 * time.Millisecond) // widen the window for a duplicate
			return http.StatusNoContent
		}
		return http.StatusNotFound
	}
	dir := t.TempDir()
	if err := SaveClaim(dir, &Claim{RelayURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/relay", Token: "tok_once", MachineID: "mach_once"}); err != nil {
		t.Fatal(err)
	}
	a := &apiServer{dataDir: dir, cloudRevoker: shrinkRevokeRetry(newCloudRevoker(dir))}
	go a.cloudRevoker.run(t.Context())
	// Let the sender's start-up flush (empty queue) finish first; a flush that
	// overlaps Disconnect makes it answer "pending", which is right then but
	// isn't what this test checks.
	time.Sleep(100 * time.Millisecond)
	rec := httptest.NewRecorder()
	a.handleCloudDisconnect(rec, httptest.NewRequest(http.MethodPost, "/api/cloud/disconnect", nil))
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["cloudRemoval"] != string(revokeRemoved) {
		t.Fatalf("cloudRemoval = %q, want removed", body["cloudRemoval"])
	}
	time.Sleep(200 * time.Millisecond) // give a stray background send its chance
	if n := cloud.count(machineauth.RevokePath); n != 1 {
		t.Fatalf("revoke requests = %d, want 1", n)
	}
}
