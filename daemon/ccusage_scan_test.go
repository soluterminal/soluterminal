package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Extraction ---------------------------------------------------------

// usageJSONL builds one transcript line. Helper keeps the fixtures readable.
func usageJSONL(t *testing.T, typ, ts, sessionID, reqID, msgID, model string, in, out, cacheRead, cacheCreate int, isSidechain bool) string {
	t.Helper()
	line := map[string]any{
		"type":        typ,
		"timestamp":   ts,
		"sessionId":   sessionID,
		"requestId":   reqID,
		"version":     "1.0.131",
		"isSidechain": isSidechain,
		"message": map[string]any{
			"id":    msgID,
			"model": model,
			"usage": map[string]any{
				"input_tokens":                in,
				"output_tokens":               out,
				"cache_read_input_tokens":     cacheRead,
				"cache_creation_input_tokens": cacheCreate,
				"cache_creation": map[string]any{
					"ephemeral_5m_input_tokens": cacheCreate,
					"ephemeral_1h_input_tokens": 0,
				},
				"service_tier": "standard",
			},
		},
	}
	b, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(b)
}

func usageJSONLWithCacheSplit(t *testing.T, ts, sessionID, reqID, msgID string, cacheCreate, cacheCreate5m, cacheCreate1h int) string {
	t.Helper()
	line := map[string]any{
		"type": "assistant", "timestamp": ts, "sessionId": sessionID, "requestId": reqID,
		"message": map[string]any{
			"id": msgID, "model": "claude-sonnet-4-6",
			"usage": map[string]any{
				"input_tokens": 3, "output_tokens": 5, "cache_read_input_tokens": 7,
				"cache_creation_input_tokens": cacheCreate,
				"cache_creation": map[string]any{
					"ephemeral_5m_input_tokens": cacheCreate5m,
					"ephemeral_1h_input_tokens": cacheCreate1h,
				},
			},
		},
	}
	b, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExtractUsageLines_BasicAndTotal(t *testing.T) {
	line := usageJSONL(t, "assistant", "2026-06-17T11:01:05.277Z", "sess-1", "req-1", "msg-1", "claude-opus-4-8", 100, 50, 200, 10, false)
	buf := []byte(line + "\n")

	recs, consumed := extractUsageLines(buf)
	if consumed != len(buf) {
		t.Fatalf("consumed=%d want=%d (complete line should be fully consumed)", consumed, len(buf))
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.SessionID != "sess-1" || r.RequestID != "req-1" || r.MessageID != "msg-1" {
		t.Errorf("identity wrong: %+v", r)
	}
	if want := 100 + 50 + 200 + 10; r.TotalTokens != want {
		t.Errorf("TotalTokens=%d want=%d", r.TotalTokens, want)
	}
	if r.CacheCreate5mTokens != 10 {
		t.Errorf("CacheCreate5mTokens=%d want=10", r.CacheCreate5mTokens)
	}
	if r.Model != "claude-opus-4-8" || r.ServiceTier != "standard" {
		t.Errorf("model/tier wrong: %+v", r)
	}
}

func TestExtractUsageLines_CacheWriteClassesRemainIndependent(t *testing.T) {
	line := usageJSONLWithCacheSplit(t, "2026-09-03T01:00:00Z", "sess", "req", "msg", 30, 10, 20)
	recs, _ := extractUsageLines([]byte(line + "\n"))
	if len(recs) != 1 {
		t.Fatalf("records = %#v", recs)
	}
	rec := recs[0]
	if rec.CacheCreateTokens != 30 || rec.CacheCreate5mTokens != 10 || rec.CacheCreate1hTokens != 20 {
		t.Fatalf("cache write split = aggregate %d 5m %d 1h %d", rec.CacheCreateTokens, rec.CacheCreate5mTokens, rec.CacheCreate1hTokens)
	}
	if rec.TotalTokens != 45 {
		t.Fatalf("total tokens = %d, want 3+5+7+30 (split is not double-counted)", rec.TotalTokens)
	}
}

func TestExtractUsageLines_SkipsSynthetic(t *testing.T) {
	synthetic := usageJSONL(t, "assistant", "2026-06-17T11:01:05.277Z", "sess-1", "req-x", "msg-x", "<synthetic>", 0, 0, 0, 0, false)
	good := usageJSONL(t, "assistant", "2026-06-17T11:02:05.277Z", "sess-1", "req-y", "msg-y", "claude-opus-4-8", 5, 5, 0, 0, false)
	buf := []byte(synthetic + "\n" + good + "\n")

	recs, _ := extractUsageLines(buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (synthetic should be skipped)", len(recs))
	}
	if recs[0].MessageID != "msg-y" {
		t.Errorf("kept the wrong record: %+v", recs[0])
	}
}

// Fix H1: a billed turn missing requestId (and/or message.id) must be KEPT
// with a deterministic synthesized surrogate key, not dropped.
func TestExtractUsageLines_SynthesizesSurrogateKeys(t *testing.T) {
	// Line missing requestId AND message.id entirely. Built by hand so the
	// fields are truly absent (the usageJSONL helper always sets them).
	line := `{"type":"assistant","timestamp":"2026-06-17T11:01:05.277Z","sessionId":"sess-1","version":"1.0.131","message":{"model":"claude-opus-4-8","usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`
	buf := []byte(line + "\n")

	recs, _ := extractUsageLines(buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (missing-id turn must be kept, not dropped)", len(recs))
	}
	r := recs[0]
	wantMsg := "synmsg:sess-1:2026-06-17T11:01:05.277Z"
	if r.MessageID != wantMsg {
		t.Errorf("MessageID=%q want synthesized %q", r.MessageID, wantMsg)
	}
	wantReq := "synreq:" + wantMsg
	if r.RequestID != wantReq {
		t.Errorf("RequestID=%q want synthesized %q", r.RequestID, wantReq)
	}
	if r.TotalTokens != 10 {
		t.Errorf("TotalTokens=%d want 10 (tokens still counted)", r.TotalTokens)
	}

	// Determinism: re-extracting the same bytes yields the same keys, so the
	// daemon's dedup and the cloud's keep-max agree across re-scans.
	recs2, _ := extractUsageLines(buf)
	if recs2[0].MessageID != r.MessageID || recs2[0].RequestID != r.RequestID {
		t.Errorf("surrogate keys not deterministic: %q/%q vs %q/%q",
			recs2[0].MessageID, recs2[0].RequestID, r.MessageID, r.RequestID)
	}
}

// Fix H1: a turn that has a message.id but is missing only requestId gets a
// requestId surrogate derived from the (real) message.id.
func TestExtractUsageLines_SurrogateRequestIDOnly(t *testing.T) {
	line := `{"type":"assistant","timestamp":"2026-06-17T11:01:05.277Z","sessionId":"sess-9","version":"1.0.131","message":{"id":"msg-real","model":"claude-opus-4-8","usage":{"input_tokens":1,"output_tokens":1}}}`
	recs, _ := extractUsageLines([]byte(line + "\n"))
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].MessageID != "msg-real" {
		t.Errorf("MessageID=%q want real msg-real (only requestId was missing)", recs[0].MessageID)
	}
	if recs[0].RequestID != "synreq:msg-real" {
		t.Errorf("RequestID=%q want synreq:msg-real", recs[0].RequestID)
	}
}

// Fix L1: a billed turn with a NON-semver version string must still be kept
// (version is no longer a hard reject) with the raw version captured.
func TestExtractUsageLines_NonSemverVersionKept(t *testing.T) {
	line := `{"type":"assistant","timestamp":"2026-06-17T11:01:05.277Z","sessionId":"s","requestId":"r","version":"future-2026","message":{"id":"m","model":"claude-opus-4-8","usage":{"input_tokens":2,"output_tokens":2}}}`
	recs, _ := extractUsageLines([]byte(line + "\n"))
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (non-semver version must not zero out usage)", len(recs))
	}
	if recs[0].CCVersion != "future-2026" {
		t.Errorf("CCVersion=%q want raw future-2026", recs[0].CCVersion)
	}
	if recs[0].TotalTokens != 4 {
		t.Errorf("TotalTokens=%d want 4", recs[0].TotalTokens)
	}
}

func TestExtractUsageLines_SkipsNonAssistantAndNoUsage(t *testing.T) {
	// A user line (has no usage) — also fails the prefilter.
	userLine := `{"type":"user","timestamp":"2026-06-17T11:00:00Z","message":{"role":"user"}}`
	// A "tool_result" line that happens to contain the word usage in content
	// but is not type=assistant.
	toolLine := `{"type":"tool_result","timestamp":"2026-06-17T11:00:01Z","message":{"usage":null}}`
	good := usageJSONL(t, "assistant", "2026-06-17T11:02:05.277Z", "sess-1", "req-1", "msg-1", "claude-opus-4-8", 5, 5, 0, 0, false)
	buf := []byte(userLine + "\n" + toolLine + "\n" + good + "\n")

	recs, _ := extractUsageLines(buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
}

func TestExtractUsageLines_PartialTrailingNotConsumed(t *testing.T) {
	good := usageJSONL(t, "assistant", "2026-06-17T11:01:05.277Z", "sess-1", "req-1", "msg-1", "claude-opus-4-8", 1, 1, 0, 0, false)
	// A complete line, then a partial line with NO trailing newline (Claude
	// mid-append). The partial must not be consumed.
	partial := `{"type":"assistant","timestamp":"2026-06-17T11:02","message":{"usage":{"inp`
	buf := []byte(good + "\n" + partial)

	recs, consumed := extractUsageLines(buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (only the complete line)", len(recs))
	}
	wantConsumed := len(good) + 1 // up to and including the newline
	if consumed != wantConsumed {
		t.Fatalf("consumed=%d want=%d (trailing partial must be left)", consumed, wantConsumed)
	}
	if consumed >= len(buf) {
		t.Fatalf("consumed (%d) should be < len(buf) (%d) — partial left for carry", consumed, len(buf))
	}
}

func TestDedupKeepMax_StreamingGrowingOutput(t *testing.T) {
	// Same (msg,req) written 3× as the turn streams, output growing.
	recs := []ccUsageRecord{
		{MessageID: "msg-1", RequestID: "req-1", InputTokens: 100, OutputTokens: 10, TotalTokens: 110},
		{MessageID: "msg-1", RequestID: "req-1", InputTokens: 100, OutputTokens: 40, TotalTokens: 140},
		{MessageID: "msg-1", RequestID: "req-1", InputTokens: 100, OutputTokens: 90, TotalTokens: 190},
		// A different turn, untouched.
		{MessageID: "msg-2", RequestID: "req-2", TotalTokens: 5},
	}
	out := dedupKeepMax(recs)
	if len(out) != 2 {
		t.Fatalf("got %d records, want 2", len(out))
	}
	// Order preserved by first appearance; the (msg-1,req-1) slot keeps max.
	if out[0].MessageID != "msg-1" || out[0].TotalTokens != 190 {
		t.Errorf("keep-max failed: %+v", out[0])
	}
	if out[1].MessageID != "msg-2" || out[1].TotalTokens != 5 {
		t.Errorf("second record wrong: %+v", out[1])
	}
}

func TestDedupKeepMax_PreservesWorkflowAndLongGapTelemetry(t *testing.T) {
	recs := []ccUsageRecord{
		{MessageID: "msg", RequestID: "req", TotalTokens: 20, WorkflowInvocationID: "wfi_1", WorkflowRole: "reviewer", CacheTTLClass: "5m", CacheTTLControl: "applied", LongGapObserved: true},
		{MessageID: "msg", RequestID: "req", TotalTokens: 40, WorkflowInvocationID: "wfi_2", WorkflowRole: "refuter", CacheTTLClass: "1h", CacheTTLControl: "unsupported"},
	}
	out := dedupKeepMax(recs)
	if len(out) != 1 || out[0].TotalTokens != 40 {
		t.Fatalf("keep-max = %#v", out)
	}
	if out[0].WorkflowInvocationID != "wfi_1" || out[0].WorkflowRole != "reviewer" || out[0].CacheTTLClass != "5m" || out[0].CacheTTLControl != "applied" || !out[0].LongGapObserved {
		t.Fatalf("telemetry was discarded by larger duplicate: %#v", out[0])
	}
}

// --- Discovery ----------------------------------------------------------

func TestClaudeConfigDirs_FromEnv(t *testing.T) {
	tmp := t.TempDir()
	// Build <tmp>/projects so the dir qualifies.
	if err := os.MkdirAll(filepath.Join(tmp, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A second candidate WITHOUT projects/ should be dropped.
	noProjects := t.TempDir()

	// claudeConfigDirs resolves symlinks (fix M3), so compare against the
	// resolved tmp (e.g. macOS /var/folders, or any /tmp symlink).
	wantTmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		wantTmp = tmp
	}

	t.Setenv("CLAUDE_CONFIG_DIR", tmp+","+noProjects)
	dirs := claudeConfigDirs()
	if len(dirs) != 1 || dirs[0] != wantTmp {
		t.Fatalf("claudeConfigDirs()=%v want [%s]", dirs, wantTmp)
	}
}

func TestClaudeConfigDirs_EnvEndingInProjects(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	wantTmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		wantTmp = tmp
	}
	// Point the env directly at the projects/ child — resolution should use
	// the parent.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "projects"))
	dirs := claudeConfigDirs()
	if len(dirs) != 1 || dirs[0] != wantTmp {
		t.Fatalf("claudeConfigDirs()=%v want [%s]", dirs, wantTmp)
	}
}

func TestListTranscripts_RecursesIntoSubagents(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "projects", "enc-cwd")
	subDir := filepath.Join(proj, "parent-sess", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainFile := filepath.Join(proj, "sess-1.jsonl")
	subFile := filepath.Join(subDir, "agent-abc.jsonl")
	nonJSONL := filepath.Join(proj, "notes.txt")
	for _, f := range []string{mainFile, subFile, nonJSONL} {
		if err := os.WriteFile(f, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got := listTranscripts([]string{root})
	want := map[string]bool{mainFile: false, subFile: false}
	for _, g := range got {
		if _, ok := want[g]; ok {
			want[g] = true
		} else if g == nonJSONL {
			t.Errorf("non-jsonl file should not be listed: %s", g)
		}
	}
	for f, found := range want {
		if !found {
			t.Errorf("expected transcript not listed: %s", f)
		}
	}
}

func TestSessionIDForTranscript(t *testing.T) {
	normal := filepath.Join("/x/projects/enc", "sess-abc.jsonl")
	if got := sessionIDForTranscript(normal); got != "sess-abc" {
		t.Errorf("normal: got %q want sess-abc", got)
	}
	sub := filepath.Join("/x/projects/enc", "parent-sess", "subagents", "agent-1.jsonl")
	if got := sessionIDForTranscript(sub); got != "parent-sess" {
		t.Errorf("subagent: got %q want parent-sess", got)
	}
	if !isSubagentTranscript(sub) {
		t.Errorf("isSubagentTranscript should be true for %s", sub)
	}
	if isSubagentTranscript(normal) {
		t.Errorf("isSubagentTranscript should be false for %s", normal)
	}
}

// --- Subagent tagging via scanOnce -------------------------------------

// captureServer is an httptest server that records the cc-usage records it
// receives and returns a configurable status code.
type captureServer struct {
	mu      sync.Mutex
	records []ccUsageRecord
	status  int
	calls   int
}

type captureRoundTripper struct{ capture *captureServer }

func (t captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.capture.handler(recorder, req)
	return recorder.Result(), nil
}

func captureClient(c *captureServer) *http.Client {
	return &http.Client{Transport: captureRoundTripper{capture: c}}
}

func (c *captureServer) handler(w http.ResponseWriter, r *http.Request) {
	var batch ccUsageBatch
	_ = json.NewDecoder(r.Body).Decode(&batch)
	c.mu.Lock()
	c.records = append(c.records, batch.Records...)
	c.calls++
	status := c.status
	c.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

func (c *captureServer) setStatus(s int) { c.mu.Lock(); c.status = s; c.mu.Unlock() }
func (c *captureServer) snapshot() ([]ccUsageRecord, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]ccUsageRecord(nil), c.records...)
	return out, c.calls
}

// writeTranscript writes lines (each already a JSON object) joined by '\n'
// with a trailing newline.
func writeTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf []byte
	for _, l := range lines {
		buf = append(buf, l...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScanOnce_SubagentTaggingAndParentInApp(t *testing.T) {
	cfgRoot := t.TempDir()
	dataDir := t.TempDir()

	enc := filepath.Join(cfgRoot, "projects", "enc-cwd")
	parentFile := filepath.Join(enc, "parent-sess.jsonl")
	subFile := filepath.Join(enc, "parent-sess", "subagents", "agent-1.jsonl")

	parentLine := usageJSONL(t, "assistant", "2026-06-17T11:01:00Z", "parent-sess", "req-p", "msg-p", "claude-opus-4-8", 10, 20, 0, 0, false)
	// Subagent line: its own isSidechain is false on disk, but the scanner
	// must force IsSidechain=true from the path.
	subLine := usageJSONL(t, "assistant", "2026-06-17T11:02:00Z", "child-sess", "req-c", "msg-c", "claude-opus-4-8", 5, 5, 0, 0, false)
	writeTranscript(t, parentFile, parentLine)
	writeTranscript(t, subFile, subLine)

	t.Setenv("CLAUDE_CONFIG_DIR", cfgRoot)

	// Ledger knows the PARENT session is in-app; the child session id is
	// never recorded (subagents don't fire hooks under their own id).
	ledger := newInAppLedger(dataDir)
	ledger.Record("parent-sess", true)
	ledger.RecordWorkflow("parent-sess", "wfi_parent", "reviewer:spec_compliance", "5m", "applied")

	cap := &captureServer{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	cursors := newCursorStore(dataDir)
	if err := cursors.Load(); err != nil {
		t.Fatal(err)
	}
	scanOnce(context.Background(), cursors, ledger, srv.Client(), srv.URL, "tok", nil)

	recs, _ := cap.snapshot()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	byMsg := map[string]ccUsageRecord{}
	for _, r := range recs {
		byMsg[r.MessageID] = r
	}
	parent, ok := byMsg["msg-p"]
	if !ok {
		t.Fatal("parent record missing")
	}
	if !parent.InApp {
		t.Error("parent record should be in_app=true (ledger says so)")
	}
	if parent.IsSidechain {
		t.Error("parent record should not be sidechain")
	}
	sub, ok := byMsg["msg-c"]
	if !ok {
		t.Fatal("subagent record missing")
	}
	if !sub.IsSidechain {
		t.Error("subagent record should be is_sidechain=true (forced from path)")
	}
	if !sub.InApp {
		t.Error("subagent record should inherit parent in_app=true via parent-session lookup")
	}
	if sub.WorkflowInvocationID != "wfi_parent" || sub.WorkflowRole != "reviewer:spec_compliance" || sub.CacheTTLClass != "5m" || sub.CacheTTLControl != "applied" {
		t.Fatalf("subagent workflow metadata did not inherit from parent: %+v", sub)
	}
}

func TestScanOnce_WorkflowMetadataAndLongGapThreshold(t *testing.T) {
	cfgRoot := t.TempDir()
	dataDir := t.TempDir()
	enc := filepath.Join(cfgRoot, "projects", "enc-cwd")
	shortFile := filepath.Join(enc, "short-sess.jsonl")
	longClassFile := filepath.Join(enc, "one-hour-sess.jsonl")
	writeTranscript(t, shortFile,
		usageJSONLWithCacheSplit(t, "2026-09-03T01:00:00Z", "short-sess", "r1", "m1", 1, 1, 0),
		usageJSONLWithCacheSplit(t, "2026-09-03T01:04:30Z", "short-sess", "r2", "m2", 1, 1, 0),
		usageJSONLWithCacheSplit(t, "2026-09-03T01:09:01Z", "short-sess", "r3", "m3", 1, 1, 0),
	)
	writeTranscript(t, longClassFile,
		usageJSONLWithCacheSplit(t, "2026-09-03T01:00:00Z", "one-hour-sess", "r4", "m4", 1, 0, 1),
		usageJSONLWithCacheSplit(t, "2026-09-03T01:10:00Z", "one-hour-sess", "r5", "m5", 1, 0, 1),
	)
	t.Setenv("CLAUDE_CONFIG_DIR", cfgRoot)
	ledger := newInAppLedger(dataDir)
	ledger.RecordWorkflow("short-sess", "wfi_short", "triage", "5m", "applied")
	ledger.RecordWorkflow("one-hour-sess", "wfi_long", "narrator", "1h", "applied")
	cap := &captureServer{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()
	cursors := newCursorStore(dataDir)
	scanOnce(context.Background(), cursors, ledger, srv.Client(), srv.URL, "tok", nil)

	recs, _ := cap.snapshot()
	byMessage := map[string]ccUsageRecord{}
	for _, rec := range recs {
		byMessage[rec.MessageID] = rec
	}
	if len(byMessage) != 5 {
		t.Fatalf("records = %#v", recs)
	}
	if byMessage["m2"].LongGapObserved {
		t.Fatal("a gap of exactly 4m30s must not count")
	}
	if !byMessage["m3"].LongGapObserved {
		t.Fatal("a gap of 4m31s must count on the current turn")
	}
	if byMessage["m5"].LongGapObserved {
		t.Fatal("a 1h-class session must never increment the 5m long-gap counter")
	}
	if byMessage["m3"].CacheCreate5mTokens != 1 || byMessage["m3"].CacheCreate1hTokens != 0 || byMessage["m5"].CacheCreate5mTokens != 0 || byMessage["m5"].CacheCreate1hTokens != 1 {
		t.Fatalf("actual cache write split was not retained: 5m=%+v 1h=%+v", byMessage["m3"], byMessage["m5"])
	}
	if got := byMessage["m3"]; got.WorkflowInvocationID != "wfi_short" || got.WorkflowRole != "triage" || got.CacheTTLClass != "5m" || got.CacheTTLControl != "applied" {
		t.Fatalf("workflow stamping = %+v", got)
	}
}

func TestScanOnce_LongGapSpansAcknowledgedPasses(t *testing.T) {
	cfgRoot := t.TempDir()
	dataDir := t.TempDir()
	file := filepath.Join(cfgRoot, "projects", "enc-cwd", "sess.jsonl")
	writeTranscript(t, file, usageJSONL(t, "assistant", "2026-09-03T01:00:00Z", "sess", "r1", "m1", "claude-sonnet-4-6", 1, 1, 0, 1, false))
	t.Setenv("CLAUDE_CONFIG_DIR", cfgRoot)
	ledger := newInAppLedger(dataDir)
	ledger.RecordWorkflow("sess", "wfi", "reviewer", "5m", "applied")
	cap := &captureServer{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()
	cursors := newCursorStore(dataDir)
	scanOnce(context.Background(), cursors, ledger, srv.Client(), srv.URL, "tok", nil)

	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(usageJSONL(t, "assistant", "2026-09-03T01:04:31Z", "sess", "r2", "m2", "claude-sonnet-4-6", 1, 1, 0, 1, false) + "\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(file, future, future)
	scanOnce(context.Background(), cursors, ledger, srv.Client(), srv.URL, "tok", nil)
	recs, _ := cap.snapshot()
	if len(recs) != 2 || recs[1].MessageID != "m2" || !recs[1].LongGapObserved {
		t.Fatalf("cross-pass long gap = %#v", recs)
	}
}

func TestScanOnce_StreamedCopiesDoNotCreateLongGapWithinPass(t *testing.T) {
	cfgRoot := t.TempDir()
	dataDir := t.TempDir()
	file := filepath.Join(cfgRoot, "projects", "enc-cwd", "sess.jsonl")
	writeTranscript(t, file,
		usageJSONLWithCacheSplit(t, "2026-09-03T01:00:00Z", "sess", "same-request", "same-message", 1, 1, 0),
		usageJSONLWithCacheSplit(t, "2026-09-03T01:04:31Z", "sess", "same-request", "same-message", 2, 2, 0),
	)
	t.Setenv("CLAUDE_CONFIG_DIR", cfgRoot)
	ledger := newInAppLedger(dataDir)
	ledger.RecordWorkflow("sess", "wfi", "reviewer", "5m", "applied")
	cap := &captureServer{}
	cursors := newCursorStore(dataDir)

	scanOnce(context.Background(), cursors, ledger, captureClient(cap), "http://capture.invalid", "tok", nil)
	recs, _ := cap.snapshot()
	if len(recs) != 1 || recs[0].MessageID != "same-message" {
		t.Fatalf("deduplicated streamed turn = %#v", recs)
	}
	if recs[0].LongGapObserved {
		t.Fatal("growing copies of one billed turn must not count as an inter-turn gap")
	}
}

func TestScanOnce_StreamedCopiesDoNotCreateLongGapAcrossPasses(t *testing.T) {
	cfgRoot := t.TempDir()
	dataDir := t.TempDir()
	file := filepath.Join(cfgRoot, "projects", "enc-cwd", "sess.jsonl")
	writeTranscript(t, file, usageJSONLWithCacheSplit(t, "2026-09-03T01:00:00Z", "sess", "same-request", "same-message", 1, 1, 0))
	t.Setenv("CLAUDE_CONFIG_DIR", cfgRoot)
	ledger := newInAppLedger(dataDir)
	ledger.RecordWorkflow("sess", "wfi", "reviewer", "5m", "applied")
	cap := &captureServer{}
	cursors := newCursorStore(dataDir)
	client := captureClient(cap)
	scanOnce(context.Background(), cursors, ledger, client, "http://capture.invalid", "tok", nil)

	appendLine := func(line string) {
		f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.WriteString(line + "\n"); err == nil {
			err = f.Close()
		} else {
			_ = f.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(2 * time.Second)
		_ = os.Chtimes(file, future, future)
	}
	appendLine(usageJSONLWithCacheSplit(t, "2026-09-03T01:04:31Z", "sess", "same-request", "same-message", 2, 2, 0))
	scanOnce(context.Background(), cursors, ledger, client, "http://capture.invalid", "tok", nil)
	recs, _ := cap.snapshot()
	if len(recs) != 2 || recs[1].LongGapObserved {
		t.Fatalf("cross-pass streamed copy was treated as a new turn: %#v", recs)
	}

	appendLine(usageJSONLWithCacheSplit(t, "2026-09-03T01:09:02Z", "sess", "next-request", "next-message", 1, 1, 0))
	scanOnce(context.Background(), cursors, ledger, client, "http://capture.invalid", "tok", nil)
	recs, _ = cap.snapshot()
	if len(recs) != 3 || recs[2].MessageID != "next-message" || !recs[2].LongGapObserved {
		t.Fatalf("distinct turn after the streamed copy did not retain the real gap: %#v", recs)
	}
}

func TestScanOnce_LateWorkflowAttributionReplaysAcknowledgedTurns(t *testing.T) {
	cfgRoot := t.TempDir()
	dataDir := t.TempDir()
	file := filepath.Join(cfgRoot, "projects", "enc-cwd", "late-session.jsonl")
	writeTranscript(t, file, usageJSONLWithCacheSplit(t, "2026-09-03T01:00:00Z", "late-session", "request", "message", 1, 1, 0))
	t.Setenv("CLAUDE_CONFIG_DIR", cfgRoot)
	ledger := newInAppLedger(dataDir)
	cap := &captureServer{}
	cursors := newCursorStore(dataDir)
	client := captureClient(cap)

	// The scanner can reach EOF before a plain-result runtime observes the
	// session ID. That first upload is intentionally unattributed.
	scanOnce(context.Background(), cursors, ledger, client, "http://capture.invalid", "tok", nil)
	recs, calls := cap.snapshot()
	if len(recs) != 1 || calls != 1 || recs[0].WorkflowInvocationID != "" {
		t.Fatalf("pre-observer scan = calls %d records %#v", calls, recs)
	}

	if err := cursors.Save(); err != nil {
		t.Fatal(err)
	}
	// Simulate a daemon restart as well as the late observer: the replay
	// decision must be durable in cursor state, not only process memory.
	reloaded := newCursorStore(dataDir)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	ledger.RecordWorkflow("late-session", "wfi_late", "triage", "5m", "applied")
	scanOnce(context.Background(), reloaded, ledger, client, "http://capture.invalid", "tok", nil)
	recs, calls = cap.snapshot()
	if len(recs) != 2 || calls != 2 {
		t.Fatalf("late attribution did not replay an unchanged transcript: calls %d records %#v", calls, recs)
	}
	got := recs[1]
	if got.MessageID != "message" || got.WorkflowInvocationID != "wfi_late" || got.WorkflowRole != "triage" || got.CacheTTLClass != "5m" || got.CacheTTLControl != "applied" {
		t.Fatalf("replayed workflow attribution = %+v", got)
	}
}

// TestScanOnce_StampAccountFromLedger verifies the scanner stamps each row's
// Account from the session ledger (captured at SessionStart). A session with a
// recorded account carries it onto every row; an unattributed session (known
// to the ledger but no account) stamps "" so the cloud falls back to the
// machine owner — exactly today's behavior.
func TestScanOnce_StampAccountFromLedger(t *testing.T) {
	cfgRoot := t.TempDir()
	dataDir := t.TempDir()

	enc := filepath.Join(cfgRoot, "projects", "enc-cwd")
	ownerFile := filepath.Join(enc, "acct-sess.jsonl")
	anonFile := filepath.Join(enc, "anon-sess.jsonl")

	ownerLine := usageJSONL(t, "assistant", "2026-06-17T11:01:00Z", "acct-sess", "req-1", "msg-1", "claude-sonnet-4-6", 10, 20, 0, 0, false)
	anonLine := usageJSONL(t, "assistant", "2026-06-17T11:02:00Z", "anon-sess", "req-2", "msg-2", "claude-sonnet-4-6", 5, 5, 0, 0, false)
	writeTranscript(t, ownerFile, ownerLine)
	writeTranscript(t, anonFile, anonLine)

	t.Setenv("CLAUDE_CONFIG_DIR", cfgRoot)

	ledger := newInAppLedger(dataDir)
	ledger.Record("acct-sess", true)
	ledger.RecordAccount("acct-sess", "alice@team")
	// anon-sess is known (in-app) but has NO account recorded — it must stamp
	// "" (fall back to machine owner), not a phantom or stale account.
	ledger.Record("anon-sess", true)

	cap := &captureServer{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	cursors := newCursorStore(dataDir)
	if err := cursors.Load(); err != nil {
		t.Fatal(err)
	}
	scanOnce(context.Background(), cursors, ledger, srv.Client(), srv.URL, "tok", nil)

	recs, _ := cap.snapshot()
	byMsg := map[string]ccUsageRecord{}
	for _, r := range recs {
		byMsg[r.MessageID] = r
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if got := byMsg["msg-1"].Account; got != "alice@team" {
		t.Errorf("attributed record Account = %q, want alice@team", got)
	}
	if got := byMsg["msg-2"].Account; got != "" {
		t.Errorf("unattributed record Account = %q, want empty", got)
	}
}

// --- End-to-end cursor advance semantics --------------------------------

func TestScanOnce_CursorAdvancesOnlyOn2xx(t *testing.T) {
	cfgRoot := t.TempDir()
	dataDir := t.TempDir()
	enc := filepath.Join(cfgRoot, "projects", "enc-cwd")
	file := filepath.Join(enc, "sess-1.jsonl")
	line := usageJSONL(t, "assistant", "2026-06-17T11:01:00Z", "sess-1", "req-1", "msg-1", "claude-opus-4-8", 10, 20, 0, 0, false)
	writeTranscript(t, file, line)

	t.Setenv("CLAUDE_CONFIG_DIR", cfgRoot)
	ledger := newInAppLedger(dataDir)
	cap := &captureServer{status: http.StatusInternalServerError}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	cursors := newCursorStore(dataDir)
	if err := cursors.Load(); err != nil {
		t.Fatal(err)
	}

	// Pass 1: cloud returns 500 → cursor must NOT advance.
	scanOnce(context.Background(), cursors, ledger, srv.Client(), srv.URL, "tok", nil)
	if off, ok := cursors.Get(file); ok && off > 0 {
		t.Fatalf("cursor advanced to %d on 5xx; must stay put", off)
	}
	if _, ts, _ := cursors.ReadState(file, 0, 0); ts != "" {
		t.Fatalf("billed timestamp advanced on 5xx: %q", ts)
	}
	recs1, calls1 := cap.snapshot()
	if calls1 == 0 || len(recs1) == 0 {
		t.Fatalf("expected the failed POST to have been attempted (calls=%d recs=%d)", calls1, len(recs1))
	}

	// Pass 2: cloud now returns 200 → cursor advances to EOF.
	cap.setStatus(http.StatusOK)
	scanOnce(context.Background(), cursors, ledger, srv.Client(), srv.URL, "tok", nil)
	off, ok := cursors.Get(file)
	if !ok {
		t.Fatal("cursor missing after successful pass")
	}
	fi, _ := os.Stat(file)
	if off != fi.Size() {
		t.Fatalf("cursor=%d want=%d (full file consumed)", off, fi.Size())
	}
	if _, ts, _ := cursors.ReadState(file, fi.Size(), fi.ModTime().UnixNano()); ts != "2026-06-17T11:01:00Z" {
		t.Fatalf("acknowledged billed timestamp = %q", ts)
	}

	// Pass 3: nothing new appended → no further POST.
	_, callsBefore := cap.snapshot()
	scanOnce(context.Background(), cursors, ledger, srv.Client(), srv.URL, "tok", nil)
	_, callsAfter := cap.snapshot()
	if callsAfter != callsBefore {
		t.Fatalf("unexpected POST on idle pass: before=%d after=%d", callsBefore, callsAfter)
	}

	// Pass 4: after a committed timestamp exists, a failed report for an
	// appended turn must preserve both the prior offset and timestamp.
	second := usageJSONL(t, "assistant", "2026-06-17T11:10:00Z", "sess-1", "req-2", "msg-2", "claude-opus-4-8", 1, 1, 0, 0, false)
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(second + "\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(file, future, future)
	cap.setStatus(http.StatusInternalServerError)
	scanOnce(context.Background(), cursors, ledger, srv.Client(), srv.URL, "tok", nil)
	newInfo, _ := os.Stat(file)
	if held, _ := cursors.Get(file); held != off {
		t.Fatalf("cursor advanced from %d to %d on appended-turn 5xx", off, held)
	}
	if _, ts, _ := cursors.ReadState(file, newInfo.Size(), newInfo.ModTime().UnixNano()); ts != "2026-06-17T11:01:00Z" {
		t.Fatalf("prior billed timestamp changed on appended-turn 5xx: %q", ts)
	}
}

func TestScanOnce_IncrementalAppend(t *testing.T) {
	cfgRoot := t.TempDir()
	dataDir := t.TempDir()
	enc := filepath.Join(cfgRoot, "projects", "enc-cwd")
	file := filepath.Join(enc, "sess-1.jsonl")
	line1 := usageJSONL(t, "assistant", "2026-06-17T11:01:00Z", "sess-1", "req-1", "msg-1", "claude-opus-4-8", 10, 20, 0, 0, false)
	writeTranscript(t, file, line1)

	t.Setenv("CLAUDE_CONFIG_DIR", cfgRoot)
	ledger := newInAppLedger(dataDir)
	cap := &captureServer{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()
	cursors := newCursorStore(dataDir)
	_ = cursors.Load()

	scanOnce(context.Background(), cursors, ledger, srv.Client(), srv.URL, "tok", nil)
	recs, _ := cap.snapshot()
	if len(recs) != 1 {
		t.Fatalf("first pass got %d records, want 1", len(recs))
	}

	// Append a second turn; only the new line should be sent.
	line2 := usageJSONL(t, "assistant", "2026-06-17T11:05:00Z", "sess-1", "req-2", "msg-2", "claude-opus-4-8", 1, 1, 0, 0, false)
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line2 + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// Nudge mtime forward in case the test runs within one stat tick.
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(file, future, future)

	scanOnce(context.Background(), cursors, ledger, srv.Client(), srv.URL, "tok", nil)
	recs, _ = cap.snapshot()
	if len(recs) != 2 {
		t.Fatalf("after append got %d total records, want 2 (incremental)", len(recs))
	}
	if recs[1].MessageID != "msg-2" {
		t.Errorf("second pass sent the wrong record: %+v", recs[1])
	}
}

// Fix M1: a file of ONLY non-usage lines (the common case — user/tool
// messages) consumes bytes but yields zero records. Its cursor MUST still
// advance, or every tick re-reads the whole file forever.
func TestScanOnce_NonUsageFileAdvancesCursor(t *testing.T) {
	cfgRoot := t.TempDir()
	dataDir := t.TempDir()
	enc := filepath.Join(cfgRoot, "projects", "enc-cwd")
	file := filepath.Join(enc, "sess-1.jsonl")
	// Lines that pass nothing: a user line and a tool_result, no "usage".
	writeTranscript(t, file,
		`{"type":"user","timestamp":"2026-06-17T11:00:00Z","message":{"role":"user","content":"hi"}}`,
		`{"type":"tool_result","timestamp":"2026-06-17T11:00:01Z","message":{"content":"ok"}}`,
	)

	t.Setenv("CLAUDE_CONFIG_DIR", cfgRoot)
	ledger := newInAppLedger(dataDir)
	cap := &captureServer{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()
	cursors := newCursorStore(dataDir)
	if err := cursors.Load(); err != nil {
		t.Fatal(err)
	}

	scanOnce(context.Background(), cursors, ledger, srv.Client(), srv.URL, "tok", nil)

	// No records were produced, so no POST should have happened...
	recs, calls := cap.snapshot()
	if len(recs) != 0 {
		t.Fatalf("got %d records, want 0 (no usage lines)", len(recs))
	}
	if calls != 0 {
		t.Fatalf("expected no POST for a zero-record pass, got %d", calls)
	}
	// ...but the cursor MUST have advanced to EOF so we don't re-scan forever.
	off, ok := cursors.Get(file)
	if !ok {
		t.Fatal("cursor missing — non-usage file was never committed (infinite re-scan bug)")
	}
	fi, _ := os.Stat(file)
	if off != fi.Size() {
		t.Fatalf("cursor=%d want=%d (whole non-usage file should be consumed)", off, fi.Size())
	}

	if err := cursors.Save(); err != nil {
		t.Fatal(err)
	}
	// A forced shutdown checkpoint must persist across a reload.
	cursors2 := newCursorStore(dataDir)
	if err := cursors2.Load(); err != nil {
		t.Fatal(err)
	}
	if off2, ok := cursors2.Get(file); !ok || off2 != fi.Size() {
		t.Fatalf("reloaded cursor=%d (ok=%v) want=%d (must persist)", off2, ok, fi.Size())
	}
}

// Fix M2: a single line larger than ccUsageMaxBytesPerFilePass would fill the
// capped window with no complete line. The oversized-line fallback re-reads to
// EOF so the line completes and the cursor advances past it.
func TestScanOnce_OversizedLineFallbackAdvancesCursor(t *testing.T) {
	cfgRoot := t.TempDir()
	dataDir := t.TempDir()
	enc := filepath.Join(cfgRoot, "projects", "enc-cwd")
	file := filepath.Join(enc, "sess-1.jsonl")

	// Build a single billed assistant line whose JSON exceeds the per-pass
	// byte cap by padding a string field, so the capped window holds no '\n'.
	pad := strings.Repeat("x", ccUsageMaxBytesPerFilePass+4096)
	bigLine := `{"type":"assistant","timestamp":"2026-06-17T11:01:00Z","sessionId":"sess-1","requestId":"req-big","version":"1.0.131","pad":"` + pad + `","message":{"id":"msg-big","model":"claude-opus-4-8","usage":{"input_tokens":11,"output_tokens":22}}}`
	if len(bigLine)+1 <= ccUsageMaxBytesPerFilePass {
		t.Fatalf("fixture not larger than cap: line=%d cap=%d", len(bigLine), ccUsageMaxBytesPerFilePass)
	}
	writeTranscript(t, file, bigLine)

	t.Setenv("CLAUDE_CONFIG_DIR", cfgRoot)
	ledger := newInAppLedger(dataDir)
	cap := &captureServer{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()
	cursors := newCursorStore(dataDir)
	if err := cursors.Load(); err != nil {
		t.Fatal(err)
	}

	scanOnce(context.Background(), cursors, ledger, srv.Client(), srv.URL, "tok", nil)

	recs, _ := cap.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (oversized line must be parsed via fallback)", len(recs))
	}
	if recs[0].MessageID != "msg-big" || recs[0].TotalTokens != 33 {
		t.Errorf("oversized record wrong: %+v", recs[0])
	}
	// The cursor MUST advance past the oversized line (to EOF) — otherwise the
	// capped window holds no '\n' and we'd re-read it forever.
	off, ok := cursors.Get(file)
	if !ok {
		t.Fatal("cursor missing after oversized-line pass")
	}
	fi, _ := os.Stat(file)
	if off != fi.Size() {
		t.Fatalf("cursor=%d want=%d (oversized line not consumed → stuck cursor)", off, fi.Size())
	}
}

// Fix M3: two config dirs that resolve (via symlink) to the same physical tree
// must collapse to one entry, so the same transcript isn't scanned/POSTed
// twice under two paths.
func TestClaudeConfigDirs_SymlinkDedupe(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "claude-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	wantReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		wantReal = real
	}

	// Pass both the real dir and a symlink to it; they must collapse to one.
	t.Setenv("CLAUDE_CONFIG_DIR", real+","+link)
	dirs := claudeConfigDirs()
	if len(dirs) != 1 {
		t.Fatalf("claudeConfigDirs()=%v want exactly 1 (symlinked dupes collapse)", dirs)
	}
	if dirs[0] != wantReal {
		t.Fatalf("claudeConfigDirs()[0]=%q want resolved %q", dirs[0], wantReal)
	}
}
