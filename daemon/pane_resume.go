package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

type fleetPaneEligibility struct {
	Snapshot PaneSnapshot
	Busy     bool
	Blocker  string
}

// fleetRespawnEligibility is the fail-closed policy for destructive in-place
// respawns (per-pane restart, live-auth migration). Busy work waits; missing durable
// provider identity blocks destructive replacement.
func fleetRespawnEligibility(p *Pane) fleetPaneEligibility {
	return fleetRespawnEligibilityVia(p, claudeTranscriptResumable)
}

func fleetRespawnEligibilityVia(p *Pane, claudeResumable func(cwd, sessionID, knownPath string) bool) fleetPaneEligibility {
	if p == nil {
		return fleetPaneEligibility{Blocker: "pane is unavailable"}
	}
	// Adopt a harness sidecar written since the last poll BEFORE judging (and
	// persisting) the pane's identity. Restart is a kill, and a pane whose
	// client just recorded its session still carries only a stale Claude id;
	// without this the eligibility check would pass on that id and the spawn
	// would relaunch Claude over the live DeepSeek conversation.
	p.refreshHarnessSession()
	live := p.Snapshot()
	if live.Agent == agentMuse && (p.codexIdentityBlocked.Load() || strings.TrimSpace(live.MuseSessionID) == "") {
		return fleetPaneEligibility{Snapshot: live, Blocker: "foreground Muse conversation identity is not currently verified"}
	}
	snap, _ := healPaneSnapshot(live)
	result := fleetPaneEligibility{Snapshot: snap}
	if snap.Status != PaneRunning {
		result.Blocker = "pane agent is not running"
		return result
	}
	result.Busy = fleetPaneBusyForRestart(p)
	// A managed terminal harness (DeepSeek today) owns its conversation in the
	// client, not in a Claude transcript or Codex rollout. Its sidecar session
	// is the durable identity, so the provider-specific transcript checks below
	// do not apply — and a stale ClaudeSessionID left on a reused shell pane
	// must never gate (or hijack) the restart.
	if strings.TrimSpace(snap.HarnessProvider) != "" {
		return result
	}
	// A managed launch carries its provider on argv, so it can be replayed
	// safely even before the client has written its sidecar.
	if cmdLaunchesManagedHarness(snap.Cmd) {
		return result
	}
	// A managed client is live in this pane but has not recorded a session yet
	// (the sidecar write is asynchronous). Fail closed: falling through would
	// resume the stale Claude/Codex id. A fresh restart replays the recorded
	// shell, never the agent, and the client re-records on the next launch.
	if p.HasManagedHarnessDescendant() {
		result.Blocker = "managed harness session identity is not yet recorded"
		return result
	}
	agent := effectiveAgentKind(snap.Agent)
	conversationID := conversationIDForAgent(snap.Agent, snap.ClaudeSessionID, snap.CodexThreadID, snap.MuseSessionID)
	if conversationID == "" {
		result.Blocker = "no durable " + agent + " conversation to resume"
		return result
	}
	switch agent {
	case agentClaude:
		if !claudeResumable(snap.Cwd, conversationID, strings.TrimSpace(snap.ClaudeTranscriptPath)) {
			result.Blocker = "Claude transcript is not durably resumable"
		}
	case agentCodex:
		if p.codexIdentityBlocked.Load() {
			result.Blocker = "Codex thread identity is not currently verified"
		} else if !codexRolloutVerifiedOnDisk(conversationID) {
			result.Blocker = "Codex rollout is not durably resumable"
		}
	}
	return result
}

func fleetPaneBusyForRestart(p *Pane) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	kind, agent := p.Kind, p.Agent
	p.mu.Unlock()
	if kind == "native" {
		// This restart latch is cleared only by authoritative provider completion
		// or confirmed process teardown. nativeInTurn also drives UI state and may
		// settle after prolonged silence, which is not proof that a call ended.
		return p.fleetTurnAdmitted.Load() || p.hasNativePending()
	}
	state := p.computeState()
	if p.fleetTurnAdmitted.Load() {
		if atomic.LoadInt64(&p.TotalStrippedChars) <= p.fleetTurnOutputWatermark.Load() {
			return true
		}
		settled := state == StateChoicePrompt || p.claudePromptVisible() || p.codexPromptVisible()
		// Muse has no terminal-chrome detector equivalent to Claude/Codex. Require
		// the generic parser to identify an actual input prompt; mere quiet/idle
		// after one output byte is not proof that the provider returned control.
		if effectiveAgentKind(agent) == agentMuse && state == StateAwaitingInput {
			settled = true
		}
		// Terminal harnesses (DeepSeek/Pi) and plain shells render no
		// Claude/Codex prompt, so the marker check above never fires and the
		// admission latch left an idle pane "busy" forever — per-pane restart
		// refused. A pane with no live agent process that
		// has been quiet for a full settle window after producing output is the
		// best available hand-back. Agent panes keep the strict marker check so a
		// silent mid-turn provider is never killed.
		if !settled && p.terminalPaneQuietForRestart(time.Now()) {
			settled = true
		}
		// Muse Code TUI panes (an explicit `muse` agent, or a shell running
		// the `muse` TUI with a live muse descendant, e.g. Muse Code): none
		// of the detectors above can fire for them — the Claude/Codex prompt
		// regexes never match the Muse screen, and
		// terminalPaneQuietForRestart deliberately excludes muse panes — so a
		// finished turn wedged the latch "busy" forever (a clearly idle pane
		// refusing restart). Settle on the "◆ Worked for …" completion marker
		// positioned after the admission watermark instead.
		if !settled && (effectiveAgentKind(agent) == agentMuse || p.HasMuseDescendant()) {
			settled = p.museTurnDoneSinceAdmission()
		}
		if settled {
			p.fleetTurnAdmitted.Store(false)
			p.fleetTurnOutputWatermark.Store(0)
		} else {
			return true
		}
	}
	return p.agentMidTurnForState(state) || state == StateChoicePrompt
}

// terminalRestartQuietWindow is how long a terminal pane with no agent process
// must be quiet before its admitted turn is treated as settled for restart
// safety. Sized longer than computeState's 3s idle threshold so a provider's
// own spinner/live line (which keeps LastActivityMs fresh) always wins first.
const terminalRestartQuietWindow = 10 * time.Second

// terminalPaneQuietForRestart is the restart-safety hand-back for terminal
// panes that have no Claude/Codex prompt to detect — managed harnesses like
// DeepSeek and Pi, and plain shells. After the pane has produced output and then
// been quiet for a full settle window with no agent process attached, the
// admitted turn is treated as finished. Agent panes (claude/codex/muse)
// deliberately do NOT use this: their silent stretches can be a live turn, so
// they keep the strict marker check in fleetPaneBusyForRestart.
func (p *Pane) terminalPaneQuietForRestart(now time.Time) bool {
	if p == nil || p.Kind == "native" {
		return false
	}
	// Explicit Codex/Muse panes have their own hand-back detector
	// (codexPromptVisible / StateAwaitingInput); keep the strict check for them
	// even when this scan sees no live agent process.
	p.mu.Lock()
	agent := effectiveAgentKind(p.Agent)
	p.mu.Unlock()
	if agent == agentCodex || agent == agentMuse {
		return false
	}
	if p.HasClaudeDescendant() || p.HasCodexDescendant() || p.HasMuseDescendant() {
		return false
	}
	last := atomic.LoadInt64(&p.LastActivityMs)
	if last == 0 {
		return true
	}
	return now.Sub(time.UnixMilli(last)) >= terminalRestartQuietWindow
}

// handleResumePane brings a suspended (exited-but-resumable) agent pane back to
// life IN PLACE — same pane id and durable provider conversation —
// instead of forcing the user to open a new pane. It is the live-exit counterpart
// to the daemon-restart reattach loop (which already resumes dead Claude panes):
// here the daemon is up, the pane's claude process exited on its own, and the
// client clicked "Resume session".
//
// The pane must be Status==exited with its provider conversation id captured
// (set true on the DTO as `resumable`). A plain shell or user-killed pane is not
// resumable and returns 400/404.
func (a *apiServer) handleResumePane(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.mgr.Get(id)
	if !ok {
		http.Error(w, "pane not found", http.StatusNotFound)
		return
	}
	// Adopt a sidecar written since the last poll before deciding whether the
	// pane is resumable: the client-recorded harness session is the durable
	// identity, and a stale Claude id must not stand in for it.
	p.refreshHarnessSession()
	snap := p.Snapshot()
	if snap.Status != PaneExited {
		// Already live (e.g. another client resumed it, or it never exited).
		// Idempotent success — hand back the current DTO so the caller
		// reconciles to the running pane.
		writeJSON(w, http.StatusOK, makePaneDTO(p, a.aria, a.attention))
		return
	}
	var identityNotes []string
	snap, identityNotes = healPaneSnapshot(snap)
	for _, note := range identityNotes {
		log.Printf("resume pane %s: identity heal: %s", snap.ID, note)
	}
	if paneSnapshotConversationID(snap) == "" && snap.RecoveryError == "" {
		http.Error(w, "pane is not resumable (no agent conversation)", http.StatusBadRequest)
		return
	}
	// Single-flight: a double-click or a second client must not race a second
	// spawn against the first (which would hit Spawn's duplicate-id guard or
	// kill the just-resumed pane). Shared with refresh + switch-mode.
	if !p.tryAcquireRespawn() {
		http.Error(w, "respawn already in progress", http.StatusConflict)
		return
	}
	defer p.releaseRespawn()
	snap = p.Snapshot()
	snap, _ = healPaneSnapshot(snap)

	// The client posts its live measured viewport {cols,rows} so the fresh PTY
	// comes up at the CURRENT window width, not the last-persisted size. The body
	// is optional and best-effort: an older client sends nothing, so live stays
	// 0,0 and resumeExitedPane falls back to the persisted dims (marked
	// unverified). A malformed body is likewise ignored, never a 400.
	var live struct {
		resizeReq
		Cwd string `json:"cwd,omitempty"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&live)
	}

	if snap.RecoveryError != "" && strings.TrimSpace(live.Cwd) != "" {
		if snap.ClaudeTranscriptPath == "" && effectiveAgentKind(snap.Agent) == agentClaude {
			snap.ClaudeTranscriptPath = resolveTranscriptPath(snap.Cwd, snap.ClaudeSessionID)
		}
		snap.Cwd = expandHome(strings.TrimSpace(live.Cwd))
	}
	newPane, err := a.resumeExitedPane(snap, live.Cols, live.Rows)
	if err != nil {
		// The session couldn't be resumed (e.g. it expired and `claude
		// --resume` exits immediately). respawnInPlace retains its saved metadata
		// and publishes a recovery tile, so later polls and restarts keep it.
		status := http.StatusInternalServerError
		if errors.Is(err, errNativeSessionDied) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, makePaneDTO(newPane, a.aria, a.attention))
}

// resumeExitedPane frees the dead pane's id from the manager, removes its stale
// agent socket, and re-spawns the provider's terminal resume CLI or headless
// native argv under the SAME id — reusing the proven switch-mode recipe
// (spawnPaneForRequest + socket removal + retry-on-quick-exit + health check).
// State the spawn opts don't carry (auto-mode, flags, prompt history, createdAt)
// is restored onto the fresh pane, mirroring ResumeClaudePane, so a resumed pane
// keeps its auto-approval policy, flag and last-prompts strip.
func (a *apiServer) resumeExitedPane(snap PaneSnapshot, liveCols, liveRows uint16) (*Pane, error) {
	opts := resumeSpawnOpts(snap, liveCols, liveRows)
	if snap.IdentityBlocked || paneSnapshotConversationID(snap) == "" {
		// Explicit retry of an unidentified terminal asks the provider to show
		// its conversation picker; it never guesses a thread from recency.
		opts.RequireResumableConversation = false
		opts.ClaudeSessionID, opts.CodexThreadID, opts.MuseSessionID = "", "", ""
		opts.ClaudeTranscriptPath = ""
		opts.HarnessProvider, opts.HarnessSessionID = "", ""
		opts.Env = nil
		if snap.Kind == "native" {
			return nil, fmt.Errorf("conversation identity is missing; choose the original conversation before retrying")
		}
		switch {
		case snap.Agent == agentCodex:
			opts.Cmd = []string{"codex", "--no-alt-screen", "resume", "--all"}
		case snap.Agent == agentMuse:
			return nil, fmt.Errorf("Muse conversation identity is missing; choose the original conversation before retrying")
		case cmdRunsClaude(snap.Cmd) || snap.Agent == agentClaude:
			opts.Cmd = []string{"claude", "--resume"}
		default:
			opts.Cmd = cloneStringSlice(snap.Cmd)
		}
	}
	return a.respawnInPlace(snap, opts)
}

// resumeSpawnOpts builds the SpawnOpts for an in-place resume. Extracted (and
// pure) so the dims decision — the crux of the post-shutdown garble fix — is
// unit-testable without spawning a real agent. Mirrors performSwitchMode's
// newOpts: cmd is the provider's interactive resume TUI for terminal; nil for
// native (Spawn rewrites it to the provider's headless protocol argv).
func resumeSpawnOpts(snap PaneSnapshot, liveCols, liveRows uint16) SpawnOpts {
	snap, _ = healPaneSnapshot(snap)
	harnessProvider := strings.TrimSpace(snap.HarnessProvider)
	if harnessProvider == "" {
		// A managed launch carries its provider on argv. Recover it so a pane
		// that has not yet written its sidecar still resumes as its harness,
		// never as the stale Claude id left on a reused shell.
		harnessProvider = harnessProviderFromCmd(snap.Cmd)
	}
	harnessSessionID := strings.TrimSpace(snap.HarnessSessionID)
	agent := effectiveAgentKind(snap.Agent)
	conversationID := conversationIDForAgent(snap.Agent, snap.ClaudeSessionID, snap.CodexThreadID, snap.MuseSessionID)
	opts := SpawnOpts{
		AutoMode:                     snap.AutoMode,
		NeverAutoAnswer:              snap.NeverAutoAnswer,
		ID:                           snap.ID,
		CreatedAt:                    snap.CreatedAt,
		RequireResumableConversation: conversationID != "" && harnessProvider == "",
		Cwd:                          snap.Cwd,
		Title:                        snap.Title,
		Cols:                         snap.Cols,
		Rows:                         snap.Rows,
		ClaudeSessionID:              strings.TrimSpace(snap.ClaudeSessionID),
		ClaudeTranscriptPath:         strings.TrimSpace(snap.ClaudeTranscriptPath),
		TitleOverride:                snap.TitleOverride,
		Origin:                       snap.Origin.clone(),
		Review:                       snap.Review.clone(),
		Identity:                     snap.Identity.clone(),
		Kind:                         snap.Kind,
		Agent:                        snap.Agent,
		CodexThreadID:                snap.CodexThreadID,
		MuseSessionID:                snap.MuseSessionID,
		HarnessSessionID:             harnessSessionID,
		HarnessProvider:              harnessProvider,
		// Carry the dead pane's durable "a turn was underway" record so Spawn
		// can tell the user a restart cut their response off (Codex's own
		// resumed history cannot distinguish that from a user interrupt).
		CodexTurnInFlight: snap.CodexTurnInFlight,
		Provider:          snap.Provider,
		Effort:            snap.Effort,
		Model:             snap.Model,
		Role:              snap.Role,
		ContextPack:       snap.ContextPack,
		NativeAllowlist:   snap.NativeAllowlist,
	}
	// A managed terminal harness owns its conversation, so resume by rebuilding
	// its launch with the resume hint on the child env — the harness-client twin
	// of `claude --resume`. This MUST precede the agent switch: a reused shell
	// pane can carry a stale ClaudeSessionID, and resuming that would relaunch
	// Claude over a live DeepSeek conversation. A hand-launched harness (the user
	// typed `deepseek` in a shell) has no managed argv to replay, so rebuild it
	// from the provider — a bare shell would not reopen the conversation.
	if (harnessProvider != "" || cmdLaunchesManagedHarness(snap.Cmd)) && snap.Kind != "native" {
		switch {
		case cmdLaunchesManagedHarness(snap.Cmd):
			// The pane's own command is already the managed launch.
			opts.Cmd = append([]string(nil), snap.Cmd...)
		case managedHarnessLaunchCmd != nil:
			if cmd, ok := managedHarnessLaunchCmd(harnessProvider); ok {
				opts.Cmd = cmd
			} else {
				opts.Cmd = append([]string(nil), snap.Cmd...)
			}
		default:
			// Launcher unavailable (pure tests / not prepared): replay the
			// recorded command. A shell cannot resume, but it also must never be
			// hijacked by the stale Claude id.
			opts.Cmd = append([]string(nil), snap.Cmd...)
		}
		// The hint is only meaningful to a managed client; a replayed shell must
		// not receive it.
		if harnessSessionID != "" && cmdLaunchesManagedHarness(opts.Cmd) {
			opts.Env = append(opts.Env, harnessResumeEnv(harnessSessionID)...)
		}
		if liveCols > 0 && liveRows > 0 {
			opts.Cols, opts.Rows = liveCols, liveRows
		} else {
			opts.DimsUnverified = true
		}
		return opts
	}
	if snap.Kind != "native" {
		switch agent {
		case agentCodex:
			opts.Cmd = []string{"codex", "resume", conversationID}
		case agentMuse:
			opts.Cmd = []string{"muse", "resume", conversationID}
		default:
			opts.Cmd = []string{"claude", "--resume", conversationID}
		}
		// Prefer the client's live measured viewport so the resumed terminal PTY
		// comes up at the CURRENT window width (verified — a real measurement).
		// Without it (older/mobile client sent no body), fall back to the
		// persisted dims but mark them unverified so the first client attach
		// re-measures and drives the real width — never blow the fresh PTY to a
		// stale panes.json size, which is the every-update / post-shutdown garble.
		if liveCols > 0 && liveRows > 0 {
			opts.Cols, opts.Rows = liveCols, liveRows
		} else {
			opts.DimsUnverified = true
		}
	}
	return opts
}

// tryAcquireRespawn CAS-claims the pane's shared respawn single-flight, returning
// false when another in-place kill→respawn (resume / refresh / switch-mode) is
// already running for this pane (the caller should 409). On true the caller MUST
// defer p.releaseRespawn(). See the `respawning` field doc on Pane.
func (p *Pane) tryAcquireRespawn() bool { return p.respawning.CompareAndSwap(false, true) }

// releaseRespawn releases the shared respawn single-flight.
func (p *Pane) releaseRespawn() { p.respawning.Store(false) }

func (a *apiServer) trackPaneRespawn(id string) func() {
	a.respawnPresenceMu.Lock()
	if a.respawnPresence == nil {
		a.respawnPresence = map[string]int{}
	}
	a.respawnPresence[id]++
	a.respawnPresenceMu.Unlock()
	return func() {
		a.respawnPresenceMu.Lock()
		defer a.respawnPresenceMu.Unlock()
		a.respawnPresence[id]--
		if a.respawnPresence[id] == 0 {
			delete(a.respawnPresence, id)
		}
	}
}

func (a *apiServer) paneRespawnInProgress(id string) bool {
	a.respawnPresenceMu.Lock()
	defer a.respawnPresenceMu.Unlock()
	return a.respawnPresence[id] > 0
}

// respawnInPlace is the shared kill→respawn primitive behind resume AND refresh:
// it frees the pane id, reaps any orphan agent that still owns the socket, then
// re-spawns under the SAME id with `opts` — removing the stale socket each
// attempt and retrying on a quick exit (the socket-handoff / session-lock race
// switch-mode also guards) — confirms the respawn is healthy, restores the
// non-spawn state SpawnOpts doesn't carry (auto-mode, flag, prompts, createdAt),
// and re-persists. Returns the NEW live *Pane.
//
// The caller MUST already hold the pane's respawn single-flight (tryAcquireRespawn)
// and, for a LIVE pane, MUST have settled any in-flight turn first — respawnInPlace
// does NOT interrupt (unlike performSwitchMode); a blind kill mid-turn would leave
// a dangling tool_use for `--resume` to repair. On total failure the original
// identity, layout reference and metadata remain as a retryable recovery tile.
func (a *apiServer) respawnInPlace(snap PaneSnapshot, opts SpawnOpts) (*Pane, error) {
	finishPresence := a.trackPaneRespawn(snap.ID)
	defer finishPresence()
	opts.AutoMode, opts.NeverAutoAnswer = snap.AutoMode, snap.NeverAutoAnswer
	opts.CreatedAt = snap.CreatedAt
	// Snapshot the agent's full descendant set BEFORE the kill. A managed
	// harness client is spawned DETACHED (its own session) by launch.mjs, so the
	// pane child's process-group teardown never signals it; once its wrapper dies
	// /proc reparents it to init and a descendant walk can no longer find it, yet
	// its DSH child keeps the session's flock — the next /resume then answers
	// "Internal error". Forcing a fresh walk here and reaping the survivors
	// releases the lock. (PID recycling cannot bite within the kill window;
	// Linux allocates pids monotonically until wraparound.)
	paneTree := snapshotProcessTreeMaxAge(0).descendantPids(snap.Pid)

	// Free the id from the manager. An exited pane lingers in m.panes (only an
	// explicit Kill deletes it) and Spawn rejects a duplicate id; a LIVE pane's
	// Kill reaches hardKillPid and reaps its agent. killPane on an already-dead
	// pane is instant: its done channel is closed, so Kill just drops the entry.
	if err := a.killPaneForRollback(snap.ID); err != nil {
		return nil, fmt.Errorf("respawn: free pane id: %w", err)
	}

	// Kill-before-replace: a SUSPENDED pane's Pane.Kill returns on its pre-closed
	// done channel WITHOUT reaching hardKillPid, so killPaneForRollback above does
	// NOT reap a still-alive orphan agent. If snap.Pid is alive and genuinely owns
	// this socket, kill its tree now — before we unlink the socket and spawn a
	// fresh agent — so the old agent can't linger as an unreachable duplicate. For
	// a live pane the kill above already reaped it, so this is a cheap no-op.
	if agentPidOwnsSocket(snap.Pid, snap.SocketPath) {
		killAgentTree(snap.Pid)
	}
	// Reap detached descendants (managed harness clients) that survived the
	// group teardown and are now orphaned.
	reapDetachedTree(paneTree)

	var newPane *Pane
	var err error
	for attempt := 0; attempt < switchModeSpawnAttempts; attempt++ {
		if attempt > 0 {
			if newPane != nil {
				_ = a.killPaneForRollback(newPane.ID)
				newPane = nil
			}
			time.Sleep(switchModeRetryDelay)
		}
		// Remove the stale socket so dialAgent links the FRESH agent, not the
		// just-killed one (the same handoff hazard switch-mode guards against).
		if snap.SocketPath != "" {
			_ = os.Remove(snap.SocketPath)
		}
		newPane, err = a.spawnPaneForRequest(opts)
		if err == nil {
			// Retries wait out the full health grace (see performSwitchMode).
			err = a.confirmSwitchHealthy(newPane, snap.Kind, attempt > 0)
		}
		if err == nil {
			break
		}
	}
	if err != nil {
		if newPane != nil {
			_ = a.killPaneForRollback(newPane.ID)
		}
		conversationID := conversationIDForAgent(snap.Agent, snap.ClaudeSessionID, snap.CodexThreadID, snap.MuseSessionID)
		log.Printf("respawn: pane=%s agent=%s conversation=%s failed: %v", snap.ID, effectiveAgentKind(snap.Agent), shortSessionID(conversationID), err)
		info := persistedFromSnapshot(snap)
		info.AgentPid = 0
		info.RecoveryError = err.Error()
		if kept := a.mgr.SuspendExisting(&info); kept != nil && a.panes != nil {
			if perr := a.panes.Put(kept); perr != nil {
				log.Printf("persist failed respawn %s: %v", snap.ID, perr)
			}
		}
		return nil, fmt.Errorf("respawn failed: %w", err)
	}

	// Approval settings were initialized by Spawn before publication. Restore
	// the remaining metadata without overwriting a concurrent settings update.
	restoreResumedState(newPane, snap)

	// Re-persist the now-running pane so it survives a later daemon restart.
	if a.panes != nil {
		if perr := a.panes.Put(newPane); perr != nil {
			log.Printf("respawn: pane=%s live but persist failed: %v", newPane.ID, perr)
		}
	}
	return newPane, nil
}

// restoreResumedState copies the pane state that SpawnOpts doesn't carry — the
// delegation-announcement override, flag, flag memo,
// and prompt history —
// from the exited snapshot onto the freshly respawned pane, and re-arms the grid
// when auto-mode is active. Mirrors ResumeClaudePane's restoration so a live
// resume preserves exactly what a daemon-restart resume does.
func restoreResumedState(p *Pane, snap PaneSnapshot) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.AnnounceDelegationEvents = snap.AnnounceDelegationEvents
	p.Flagged = snap.Flagged
	p.FlagMemo = snap.FlagMemo
	p.FlagOwner = snap.FlagOwner
	p.LastPrompts = cloneStringSlice(snap.LastPrompts)
	p.mu.Unlock()
	if p.Snapshot().AutoMode != "" {
		p.ensureGrid()
	}
}
