package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// warden.go — the summoner. A deterministic watcher that, ONLY when enabled,
// detects two fleet events and spawns a short-lived, tool-scoped headless
// Claude run for each:
//
//   - debrief: a pane that did real agent work has exited → harvest outcomes,
//     follow-ups, and lessons before its scrollback is gone.
//   - triage:  a pane has sustained needs-user past the configured delay →
//     the real-time push already fired; prepare a proper brief for the user.
//
// The judgment run gets a per-run scoped capability token (mcp_scope.go): it
// can read the fleet and annotate (flags) but cannot send_input, spawn,
// kill, edit files, or create schedules. The scope is daemon-enforced, so a
// drifted or injected run is bounded by Go code, not by its prompt.

const (
	wardenTickInterval = 15 * time.Second
	wardenRunTimeout   = 8 * time.Minute
	// wardenDebriefMinOutput gates debrief on the pane having produced real
	// output — a pane that spawned and died immediately has nothing to learn.
	wardenDebriefMinOutput = 2000 // stripped chars
	wardenLogLimit         = 256 * 1024
	wardenScopeIdentity    = "warden"
)

// wardenReadAnnotateTools is the v1 tool budget: read the fleet + annotate
// (flags). Deliberately NO send_input, answer_prompt, spawn_pane,
// kill_pane, schedules, or file writes — a caretaker that observes and
// reports, never one that acts unattended. Widening this is a deliberate,
// trust-gated future step, not a config knob.
func wardenReadAnnotateTools() []string {
	return []string{
		"list_panes", "get_pane_state", "read_pane_tail", "read_activity",
		"list_ports", "get_pane_livecwd", "git_status",
		"list_workspaces", "set_pane_flag",
	}
}

type wardenEventKind string

const (
	wardenDebrief wardenEventKind = "debrief"
	wardenTriage  wardenEventKind = "triage"
	// wardenBrief is the on-demand, pane-agnostic full-fleet rollup — the
	// "where was I?" brief. Not auto-detected; only fired by RunNow (the
	// /api/warden/run-now endpoint), and allowed even while disabled so it
	// works as a manual tool.
	wardenBrief wardenEventKind = "brief"
)

// wardenRunRecord is the in-memory summary of a finished run, surfaced by the
// isolated /api/warden/runs endpoint. Deliberately NOT routed through the
// schedules/reports rail — the warden owns its own reporting surface so it
// can't perturb the existing inbox.
type wardenRunRecord struct {
	Kind      wardenEventKind `json:"kind"`
	PaneID    string          `json:"paneId,omitempty"`
	PaneTitle string          `json:"paneTitle,omitempty"`
	At        time.Time       `json:"at"`
	Status    string          `json:"status"` // "ok" | "timeout" | "error"
	Detail    string          `json:"detail,omitempty"`
	Summary   string          `json:"summary,omitempty"`
	LogFile   string          `json:"logFile,omitempty"`
}

const wardenMaxRunRecords = 30

type wardenEvent struct {
	Kind      wardenEventKind
	PaneID    string
	PaneTitle string
	Cwd       string
	// Reason is the needs-user reason (triage) or exit summary (debrief),
	// carried into the run prompt.
	Reason    string
	Episode   string
	AutoFlags bool
}

// wardenPaneTrack is the per-pane memory the detector carries across ticks.
type wardenPaneTrack struct {
	wasWorkingClaude bool      // saw this pane running a Claude/native session
	exited           bool      // already emitted a debrief for this exit
	needsSince       time.Time // when the current needs-user episode began (zero = not in one)
	episode          string
	triagedEpisode   string        // episode already accepted for a run
	lastState        PaneAttention // last attention seen
}

// execResult is the minimal outcome the summoner records per run.
type execResult struct {
	output   []byte
	err      error
	timedOut bool
}

// wardenExecFn runs the headless judgment process. Injectable so tests drive
// the summoner without shelling out to claude.
type wardenExecFn func(ctx context.Context, bin string, args []string, cwd, prompt string) execResult

type Warden struct {
	settings *wardenSettingsStore
	mgr      *Manager
	panes    paneStore
	scopes   *mcpScopeStore
	uiBus    *uiBus
	dataDir  string
	mcpURL   string

	mu      sync.Mutex
	tracks  map[string]*wardenPaneTrack
	runAt   []time.Time       // spawn timestamps, for the per-hour cap
	running bool              // single-flight: at most one warden run at a time
	recent  []wardenRunRecord // newest-first ring of finished runs (isolated reports)
	// skipReported holds events already reported as skipped (busy or capped),
	// so a waiting event is logged/published once per episode, not every tick.
	skipReported map[string]bool

	// exec is the process runner; real spawn by default, swapped in tests.
	exec wardenExecFn
	// resolveBin finds the claude binary; injectable so tests don't depend on
	// a real install.
	resolveBin func() (string, error)
	// onRun, when set, is called with each finished run's metadata — PR4
	// wires this to the reports rail. Nil = just log.
	onRun func(ev wardenEvent, start time.Time, res execResult, logFile string)
	// clock is injectable so tests can advance time deterministically.
	clock func() time.Time
}

func NewWarden(settings *wardenSettingsStore, mgr *Manager, scopes *mcpScopeStore, bus *uiBus, dataDir, addr string) *Warden {
	w := &Warden{
		settings:   settings,
		mgr:        mgr,
		scopes:     scopes,
		uiBus:      bus,
		dataDir:    dataDir,
		mcpURL:     wardenMCPURL(addr),
		tracks:     map[string]*wardenPaneTrack{},
		clock:      time.Now,
		resolveBin: resolveClaudeBin,
	}
	w.exec = w.realExec
	w.loadRuns() // restore brief history across restarts
	return w
}

// wardenMCPURL turns the daemon listen addr into a loopback URL the spawned
// child can dial. The daemon commonly binds a WILDCARD host — ":8787",
// "0.0.0.0:8787" (the desktop app's launch flag), or "[::]:8787" — none of
// which are dialable *connect* addresses. The warden always reaches its own
// daemon over loopback, so normalize any unspecified/wildcard host to
// 127.0.0.1. (Getting this wrong hands the child "http://0.0.0.0:8787/mcp",
// which silently fails to connect and the run reports "MCP tools not
// connected" — the exact production failure this replaces.)
func wardenMCPURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// No host:port split (bare port or malformed) — best-effort loopback.
		host, port = "", addr
		if strings.HasPrefix(addr, ":") {
			port = strings.TrimPrefix(addr, ":")
		}
	}
	switch host {
	case "", "0.0.0.0", "::", "*":
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s/mcp", net.JoinHostPort(host, port))
}

func (w *Warden) now() time.Time {
	if w.clock != nil {
		return w.clock()
	}
	return time.Now()
}

// Start runs the summoner loop until ctx is cancelled. It always ticks (cheap)
// but only detects+spawns when enabled — so toggling the setting takes effect
// on the next tick with no restart.
func (w *Warden) Start(ctx context.Context) {
	tick := time.NewTicker(wardenTickInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.tickOnce(ctx)
		}
	}
}

func (w *Warden) tickOnce(ctx context.Context) {
	// Cleanup is independent of both switches: an opted-in flag already on a
	// pane must still go away when its episode ends after an opt-out.
	w.cleanupWardenFlags()
	if w.settings == nil || !w.settings.Enabled() {
		// Keep pane tracking warm even while disabled? No — a disabled
		// warden should carry no state, so enabling it starts clean and
		// doesn't immediately fire on a backlog of pre-existing exits.
		w.mu.Lock()
		w.tracks = map[string]*wardenPaneTrack{}
		w.mu.Unlock()
		return
	}
	events := w.detect(w.now())
	w.pruneSkipReports(events)
	for _, ev := range events {
		if !w.tryReserveSlot() {
			// Detection does not acknowledge events. Every still-active event
			// in this batch remains eligible on the next tick.
			if w.firstSkip(ev) {
				log.Printf("warden: skipped %s for pane %s (busy or rate-capped; will retry)", ev.Kind, ev.PaneID)
				w.publish(uiEvent{Type: "warden-skipped", PaneID: ev.PaneID, Text: string(ev.Kind)})
			}
			return
		}
		if !w.ackEvent(ev) {
			w.unreserveSlot()
			continue
		}
		go w.runEvent(ctx, ev)
	}
}

func wardenSkipKey(ev wardenEvent) string {
	return string(ev.Kind) + "\x00" + ev.PaneID + "\x00" + ev.Episode
}

// firstSkip reports whether ev has not been reported as skipped yet.
func (w *Warden) firstSkip(ev wardenEvent) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.skipReported == nil {
		w.skipReported = map[string]bool{}
	}
	key := wardenSkipKey(ev)
	if w.skipReported[key] {
		return false
	}
	w.skipReported[key] = true
	return true
}

// pruneSkipReports forgets skip reports for events no longer pending, so the
// map stays bounded and a later episode is reported again.
func (w *Warden) pruneSkipReports(events []wardenEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.skipReported) == 0 {
		return
	}
	live := make(map[string]bool, len(events))
	for _, ev := range events {
		live[wardenSkipKey(ev)] = true
	}
	for key := range w.skipReported {
		if !live[key] {
			delete(w.skipReported, key)
		}
	}
}

func (w *Warden) cleanupWardenFlags() {
	for _, p := range w.mgr.List() {
		w.cleanupWardenFlag(p)
	}
}

func (w *Warden) cleanupWardenFlag(p *Pane) {
	snap := p.Snapshot()
	if !snap.Flagged || snap.FlagOwner != wardenScopeIdentity || snap.FlagEpisode == "" || snap.FlagRevision == "" {
		return
	}
	state, _, episode := p.attentionEpisodeNow()
	if state == "" || (state == AttentionNeedsUser && episode == snap.FlagEpisode) {
		return
	}
	err := w.mgr.changeCurrentFlag(p, w.panes, func(current PaneSnapshot) error {
		if !current.Flagged || current.FlagOwner != wardenScopeIdentity || current.FlagEpisode != snap.FlagEpisode || current.FlagRevision != snap.FlagRevision {
			return errWardenFlagChanged
		}
		state, _, episode := p.attentionEpisodeNow()
		if state == "" || (state == AttentionNeedsUser && episode == current.FlagEpisode) {
			return errWardenFlagChanged
		}
		return nil
	}, false, "", "", "")
	if err != nil && err != errWardenFlagChanged {
		log.Printf("warden: clear flag on %s: %v", snap.ID, err)
	}
}

// detect compares the live fleet against per-pane tracking and returns the
// events that should fire this tick. Holds w.mu; pure w.r.t. side effects
// except updating the tracking maps (which is the point).
func (w *Warden) detect(now time.Time) []wardenEvent {
	w.mu.Lock()
	defer w.mu.Unlock()

	triageAfter := time.Duration(w.settings.Snapshot().TriageAfterMin) * time.Minute
	var events []wardenEvent
	seen := map[string]bool{}

	for _, p := range w.mgr.List() {
		snap := p.Snapshot()
		seen[snap.ID] = true
		tr := w.tracks[snap.ID]
		if tr == nil {
			tr = &wardenPaneTrack{}
			w.tracks[snap.ID] = tr
		}

		isClaude := snap.ClaudeSessionID != "" || snap.Kind == "native" || p.HasClaudeDescendant()
		if snap.Status != PaneExited && isClaude {
			tr.wasWorkingClaude = true
		}

		// --- debrief: a real Claude/native session just exited ---
		if snap.Status == PaneExited && tr.wasWorkingClaude && !tr.exited {
			if atomic.LoadInt64(&p.TotalStrippedChars) >= wardenDebriefMinOutput {
				events = append(events, wardenEvent{
					Kind:      wardenDebrief,
					PaneID:    snap.ID,
					PaneTitle: snap.Title,
					Cwd:       snap.Cwd,
					Reason:    wardenExitReason(snap),
				})
			} else {
				tr.exited = true
			}
		}

		// --- triage: needs-user sustained past the delay ---
		att, reason, episode := p.attentionEpisodeNow()
		if att == AttentionNeedsUser {
			if episode != tr.episode {
				tr.episode = episode
				tr.needsSince = now
				tr.triagedEpisode = ""
			}
			if !tr.needsSince.IsZero() && now.Sub(tr.needsSince) >= triageAfter && tr.triagedEpisode != episode {
				events = append(events, wardenEvent{
					Kind:      wardenTriage,
					PaneID:    snap.ID,
					PaneTitle: snap.Title,
					Cwd:       snap.Cwd,
					Reason:    reason,
					Episode:   episode,
				})
			}
		} else {
			// Left the needs-user episode — reset so a future one re-triages.
			tr.needsSince = time.Time{}
			tr.episode = ""
			tr.triagedEpisode = ""
		}
		tr.lastState = att
	}

	// Forget panes fully removed from the manager (Kill deletes them).
	for id := range w.tracks {
		if !seen[id] {
			delete(w.tracks, id)
		}
	}
	return events
}

var errWardenFlagChanged = fmt.Errorf("flag changed before Warden cleanup")

// ackEvent commits only a successfully reserved, still-valid event. It also
// prevents a second detected event in the same batch from being consumed.
func (w *Warden) ackEvent(ev wardenEvent) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	tr := w.tracks[ev.PaneID]
	p, ok := w.mgr.Get(ev.PaneID)
	if !ok || tr == nil {
		return false
	}
	if ev.Kind == wardenDebrief {
		if tr.exited || p.Snapshot().Status != PaneExited {
			return false
		}
		tr.exited = true
		return true
	}
	state, _, episode := p.attentionEpisodeNow()
	if state != AttentionNeedsUser || episode != ev.Episode || tr.episode != ev.Episode || tr.triagedEpisode == ev.Episode {
		return false
	}
	tr.triagedEpisode = ev.Episode
	return true
}

func (w *Warden) unreserveSlot() {
	w.mu.Lock()
	w.running = false
	if len(w.runAt) > 0 {
		w.runAt = w.runAt[:len(w.runAt)-1]
	}
	w.mu.Unlock()
}

func wardenExitReason(snap PaneSnapshot) string {
	if snap.ExitCode != nil {
		return fmt.Sprintf("exited with code %d", *snap.ExitCode)
	}
	return "exited"
}

// tryReserveSlot enforces single-flight + the per-hour cap. Returns true and
// records a run start on success.
func (w *Warden) tryReserveSlot() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running {
		return false
	}
	now := w.now()
	cutoff := now.Add(-time.Hour)
	kept := w.runAt[:0]
	for _, t := range w.runAt {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	w.runAt = kept
	if len(w.runAt) >= w.settings.Snapshot().MaxRunsPerHour {
		return false
	}
	w.runAt = append(w.runAt, now)
	w.running = true
	return true
}

func (w *Warden) releaseSlot() {
	w.mu.Lock()
	w.running = false
	w.mu.Unlock()
}

// runEvent mints a scoped token, spawns the judgment run, and records it.
func (w *Warden) runEvent(ctx context.Context, ev wardenEvent) {
	defer w.releaseSlot()

	bin, err := w.resolveBin()
	if err != nil {
		log.Printf("warden: cannot run %s for pane %s: %v", ev.Kind, ev.PaneID, err)
		return
	}

	allowed := make(map[string]bool)
	for _, tool := range wardenReadAnnotateTools() {
		allowed[tool] = true
	}
	scope := &mcpScope{Identity: wardenScopeIdentity, Tools: allowed}
	if ev.Kind == wardenTriage {
		scope.WardenPaneID, scope.WardenEpisode = ev.PaneID, ev.Episode
	}
	token := w.scopes.MintScoped(scope, wardenRunTimeout+time.Minute)
	defer w.scopes.Revoke(token)

	cfgPath, cleanup, err := w.writeMCPConfig(token)
	if err != nil {
		log.Printf("warden: mcp config for pane %s: %v", ev.PaneID, err)
		return
	}
	defer cleanup()

	cwd := ev.Cwd
	if fi, statErr := os.Stat(cwd); statErr != nil || !fi.IsDir() {
		cwd, _ = os.UserHomeDir()
	}
	args := wardenClaudeArgs(cfgPath, w.settings.Snapshot().Model, wardenReadAnnotateTools())
	ev.AutoFlags = w.settings.Snapshot().AutoFlags
	prompt := wardenPrompt(ev)

	start := w.now()
	runCtx, cancel := context.WithTimeout(ctx, wardenRunTimeout+30*time.Second)
	defer cancel()
	res := w.exec(runCtx, bin, args, cwd, prompt)

	header := fmt.Sprintf("# warden %s · pane=%s (%s) · cwd=%s · started=%s\n\n",
		ev.Kind, ev.PaneID, ev.PaneTitle, cwd, start.Format(time.RFC3339))
	logFile := writeRunLog(w.runDir(), start, append([]byte(header), res.output...))

	status, statusIcon := "ok", "🦉"
	detail := string(ev.Kind) + " done"
	if res.timedOut {
		status, statusIcon, detail = "timeout", "⚠️", string(ev.Kind)+" timed out"
	} else if res.err != nil {
		status, statusIcon, detail = "error", "⚠️", string(ev.Kind)+" failed: "+res.err.Error()
	}
	log.Printf("warden: %s pane=%s %s", ev.Kind, ev.PaneID, detail)
	w.recordRun(wardenRunRecord{
		Kind:      ev.Kind,
		PaneID:    ev.PaneID,
		PaneTitle: ev.PaneTitle,
		At:        start,
		Status:    status,
		Detail:    detail,
		Summary:   wardenOutputPreview(res.output),
		LogFile:   logFile,
	})
	w.publish(uiEvent{
		Type:       "toast",
		Text:       fmt.Sprintf("%s Warden: %s", statusIcon, detail),
		SkipRecent: true,
	})
	if w.onRun != nil {
		w.onRun(ev, start, res, logFile)
	}
}

// recordRun prepends a finished-run record to the newest-first ring, capped,
// and persists the ring so history survives a daemon restart.
func (w *Warden) recordRun(rec wardenRunRecord) {
	w.mu.Lock()
	w.recent = append([]wardenRunRecord{rec}, w.recent...)
	if len(w.recent) > wardenMaxRunRecords {
		w.recent = w.recent[:wardenMaxRunRecords]
	}
	w.saveRunsLocked()
	w.mu.Unlock()
}

func (w *Warden) runsIndexPath() string {
	return filepath.Join(w.runDir(), "index.json")
}

// saveRunsLocked persists the run ring to <runDir>/index.json. Without this
// the in-memory ring resets on every daemon update/swap, so the Warden tab
// and inbox would forget every brief the moment the daemon restarts. Caller
// holds w.mu. Best-effort: a failed persist must never break a run.
func (w *Warden) saveRunsLocked() {
	if err := os.MkdirAll(w.runDir(), 0o700); err != nil {
		return
	}
	b, err := json.Marshal(w.recent)
	if err != nil {
		return
	}
	_ = writeFileAtomicSynced(w.runsIndexPath(), b, 0o600)
}

// loadRuns restores the ring from disk at startup. Best-effort: a missing or
// corrupt index just starts empty.
func (w *Warden) loadRuns() {
	b, err := os.ReadFile(w.runsIndexPath())
	if err != nil {
		return
	}
	var recs []wardenRunRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		return
	}
	if len(recs) > wardenMaxRunRecords {
		recs = recs[:wardenMaxRunRecords]
	}
	w.mu.Lock()
	w.recent = recs
	w.mu.Unlock()
}

// RecentRuns returns a copy of the finished-run ring (newest first).
func (w *Warden) RecentRuns() []wardenRunRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]wardenRunRecord, len(w.recent))
	copy(out, w.recent)
	return out
}

// RunNow fires an on-demand full-fleet brief immediately, subject to the same
// single-flight + hourly cap. Allowed even while disabled — it is a manual
// tool, not the autonomous loop. Returns false if a run is already in flight
// or the hourly cap is hit.
func (w *Warden) RunNow(ctx context.Context) bool {
	if !w.tryReserveSlot() {
		return false
	}
	home, _ := os.UserHomeDir()
	ev := wardenEvent{Kind: wardenBrief, Cwd: home, PaneTitle: "fleet"}
	go w.runEvent(ctx, ev)
	return true
}

// wardenOutputPreview is the lede of a run's output for the runs listing —
// keyed-secret-scrubbed and length-capped, so a listing never becomes a
// credential exfiltration surface even if the model echoed something.
func wardenOutputPreview(out []byte) string {
	text := strings.TrimSpace(redactTerminalTail(string(out)))
	const max = 280
	if r := []rune(text); len(r) > max {
		return strings.TrimSpace(string(r[:max])) + "…"
	}
	return text
}

func (w *Warden) runDir() string {
	return filepath.Join(w.dataDir, "warden-runs")
}

func (w *Warden) publish(e uiEvent) {
	if w.uiBus != nil {
		w.uiBus.publish(e)
	}
}

// writeMCPConfig writes a 0600 --mcp-config pointing the child at /mcp with
// the scoped bearer token, and returns a cleanup that removes it.
func (w *Warden) writeMCPConfig(token string) (string, func(), error) {
	cfg, err := wardenMCPConfigJSON(w.mcpURL, token)
	if err != nil {
		return "", func() {}, err
	}
	dir := w.runDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", func() {}, err
	}
	f, err := os.CreateTemp(dir, "mcp-*.json")
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	if _, err := f.Write(cfg); err != nil {
		f.Close()
		_ = os.Remove(path)
		return "", func() {}, err
	}
	_ = f.Chmod(0o600)
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", func() {}, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}

// wardenMCPConfigJSON builds the --mcp-config body: a single http server
// carrying the scoped token as an Authorization header. Combined with
// --strict-mcp-config on the argv, this is the ONLY MCP server the child
// sees — the user-scope tokenless registration is ignored, so the child
// cannot escape its scope by calling the full-access server.
func wardenMCPConfigJSON(mcpURL, token string) ([]byte, error) {
	body := map[string]any{
		"mcpServers": map[string]any{
			"soluterminal": map[string]any{
				"type": "http",
				"url":  mcpURL,
				"headers": map[string]any{
					"Authorization": "Bearer " + token,
				},
			},
		},
	}
	return json.Marshal(body)
}

// wardenClaudeArgs builds the headless argv. --strict-mcp-config is load-
// bearing for the security model (see wardenMCPConfigJSON). --allowedTools
// pre-authorizes exactly the scoped MCP tools so headless runs don't stall on
// a permission prompt (which can't be answered) — everything else is
// auto-denied, a second wall behind the daemon-side scope.
func wardenClaudeArgs(cfgPath, model string, tools []string) []string {
	args := []string{
		"-p",
		"--output-format", "text",
		"--strict-mcp-config",
		"--mcp-config", cfgPath,
		"--allowedTools", strings.Join(wardenToolFlagNames(tools), ","),
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

// wardenToolFlagNames maps bare tool names to their MCP-qualified form as
// Claude Code sees them: mcp__<server>__<tool>.
func wardenToolFlagNames(tools []string) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = "mcp__soluterminal__" + t
	}
	return out
}

// realExec is the default process runner. Prompt on stdin (sidesteps argv
// parsing for dash-leading prompts, per schedules_task.go's note).
func (w *Warden) realExec(ctx context.Context, bin string, args []string, cwd, prompt string) execResult {
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	cmd.Env = providerChildEnvForBin(bin, os.Environ())
	cmd.Stdin = strings.NewReader(prompt)
	applyDetached(cmd)
	out := &boundedBuf{limit: wardenLogLimit}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := startWithoutCoreDumps(cmd); err != nil {
		return execResult{err: fmt.Errorf("spawn: %w", err)}
	}
	kill := time.AfterFunc(wardenRunTimeout, func() { _ = killChildTree(cmd.Process) })
	werr := waitOwnedCommand(cmd)
	timedOut := !kill.Stop()
	// Also honor ctx cancellation (daemon shutdown).
	select {
	case <-ctx.Done():
		_ = killChildTree(cmd.Process)
	default:
	}
	return execResult{output: out.buf, err: werr, timedOut: timedOut}
}
