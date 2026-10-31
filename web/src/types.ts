export type PaneStatus = 'running' | 'exited';

export type RecoveryResumeMode = 'manual' | 'auto' | 'paused';

export interface RecoverySettings {
  resumeAfterRestart: RecoveryResumeMode;
}

// Per-install self-update preference (GET/PUT /api/update/settings). 'off' keeps
// the notify-then-click banner; 'when-idle' auto-applies the daemon swap the
// moment it's safe (no native pane mid-turn). Daemon-stored — see
// daemon/update_settings.go.
export type UpdateAutoApplyMode = 'off' | 'when-idle';

export interface UpdateSettings {
  autoApply: UpdateAutoApplyMode;
  // Optional because older daemons omit it; clients then retain the manual flow.
  shellAutoApply?: UpdateAutoApplyMode;
}

export interface ShellAutoRestart {
  version: string;
  deadlineMs: number;
}

// Per-machine native (Focus-renderer) toggles. promptSuggestions is the opt-in
// for Claude's own "next-prompt" suggestions (ghost text in the composer);
// default OFF — enabling it adds one cached background model call per turn.
export interface NativeSettings {
  promptSuggestions: boolean;
  // The renderer NEW Claude sessions preselect in the new-session dialog:
  // '' (terminal, the shipping default) or 'native' (Focus). A client-default
  // hint only — the per-session choice always wins. Optional so older daemons
  // (which won't echo the field) keep working.
  defaultClaudeMode?: '' | 'native';
}

// The Warden — an opt-in, event-driven fleet caretaker (GET/PUT
// /api/warden/settings). Default OFF: nothing spawns until `enabled`. See
// daemon/warden_settings.go. model '' = the account default; triageAfterMin
// and maxRunsPerHour are clamped daemon-side (2–120, 1–30).

export interface WardenSettings {
  enabled: boolean;
  autoFlags?: boolean;
  model?: string;
  triageAfterMin?: number;
  maxRunsPerHour?: number;
}

// One finished Warden run, from GET /api/warden/status. Isolated from the
// schedules/reports rail on purpose (see PLAN-warden). status: ok|timeout|error.
export interface WardenRun {
  kind: 'brief' | 'debrief' | 'triage';
  paneId?: string;
  paneTitle?: string;
  at: string;
  status: string;
  detail?: string;
  summary?: string;
  logFile?: string;
}

export interface WardenStatus {
  enabled: boolean;
  runs: WardenRun[];
}


export type PaneRuntimeState =
  | 'busy'
  | 'awaiting-input'
  | 'choice-prompt'
  | 'recent'
  | 'idle'
  | 'exited';

export type ContextPackStatus = 'injected' | 'cwd' | 'unsupported' | 'missing';

/** How a pane came to exist (daemon pane_origin.go): who or what started it,
 *  which lead it works for, and what it was started for. Informational only;
 *  absent on panes that predate it. */
/** A pane's handle (addressable as @handle, unique among panes) and its
 *  current one-line status (daemon/pane_identity.go). */
export interface PaneIdentity {
  handle?: string;
  workingOn?: string;
  workingOnAt?: number;
  /** "user", or the label of the agent pane that set it ("#2 Claude"). */
  workingOnBy?: string;
}

export interface PaneOrigin {
  source: 'user' | 'agent' | 'schedule' | 'api';
  byPaneId?: string;
  byPaneTitle?: string;
  byAgent?: string;
  leadPaneId?: string;
  leadPaneTitle?: string;
  /** What the pane was started for, as given at start (historical). */
  purpose?: string;
  workspace?: string;
  at: number;
}

/** The latest independent review (scripts/solu-review) of the work in a pane,
 *  as that script reported it (daemon pane_review.go). Informational. */
export interface PaneReview {
  verdict: 'approve' | 'request-changes' | 'comment' | 'error';
  reviewer: string;
  model?: string;
  author?: string;
  fallback?: string;
  round: number;
  mustFix: number;
  notes: number;
  title?: string;
  branch?: string;
  head?: string;
  summary?: string;
  findings?: { severity: string; where?: string; what: string; blocking: boolean }[];
  questions?: string[];
  error?: string;
  at: number;
  /** The author's note that the must-fix items were fixed after this review
   *  (`scripts/solu-review --fixed`); not a re-review. */
  followup?: { note: string; head?: string; at: number };
}

export interface PaneInfo {
  unsentMessage?: { id: string; sender: string };
	// Current live-auth evidence, distinct from selection captured at launch.
	codexAuth?: {
		state: string; account?: string; canReconnect?: boolean; recovery?: string; retryAt?: number;
		// Why a pane without the live bridge cannot reconnect in place.
		blocker?: 'team' | 'disabled' | 'custom-launch';
		// 'shell': reconnecting replaces this shell pane's prompt with the resumed Codex.
		reconnectMode?: 'shell';
		reconnectArmed?: boolean;
		reconnectError?: string;
	};
  recoveryError?: string;
  // Client placeholder for a saved pane absent from the daemon.
  restorePending?: boolean;
  credential?: {
    provider: string;
    account?: string;
    source: 'launch' | 'broker';
    activeAccount?: string;
    restartRequired?: boolean;
    liveState?: string;
    recovery?: string;
    retryAt?: number;
    teamId?: string;
    activeTeamId?: string;
  };
  titleOverride?: string;
  socketPath?: string;
  id: string;
  cwd: string;
  cmd: string[];
  title: string;
  status: PaneStatus;
  createdAt: string;
  exitCode?: number;
  // True when this pane's Claude session exited on its own (not an explicit
  // kill) and the daemon retained the session id — a POST /api/panes/{id}/resume
  // will respawn `claude --resume <claudeSessionId>` on the SAME pane id. Pairs
  // with status:'exited'. Absent/false against an older daemon → no Resume
  // affordance (falls back to the plain "session ended" dead-end).
  resumable?: boolean;
  // Set with resumable when the daemon suspended this pane across a restart
  // while Settings › Terminal & panes › Sessions › After a restart is "Don't resume": the pane shows
  // as paused with no offer to resume; the pane menu's Resume session still
  // resumes it.
  restartPaused?: boolean;
  cols: number;
  rows: number;
  // True while the daemon's cols/rows are the persisted copy from before a
  // daemon restart rather than the live PTY size (old agent that can't
  // report its dims, no client resize applied yet). Never seed a resize
  // from unverified dims — echoing them back resized real PTYs to
  // weeks-stale widths after every auto-update and garbled every pane.
  dimsUnverified?: boolean;
  pid: number;
  lastActivityMs: number;
  state: PaneRuntimeState;
  // True when a `claude` process is live anywhere in this pane's process
  // tree — even if the top-level command was `bash` and Claude was
  // launched from inside. Drives Claude-aware UI behaviors.
  hasClaude: boolean;
  // True when a `codex` process is live anywhere in this pane's process
  // tree. Used for Codex-specific resize recovery; Codex renders in the
  // normal buffer and can leave stale cells after large layout jumps.
  hasCodex?: boolean;
  // True when a `muse` (Muse Spark) process is live anywhere in this
  // pane's process tree. Mirrors hasCodex for TUI-overlay / auto-approve
  // gating so a shell-with-muse pane is treated as a Muse pane.
  hasMuse?: boolean;
  // True when this pane runs a SoluTerminal-managed Pi session (its approval
  // extension is on the child argv). Pi's own TUI has no permission system;
  // the managed extension supplies the numbered approval menu, so Pi panes
  // join Claude/Codex/Muse in the auto-approval surfaces.
  hasPi?: boolean;
  // True when this pane runs the SoluTerminal DeepSeek terminal client (the
  // client filename is on the child argv). That client renders the focused
  // numbered approval menu the daemon auto-answers, so DeepSeek panes join
  // Claude/Codex/Muse/Pi in the auto-approval surfaces.
  hasDeepseek?: boolean;
  // True when the pane's running `claude` inherited a nested child-session
  // env (the daemon was launched from inside a claude session) and so never
  // writes its own transcript — the conversation/Activity view stays empty
  // until the pane is restarted/migrated. Server omits it unless conclusively
  // true (Linux-only detection). Drives the degraded banner + Fix flow.
  degraded?: boolean;
  degradedReason?: string;
  // Current working directory of the shell inside the pane — reflects
  // the user's `cd`s. Populated server-side via /proc/<shellpid>/cwd on
  // every /api/panes poll. Prefer this over `cwd` (which is the
  // spawn-time value) anywhere the user expects "where am I now".
  // Optional: missing if the descendant walk couldn't find a live shell
  // (e.g. pane exited).
  liveCwd?: string;
  // Which backend this pane lives on. Populated in dual-backend router
  // mode so the sidebar can aggregate pane state across WSL and Windows
  // simultaneously; undefined in single-backend mode.
  backend?: 'wsl' | 'win';
  // Recent submitted-input history, newest first, recorded daemon-side
  // at the PTY write choke point (daemon/prompt_recorder.go) so every
  // transport and every client sees the same strip. Survives daemon
  // restarts via panes.json.
  lastPrompts?: string[];
  // ARIA auto-approval policy for this pane. Empty / undefined = off
  // (manual approval). 'always' = ARIA auto-approves every prompt
  // without a cancel countdown. 'cautious' = ARIA only approves
  // prompts it judges low-risk; otherwise it leaves the question for
  // the user. Per-hour cap + global pause still apply to both.
  autoMode?: '' | 'always' | 'cautious';
  // Daemon-computed "who needs to act" signal (daemon/attention.go). This is
  // what every alert surface keys on — NOT the raw `state`:
  //   'none'         — nothing is waiting on anyone.
  //   'auto-pending' — a prompt is open but auto-approval is expected to
  //                    handle it; show quietly (⚡), never alert.
  //   'needs-user'   — a human is required; the ONLY value that should
  //                    toast, speak, or light the attention badge.
  // undefined = older daemon (or the engine's first ~2s) → fall back to
  // state-based alerting so nothing is silently swallowed.
  attention?: 'none' | 'auto-pending' | 'needs-user';
  // Current hold reason for auto-pending, or explanation when needs-user has one ("AskUserQuestion needs
  // your answer…", "won't auto-approve — touches secrets", a decline reason).
  attentionReason?: string;
  // LLM-graded attention item for the CURRENT needs-user prompt — a one-line
  // summary, a low|medium|high risk grade, and a suggested next step
  // (daemon/aria_attention_summary.go). Undefined until the async summary
  // lands; consumers fall back to the heuristic tail preview.
  attentionSummary?: string;
  attentionRisk?: 'low' | 'medium' | 'high' | '';
  attentionAction?: string;
  // The pane's SINGLE open native tool-permission (daemon approvalHint):
  // present only for a native pane with exactly one open approval, so
  // notifications can say WHAT is being asked ("Bash: git push …") without
  // a history fetch. Absent for terminal / zero / parallel approvals.
  pendingApproval?: { requestId: string; toolName: string; summary?: string };
  // Reasoning-effort level baked into a native pane's `claude --effort` argv.
  // Surfaced here so the Focus header can show/seed the current level. '' or
  // absent = claude's built-in default. Only meaningful for native panes.
  effort?: '' | 'low' | 'medium' | 'high' | 'xhigh' | 'max';
  // Explicit Claude model id baked into a native pane's `claude --model` argv.
  // '' or absent = no flag — the session runs its STORED model on resume (not
  // necessarily the settings default). The picker ranks an in-flight choice,
  // then this durable flag, then session_init so a replayed pre-respawn init
  // cannot mark the old model current after /model.
  model?: string;
  // Daemon-persisted product role. 'orchestrator' = a chief-of-staff Claude or
  // Codex pane spawned with the delegation brief (daemon/pane_role.go); absent for a
  // plain pane. Drives the header role chip and is round-tripped into workspace
  // defs so a restore doesn't silently demote it.
  role?: PaneRole;
  // Local context-pack path and the adapter outcome from the last spawn.
  // The daemon exposes the path only; briefing content never rides this DTO.
  contextPack?: string;
  /** Who or what started this pane and why. */
  origin?: PaneOrigin;
  /** Latest solu-review result for this pane's work. */
  review?: PaneReview;
  /** The pane's @handle and what it is working on now. */
  identity?: PaneIdentity;
  /** The last message another agent pane delivered here (recent activity;
   *  never ownership). */
  lastAgentMessage?: { fromPaneId: string; fromLabel: string; at: number; outcome: string };
  contextPackStatus?: ContextPackStatus;
  // The user's EXPLICIT choice about the "[solu] delegated work" status line
  // soluterminal types into this pane's Claude session when work it delegated
  // moves (daemon/delegation_nudge.go). Absent = follow the role default (on
  // for an orchestrator pane, off otherwise).
  announceDelegationEvents?: boolean;
  // The daemon-computed EFFECTIVE value of the above — the override resolved
  // against the role default. This is what the toggle renders; never derive it
  // client-side, or the menu and the daemon can disagree.
  announceDelegation?: boolean;
  // LLM backend this pane's claude routes to ('glm' = Z.ai, ''/'anthropic' =
  // Anthropic). Sent by the daemon DTO; surfaced so workspace autosave can
  // round-trip it back into the pane def (otherwise a def-restore downgrades).
  provider?: 'anthropic' | 'glm';
  // Server-side timestamp (Unix millis) of the most recent successful
  // ARIA auto-mode fire on this pane. Drives the "fired Xs ago" label
  // on the auto-mode strip. In-memory on the daemon — resets to 0 on
  // daemon restart. Absent when the pane has never auto-fired.
  autoModeLastFiredMs?: number;
  // Consecutive no-action ARIA auto-evals (model declined OR errored).
  // The recovery sweeper suspends a pane once this hits 3, so the UI
  // can render a "backed off" badge to explain why an auto-mode pane
  // stopped responding. 0 means healthy / never tracked.
  ariaBackoffStreak?: number;
  // User-set "come back to this later" marker. When true the pane
  // renders an orange highlight and counts toward the sidebar's
  // flagged-pane badge so the user can find pending threads without
  // re-entering each workspace. Persisted across daemon restart.
  flagged?: boolean;
  // Optional one-liner attached when flagging ("stuck on auth race");
  // shown on hover and surfaced in the sidebar tooltip.
  flagMemo?: string;
  flagOwner?: string;
  // Claude Code conversation id captured from Claude hook payloads.
  // Used by the daemon to recreate this pane after a host reboot with
  // `claude --resume <id>`.
  claudeSessionId?: string;
  // True only when claudeSessionId's transcript actually exists on disk (daemon
  // checks it with a single os.Stat). The id is captured at SessionStart, before
  // the transcript is written, so gate the header "resume" badge on this — not on
  // claudeSessionId alone, which would show the badge prematurely on a brand-new
  // session you haven't spoken to yet.
  resumeReady?: boolean;
  identityBlocked?: boolean;
  // Build version the pane's agent process reported at attach (e.g. "0.0.1142").
  agentVersion?: string;
  // True only when the pane's agent speaks an older agent protocol than the
  // daemon, i.e. restarting the pane would actually change its behavior. A
  // daemon-only release never sets it. Omitted when false.
  agentStale?: boolean;
  // Renderer kind. Absent / '' = the default terminal (xterm/grid) pane —
  // EVERY existing pane keeps this path byte-identically. 'native' = the
  // native-renderer pane (headless Claude over stream-json, normalized to
  // NativeEvents and rendered as markdown/tool-cards/approvals at the
  // browser's own width). Daemon emits json:"kind,omitempty" (pane.go); the
  // native render path engages purely on `pane.kind === 'native'`. Gated
  // server-side behind SOLU_NATIVE_RENDERER for creation.
  kind?: '' | 'native';
  // Agent protocol, independent from the renderer. Omitted means the legacy
  // Claude default; Codex / Muse are explicit so they survive workspace persistence.
  agent?: 'claude' | 'codex' | 'muse';
  codexThreadId?: string;
  // Durable Muse session UUID (muse resume <id> / muse exec --session-id).
  museSessionId?: string;
  // Set by the daemon when something discontinuous happened to this pane's
  // conversation. reason discriminates the banner copy: absent/'' = a
  // Terminal↔Native mode switch interrupted an in-flight turn (fromKind is the
  // renderer switched AWAY from); 'resume-repair' = a restart cut off the
  // previous response (the resume synthetically answered a dangling tool_use);
  // 'fresh-start' = a requested --resume was dropped (no on-disk transcript /
  // session live in another pane) and the pane started a NEW conversation
  // (interrupted=false — informational, nothing to continue). The daemon
  // clears it (→ null/absent) the moment the next user turn is sent, so the
  // client's banner is one-shot per notice.
  switchNotice?: {
    interrupted: boolean;
    fromKind: 'native' | 'terminal';
    at: number;
    reason?: 'resume-repair' | 'fresh-start';
  } | null;
  // Set by the daemon when a DEFERRED ("switch when the turn finishes") renderer
  // switch is ARMED and waiting for the current turn to settle. `target` is the
  // renderer being switched TO ('native' | 'terminal'). The daemon clears it
  // (→ null/absent) the moment the switch fires OR is cancelled. Distinct from
  // paneModeSwitching (a client-only "flip in flight" flag for the now-switch
  // round-trip): pendingSwitch is an armed-but-not-yet-fired server state.
  pendingSwitch?: { target: 'native' | 'terminal'; at: number } | null;
  // Daemon-computed count of "[solu] delegated work" updates buffered for
  // this pane (held while a turn is in flight or the composer has text;
  // delivered when idle or with the next Focus message).
  delegatedPending?: { count: number; needsYou: number; oldestAt: number } | null;
  // Prompt-aware "switching the renderer now would interrupt live agent work".
  // The daemon computes this from a visible Claude prompt box, so it does NOT
  // misread a terminal pane's ~3s post-turn recency tail (state still reads
  // 'busy'/'recent' from final redraws) as working. The mode-switch confirm gates
  // on this instead of the coarse `state`, so a just-settled pane switches
  // one-click rather than wrongly offering "switch when done". Always sent by a
  // current daemon (false is meaningful); absent only against an older daemon,
  // where shouldConfirmModeSwitch falls back to the recency states.
  agentMidTurn?: boolean;
  // The daemon's per-process id. Changes only on a daemon restart — exactly when
  // a native pane's event ring is rebuilt from seq 1 (AttachExisting) while the
  // surviving agent keeps its pid, so pid-based respawn detection can't see the
  // reset. useNativePane resets + re-seeds its fold cache on a change (same path
  // as a pid respawn) to make a mid-turn daemon swap reattach losslessly. Absent
  // against an older daemon → never triggers a reset (resets only on a
  // defined→defined change).
  nativeEpoch?: number;
  // Last-sampled resident memory (MB) of the pane's selected Claude or Codex
  // process — the heap "Restart agent" reclaims. Refreshed by the daemon's slow
  // RSS sampler (~60s), so it can lag up to a minute. Drives the Focus header
  // memory chip. Absent/0 when unsampled, no matching child, or non-Linux.
  agentRssMB?: number;
  // Compatibility alias emitted by older daemons and during the rollout that
  // introduced agentRssMB. New UI code prefers agentRssMB.
  claudeRssMB?: number;
  // A background Claude Code Workflow still running after the launching turn
  // ended. The Workflow tool returns immediately and notifies on completion, so
  // agentMidTurn settles to idle while its subagents keep churning — the pane
  // would otherwise look done. Present only while a workflow is in flight; the
  // footer renders "workflow · N/M agents · active Xs ago" and infers
  // stalled-vs-working from lastActivityMs (freshest workflow file mtime, unix-ms;
  // 0 = nothing written yet). See daemon/workflow_status.go.
  workflowStatus?: {
    running: boolean;
    agentsTotal: number;
    agentsDone: number;
    lastActivityMs: number;
  } | null;
  // Terminal-mode agent turn tracking (daemon/turns.go): whether a turn is
  // running and since when, plus the last completed turn's change stats.
  // Absent on older daemons and on panes that never ran an agent turn.
  turn?: PaneTurnSummary;
}

// One completed agent turn in a terminal pane. Files/added/removed are the
// repo-wide change between the turn's start and end snapshots; overlapPanes
// names other agent panes that worked in the same repo meanwhile.
export interface TurnRecord {
  source?: string;
  sourceTurnId?: string;
  outcome?: 'completed' | 'interrupted' | 'failed';
  historical?: boolean;
  n: number;
  agent?: string;
  startedMs: number;
  endedMs: number;
  repo?: string;
  branch?: string;
  files: number;
  added: number;
  removed: number;
  overlapPanes?: string[];
  skippedUntracked?: number;
  note?: string;
}

// A past agent conversation offered for resume in the New Pane dialog.
export interface BrowsedSession {
  agent: 'claude' | 'codex';
  id: string;
  title?: string;
  firstPrompt?: string;
  updatedMs: number;
  automated?: boolean;
  cwd: string;
}

export interface PaneTurnSummary {
  uncertain?: boolean;
  historyRevision?: number;
  active: boolean;
  startedMs?: number;
  count: number;
  last?: TurnRecord;
}

export type PaneAutoMode = '' | 'always' | 'cautious';

// PaneEffort is the per-pane reasoning-effort level (claude's --effort).
// Provider-agnostic: drives both Anthropic and GLM panes (GLM maps
// high-and-below → GLM High, xhigh/max → GLM Max). '' = claude's default.
export type PaneEffort = '' | 'low' | 'medium' | 'high' | 'xhigh' | 'max';

// PaneRole is a pane's host-neutral product role. A plain pane omits it.
export type PaneRole = 'orchestrator' | 'task_owner';

export interface OrchestratorHostInfo {
  agent: 'claude' | 'codex' | 'muse';
  label: string;
  supported: boolean;
  installed: boolean;
  available: boolean;
  reason?: string;
}

export interface OrchestratorHostsInfo {
  policyVersion: string;
  hosts: OrchestratorHostInfo[];
}

// Recurring prompt-injection. See daemon/schedules.go for fire
// semantics — in short, the cron expression fires the daemon's
// scheduler which writes `prompt + \r` into the target pane's PTY.
// Skipped (with a reason logged to `runs`) when the pane is busy or
// exited.
export type ScheduleRunStatus = 'fired' | 'skipped' | 'error';

// Permission preset for kind='task' runs. 'plan' reads and plans but writes
// nothing; 'edit' lets Claude change files. The daemon maps these to
// --permission-mode; bypassPermissions is deliberately not offered.
export type SchedulePermission = 'plan' | 'edit';

export interface ScheduleRun {
  at: string;
  status: ScheduleRunStatus;
  detail?: string;
  paneId?: string;   // prompt runs: the pane the prompt was delivered into
  logFile?: string;  // agent/task runs: captured-output file for /api/schedules/{id}/runlog
  summary?: string;  // task runs: the report's opening lines (also the push body)
}

export interface Schedule {
  id: string;
  name: string;
  cron?: string;          // 5-field cron — absent for one-time
  runAt?: string;         // ISO — one-time fire instant (mutually exclusive with cron)
  until?: string;         // ISO — recurring stop time (nil = forever)
  maxFires?: number;      // recurring: stop after N successful fires (0/undefined = unlimited)
  snoozeUntil?: string;   // ISO — one upcoming fire suppressed; cleared on skip
  // Work kind: absent/'prompt' = inject prompt into paneId's PTY;
  // 'agent' = run the built-in local agent `agentId` as a child process
  // (daemon/schedules_agent.go). Agent schedules have no paneId/prompt.
  // 'task' = run a fresh headless Claude session on `prompt` in `cwd`
  // (daemon/schedules_task.go). Task schedules have no paneId.
  kind?: 'prompt' | 'agent' | 'task';
  agentId?: string;
  // Set when this schedule has been imported into a managed agent
  // (docs/DESIGN-agents-surface.md §8.3). Such rows are omitted from
  // GET /api/schedules entirely — the agent card is their surface — so this
  // only ever arrives through the ?includeAgentRuns=1 debugging view.
  ownedByAgentId?: string;
  // Creator provenance stamped by the daemon on MCP create_schedule — WHO
  // filed the schedule. Distinct from `paneId` below, which for prompt-kind
  // schedules is the pane the prompt is delivered TO. Absent for UI-created
  // and legacy schedules.
  createdBy?: { source: string; paneId?: string; paneTitle?: string };
  apply?: boolean;        // agent write-mode (--apply); off = dry-run
  agentRunning?: boolean; // server-computed: a spawned run (agent or task) is in flight
  // kind='task' fields. permission is a preset name, never a raw claude flag.
  cwd?: string;
  permission?: SchedulePermission;
  model?: string;
  timeoutMin?: number;    // 0/absent = the daemon's 30-minute default
  paneId?: string;
  prompt?: string;
  // Phone push on every successful run (errors always push). The dialog
  // defaults this ON for one-time reminders.
  notifyOnRun?: boolean;
  // Task-kind only: post each successful run's report into a chat through an
  // optional chat-delivery feature.
  deliverChat?: ScheduleChatRef;
  // Task-kind recurring only: if runs were missed while the daemon was
  // off, run ONCE shortly after startup (not once per miss).
  catchUp?: boolean;
  enabled: boolean;
  createdAt: string;
  runs?: ScheduleRun[];   // newest first, capped at 20 server-side
  nextFire?: string;      // server-computed; absent when disabled

  // Lifetime counters (survive beyond the 20-run cap).
  fireCount: number;
  skipCount: number;
  errorCount: number;
  lastFiredAt?: string;
  lastReadAt?: string;  // newest run the user has read (reports reader); daemon-synced
  // Why the DAEMON paused it (e.g. "its target pane is gone"); absent when
  // the user paused it. Cleared when it is turned back on or retargeted.
  pausedReason?: string;
}

export interface ScheduleInput {
  name?: string;
  cron?: string;          // one of cron / runAt is required
  runAt?: string;
  until?: string;
  maxFires?: number;
  kind?: 'prompt' | 'agent' | 'task';
  agentId?: string;       // kind=agent only
  apply?: boolean;        // kind=agent only
  paneId?: string;        // kind=prompt only
  prompt?: string;        // kind=prompt | task
  cwd?: string;           // kind=task only
  permission?: SchedulePermission; // kind=task only
  model?: string;         // kind=task only
  timeoutMin?: number;    // kind=task only
  notifyOnRun?: boolean;
  deliverChat?: ScheduleChatRef; // kind=task only
  catchUp?: boolean;      // kind=task recurring only
  enabled?: boolean;
}

// Where a task schedule's reports go, through an optional chat-delivery
// feature (extensions.ts scheduleChatDelivery).
export interface ScheduleChatRef {
  agentId?: string; // '' = the feature's first configured sender
  source: string;   // the chat network
  chatId: string;
  chatName?: string; // display only
}


// One built-in runnable agent (GET /api/schedules/agents). present=false
// (or a 404 from an older daemon) simply hides the agent option.
export interface ScheduleAgent {
  id: string;
  title: string;
  present: boolean;
  running: boolean;
  defaultCron: string;
}

// Internal browser tile — renders an iframe at `url`. State (url, title)
// lives in the store's `browsers` map; the layout tree just references
// by `browserId`. This mirrors how terminal panes work.
export interface BrowserInfo {
  id: string;       // "browser_xxx"
  url: string;
  title?: string;   // last known <title> from the iframe document (when cross-origin-readable)
  createdAt: string;
  service?: BrowserServiceTarget;
}

export interface BrowserServiceTarget {
  url: string;
  previewPath?: string;
  backend?: 'wsl' | 'win';
  machinePrefix: string;
  paneId?: string;
  port?: number;
  addr?: string;
  proto?: string;
}

// File tile — line-level viewer with one-value-at-a-time editing. Only the
// path + id are stored in layout/workspace JSON; the content is fetched
// fresh on open so we never persist potentially-sensitive file contents.
export interface FileTileInfo {
  id: string;       // "file_xxx"
  path: string;
  createdAt: string;
}

// Diff tile — read-only "review what changed" view of a git repo. Unlike
// browser/file tiles it carries its full state (repo path + staged flag)
// INLINE in the layout leaf rather than in a separate store map: a diff has
// no mutable server-tracked state, just a path + a toggle, and the actual
// diff text is always re-fetched on mount. Keeping it inline means the tile
// persists across reloads for free (the layout itself is persisted) with no
// snapshot/restore wiring and no possibility of an orphaned leaf.
export type Layout =
  | { type: 'pane'; paneId: string }
  | { type: 'browser'; browserId: string }
  | { type: 'file'; fileId: string }
  // `turn` switches the tile from the live working/staged diff to one agent
  // turn's recorded change (GET /api/panes/{id}/turns/{n}/diff).
  | { type: 'diff'; diffId: string; path: string; staged: boolean; turn?: { paneId: string; n: number } }
  | { type: 'tasks'; tasksId: string; selectedTaskId: string | null; composing: boolean; ownerTaskId?: string }
  | { type: 'split'; direction: 'horizontal' | 'vertical'; ratio: number; a: Layout; b: Layout };

// Minimal info needed to respawn a pane when loading a workspace whose
// original panes are no longer alive. Optional for backwards compat —
// workspaces saved before v0.2 won't have this and simply won't restore
// panes that have since been killed.
export interface PaneDef {
  identityBlocked?: boolean;
  id: string;          // pane id at save time (may no longer exist)
  cwd: string;
  cmd: string[];
  title: string;
  cols?: number;
  rows?: number;
  flagged?: boolean;
  flagMemo?: string;
  claudeSessionId?: string;
  // Rendering model: 'native' for a Focus pane, undefined for a terminal.
  // Persisted so restore + the cached-first switch's optimistic seed recreate
  // a native pane AS native (not a terminal showing raw JSONL).
  kind?: 'native';
  // Agent protocol. Omitted means legacy/default Claude.
  agent?: 'claude' | 'codex' | 'muse';
  codexThreadId?: string;
  // Durable Muse session UUID (muse resume <id> / muse exec --session-id).
  museSessionId?: string;
  // LLM backend + reasoning effort, persisted so a workspace def-restore (and
  // the client autosave that overwrites workspaces.json) preserves them instead
  // of silently downgrading a GLM/high-effort pane to Anthropic/default.
  provider?: 'anthropic' | 'glm';
  effort?: PaneEffort;
  model?: string;
  // Product role, persisted so a def-restore recreates an orchestrator pane AS
  // orchestrator (with its brief) instead of a plain claude pane.
  role?: PaneRole;
  contextPack?: string;
  /** Name, origin and identity survive a workspace restore. */
  titleOverride?: string;
  origin?: PaneOrigin;
  identity?: PaneIdentity;
}

export interface Workspace {
  name: string;
  /** Layout generation bound to pane reassignment; rejects stale autosaves. */
  paneMoveVersion?: number;
  layout: Layout | null;
  paneIds: string[];
  panes?: PaneDef[];        // inline pane snapshot so workspaces are restorable
  browsers?: BrowserInfo[]; // inline browser snapshot, same idea
  files?: FileTileInfo[];   // inline file-tile snapshot (path + id — contents re-fetched)
  activeAt: string;
  /**
   * Which backend this workspace lives on. Only populated in the
   * Tauri desktop app's dual-backend router mode (Backend::Dual).
   * Server-side schema doesn't store this — we tag client-side at
   * fetch time based on which daemon the list came from.
   */
  backend?: 'wsl' | 'win';
  /**
   * Delete-protected. The daemon auto-creates a "Default" workspace
   * on first run with this flag set so the sidebar always has at
   * least one clickable entry per backend. Users can rename but the
   * × button is hidden.
   */
  protected?: boolean;
  /** Default local context pack applied to newly-created panes. */
  contextPack?: string;
  /** Client-only marker for entries hydrated from the lightweight poll path. */
  summary?: boolean;
}

export interface WorkspaceSummary {
  name: string;
  paneMoveVersion?: number;
  paneIds: string[];
  activeAt: string;
  backend?: 'wsl' | 'win';
  protected?: boolean;
  contextPack?: string;
}

export interface Port {
  port: number;
  addr: string;
  pid: number;
  process: string;
  cmdline: string;
  cwd: string;
  paneId?: string;
  firstAt: string;
  proto: string;
  // Which backend this listener belongs to in dual-backend router mode.
  backend?: 'wsl' | 'win';
  scheme?: 'http' | 'https';
  webStatus?: 'ready' | 'unverified' | 'internal';
  previewPath?: string;
  paneTitle?: string;
  workspace?: string;
}

export interface Health {
  // Live workflow task-engine availability, independent of shared services.
  workflowEngineAvailable?: boolean;
  status: string;
  // Focus (native renderer) availability — false/absent once Focus is retired.
  focusEnabled?: boolean;
  shinoqueRelay?: boolean;
  version: string;
  uptimeSec: number;
  panes: number;
  // Independently embedded frontend provenance. Older daemons omit these;
  // callers must then use the legacy daemon-vs-running-bundle fallback.
  webVersion?: string;
  webSourceSha?: string;
  // Build provenance; channel is "preview" for the owner's follow-main builds.
  build?: { channel?: string };
  // Which backend the daemon runs as ("wsl"|"host"|"win"|"dual") and its
  // platform ("windows-amd64"/"linux-amd64") — from GET /api/health. Absent
  // on older daemons. Used by the StatusBar backend chip.
  backendID?: string;
  shellPlatform?: string;
  system?: {
    level: 'ok' | 'warn' | 'critical' | 'unknown';
    reason?: string;
    // Every area that is not ok, worst first. Absent on older daemons.
    // detail: the long tail behind a short text (host disk: VHDX, %TEMP%), for tooltips.
    reasons?: { key: string; level: 'warn' | 'critical' | 'unknown'; text: string; detail?: string }[];
    sampledAt?: string;
  };
  // Claude Code CLI bootstrap status (daemon claude_bootstrap.go). Absent on
  // older daemons. claudeChecked=false means the first probe hasn't completed
  // yet — surface "checking…" rather than a premature "not installed".
  claudeChecked?: boolean;
  claudeInstalled?: boolean;
  claudeVersion?: string;
  claudeAuthed?: boolean;
  // Muse Code CLI bootstrap status (daemon muse_bootstrap.go) — the parallel
  // of the Claude fields above. Absent on older daemons; museChecked=false
  // means the first probe hasn't completed yet.
  museChecked?: boolean;
  museInstalled?: boolean;
  museVersion?: string;
  museAuthed?: boolean;
  // Codex CLI bootstrap status (daemon codex_bootstrap.go). Absent on older
  // daemons; codexChecked=false means the first probe hasn't completed yet.
  codexChecked?: boolean;
  codexInstalled?: boolean;
  codexVersion?: string;
  codexAuthed?: boolean;
  // Where this daemon's panes run (daemon pane_environment.go). Absent on
  // older daemons.
  paneEnvironment?: 'wsl' | 'windows' | 'linux' | 'macos';
  releaseReadiness?: ReleaseReadiness;
}

export interface ReleaseReadiness {
  state: 'ok' | 'unknown';
  reason?: string;
  lastReleaseVersion: string;
  lastReleaseAnchor: string;
  anchorSource: 'tag' | 'bump-commit' | 'published-manifest' | '';
  anchorProvenance: 'exact' | 'inferred';
  // Why the exact published anchor was degraded or unavailable (fetch failing,
  // released commit absent, diverged checkout). Empty/omitted for a clean
  // anchor; older daemons omit it entirely.
  anchorNote?: string;
  unreleasedTotal: number;
  functionalCount: number;
  docsOnlyCount: number;
  functionalPaths: string[];
  suggestedNextVersion: string;
  oldestUnreleasedAt: string;
  runningVersion: string;
  dirty: boolean;
  computedAt: string;
  // Landings rather than changed files — what the release-train cadence
  // policy triggers on. Older daemons omit both.
  functionalCommitCount?: number;
  oldestFunctionalUnreleasedAt?: string;
}

export interface ReleaseWaiver {
  taskId: string;
  reason: string;
  by?: string;
  at?: string;
}

export interface ReleaseCut {
  id: string;
  version: string;
  repoKey: string;
  sourceSha?: string;
  includes?: string[];
  exclusions?: ReleaseWaiver[];
  declaredBy: 'api' | 'publish.sh';
  status: 'open' | 'published' | 'abandoned';
  publishedSha?: string;
  publishedAt?: string;
  createdAt: string;
  updatedAt: string;
}

export interface ReleaseBlocker {
  taskId: string;
  title: string;
  status: string;
  queueState?: string;
  landOutcome?: string;
  tier?: string;
  why: string;
  landBlocker?: string;
  conflictPaths?: string[];
  blockedByTaskIds?: string[];
  infraCode?: string;
  infraReason?: string;
  clearAction?: string;
}

export interface ReleaseDeadlock {
  code: 'release_runtime_activation_cycle' | string;
  blockerTaskId: string;
  activationTaskIds: string[];
  cause: string;
  breakAction: string;
}

export interface ReleaseInclude {
  taskId: string;
  title: string;
  status: string;
  landOutcome?: string;
  ancestor?: 'verified' | 'assumed' | 'missing' | 'pending' | '';
}

export interface ReleaseCheck {
  allowed: boolean;
  cut: ReleaseCut | null;
  blockers: ReleaseBlocker[];
  waived: ReleaseWaiver[];
  included: ReleaseInclude[];
  warnings: string[];
  deadlocks?: ReleaseDeadlock[];
}

// Self-update status as exposed by GET /api/update/status. Mirrors
// daemon/updater.go's UpdateStatus struct — keep field names in sync.
export interface UpdateStatus {
  current: string;
  latest?: string;
  available: boolean;
  // Post-admission handoff level: true only after download and final safety
  // checks admit the swap. For legacy daemons, workflowDeferred=true takes
  // precedence over a simultaneous applying=true value.
  applying?: boolean;
  // Exact-version rollback hold and the last user-driven rollback attempt.
  // Mirrors daemon/updater.go's UpdateStatus fields.
  holdVersion?: string;
  holdReason?: string;
  holdExpiresMs?: number;
  rollbackState?: 'applying' | 'completed' | 'failed';
  rollbackTarget?: string;
  rollbackError?: string;
  lastCheckedMs?: number;
  // When a check last SUCCEEDED. lastCheckedMs advances on failed attempts
  // too, and latest/notes are last-known-good cache — key any "can't reach
  // the update server" treatment off this, not lastCheckedMs. Absent/0 =
  // never succeeded this daemon lifetime.
  lastSuccessMs?: number;
  lastError?: string;
  notes?: string;
  workflowDeferred?: boolean;
  workflowDeferReason?: string;
  workflowDeferSinceMs?: number;
  workflowDeferDeadlineMs?: number;
  workflowInflight?: number;
  workflowDeferAttempts?: number;
  workflowNextRetryMs?: number;
  workflowDeferState?: 'draining' | 'backoff' | 'publish';
  sourceSha?: string;
  sourceProvenance?: 'exact' | '';
  // The build this install follows (daemon/update_channel.go): "team" only
  // after a positive team verdict. Absent until the first decision.
  updateChannel?: 'public' | 'team';
  // Tauri-shell installer channel. Empty/false when the daemon was
  // launched standalone (no --shell-version) or when there's no shell
  // installer in the manifest for this platform.
  shellCurrent?: string;
  shellLatest?: string;
  shellAvailable?: boolean;
  shellApplying?: boolean;
  shellLaunch?: {
    // rejected: the silent courier found the installer changed after it was
    // verified, installed nothing and removed it (no stagedPath to offer).
    state: 'staged' | 'launching' | 'launched' | 'launch-failed' | 'rejected';
    stagedPath?: string;
    method?: string;
    pid?: string;
    session?: string;
    error?: string;
    updatedAtMs?: number;
  };
  // True when the installer bundles a daemon >= manifest latest. False is an
  // intentional state after daemon-only publishes; it is informational and
  // must not trigger a daemon-first sequence. Admission compares shellLatest
  // (the bundled daemon) with the actually installed version instead.
  shellCarriesLatestDaemon?: boolean;
  // Mandatory-minimum enforcement (min_daemon_version / min_shell_version in
  // the operator's signed control file). required: this daemon is below the
  // floor — the store force-enables the when-idle auto-apply even for "Notify
  // me" users, and RequiredUpdateModal blocks if it can't proceed.
  // shellRequired: the desktop installer is below its floor — it can't
  // self-apply, so the modal blocks new work behind "Restart & update".
  // Absent on daemons older than the enforcement code.
  required?: boolean;
  requiredReason?: string;
  shellRequired?: boolean;
  shellRequiredReason?: string;
  // The signed control file (or this channel's default: on for team, off for
  // public) has the unattended installer restart off. The countdown never
  // arms and the ordinary installer banner shows; clicks are unaffected.
  shellAutoApplyPaused?: boolean;
  shellAutoApplyPausedReason?: string;
}

export type UpdateHoldCause = 'focus' | 'workflow' | 'unknown';

export interface UpdateHold {
  cause: UpdateHoldCause;
  since: number;
  auto: boolean;
  reason?: string;
  paneId?: string;
  workflowInflight?: number;
  workflowDeferDeadlineMs?: number;
  workflowNextRetryMs?: number;
  // Present when the shared workload hold belongs to a rollback rather than
  // a forward update, so retry and explicit-force actions preserve intent.
  rollbackTarget?: string;
  rollbackHoldHours?: number;
}

export interface UpdateHoldPresentation {
  cause?: UpdateHoldCause;
  workflowBackoff: boolean;
  workflowInflight?: number;
  workflowDeferDeadlineMs?: number;
  workflowNextRetryMs?: number;
}

export interface RollbackTarget {
  version: string;
  ranAtMs: number;
  eligible: boolean;
  reason?: string;
}

export interface RollbackVersionHold {
  version: string;
  fromVersion: string;
  reason: string;
  createdAtMs: number;
  expiresAtMs: number;
}

export interface RollbackTargetsResponse {
  current: string;
  targets: RollbackTarget[];
  hold: RollbackVersionHold | null;
  lastAttempt: { targetVersion: string; state: string; error?: string; atMs: number } | null;
}

export interface RollbackDecline {
  store: string;
  requiredReader: number;
  targetReader: number;
  targetVersion: string;
  message: string;
}

export interface RollbackPresentation {
  held: boolean;
  heldVersion?: string;
  holdExpiresMs?: number;
  rollbackFailed: boolean;
  rollbackTarget?: string;
  rollbackError?: string;
}

export function deriveRollbackPresentation(
  status: UpdateStatus | null | undefined,
): RollbackPresentation {
  return {
    held: !!status?.holdVersion,
    heldVersion: status?.holdVersion,
    holdExpiresMs: status?.holdExpiresMs,
    rollbackFailed: status?.rollbackState === 'failed',
    rollbackTarget: status?.rollbackTarget,
    rollbackError: status?.rollbackError,
  };
}

// Authoritative workflow level state wins over browser-local hold metadata.
// Before the next level read, a typed workflow 409 still carries enough data
// to render its retry state immediately and truthfully.
export function deriveUpdateHoldPresentation(
  status: UpdateStatus | null | undefined,
  deferredUpdate: UpdateHold | null | undefined,
): UpdateHoldPresentation {
  const authoritativeWorkflow = status?.workflowDeferred === true
    || status?.workflowDeferState != null;
  const localWorkflow = deferredUpdate?.cause === 'workflow';
  const authoritativeBackoff = status?.workflowDeferState === 'backoff';
  const localBackoff = localWorkflow && deferredUpdate.workflowNextRetryMs != null;

  if (authoritativeWorkflow || localWorkflow) {
    return {
      cause: 'workflow',
      // A typed 409 is the freshest observation of this POST. Surface its
      // retry immediately even when the cached level still says "draining";
      // the next GET will converge on the daemon's backoff state.
      workflowBackoff: authoritativeBackoff || localBackoff,
      workflowInflight: authoritativeWorkflow
        ? (status?.workflowInflight ?? deferredUpdate?.workflowInflight)
        : deferredUpdate?.workflowInflight,
      workflowDeferDeadlineMs: authoritativeWorkflow
        ? (status?.workflowDeferDeadlineMs ?? deferredUpdate?.workflowDeferDeadlineMs)
        : deferredUpdate?.workflowDeferDeadlineMs,
      workflowNextRetryMs: authoritativeBackoff
        ? (status?.workflowNextRetryMs ?? deferredUpdate?.workflowNextRetryMs)
        : localBackoff
          ? deferredUpdate.workflowNextRetryMs
          : status?.workflowNextRetryMs,
    };
  }
  return {
    cause: deferredUpdate?.cause,
    workflowBackoff: false,
  };
}

export interface CreatePaneReq {
  restore?: boolean;
  /** "user" when the person opened the pane from the app; the daemon records other callers as "api". */
  startedBy?: 'user';
  /** Restore only: bring back the saved name, origin and identity. */
  titleOverride?: string;
  origin?: PaneOrigin;
  identity?: PaneIdentity;
  identityBlocked?: boolean;
  cwd?: string;
  cmd?: string[];
  title?: string;
  cols?: number;
  rows?: number;
  workspace?: string;
  workspaceTarget?: string;
  workspaceSplit?: 'horizontal' | 'vertical';
  // Request a native-renderer pane instead of a terminal pane. Gated
  // server-side by SOLU_NATIVE_RENDERER (400 if requested while disabled);
  // omit for the default terminal pane. See daemon/api.go createPane.
  kind?: 'native';
  // Agent protocol, independent from renderer kind. Codex Focus uses the
  // app-server adapter; omitted remains the legacy/default Claude identity.
  agent?: 'claude' | 'codex' | 'muse';
  codexThreadId?: string;
  // Durable Muse session UUID (muse resume <id> / muse exec --session-id).
  museSessionId?: string;
  claudeSessionId?: string;
  // Route the pane's `claude` to GLM (Z.ai) instead of Anthropic. This is a
  // NAME only — the daemon resolves the Z.ai key server-side from
  // ~/.config/glm/env; the browser NEVER sends a token. Omit (or 'anthropic')
  // for the default. A 'glm' request without a configured key gets a 400.
  provider?: 'anthropic' | 'glm';
  // Reasoning-effort level (--effort). Provider-agnostic. Omit for claude's
  // default; a GLM pane left empty is defaulted to 'max' server-side.
  effort?: PaneEffort;
  // Explicit Claude model id (--model). Omit to keep claude's default (fresh
  // session) or the session-stored model (resume).
  model?: string;
  // Product role. Valid for a supported Claude or Codex host. Omit for plain.
  role?: PaneRole;
  /** Absolute/~ local directory, or "none" to opt out of a workspace default. */
  contextPack?: string;
}

// What Claude Code would see for a given pane's project. Returned by
// GET /api/mcp/servers?cwd=…. Read-only — the UI doesn't mutate this;
// users still edit ~/.claude.json or .mcp.json with their own tooling.

// Claude account credentials management (left-sidebar "Credentials"
// section). The active account is machine-wide — switching it changes
// the global Claude login used by every pane. See daemon/claude_accounts.go.
//
// One rate-limit window's usage. `available` is false when the account
// has never been probed (show "—" rather than 0%). `utilPct` is 0-100.
export interface ClaudeUsageWindow {
  utilPct: number;
  resetEpoch: number;
  resetInLabel: string;
  // False when the provider did not report this window. Optional for
  // compatibility with older daemons, which always implied both windows.
  available?: boolean;
  // Expected ("paced") utilization % by now per the user's burn model.
  // 0 when the daemon can't compute a pace (e.g. too little history);
  // the pace overlay is only rendered when this is > 0.
  pacePct: number;
}

export interface ClaudeUsage {
  source?: string;
  sevenDStatus?: string;
  fiveH: ClaudeUsageWindow;
  sevenD: ClaudeUsageWindow;
  fetchedAtEpoch: number;
  stale: boolean;
  // False = this account has never been probed; render usage as "—".
  available: boolean;
}

export interface ClaudeAccount {
  // Present for virtual broker choices; owner edits remain locked.
  brokerReady?: boolean;
  name: string;
  active: boolean;
  subscriptionType?: string;
  usage: ClaudeUsage;
  // Short per-account message to surface in a warning color when non-empty.
  error: string;
  // Next billing-renewal date (unix seconds). When renewalDayOverride > 0 this
  // is the user's pinned billing day; otherwise it's an ESTIMATE — the monthly
  // anniversary auto-derived from the subscription start (best-effort, present
  // only when the OAuth profile probe succeeded), which is NOT authoritative.
  renewalEpoch?: number;
  // User-set billing day-of-month (1..31) that overrides the auto-derived
  // renewal. 0 / absent = auto-derived (no override). The auto date is the
  // ORIGINAL signup anchor and is wrong once a billing cycle moves, so the
  // user can pin the real day here and it wins.
  renewalDayOverride?: number;
  // e.g. "stripe_subscription". From the OAuth profile probe.
  billingType?: string;
  // e.g. "active" / "past_due". From the OAuth profile probe.
  subscriptionStatus?: string;
}

export interface ClaudeAccountsResponse {
  active: string;
  machineWide: boolean;
  // True when the current login isn't saved as a named account yet.
  currentUnsaved: boolean;
  accounts: ClaudeAccount[];
}

// --- Auto-switch (swap-based, Phase 1) ---
//
// The daemon-side controller keeps the BEST Claude account active
// automatically, swapping the machine-wide login only at safe idle
// moments. These DTOs are token-free (mirrors every other claude DTO):
// no token material ever appears in the wire shape. See
// docs/autoswitch-plan.md §7.

// Controller runtime state, surfaced one-word for the status line + badge:
//   ok            — managed, nothing to do (active account is fine)
//   paused        — auto-switch off, or in manual cooldown
//   dwell         — a better account exists but min-dwell hasn't elapsed
//   queued        — switch decided, waiting for a safe (idle) break
//   all-exhausted — every pooled account near its limit
//   pinned        — user pinned a specific account (no auto-switching)
export type AutoSwitchState = 'ok' | 'paused' | 'dwell' | 'queued' | 'all-exhausted' | 'pinned' | 'usage-unknown' | 'no-alternative';

// GET /api/claude/autoswitch response. `pool` maps account name → opted-in.
// `pendingTarget`/`dwellUntil`/`cooldownUntil` are present only when the
// matching state is active (queued/dwell/cooldown), so the UI can render
// live countdowns without polling (refreshed by the SSE event).
export interface AutoSwitchStatus {
  management?: 'team-broker';
  enabled: boolean;
  pinned: string;
  switchThresholdPct: number;
  softWarnPct: number;
  pool: Record<string, boolean>;
  state: AutoSwitchState;
  statusLine: string;
  pendingTarget?: string;
  dwellUntil?: number;
  cooldownUntil?: number;
}

// PUT /api/claude/autoswitch body. Any subset is accepted (Partial), the
// daemon validates + persists + reconfigures and returns the new status.
export interface AutoSwitchUpdate {
  enabled: boolean;
  pinned: string;
  switchThresholdPct: number;
  softWarnPct: number;
  pool: Record<string, boolean>;
}

// One row of the rolling switch history (FIFO cap 50, daemon-side).
// `trigger` distinguishes background auto-switches from manual/reactive
// ones so the UI can label "why am I here". Token-free.
export interface AutoSwitchHistoryEntry {
  ts: number;
  from: string;
  to: string;
  reason: string;
  fromPct5h: number;
  trigger: 'auto' | 'manual' | 'reactive';
}

// GET /api/claude/autoswitch/history response (lazy-loaded by the UI
// disclosure — never auto-fetched).
export interface AutoSwitchHistory {
  entries: AutoSwitchHistoryEntry[];
}

// --- Team-shared credentials (mint-and-distribute v2 WEB) ---
//
// The cloud is a dumb encrypted mailbox. The OWNER mints one INDEPENDENT
// full-scope OAuth bundle per teammate-per-account (browser consent each) and
// distributes a SET per teammate; the OWNER also sets the switching rules ONCE
// for everyone. MEMBERS pull their set + rules and run them through the EXISTING
// personal auto-switch — zero credential management, rules applied read-only.
// The web talks ONLY to the daemon's local endpoints (GET /api/claude/team, the
// mint wizard, rules, revoke); the daemon proxies the cloud. See
// docs/team-creds/MINT-DISTRIBUTE.md §3-§5 + the Phase C mint contract.
//
// Shapes match what the daemon emits (claudecreds_team_api.go getClaudeTeam +
// claudecreds_teamclient.go DTOs). No bundles ever cross this wire.

// Flat per-window usage snapshot as the cloud/daemon emit it (UsageDTO). Kept
// BYTE-IDENTICAL so a future team usage bar can render it unchanged. A
// never-probed account arrives as `usage: null`. (In v2 the owner-side per-member
// usage is populated only when usage telemetry has been pushed to the cloud —
// otherwise null; live member usage is visible in each member's own accounts list.)
export interface ClaudeTeamUsage {
  fiveHUtilPct: number;
  sevenDUtilPct: number;
  fiveHResetEpoch: number;
  sevenDResetEpoch: number;
  fetchedAtEpoch: number;
  subscriptionType: string;
}

// Owner-distributed switching rules (daemon teamRules). The owner edits these
// ONCE for the whole team; every member applies them READ-ONLY. Members see them
// but cannot change them. NOTE: there is NO ranking-strategy knob — the daemon's
// selectBest is hard-coded earliest-weekly-deadline.
export interface ClaudeTeamRules {
  autoSwitchEnabled: boolean;
  switchThresholdPct: number;
  softWarnPct: number;
  maxAccountsPerMember?: number; // 0/absent = unlimited; owner-set cap
  rulesVersion?: number;
}

// Owner-facing sync indicator for one teammate (daemon teamMemberSync). Synced
// iff the member's daemon acked the CURRENT setHash; ackedAtEpoch is the last
// pull time (0 = never), surfaced even when stale so the badge can say
// "pending · last seen 2h ago".
export interface ClaudeTeamMemberSync {
  synced: boolean;
  ackedAtEpoch: number;
  accountCount: number;
}

// One account in the owner's per-member summary (daemon MemberSetAccountSummary,
// GET /sets/{memberUserId} — NO bundle). usage is null unless telemetry exists.
export interface ClaudeTeamMemberAccount {
  accountLabel: string;
  subscriptionType: string;
  sourceAccount: string;
  setVersion: number;
  brokerStatus?: 'ready' | 'migration_required' | 'unsupported';
  migrationState?: 'ready' | 'legacy_rotation_required' | 'unsupported';
  mintedAtEpoch: number;
  assignedBy?: string; // owner who minted this account (audit; shown in the minted tooltip)
  usage: ClaudeTeamUsage | null;
  // Member-daemon-reported health (POST /api/team/claude/status): probeError
  // ""/absent = healthy; a non-empty value (e.g. "re-auth needed") means the
  // member's machine can no longer use this account → owner should Repair.
  probeError?: string;
  statusReportedAtEpoch?: number; // 0/absent = the member's daemon never reported
}

// Member-daemon-reported "currently using" pointer (daemon TeamMemberActive,
// roster `active`): which Claude/Codex account that member's machine is signed
// in with RIGHT NOW. kind: 'team' (account = an owner-minted label),
// 'personal' (the member's own login; account carries its local name),
// 'unsaved' (live Claude login not saved as an account), 'none' (no login),
// '' (unknown / sanitized). Absent entirely when the member's daemon never
// reported (older daemon, or not running). reportedAtEpoch = last report time;
// the member heartbeats every ≤30 min, so an older pointer means their daemon
// stopped reporting.
export interface ClaudeTeamMemberActive {
  claudeAccount: string;
  claudeKind: '' | 'team' | 'personal' | 'unsaved' | 'none';
  codexAccount: string;
  codexKind: '' | 'team' | 'personal' | 'none';
  reportedAtEpoch: number;
}

// One teammate row in the owner roster (daemon TeamMember + per-member summary).
export interface ClaudeTeamMember {
  userId: string;
  displayName: string;
  email: string;
  assignedCount: number;
  needsRepairCount?: number;            // accounts the member's daemon reported dead
  accounts?: ClaudeTeamMemberAccount[]; // present when the per-member summary loaded
  sync?: ClaudeTeamMemberSync;          // present when the per-member summary loaded
  active?: ClaudeTeamMemberActive;      // "currently using" pointer (absent = never reported)
}

// One account a MEMBER has been provisioned (member getClaudeTeam `provisioned[]`).
// Bundle-free; live usage for these shows in the member's normal accounts list
// (they are real local team__ accounts).
export interface ClaudeTeamProvisionedAccount {
  accountLabel: string;
  subscriptionType: string;
  sourceAccount: string;
  setVersion: number;
  brokerStatus?: 'ready' | 'migration_required' | 'unsupported';
  migrationState?: 'ready' | 'legacy_rotation_required' | 'unsupported';
  mintedAtEpoch: number;
}

// GET /api/claude/team — role-aware, READ-ONLY, no bundles. `available` is false
// in local mode (no cloud creds) → the whole team UI hides. Discriminated by
// `role`; only the matching arm is populated.
export interface ClaudeTeam {
  available: boolean;
  teamId?: string;
  teamName?: string;
  role?: 'owner' | 'member';
  // OWNER arm
  members?: ClaudeTeamMember[];
  sourceNames?: string[];        // the owner's mintable source accounts (grid columns)
  // MEMBER arm
  provisioned?: ClaudeTeamProvisionedAccount[];
  teamRulesLocked?: boolean;     // member: the owner rules are applied + locked locally
  // The daemon needed to bootstrap this member's live login from the
  // provisioned set and every activation failed — the member is logged out
  // with the cause otherwise buried in the daemon log. Absent when the
  // bootstrap succeeded or wasn't needed.
  bootstrapError?: string;
  // SHARED
  rules?: ClaudeTeamRules;        // owner edits; member reads
  // Present when the cloud fetch failed (not-in-team / blip).
  error?: string;
}

// POST /api/claude/team/mint/start response (owner): the authorize URL to open.
export interface ClaudeTeamMintStart {
  mintSessionId: string;
  authorizeUrl: string;
}

// POST /api/claude/team/mint/complete response (owner): one minted+uploaded account.
export interface ClaudeTeamMintResult {
  ok: boolean;
  accountLabel: string;
  subscriptionType: string;
  mintedAtEpoch: number;
}

// --- Codex team-shared credentials ---
//
// Codex does not expose the same browser mint flow as Claude credentials. The
// owner saves/imports Codex auth snapshots locally, then assigns those source
// accounts to teammates through the same encrypted team mailbox pattern. No
// auth bundle crosses the web UI.
export interface CodexAccount {
  name: string;
  // Switch key the activate / auto-switch pool / pin surfaces address the row
  // by: the name for a personal source, `team__<label>` for a team snapshot.
  // Optional only for older daemons; fall back to `name` when absent.
  key?: string;
  authKind: 'access_token' | 'api_key' | string;
  active: boolean;
  teamManaged: boolean;
  // ChatGPT plan type from the usage probe (e.g. "pro", "prolite"). Absent
  // until first probe and for api_key accounts (no ChatGPT bearer to probe).
  plan?: string;
  // Same 5h/7d usage shape as the Claude accounts. Individual windows may be
  // unavailable because Codex plans do not necessarily report both.
  usage?: ClaudeUsage;
  // Short per-account probe error, e.g. "token expired; refresh to update".
  error?: string;
  needsReauth?: boolean;
  refreshing?: boolean;
  // Recent provider probe of this exact saved credential revision.
  probeVerified?: boolean;
}

export interface CodexLoginStatus {
  id: string;
  name: string;
  status: 'starting' | 'waiting' | 'saving' | 'publishing' | 'complete' | 'failed' | 'cancelled';
  memberUserId?: string;
  recipient?: string;
  authorizeUrl?: string;
  userCode?: string;
  error?: string;
}

export interface CodexAccountsResponse {
  accounts: CodexAccount[];
  active?: string;
  liveAuthPresent: boolean;
  // A valid live Codex login that matches no saved row (auto-switch refuses
  // to overwrite it until it is saved or rescued).
  currentUnsaved?: boolean;
}

export interface CodexTeamPolicy {
  maxAccountsPerMember?: number;
  policyVersion?: number;
  // Owner-distributed switching rules (applied read-only on members). Absent
  // from an older cloud → members keep local control.
  autoSwitchEnabled?: boolean;
  switchThresholdPct?: number;
}

// PUT /api/codex/team/policy body (owner). Any subset; omitted fields keep
// their stored value.
export interface CodexTeamPolicyUpdate {
  autoSwitchEnabled?: boolean;
  switchThresholdPct?: number;
  maxAccountsPerMember?: number;
}

export interface CodexTeamMemberSync {
  synced: boolean;
  ackedAtEpoch: number;
  accountCount: number;
  targets?: Array<{ machineId: string; backendId: string; synced: boolean; ackedAtEpoch: number; accountCount: number }>;
}

export interface CodexTeamMemberAccount {
  accountLabel: string;
  authKind: 'access_token' | 'api_key' | string;
  sourceAccount: string;
  setVersion: number;
  brokerStatus?: 'ready' | 'migration_required' | 'unsupported';
  migrationState?: 'ready' | 'legacy_rotation_required' | 'unsupported';
  authorizedAtEpoch: number;
  assignedBy?: string;
  healthState?: 'pending' | 'healthy' | 'reauth' | 'error' | 'unverified';
  healthError?: string;
  healthReportedAtEpoch?: number;
  targetMachineId?: string;
  targetBackendId?: string;
}

export interface CodexTeamMember {
  userId: string;
  displayName: string;
  email: string;
  assignedCount: number;
  accounts?: CodexTeamMemberAccount[];
  sync?: CodexTeamMemberSync;
  needsRepairCount?: number;
  machines?: CodexTeamMachine[];
}

export interface CodexTeamMachine { id: string; name: string; online: boolean; backends: string[] | null; }

export interface CodexTeamProvisionedAccount {
  accountLabel: string;
  authKind: 'access_token' | 'api_key' | string;
  sourceAccount: string;
  setVersion: number;
  brokerStatus?: 'ready' | 'migration_required' | 'unsupported';
  migrationState?: 'ready' | 'legacy_rotation_required' | 'unsupported';
  authorizedAtEpoch: number;
}

export interface CodexTeam {
  available: boolean;
  teamId?: string;
  teamName?: string;
  role?: 'owner' | 'member';
  members?: CodexTeamMember[];
  sourceNames?: string[];
  provisioned?: CodexTeamProvisionedAccount[];
  policy?: CodexTeamPolicy;
  // Member: the owner's switching rules are applied and locked locally.
  policyLocked?: boolean;
  bootstrapError?: string;
  error?: string;
}

// ── Muse credentials (third team-credential bundle kind) ────────────────────
// Muse has no usage/plan surface, so these rows carry no usage DTO. The
// member-side rows carry the daemon's install state instead: whether a
// snapshot is staged (connected), whether it IS the machine's live login
// (applied), whether a newer assignment is waiting (updateAvailable), and
// whether muse has refreshed the local copy since we installed it
// (liveRotated) — see docs/team-creds/MUSE.md.
export interface MuseAccount {
  name: string;
  authKind: 'oauth' | 'api_key' | string;
  active: boolean;
  teamManaged: boolean;
}

export interface MuseAccountsResponse {
  accounts: MuseAccount[];
  active?: string;
  liveAuthPresent: boolean;
  livePersonal?: boolean;
  authPath?: string;
}

export interface MuseTeamPolicy {
  maxAccountsPerMember?: number;
  policyVersion?: number;
}

export interface MuseTeamMemberSync {
  synced: boolean;
  ackedAtEpoch: number;
  accountCount: number;
}

export interface MuseTeamMemberAccount {
  accountLabel: string;
  authKind: 'oauth' | 'api_key' | string;
  sourceAccount: string;
  setVersion: number;
  brokerStatus?: 'ready' | 'migration_required' | 'unsupported';
  migrationState?: 'ready' | 'legacy_rotation_required' | 'unsupported';
  authorizedAtEpoch: number;
  assignedBy?: string;
}

export interface MuseTeamMember {
  userId: string;
  displayName: string;
  email: string;
  assignedCount: number;
  accounts?: MuseTeamMemberAccount[];
  sync?: MuseTeamMemberSync;
}

export interface MuseTeamProvisionedAccount {
  accountLabel: string;
  authKind: 'oauth' | 'api_key' | string;
  sourceAccount: string;
  setVersion: number;
  brokerStatus?: 'ready' | 'migration_required' | 'unsupported';
  migrationState?: 'ready' | 'legacy_rotation_required' | 'unsupported';
  authorizedAtEpoch: number;
  connected: boolean;
  applied: boolean;
  updateAvailable: boolean;
  liveRotated: boolean;
}

export interface MuseTeam {
  available: boolean;
  teamId?: string;
  teamName?: string;
  role?: 'owner' | 'member';
  members?: MuseTeamMember[];
  sourceNames?: string[];
  provisioned?: MuseTeamProvisionedAccount[];
  policy?: MuseTeamPolicy;
  bootstrapError?: string;
  error?: string;
  livePersonal?: boolean;
  authPath?: string;
}

export interface McpServer {
  name: string;
  type: 'http' | 'sse' | 'stdio' | '';
  source: 'user' | 'project' | '.mcp.json' | 'codex';
  url?: string;
  command?: string;
  args?: string[];
  // Liveness from a TCP connect probe on the daemon side. "alive" /
  // "dead" for http/sse only; stdio is left empty (we can't tell
  // without spawning the command). Missing / empty = "don't know" —
  // the dot is simply absent in that case.
  status?: 'alive' | 'dead';
}

// Preview channel (GET /api/update/preview): the owner's follow-main builds.
// available=false on machines without a soluterminal source checkout, and for
// remote viewers — the UI then shows nothing about channels.
export interface PreviewChannelStatus {
  available: boolean;
  channel?: 'preview' | 'stable';
  running?: string;
  stable?: string;
  main: {
    running?: string;
    mainSha?: string;
    mainVersion?: string;
    ci?: string; // attested | pending | failed | unavailable
    checkedMs?: number;
    error?: string;
  };
  newChanges: number;
  ahead: number;
  /** Merged changes not yet in a release, newest first, with the steps each
   *  pull request gives for trying it and whether this app already has it. */
  unreleased?: { n: number; sha: string; pr?: number; title: string; howToTry?: string; installed: boolean }[];
  checking?: boolean;
  job?: 'building-preview' | 'switching-to-stable';
  jobLine?: string;
}

// GET/PUT /api/view (daemon/personal_view.go).
export interface PersonalViewSettings {
  personal: boolean;
  available: boolean;
  since?: string;
}

// GET /api/entitlements (daemon/entitlements.go). The tier fields arrived in
// v0.0.1200; older daemons send only team and the team-pool flags.
export type EntitlementsTier = 'none' | 'account' | 'team';

export interface Entitlements {
  tier?: EntitlementsTier;
  team: boolean;
  // The operator's internal tooling (workflow engine, task owners, release
  // train, agents, Aria, Warden, the team credential pool). Older daemons
  // omit it; it then follows team.
  operator?: boolean;
  // The personal view is on: the daemon still reports the real verdict and
  // the web hides team and operator surfaces (entitlements.ts).
  viewPersonal?: boolean;
  account?: boolean;
  accountSwitching: boolean;
  teamCredentials: boolean;
  remoteAccess?: boolean;
  push?: boolean;
  voice?: boolean;
  smartApprovals?: boolean;
  source?: string;
}
