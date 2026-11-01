package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func archiveGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Worker Test", "-c", "user.email=worker@example.invalid"}, args...)...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func archiveRepo(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	archiveGit(t, cwd, "init", "--template=", "-b", "main")
	putCapabilityFile(t, filepath.Join(cwd, "tracked.txt"), "ready\n")
	archiveGit(t, cwd, "add", "tracked.txt")
	archiveGit(t, cwd, "commit", "-m", "fixture")
	remote := filepath.Join(t.TempDir(), "remote.git")
	archiveGit(t, cwd, "init", "--bare", "--template=", remote)
	archiveGit(t, cwd, "remote", "add", "origin", remote)
	archiveGit(t, cwd, "push", "-u", "origin", "main")
	return cwd
}
func archiveFixture(t *testing.T) (*apiServer, *Pane, time.Time) {
	t.Helper()
	_, _ = capabilityFixture(t)
	a, _ := newQuotaWorkerAPI(t)
	cwd := archiveRepo(t)
	out := workerCall(t, a, "normal")
	p, _ := a.mgr.Get(out["id"].(string))
	now := time.Now()
	q := a.workers()
	q.mu.Lock()
	q.startedAt = now.Add(-10 * time.Minute)
	q.settings.WorkerAutoArchive = true
	q.archiveEnabled.Store(true)
	r := q.requests[0]
	r.Info.State = "finished"
	r.Info.Provider = "claude"
	info := r.Info
	q.mu.Unlock()
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	raw := fmt.Sprintf("{\"type\":\"user\",\"promptSource\":\"typed\",\"timestamp\":%q,\"message\":{\"content\":\"task\"}}\n{\"type\":\"assistant\",\"timestamp\":%q,\"message\":{\"stop_reason\":\"end_turn\",\"content\":[{\"type\":\"text\",\"text\":\"Final answer\"}]}}\n", now.Add(-2*time.Minute).Format(time.RFC3339Nano), now.Add(-time.Minute).Format(time.RFC3339Nano))
	putCapabilityFile(t, transcript, raw)
	p.mu.Lock()
	p.Cwd = cwd
	p.Cmd = []string{"claude"}
	p.Agent = "claude"
	p.ClaudeSessionID = "00000000-0000-4000-8000-000000000001"
	p.ClaudeTranscriptPath = transcript
	o := p.Origin.clone()
	o.Worker = &info
	p.Origin = o
	p.mu.Unlock()
	return a, p, now
}
func TestWorkerArchiveEligibilityMatrix(t *testing.T) {
	for bits := 0; bits < 32; bits++ {
		finished, final, read, noInput, pushed := bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0, bits&16 != 0
		allow, flag := workerArchiveAllowed(finished, final, read, noInput, pushed)
		if allow != (bits == 31) || flag != (finished && final && noInput && (!read || !pushed)) {
			t.Fatalf("bits=%d allow=%v flag=%v", bits, allow, flag)
		}
	}
}

type archiveOrderingStore struct {
	failingPaneStore
	beforePut func(*Pane)
}

func (s *archiveOrderingStore) Put(p *Pane) error {
	s.beforePut(p)
	return s.failingPaneStore.Put(p)
}

func TestWorkerArchiveLifecyclePersistenceKeepsMembershipLease(t *testing.T) {
	a, p, now := archiveFixture(t)
	q := a.workers()
	q.mu.Lock()
	q.requests[0].Info.State = "running"
	q.mu.Unlock()
	p.mu.Lock()
	p.Status = PaneExited
	p.mu.Unlock()
	store := &archiveOrderingStore{beforePut: func(got *Pane) {
		if got != p {
			t.Fatal("unexpected pane")
		}
		// Archive removal takes the manager write lock. It must wait for
		// this save, so its subsequent Remove cannot be undone by the tick.
		if a.mgr.mu.TryLock() {
			a.mgr.mu.Unlock()
			t.Fatal("lifecycle save can race archive removal")
		}
	}}
	a.panes = store
	a.tickWorkers(now)
	if len(store.putIDsSnapshot()) != 1 {
		t.Fatal("lifecycle transition was not persisted")
	}
}
func TestWorkerArchiveKeepsBlockedWorkAndFlagsLead(t *testing.T) {
	for _, kind := range []string{"dirty", "unpushed", "unread", "wrong-reader", "input-after-final", "disabled"} {
		t.Run(kind, func(t *testing.T) {
			a, p, now := archiveFixture(t)
			if kind != "unread" {
				caller := "lead"
				if kind == "wrong-reader" {
					caller = "stranger"
				}
				a.noteWorkerReplyRead(caller, p, a.paneLatestReply(p))
			}
			switch kind {
			case "dirty":
				putCapabilityFile(t, filepath.Join(p.Cwd, "tracked.txt"), "changed")
			case "unpushed":
				putCapabilityFile(t, filepath.Join(p.Cwd, "tracked.txt"), "changed")
				archiveGit(t, p.Cwd, "commit", "-am", "not pushed")
			case "input-after-final":
				atomic.StoreInt64(&p.LastInputMs, now.UnixMilli())
				p.inputRevision.Add(1)
			case "disabled":
				q := a.workers()
				q.mu.Lock()
				q.settings.WorkerAutoArchive = false
				q.archiveEnabled.Store(false)
				q.mu.Unlock()
			}
			a.tryArchiveWorker(p, now)
			if _, ok := a.mgr.Get(p.ID); !ok {
				t.Fatal("blocked worker was closed")
			}
			if len(a.archivedWorkerSummaries()) != 0 {
				t.Fatal("blocked worker was archived")
			}
			lead, _ := a.mgr.Get("lead")
			wantFlag := kind != "input-after-final" && kind != "disabled"
			if lead.Snapshot().Flagged != wantFlag {
				t.Fatalf("lead flag=%v want %v", lead.Snapshot().Flagged, wantFlag)
			}
		})
	}
}
func TestWorkerArchiveReadReceiptsAndTombstones(t *testing.T) {
	for _, reader := range []string{"read_pane_tail", "wait_for_pane", "turn-wait"} {
		t.Run(reader, func(t *testing.T) {
			a, p, now := archiveFixture(t)
			stale, _ := a.ws.Get("Work")
			switch reader {
			case "read_pane_tail":
				text, bad := mcpResultText(t, a.mcpToolReadPaneTail(map[string]any{"pane_id": p.ID}, "lead"))
				if bad || !strings.Contains(text, "Final answer") {
					t.Fatal(text)
				}
			case "wait_for_pane":
				text, bad := mcpResultText(t, a.mcpToolWaitForPane(map[string]any{"pane_id": p.ID}, "lead"))
				if bad || !strings.Contains(text, "Final answer") {
					t.Fatal(text)
				}
			case "turn-wait":
				res := a.waitForPaneTurn(p, paneTurnBaseline{Since: now.Add(-3 * time.Minute)}, time.Second, "lead")
				if res.Status != "replied" {
					t.Fatalf("wait: %+v", res)
				}
			}
			a.tryArchiveWorker(p, now)
			if _, ok := a.mgr.Get(p.ID); ok {
				t.Fatal("archived pane still live")
			}
			rows := a.archivedWorkerSummaries()
			if len(rows) != 1 || rows[0].Origin.LeadPaneID != "lead" || rows[0].Worker.ArchiveID == "" {
				t.Fatalf("archive not listed under lead: %+v", rows)
			}
			saved, _ := a.ws.Get("Work")
			if mcpWorkspaceHasPaneID(saved.PaneIds, p.ID) || !mcpWorkspaceHasPaneID(saved.RemovedPaneIDs, p.ID) {
				t.Fatal("archive did not author tombstone")
			}
			_ = a.ws.Put(stale)
			saved, _ = a.ws.Get("Work")
			if mcpWorkspaceHasPaneID(saved.PaneIds, p.ID) {
				t.Fatal("stale save resurrected archived pane")
			}
			text, ok := a.archivedWorkerReply(p.ID)
			if !ok || text != "Final answer" {
				t.Fatal("archive reply not retained")
			}
			if raw, err := os.ReadFile(filepath.Join(a.archiveRoot(), rows[0].Worker.ArchiveID, "transcript.jsonl")); err != nil || !strings.Contains(string(raw), "Final answer") {
				t.Fatalf("transcript missing: %v", err)
			}
			// Input against a captured reference cannot revive an archived process.
			if _, err := p.Write([]byte("new work\r")); err == nil {
				t.Fatal("archived process accepted input")
			}
		})
	}
}
func TestWorkerArchiveExpiryPinAndReopen(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	a.tryArchiveWorker(p, now)
	rows := a.archivedWorkerSummaries()
	if len(rows) != 1 {
		t.Fatal("not archived")
	}
	id := rows[0].Worker.ArchiveID
	s := a.workerArchives()
	s.mu.Lock()
	next := append([]workerArchiveEntry(nil), s.entries...)
	next[0].Pinned = true
	if err := a.saveWorkerArchivesLocked(s, next); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	a.expireWorkerArchives(now.Add(25 * time.Hour))
	if len(a.archivedWorkerSummaries()) != 1 {
		t.Fatal("pinned archive expired")
	}
	reloaded := &apiServer{dataDir: a.dataDir, mgr: a.mgr}
	if len(reloaded.archivedWorkerSummaries()) != 1 {
		t.Fatal("archive did not survive restart")
	}
	req := httptest.NewRequest("POST", "/api/worker-archives/"+id+"/reopen", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	a.reopenWorkerArchive(w, req)
	if w.Code != 201 {
		t.Fatalf("reopen %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		ID     string      `json:"id"`
		Origin *PaneOrigin `json:"origin"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID == p.ID || body.Origin == nil || body.Origin.LeadPaneID != "lead" || !body.Origin.Worker.Pinned {
		t.Fatalf("reopen lost identity/provenance: %+v", body)
	}
	if len(a.archivedWorkerSummaries()) != 0 {
		t.Fatal("reopen left duplicate archive")
	}
	saved, _ := a.ws.Get("Work")
	if !mcpWorkspaceHasPaneID(saved.PaneIds, body.ID) || mcpWorkspaceHasPaneID(saved.PaneIds, p.ID) {
		t.Fatal("reopen used tombstoned id")
	}
}
func TestWorkerArchiveUnpinnedExpires(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	a.tryArchiveWorker(p, now)
	rows := a.archivedWorkerSummaries()
	if len(rows) != 1 {
		t.Fatal("not archived")
	}
	id := rows[0].Worker.ArchiveID
	a.expireWorkerArchives(now.Add(24 * time.Hour))
	if len(a.archivedWorkerSummaries()) != 0 {
		t.Fatal("expired archive retained")
	}
	if _, err := os.Stat(filepath.Join(a.archiveRoot(), id)); !os.IsNotExist(err) {
		t.Fatal("expired transcript copy retained")
	}
	if _, err := os.Stat(p.Snapshot().ClaudeTranscriptPath); err != nil {
		t.Fatal("archive expiry deleted provider-owned history")
	}
}
func TestWorkerArchiveGitProof(t *testing.T) {
	cwd := archiveRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, reason := workerGitCleanPushed(ctx, cwd); reason != "" {
		t.Fatal(reason)
	}
	archiveGit(t, cwd, "checkout", "--detach")
	if _, reason := workerGitCleanPushed(ctx, cwd); reason == "" {
		t.Fatal("detached worktree treated as pushed")
	}
}

func TestWorkerArchiveInputOrReplacementDuringPreparationAborts(t *testing.T) {
	for _, kind := range []string{"input", "replacement"} {
		t.Run(kind, func(t *testing.T) {
			a, p, now := archiveFixture(t)
			a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
			s := a.workerArchives()
			s.beforeCommit = func() {
				if kind == "input" {
					p.inputRevision.Add(1)
					atomic.StoreInt64(&p.LastInputMs, time.Now().UnixMilli())
				} else {
					replacement := transactionTestPane(p.ID)
					replacement.Title = "replacement"
					a.mgr.mu.Lock()
					a.mgr.panes[p.ID] = replacement
					a.mgr.mu.Unlock()
				}
			}
			a.tryArchiveWorker(p, now)
			current, ok := a.mgr.Get(p.ID)
			if !ok || (kind == "replacement" && current.Title != "replacement") {
				t.Fatal("preparation race closed the current process")
			}
			if len(a.archivedWorkerSummaries()) != 0 {
				t.Fatal("aborted preparation created archive")
			}
			saved, _ := a.ws.Get("Work")
			if mcpWorkspaceHasPaneID(saved.RemovedPaneIDs, p.ID) {
				t.Fatal("aborted preparation created tombstone")
			}
		})
	}
}
func TestWorkerArchiveRequiresCurrentFinalTurn(t *testing.T) {
	for _, kind := range []string{"new-final", "missing-time", "previous-daemon"} {
		t.Run(kind, func(t *testing.T) {
			a, p, now := archiveFixture(t)
			a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
			path := p.Snapshot().ClaudeTranscriptPath
			switch kind {
			case "new-final":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw = append(raw, []byte(fmt.Sprintf("{\"type\":\"assistant\",\"timestamp\":%q,\"message\":{\"stop_reason\":\"end_turn\",\"content\":[{\"type\":\"text\",\"text\":\"New final\"}]}}\n", now.Add(-30*time.Second).Format(time.RFC3339Nano)))...)
				putCapabilityFile(t, path, string(raw))
			case "missing-time":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				putCapabilityFile(t, path, strings.ReplaceAll(string(raw), now.Add(-time.Minute).Format(time.RFC3339Nano), ""))
			case "previous-daemon":
				q := a.workers()
				q.mu.Lock()
				q.startedAt = now
				q.mu.Unlock()
			}
			a.tryArchiveWorker(p, now)
			if _, ok := a.mgr.Get(p.ID); !ok {
				t.Fatal("missing/current-turn proof closed worker")
			}
			if len(a.archivedWorkerSummaries()) != 0 {
				t.Fatal("stale receipt archived worker")
			}
		})
	}
}

func TestWorkerArchiveShutdownFailureRetainsLiveLayout(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	s := a.workerArchives()
	s.teardown = func(*Pane) error { return fmt.Errorf("shutdown failed") }
	a.tryArchiveWorker(p, now)
	if _, ok := a.mgr.Get(p.ID); !ok || p.workerArchiving.Load() {
		t.Fatal("failed shutdown left worker detached or input blocked")
	}
	saved, _ := a.ws.Get("Work")
	if !mcpWorkspaceHasPaneID(saved.PaneIds, p.ID) || mcpWorkspaceHasPaneID(saved.RemovedPaneIDs, p.ID) {
		t.Fatal("failed archive did not compensate its own tombstone")
	}
	if len(a.archivedWorkerSummaries()) != 0 {
		t.Fatal("failed archive left duplicate record")
	}
}

func TestWorkerArchivePinHTTPAndDisablePersistence(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	a.tryArchiveWorker(p, now)
	rows := a.archivedWorkerSummaries()
	if len(rows) != 1 {
		t.Fatal("not archived")
	}
	id := rows[0].Worker.ArchiveID
	mux := http.NewServeMux()
	a.registerWorkerArchiveRoutes(mux)
	for _, body := range []string{`{}`, `{"pinned":"yes"}`} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("PATCH", "/api/worker-archives/"+id, strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("invalid pin accepted: %d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("PATCH", "/api/worker-archives/"+id, strings.NewReader(`{"pinned":true}`)))
	if w.Code != 200 {
		t.Fatalf("pin: %d %s", w.Code, w.Body.String())
	}
	a.expireWorkerArchives(now.Add(25 * time.Hour))
	rows = a.archivedWorkerSummaries()
	if len(rows) != 1 || !rows[0].Worker.Pinned || !rows[0].Origin.Worker.Pinned {
		t.Fatal("pin was not retained in archive views")
	}
	settings := defaultWorkerSettings()
	off := false
	settings, changed := (workerSettingsPatch{WorkerAutoArchive: &off}).apply(settings)
	if !changed || settings.WorkerAutoArchive {
		t.Fatal("archive opt-out ignored")
	}
	raw, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(a.dataDir, "worker-settings.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	other := &apiServer{dataDir: a.dataDir}
	q := other.workers()
	q.mu.Lock()
	other.loadWorkerSettingsLocked(q)
	on := q.settings.WorkerAutoArchive
	q.mu.Unlock()
	if on {
		t.Fatal("archive opt-out did not survive reload")
	}
}
func TestWorkerArchiveCancelledDaemonDoesNotClose(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	s := a.workerArchives()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.context = ctx
	s.beforeCommit = cancel
	a.tryArchiveWorker(p, now)
	if _, ok := a.mgr.Get(p.ID); !ok || p.workerArchiving.Load() {
		t.Fatal("cancelled daemon archived worker")
	}
	if len(a.archivedWorkerSummaries()) != 0 {
		t.Fatal("cancelled archive left durable entry")
	}
}

func TestWorkerArchiveExpiryDoesNotReconcileDuringTeardown(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	s := a.workerArchives()
	s.operation.Lock()
	defer s.operation.Unlock()
	s.teardown = func(p *Pane) error {
		// Simulate the absence between exact-pointer detach and a failed
		// shutdown restoring membership. A concurrent list must not remove
		// the live pane's persisted record in that interval.
		a.mgr.mu.Lock()
		delete(a.mgr.panes, p.ID)
		a.mgr.mu.Unlock()
		a.expireWorkerArchives(now)
		store := a.panes.(*failingPaneStore)
		if len(store.removedSnapshot()) != 0 {
			t.Fatal("expiry removed a pane during teardown")
		}
		a.mgr.mu.Lock()
		a.mgr.panes[p.ID] = p
		a.mgr.mu.Unlock()
		return fmt.Errorf("shutdown failed")
	}
	a.tryArchiveWorker(p, now)
	if _, ok := a.mgr.Get(p.ID); !ok || p.workerArchiving.Load() {
		t.Fatal("failed archive did not restore the worker")
	}
}

func TestWorkerArchiveGitCannotExecuteDrivers(t *testing.T) {
	cwd := archiveRepo(t)
	marker := filepath.Join(t.TempDir(), "executed")
	payload := "touch '" + marker + "'"
	putCapabilityFile(t, filepath.Join(cwd, ".gitattributes"), "tracked.txt filter=evil diff=evil\n")
	archiveGit(t, cwd, "add", ".gitattributes")
	archiveGit(t, cwd, "commit", "-m", "attributes")
	archiveGit(t, cwd, "push")
	for _, key := range []string{"filter.evil.clean", "filter.evil.smudge", "filter.evil.process", "diff.evil.textconv", "core.fsmonitor"} {
		archiveGit(t, cwd, "config", key, payload)
	}
	archiveGit(t, cwd, "config", "filter.evil.required", "true")
	// Force rechecking contents rather than trusting a cached index stat.
	now := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(cwd, "tracked.txt"), now, now); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	putCapabilityFile(t, global, "[core]\nfsmonitor = "+payload+"\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", payload)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, reason := workerGitCleanPushed(ctx, cwd); reason != "" {
		t.Fatalf("clean worktree: %s", reason)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("worker-controlled Git driver executed")
	}
	putCapabilityFile(t, filepath.Join(cwd, "tracked.txt"), "changed\n")
	if _, reason := workerGitCleanPushed(ctx, cwd); reason != "uncommitted work" {
		t.Fatalf("dirty check: %s", reason)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("dirty check executed driver")
	}
}

func TestWorkerArchiveLegacyProcessRetainsLease(t *testing.T) {
	a, p, now := archiveFixture(t)
	p.mu.Lock()
	p.Pid = 123
	p.mu.Unlock()
	released := false
	q := a.workers()
	q.mu.Lock()
	q.requests[0].release = func() { released = true }
	q.mu.Unlock()
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	a.tryArchiveWorker(p, now)
	if released {
		t.Fatal("legacy archive released credentials without descendant shutdown proof")
	}
	if live, ok := a.mgr.Get(p.ID); !ok || live != p {
		t.Fatal("legacy pane was removed")
	}
	ws, _ := a.ws.Get("Work")
	if !mcpWorkspaceHasPaneID(ws.PaneIds, p.ID) {
		t.Fatal("legacy pane layout was not restored")
	}
}

func TestWorkerArchiveAgentCannotReopenOrUnpin(t *testing.T) {
	a, p, now := archiveFixture(t)
	a.noteWorkerReplyRead("lead", p, a.paneLatestReply(p))
	a.tryArchiveWorker(p, now)
	rows := a.archivedWorkerSummaries()
	if len(rows) != 1 {
		t.Fatal("not archived")
	}
	id := rows[0].Worker.ArchiveID
	state := a.workerArchives()
	state.mu.Lock()
	entries := append([]workerArchiveEntry(nil), state.entries...)
	entries[0].Pinned = true
	if err := a.saveWorkerArchivesLocked(state, entries); err != nil {
		t.Fatal(err)
	}
	state.mu.Unlock()
	_ = authorityTestServer(t) // Use real peer classification, not the suite's human hook.
	caller := transactionTestPane("agent-caller")
	caller.Pid = startSleeper(t).Process.Pid
	a.mgr.panes[caller.ID] = caller
	stubPeerPID(t, caller.Pid, true)
	mux := http.NewServeMux()
	a.registerWorkerArchiveRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	for _, route := range []struct{ method, path, body string }{{"POST", "/api/worker-archives/" + id + "/reopen", `{}`}, {"PATCH", "/api/worker-archives/" + id, `{"pinned":false}`}, {"PATCH", "/api/worker-archives/" + id, `{"pinned":true}`}} {
		req, err := http.NewRequest(route.method, server.URL+route.path, strings.NewReader(route.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 403 || body["error"] != "human_required" {
			t.Fatalf("agent archive mutation: status=%d body=%v err=%v", resp.StatusCode, body, err)
		}
	}
	rows = a.archivedWorkerSummaries()
	if len(rows) != 1 || !rows[0].Worker.Pinned {
		t.Fatal("refused mutation changed archive")
	}
}
