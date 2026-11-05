package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// OpenAI Codex CLI rollout scanner — the Codex counterpart of the Claude
// transcript scanner in ccusage_scan.go, feeding the SAME cloud endpoint and
// table with provider="codex" rows.
//
// It shares that scanner's contract: a single slow background goroutine (never
// on the computeState hot path), bounded incremental reads, byte prefilters,
// cursors committed only after the cloud acks a chunk, and the cloud's keep-max
// upsert as the dedup authority. Codex-specific differences:
//
//   - Roots are $CODEX_HOME/sessions and ~/.codex/sessions plus their
//     archived_sessions siblings, collapsed through EvalSymlinks. Workflow
//     lanes and managed homes link their sessions/ back to ~/.codex, so
//     they are covered without being walked twice.
//   - The context a record needs (thread, model, cwd, account) arrives in
//     earlier lines, so each cursor persists a codexFileState.
//   - History older than codexUsageBackfillHorizon is skipped on first sight:
//     Codex keeps every rollout forever (13 GB on a heavy machine), and the
//     dashboard's longest bounded window is 90 days.

const (
	codexUsageScanInterval = 90 * time.Second
	// Start after the Claude scanner's first pass so the two never contend
	// for the cloud pool at boot.
	codexUsageInitialDelay = 45 * time.Second
	codexUsageChunkRecords = 2000
	// codexUsageMaxFilesPerPass and codexUsageMaxBytesPerPass spread a large
	// first-run backfill over many ticks.
	codexUsageMaxFilesPerPass = 400
	codexUsageMaxBytesPerPass = 256 << 20
	// codexUsageBackfillHorizon bounds first-sight history (by file mtime).
	codexUsageBackfillHorizon = 90 * 24 * time.Hour
	// codexUsageMetaLineMax bounds the leading session_meta line read when a
	// skipped rollout grows (it embeds the full base instructions).
	codexUsageMetaLineMax = 8 << 20
	// codexUsageProbeInterval spaces re-probes of an older cloud.
	codexUsageProbeInterval = 30 * time.Minute
)

// codexCursor is one rollout's persisted resume point plus its carried state.
type codexCursor struct {
	Offset          int64          `json:"offset"`
	Size            int64          `json:"size"`
	ModTimeUnixNano int64          `json:"modTimeUnixNano"`
	State           codexFileState `json:"state"`
	// AttributionKey is the ledger attribution (in-app, account, workflow)
	// applied when the consumed rows were acknowledged; a stronger value later
	// replays the file so the cloud's fill-if-empty upsert enriches the rows.
	AttributionKey string `json:"attributionKey,omitempty"`
	// Skipped marks a rollout first seen beyond the backfill horizon whose
	// existing bytes were deliberately not read.
	Skipped bool `json:"skipped,omitempty"`
}

type codexCursorStore struct {
	path          string
	mu            sync.Mutex
	m             map[string]codexCursor
	checkpoint    usageCheckpoint[codexCursor]
	uploadAllowed func() bool
}

func newCodexCursorStore(dataDir string) *codexCursorStore {
	return &codexCursorStore{path: filepath.Join(dataDir, "codex-usage-cursors.json"), m: map[string]codexCursor{}, checkpoint: usageCheckpoint[codexCursor]{lastSave: time.Now()}}
}

func (s *codexCursorStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(b) == 0 {
		return nil
	}
	var m map[string]codexCursor
	if err := json.Unmarshal(b, &m); err != nil {
		quarantineCorruptState(s.path, err)
		s.m = map[string]codexCursor{}
		return nil
	}
	if m == nil {
		m = map[string]codexCursor{}
	}
	s.m = m
	s.checkpoint.lastSave = s.checkpoint.clock()
	return nil
}

// Resume returns the cursor to continue from, or a zero cursor (read from the
// start with fresh state) when the file is new or was truncated/replaced.
func (s *codexCursorStore) Resume(path string, curSize, curModNano int64) (codexCursor, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[path]
	if !ok {
		return codexCursor{}, false
	}
	if curSize < c.Offset || curSize < c.Size || curModNano < c.ModTimeUnixNano {
		return codexCursor{}, false
	}
	return c, true
}

func (s *codexCursorStore) Set(path string, c codexCursor) {
	s.mu.Lock()
	s.checkpoint.set(s.m, path, c)
	s.mu.Unlock()
}

func (s *codexCursorStore) Save() error { return s.save(true) }
func (s *codexCursorStore) save(force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpoint.save(s.path, s.m, force)
}
func (s *codexCursorStore) commitSkipped(updates map[string]codexCursor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint.skip(s.m, updates)
	return s.checkpoint.save(s.path, s.m, true)
}
func (s *codexCursorStore) flushSkipped() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.checkpoint.pending) == 0 {
		return nil
	}
	return s.checkpoint.save(s.path, s.m, true)
}
func (s *codexCursorStore) revision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpoint.revision
}

// codexAccountResolver maps a rollout's (creator_account_id, creator_user_id)
// to the label of a saved Codex login. Nil-safe.
type codexAccountResolver func(accountID, userID string) string

// startCodexUsageScanner launches the rollout-scan goroutine. Gated by the
// caller on cloud creds like startCCUsageScanner; codex may be nil (no Codex
// credential manager), which only disables account labels.
func startCodexUsageScanner(ctx context.Context, dataDir, cloudBase, token string, ledger *inAppLedger, codex codexLoginState) {
	cursors := newCodexCursorStore(dataDir)
	if err := cursors.Load(); err != nil {
		log.Printf("codex-usage: load cursors: %v (starting fresh)", err)
	}
	client := &http.Client{Timeout: ccUsageReportTimeout}
	privacy := activePrivacy.Load()
	if privacy != nil {
		cursors.uploadAllowed = privacy.usageUploadAllowed
		client.Transport = usageUploadTransport{allowed: privacy.usageUploadAllowed}
	}
	unregister := privacy.registerUsage(cursors.Save, func(ctx context.Context) error {
		snapshot, err := snapshotUsageSkips(listCodexRollouts(codexUsageRoots()))
		if err != nil {
			return err
		}
		return drainUsageSkips(ctx, cursors.revision, func() {
			codexUsageScanPass(ctx, cursors, ledger, client, "", "", codex, codexUsageRoots(), time.Now(), snapshot)
		}, cursors.Save, func() bool {
			return snapshot.reached(func(path string) int64 {
				cursors.mu.Lock()
				defer cursors.mu.Unlock()
				return cursors.m[path].Offset
			})
		})
	})
	defer unregister()
	stopCheckpoints := startUsageCheckpoints(cursors.save, cursors.nextCheckpoint)
	defer stopCheckpoints()

	var supportedAt, probedAt time.Time
	runScan := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("codex-usage: scan panic recovered: %v", r)
				reportPanicToCloud("codex-usage-scan", r)
			}
		}()
		if !usageUploadAllowedNow() {
			// Usage statistics are off: advance past new turns without
			// sending them (see startCCUsageScanner).
			codexUsageScanPass(ctx, cursors, ledger, client, "", "", codex, codexUsageRoots(), time.Now())
			return
		}
		// A cloud that predates the provider column would store Codex rows as
		// Claude forever (and the acked cursors would never re-send them), so
		// nothing is reported until the cloud proves it understands providers.
		if supportedAt.IsZero() {
			if !probedAt.IsZero() && time.Since(probedAt) < codexUsageProbeInterval {
				return
			}
			probedAt = time.Now()
			if !cloudSupportsUsageProviders(ctx, client, cloudBase, token) {
				log.Printf("codex-usage: cloud does not support usage providers yet; Codex capture waits")
				return
			}
			supportedAt = time.Now()
		}
		codexUsageScanPass(ctx, cursors, ledger, client, cloudBase, token, codex, codexUsageRoots(), time.Now())
	}
	run := func() { privacy.runUsage(runScan) }

	select {
	case <-ctx.Done():
		return
	case <-time.After(codexUsageInitialDelay):
	}
	run()
	tick := time.NewTicker(codexUsageScanInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			run()
		}
	}
}

// cloudSupportsUsageProviders asks the cloud's usage read API for a Codex
// filtered summary: a provider-aware cloud echoes "provider":"codex", an older
// one ignores the parameter. Any error means "not yet".
func cloudSupportsUsageProviders(ctx context.Context, client *http.Client, cloudBase, token string) bool {
	if cloudBase == "" || token == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", cloudBase+"/api/cc-usage/summary?window=7d&provider=codex", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var body struct {
		Provider string `json:"provider"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body) != nil {
		return false
	}
	return body.Provider == "codex"
}

// codexUsageRoots returns the physical rollout roots to walk: each sessions
// root from codexSessionsRoots plus its archived_sessions sibling, resolved
// through symlinks and deduped so one physical tree is walked once.
func codexUsageRoots() []string {
	var candidates []string
	for _, root := range codexSessionsRoots() {
		candidates = append(candidates, root, filepath.Join(filepath.Dir(root), "archived_sessions"))
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range candidates {
		resolved := d
		if r, err := filepath.EvalSymlinks(d); err == nil {
			resolved = r
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		if fi, err := os.Stat(resolved); err == nil && fi.IsDir() {
			out = append(out, resolved)
		}
	}
	return out
}

func listCodexRollouts(roots []string) []string {
	var out []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
			if err != nil {
				if de != nil && de.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !de.IsDir() && strings.HasPrefix(de.Name(), "rollout-") && strings.HasSuffix(de.Name(), ".jsonl") {
				out = append(out, path)
			}
			return nil
		})
	}
	return out
}

// Every cursor-writing mode resolves the same local metadata. Omitting the
// resolver only while OFF/draining would persist a weaker key and turn the
// first ON pass into a false stronger-attribution replay of skipped history.
func codexUsageScanPass(ctx context.Context, cursors *codexCursorStore, ledger *inAppLedger, client *http.Client, cloudBase, token string, login codexLoginState, roots []string, now time.Time, snapshots ...*usageSkipSnapshot) {
	var accounts codexAccountResolver
	if login != nil {
		accounts = login.UsageAccountResolver()
	}
	codexScanOnce(ctx, cursors, ledger, client, cloudBase, token, accounts, roots, now, snapshots...)
}

// codexScanOnce performs one pass. Like scanOnce, records accumulate into
// chunks that are deduped, reported, and only then have their cursors
// committed; a failed report leaves those cursors for the next pass.
func codexScanOnce(ctx context.Context, cursors *codexCursorStore, ledger *inAppLedger, client *http.Client, cloudBase, token string, accounts codexAccountResolver, roots []string, now time.Time, snapshots ...*usageSkipSnapshot) {
	if cloudBase != "" {
		if cursors.flushSkipped() != nil {
			log.Print("codex-usage: pending skip checkpoint failed; upload blocked")
			return
		}
		if cursors.uploadAllowed != nil && !cursors.uploadAllowed() {
			return
		}
	}
	defer func() {
		if cursors.save(false) != nil {
			log.Print("codex-usage: checkpoint failed")
		}
	}()
	var files []string
	if len(snapshots) > 0 && snapshots[0] != nil {
		files = snapshots[0].files
	} else {
		files = listCodexRollouts(roots)
	}

	type pendingCursor struct {
		path string
		cur  codexCursor
	}
	var accum []ccUsageRecord
	var pending []pendingCursor
	filesThisPass := 0
	var bytesThisPass int64
	horizon := now.Add(-codexUsageBackfillHorizon)

	flush := func() bool {
		if cloudBase != "" && cursors.uploadAllowed != nil && !cursors.uploadAllowed() {
			return false
		}
		if len(accum) > 0 {
			deduped := dedupKeepMax(accum)
			if err := reportCCUsageBatch(ctx, client, cloudBase, token, deduped); err != nil {
				log.Printf("codex-usage: report %d records failed (cursors held for retry): %v", len(deduped), err)
				return false
			}
		}
		if cloudBase == "" || token == "" {
			updates := make(map[string]codexCursor, len(pending))
			for _, pc := range pending {
				updates[pc.path] = pc.cur
			}
			if cursors.commitSkipped(updates) != nil {
				log.Print("codex-usage: skip checkpoint failed; upload blocked")
				return false
			}
		} else {
			for _, pc := range pending {
				cursors.Set(pc.path, pc.cur)
			}
		}
		accum, pending = accum[:0], pending[:0]
		return true
	}

	for _, path := range files {
		if filesThisPass >= codexUsageMaxFilesPerPass || bytesThisPass >= codexUsageMaxBytesPerPass {
			break
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		size, modNano := usageSkipLimit(fi.Size(), path, snapshots), fi.ModTime().UnixNano()
		cur, resumed := cursors.Resume(path, size, modNano)
		if !resumed && fi.ModTime().Before(horizon) {
			// Beyond the backfill horizon: remember the file without reading
			// its history. Bytes appended later (a resumed old thread) are
			// still scanned.
			pending = append(pending, pendingCursor{path: path, cur: codexCursor{Offset: size, Size: size, ModTimeUnixNano: modNano, Skipped: true}})
			continue
		}

		st := cur.State
		if cur.Offset > 0 && st.ThreadID == "" {
			// Resuming mid-file without state (a skipped old rollout grew):
			// recover identity from the leading session_meta line.
			codexLoadLeadingMeta(path, &st)
		}
		attr := codexAttribution(ledger, accounts, st)
		start := cur.Offset
		if resumed && start > 0 && !cur.Skipped && codexAttributionStronger(attr.key, cur.AttributionKey) {
			// Stronger attribution arrived after these rows were acked:
			// replay the file from the start with fresh state.
			start, st = 0, codexFileState{}
		}
		if size <= start {
			continue
		}
		filesThisPass++

		var buf []byte
		var ok bool
		if len(snapshots) > 0 && snapshots[0] != nil {
			buf, ok = usageSkipWindow(path, start, size)
		} else {
			buf, ok = readJSONLWindow(path, start)
		}
		if !ok {
			continue
		}
		bytesThisPass += int64(len(buf))
		recs, consumed := extractCodexUsage(buf, &st)
		if len(snapshots) > 0 && snapshots[0] != nil && start+int64(len(buf)) == size {
			consumed = len(buf)
		}
		// The window may have revealed the identity (first pass over a new
		// file), so attribution is resolved again from the updated state.
		attr = codexAttribution(ledger, accounts, st)
		for i := range recs {
			attr.apply(&recs[i])
		}
		accum = append(accum, recs...)
		pending = append(pending, pendingCursor{path: path, cur: codexCursor{
			Offset: start + int64(consumed), Size: size, ModTimeUnixNano: modNano,
			State: st, AttributionKey: attr.key, Skipped: cur.Skipped && start > 0,
		}})
		if len(accum) >= codexUsageChunkRecords {
			if !flush() {
				return
			}
		}
	}
	if !flush() {
		log.Printf("codex-usage: final flush failed; %d cursors held for retry next pass", len(pending))
	}
}

// codexLoadLeadingMeta reads the rollout's first line (its session_meta) into
// st. Best-effort: a missing/oversized/unparseable first line leaves st as is.
func codexLoadLeadingMeta(path string, st *codexFileState) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), codexUsageMetaLineMax)
	if !sc.Scan() {
		return
	}
	line := append(append([]byte(nil), sc.Bytes()...), '\n')
	_, _ = extractCodexUsage(line, st)
}

// codexAttributionResult is what the ledger and credential store add to a
// rollout's records, plus a stable key of it for replay detection.
type codexAttributionResult struct {
	inApp                bool
	account              string
	workflowInvocationID string
	workflowRole         string
	key                  string
}

// codexAttributionStronger reports whether newKey adds a field the stored key
// lacked (in-app learned, or an account/workflow field filled). Only then is a
// replay useful: the cloud's upsert fills empty fields and never downgrades, so
// a weaker or merely different key (a changed live login, a
// ledger prune) must not re-upload the whole rollout.
func codexAttributionStronger(newKey, oldKey string) bool {
	if newKey == "" || newKey == oldKey {
		return false
	}
	nf := strings.Split(newKey, "\x1f")
	of := strings.Split(oldKey, "\x1f")
	for i, v := range nf {
		old := ""
		if i < len(of) {
			old = of[i]
		}
		if i == 0 {
			if v == "1" && old != "1" {
				return true
			}
			continue
		}
		if v != "" && old == "" {
			return true
		}
	}
	return false
}

func (a codexAttributionResult) apply(rec *ccUsageRecord) {
	rec.InApp = a.inApp
	rec.Account = a.account
	rec.WorkflowInvocationID = a.workflowInvocationID
	rec.WorkflowRole = a.workflowRole
}

// codexAttribution resolves in-app, account and workflow for a rollout.
//
//   - in-app: soluterminal's own Codex launcher stamps originator
//     "soluterminal_*"; a pane that adopted the thread records it in the ledger.
//   - workflow + account: a workflow run observes its thread id into the ledger
//     with the invocation's credential lane (authoritative). Subagent threads
//     fall back to their root session's entry.
//   - account otherwise: the rollout's creator account matched to a saved
//     Codex login. Proxied threads are skipped because their creator identity
//     does not establish billing attribution.
func codexAttribution(ledger *inAppLedger, accounts codexAccountResolver, st codexFileState) codexAttributionResult {
	var r codexAttributionResult
	if st.ThreadID == "" {
		return r
	}
	r.inApp = strings.HasPrefix(st.Originator, "soluterminal")
	if ledger != nil {
		for _, id := range []string{st.ThreadID, st.SessionID} {
			if id == "" {
				continue
			}
			if ia, known := ledger.Lookup(id); known && ia {
				r.inApp = true
			}
			if r.workflowInvocationID == "" && r.workflowRole == "" {
				r.workflowInvocationID, r.workflowRole, _, _ = ledger.LookupWorkflow(id)
			}
			if r.account == "" {
				r.account = ledger.LookupAccount(id)
			}
		}
	}
	if r.account == "" && accounts != nil && st.ModelProvider != "solu_team_broker" && st.AccountID != "" {
		r.account = accounts(st.AccountID, st.UserID)
	}
	if r.inApp || r.account != "" || r.workflowInvocationID != "" || r.workflowRole != "" {
		inApp := "0"
		if r.inApp {
			inApp = "1"
		}
		r.key = strings.Join([]string{inApp, r.account, r.workflowInvocationID, r.workflowRole}, "\x1f")
	}
	return r
}

func (s *codexCursorStore) nextCheckpoint() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpoint.nextDelay()
}
