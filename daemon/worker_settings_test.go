package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func setWorkerAccountsForTest(a *apiServer, scope string) {
	q := a.workers()
	q.mu.Lock()
	defer q.mu.Unlock()
	a.loadWorkerSettingsLocked(q)
	q.settings.WorkerAccounts = scope
}

func TestWorkerSettingsPaneCannotWidenOnLoopback(t *testing.T) {
	a := authorityTestServer(t)
	a.dataDir, a.metrics = t.TempDir(), NewMetrics()
	if err := a.agentAuthority.set(agentAuthorityRecord{Panes: true}); err != nil {
		t.Fatal(err)
	}
	pane := transactionTestPane("agent")
	pane.Pid = startSleeper(t).Process.Pid
	a.mgr.panes[pane.ID] = pane
	stubPeerPID(t, pane.Pid, true)
	mux := http.NewServeMux()
	a.routes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	put := func(body string, status int) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut, server.URL+"/api/worker-settings", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		// Browser headers and agent authority must never delegate this gate.
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != status {
			t.Fatalf("%s: status=%d want=%d", body, resp.StatusCode, status)
		}
		if status == http.StatusForbidden {
			var result map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result["error"] != "human_required" || result["caller"] != "pane" {
				t.Fatalf("refusal=%v err=%v", result, err)
			}
		}
	}
	for _, body := range []string{`{"maxRunning":32}`, `{"workerLeadReservePct":0}`, `{"workerAccounts":"saved"}`, `{"workerAccounts":"team"}`, `{"finishAfterMinutes":60}`, `{"finishAfterMinutes":1}`, `{"workerExpectedRunMinutes":120}`, `{"maxRunning":32,"workerAccounts":"active","workerAutoArchive":false}`} {
		put(body, http.StatusForbidden)
	}
	put(`{"workerAccounts":"invalid"}`, http.StatusBadRequest)
	q := a.workers()
	q.mu.Lock()
	if q.settings != defaultWorkerSettings() {
		t.Fatalf("refused update changed settings: %+v", q.settings)
	}
	q.mu.Unlock()
	put(`{"maxRunning":2,"workerLeadReservePct":30,"workerAutoArchive":false,"workerExpectedRunMinutes":20}`, http.StatusOK)
	put(`{"workerAutoArchive":true}`, http.StatusForbidden)
	// Previously authorized wider scopes can always be narrowed by a pane.
	setWorkerAccountsForTest(a, "team")
	put(`{"workerAccounts":"saved"}`, http.StatusOK)
	put(`{"workerAccounts":"active"}`, http.StatusOK)
	put(`{"workerAccounts":"saved"}`, http.StatusForbidden)
	reloaded := &apiServer{dataDir: a.dataDir}
	rq := reloaded.workers()
	rq.mu.Lock()
	reloaded.loadWorkerSettingsLocked(rq)
	if rq.settings.WorkerAccounts != "active" || rq.settings.MaxRunning != 2 || rq.settings.WorkerAutoArchive {
		t.Fatalf("persistence=%+v", rq.settings)
	}
	rq.mu.Unlock()
}

func TestWorkerSettingsHumanCanWiden(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(map[bool]string{false: "browser", true: "relay"}[relay], func(t *testing.T) {
			a := authorityTestServer(t)
			a.dataDir, a.metrics = t.TempDir(), NewMetrics()
			stubPeerPID(t, 0, false)
			mux := http.NewServeMux()
			a.routes(mux)
			req := httptest.NewRequest(http.MethodPut, "/api/worker-settings", strings.NewReader(`{"maxRunning":32,"workerLeadReservePct":0,"workerAccounts":"team"}`))
			if relay {
				req = req.WithContext(context.WithValue(req.Context(), relayTransportKey{}, true))
			} else {
				req.Header.Set("Sec-Fetch-Site", "same-origin")
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d %s", w.Code, w.Body.String())
			}
		})
	}
}
