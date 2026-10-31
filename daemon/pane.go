package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/hinshun/vt10x"

	"soluterminal/grid"
)

const (
	ringBufferBytes = 8 * 1024 * 1024 // replay window for new WS subscribers + ARIA state-detection sample
	// Bumped 1MB → 8MB on 2026-04-27 (v0.0.336) after a Claude pane
	// with an aggressive ●-spinner animation rolled the buffer fast
	// enough to push a still-on-screen choice-prompt menu out of view.
	// xterm.js scrollback in the browser kept the menu visible, but
	// the daemon's classifier saw only spinner frames + an empty tail
	// → ariaWatch never saw choice-prompt → no auto-approval. 8MB
	// gives ~8× headroom against animation churn before the menu
	// emission gets evicted; per-pane memory cost is acceptable
	// (10 panes ≈ 80MB total).
	defaultCols     = 120
	defaultRows     = 32
	maxTerminalCols = 512
	maxTerminalRows = 256
	// minTerminalCols floors the width reported to the PTY. Claude Code's
	// Bubble Tea TUI has a layout minimum of ~40 cols; below it, cursor
	// positioning escape sequences break and text overwrites itself (seen
	// when 7-8 panes are tiled and fit() measures very narrow panes). The
	// client applies the same MIN_PANE_COLS=40 floor (web Pane.tsx); this
	// guard makes the daemon reject sub-40 widths too.
	minTerminalCols = 40

	// vtPaintEnabled gates the synthetic-paint emission built on top
	// of the vt10x virtual-terminal emulator. Introduced in v0.0.175
	// as the "real fix" for Codex/Ratatui ghost cells; disabled in
	// v0.0.183 after a user screenshot (solu-paste-081fdc1eb733c114
	// .png) revealed two failure modes in the field:
	//
	//   1. OSC response bytes (e.g. "\x1b]10;rgb:xxxx/yyyy/zzzz") —
	//      xterm.js's answer to Codex's color queries — were being
	//      absorbed by vt10x as literal cell chars instead of parsed
	//      and discarded. Our synthetic paint then emitted them back
	//      to xterm as text, leaving "10;rgb:e5e5/e9e9/f0f0" visible
	//      at the top of the pane.
	//   2. Ghost input widgets persisted at the bottom of the pane
	//      despite the v0.0.176/v0.0.177 vt-clear-on-resize path. The
	//      vt10x library is dated (last update 2022) and diverges
	//      from xterm.js on sequences Codex uses (truecolor, scroll
	//      regions, bracketed paste), so its grid state drifts from
	//      the actual terminal state.
	//
	// Disabling reverts Subscribe to the v0.0.174 raw-ringbuf-plus-
	// 1049h-prefix path (which shipped for weeks without introducing
	// new symptoms) and keeps the v0.0.172 clear-inject-to-client on
	// resize. vt.Write / vt.Resize calls are also skipped so the
	// emulator doesn't waste CPU consuming bytes it won't serve.
	// Flip to true if we integrate a more robust VT (charmbracelet
	// /x/vt, or a terminfo-faithful fork) and can re-test.
	vtPaintEnabled = false
)

// daemonEpoch is a per-PROCESS id, stamped once at package init. It's the base
// for every native pane's NativeEpoch (see nextNativeEpoch): a daemon restart
// gives a new base, so all panes get a fresh epoch and the client re-seeds (the
// auto-update-swap reattach case). NOT used directly as NativeEpoch anymore —
// that would be constant within a process and miss same-daemon respawns.
var daemonEpoch = uint64(time.Now().UnixNano())

// nativeEpochSeq makes NativeEpoch change on EVERY native (re)spawn, not just a
// daemon restart. A Term↔Focus switch kills the agent and respawns it into a
// BRAND-NEW *Pane with a FRESH empty ring (re-seq'd 1..N) while staying in the
// same daemon process — so daemonEpoch alone never changed, the client's
// epoch-based reset never fired, and the surviving cache dedup'd the new ring's
// low seqs as "already seen" → a BLANK pane after a few switches. Stamping a
// unique epoch per ensureNative() (which runs once per *Pane, on both fresh spawn
// and AttachExisting) gives the client the missing "ring restarted" signal on
// every respawn. Seeded off daemonEpoch so values stay unique across restarts
// too (new process → new base + counter).
var nativeEpochSeq atomic.Uint64

func nextNativeEpoch() uint64 { return daemonEpoch + nativeEpochSeq.Add(1) }

func saneTerminalDimensions(cols, rows int) (uint16, uint16, bool) {
	if cols <= 0 || rows <= 0 {
		return 0, 0, false
	}
	if cols > maxTerminalCols {
		cols = maxTerminalCols
	}
	if cols < minTerminalCols {
		cols = minTerminalCols
	}
	if rows > maxTerminalRows {
		rows = maxTerminalRows
	}
	return uint16(cols), uint16(rows), true
}

type PaneStatus string

const (
	PaneRunning PaneStatus = "running"
	PaneExited  PaneStatus = "exited"
)

// Pane is a logical terminal session backed by a `solud agent` process
// holding the actual PTY. The agent survives daemon restarts (spawned with
// Setsid=true), so sessions persist across Ctrl-C/restart of the daemon.
//
// Fields marked "immutable after spawn" are safe to read without p.mu.
// Everything else (Title, Status, ExitCode, Cols, Rows) must be accessed
// under p.mu — use Snapshot() to serialize or persist.
type Pane struct {
	// Delivery receipts are ephemeral and immutable after publication under mu.
	unsentMessage           *unsentPaneMessage
	deliveryMu              sync.Mutex
	inputRevision           atomic.Uint64
	Credential              *paneCredentialInfo `json:"credential,omitempty"`
	credentialSelection     func(provider string) string
	credentialTeamSelection func(provider string) string
	credentialLiveStatus    func(paneID string) codexLiveStatus
	codexLiveDataDir        string

	ID        string     `json:"id"` // immutable after spawn
	Cwd       string     `json:"cwd"`
	Cmd       []string   `json:"cmd"` // guarded by mu; replace slices instead of mutating their elements
	Title     string     `json:"title"`
	Status    PaneStatus `json:"status"`
	CreatedAt time.Time  `json:"createdAt"` // immutable after spawn
	ExitCode  *int       `json:"exitCode,omitempty"`
	// Resumable is set true by markExited when a Claude pane's process
	// exits on its own (not an explicit user kill) AND a claude session id
	// was captured. It tells the client to offer a "Resume session" button
	// (POST /api/panes/{id}/resume) instead of a dead-end. Guarded by mu;
	// rides PaneSnapshot/DTO. Not persisted — the suspended state is
	// in-memory only (ephemeral across daemon restarts).
	Resumable bool `json:"resumable,omitempty"`
	// RestartPaused marks a pane the daemon suspended across a restart while
	// Settings › Claude › After a restart is "Don't resume": the client shows
	// it as paused without offering to resume it (the person can still
	// resume it from the pane menu). Set once by suspendAfterRestart before
	// the pane is published; a resumed pane is a fresh Pane without it.
	RestartPaused bool   `json:"restartPaused,omitempty"`
	RecoveryError string `json:"recoveryError,omitempty"`
	Cols          uint16 `json:"cols"`
	Rows          uint16 `json:"rows"`
	// dimsVerified reports whether Cols/Rows describe the REAL PTY: true
	// from Spawn (we sized the PTY ourselves), false from AttachExisting
	// (dims came from panes.json, which is only written at spawn/resume-
	// style events and drifts arbitrarily far from the live PTY), set true
	// again by adoptAgentDims (the agent reported its size, FrameDims) or
	// by the first applied client Resize. While false, PaneSnapshot flags
	// the dims so clients don't seed their attach resize with a stale
	// value and blow the real PTY to it — the "every app update garbles
	// all panes" bug. Guarded by mu.
	dimsVerified bool
	Pid          int                   `json:"pid"` // agent process PID, immutable after spawn
	processScope *processScopeIdentity // immutable creation-bound ownership; persisted separately from the API DTO
	// Runtime provenance learned from FrameAgentInfo. It is deliberately not
	// persisted; every live wrapper identifies itself after daemon attachment.
	// agentProto is the reported agentProtoVersion (0 = pre-proto agent).
	agentVersion     string
	agentReportedPID int
	agentProto       int
	// Terminal submissions remain busy for restart safety until output advances
	// and a provider hand-back signal proves the turn settled.
	fleetTurnStateMu         sync.Mutex
	fleetTurnStateGeneration uint64
	fleetTurnAdmitted        atomic.Bool
	fleetTurnOutputWatermark atomic.Int64
	// test-only: makes HasCodexDescendant report true without a live codex
	// process; procfs-based detection is impossible for synthetic test panes
	// (Pid==0). Never set in production code.
	forceCodexForTest bool

	// harnessSessionDirs are the per-provider directories where a managed
	// terminal harness client records its per-pane durable session id. Derived
	// from the daemon data dir at spawn/attach so discoverHarnessSession can
	// run from the process-scan poll without a Manager reference.
	harnessSessionDirs []harnessSessionDir

	// LastActivityMs is updated on live PTY bytes, never historical replay. Unix millis. Atomic.
	LastActivityMs int64 `json:"lastActivityMs"`
	// Immutable after readLoop starts; tests can inject reception time per pane.
	frameReceivedAt func() time.Time

	// LastInputMs is updated on every user-input write to the agent (all four
	// input transports funnel through Pane.write). Unix millis. Atomic.
	// computeState compares it against LastActivityMs to tell keystroke ECHO
	// (output hard on the heels of input = the TUI redrawing its composer)
	// from real agent work — without this, typing in a live terminal reads
	// as "busy" and remote clients show a phantom Stop button.
	LastInputMs int64 `json:"-"`
	// A one-time Codex transport migration closes only this pane's input while
	// it drains existing writes and rechecks the idle/draft/approval guards.
	codexLiveMigrating atomic.Bool

	// TotalStrippedChars is a cumulative count of post-ANSI-strip chars
	// ever produced by PTY output. Updated from the readLoop alongside
	// each buffer.Write. Lives here (not on RingBuffer) because the
	// strip is a concern of state detection, not of the raw byte ring.
	// Units of CHARS (not bytes) so it matches the domain of the scan
	// string `s` in computeState — absolute positions computed against
	// this stay stable across scans regardless of how much ANSI noise
	// the app emits. A prior version mixed raw bytes with stripped
	// chars, which made match positions drift UP over time and caused
	// answered menus to re-fire the state.
	TotalStrippedChars int64 `json:"-"`

	// LastInputStrippedPos is TotalStrippedChars at the moment of the
	// most recent client → PTY write. Compared against each detected
	// menu's absolute stripped-char position to decide "has the user
	// acted on this specific menu". Not persisted.
	LastInputStrippedPos int64 `json:"-"`

	// LastAckedChoicePromptPos is the absolute stripped-char position
	// of the most recent `❯ 1.` match that computeState returned as
	// choice-prompt. Same units as TotalStrippedChars so comparisons
	// are drift-free. Not persisted.
	LastAckedChoicePromptPos int64 `json:"-"`

	LastAckedAwaitingInputPos int64 // absolute stripped pos when we last announced awaiting-input

	// LastGridChoicePromptHash is the fingerprint of the most recent
	// grid-reconstructed choice-prompt instance. The fingerprint carries
	// stable menu rows plus Codex MCP tool/argument identity; computeState
	// advances LastAckedChoicePromptPos when that identity changes or the
	// grid-feed lifecycle generation proves that an answered same-looking
	// prompt disappeared and then returned in a later committed TUI frame.
	LastGridChoicePromptHash uint64 `json:"-"`

	// LastScreenChoicePromptHash is the fingerprint of the most recent
	// replay-screen choice-prompt instance (see the screen fallback in
	// computeStateWithHasCodex). Kept separate from the grid hash: screen
	// text has no feed lifecycle generation of its own, so the two sources
	// admit and re-confirm independently while sharing
	// LastAckedChoicePromptPos as the single answered-position key.
	LastScreenChoicePromptHash uint64 `json:"-"`

	// GridChoicePromptGeneration is advanced by the grid FEED path whenever a
	// committed synchronized-output frame changes from no recognized choice
	// prompt to a prompt (or changes prompt identity). Unlike computeState's
	// 2s/5s pollers, that path observes every ratatui frame, so a short gap
	// between identical MCP modals cannot be missed. LastAckedGridChoicePromptGeneration
	// records the generation consumed by computeState. Both are in-memory-only;
	// LastAckedChoicePromptPos remains the single instance key exposed to
	// watcher, sweeper, and attention consumers.
	GridChoicePromptGeneration          uint64 `json:"-"`
	LastAckedGridChoicePromptGeneration uint64 `json:"-"`
	// Raw terminal output may paint a menu without a synchronized grid
	// frame. Coalesce its watcher wake per pane so streaming output cannot
	// turn the 64 KB state scan into a per-payload hot path. A hint received
	// during the pending timer gets one trailing wake, preserving rapid
	// back-to-back menus without a synchronized grid commit.
	autoPromptRawWakeMu    sync.Mutex
	autoPromptRawWakeTimer *time.Timer
	autoPromptRawWakeAgain bool
	autoPromptLastRawWake  time.Time
	// autoPromptExpectUntilMs (unix ms) opens a short window in which a
	// permission menu is expected: Claude's PreToolUse hook arrived, or an
	// auto-approval just landed and the next queued menu follows. Claude
	// does not bracket frames with DEC 2026 in daemon PTYs, so the grid
	// generation edge never fires for it, and its menus usually end more
	// than 512 raw bytes before the output tail. Inside the window every
	// output payload schedules the settled wake, unthrottled.
	autoPromptExpectUntilMs atomic.Int64

	// Grid-feed lifecycle parser state. Guarded by subsMu: readLoop, replay
	// seeding, resize, and teardown already serialize access to gridEmu with
	// that lock. The observed hash is the last COMMITTED grid frame, not the
	// most recently acknowledged prompt hash above.
	gridChoicePromptObservedHash uint64
	gridSyncEndMatch             int

	// SocketPath is where the agent listens. Persisted to disk so the daemon
	// can reattach after a restart. Immutable after spawn.
	SocketPath string `json:"socketPath"`

	// LastPrompts is the pane's recent submitted-input history, newest
	// first, capped at lastPromptsMax. Reconstructed by promptRec from
	// the bytes flowing through Write, so every transport (WS typing,
	// inject, ARIA, schedules) is captured. Guarded by mu; persisted
	// via PersistedPane so it survives daemon restarts; served to all
	// clients in PaneSnapshot.
	LastPrompts []string `json:"lastPrompts,omitempty"`

	// onPromptsCommit fires after LastPrompts changes (outside mu).
	// Wired by the Manager at spawn/attach to persist + publish a
	// "pane-prompt" UI event. Nil in tests.
	onPromptsCommit func(*Pane)

	// onClaudeSessionRotated fires (outside mu) ONLY on a genuine session-id
	// rotation — a prior non-empty id replaced by a different one, i.e. a
	// brand-new conversation in this pane (/clear, respawn, resume elsewhere).
	// It deliberately does NOT fire for the first id a pane learns or for a
	// re-observation of the id it already holds, so reattach and replay are
	// silent. Wired to the delegation nudger's session-start digest. Nil in
	// tests and on paths that don't wire it (a no-op then).
	onClaudeSessionRotated func(paneID, sessionID string)

	// onCodexTurnFlagCommit fires (outside mu) when a Codex Focus pane's DURABLE
	// turn flag transitions, so the Manager can persist it. Deliberately NOT
	// onPromptsCommit: that one also publishes a "pane-prompt" UI event, and a
	// turn edge changes no prompts — reusing it would spam every client (and the
	// relay) two redundant prompt events per Codex turn. Nil in tests.
	onCodexTurnFlagCommit func(*Pane)

	// onTurnStateEdge fires (outside mu) when a native pane's nativeInTurn flag
	// flips true↔false, so the Manager can publish a real-time pane-state
	// (AgentMidTurn) edge instead of the client waiting on the 2s activity ticker
	// / 3s poll. Wired alongside onPromptsCommit at spawn/attach. Nil in tests and
	// on paths that don't wire it (a no-op then).
	onTurnStateEdge func(*Pane)

	// onDimsCommit fires (outside mu) after Cols/Rows change — from a client
	// resize or an agent FrameDims adoption. Wired in main.go to a debounced
	// paneStore.Put so panes.json tracks the live PTY size; without it the
	// persisted dims fossilize at spawn time and every daemon restart rebuilds
	// panes with sizes the PTY hasn't had for weeks. Nil in tests.
	onDimsCommit         func(*Pane)
	onOwnerViewersCommit func(*Pane) error

	mu                             sync.Mutex
	agentConn                      net.Conn
	agentWriteMu                   sync.Mutex
	ownerRetirementGate            sync.Mutex
	ownerRetirementRestoreDisabled atomic.Bool
	ownerViewerLease               *ownerViewerLease
	ownerRetirementCommitted       bool
	ownerViewerKnowledgeKnown      bool                      // subsMu; false for legacy/unknown restored viewer history
	ownerViewerPendingDevices      map[string]bool           // subsMu; only positive ACK can clear
	ownerViewerRevision            uint64                    // subsMu
	ownerRetirementSupported       bool                      // mu
	ownerRetirementReplies         chan ownerRetirementReply // mu
	// promptRec parses client→PTY bytes inside Write; guarded by
	// agentWriteMu (Write is its only caller).
	promptRec promptRecorder

	// composerNonEmptyUntil is the native delegation-nudge composer seam. The
	// browser reasserts presence while a draft exists; expiry releases a draft
	// abandoned by a vanished client.
	composerNonEmptyUntil atomic.Int64

	// delegationPending is the daemon-computed buffered-update snapshot exposed
	// to pane DTOs for the compact header chip.
	delegationPending atomic.Pointer[delegationPendingInfo]
	// wheelAcc carries leftover wheel lines between writes for the ConPTY
	// wheel→page-key translation (input_wheel.go); guarded by agentWriteMu,
	// as is wheelFlushTimer (the gesture-end flush).
	wheelAcc        wheelAccum
	wheelFlushTimer *time.Timer
	// resizeMu serializes Pane.Resize end-to-end (dims commit through
	// FrameResize write) so the frame order seen by the agent always
	// matches the order dims were committed. See Resize for the race
	// this prevents.
	resizeMu sync.Mutex
	buffer   *RingBuffer

	// widthMarkMu guards lastWidthOff/lastWidthCols: the ring absolute offset
	// (a past buffer.TotalWritten value) and the value of the most recent PTY
	// width CHANGE. A reattach replay uses these to drop scrollback that was
	// hard-wrapped at a now-stale width — those hard newlines can never reflow,
	// so replayed verbatim they are the "narrow band above, correct screen
	// below" artifact. A leaf lock: taken briefly in Resize (already under
	// resizeMu) and in subscribeStream (after releasing subsMu); it never wraps
	// subsMu/resizeMu. lastWidthCols == 0 means "no change recorded yet".
	widthMarkMu   sync.Mutex
	lastWidthOff  int64
	lastWidthCols uint16

	// userClosing is set true by Pane.Kill() (under mu) BEFORE the kill is
	// signaled, so markExited can tell an explicit user kill (delete /
	// switch-mode / migrate / rollback — all want full removal) apart from
	// a natural process-exit (the agent's claude process ended on its own).
	// Only the latter, with a session id, becomes Resumable. Guarded by mu.
	userClosing bool
	// respawning is the SHARED single-flight guard for every in-place
	// kill→respawn of this pane — resume (POST /resume), refresh
	// (POST /refresh), the synchronous switch-mode "now" path, AND the
	// deferred-switch watcher's fire. They all reuse the same pane id and
	// agent socket, and Spawn's duplicate-id / socket-handoff guards are NOT
	// transactional, so two concurrent respawns would bind two agents to one
	// socket and orphan one. Every entry point CASes this false→true before it
	// kills the agent and Stores false when done; a loser 409s. Lock-free.
	// Acquire at the HTTP/watcher entry point, never inside respawnInPlace /
	// performSwitchMode (which the entry points call while holding it).
	respawning atomic.Bool
	// codexIdentityBlocked is a persisted safety latch for a terminal pane with
	// a foreground Codex process whose exact thread is not currently provable
	// (fresh, ambiguous, or transiently unreadable). The last known-good durable
	// thread stays persisted, but DTO/switch paths hide/refuse it until discovery
	// succeeds or that foreground process exits. This prevents both stale resume
	// and destructive loss of continuity on /proc exit/read races.
	codexIdentityBlocked atomic.Bool

	// agentRssMB is the last-sampled resident memory (MB) of this pane's selected
	// Claude or Codex descendant — the long-lived process "Restart agent"
	// reclaims. Written by the RSS sampler on its slow (~60s) timer and read on
	// the DTO path, so the header memory chip never pays a per-render /proc read
	// (the cost that GC-death-spiraled computeState). 0 = not yet sampled / no
	// matching agent child / non-Linux.
	agentRssMB atomic.Int64

	subsMu  sync.RWMutex
	subs    map[int64]*subEntry
	nextSub int64
	// subsDetached marks a pane object the manager has dropped (Manager.Kill).
	// Once set, subscribeStream refuses new registrations with an already-
	// closed stream: no new bytes can ever reach an orphaned object, so a
	// late subscriber (a client reconnect racing the kill) must be bounced
	// straight to a reconnect that resolves the SUCCESSOR pane under the
	// same id — not parked forever on this orphan. Guarded by subsMu.
	subsDetached bool
	// ownerSubID is the single sub currently allowed to drive PTY
	// size AND write keystrokes. 0 = unowned. When a client
	// disconnects while holding ownership the pane becomes unowned
	// and the next sub to connect auto-claims. Explicit claim via
	// WS {type:"claim"} switches ownership to the sender.
	ownerSubID int64

	// altScreen tracks whether the running app is in the DEC
	// private alternate screen buffer (mode 1049). Updated from
	// readLoop by scanning PTY output; read from Subscribe to
	// decide whether to prepend the enter sequence to the replay.
	//
	// Protected by subsMu (the WRITE lock). Why the same lock as
	// the subscriber list and not its own mutex: readLoop must
	// produce each payload atomically as the triple (bytes
	// appended to buffer, altScreen after this payload, fanout to
	// current subs). If Subscribe observed any of those three out
	// of step — e.g. snapshot taken after a payload's write but
	// before its trackModes, or vice versa — the reattaching
	// client would paint alt-screen cells in the wrong buffer.
	// An earlier split-lock design (dedicated altScreenMu) left
	// exactly that race open and Codex flagged it.
	//
	// Why we need this at all: the 256 KB ringbuf is raw bytes.
	// For a long-running TUI (codex / Ratatui), the initial
	// \x1b[?1049h gets rotated out of the window. A reattaching
	// client then receives cell-addressing bytes while xterm is
	// still in the normal buffer — content lands in scrollback,
	// alt-screen stays blank, user sees "empty spaces and black
	// chunks." Prepending the correct enter sequence restores
	// xterm's parser to the buffer the app is drawing into.
	altScreen bool

	// modeTail carries up to 32 bytes from the previous readLoop
	// payload so an escape sequence straddling a chunk boundary
	// (PTY reads are 32 KB; CSI ? ... h/l can span ~10 bytes) is
	// still recognised. Without it, a mode toggle split across
	// reads leaves altScreen stale until the app emits another
	// toggle — in a long-running Codex session that can be never.
	// Also protected by subsMu.
	modeTail []byte

	// modeState tracks the last seen set/reset for a small allowlist
	// of DEC private modes (bracketed paste, mouse reporting, cursor
	// keys, ...). TUIs emit these once at startup, so a tail-capped
	// replay (see replayTailCap*) can slice them out of the window —
	// reattaching clients would then lose bracketed paste / mouse
	// support until the app re-asserts them (often never). Subscribe
	// prepends a synthetic prefix rebuilt from this map whenever the
	// replay is truncated. Only modes present in the map were ever
	// observed; unseen modes are left to terminal defaults.
	// Protected by subsMu, same as altScreen/modeTail.
	modeState map[int]bool

	// vt is a virtual terminal emulator that consumes every PTY
	// byte alongside the ringbuf. The ringbuf is a byte log; vt is
	// a cell-grid model of what the terminal actually looks like.
	// Used to emit a synthetic full repaint on reattach and on
	// live resize — Codex (Ratatui) emits only cell-level diffs
	// against its own back-buffer, so the ringbuf's raw bytes
	// can't be trusted to reconstitute the display state. vt
	// gives us our own authoritative model to paint from.
	//
	// Writes to vt happen from readLoop under subsMu, same as
	// buffer.Write and trackModes. vt has its own internal mutex
	// (Lock/Unlock on the View interface) for reads.
	vt vt10x.Terminal

	// vtPaintTimer schedules the delayed synthetic-paint emit
	// after a live resize. Reset on each Resize so a rapid
	// sequence only emits once, ~200 ms after the last one.
	vtPaintTimer *time.Timer

	// redrawInProgress guards against concurrent Redraw sequences
	// stomping on each other. Each Redraw spawns a ~1-second bump
	// goroutine that temporarily changes p.Cols/p.Rows. If a
	// second Redraw starts while the first is in-flight it would
	// read the in-flight (shrunken) dims, restore to THOSE instead
	// of the client's intended size, and the pane would permanently
	// drift smaller on every rapid click. Flag prevents that.
	redrawInProgress atomic.Bool

	// nativePendingApprovals holds EVERY currently-open native permission
	// request for a native pane (Kind=="native"), keyed by RequestID. Claude can
	// emit several can_use_tool requests concurrently (parallel tool calls —
	// e.g. two WebSearch / multiple Read), so a single slot is wrong: the 2nd
	// would overwrite the 1st, leaving the agent hung forever waiting for an
	// approval the daemon can no longer deliver. Set by the native parser
	// goroutine on a control_request[can_use_tool]; entries cleared on
	// resolve / control_cancel_request / interrupt / turn_end / pane exit.
	// Guarded by nativeApprovalMu — computeState must NOT take this lock on its
	// hot path, so nativePendingCount mirrors len(map) for a lock-free check.
	nativeApprovalMu       sync.Mutex
	nativePendingApprovals map[string]*NativeApproval
	// nativeAllowlist is the per-pane "always allow this tool" set: a set of
	// EXACT tool names (e.g. "Read", "Bash") the user has opted into
	// auto-approving for THIS pane's conversation. When a can_use_tool arrives
	// for a tool in this set, the daemon resolves it as ALLOW immediately
	// (silent, like ARIA auto-approve) instead of parking a pending approval
	// card for the user. Guarded by nativeApprovalMu — the same lock that
	// guards the pending-approval map — because the allowlist check and the
	// park/resolve decision are one atomic step in the approval domain.
	// Persisted with the pane (PersistedPane.NativeAllowlist) so it survives
	// reconnect + daemon-restart-resume; cleared only when a genuinely NEW
	// session starts (see clearNativeAllowlist call in persistNativeSessionID).
	nativeAllowlist map[string]bool
	// nativePendingCount is the lock-free hot-path mirror of
	// len(nativePendingApprovals): read at the very top of computeState (one
	// atomic load) so a pending approval short-circuits to StateChoicePrompt
	// without the 64KB byte scan. For terminal panes it stays 0 forever — a
	// single cache-hot int read, preserving the zero-cost fallback guarantee.
	nativePendingCount atomic.Int32

	// attention is the last-computed "who needs to act" signal (see
	// attention.go). Written only by the attentionEngine; read lock-free by
	// DTO builds and the pusher. Nil until the first evaluation — consumers
	// treat that as "unknown, assume the human is needed". Never persisted.
	attention atomic.Pointer[paneAttentionRecord]
	// Refresh can run concurrently from the ticker and turn-edge hooks.
	// Observation numbers are assigned before classification; flagMu guards
	// the last committed number so an older observation cannot publish later.
	attentionObservationSeq atomic.Uint64
	attentionCommittedSeq   uint64
	// flagMu serializes flag checks, writes and persistence across manual,
	// scoped and Warden cleanup paths. p.mu still protects snapshots.
	flagMu sync.Mutex

	// attentionSummary is the LLM-written one-liner for the CURRENT
	// needs-user prompt (see aria_attention_summary.go). Written by the
	// summarizer goroutine, cleared when the pane leaves needs-user; read
	// lock-free by DTO builds and the pusher. Nil = no summary (heuristic
	// preview stands). Never persisted.
	attentionSummary atomic.Pointer[attentionSummaryRecord]

	// autoDecline records the most recent auto-approval decline for the
	// pane's CURRENT menu (scoped by LastAckedChoicePromptPos so a new menu
	// invalidates it). Set from Aria's eval-outcome hook; read by the
	// attentionEngine to flip needs-user with the decline's reason attached.
	autoDeclineMu     sync.Mutex
	autoDeclineReason string
	autoDeclinePos    int64

	// Kind selects the pane's rendering model. The default ("") is the
	// terminal pane: raw PTY bytes fanned out to clients, byte-identical
	// to the pre-native behavior (the fallback guarantee). "native" runs
	// a headless Claude agent emitting stream-json that the daemon
	// normalizes to the NativeEvent schema; clients render the model at
	// their own width. Set at spawn and immutable after.
	// Persisted so a native pane reattaches as native across restart.
	Kind string `json:"kind,omitempty"`

	// Agent identifies the agent protocol independently from Kind. Claude is
	// the legacy zero value (""), while "codex" and "muse" are persisted explicitly.
	Agent string `json:"agent,omitempty"`
	// CodexThreadID is the app-server thread id for native Codex panes.
	CodexThreadID string `json:"codexThreadId,omitempty"`
	// MuseSessionID is the durable Muse session UUID (`muse exec
	// --session-id` / `muse resume <id>`). The daemon mints it for native
	// Muse panes; terminal panes get it only from a `muse resume <id>` argv.
	MuseSessionID string `json:"museSessionId,omitempty"`

	// HarnessSessionID is the durable session id of a managed terminal harness
	// (the DeepSeek client's DSH session). Unlike Claude/Codex/Muse this id is
	// not derivable from argv or a provider rollout: the client writes it to a
	// per-pane sidecar (SOLU_PANE_ID-keyed, see deepseek-terminal.mjs
	// recordPaneSession) and the daemon adopts it. It is what lets a harness
	// pane resume its conversation when its pane agent is replaced.
	HarnessSessionID string `json:"harnessSessionId,omitempty"`
	// HarnessProvider names the managed harness that owns HarnessSessionID
	// ("deepseek"; future managed clients add their own slug). It is what
	// stops a stale ClaudeSessionID on a reused shell pane from resuming the
	// wrong provider over a live harness conversation.
	HarnessProvider string `json:"harnessProvider,omitempty"`

	// Provider routes this pane's `claude` process to an LLM backend: "" (or
	// "anthropic") = Anthropic, the default and byte-identical to today;
	// "glm" = Z.ai's Anthropic-compatible endpoint. It carries the NAME, not
	// any key — the agent resolves the GLM token at spawn (see glm_settings.go)
	// and the env is re-derived on EVERY (re)spawn, so it must round-trip to
	// disk to survive a daemon restart / auto-update. Set at spawn, immutable.
	Provider string `json:"provider,omitempty"`

	// Effort is the reasoning-effort level baked into a native pane's
	// `claude --effort` argv ("" | "low" | "medium" | "high" | "xhigh" |
	// "max"). Provider-agnostic. Unlike Provider it is MUTABLE at runtime — the
	// effort-change endpoint respawns the pane with claude --resume so the new
	// level takes effect mid-conversation. Round-trips to disk so it survives a
	// restart / switch / resume. "" means claude's built-in default (no flag).
	Effort string `json:"effort,omitempty"`

	// Model is the Claude model id baked into a native pane's `claude --model`
	// argv ("" = no flag). Mutable like Effort (the model-change endpoint
	// respawns with --resume). NOTE: because `--resume` makes the session's
	// stored model sticky, "" does NOT mean "settings default" on an existing
	// session — it means "whatever the session last ran".
	Model string `json:"model,omitempty"`

	// Role is the pane's product role: "" (plain) or "orchestrator" — the
	// chief-of-staff Claude or Codex pane whose every (re)spawn receives the
	// shared policy through its host adapter (see pane_role_host.go). Round-trips
	// to disk so restart/resume/renderer switches never silently demote it.
	Role string `json:"role,omitempty"`
	// ContextPack is the local pack directory path and ContextPackStatus records
	// how the spawn adapter handled it. No briefing content ever rides the DTO.
	ContextPack       string `json:"contextPack,omitempty"`
	ContextPackStatus string `json:"contextPackStatus,omitempty"`

	// AnnounceDelegationEvents governs whether soluterminal types a one-line
	// "[solu] delegated work …" status into this pane's supported host session
	// when work the pane delegated needs a decision, fails, or lands
	// (delegation_nudge.go).
	//
	// Tri-state on purpose: nil means "follow the role default" (on for an
	// orchestrator — the whole point of a chief-of-staff pane is that it hears
	// back — off for every other pane), and a non-nil value is the user's
	// explicit override, which must survive a role change rather than being
	// silently re-derived. The pointer is REPLACED, never mutated in place, so
	// a snapshot copy of it is race-free.
	AnnounceDelegationEvents *bool `json:"announceDelegationEvents,omitempty"`

	// AutoMode is ARIA's per-pane auto-approval policy. One of:
	//   ""        — off; pane needs manual approval (default).
	//   "always"  — ARIA approves every awaiting-input prompt without
	//               a cancel countdown.
	//   "cautious" — ARIA only approves prompts that look low-risk
	//               (read-only, formatting, status); anything that
	//               could mutate state outside the repo, hit the
	//               network in a new way, delete, or touch secrets is
	//               left for the user. Keeps the 5s cancel window.
	// Per-hour cap (ariaAutoMaxPerHour) and global pause still apply
	// to both modes — AutoMode only governs how the evaluator decides,
	// not whether the safety gates run.
	AutoMode string `json:"autoMode,omitempty"`
	// Every policy transition revokes in-flight approval decisions, including
	// switching off and back to the same mode. Guarded by mu; not persisted.
	autoModeGeneration uint64
	// Serializes mode transitions with guarded transport writes without
	// holding mu (and blocking UI snapshots) throughout network I/O.
	autoModeMu sync.Mutex

	// AutoModeLastFiredMs is when the last AutoMode auto-fire actually
	// landed (approved + no send error). Drives the "fired Xs ago"
	// label on the auto-mode strip in the ARIA panel. In-memory only
	// — survives within a session but not across daemon restart, since
	// the staleness window the user cares about is "recent activity",
	// not historical audit.
	AutoModeLastFiredMs int64 `json:"autoModeLastFiredMs,omitempty"`

	// Narrating records whether this native pane's agent process was spawned with
	// the narration system prompt (--append-system-prompt). It's a property of the
	// running process — set at spawn from the global native setting, and a live
	// agent keeps it for its lifetime — so the UI can show a per-pane "narrating"
	// badge reflecting the TRUTH of the process, not the (possibly newer) global
	// toggle. Persisted so a survived-agent reattach across restart keeps the
	// correct badge; a respawn recomputes it from the current setting.
	Narrating bool `json:"narrating,omitempty"`

	// Flagged is the user's "come back to this later" marker. When true
	// the pane gets an orange highlight on screen and counts toward the
	// flagged-pane badge in the sidebar so the user can spot pending
	// threads without re-entering each workspace. FlagMemo is an
	// optional one-liner ("stuck on auth race") that surfaces on hover.
	// Persisted so a flag survives daemon restart — the whole point is
	// that you don't lose track between sessions.
	Flagged  bool   `json:"flagged,omitempty"`
	FlagMemo string `json:"flagMemo,omitempty"`
	// FlagOwner records WHO set the current flag: "user" (UI/HTTP paths),
	// "agent" (a full-access MCP caller, i.e. an interactive pane session),
	// or a scoped MCP identity like "warden". Lets provenance guards stop a
	// scoped caller from overwriting or clearing someone else's flag.
	// Cleared together with the flag.
	FlagOwner string `json:"flagOwner,omitempty"`
	// FlagEpisode and FlagRevision mark only new episode-bound Warden flags.
	// Historical flags have neither and are never swept by Warden cleanup.
	FlagEpisode  string `json:"-"`
	FlagRevision string `json:"-"`

	// ClaudeSessionID is the Claude Code conversation id observed from
	// Claude's hook payloads. Persisted so a host reboot can recreate this
	// pane with `claude --resume <id>` instead of guessing by cwd.
	ClaudeSessionID string `json:"claudeSessionId,omitempty"`

	// ClaudeTranscriptPath is the absolute path to the MAIN conversation
	// transcript Claude Code is writing for this pane, captured from the hook
	// payload's transcript_path on MAIN-session events only. Authoritative for
	// the conversation endpoint: it follows session-id rotation (/compact,
	// --resume) and is never a subagent/workflow "orphan" id (those have no
	// top-level <sid>.jsonl, only a <sid>/subagents dir). See SetClaude
	// TranscriptPath and transcriptForPane.
	ClaudeTranscriptPath string `json:"claudeTranscriptPath,omitempty"`

	// TitleOverride is a per-pane USER title that WINS over Claude Code's
	// auto-generated ai-title in the Activity-tab header. Set either by the
	// user typing it or by tapping "generate" (a fresh Haiku-authored title);
	// both write this same field. Empty falls back to ai-title. Persisted so it
	// survives a daemon restart / resume, mirroring ClaudeTranscriptPath. See
	// SetTitleOverride and the /title + /title/generate endpoints.
	TitleOverride string `json:"titleOverride,omitempty"`
	// Origin records who or what started this pane and why (pane_origin.go).
	// Persisted; nil for panes that predate it.
	Origin *PaneOrigin `json:"origin,omitempty"`
	// Review is the latest solu-review result for this pane's work (pane_review.go).
	Review *PaneReview `json:"review,omitempty"`
	// Identity is the pane's handle and working-on line (pane_identity.go).
	// Persisted; nil when neither is set.
	Identity *PaneIdentity `json:"identity,omitempty"`
	// lastAgentMessage: the last message another agent delivered here
	// (pane_message_receipt.go). Guarded by mu; not persisted.
	lastAgentMessage *agentMessageReceipt

	// gridEmu is the server-authored VT emulator. Lazily created when a
	// pane needs server-side cell state: ARIA auto-mode prompt/choice
	// detection (GridSnapshot → gridVisibleText) and the debug-grid
	// endpoint. Fed from the fanout loop alongside buffer.Write, so it
	// sees the exact same PTY byte stream the bytes-mode clients render.
	// Never torn down while the pane lives — it retains state across
	// client disconnects. Distinct from replayEmu (reconnect repaint) and
	// agentGrid (native renderer), which each own their own emulator.
	gridEmu *grid.Emulator

	// gridReplyStop stops the OSC-reply pump goroutine when the pane exits.
	gridReplyStop chan struct{}

	// --- native-renderer state (Kind=="native" only) ---
	//
	// All of these are guarded by nativeMu — a DEDICATED mutex, NOT subsMu.
	// The ONLY point native state touches the subsMu hot path is the single
	// non-blocking tee send (pane.go readLoop): the parser goroutine itself
	// runs entirely off-lock, draining nativeTeeCh. Terminal panes (Kind=="")
	// never allocate any of these — they stay nil, so the hot-path tee guard
	// (`p.nativeTeeCh != nil`) is one cache-hot pointer compare, preserving
	// the byte-identical fallback guarantee.
	nativeMu sync.Mutex

	// nativeAdapter normalizes this pane's raw agent JSONL into NativeEvents
	// (Claude stream-json today). Set once by ensureNative; pure transform.
	nativeAdapter NativeAdapter

	// nativeTeeCh carries raw PTY payload copies (tagged with whether they came
	// from an agent stdout-ring backlog replay — see nativeChunk) from the
	// readLoop hot path to the parser goroutine. Buffered + drop-on-full (the
	// readLoop NEVER blocks on it). nil for terminal panes. Closed by
	// shutdownNative.
	nativeTeeCh chan nativeChunk

	// nativeStop signals the parser goroutine to exit. Closed by shutdownNative.
	nativeStop chan struct{}

	// nativeSubs are native-event subscribers (the SSE /api/panes/{id}/native
	// fan-out). Each gets every NativeEvent after it joins; the bounded ring
	// backfills history on connect. Buffered + drop-on-full. Guarded by nativeMu.
	nativeSubs    map[int64]chan NativeEvent
	nextNativeSub int64

	// nativeRing is the bounded per-pane event history (drop-oldest by count
	// AND bytes) so late SSE joiners and ?since= gap-fill have recent events
	// without re-parsing the transcript. Guarded by nativeMu.
	nativeRing      []NativeEvent
	nativeRingBytes int

	// nativeSeq is the monotonic per-pane sequence counter the parser goroutine
	// stamps onto every egress NativeEvent. Guarded by nativeMu.
	nativeSeq uint64

	// nativeEpoch is this pane's unique "agent spawn" id, stamped once by
	// ensureNative (see nextNativeEpoch). Surfaced as PaneSnapshot.NativeEpoch so
	// the client detects a respawn (mode switch / daemon swap) and re-seeds.
	nativeEpoch atomic.Uint64

	// nativeInTurn is the explicit "the agent is mid-turn" flag for native panes
	// — set true when a user turn is written (paneNativeInput) OR when an
	// assistant message starts streaming (first text/thinking/tool_call of a
	// turn), and cleared on turn_end / result / error. This is MORE precise than
	// recency for native panes: a native pane emits no PTY bytes once the agent
	// is headless, so LastActivityMs would read "idle" the instant streaming
	// pauses even though the turn is still in flight. IsAgentWorking() reads it
	// lock-free (one atomic load) for the graceful-switch busy guard. For
	// terminal panes it stays false forever (the parser never runs), so the
	// terminal arm of IsAgentWorking falls back to computeState recency.
	nativeInTurn atomic.Bool

	// codexTurnInFlight is the DURABLE twin of nativeInTurn for Codex Focus
	// panes: it mirrors the same turn edges but is persisted (panes.json
	// codexTurnInFlight) and, unlike nativeInTurn, is NEVER read by
	// computeState — a suspended pane restores it to know its last turn died
	// mid-flight without thereby rendering as "working".
	//
	// It exists because Codex CANNOT distinguish a crash-orphaned turn from a
	// deliberate user interrupt: measured against codex-cli 0.144.4, a turn
	// killed mid-flight and a turn the user ESC'd BOTH resume as
	// status:"interrupted" with completedAt/durationMs populated and
	// error:null, and both rollouts end with the same
	// `turn_aborted{reason:"interrupted"}` record (Codex even injects the same
	// "The user interrupted" text into the model's context). So the resumed
	// history cannot tell the user WHY their response stopped. This flag is the
	// daemon's own record: set when a turn starts, cleared when Codex reports
	// turn/completed (including a user interrupt). Surviving as true across a
	// respawn therefore means exactly one thing — the process died mid-turn
	// without ever reporting an end — which Spawn turns into the
	// "resume-repair" notice (Claude reads its transcript instead — prepareClaudeResume).
	codexTurnInFlight atomic.Bool

	// nativeGotInit flips true when this pane's live stream produces its FIRST
	// system/init (EvSessionInit) — the earliest positive proof that a headless
	// `claude --resume` came up healthy rather than dying on a bad session id.
	// Per-Pane (a respawn is a fresh struct), so it needs no reset. Read by
	// confirmSwitchHealthy to end the post-respawn health grace early instead of
	// always burning the full window on every mode switch / resume / refresh.
	nativeGotInit atomic.Bool

	// nativeEchoSuppress holds the texts of FRESH-turn prompts the daemon emitted
	// as user_text at INPUT time (paneNativeInput) so they precede the reply in
	// seq order — Claude's `--replay-user-messages` echo is NOT ordered before the
	// reply on stdout, so a late echo would render the prompt below the answer.
	// Each entry is consumed (dropped) when its matching live echo arrives, so we
	// don't double-render it. Content-FIFO (mirrors the client's reconcilePending);
	// mid-turn STEER inputs are NOT registered here (their echo flows through and
	// the client anchors it to the next turn). Guarded by nativeEchoMu; bounded.
	nativeEchoMu       sync.Mutex
	nativeEchoSuppress []string

	// nativeLastEventNano is the unix-nano timestamp of the last LIVE native event
	// (and of a freshly-submitted user turn). nativeOpenTools counts tool_calls
	// that haven't yet seen their tool_result (a tool the agent is waiting on).
	// Together they let agentMidTurnForState SETTLE a stuck nativeInTurn: the
	// deny-with-reason AskUserQuestion answer path emits no turn_end, so the flag
	// can hang true after the agent has gone idle. When no tool is in flight AND no
	// event has streamed for longer than any plausible tool_result→next-message
	// gap, the turn has settled. The open-tool guard is what keeps a long, silent
	// tool run (event-less but genuinely working) from being mistaken for idle.
	nativeLastEventNano atomic.Int64
	nativeOpenTools     atomic.Int32
	// nativeLastWasToolResult is true when the most recent live event was a
	// tool_result — i.e. the model is in the event-less gap before it resumes its
	// next message. That gap gets a longer settle allowance so the FAST normal
	// settle (after a finished response) doesn't flicker the spinner mid-gap.
	nativeLastWasToolResult atomic.Bool

	// nativeWorkflowActive is set when this pane launched a Claude Code Workflow
	// (a `Workflow` tool_call streamed by). A Workflow runs its subagents in the
	// BACKGROUND: the tool returns a task id immediately and the launching turn
	// then settles to idle (nativeInTurn → false) while the subagents keep
	// churning — so the pane would look done even though work is ongoing. While
	// this flag is set the DTO carries a WorkflowStatus (scanned from the session's
	// subagents/workflows dir) so the footer shows "workflow · N/M agents · active
	// Xs ago". Cleared on the NEXT turn start (false→true edge) — the workflow's
	// completion injects exactly such a turn, and a user steer is an explicit move
	// on. Not persisted: a daemon restart drops it (the agent process is gone too).
	nativeWorkflowActive atomic.Bool

	// pendingSwitchNotice records that the most recent Terminal↔Native switch
	// INTERRUPTED an in-flight turn, so the client can show a "previous response
	// interrupted — Continue" marker on the pane. Set under switchNoticeMu when a
	// graceful switch interrupts a working agent; cleared when the next user turn
	// is written (paneNativeInput / the terminal input path) or on an explicit
	// ack. nil when there's no outstanding notice. Rides the pane DTO as
	// switchNotice (see makePaneDTO).
	switchNoticeMu      sync.Mutex
	pendingSwitchNotice *switchNotice

	// pendingSwitch holds an ARMED but not-yet-fired deferred mode switch
	// (when:"deferred"): the user asked to flip Terminal↔Native but wants it
	// to wait for the agent's current turn to finish rather than force-
	// interrupting it. A standalone watcher goroutine (deferredSwitchWatcher)
	// polls turnSettledForSwitch and runs performSwitchMode once the turn has
	// settled. pendingSwitchMu is a LEAF lock — never held while taking
	// nativeMu/subsMu/p.mu — so it can be read from the DTO path and the
	// watcher without deadlock risk. pendingSwitchWatching gates a single
	// watcher goroutine per pane (CAS false→true on arm). nil pendingSwitch =
	// nothing armed; the watcher exits when it observes nil.
	pendingSwitchMu       sync.Mutex
	pendingSwitch         *pendingDeferredSwitch
	pendingSwitchWatching atomic.Bool

	// replayEmu tracks the current visible terminal grid for bytes-mode
	// reattach fallback. The raw ring buffer remains the primary replay
	// because it preserves scrollback; replayEmu is only used when that
	// byte log has become visually blank after a long-running TUI churns
	// the ring with cursor/spinner updates.
	replayEmu *grid.Emulator

	// resizeSettleTimer debounces the auto-Redraw() kick that cleans up
	// ratatui/codex ghost cells left over from diff-only repaints after
	// a client resize. Reset on every incoming client resize; fires the
	// Redraw bump-sequence once the user stops moving. Guarded by
	// resizeSettleMu — NOT subsMu, because Redraw itself re-enters
	// Pane.Resize (internal bumps), and tangling timers with subsMu
	// would deadlock against the fanout lock Redraw takes for its
	// scroll-up injection.
	resizeSettleMu    sync.Mutex
	resizeSettleTimer *time.Timer

	// Resize coalescing. A mobile client's multi-pass fit (font-load races,
	// keyboard open/close, bottom-bar mount/unmount) fires several resizes
	// with different row counts within a few hundred ms; applying each one
	// is a SIGWINCH that a normal-buffer Codex pane can't cleanly repaint
	// (the cols-1 reattach bump is deliberately skipped for it), so the
	// storm churns its renderer. recomputeSize routes through here to
	// collapse a burst into ONE Resize after the geometry settles. Guarded
	// by its own mutex — Resize takes resizeMu/subsMu, so this must not.
	resizeCoalesceMu    sync.Mutex
	resizeCoalesceTimer *time.Timer
	pendingResizeCols   uint16
	pendingResizeRows   uint16
	pendingResizeGrid   bool

	// mobileSize* remembers the most recent phone (mobile sub) geometry so a
	// brief mobile RECONNECT GAP can't hand the shared PTY back to a wider
	// desktop sub mid-session. The native app's WS connection churns (backoff
	// reconnects, app lifecycle); during a gap the only remaining sub is the
	// always-mounted desktop (warm pane, visible=true, wide), so desiredSizeLocked
	// would flip the PTY desktop-wide — clipping Codex on the phone and
	// triggering a repaint that snaps the phone's scroll to the bottom. While a
	// mobile width was applied within mobileSizeHoldWindow and NO mobile sub is
	// currently visible, we hold the phone width instead, killing the 84⟷62
	// oscillation. lastSizeLog dedupes the diagnostic so we log only transitions.
	// Guarded by mobileSizeMu — a leaf lock taken inside desiredSizeLocked (under
	// subsMu) and recomputeSize; it never wraps subsMu/resizeMu.
	mobileSizeMu   sync.Mutex
	lastMobileCols uint16
	lastMobileRows uint16
	lastMobileAt   time.Time
	lastSizeHeld   bool   // last desiredSizeLocked used the reconnect-gap hold
	lastSizeLog    string // dedupes the mobile-size diagnostic to transitions

	// stateCacheData memoizes computeState's result for the periodic
	// background watchers (pusher/activity/long-running) so they share one
	// computation per (output-position, native-pending, TTL) window instead
	// of each redoing the 64KB SnapshotTail + stripANSI + regex + procfs walk
	// every 2s. Strict perf optimization, zero behavior change; the /api/panes
	// request path still calls the DIRECT uncached compute. See state_cache.go.
	// Self-guarded by its own mutex.
	stateCacheData stateCache
	stateTailCache stateTailCache

	// aiTitleCache holds the last-known Claude-generated conversation title
	// (the type:"ai-title" line in the transcript — the same value the Activity
	// header shows). Refreshed strictly OFF the request path (the conversation
	// endpoint when a pane is opened, and the activity watcher on turn
	// completion) so the bulk /api/panes DTO can surface it without paying a
	// per-pane transcript scan on the list hot path. Lock-free atomic pointer:
	// one Load when building each pane DTO. Never persisted — re-warmed from the
	// transcript after a daemon restart. Empty until first warmed; the client's
	// list subtitle falls back to titleOverride / last-prompt meanwhile.
	aiTitleCache atomic.Pointer[string]

	done chan struct{}
}

type delegationPendingInfo struct {
	Count    int
	NeedsYou int
	OldestAt time.Time
}

// composerPendingLen is the terminal delegation-nudge composer seam. The
// prompt recorder already tracks edits and clears on submit/cancel controls.
func (p *Pane) composerPendingLen() int {
	pending, _ := p.composerPendingSnapshot()
	return pending
}

// composerPendingSnapshot returns the recorder's current draft length and the
// Unix-millisecond time of its most recent recorded input as one locked view.
func (p *Pane) composerPendingSnapshot() (int, int64) {
	if p == nil {
		return 0, 0
	}
	p.agentWriteMu.Lock()
	defer p.agentWriteMu.Unlock()
	return p.promptRec.draft()
}

// SetComposerNonEmpty records the native composer's current presence with a
// TTL so an unmounted or disconnected client cannot hold delivery forever.
func (p *Pane) SetComposerNonEmpty(nonEmpty bool, now time.Time) {
	if p == nil {
		return
	}
	if !nonEmpty {
		p.composerNonEmptyUntil.Store(0)
		return
	}
	p.composerNonEmptyUntil.Store(now.Add(delegationComposerPresenceTTL).UnixMilli())
}

func (p *Pane) composerNonEmptyNow(now time.Time) bool {
	return p != nil && p.composerNonEmptyUntil.Load() > now.UnixMilli()
}

func (p *Pane) setDelegationPending(count, needsYou int, oldest time.Time) {
	if p == nil {
		return
	}
	if count == 0 {
		p.delegationPending.Store(nil)
		return
	}
	p.delegationPending.Store(&delegationPendingInfo{Count: count, NeedsYou: needsYou, OldestAt: oldest})
}

func (p *Pane) delegationPendingSnapshot() *delegationPendingInfo {
	if p == nil {
		return nil
	}
	v := p.delegationPending.Load()
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}

// AITitleCache returns the last-warmed Claude-generated ai-title for the pane,
// or "" if it has not been read from the transcript yet. Lock-free.
func (p *Pane) AITitleCache() string {
	if s := p.aiTitleCache.Load(); s != nil {
		return *s
	}
	return ""
}

// setAITitleCache updates the cached ai-title. It ignores an empty title (so a
// transcript whose ai-title line has scrolled past the tail window never
// CLEARS a good cached value) and is a no-op when unchanged.
func (p *Pane) setAITitleCache(title string) {
	if title == "" {
		return
	}
	if cur := p.aiTitleCache.Load(); cur != nil && *cur == title {
		return
	}
	p.aiTitleCache.Store(&title)
}

// PaneSnapshot is a lock-free copy of a Pane's serializable fields, safe
// for concurrent reads (JSON serialization, persistence).
type PaneSnapshot struct {
	UnsentMessage *unsentPaneMessage  `json:"unsentMessage,omitempty"`
	Credential    *paneCredentialInfo `json:"credential,omitempty"`

	ID              string     `json:"id"`
	Cwd             string     `json:"cwd"`
	Cmd             []string   `json:"cmd"`
	Title           string     `json:"title"`
	Status          PaneStatus `json:"status"`
	CreatedAt       time.Time  `json:"createdAt"`
	ExitCode        *int       `json:"exitCode,omitempty"`
	Resumable       bool       `json:"resumable,omitempty"`
	RestartPaused   bool       `json:"restartPaused,omitempty"`
	RecoveryError   string     `json:"recoveryError,omitempty"`
	IdentityBlocked bool       `json:"-"`
	Cols            uint16     `json:"cols"`
	Rows            uint16     `json:"rows"`
	// DimsUnverified is true while Cols/Rows are the persisted copy from a
	// pre-restart daemon rather than the live PTY size (old agent that
	// doesn't report FrameDims, and no client resize applied yet). Clients
	// must not seed attach resizes from unverified dims — doing so resized
	// real PTYs to weeks-stale widths after every auto-update and garbled
	// every pane. Cleared by FrameDims adoption or the first applied resize.
	DimsUnverified bool                  `json:"dimsUnverified,omitempty"`
	Pid            int                   `json:"pid"`
	ProcessScope   *processScopeIdentity `json:"-"`
	LastActivityMs int64                 `json:"lastActivityMs"`
	SocketPath     string                `json:"socketPath"`
	Kind           string                `json:"kind,omitempty"`
	Agent          string                `json:"agent,omitempty"`
	CodexThreadID  string                `json:"codexThreadId,omitempty"`
	MuseSessionID  string                `json:"museSessionId,omitempty"`
	// HarnessSessionID/HarnessProvider mirror Pane's durable managed-harness
	// conversation pointer (see the Pane fields). They ride the snapshot so the
	// resume path, persistence, and the DTO all observe a coherent copy.
	HarnessSessionID string `json:"harnessSessionId,omitempty"`
	HarnessProvider  string `json:"harnessProvider,omitempty"`
	// CodexTurnInFlight mirrors Pane.codexTurnInFlight (see its doc): a Codex
	// Focus turn was underway and had not reported an end. Carried so the
	// resume path can tell a crash-orphaned turn from a user interrupt —
	// Codex's own resumed history reports both identically.
	CodexTurnInFlight bool   `json:"codexTurnInFlight,omitempty"`
	Provider          string `json:"provider,omitempty"`
	Effort            string `json:"effort,omitempty"`
	Model             string `json:"model,omitempty"`
	Role              string `json:"role,omitempty"`
	ContextPack       string `json:"contextPack,omitempty"`
	ContextPackStatus string `json:"contextPackStatus,omitempty"`
	// AnnounceDelegationEvents is the user's EXPLICIT override (see Pane); nil
	// means "follow the role default". The daemon-computed effective value
	// rides the DTO separately as announceDelegation.
	AnnounceDelegationEvents *bool                `json:"announceDelegationEvents,omitempty"`
	AutoMode                 string               `json:"autoMode,omitempty"`
	AutoModeLastFiredMs      int64                `json:"autoModeLastFiredMs,omitempty"`
	Narrating                bool                 `json:"narrating,omitempty"`
	Flagged                  bool                 `json:"flagged,omitempty"`
	FlagMemo                 string               `json:"flagMemo,omitempty"`
	FlagOwner                string               `json:"flagOwner,omitempty"`
	FlagEpisode              string               `json:"-"`
	FlagRevision             string               `json:"-"`
	ClaudeSessionID          string               `json:"claudeSessionId,omitempty"`
	ClaudeTranscriptPath     string               `json:"claudeTranscriptPath,omitempty"`
	TitleOverride            string               `json:"titleOverride,omitempty"`
	Origin                   *PaneOrigin          `json:"origin,omitempty"`
	Review                   *PaneReview          `json:"review,omitempty"`
	Identity                 *PaneIdentity        `json:"identity,omitempty"`
	LastAgentMessage         *agentMessageReceipt `json:"lastAgentMessage,omitempty"`
	// NativeAllowlist is the pane's "always allow this tool" set (sorted tool
	// names) for native panes. Rides the snapshot so it persists with the pane
	// and so any DTO/persist path observes a coherent copy.
	NativeAllowlist []string `json:"nativeAllowlist,omitempty"`
	// LastPrompts is the pane's recent submitted-input history, newest
	// first (see Pane.LastPrompts). Rides the DTO so every client —
	// desktop, phone over the relay — renders the same strip.
	LastPrompts []string `json:"lastPrompts,omitempty"`
}

// Snapshot returns a coherent copy of the pane's serializable fields
// under p.mu. Callers should use this — not the *Pane directly — when
// marshaling or persisting, to avoid racing with mutators.
func (p *Pane) Snapshot() PaneSnapshot {
	// Read the allowlist under its OWN lock (nativeApprovalMu) BEFORE taking
	// p.mu — it's guarded by a different mutex than the rest of the fields, and
	// taking it first keeps a single, fixed lock order (nativeApprovalMu →
	// p.mu) so Snapshot can't deadlock against any path that holds p.mu and
	// then touches the allowlist (none does today, but the order is now pinned).
	allowlist := p.allowlistTools()
	p.mu.Lock()
	defer p.mu.Unlock()
	return PaneSnapshot{
		UnsentMessage:            p.currentUnsentMessageLocked(),
		Credential:               clonePaneCredential(p.Credential),
		ID:                       p.ID,
		Cwd:                      p.Cwd,
		Cmd:                      p.Cmd,
		Title:                    p.Title,
		Status:                   p.Status,
		CreatedAt:                p.CreatedAt,
		ExitCode:                 p.ExitCode,
		Resumable:                p.Resumable,
		RestartPaused:            p.RestartPaused,
		RecoveryError:            p.RecoveryError,
		IdentityBlocked:          p.codexIdentityBlocked.Load(),
		Cols:                     p.Cols,
		Rows:                     p.Rows,
		DimsUnverified:           !p.dimsVerified,
		Pid:                      p.Pid,
		ProcessScope:             cloneValuePtr(p.processScope),
		LastActivityMs:           atomic.LoadInt64(&p.LastActivityMs),
		SocketPath:               p.SocketPath,
		Kind:                     p.Kind,
		Agent:                    p.Agent,
		CodexThreadID:            p.CodexThreadID,
		MuseSessionID:            p.MuseSessionID,
		HarnessSessionID:         p.HarnessSessionID,
		HarnessProvider:          p.HarnessProvider,
		CodexTurnInFlight:        p.codexTurnInFlight.Load(),
		Provider:                 p.Provider,
		Effort:                   p.Effort,
		Model:                    p.Model,
		Role:                     p.Role,
		ContextPack:              p.ContextPack,
		ContextPackStatus:        p.ContextPackStatus,
		AnnounceDelegationEvents: p.AnnounceDelegationEvents,
		AutoMode:                 p.AutoMode,
		AutoModeLastFiredMs:      p.AutoModeLastFiredMs,
		Narrating:                p.Narrating,
		Flagged:                  p.Flagged,
		FlagMemo:                 p.FlagMemo,
		FlagOwner:                p.FlagOwner,
		FlagEpisode:              p.FlagEpisode,
		FlagRevision:             p.FlagRevision,
		ClaudeSessionID:          p.ClaudeSessionID,
		ClaudeTranscriptPath:     p.ClaudeTranscriptPath,
		TitleOverride:            p.TitleOverride,
		Origin:                   p.Origin.clone(),
		Review:                   p.Review.clone(),
		Identity:                 p.Identity.clone(),
		LastAgentMessage:         p.lastAgentMessage.clone(),
		NativeAllowlist:          allowlist,
		// sanitizePrompts so menu-choice noise ("11"/"111") that was
		// recorded and persisted before the observe()-time filter clears
		// from every client's strip on the next read, not after three
		// real prompts age it out. Snapshot is the single marshal/persist
		// path, so cleaning here also rewrites it out of panes.json.
		LastPrompts: sanitizePrompts(cloneStringSlice(p.LastPrompts)),
	}
}

type Manager struct {
	credentialSelection func(provider string) string
	retirementOriginMu  sync.RWMutex
	retirementOrigins   sync.Map // permanent automation provenance; durable in dataDir when configured

	mu         sync.RWMutex
	panes      map[string]*Pane
	dataDir    string
	onPaneExit func(*Pane)
	// onPaneRemoved fires after Kill takes a pane out of the manager (set
	// with SetOnPaneRemoved; main.go drops agents' open requests about it).
	onPaneRemoved func(id string)
	// onPromptsCommit is copied onto each pane at spawn/attach; fires
	// after the pane's LastPrompts history changes. Wired in main.go to
	// persist the pane and publish a "pane-prompt" UI event.
	onPromptsCommit func(*Pane)
	// onTurnStateEdge is copied onto each pane at spawn/attach; fires when a
	// native pane's in-turn flag flips. Wired in main.go to publish a real-time
	// pane-state (AgentMidTurn) edge.
	onTurnStateEdge func(*Pane)
	// onClaudeSessionRotated is copied onto each pane at spawn/attach; fires
	// when a pane's Claude session id rotates to a genuinely new value. Wired
	// in main.go to the delegation nudger's session-start digest.
	onClaudeSessionRotated func(paneID, sessionID string)
	// onCodexTurnFlagCommit is copied onto each pane at spawn/attach; fires when
	// a Codex Focus pane's DURABLE turn flag transitions. Wired in main.go to a
	// persist-only write (no UI event — see the Pane field's doc).
	onCodexTurnFlagCommit func(*Pane)
	// onDimsCommit is copied onto each pane at spawn/attach; fires after the
	// pane's Cols/Rows change. Wired in main.go to a debounced persist so
	// panes.json tracks the live PTY size across daemon restarts.
	onDimsCommit         func(*Pane)
	onOwnerViewersCommit func(*Pane) error
	// bus, when set, receives terminal-signal events (cmd.exit,
	// build.failed) emitted from readLoop once a pane's process has
	// exited. Optional — nil in tests or before main.go wires it.
	bus *uiBus
	// Internal auto-approval wakeups. Unlike uiBus, these do not fan out to
	// browser SSE clients or enter the activity history. A full channel is
	// safe: the regular activity poll and recovery sweep remain in place.
	autoPromptWake chan string
	// glmSettings holds the non-secret GLM (Z.ai) provider config + the live
	// key resolver, for panes spawned with Provider=="glm". Always non-nil
	// (newGlmSettingsStore falls back to defaults). The token is never stored
	// here — see glm_settings.go.
	glmSettings *glmSettingsStore
	// promptSuggestionsEnabled, when set, reports whether the per-machine
	// prompt-suggestions toggle is on; read fresh at every native (re)spawn to
	// decide whether --prompt-suggestions joins the headless claude argv. nil
	// in tests → treated as OFF. Wired in main.go from the native settings store.
	promptSuggestionsEnabled func() bool
	// Non-secret loopback endpoint for the per-pane auth bridge's refresh
	// requests. Only the daemon exchanges refresh grants; the bridge reads
	// the committed auth.json and never receives a grant over HTTP.
	codexDaemonURL string
	// Resolved at each spawn so a resumed pane receives a fresh local broker
	// capability and can never fall back to a saved personal CLI login.
	// ext holds optional features' state (extSlot, extensions.go).
	ext extState
}

func NewManager(dataDir string) *Manager {
	return &Manager{

		panes:          map[string]*Pane{},
		dataDir:        dataDir,
		autoPromptWake: make(chan string, 128),
		glmSettings:    newGlmSettingsStore(dataDir),
	}
}

func (m *Manager) List() []*Pane {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Pane, 0, len(m.panes))
	for _, p := range m.panes {
		out = append(out, p)
	}
	return out
}

func (m *Manager) Get(id string) (*Pane, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.panes[id]
	return p, ok
}

func lockPaneWriter(ctx context.Context, mu *sync.Mutex) error {
	if mu.TryLock() {
		return nil
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if mu.TryLock() {
				return nil
			}
		}
	}
}

func lockPaneReader(ctx context.Context, mu *sync.RWMutex) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if mu.TryRLock() {
		return nil
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if mu.TryRLock() {
				return nil
			}
		}
	}
}

func (p *Pane) agentRuntimeInfo() (version string, pid, proto int) {
	if p == nil {
		return "", 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pid = p.agentReportedPID
	if pid <= 0 {
		pid = p.Pid
	}
	return p.agentVersion, pid, p.agentProto
}

// DisplayTitle is the pane's header title (Title → Cmd[0] → ID), empty when
// the pane is unknown to this daemon. Deliberately the same precedence
// list_panes and the pane header use, so provenance stamped from here reads
// exactly like what the human sees on the pane.
//
// Nil-receiver safe: MCP/workflow tests build an apiServer without a manager,
// and a caller stamping provenance must degrade to "no title" rather than
// panic.
func (m *Manager) DisplayTitle(id string) string {
	if m == nil {
		return ""
	}
	p, ok := m.Get(id)
	if !ok {
		return ""
	}
	return displayTitle(p)
}

// withCurrentPane executes fn while holding a read ownership lease on p. A
// concurrent switch/restart cannot replace the pointer until fn returns, so a
// delayed process probe cannot persist stale socket/PID/kind data after the new
// pane. fn must not call back into Manager.
func (m *Manager) withCurrentPane(p *Pane, fn func()) bool {
	if p == nil || fn == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.withCurrentPaneLocked(p, fn)
}

// withCurrentPaneContext has the same ownership predicate as withCurrentPane,
// but bounds waiting for membership (e.g. an HTTP conditional input request).
func (m *Manager) withCurrentPaneContext(ctx context.Context, p *Pane, fn func()) (bool, error) {
	if p == nil || fn == nil {
		return false, nil
	}
	if err := lockPaneReader(ctx, &m.mu); err != nil {
		return false, err
	}
	defer m.mu.RUnlock()
	return m.withCurrentPaneLocked(p, fn), nil
}

func (m *Manager) withCurrentPaneLocked(p *Pane, fn func()) bool {
	if current, ok := m.panes[p.ID]; !ok || current != p {
		return false
	}
	fn()
	return true
}

// trackModes scans a PTY output chunk for DEC private mode toggles
// and updates p.altScreen when mode 1049 is set or reset. Handles
// generic `CSI ? <params> h/l` sequences, so both \x1b[?1049h and
// combined forms like \x1b[?25;1049h are recognised — every entry
// in the semicolon-separated parameter list is inspected. A small
// allowlist of other DEC modes (cursor keys, mouse reporting,
// bracketed paste, ...) is recorded in p.modeState so tail-capped
// replays can re-assert them; see replayModePrefix.
//
// Caller MUST hold p.subsMu (write lock) — trackModes writes
// p.altScreen and p.modeTail without further synchronisation, on
// the assumption that readLoop sequences Write + trackModes +
// fanout under a single subsMu.Lock so Subscribe can observe a
// self-consistent (snapshot, altScreen) pair.
//
// modeTail carries unconsumed bytes from the previous call so a
// sequence that straddles a 32 KB payload boundary is still
// recognised. It is capped at 32 bytes — larger than any realistic
// DEC-private set/reset — to bound memory and prevent an adversary
// (or a bug emitting `\x1b[?` forever) from growing it without limit.
func (p *Pane) trackModes(b []byte) {
	var scan []byte
	if len(p.modeTail) > 0 {
		scan = make([]byte, 0, len(p.modeTail)+len(b))
		scan = append(scan, p.modeTail...)
		scan = append(scan, b...)
	} else {
		scan = b
	}

	i := 0
	for i < len(scan) {
		if scan[i] != 0x1b {
			i++
			continue
		}
		// Need at least ESC [ ? to be a candidate.
		if i+2 >= len(scan) {
			break // partial; preserve in tail.
		}
		if scan[i+1] != '[' || scan[i+2] != '?' {
			i++
			continue
		}
		// Walk the parameter list: digits and ';' until 'h' or 'l'.
		j := i + 3
		terminated := false
		bogus := false
		for j < len(scan) {
			c := scan[j]
			if c == 'h' || c == 'l' {
				terminated = true
				break
			}
			if c != ';' && (c < '0' || c > '9') {
				bogus = true
				break
			}
			j++
		}
		if bogus {
			i++ // not a mode set/reset — skip this ESC and keep scanning.
			continue
		}
		if !terminated {
			break // incomplete; preserve from i onward in tail.
		}
		set := scan[j] == 'h'
		forEachParam(scan[i+3:j], func(mode int) {
			if mode == 1049 {
				p.altScreen = set
				return
			}
			if _, tracked := replayTrackedModes[mode]; tracked {
				if p.modeState == nil {
					p.modeState = make(map[int]bool, 8)
				}
				p.modeState[mode] = set
			}
		})
		i = j + 1
	}

	// Preserve the tail. Anything from i onward is either past the
	// last complete sequence or the start of a partial one we'll
	// finish parsing on the next call. Cap at 32 bytes — a complete
	// DEC private set/reset is well under that.
	tailStart := i
	if len(scan)-tailStart > 32 {
		tailStart = len(scan) - 32
	}
	p.modeTail = append(p.modeTail[:0], scan[tailStart:]...)
}

// paramListContains reports whether `params` (a byte slice like
// "25;1049" or "1049" from a CSI DEC-private sequence) contains
// `want` as one of its semicolon-separated decimal parameters.
// Leading/empty params are treated as 0 (matching xterm), so
// `;1049` and `1049;` both match 1049.
func paramListContains(params []byte, want int) bool {
	n := 0
	for _, c := range params {
		if c == ';' {
			if n == want {
				return true
			}
			n = 0
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n == want
}

// forEachParam invokes fn for every semicolon-separated decimal
// parameter in a CSI DEC-private parameter list. Same parsing rules
// as paramListContains (empty params read as 0). Bails on any
// non-digit/non-semicolon byte, matching the caller's pre-validation.
func forEachParam(params []byte, fn func(int)) {
	n := 0
	for _, c := range params {
		if c == ';' {
			fn(n)
			n = 0
			continue
		}
		if c < '0' || c > '9' {
			return
		}
		n = n*10 + int(c-'0')
	}
	fn(n)
}

// replayTrackedModes is the allowlist of DEC private modes whose
// last-seen state is recorded for replay re-synthesis, in the order
// they're emitted by replayModePrefix. TUIs set these once at
// startup; a tail-capped replay would otherwise lose them and the
// reattached terminal would silently drop bracketed paste, mouse
// events, or application cursor keys. 1049 (alt screen) is handled
// separately via p.altScreen.
var replayTrackedModeOrder = []int{1, 7, 12, 25, 1000, 1002, 1003, 1004, 1005, 1006, 2004}

var replayTrackedModes = func() map[int]struct{} {
	m := make(map[int]struct{}, len(replayTrackedModeOrder))
	for _, mode := range replayTrackedModeOrder {
		m[mode] = struct{}{}
	}
	return m
}()

// replayModePrefix rebuilds set/reset sequences for every tracked
// mode the pane has ever toggled, for prepending to a truncated
// replay. Caller must hold p.subsMu (read or write) — same as the
// snapshot it accompanies, so the prefix is consistent with the cut.
func (p *Pane) replayModePrefix() []byte {
	if len(p.modeState) == 0 {
		return nil
	}
	var b []byte
	for _, mode := range replayTrackedModeOrder {
		set, seen := p.modeState[mode]
		if !seen {
			continue
		}
		final := byte('l')
		if set {
			final = 'h'
		}
		b = append(b, fmt.Sprintf("\x1b[?%d%c", mode, final)...)
	}
	return b
}

// stampLiveFrameActivity is called under subsMu only for FrameData. Replay
// still advances transcript/grid positions but does not change historical age.
func (p *Pane) stampLiveFrameActivity() {
	now := time.Now
	if p.frameReceivedAt != nil {
		now = p.frameReceivedAt
	}
	atomic.StoreInt64(&p.LastActivityMs, now().UnixMilli())
}

// readLoop consumes frames from the agent and fans PTY bytes out to WS subs.
func (p *Pane) readLoop(m *Manager) {
	// Capture the connection once at the start of the loop. markExited
	// (which may fire below) nils out p.agentConn, and without a captured
	// reference a subsequent ReadFrame(nil) would crash.
	p.mu.Lock()
	conn := p.agentConn
	p.mu.Unlock()
	if conn == nil {
		return
	}

	// exit runs once — on clean FrameExit, on EOF, or on any frame-read
	// error. Centralises the bookkeeping so we can't leave `done` unclosed.
	exited := false
	exit := func(code *int) {
		if exited {
			return
		}
		exited = true
		p.markExited(code)
		if m != nil && m.onPaneExit != nil {
			m.onPaneExit(p)
		}
		// Terminal-signal events for the aria-watcher. markExited has
		// already finalized ExitCode and closed the agent socket, so
		// emitting here avoids the race the spec warns about.
		if m != nil && m.bus != nil {
			m.emitPaneExitEvents(p)
		}
		close(p.done)
	}

	for {
		t, payload, err := ReadFrame(conn)
		if err != nil {
			exit(nil)
			return
		}
		switch t {
		case FrameData, FrameReplay:
			if len(payload) > 0 {
				gridGenerationBefore := atomic.LoadUint64(&p.GridChoicePromptGeneration)
				// One atomic critical section per payload: append to
				// the ringbuf, update derived state (stripped-char
				// counter, alt-screen mode), and fan the bytes out
				// to subscribers, all under subsMu.Lock.
				//
				// The atomicity matters for Subscribe: it takes the
				// SAME subsMu.Lock to read the snapshot AND the
				// altScreen bit, so whichever point between payloads
				// it lands on, it sees a self-consistent (bytes,
				// mode) pair. An earlier split-lock design let a
				// snapshot taken mid-payload pair with a stale
				// altScreen — Codex flagged it as the remaining bug.
				//
				// Holding a write lock during fanout is fine because
				// channel sends are non-blocking (drop-on-full). No
				// slow-subscriber backpressure reaches this lock.
				p.subsMu.Lock()
				if p.Kind == "native" {
					// Native panes (Kind=="native", the DEFAULT) carry
					// claude -p stream-json, NOT terminal bytes. They render
					// exclusively via the per-pane native SSE stream
					// (SubscribeNative → nativeParseLoop), so the whole
					// terminal-rendering machinery is dead weight here and was
					// the heaviest always-on per-payload cost at scale:
					//   - replayEmu.Feed / DrainReplies  (VT emulation)
					//   - countStrippedChars              (state-detection counter)
					//   - trackModes                      (alt-screen tracking)
					//   - fanoutPayloadLocked             (bytes-WS subscribers)
					// none of which a native pane ever consults. We MUST still:
					//   - tee to nativeTeeCh (this is how native rendering works)
					//   - keep the ring buffer current (debug-tail / export read it)
					//   - stamp LastActivityMs (state freshness)
					//
					// The bytes-fanout / replayEmu skip is gated on
					// len(p.subs)==0: native panes are served by NativePane on
					// the client (never the xterm bytes path, see PaneGrid.tsx /
					// WarmPaneLayer.tsx), so p.subs is always empty for them. The
					// guard makes the skip self-correcting and zero-risk: if a
					// bytes subscriber ever did attach, we fall back to feeding
					// the emulator and fanning out so reattach/lag-repair stays
					// correct.
					p.buffer.Write(payload)
					if p.nativeTeeCh != nil {
						// Raw native bytes are a machine protocol. Backpressure the
						// local reader rather than dropping an approval/turn boundary;
						// normalized browser fanout remains Seq-resynchronizable.
						p.nativeTeeCh <- nativeChunk{data: append([]byte(nil), payload...), replay: t == FrameReplay}
					}
					if len(p.subs) > 0 {
						if p.replayEmu != nil {
							p.replayEmu.Feed(payload)
							_ = p.replayEmu.DrainReplies()
						}
						p.fanoutPayloadLocked(payload)
					}
					if t == FrameData {
						p.stampLiveFrameActivity()
					}
					p.subsMu.Unlock()
					break
				}
				p.buffer.Write(payload)
				// Native panes (Kind=="native") emit JSONL, not terminal
				// output: the VT scrollback emulator (replayEmu) and the
				// stripped-char counter (TotalStrippedChars) are pure waste
				// for them. The native renderer consumes the PARSED event ring
				// (fed by the nativeTeeCh tee below), never these VT models, so
				// skipping them costs a native pane nothing it reads while
				// removing per-payload Feed/DrainReplies CPU and a stripANSI
				// allocation. `p.nativeTeeCh != nil` is the SAME native signal
				// the tee uses; for terminal panes (Kind=="") it stays nil
				// forever, so the terminal branch below is byte-identical to
				// the prior unconditional path — the fallback guarantee. We
				// KEEP buffer.Write, the grid tee, the native tee, trackModes,
				// LastActivityMs, and fanout for BOTH kinds (each gated on its
				// own already-existing nil/enabled check), so nothing a native
				// pane genuinely needs is dropped.
				if p.nativeTeeCh == nil {
					// Terminal-pane VT machinery. Order preserved exactly.
					if vtPaintEnabled && p.vt != nil {
						_, _ = p.vt.Write(payload)
					}
					if p.replayEmu != nil {
						p.replayEmu.Feed(payload)
						_ = p.replayEmu.DrainReplies()
					}
				}
				// Grid-emulator tee: same byte stream, independent model.
				// Only costs CPU when the emulator is initialised (ARIA
				// auto-mode panes). Feeds ARIA prompt/choice detection and
				// the debug-grid endpoint via GridSnapshot.
				if p.gridEmu != nil {
					p.feedGridPayloadLocked(payload, t == FrameData)
				}
				// Native-mode tee: hand a payload copy to the parser
				// goroutine, which runs the adapter OFF this lock. Only
				// non-nil for Kind=="native" panes; terminal panes pay one
				// pointer compare. The send is lossless and can backpressure:
				// dropping raw machine-protocol bytes can orphan approvals or
				// turn boundaries. No JSON work happens under this lock.
				if p.nativeTeeCh != nil {
					p.nativeTeeCh <- nativeChunk{data: append([]byte(nil), payload...), replay: t == FrameReplay}
				} else {
					// TotalStrippedChars feeds the stripped-char position math
					// in computeState's TUI byte-scan and the auto-approve
					// recheck — both terminal-only. A native pane short-circuits
					// computeState on its pending-approval count and never scans
					// the buffer for a TUI menu, so this strip-count is skipped
					// for native panes. Kept inside the same terminal-only guard
					// so the counter only advances when the scan that reads it
					// can run. countStrippedChars is the allocation-free
					// equivalent of len(stripANSI(payload)).
					atomic.AddInt64(&p.TotalStrippedChars, int64(countStrippedChars(payload)))
				}
				p.trackModes(payload)
				if t == FrameData {
					p.stampLiveFrameActivity()
				}
				p.fanoutPayloadLocked(payload)
				hasAutoGrid := p.gridEmu != nil
				p.subsMu.Unlock()
				if t == FrameData && hasAutoGrid && m != nil {
					p.mu.Lock()
					autoMode := p.AutoMode
					p.mu.Unlock()
					if autoMode != "" {
						if atomic.LoadUint64(&p.GridChoicePromptGeneration) != gridGenerationBefore {
							m.enqueueAutoPromptWake(p.ID)
						} else if p.autoPromptExpected() || autoPromptRawOutputHint(p.buffer.SnapshotTail(512)) {
							p.scheduleRawAutoPromptWake(m)
						}
					}
				}
			}
		case FrameSyntheticPaint:
			if len(payload) > 0 {
				// Synthetic paint is a one-shot repair frame from the
				// long-lived pane agent after daemon reconnect/update. It
				// is NOT historical PTY output: do not store it in the
				// ring buffer and do not fan it out directly. Older agents
				// serialized ANSI palette colors as hardcoded truecolor
				// (e.g. dark #000080), which made update replay render
				// paths dark until fresh shell output repainted them.
				//
				// Instead, feed only our daemon-side grid models. When
				// FrameReplayEnd arrives, emitCodexReplayRepair reserializes
				// the current grid with this daemon's serializer, preserving
				// ANSI colors as themeable SGR palette codes.
				p.subsMu.Lock()
				if p.replayEmu != nil {
					p.replayEmu.Feed(payload)
					_ = p.replayEmu.DrainReplies()
				}
				if p.gridEmu != nil {
					p.feedGridPayloadLocked(payload, false)
				}
				p.subsMu.Unlock()
			}
		case FrameDims:
			// The agent's report of the PTY's real dimensions — the first
			// frame on every (re)attach. Adopt it: after a daemon restart
			// this pane's dims came from stale panes.json, and serving that
			// lie from /api/panes made reconnecting clients resize the real
			// PTY to it (the every-update garble). See adoptAgentDims.
			var rs struct {
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if json.Unmarshal(payload, &rs) == nil {
				p.adoptAgentDims(rs.Cols, rs.Rows)
			}
		case FrameAgentInfo:
			var rs struct {
				Version    string `json:"version"`
				PID        int    `json:"pid"`
				Proto      int    `json:"proto"`
				Retirement bool   `json:"ownerRetirement,omitempty"`
			}
			if json.Unmarshal(payload, &rs) == nil && rs.PID > 0 {
				p.mu.Lock()
				// The known wrapper PID is the ownership boundary; never let a
				// malformed frame redirect inventory to an unrelated process.
				if p.Pid <= 0 || p.Pid == rs.PID {
					p.agentVersion = strings.TrimSpace(rs.Version)
					p.agentReportedPID = rs.PID
					p.agentProto = rs.Proto
					p.ownerRetirementSupported = rs.Retirement
				}
				p.mu.Unlock()
			}
		case FrameOwnerRetireReply:
			var reply ownerRetirementReply
			if len(payload) <= 8192 && json.Unmarshal(payload, &reply) == nil {
				p.mu.Lock()
				replies := p.ownerRetirementReplies
				p.mu.Unlock()
				if replies != nil {
					select {
					case replies <- reply:
					default:
					}
				}
			}
		case FrameReplayEnd:
			// Historical replay bytes can contain any number of old synchronized
			// frames. Install only the final screen as the live lifecycle baseline;
			// replay transients must never turn over an answered prompt.
			p.subsMu.Lock()
			if p.gridEmu != nil {
				p.resetGridPromptLifecycleLocked(p.gridEmu)
			}
			p.subsMu.Unlock()
			if p.Kind == "native" {
				p.subsMu.RLock()
				tee := p.nativeTeeCh
				p.subsMu.RUnlock()
				if tee != nil {
					// Unlike raw client fanout, this protocol boundary is
					// load-bearing and must never be dropped.
					select {
					case tee <- nativeChunk{replayEnd: true}:
					case <-p.done:
						return
					}
				}
			}
			p.emitCodexReplayRepair()
		case FrameExit:
			var parsed struct {
				Code int `json:"code"`
			}
			_ = json.Unmarshal(payload, &parsed)
			code := parsed.Code
			exit(&code)
			return
		}
	}
}

// enqueueAutoPromptWake is an internal, best-effort signal. The periodic
// activity watcher and 5-second recovery sweep remain the fallback if a
// burst fills the channel or the ARIA watcher is not yet running.
func (m *Manager) enqueueAutoPromptWake(paneID string) {
	if m == nil || m.autoPromptWake == nil {
		return
	}
	select {
	case m.autoPromptWake <- paneID:
	default:
	}
}

// A text-streamed menu can lack a synchronized-output commit. This cheap
// candidate hint uses just the last 512 raw bytes; the watcher later runs
// the full state and approval classifiers. Both a numbered option and a
// question, focus marker, or second option must be visible to limit wakes
// during ordinary output. Split frames are covered by the recent ring tail.
func autoPromptRawOutputHint(tail []byte) bool {
	if !bytes.Contains(tail, []byte("1.")) {
		return false
	}
	return bytes.IndexByte(tail, '?') >= 0 ||
		bytes.Contains(tail, []byte("❯")) ||
		bytes.Contains(tail, []byte("›")) ||
		bytes.Contains(tail, []byte("2."))
}

// autoPromptExpectWindow bounds how long after a PreToolUse hook or an
// auto-fire the output path keeps scheduling prompt wakes. It covers
// Claude's permission checks and queued subagent menus without turning
// ordinary output into a standing per-payload state scan.
const autoPromptExpectWindow = 3 * time.Second

// expectAutoPrompt opens the expected-menu window for an auto-mode pane.
func (p *Pane) expectAutoPrompt() {
	p.autoPromptExpectUntilMs.Store(time.Now().Add(autoPromptExpectWindow).UnixMilli())
}

func (p *Pane) autoPromptExpected() bool {
	return time.Now().UnixMilli() < p.autoPromptExpectUntilMs.Load()
}

func (p *Pane) scheduleRawAutoPromptWake(m *Manager) {
	p.autoPromptRawWakeMu.Lock()
	if p.autoPromptRawWakeTimer != nil {
		p.autoPromptRawWakeAgain = true
		p.autoPromptRawWakeMu.Unlock()
		return
	}
	delay := 120 * time.Millisecond
	if sinceLast := time.Until(p.autoPromptLastRawWake.Add(500 * time.Millisecond)); sinceLast > delay && !p.autoPromptExpected() {
		delay = sinceLast
	}
	// Let multi-frame text menus finish painting. A partial first frame can
	// otherwise acknowledge a menu before its options are visible. Within
	// the throttle window, retain the hint for one delayed wake instead of
	// dropping a second prompt that followed the first in under 500ms.
	p.autoPromptRawWakeTimer = time.AfterFunc(delay, func() {
		p.autoPromptRawWakeMu.Lock()
		p.autoPromptRawWakeTimer = nil
		p.autoPromptLastRawWake = time.Now()
		again := p.autoPromptRawWakeAgain
		p.autoPromptRawWakeAgain = false
		p.autoPromptRawWakeMu.Unlock()
		m.enqueueAutoPromptWake(p.ID)
		if again {
			p.scheduleRawAutoPromptWake(m)
		}
	})
	p.autoPromptRawWakeMu.Unlock()
}

// fanoutPayloadLocked delivers one PTY payload to every subscriber.
// Caller holds subsMu (write). Sends are non-blocking: a full channel
// means the sub's WS writer can't drain fast enough (slow uplink — the
// cloud relay during an output burst is the canonical case). Dropping
// payloads corrupts that sub's terminal — Claude/Codex emit
// relative-cursor diff frames, so missing bytes leave every later
// frame landing on wrong content (jumbled duplicates).
//
// Lag protocol (exact, no grid-model exposure): on the first failed
// send the sub is marked lagged with the ring offset where its stream
// stopped, and it receives NOTHING further (sporadic successes would
// interleave post-gap bytes out of order). Once its channel drains
// below half capacity, it gets one catch-up slice — the exact ring
// bytes from laggedFrom to now, which includes the current payload —
// and resumes the live stream seamlessly. Only if the ring has
// already evicted the gap (multi-MiB stall) do we fall back to the
// deferred grid repaint, accepting model-drift risk as last resort.
func (p *Pane) fanoutPayloadLocked(payload []byte) {
	for id, e := range p.subs {
		if e.lagged {
			if len(e.ch) > cap(e.ch)/2 {
				continue // still draining; ring keeps accumulating for us
			}
			slice, ok := p.buffer.SnapshotSince(e.laggedFrom)
			if !ok {
				e.lagged = false
				p.scheduleSubRepairLocked(id, e, "lag overflow")
				continue
			}
			// Strip one-shot terminal queries: the agent's reply pump
			// answered them while no client was listening; replaying
			// them would make xterm.js answer again into the PTY.
			clean := stripTerminalQuerySequences(slice)
			select {
			case e.ch <- clean:
				e.lagged = false
				log.Printf("pane %s: lag catch-up → sub %d (%d bytes exact)",
					p.ID, id, len(clean))
			default:
				// Shouldn't happen at half capacity; retry next payload.
			}
			continue // current payload is inside the slice
		}
		select {
		case e.ch <- payload:
		default:
			e.lagged = true
			// The payload was already appended to the ring by readLoop,
			// so the sub's stream stopped just BEFORE it.
			e.laggedFrom = p.buffer.TotalWritten() - int64(len(payload))
		}
	}
}

// Quiet-gating constants for deferred sub repairs. PTY reads are 32 KiB
// payload chunks, NOT TUI frames — a big frame spans several payloads,
// so a repaint serialized at an arbitrary payload boundary can be
// mid-frame. Mid-frame paints can't express transient parser state
// (pending-wrap at the last column is the classic case: the paint's
// closing CUP clears it, desyncing the client by one cell/row for the
// rest of the frame) and that's how old prompt text reappeared as
// uneditable ghosts in Claude's input row (v0.0.509 report). 80ms of
// PTY silence is a reliable frame boundary — Claude/Codex emit a
// frame's payloads back-to-back — and the 2s ceiling guarantees a
// continuously-chatty pane still gets repaired rather than waiting
// forever with a stale screen.
const (
	subRepairQuiet   = 80 * time.Millisecond
	subRepairMaxWait = 2 * time.Second
	subRepairPoll    = 25 * time.Millisecond
)

// scheduleSubRepair queues a deferred grid repaint for one subscriber,
// to run at the next quiet moment in the PTY stream. Used after any
// event that may have left the sub's terminal diverged from the pane's
// real screen: a skip-replay reconnect (bytes lost in the WS gap), a
// fanout lag drop, or a truncated replay whose inline paint may have
// been serialized mid-frame. Dedupes per sub via repairPending.
func (p *Pane) scheduleSubRepair(subID int64, reason string) {
	p.subsMu.Lock()
	e, ok := p.subs[subID]
	if ok {
		p.scheduleSubRepairLocked(subID, e, reason)
	}
	p.subsMu.Unlock()
}

// scheduleSubRepairLocked is scheduleSubRepair for callers already
// holding subsMu (write).
func (p *Pane) scheduleSubRepairLocked(subID int64, e *subEntry, reason string) {
	if e.repairPending {
		return
	}
	e.repairPending = true
	go p.runSubRepair(subID, reason)
}

// runSubRepair waits for a frame boundary (subRepairQuiet of PTY
// silence, or the subRepairMaxWait ceiling), then serializes the
// replay emulator and queues the paint on the sub's fanout channel.
// FIFO ordering through that channel keeps the paint consistent with
// live bytes: anything fanned out after the serialization (both under
// subsMu) lands after the paint. Exits when the sub disconnects or
// the pane dies; alt-screen and blank panes get no paint (alt-screen
// repairs ride the SIGWINCH bump instead).
func (p *Pane) runSubRepair(subID int64, reason string) {
	start := time.Now()
	observedActivity := atomic.LoadInt64(&p.LastActivityMs)
	quietSince := start
	tick := time.NewTicker(subRepairPoll)
	defer tick.Stop()
	for {
		now := time.Now()
		last := atomic.LoadInt64(&p.LastActivityMs)
		if last != observedActivity {
			observedActivity = last
			quietSince = now
		}
		quiet := last == 0 || now.Sub(quietSince) >= subRepairQuiet
		maxWait := now.Sub(start) >= subRepairMaxWait
		ready := quiet || maxWait

		if ready {
			p.subsMu.Lock()
			e, ok := p.subs[subID]
			if !ok {
				p.subsMu.Unlock()
				return
			}
			// LastActivityMs is stamped by readLoop under subsMu. Recheck it
			// after acquiring the same lock so a payload that landed between the
			// poll and this critical section cancels the repair instead of having
			// a stale paint spliced immediately after it. Track quiet time with
			// time.Time's monotonic component rather than trusting the wall-clock
			// age encoded in LastActivityMs; WSL clock corrections can otherwise
			// make a fresh stream look 80ms old for one poll.
			now = time.Now()
			last = atomic.LoadInt64(&p.LastActivityMs)
			if last != observedActivity {
				observedActivity = last
				quietSince = now
			}
			maxWait = now.Sub(start) >= subRepairMaxWait
			if last != 0 && now.Sub(quietSince) < subRepairQuiet && !maxWait {
				p.subsMu.Unlock()
				select {
				case <-p.done:
					return
				case <-tick.C:
				}
				continue
			}
			// Native/mobile terminal subs render the raw stream + their own
			// scrollback; a synthetic CUP repair clears that scrollback and
			// pins a stale frame. Web/PWA mobile clients keep this false
			// because their xterm.js full-replay fallback still needs repair.
			if e.skipSyntheticPaint {
				e.repairPending = false
				p.subsMu.Unlock()
				return
			}
			var paint []byte
			if !p.altScreen && p.replayEmu != nil {
				if snap := p.replayEmu.Snapshot(); gridSnapshotHasText(snap) {
					skipRow := -1
					if !p.HasCodexDescendant() && !p.HasMuseDescendant() {
						// Live surface: never let model drift overwrite
						// the TUI's input row (see serializeGridSnapshot).
						skipRow = snap.CursorY
					}
					// Clamp COLS to THIS subscriber's width, same as the
					// inline replay path (pane_subscribe.go). The unclamped
					// model is still at the owner's (possibly wide) cols, so
					// emitting raw CUP coords overflows a narrow/mobile xterm
					// into garbage. This repair fires on exactly the
					// lag/reconnect bursts a multi-subagent run produces, so
					// an unclamped paint makes the repair itself jumble.
					// maxRows=0: never clamp rows — that dropped Codex's
					// bottom input row on a phone whose row count lagged the
					// live grid (virtual keyboard).
					paint = serializeGridSnapshotClamped(snap, skipRow, int(e.cols), 0)
				}
			}
			if paint == nil {
				e.repairPending = false
				p.subsMu.Unlock()
				return
			}
			select {
			case e.ch <- paint:
				e.repairPending = false
				p.subsMu.Unlock()
				log.Printf("pane %s: repair paint → sub %d (%s, waited %dms, %d bytes)",
					p.ID, subID, reason, time.Since(start).Milliseconds(), len(paint))
				return
			default:
				// Channel full — writer still draining. Keep waiting;
				// the sub-exists check above bounds the loop's lifetime.
			}
			p.subsMu.Unlock()
		}

		select {
		case <-p.done:
			return
		case <-tick.C:
		}
	}
}

func (p *Pane) emitCodexReplayRepair() {
	if (!p.HasCodexDescendant() && !p.HasMuseDescendant()) || p.replayEmu == nil {
		return
	}
	p.subsMu.Lock()
	if p.altScreen {
		p.subsMu.Unlock()
		return
	}
	snap := p.replayEmu.Snapshot()
	if !gridSnapshotHasText(snap) {
		p.subsMu.Unlock()
		return
	}
	// Clamp per-sub to each viewer's geometry. Broadcasting the owner-width
	// paint shattered box-drawing on a narrower (phone) viewer — same bug
	// the resize-repair path had; see gridPaintForSub. A sub at owner width
	// clamps to a no-op.
	for _, e := range p.subs {
		paint := p.gridPaintForSub(snap, e)
		if len(paint) == 0 {
			continue
		}
		select {
		case e.ch <- paint:
		default:
		}
	}
	p.subsMu.Unlock()
}

func (p *Pane) markExited(code *int) {
	p.mu.Lock()
	p.Status = PaneExited
	if p.ExitCode == nil {
		p.ExitCode = code
	}
	// A natural process-exit of a pane that captured its provider-specific
	// conversation id is RESUMABLE — the client offers "Resume
	// session" instead of forcing a fresh pane. An explicit user kill
	// (userClosing) or a session-less pane (plain shell) is not.
	p.Resumable = !p.userClosing &&
		(strings.TrimSpace(p.HarnessSessionID) != "" ||
			conversationIDForAgent(p.Agent, p.ClaudeSessionID, p.CodexThreadID, p.MuseSessionID) != "")
	p.mu.Unlock()

	// close subscribers
	p.subsMu.Lock()
	for _, e := range p.subs {
		close(e.ch)
	}
	p.subs = map[int64]*subEntry{}
	p.subsMu.Unlock()

	p.shutdownGrid()
	p.shutdownNative()

	p.resizeSettleMu.Lock()
	if p.resizeSettleTimer != nil {
		p.resizeSettleTimer.Stop()
		p.resizeSettleTimer = nil
	}
	p.resizeSettleMu.Unlock()

	p.resizeCoalesceMu.Lock()
	if p.resizeCoalesceTimer != nil {
		p.resizeCoalesceTimer.Stop()
		p.resizeCoalesceTimer = nil
	}
	p.resizeCoalesceMu.Unlock()

	p.agentWriteMu.Lock()
	if p.wheelFlushTimer != nil {
		p.wheelFlushTimer.Stop()
		p.wheelFlushTimer = nil
	}
	p.agentWriteMu.Unlock()

	p.mu.Lock()
	conn := p.agentConn
	p.agentConn = nil
	p.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// Write forwards keystrokes to the agent and feeds them to the prompt
// recorder (the last-prompts strip).
func (p *Pane) Write(b []byte) (int, error) {
	return p.write(b, true)
}

// WriteNoPromptRecord forwards bytes to the agent WITHOUT feeding the
// prompt recorder. Auto-approval and menu-choice replies use this: they
// are control answers to a TUI menu, not user prompts. Recording them
// both polluted the strip with "1"/"11"/"111" AND — because the body
// digit is written before the submit-Enter and the Enter is skipped when
// the prompt changes mid-write (see toolSendInput) — left the digit
// lingering in the recorder buffer to prepend onto the next real prompt
// ("2/usage"). Bypassing the recorder keeps the digit out of its buffer
// entirely, killing both symptoms at the source.
func (p *Pane) WriteNoPromptRecord(b []byte) (int, error) {
	return p.write(b, false)
}

func (p *Pane) write(b []byte, recordPrompt bool) (int, error) {
	return p.writeWithFleetAdmissionMode(b, recordPrompt, false)
}

// setFleetTurnAdmission serializes latch transitions without taking
// agentWriteMu. Provider output must be able to report authoritative completion
// while the transport write is still blocked.
func (p *Pane) setFleetTurnAdmission(active bool) uint64 {
	p.fleetTurnStateMu.Lock()
	p.fleetTurnStateGeneration++
	generation := p.fleetTurnStateGeneration
	p.fleetTurnAdmitted.Store(active)
	p.fleetTurnStateMu.Unlock()
	return generation
}

// clearFleetTurnAdmissionIfCurrent rolls back only the admission installed by
// this write. A completion or later provider event advances the generation and
// must not be overwritten by delayed transport error handling.
func (p *Pane) clearFleetTurnAdmissionIfCurrent(generation uint64) {
	p.fleetTurnStateMu.Lock()
	if p.fleetTurnStateGeneration == generation {
		p.fleetTurnStateGeneration++
		p.fleetTurnAdmitted.Store(false)
	}
	p.fleetTurnStateMu.Unlock()
}

// writeNativeTurn makes a successfully submitted native provider turn visible
// to restart safety before releasing agentWriteMu. Prompt persistence and UI
// bookkeeping happen later and may block.
func (p *Pane) writeNativeTurn(b []byte) (int, error) {
	return p.writeWithFleetAdmissionMode(b, false, true)
}

func (p *Pane) writeWithFleetAdmissionMode(b []byte, recordPrompt, nativeTurn bool) (int, error) {
	return p.writeWithFleetAdmissionPolicy(b, recordPrompt, nativeTurn, nil, time.Time{}, nil)
}

func (p *Pane) writeWithAutoModePolicy(b []byte, recordPrompt bool, policy paneAutoModePolicy, deadline time.Time) (int, error) {
	return p.writeWithFleetAdmissionPolicy(b, recordPrompt, false, &policy, deadline, nil)
}

// The check and write share the writer lock: another input cannot slip between
// a recovery action's composer check and its keystroke.
func (p *Pane) writeWithPrecondition(b []byte, recordPrompt, nativeTurn bool, check func() error) (int, error) {
	return p.writeWithFleetAdmissionPolicy(b, recordPrompt, nativeTurn, nil, time.Time{}, check)
}

func (p *Pane) writeWithFleetAdmissionPolicy(b []byte, recordPrompt, nativeTurn bool, policy *paneAutoModePolicy, deadline time.Time, check func() error) (int, error) {
	if err := p.ownerRetirementInputBarrier(); err != nil {
		return 0, err
	}
	menuShortcut := recordPrompt && terminalPermissionShortcut(p, b)
	// Sampled before the write, like the shortcut: is this keystroke answering
	// a menu rather than typing into the composer? Not under agentWriteMu: the
	// screen helpers take subsMu.
	menu := menuInputNone
	if recordPrompt && p.Kind != "native" && !isTerminalReplyOnly(b) {
		menu = p.menuInputState()
	}
	p.mu.Lock()
	conn := p.agentConn
	p.mu.Unlock()
	if conn == nil {
		return 0, errors.New("pane not connected")
	}
	p.agentWriteMu.Lock()
	if check != nil {
		if err := check(); err != nil {
			p.agentWriteMu.Unlock()
			return 0, err
		}
	}
	// Keep split terminal-control framing intact: a printable fragment inside
	// an OSC/CSI is not a menu key even if it happens to be "y" or "1".
	if menuShortcut && !p.promptRec.inStringControl && !p.promptRec.inCSI && !p.promptRec.escapePending {
		recordPrompt = false
	}
	if p.codexLiveMigrating.Load() {
		p.agentWriteMu.Unlock()
		return 0, errors.New("Codex connection is being upgraded; retry input shortly")
	}
	// ConPTY drops mouse-wheel reports on the floor (see input_wheel.go), so
	// on Windows alt-screen panes they are converted to page keys here — the
	// choke point every input path (WS binary/text, inject) funnels through.
	// Only the PTY payload changes: presence classification and the prompt
	// recorder below still see the ORIGINAL bytes, so a scroll can never
	// count as the user typing (isTerminalReplyOnly already classifies wheel
	// reports as autonomous chatter — translated page keys would not be).
	payload := b
	if wheelNeedsPageKeys && p.inAltScreen() {
		payload = translateWheelBytes(b, &p.wheelAcc, p.wheelPageLines(), time.Now())
		// A short gesture may end below the page threshold — flush its
		// remainder as one page once it goes quiet (input_wheel.go).
		p.scheduleWheelFlush()
		if len(payload) == 0 {
			// Wheel lines swallowed into the accumulator — nothing to send
			// yet, and the bookkeeping below is a no-op for pure wheel input.
			p.agentWriteMu.Unlock()
			return len(b), nil
		}
	}
	var nativeAdmissionGeneration uint64
	if nativeTurn {
		// Arm before the request is visible to the provider. Completion can race
		// WriteFrame returning, and its clear must remain authoritative.
		nativeAdmissionGeneration = p.setFleetTurnAdmission(true)
	}
	releaseInput, inputErr := p.codexLiveInputLease(b)
	if inputErr != nil {
		if nativeTurn {
			p.clearFleetTurnAdmissionIfCurrent(nativeAdmissionGeneration)
		}
		p.agentWriteMu.Unlock()
		return 0, inputErr
	}
	err := func() error {
		if policy == nil {
			return WriteFrame(conn, FrameData, payload)
		}
		// Serialize the final policy comparison and transport write with
		// SetAutoMode. A queued writer must not reuse a revoked decision.
		p.autoModeMu.Lock()
		defer p.autoModeMu.Unlock()
		p.mu.Lock()
		if !policy.matchesLocked(p) || p.agentConn != conn {
			p.mu.Unlock()
			return errAutoModePolicyChanged
		}
		p.mu.Unlock()
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return err
		}
		defer conn.SetWriteDeadline(time.Time{})
		frame := autoModeFrameWriter{Writer: conn}
		err := WriteFrame(&frame, FrameData, payload)
		if err != nil && frame.written > 0 {
			// A timeout may have written the frame header but not its body.
			// Reusing that stream would corrupt every subsequent frame.
			_ = conn.Close()
			p.mu.Lock()
			if p.agentConn == conn {
				p.agentConn = nil
			}
			p.mu.Unlock()
		}
		return err
	}()
	releaseInput()
	if err != nil {
		if nativeTurn {
			p.clearFleetTurnAdmissionIfCurrent(nativeAdmissionGeneration)
		}
		p.agentWriteMu.Unlock()
		return 0, err
	}
	commits := p.observeSuccessfulInputLocked(b, recordPrompt, menu)
	p.agentWriteMu.Unlock()
	p.finishSuccessfulInput(commits)
	return len(b), nil
}

// observeSuccessfulInputLocked is shared by ordinary input and conditional
// submission. Caller holds agentWriteMu. It never invokes callbacks or Manager.
//
// menu says whether a choice menu was on screen when this input was sent;
// recorded bytes typed into one go to the recorder's menu segment instead of
// the composer draft (see menuInputState).
func (p *Pane) observeSuccessfulInputLocked(b []byte, recordPrompt bool, menu menuInput) []string {
	// Before stamping lastRecordedInputMs: entering a menu saves the draft's
	// own age, which a menu keystroke must not renew.
	if recordPrompt {
		switch {
		case menu == menuInputLive:
			p.promptRec.enterMenu()
		case menu == menuInputDrawn:
			// Still answering the menu already entered (or composer text that
			// merely looks like one, which never entered it): no change.
		case !isTerminalReplyOnly(b):
			p.promptRec.leaveMenu()
		}
	}
	// Snapshot cumulative stripped-char count AT THIS INPUT. Used by
	// computeState to decide whether a prompt-like pattern in
	// scrollback is current or already answered. Store after the
	// frame write so a failed write doesn't shift the baseline. This
	// happens for every input, recorded or not — an auto-approval still
	// answered the prompt. NOT for autonomous terminal protocol traffic
	// (focus reports, DSR/DA replies, mouse wheel — isTerminalReplyOnly):
	// an attached client glancing at the pane is not the user answering
	// it, and counting it both re-armed the awaiting-input notifier
	// forever and marked real approval menus as answered.
	if !isTerminalReplyOnly(b) {
		p.inputRevision.Add(1)
		inputAtMs := time.Now().UnixMilli()
		inputPos := atomic.LoadInt64(&p.TotalStrippedChars)
		// Grid admission may place the prompt just beyond the current output.
		// A successful answer is later than that admission even if the TUI
		// has emitted no additional bytes. Otherwise input <= ack keeps the
		// answered modal live and consumes the next identical modal's edge.
		if acked := atomic.LoadInt64(&p.LastAckedChoicePromptPos); acked > 0 && inputPos <= acked {
			inputPos = acked + 1
		}
		atomic.StoreInt64(&p.LastInputStrippedPos, inputPos)
		atomic.StoreInt64(&p.LastInputMs, inputAtMs)
		if recordPrompt {
			p.promptRec.lastRecordedInputMs = inputAtMs
		}
	}
	// Prompt history — parse this write for committed input lines.
	// Recorded after a successful frame write only, so bytes that
	// never reached the PTY can't appear in the strip. Skipped (and the
	// bytes kept out of the recorder buffer) for menu-choice replies.
	var commits []string
	if recordPrompt {
		commits = p.promptRec.observe(b)
	}
	if p.Kind != "native" && len(commits) > 0 {
		// Track every terminal submission, not only writes that happen after an
		// explicit restart request. That closes the otherwise unobservable race
		// where a provider accepts a prompt immediately before the coordinator
		// arms, but has not emitted its first output byte when preflight samples
		// terminal state.
		p.fleetTurnOutputWatermark.Store(atomic.LoadInt64(&p.TotalStrippedChars))
		p.fleetTurnAdmitted.Store(true)
	}
	return commits
}

// finishSuccessfulInput must run outside agentWriteMu, p.mu AND any Manager
// membership lease: onPromptsCommit can itself acquire withCurrentPane.
func (p *Pane) finishSuccessfulInput(commits []string) {
	if p.recordCommittedPrompts(commits...) {
		// A committed user prompt answers any outstanding switch-interrupted
		// notice (the user took the "Continue"), so clear it. This covers the
		// terminal pane path, whose only "user submitted input" signal is a
		// committed prompt; native panes also clear explicitly in paneNativeInput.
		p.clearSwitchNotice()
	}
}

// recordCommittedPrompts updates the user-facing prompt history independently
// of transport framing. Native agents receive JSON-RPC/stream-json envelopes;
// callers write those with WriteNoPromptRecord and record only the original
// user text here so protocol bytes never leak into LastPrompts.
func (p *Pane) recordCommittedPrompts(commits ...string) bool {
	filtered := make([]string, 0, len(commits))
	for _, commit := range commits {
		commit = collapseSpaces(commit)
		if n := utf8.RuneCountInString(commit); n >= promptMinRunes && n <= promptMaxRunes && !isMenuChoiceNoise(commit) {
			filtered = append(filtered, commit)
		}
	}
	if len(filtered) == 0 {
		return false
	}
	p.mu.Lock()
	for _, commit := range filtered {
		p.LastPrompts = pushPrompt(p.LastPrompts, commit)
	}
	p.mu.Unlock()
	if p.onPromptsCommit != nil {
		p.onPromptsCommit(p)
	}
	return true
}

// Rename updates the pane's user-facing title. The title is just a label;
// it doesn't affect the running process. An empty or whitespace-only title
// falls back to the command name so the UI always has something to show.
func (p *Pane) Rename(title string) {
	title = strings.TrimSpace(title)
	p.mu.Lock()
	if title == "" && len(p.Cmd) > 0 {
		title = p.Cmd[0]
	}
	p.Title = title
	p.mu.Unlock()
}

// flagMemoMaxRunes caps the flag memo. Counted in RUNES, not bytes — the old
// 200-byte slice could split a UTF-8 sequence mid-rune. Raised to 500 so an
// agent-written brief (what/why/recommendation) fits; the UI truncates the
// visual line itself.
const flagMemoMaxRunes = 500

// SetFlag toggles the pane's "come back to this later" marker and stores an
// optional memo, with "user" provenance — the UI/HTTP path. Clearing the flag
// also clears the memo and owner so a future re-flag starts fresh.
func (p *Pane) SetFlag(flagged bool, memo string) {
	p.SetFlagAs(flagged, memo, "user")
}

// SetFlagAs is SetFlag with explicit provenance ("user", "agent", or a scoped
// MCP identity like "warden"). Provenance is metadata only at this layer —
// the ownership GUARD lives in the callers that need it (mcpToolSetPaneFlag
// for scoped callers); the user's own paths stay unrestricted.
func (p *Pane) SetFlagAs(flagged bool, memo, owner string) {
	p.flagMu.Lock()
	defer p.flagMu.Unlock()
	p.setFlagLocked(flagged, memo, owner, "")
}

func newFlagRevision() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("flag revision: " + err.Error())
	}
	return hex.EncodeToString(raw[:])
}

// setFlagLocked requires flagMu. A fresh revision means even a same-owner
// replacement cannot be mistaken for the flag a cleanup observed earlier.
func (p *Pane) setFlagLocked(flagged bool, memo, owner, episode string) {
	memo = strings.TrimSpace(memo)
	if r := []rune(memo); len(r) > flagMemoMaxRunes {
		memo = string(r[:flagMemoMaxRunes])
	}
	p.mu.Lock()
	p.Flagged = flagged
	if flagged {
		p.FlagMemo = memo
		p.FlagOwner = owner
		p.FlagEpisode = episode
		p.FlagRevision = ""
		if episode != "" {
			p.FlagRevision = newFlagRevision()
		}
	} else {
		p.FlagMemo = ""
		p.FlagOwner = ""
		p.FlagEpisode = ""
		p.FlagRevision = ""
	}
	p.mu.Unlock()
}

// changeFlag keeps eligibility, mutation, persistence, and rollback in one
// per-pane transaction. No later manual write can be lost to a failed save.
// Live callers must enter through Manager.changeCurrentFlag, which also fences
// same-ID replacement/removal while this object is persisted.
func (p *Pane) changeFlag(store paneStore, check func(PaneSnapshot) error, flagged bool, memo, owner, episode string) error {
	p.flagMu.Lock()
	defer p.flagMu.Unlock()
	prev := p.Snapshot()
	if check != nil {
		if err := check(prev); err != nil {
			return err
		}
	}
	p.setFlagLocked(flagged, memo, owner, episode)
	if store != nil {
		if err := store.Put(p); err != nil {
			p.mu.Lock()
			p.Flagged, p.FlagMemo, p.FlagOwner = prev.Flagged, prev.FlagMemo, prev.FlagOwner
			p.FlagEpisode, p.FlagRevision = prev.FlagEpisode, prev.FlagRevision
			p.mu.Unlock()
			return err
		}
	}
	return nil
}

// changeCurrentFlag holds membership stable until persistence completes. A
// caller may have captured p before a respawn or deletion; persisting that old
// object would otherwise overwrite or resurrect the current durable record.
// Lock order is manager -> flag -> pane/store. Checks must not reenter Manager.
func (m *Manager) changeCurrentFlag(p *Pane, store paneStore, check func(PaneSnapshot) error, flagged bool, memo, owner, episode string) error {
	var err error
	if !m.withCurrentPane(p, func() {
		err = p.changeFlag(store, check, flagged, memo, owner, episode)
	}) {
		return errWardenFlagChanged
	}
	return err
}

// setClaudeSessionIDReportingPrior sets the session id and reports both whether
// it changed AND the prior non-empty id (if any). The caller uses prior to
// distinguish the FIRST id learned for this pane (prior=="" — a restore/resume
// continuing the same conversation, keep the allowlist) from a session ROTATION
// to a genuinely different id (prior!="" and changed — a brand-new conversation,
// clear the allowlist). Empty input is a no-op (changed=false).
func (p *Pane) setClaudeSessionIDReportingPrior(sessionID string) (changed bool, prior string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	prior = p.ClaudeSessionID
	if prior == sessionID {
		return false, prior
	}
	p.ClaudeSessionID = sessionID
	return true, prior
}

func (p *Pane) SetClaudeSessionID(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ClaudeSessionID == sessionID {
		return false
	}
	p.ClaudeSessionID = sessionID
	return true
}

// SetCodexThreadID records the durable app-server thread id for a Codex pane.
// It intentionally does not share ClaudeSessionID: Claude-only transcript,
// duplicate-owner, and resume-repair logic must never run against Codex state.
func (p *Pane) SetCodexThreadID(threadID string) bool {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.CodexThreadID == threadID {
		return false
	}
	p.CodexThreadID = threadID
	return true
}

// SetMuseSessionID mirrors SetCodexThreadID for Muse panes: trims, ignores
// empty, and reports whether the durable session id actually changed.
func (p *Pane) SetMuseSessionID(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.MuseSessionID == sessionID {
		return false
	}
	p.MuseSessionID = sessionID
	return true
}

// replaceStaleForegroundRecipeLocked discards a different provider's obsolete
// argv after authoritative foreground discovery. Keeping a bare current-provider
// recipe prevents later identity healing from reviving the old conversation or
// flags while the new provider has not supplied a durable identity yet. Matching
// provider and generic shell recipes remain unchanged. Caller holds p.mu.
func (p *Pane) replaceStaleForegroundRecipeLocked(agent string) bool {
	if prior := agentFromCmd(p.Cmd); prior != "" && prior != agent {
		p.Cmd = []string{agent}
		return true
	}
	return false
}

// SetForegroundCodexIdentity records the identity of the live foreground Codex
// process and marks the pane as Codex in the same critical section. Shell panes
// historically have an empty Agent (the Claude-compatible zero value), so
// writing only CodexThreadID would leave switch/resume code on the wrong
// provider. A new foreground process is stronger evidence than a persisted old
// thread and therefore replaces it. Empty threadID blocks the prior durable
// identity without erasing it while a fresh/ambiguous Codex is active.
func (p *Pane) SetForegroundCodexIdentity(threadID string) bool {
	threadID = strings.TrimSpace(threadID)
	p.mu.Lock()
	defer p.mu.Unlock()
	recipeChanged := p.replaceStaleForegroundRecipeLocked(agentCodex)
	if threadID == "" {
		wasBlocked := p.codexIdentityBlocked.Swap(true)
		if p.Agent == agentCodex && wasBlocked {
			return recipeChanged
		}
		p.Agent = agentCodex
		return true
	}
	p.codexIdentityBlocked.Store(false)
	if p.Agent == agentCodex && p.CodexThreadID == threadID {
		return recipeChanged
	}
	p.Agent = agentCodex
	p.CodexThreadID = threadID
	return true
}

// SetForegroundFreshCodexIdentity records a fully inspected foreground Codex
// with no open rollout and no resume argv. Clearing an older durable id is safe
// here: this process is explicitly a new conversation, so carrying the old id
// into Focus would be the data-loss bug.
func (p *Pane) SetForegroundFreshCodexIdentity() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	recipeChanged := p.replaceStaleForegroundRecipeLocked(agentCodex)
	wasBlocked := p.codexIdentityBlocked.Swap(false)
	if p.Agent == agentCodex && p.CodexThreadID == "" && !wasBlocked {
		return recipeChanged
	}
	p.Agent = agentCodex
	p.CodexThreadID = ""
	return true
}

// SetForegroundResumingCodexIdentity accepts the short interval after a shell
// launches `codex resume <id>` but before Codex opens the rollout. The caller
// has already proved the argv/process ownership and the explicit id; the
// durable rollout guard remains in the actual resume/restart path.
func (p *Pane) SetForegroundResumingCodexIdentity(threadID string) bool {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	recipeChanged := p.replaceStaleForegroundRecipeLocked(agentCodex)
	wasBlocked := p.codexIdentityBlocked.Swap(false)
	if p.Agent == agentCodex && p.CodexThreadID == threadID && !wasBlocked {
		return recipeChanged
	}
	p.Agent = agentCodex
	p.CodexThreadID = threadID
	return true
}

// SetForegroundClaudeIdentity hands a reused shell pane back to Claude and
// removes stale Codex continuity while retaining historical Muse identity.
// Claude keeps the legacy empty Agent encoding; ClaudeSessionID remains in its
// separate persistence domain.
func (p *Pane) SetForegroundClaudeIdentity() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	recipeChanged := p.replaceStaleForegroundRecipeLocked(agentClaude)
	wasBlocked := p.codexIdentityBlocked.Swap(false)
	fromOther := p.Agent == agentCodex || p.Agent == agentMuse || p.CodexThreadID != "" || wasBlocked
	if !fromOther && !recipeChanged {
		return false
	}
	p.Agent = ""
	p.CodexThreadID = ""
	if fromOther {
		// The hidden Claude identity may precede the other provider in this reused
		// shell. The foreground hook repopulates its current ID/path. Repairing only
		// an old launch recipe must preserve the current Claude conversation.
		p.ClaudeSessionID = ""
		p.ClaudeTranscriptPath = ""
	}
	return true
}

func (p *Pane) setCodexIdentityBlocked(blocked bool) bool {
	return p.codexIdentityBlocked.Swap(blocked) != blocked
}

// SetClaudeTranscriptPath records the absolute path of the pane's MAIN
// conversation transcript, captured from a main-session hook's transcript_path.
// Returns true if it changed (so the caller persists). Empty input is a no-op:
// callers gate on a main-transcript shape before calling, so we never store a
// subagent/workflow path here.
func (p *Pane) SetClaudeTranscriptPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ClaudeTranscriptPath == path {
		return false
	}
	p.ClaudeTranscriptPath = path
	return true
}

// SetTitleOverride records the per-pane user title that wins over the ai-title
// in the Activity header. The handler passes the ALREADY-TRIMMED value: a
// non-empty string sets the override, an empty string CLEARS it (falling back
// to ai-title). Returns true if it changed (so the caller persists). Mirrors
// SetClaudeTranscriptPath, except empty is a legitimate clear here rather than
// a no-op.
func (p *Pane) SetTitleOverride(s string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.TitleOverride == s {
		return false
	}
	p.TitleOverride = s
	return true
}

// HasOwner reports whether any sub currently owns this pane.
// Used by the HTTP resize endpoint to refuse external resizes
// while a client is actively claiming the pane dims.
func (p *Pane) HasOwner() bool {
	p.subsMu.RLock()
	defer p.subsMu.RUnlock()
	_, ok := p.subs[p.ownerSubID]
	return ok
}

// Kill asks the agent to terminate the child and then shut down.
func (p *Pane) Kill() error {
	p.mu.Lock()
	// Mark this as an explicit user-intended teardown BEFORE signaling, so
	// the readLoop's markExited (which fires once the child dies) does not
	// flag the pane Resumable. Every permanent-kill path (delete, switch-mode,
	// migrate, rollback, ARIA) routes through here.
	p.userClosing = true
	conn := p.agentConn
	scope := cloneValuePtr(p.processScope)
	p.mu.Unlock()
	if conn != nil {
		p.agentWriteMu.Lock()
		_ = WriteFrame(conn, FrameKillRequest, nil)
		p.agentWriteMu.Unlock()
	}
	// Give the agent a moment to wind down; if it doesn't, force-kill.
	select {
	case <-p.done:
		if scope != nil {
			_, err := stopProcessScopeIdentity(*scope)
			return err
		}
		return nil
	case <-time.After(3 * time.Second):
	}
	if scope != nil {
		_, err := stopProcessScopeIdentity(*scope)
		return err
	}
	if p.Pid > 0 {
		return killLegacyPaneAgent(p.Pid, p.SocketPath)
	}
	return nil
}

// ChildPids returns agent pid + all its descendants, used by the port
// registry to tag TCP listeners with the owning pane.
func (p *Pane) ChildPids() []int {
	if p.Pid <= 0 {
		return nil
	}
	return descendantPids(p.Pid)
}

// HasClaudeDescendant returns true if any process in this pane's process
// tree is running `claude`. Used so the UI can treat a shell-with-claude
// the same as a direct-claude pane for state-aware features — without the
// user having to launch Claude as the top-level command.
func (p *Pane) HasClaudeDescendant() bool {
	if p.Pid <= 0 {
		return false
	}
	for _, pid := range descendantPids(p.Pid) {
		if pid == p.Pid {
			continue // skip the agent itself
		}
		if readComm(pid) == "claude" {
			return true
		}
	}
	return false
}

// HasCodexDescendant returns true if any process in this pane's process
// tree is running `codex`. Used by Pane.Resize to decide whether to
// inject a clear on a large-delta resize: codex on Linux doesn't enter
// alt-screen (it uses sync-update + DECSTBM in the normal buffer), so
// the existing altScreen-gated clear doesn't fire and ghost cells
// linger after a discontinuous resize like a browser-close. Walking
// procfs is cheap and only happens once per "large enough" resize.
func (p *Pane) HasCodexDescendant() bool {
	if p.forceCodexForTest {
		return true
	}
	if p.Pid <= 0 {
		return false
	}
	for _, pid := range descendantPids(p.Pid) {
		if pid == p.Pid {
			continue
		}
		if readComm(pid) == "codex" {
			return true
		}
	}
	return false
}

// HasMuseDescendant returns true if any process in this pane's process
// tree is running `muse` (Muse Spark). Mirrors HasCodexDescendant for
// the same TUI-overlay / resize-repaint signals; the daemon treats a
// shell-with-muse the same as a direct-muse pane for state-aware UI.
func (p *Pane) HasMuseDescendant() bool {
	if p.Pid <= 0 {
		return false
	}
	for _, pid := range descendantPids(p.Pid) {
		if pid == p.Pid {
			continue
		}
		// comm covers the running binary; the argv fallback covers the
		// launcher bash phase and wrappers (see isMuseCmdline).
		if isMuseProcessName(readComm(pid)) || isMuseCmdline(readHarnessCmdline(pid)) {
			return true
		}
	}
	return false
}

// HasPiDescendant reports whether this pane's process tree is running a
// SoluTerminal-managed Pi session. A managed launch is the only one that
// passes `--extension .../pi-approval.mjs` (launch.mjs piPlanFor), so the
// extension path on the child argv is the precise marker: a user's own
// unmanaged `pi` never matches, even though the runtime binary is the same.
// Used to surface the per-pane auto-approval chip for Pi panes, whose
// approval menu would otherwise be unrecognizable as a "harness" pane.
func (p *Pane) HasPiDescendant() bool {
	if p.Pid <= 0 {
		return false
	}
	for _, pid := range descendantPids(p.Pid) {
		if pid == p.Pid {
			continue
		}
		if isManagedPiCmdline(readHarnessCmdline(pid)) {
			return true
		}
	}
	return false
}

// isManagedPiCmdline recognizes the managed Pi child argv. Kept pure so the
// marker is pinned by a unit test rather than only by a live process tree.
func isManagedPiCmdline(cmd string) bool {
	return strings.Contains(cmd, "pi-approval.mjs")
}

// HasDeepseekDescendant reports whether this pane's process tree is running the
// SoluTerminal DeepSeek terminal client. A managed launch always runs
// `node .../deepseek-terminal.mjs <dsh-runtime> <model>` (launch.mjs
// deepseekPlan), so the client filename is the precise marker: a bare DSH
// runtime or a user's own wrapper never matches. Used to surface the per-pane
// auto-approval chip for DeepSeek panes, whose focused approval menu the daemon
// already scrapes but whose process tree looked like a plain shell.
func (p *Pane) HasDeepseekDescendant() bool {
	if p.Pid <= 0 {
		return false
	}
	for _, pid := range descendantPids(p.Pid) {
		if pid == p.Pid {
			continue
		}
		if isManagedDeepseekCmdline(readHarnessCmdline(pid)) {
			return true
		}
	}
	return false
}

// isManagedDeepseekCmdline recognizes the DeepSeek terminal-client argv. Kept
// pure so the marker is pinned by a unit test rather than only by a live
// process tree.
func isManagedDeepseekCmdline(cmd string) bool {
	return strings.Contains(cmd, "deepseek-terminal.mjs")
}

// HasManagedHarnessDescendant reports whether a managed terminal harness client
// is live in this pane's tree (DeepSeek today, Pi next). A restart must not
// fall back to the pane's stale Claude/Codex id while the harness is running
// but has not yet recorded its sidecar session, so eligibility fails closed on
// this signal.
func (p *Pane) HasManagedHarnessDescendant() bool {
	return p.HasDeepseekDescendant() || p.HasPiDescendant()
}

// SetOnPaneRemoved sets the callback Kill runs once a pane is gone.
func (m *Manager) SetOnPaneRemoved(f func(id string)) {
	m.mu.Lock()
	m.onPaneRemoved = f
	m.mu.Unlock()
}

func (m *Manager) Kill(id string) error {
	p, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("pane not found: %s", id)
	}
	if err := p.Kill(); err != nil {
		return err
	}
	<-p.done
	m.mu.Lock()
	delete(m.panes, id)
	removed := m.onPaneRemoved
	m.mu.Unlock()
	if removed != nil {
		removed(id)
	}
	// Release any subscribers still parked on this object. For a LIVE pane,
	// markExited already closed them when the process died; but an EXITED or
	// SUSPENDED pane accepts subscribers AFTER markExited (the client showing
	// scrollback + the Resume overlay), and a SuspendExisting pane never had a
	// process at all — killing those (the respawn-in-place path: resume after
	// a reboot, live-exit resume) used to leave the client's WS open on the
	// orphaned object while the fresh `claude --resume` painted its TUI to the
	// SUCCESSOR pane's empty sub map. Result: a blank pane until the next
	// keystroke's write error tore the zombie socket down (feedback #18).
	// Detaching here closes those channels so the client reconnects and
	// resolves the successor (or a 404 that the pid-keyed respawn-reconnect
	// backstop mops up).
	p.detachSubscribers()
	return nil
}
