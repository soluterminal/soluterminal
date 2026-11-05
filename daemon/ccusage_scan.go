package main

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Claude Code transcript scanner.
//
// A standalone background goroutine that, every tick, reads the bytes
// appended to each Claude Code transcript since the previous pass, extracts
// billed assistant turns, tags each as in-app/out-app, and POSTs them to the
// cloud (HMAC-signed via reportCCUsageBatch). The cloud is the dedup
// authority (keep-max upsert on (machine_id, message_id, request_id)), so
// re-sends are harmless and we advance a file's cursor ONLY after its batch
// is acked.
//
// HOT-PATH WARNING: this MUST NOT run inside computeState / per-pane tickers.
// That path caused the v0.0.484 GC death-spiral (state.go). This is a single
// separate goroutine doing bounded file I/O on a slow (90s) cadence, with a
// "usage" byte-prefilter and incremental reads so steady-state cost is tiny.
//
// The leaf modules it composes (do not duplicate their logic here):
//   - cursorStore   (ccusage_cursors.go): per-file resume offsets, atomic.
//   - inAppLedger   (ccusage_ledger.go):  sessionId → in_app, fed by hooks.
//   - extractUsageLines / dedupKeepMax (ccusage_extract.go): pure parsing.
//   - reportCCUsageBatch (ccusage_report.go): the signed POST.

const (
	// ccUsageScanInterval is the ticker cadence. Slow on purpose — usage
	// data is not latency-sensitive and we never want this competing with
	// the render/state hot paths for CPU or IO.
	ccUsageScanInterval = 90 * time.Second

	// ccUsageInitialDelay staggers the first scan a few seconds past boot so
	// the daemon's latency-sensitive startup (reattach, port bind, relay
	// connect) finishes first.
	ccUsageInitialDelay = 8 * time.Second

	// ccUsageChunkRecords is the soft cap on how many records we accumulate
	// across files before flushing (dedup → report → advance cursors). It
	// bounds peak memory and keeps the first-run backfill from spiking: a
	// huge history is sent in chunks, with the remaining files naturally
	// picked up on subsequent ticks. Sized a few batches' worth above
	// reportCCUsageBatch's 500-per-POST chunking.
	ccUsageChunkRecords = 2000

	// ccUsageMaxFilesPerPass caps how many transcript files we even open in
	// a single pass, so a machine with thousands of transcripts spreads its
	// backfill over several ticks rather than statting/opening everything at
	// once. Files past the cap are seen next pass (their cursors are
	// untouched, so nothing is lost).
	ccUsageMaxFilesPerPass = 400

	// ccUsageLedgerRetention is how long a session's in-app flag is kept in
	// the ledger before Prune drops it — comfortably past Claude's ~30-day
	// transcript retention so we never prune a session whose file we might
	// still re-scan.
	ccUsageLedgerRetention = 45 * 24 * time.Hour

	// ccUsageMaxBytesPerFilePass bounds how many bytes a single file
	// contributes per pass (fix M2). Without this, the first-run backfill of a
	// single huge transcript would io.ReadAll the whole thing into memory in
	// one shot. Instead we read at most this window, consume up to its last
	// complete line, advance the cursor only by what we consumed, and pick up
	// the remainder on subsequent ticks (composes with the M1 cursor commit).
	ccUsageMaxBytesPerFilePass = 16 << 20 // 16 MiB
)

// startCCUsageScanner launches the transcript-scan goroutine. It is gated by
// the caller on the cloud relay being configured (non-empty cloudBase +
// token); reportCCUsageBatch is itself a no-op on empty creds, but gating
// avoids needless file I/O on a local-only daemon.
//
// dataDir is where the cursor file lives (<dataDir>/cc-usage-cursors.json);
// ledger is the shared in-app ledger fed by the hook handlers — we Flush (and
// periodically Prune) it here so hook-recorded entries reach disk.
//
// A panic in any single scan is recovered + logged so a malformed transcript
// or a transient FS error can never take down the daemon.
func startCCUsageScanner(ctx context.Context, dataDir, cloudBase, token string, ledger *inAppLedger) {
	cursors := newCursorStore(dataDir)
	if err := cursors.Load(); err != nil {
		log.Printf("cc-usage: load cursors: %v (starting fresh)", err)
	}
	// One reusable client across all passes (connection reuse, single
	// timeout policy). reportCCUsageBatch tolerates a nil client too, but an
	// explicit shared one is cheaper over the daemon's lifetime.
	client := &http.Client{Timeout: ccUsageReportTimeout}
	privacy := activePrivacy.Load()
	if privacy != nil {
		cursors.uploadAllowed = privacy.usageUploadAllowed
		client.Transport = usageUploadTransport{allowed: privacy.usageUploadAllowed}
	}
	unregister := privacy.registerUsage(cursors.Save, func(ctx context.Context) error {
		snapshot, err := snapshotUsageSkips(listTranscripts(claudeConfigDirsForDataDir(filepath.Dir(cursors.path))))
		if err != nil {
			return err
		}
		return drainUsageSkips(ctx, cursors.revision, func() { scanOnce(ctx, cursors, ledger, client, "", "", nil, snapshot) }, cursors.Save, func() bool {
			return snapshot.reached(func(path string) int64 { off, _ := cursors.Get(path); return off })
		})
	})
	defer unregister()
	stopCheckpoints := startUsageCheckpoints(cursors.save, cursors.nextCheckpoint)
	defer stopCheckpoints()

	// One resolver for the scanner's lifetime so cwd→repo lookups are cached
	// across passes (a remote URL effectively never changes).
	repos := newRepoResolver()

	runScan := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("cc-usage: scan panic recovered: %v", r)
				reportPanicToCloud("cc-usage-scan", r)
			}
		}()
		if !usageUploadAllowedNow() {
			// Usage statistics are off (privacy_settings.go): read past the
			// new turns without sending them, so switching it on later only
			// shares turns from then on, never the backlog.
			scanOnce(ctx, cursors, ledger, client, "", "", nil)
			return
		}
		scanOnce(ctx, cursors, ledger, client, cloudBase, token, repos)
	}

	run := func() { privacy.runUsage(runScan) }

	// One scan shortly after boot, then on the tick.
	select {
	case <-ctx.Done():
		return
	case <-time.After(ccUsageInitialDelay):
	}
	run()

	// One-time retroactive git_repo backfill for rows captured before this
	// machine upgraded (self-heal). Runs in its own goroutine so it never
	// delays the scan ticker; gated by a marker file so it runs once. Shares
	// the cached resolver with the scanner (repoResolver is mutex-guarded).
	if usageUploadAllowedNow() {
		go maybeRunRepoBackfill(ctx, client, cloudBase, token, dataDir, repos)
	}

	tick := time.NewTicker(ccUsageScanInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			// Best-effort final persist so the last pass's ledger upgrades
			// and cursor advances survive a clean shutdown.
			_ = ledger.Flush()
			return
		case <-tick.C:
			run()
		}
	}
}

// scanOnce performs a single scan pass over all discovered transcripts.
//
// For each file it resumes from the cursor (resetting on truncation/
// replacement via ShouldResetTo), reads only the appended bytes up to the
// last complete line, extracts billed turns, and tags in_app/is_sidechain.
// Records accumulate across files into a chunk; when the chunk fills (or at
// the end of the pass) we dedup keep-max, POST, and — only on a 2xx ack —
// advance every cursor in that chunk and Save. A failed POST leaves those
// cursors untouched so the next pass re-reads and re-sends (idempotent
// upsert makes that safe). Each chunk is therefore atomic: nothing is lost
// and nothing is double-committed.
// repos may be nil (tests pass nil to stay hermetic / skip git execs); when
// non-nil each record's GitRepo is resolved from its cwd.
func scanOnce(ctx context.Context, cursors *cursorStore, ledger *inAppLedger, client *http.Client, cloudBase, token string, repos *repoResolver, snapshots ...*usageSkipSnapshot) {
	// Persist any hook-recorded ledger entries and trim ancient ones on EVERY
	// exit path (fix N2). A deferred flush+prune means even a pass that bails
	// early — no files, or a mid-loop report failure that returns — still
	// reaches disk and still prunes. Prune is cheap and must not be skipped by
	// a never-fully-successful pass, or the ledger grows without bound.
	defer func() {
		if err := ledger.Flush(); err != nil {
			log.Printf("cc-usage: ledger flush: %v", err)
		}
		ledger.Prune(time.Now().Add(-ccUsageLedgerRetention))
	}()

	if cloudBase != "" {
		if cursors.flushSkipped() != nil {
			log.Print("cc-usage: pending skip checkpoint failed; upload blocked")
			return
		}
		if cursors.uploadAllowed != nil && !cursors.uploadAllowed() {
			return
		}
	}
	defer func() {
		if cursors.save(false) != nil {
			log.Print("cc-usage: checkpoint failed")
		}
	}()
	var files []string
	if len(snapshots) > 0 && snapshots[0] != nil {
		files = snapshots[0].files
	} else {
		files = listTranscripts(claudeConfigDirsForDataDir(filepath.Dir(cursors.path)))
	}
	if len(files) == 0 {
		return
	}

	// pendingCursor is a file whose new offset we'll commit IFF the chunk
	// it belongs to is acked.
	type pendingCursor struct {
		path                   string
		offset                 int64
		size                   int64
		modNano                int64
		lastBilledTurnTS       string
		lastBilledMessageID    string
		lastBilledRequestID    string
		workflowAttributionKey string
	}

	var accum []ccUsageRecord
	var pending []pendingCursor
	filesThisPass := 0

	// flush dedups + reports the current chunk (if any), then advances the
	// pending cursors. Returns false ONLY if the report failed (caller stops
	// the pass to avoid hammering a down cloud; the untouched cursors retry
	// next tick).
	//
	// Fix M1: a chunk that consumed bytes but produced ZERO usage records
	// (the common case — files full of user/tool lines) must STILL advance
	// those files' cursors, or we re-read them every tick forever. So we only
	// skip the POST when accum is empty; we ALWAYS commit pending cursors.
	flush := func() bool {
		if cloudBase != "" && cursors.uploadAllowed != nil && !cursors.uploadAllowed() {
			return false
		}
		if len(accum) > 0 {
			deduped := dedupKeepMax(accum)
			if err := reportCCUsageBatch(ctx, client, cloudBase, token, deduped); err != nil {
				log.Printf("cc-usage: report %d records failed (cursors held for retry): %v", len(deduped), err)
				return false // leave cursors unadvanced for retry
			}
		}
		if cloudBase == "" || token == "" {
			updates := make(map[string]scanCursor, len(pending))
			for _, pc := range pending {
				updates[pc.path] = scanCursor{
					Offset: pc.offset, Size: pc.size, ModTimeUnixNano: pc.modNano,
					LastBilledTurnTS: pc.lastBilledTurnTS, LastBilledMessageID: pc.lastBilledMessageID,
					LastBilledRequestID: pc.lastBilledRequestID, WorkflowAttributionKey: pc.workflowAttributionKey,
				}
			}
			if cursors.commitSkipped(updates) != nil {
				log.Print("cc-usage: skip checkpoint failed; upload blocked")
				return false
			}
		} else {
			for _, pc := range pending {
				cursors.SetBilledState(pc.path, pc.offset, pc.size, pc.modNano, pc.lastBilledTurnTS, pc.lastBilledMessageID, pc.lastBilledRequestID, pc.workflowAttributionKey)
			}
		}
		accum = accum[:0]
		pending = pending[:0]
		return true
	}

	for _, path := range files {
		if filesThisPass >= ccUsageMaxFilesPerPass {
			// Remaining files wait for the next tick.
			break
		}
		select {
		case <-ctx.Done():
			return
		default:
		}

		fi, err := os.Stat(path)
		if err != nil {
			continue // file vanished between walk and stat — skip
		}
		curSize := usageSkipLimit(fi.Size(), path, snapshots)
		curModNano := fi.ModTime().UnixNano()
		start, lastBilledTurnTS, lastBilledMessageID, lastBilledRequestID, storedAttributionKey, reset := cursors.ReadBilledState(path, curSize, curModNano)
		// Resolve workflow attribution before the unchanged-file fast path. A
		// plain JSON result may reveal its session ID only after an earlier scan
		// reached EOF. Re-reading acknowledged bytes when the durable metadata
		// becomes stronger lets the cloud's keep-max upsert enrich those rows.
		sid := sessionIDForTranscript(path)
		workflowInvocationID, workflowRole, cacheTTLClass, cacheTTLControl := "", "", "", ""
		account := ""
		if sid != "" {
			workflowInvocationID, workflowRole, cacheTTLClass, cacheTTLControl = ledger.LookupWorkflow(sid)
			// "" for an unknown/unattributed session → cloud falls back to the
			// machine owner (today's behavior). LookupAccount returns "" then.
			account = ledger.LookupAccount(sid)
		}
		// The account joins the attribution key so a session whose account is
		// learned after its turns were acknowledged is replayed once; the
		// cloud's fill-if-empty upsert then enriches those rows.
		attributionKey := ccUsageWorkflowAttributionKey(workflowInvocationID, workflowRole, cacheTTLClass, cacheTTLControl)
		if account != "" {
			attributionKey += "\x1e" + account
		}
		// A cursor written before the account joined the key carries the
		// same workflow key without the suffix; those rows were already sent
		// with the account the ledger held, so it is not a reason to replay.
		legacyMatch := account != "" && storedAttributionKey != "" && !strings.Contains(storedAttributionKey, "\x1e") &&
			attributionKey == storedAttributionKey+"\x1e"+account
		if !reset && start > 0 && attributionKey != "" && attributionKey != storedAttributionKey && !legacyMatch {
			start = 0
			lastBilledTurnTS, lastBilledMessageID, lastBilledRequestID = "", "", ""
			reset = true
		}
		if curSize <= start {
			continue // nothing new appended since last pass
		}
		filesThisPass++

		var recs []ccUsageRecord
		var consumed int
		if len(snapshots) > 0 && snapshots[0] != nil {
			buf, ok := usageSkipWindow(path, start, curSize)
			if !ok {
				continue
			}
			recs, consumed = extractUsageLines(buf)
			// A partial line at the opt-in boundary belongs to the skipped
			// interval too; do not later upload its completion.
			if start+int64(len(buf)) == curSize {
				consumed = len(buf)
			}
		} else {
			recs, consumed = readUsageFrom(path, start)
		}
		// Even if this file yielded zero NEW records (e.g. all appended
		// lines were non-usage), we still want to advance its cursor past
		// the consumed bytes so we don't re-scan them. The advance only
		// commits with the chunk, though, to keep the pass atomic.
		newOffset := start + int64(consumed)

		// Finalize attribution now that we know the file path.
		inApp := false
		if sid != "" {
			if ia, known := ledger.Lookup(sid); known {
				inApp = ia
			}
		}
		isSub := isSubagentTranscript(path)
		lastValid := time.Time{}
		if !reset && lastBilledTurnTS != "" {
			lastValid, _ = time.Parse(time.RFC3339Nano, lastBilledTurnTS)
		}
		for i := range recs {
			recs[i].InApp = inApp
			recs[i].Account = account
			recs[i].WorkflowInvocationID = workflowInvocationID
			recs[i].WorkflowRole = workflowRole
			recs[i].CacheTTLClass = cacheTTLClass
			recs[i].CacheTTLControl = cacheTTLControl
			if isSub {
				// Subagent files are always sidechain turns regardless of
				// the line's own field.
				recs[i].IsSidechain = true
			}
			// Attach the cwd's git remote (cached per-cwd). nil in tests.
			if repos != nil {
				recs[i].GitRepo = repos.resolve(ctx, recs[i].Cwd)
			}
			if ts, err := time.Parse(time.RFC3339Nano, recs[i].EventTS); err == nil && (lastValid.IsZero() || ts.After(lastValid)) {
				sameBilledTurn := lastBilledMessageID != "" && lastBilledRequestID != "" &&
					recs[i].MessageID == lastBilledMessageID && recs[i].RequestID == lastBilledRequestID
				// A legacy timestamp without identity is uncertain. Establish the
				// first identity without emitting a potentially false long-gap.
				identityKnown := lastBilledMessageID != "" && lastBilledRequestID != ""
				if !lastValid.IsZero() && identityKnown && !sameBilledTurn && cacheTTLClass == ccCacheTTL5m && cacheTTLControl == ccCacheTTLControlApplied && ts.Sub(lastValid) > 4*time.Minute+30*time.Second {
					recs[i].LongGapObserved = true
				}
				lastValid = ts
				lastBilledMessageID = recs[i].MessageID
				lastBilledRequestID = recs[i].RequestID
			}
		}
		if !lastValid.IsZero() {
			lastBilledTurnTS = lastValid.UTC().Format(time.RFC3339Nano)
		} else if reset {
			lastBilledTurnTS, lastBilledMessageID, lastBilledRequestID = "", "", ""
		}

		accum = append(accum, recs...)
		pending = append(pending, pendingCursor{
			path: path, offset: newOffset, size: curSize, modNano: curModNano,
			lastBilledTurnTS: lastBilledTurnTS, lastBilledMessageID: lastBilledMessageID,
			lastBilledRequestID: lastBilledRequestID, workflowAttributionKey: attributionKey,
		})

		if len(accum) >= ccUsageChunkRecords {
			if !flush() {
				// Report failed: stop the pass. Already-committed earlier
				// chunks stay committed; this chunk's cursors are untouched.
				// The deferred ledger flush+prune still runs.
				return
			}
		}
	}

	// Final partial chunk. Log if it failed (fix N1) — the deferred ledger
	// flush+prune still runs regardless.
	if !flush() {
		log.Printf("cc-usage: final flush failed; %d cursors held for retry next pass", len(pending))
	}
}

func ccUsageWorkflowAttributionKey(invocationID, role, ttlClass, control string) string {
	if invocationID == "" && role == "" && ttlClass == "" && control == "" {
		return ""
	}
	return strings.Join([]string{invocationID, role, ttlClass, control}, "\x1f")
}

// readUsageFrom opens path, seeks to start, reads a BOUNDED window
// (ccUsageMaxBytesPerFilePass, fix M2) and extracts the usage records from the
// complete lines in it. It returns the records and the number of bytes
// consumed (up to and including the last '\n' in the window); a trailing
// partial line — or any bytes past the window — is left unconsumed so it is
// re-read next pass. Any open/read error yields (nil, 0) — the caller then
// doesn't advance the cursor and retries next pass.
//
// The bound keeps first-run backfill of a single huge transcript from spiking
// memory: we read at most one window per pass, advance the cursor by what we
// consumed, and pick up the remainder on the next tick (composes with the M1
// cursor commit, which now advances cursors even on a zero-record window).
//
// Oversized-line edge case: if the capped window contains NO newline but the
// file has more bytes (we filled the cap without hitting EOF), a single line
// is larger than the cap. To avoid a stuck cursor that can never advance past
// it, we fall back to reading the REST of this file to EOF for this one pass so
// that oversized line becomes a complete line and the cursor can move past it.
func readUsageFrom(path string, start int64) (recs []ccUsageRecord, consumed int) {
	buf, ok := readJSONLWindow(path, start)
	if !ok {
		return nil, 0
	}
	return extractUsageLines(buf)
}

// readJSONLWindow returns the bytes a scanner should parse from path starting
// at start: at most ccUsageMaxBytesPerFilePass, or — when that window holds no
// complete line because a single line is larger than the cap — the rest of the
// file, so the cursor can always advance past an oversized line. ok is false on
// any open/seek/read error (the caller then holds its cursor and retries).
// Shared by the Claude transcript and Codex rollout scanners.
func readJSONLWindow(path string, start int64) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return nil, false
		}
	}

	// Read at most one window. io.LimitReader caps the bytes; ReadAll then
	// drains exactly that window (or to EOF if the file is shorter).
	buf, err := io.ReadAll(io.LimitReader(f, ccUsageMaxBytesPerFilePass))
	if err != nil {
		return nil, false
	}

	// Oversized-single-line fallback: we filled the cap and the window held no
	// complete line, so the cursor would never advance past this line and we'd
	// re-read the same capped window forever. Re-read the rest of THIS file
	// from start to EOF this pass so the line completes and the cursor can
	// move on.
	if len(buf) == ccUsageMaxBytesPerFilePass && bytes.IndexByte(buf, '\n') < 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return nil, false
		}
		full, err := io.ReadAll(f)
		if err != nil {
			return nil, false
		}
		return full, true
	}
	return buf, true
}

// claudeConfigDirs resolves the Claude Code config directories to scan,
// mirroring ccusage's resolution (plan A1):
//
//   - If CLAUDE_CONFIG_DIR is set: comma-split, trim, expand a leading "~",
//     and (if an entry itself ends in "projects") use its parent. Keep each
//     entry whose "<dir>/projects" is a directory.
//   - Otherwise default to $XDG_CONFIG_HOME/claude (default ~/.config/claude)
//     AND ~/.claude — keep each whose "projects/" child exists.
//
// The returned slice is deduped, order-stable.
func claudeConfigDirs() []string {
	var candidates []string
	if env := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); env != "" {
		for _, part := range strings.Split(env, ",") {
			d := expandHome(strings.TrimSpace(part))
			if d == "" {
				continue
			}
			// If the entry points straight at a projects dir, use its parent
			// so "<dir>/projects" resolves correctly below.
			if filepath.Base(d) == "projects" {
				d = filepath.Dir(d)
			}
			candidates = append(candidates, d)
		}
	} else {
		xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if xdg == "" {
				xdg = filepath.Join(home, ".config")
			}
			candidates = append(candidates, filepath.Join(xdg, "claude"))
			candidates = append(candidates, filepath.Join(home, ".claude"))
		} else if xdg != "" {
			candidates = append(candidates, filepath.Join(xdg, "claude"))
		}
	}

	// Resolve symlinks before dedupe (fix M3): ~/.config/claude and ~/.claude
	// can point at the same physical tree (one a symlink to the other). Without
	// resolution they'd be two distinct map keys, so every transcript under
	// them would be scanned — and POSTed — twice, under two different paths
	// (each with its own cursor). Collapsing to the resolved path makes one
	// physical tree appear once. On EvalSymlinks error (e.g. dir doesn't
	// exist) we keep the unresolved path so behavior is otherwise unchanged.
	seen := map[string]struct{}{}
	var out []string
	for _, d := range candidates {
		resolved := d
		if r, err := filepath.EvalSymlinks(d); err == nil {
			resolved = r
		}
		if _, ok := seen[resolved]; ok {
			continue
		}
		seen[resolved] = struct{}{}
		if fi, err := os.Stat(filepath.Join(resolved, "projects")); err == nil && fi.IsDir() {
			out = append(out, resolved)
		}
	}
	return out
}

// Windows lanes use directory junctions/copies rather than ordinary symlinks.
// Include only lane projects directories that are physically real there;
// non-Windows symlink lanes are already collapsed by EvalSymlinks above.
func claudeConfigDirsForDataDir(dataDir string) []string {
	dirs := claudeConfigDirs()
	if claudeUsageExtraDirs == nil || dataDir == "" {
		return dirs
	}
	extra := claudeUsageExtraDirs(dataDir)
	seen := make(map[string]bool, len(dirs)+len(extra))
	for _, dir := range dirs {
		seen[dir] = true
	}
	for _, dir := range extra {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	return dirs
}

// claudeUsageExtraDirs lists further Claude config directories under dataDir
// whose transcripts count as this machine's usage (a feature's isolated
// Claude homes). nil: only the user's own config directories.
var claudeUsageExtraDirs func(dataDir string) []string

// listTranscripts walks "<dir>/projects" for each config dir and returns
// every "*.jsonl" file. It RECURSES into subdirectories — that is how
// subagent transcripts under "<sessionId>/subagents/agent-*.jsonl" get
// included (they carry real billed usage, is_sidechain=true). Walk errors on
// individual entries are skipped rather than aborting the walk.
func listTranscripts(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		root := filepath.Join(d, "projects")
		_ = filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
			if err != nil {
				// Unreadable entry — skip it but keep walking siblings.
				if de != nil && de.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if de.IsDir() {
				return nil
			}
			if strings.HasSuffix(de.Name(), ".jsonl") {
				out = append(out, path)
			}
			return nil
		})
	}
	return out
}

// isSubagentTranscript reports whether path is a subagent (Task) transcript,
// i.e. lives under a ".../subagents/" directory. Those turns are sidechain
// turns regardless of the line's own isSidechain field.
func isSubagentTranscript(path string) bool {
	dir := filepath.Dir(path)
	return filepath.Base(dir) == "subagents"
}

// sessionIDForTranscript derives the session id to look up in the in-app
// ledger for a transcript file:
//
//   - Normal file ".../projects/<enc>/<sessionId>.jsonl" → the file's base
//     name without ".jsonl" (the sessionId).
//   - Subagent file ".../projects/<enc>/<parentSessionId>/subagents/agent-*.jsonl"
//     → the PARENT sessionId, which is the directory two levels up from the
//     file (the component above "subagents/"). The hooks only ever saw the
//     parent session, so that's what the ledger knows.
//
// Returns "" when the shape is unrecognized; the scanner then treats the
// file as out-app.
func sessionIDForTranscript(path string) string {
	if isSubagentTranscript(path) {
		// .../<parentSessionId>/subagents/agent-X.jsonl
		subagentsDir := filepath.Dir(path)      // .../<parent>/subagents
		parentDir := filepath.Dir(subagentsDir) // .../<parent>
		return filepath.Base(parentDir)
	}
	base := filepath.Base(path)
	return strings.TrimSuffix(base, ".jsonl")
}

// The prompt-cache TTL class and control values a transcript records.
const (
	ccCacheTTL5m             = "5m"
	ccCacheTTLControlApplied = "applied"
)
