import type { ClosedPane } from './lib/closedPanes';
import type { CreatePaneReq, Health, McpServer, NativeSettings, OrchestratorHostsInfo, PaneEffort, PaneIdentity, PaneInfo, PaneRole, Port, RecoverySettings, ReleaseCheck, Schedule, ScheduleAgent, ScheduleInput, TurnRecord, BrowsedSession, UpdateSettings, Workspace, WorkspaceSummary } from './types';
import { isRouterMode } from './dualBackend';
import { setEnterGapConfig } from './ptySubmit';
import { openTicketedEventSource, type EventStream } from './lib/ticketedEventSource';
import { SupportBundleRemoteError, type SupportBundleResponse } from './lib/supportBundle';
import {
  deviceEnrolledFor,
  deviceSignatureHeaders,
  relayPrefixFor,
  relayTarget,
  signDeviceRequest,
  withDeviceSignatureQuery,
} from './lib/deviceIdentity';
import {
  closePrivateRelayChannels,
  configurePrivateRelay,
  openPrivateRelayEventStream,
  PrivateRelayApprovalNeeded,
  privateRelayTarget,
  relayFetch,
} from './lib/privateRelay';
import {
  forceProbeNow,
  getMachineAccessState,
  reportMachineScopeStatus,
  resetMachineAccess,
  shouldAllowMachineRequest,
} from './lib/machineAccess';

export { SupportBundleRemoteError };
export type { SupportBundleResponse, SupportBundleSection } from './lib/supportBundle';

export interface TeamBrokerActivityResponse {
  window: string;
  activity: Array<{
    memberUserId: string;
    provider: string;
    accountLabel: string;
    requestCount: number;
    lastUsedAtEpoch: number;
  }>;
}

export interface DirCandidate {
  path: string;
  source: 'recent' | 'discovered';
  label: string;
}

// (Codex provider) Which CLI produced the usage. The read API accepts an
// optional `provider` filter (all = no filter) and echoes the resolved value;
// a response WITHOUT the echo came from an older cloud that ignored the filter.
// See docs/USAGE-CODEX-PROVIDER-CONTRACT.md.
export type UsageProvider = 'all' | 'claude' | 'codex';

// (Codex provider) One provider's slice of the window, cost DESC. Token classes
// are normalized to the Claude shape server-side; reasoning_output_tokens is a
// SUBSET of output_tokens (never added to totals).
export interface CCUsageProviderRow {
  provider: string;        // "claude" | "codex"
  cost_usd: number;
  cost_share: number;      // 0..1
  turns: number;
  sessions: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_create_tokens: number;
  reasoning_output_tokens?: number;
}

export interface CCUsageSummary {
  window: string;          // echo of resolved window: "7d"|"30d"|"90d"|"all"
  window_days: number;     // 7|30|90, or 0 for "all"
  generated_at: string;    // RFC3339 UTC
  currency: string;        // "USD"
  notional: boolean;       // always true — subscription users; API-equivalent

  // Client-only flag (NOT from the JSON body): set true when the response
  // carried the `X-Solu-Usage-Stale: 1` header, i.e. the cloud served a cached
  // snapshot because the live query was transient-failing. The UI surfaces a
  // tiny "showing recent (cached) data" hint. Always absent on fresh data.
  _stale?: boolean;

  // (Codex provider) Echo of the applied provider filter. Absent on older
  // clouds, which ignore the filter and always return all providers.
  provider?: UsageProvider;
  // (Local-day grouping) Echo of the IANA zone the cloud bucketed days in
  // (`daily[]`, `active_days`, `by_member[].daily`, activity cells). Present →
  // dates are calendar days in this zone and the heatmap must NOT be rotated.
  // Absent on older clouds → days are UTC and the client rotates the heatmap.
  tz?: string;
  // (Codex provider) Per-provider split, cost DESC. Empty/absent when no rows
  // or on older clouds.
  by_provider?: CCUsageProviderRow[];

  hero: {
    cost_usd: number;
    turns: number;
    sessions: number;
    active_days: number;
    fresh_tokens: number;  // Σ(input + output + cache_create)
    cached_tokens: number; // Σ cache_read
    models_used: number;
    // (Phase 2.1) cached / (cached + input + cache_create_eff); 0..1; input-side
    // only (excludes output). May be absent on older/empty responses.
    cache_hit_rate?: number;
    // (Codex provider) The part of cost_usd priced on an assumed tier (unknown
    // model ids). Absent on older clouds.
    estimated_cost_usd?: number;
  };

  by_model: Array<{
    model: string;
    // (Codex provider) "claude" | "codex"; the grouping key is provider+model.
    // Absent on older clouds (treat as claude).
    provider?: string;
    tier: string;          // opus|sonnet|haiku
    turns: number;
    input_tokens: number;
    output_tokens: number;
    cache_read_tokens: number;
    cache_create_tokens: number;
    cache_create_5m_tokens?: number;
    cache_create_1h_tokens?: number;
    cost_usd: number;
    cost_per_turn_usd: number;
    cost_share: number;    // 0..1
    // (Phase 2.1) true when the tier was assigned by assumption (fable→sonnet,
    // unknown→opus) — the model id matched none of opus/sonnet/haiku. May be
    // absent on older responses.
    estimated?: boolean;
    // (Phase 2.1) USD by token class; the four sum to cost_usd. May be absent.
    cost_breakdown?: {
      input: number;
      output: number;
      cache_read: number;
      cache_create: number;
    };
  }>;

  by_role?: Array<{
    role: string;
    turns: number;
    input_tokens: number;
    cache_read_tokens: number;
    cache_create_tokens: number;
    cache_create_5m_tokens: number;
    cache_create_1h_tokens: number;
    cache_hit_rate: number;
    long_gap_observed: number;
  }>;

  // Per-Claude-account breakdown (which login produced each turn). account is ""
  // for unattributed turns (old daemons / sessions the daemon couldn't attribute)
  // — the UI labels it "Unattributed". May be absent/empty on older responses or
  // when no account data has been captured yet.
  by_account?: Array<{
    account: string;
    // (Codex provider) grouping key is provider+account. Absent on older clouds.
    provider?: string;
    turns: number;
    input_tokens: number;
    output_tokens: number;
    cache_read_tokens: number;
    cache_create_tokens: number;
    cost_usd: number;
    cost_per_turn_usd: number;
    cost_share: number; // 0..1
  }>;

  top_projects: Array<{
    project: string;       // last path segment of cwd; "(unknown)" if empty
    cost_usd: number;
    turns: number;
    cost_share: number;    // 0..1
    repo?: string;         // "owner/repo" slug; "" / absent when the cwd has no git remote
    repo_url?: string;     // clickable https URL to the repo; "" / absent when none

    // (Phase 3c) OWNER-ONLY team view: which members contributed to THIS project
    // and how much — the "who works on what" breakdown. Present ONLY on the
    // team-summary top_projects served to a team OWNER; it is the per-individual
    // project-attribution privacy boundary, so it is ABSENT on solo/member
    // responses AND on a non-owner team response (a non-owner's top_projects is
    // their own personal list, which never carries this). Sorted cost DESC.
    by_member?: Array<{
      user_id: string;
      display_name: string;
      cost_usd: number;
      turns: number;
      cost_share: number;  // member cost in THIS project / project total, 0..1
    }>;
  }>;

  daily: Array<{
    date: string;          // "YYYY-MM-DD"; only days with >=1 turn (client zero-fills)
    cost_usd: number;
    turns: number;
    // (Phase 2.2) per-model cost this day: {model: cost_usd}; keys present only
    // for models active that day; the values sum to that day's cost_usd. Lets the
    // client render the daily bars STACKED by model. May be absent on older
    // responses — fall back to the single-color total bar when so.
    by_model?: Record<string, number>;
  }>;

  in_out: {
    in_app: { turns: number; cost_usd: number };
    out_app: { turns: number; cost_usd: number };
  };

  subagent_share: {
    cost_usd: number;
    cost_share: number;    // 0..1
  };

  // (Phase 2.1) overall USD by token class; the four sum to hero.cost_usd. May
  // be absent on older responses — guard before rendering the breakdown.
  cost_breakdown?: {
    input: number;
    output: number;
    cache_read: number;
    cache_create: number;
  };

  // (Phase 2.1) the price table the numbers were computed with, for
  // traceability. Rendered straight from the response — never hardcoded in the
  // client. May be absent on older responses (hide the price table if so).
  pricing?: {
    as_of: string;         // e.g. "2026-06"
    currency: string;      // "USD"
    unit: string;          // "per_mtok" — values are per MILLION tokens
    // Tiers keyed by name. Known Claude keys are opus/sonnet/haiku/fable, plus
    // OpenAI tiers once Codex usage is priced. The map is open-ended and every
    // key optional so the client renders whatever tiers the server sends (it
    // iterates the keys, never hardcoding the set).
    tiers: Partial<Record<string, CCUsageTierPrice>>;
    assumptions: string;   // human-readable tier-assignment assumptions
  };

  // (Phase 2.2) server-tool call counts over the window. Raw COUNTS only (we
  // don't have their token cost) — the UI shows counts, never dollars. May be
  // absent on older responses; hide the stat when absent or both are 0.
  web_tools?: {
    searches: number;
    fetches: number;
  };

  // (Phase 2.2) top 5 sessions by cost_usd DESC (tie-break session_id). Each
  // carries the session's single highest-cost model, the modal-cwd project
  // label, turn count, and the window-clamped time span. May be absent (or [])
  // on older/empty responses; hide the section when absent or empty. NEVER
  // surface session_id prominently — it's a UUID; use it only as the React key.
  top_sessions?: Array<{
    session_id: string;
    project: string;
    model: string;
    turns: number;
    cost_usd: number;
    started_at: string;       // RFC3339 UTC; min event_ts in the window
    ended_at: string;         // RFC3339 UTC; max event_ts in the window
    duration_seconds: number; // window-clamped (ended_at - started_at)
  }>;

  // (Phase 1) Prior equal-length window, for period-over-period deltas (the
  // hero cards render a ±% vs this). Hero-only — no per-model/daily detail.
  // null when window=="all" (no prior period) or when there's no prior data.
  // Absent on older responses — guard before computing a delta. For team scope
  // this is the team-WIDE prior hero; for member scope, that member's prior.
  previous?: {
    window: string;          // same length as the current window, e.g. "30d"
    hero: {
      cost_usd: number;
      turns: number;
      sessions: number;
      active_days: number;
      fresh_tokens: number;
      cached_tokens: number;
      cache_hit_rate?: number;
    };
  } | null;

  // (Phase 1) Activity intensity by hour-of-day × day-of-week, for a heatmap.
  // Cells are SPARSE (only non-zero) and in UTC; dow is 0=Sunday..6=Saturday to
  // match JS Date.getDay(); hour is 0..23. The client rotates UTC→local for
  // display. For team scope these are team-WIDE totals (a pure aggregate, safe
  // for every member). Absent on older responses — hide the heatmap when so.
  activity?: {
    tz: 'UTC';
    cells: Array<{
      dow: number;           // 0..6, Sunday=0 (matches Date.getDay())
      hour: number;          // 0..23, UTC
      turns: number;
      output_tokens: number;
    }>;
  };
}

// (Phase 2.1) per-tier price row, per MILLION tokens (USD).
export interface CCUsageTierPrice {
  input: number;
  output: number;
  cache_read: number;
  cache_write_5m: number;
  cache_write_1h: number;
  // (Codex provider) "anthropic" | "openai" and a display label. Absent on
  // older clouds (treat as anthropic, label = tier key).
  provider?: string;
  label?: string;
  // (Codex provider) OpenAI has ONE cache-write rate: both cache_write_* carry
  // it and this flag is true, so the UI shows the 1h column as "—".
  cache_write_single?: boolean;
}

// (Phase 3) Team usage rollup. Same shape + math as the per-account
// CCUsageSummary (hero / by_model / top_projects / daily / in_out /
// subagent_share / cost_breakdown / web_tools / pricing — all team-WIDE and
// visible to every member), PLUS team context and a privacy-gated per-member
// breakdown. See docs/USAGE-PHASE2-CONTRACT.md §"Phase 3 — Team summary".
//
// `top_sessions` is intentionally NOT included at team level in v1 (it would
// leak per-session attribution) — so the team view never renders the busiest-
// sessions section. The base type's field stays optional, so omitting it is
// contract-compliant.
export interface CCUsageTeamSummary extends CCUsageSummary {
  // The caller's resolved/primary team for this response. null when the caller
  // is in NO team — the client hides the Team toggle entirely when so. Older
  // daemons/cloud that predate Phase 3 also won't carry it → treat as null.
  team: {
    team_id: string;
    name: string;
    slug: string;
    is_owner: boolean;        // caller == teams.owner_id
    your_roles: string[];     // caller's team_members.roles
    can_see_members: boolean; // = is_owner (v1); server-enforced
    members_total: number;    // COUNT(team_members)
    members_active: number;   // members with >=1 turn in the window
  } | null;

  // ALL teams the caller belongs to. A picker is shown only when length > 1.
  // May be absent on older responses — guard with `?? []`.
  teams: Array<{
    team_id: string;
    name: string;
    is_owner: boolean;
  }>;

  // Privacy-GATED per-member breakdown, sorted by cost_usd DESC (tie-break
  // user_id), already redacted server-side: owner → ALL members; non-owner →
  // ONLY their own entry (matches team.can_see_members). May be absent/empty on
  // older/empty responses — guard with `?? []` before mapping.
  by_member: Array<{
    user_id: string;
    display_name: string;
    email: string;
    is_you: boolean;
    roles: string[];
    cost_usd: number;
    turns: number;
    sessions: number;
    top_model: string;  // member's single highest-cost model
    cost_share: number; // member cost / team hero.cost_usd, 0..1

    // (Phase 2) Enriched per-member metrics powering the owner's 3-lens
    // comparison (Output / Efficiency / Cost). All OPTIONAL — absent on older
    // responses; the table falls back to cost-only columns when so. These are
    // owner-gated by the SAME server-side redaction as the row itself: an owner
    // receives every member's enriched row; a non-owner receives ONLY their own
    // (so a non-owner can still see their own efficiency numbers). NEVER expose
    // another member's enriched data to a non-owner.
    active_days?: number;
    output_tokens?: number;
    input_tokens?: number;
    cache_read_tokens?: number;
    cache_create_tokens?: number; // effective (5m/1h-aware), matches by_model
    cost_per_turn_usd?: number;
    cache_hit_rate?: number;      // 0..1, input-side (excludes output)
    // Per-member daily cost/turns series for an inline sparkline + the owner's
    // multi-member trend overlay. Only days with >=1 turn (client zero-fills).
    // Owner-only via the same row redaction. Absent on older responses.
    daily?: Array<{ date: string; cost_usd: number; turns: number }>;

    // (Phase 3a) Per-member MODEL MIX — how this member splits work across models
    // (Opus vs Sonnet vs Haiku vs Fable). Sorted by cost DESC. Owner-gated by the
    // same row redaction. Lets the owner see HOW a member works, not just how much.
    // Absent on older responses → hide the mini model-mix.
    by_model?: Array<{
      model: string;
      tier: string;        // opus|sonnet|haiku|fable
      cost_usd: number;
      turns: number;
      cost_share: number;  // this member's cost in this model / member total, 0..1
    }>;

    // (Phase 3a) This member's prior equal-length window totals, for a per-member
    // delta ("who's ramping up / cooling down"). null when window=="all" or no
    // prior data. Owner-gated. Absent on older responses → no per-member Δ shown.
    previous?: { cost_usd: number; turns: number } | null;

    // (Codex provider) This member's per-provider split (owner-gated by the
    // same row redaction). Absent on older clouds → no provider mix shown.
    by_provider?: Array<{ provider: string; cost_usd: number; turns: number }>;
  }>;

  // Anonymous, non-identifying comparison stats for team members. The cloud
  // computes this from the full roster before redaction, then serves only
  // aggregate/rank values; it never includes another member's name, email,
  // user_id, project, or per-person row.
  team_peer_stats?: {
    members_total: number;
    members_active: number;
    metrics: Array<{
      key: string;
      label: string;
      format: 'usd' | 'int' | 'pct' | 'tok';
      higher_is_better: boolean;
      value: number;
      rank: number;
      rank_total: number;
      percentile: number;
      team_avg: number;
    }>;
  } | null;
}

// Lightweight team-membership response returned by
// /api/cc-usage/team-summary?context=1. Older cloud versions ignore the query
// parameter and return a full CCUsageTeamSummary instead, so callers must accept
// the union and use `context_only === true` as the discriminator.
export interface CCUsageTeamContext {
  context_only: true;
  generated_at: string;
  tz?: string;
  team: CCUsageTeamSummary['team'];
  teams: CCUsageTeamSummary['teams'];
  _stale?: boolean;
}

// (Phase 3.1) Member detail. A team OWNER drilling from a `by_member` row into
// ONE member's FULL per-account detail. The response is byte-identical to that
// member's own `/summary` (hero / by_model / top_projects / daily / top_sessions
// / in_out / subagent_share / cost_breakdown / web_tools / pricing — the owner is
// authorized for full detail, INCLUDING top_sessions) PLUS a `member` meta block.
// So it extends the per-account CCUsageSummary verbatim. See
// docs/USAGE-PHASE2-CONTRACT.md §"Phase 3.1 — Member detail".
//
// AUTHORIZATION is server-side: the caller may view `user_id` only when it's
// their own, or they OWN a team that user_id belongs to — otherwise the server
// returns 403 (the client surfaces a small "not authorized" note + falls back to
// the team view; it never tries to render a drill it isn't allowed to see).
// One machine belonging to the drilled-into member, mirroring the /api/machines
// MachineDTO shape: live relay status plus the daemon + installer (shell)
// versions the machine last announced at handshake (separate update channels, so
// they can differ). Owner-gated by the same rule as the member detail itself.
export interface MemberMachine {
  id: string;
  name: string;
  online: boolean;
  reconnecting?: boolean;
  health?: string;
  backends?: string[];
  lastSeenAt?: string;
  firstConnectAt?: string;
  daemonVersion?: string;
  shellVersion?: string;
  shellPlatform?: string;
  ownerEmail?: string;
  ownerName?: string;
}

export interface CCUsageMemberSummary extends CCUsageSummary {
  // Meta about the member this detail belongs to. May be absent on older/empty
  // responses (the 403/empty-DB shape still carries it per the contract, but
  // guard anyway) — the client falls back to the selected member's display name.
  member: {
    user_id: string;
    display_name: string;
    email: string;
    roles: string[];
    is_you: boolean;
  };
  // The member's machines (owner-gated). Absent on older cloud / the no-DB
  // shape — guard with `?? []`. Powers the owner's "what are they running"
  // view: per-machine installer + daemon version and online status.
  machines?: MemberMachine[];
}

// Runtime-configurable routing. Local mode leaves `prefix` empty and
// `token` null — all calls hit the same-origin daemon. Cloud mode sets
// `prefix` to `/m/<machineId>` and `token` to the Supabase JWT so the
// cloud can authenticate and forward to the user's daemon.
//
// Dual-backend router mode (Tauri, Backend::Dual) sets `backendPrefix`
// to `/b/wsl` or `/b/win` so the same-origin `solud router` process
// multiplexes calls to the right daemon. `backendPrefix` is appended
// AFTER `prefix` so cloud + router could compose in theory, but today
// they're mutually exclusive (router is desktop-only).
//
// Configured by cloud.ts and by dualBackend init code; read-only elsewhere.
let prefix = '';
let backendPrefix = '';
let token: string | null = null;
// The private relay channel's WebSocket authenticates to the cloud the way
// pane sockets do (see wsProtocols below).
configurePrivateRelay({
  protocols: () => (token ? ['solu-auth', 'bearer.' + token] : undefined),
  // The approval prompt listens for this (DEVICE_APPROVAL_EVENT, declared below).
  onApprovalNeeded: (target) => requestDeviceApproval(target),
  // Not through the channel (that is what failed): the status endpoint is
  // exempt from the device gate and carries only this device's standing.
  deviceStatus: async (target, id) => {
    const path = `/api/remote-devices/${encodeURIComponent(id)}/status`;
    const headers = new Headers();
    if (token) headers.set('Authorization', `Bearer ${token}`);
    const init = await signRelayRequest(target, path, {}, headers);
    // Bounded: the channel's open waits on this answer.
    const r = await fetch(relayPrefixFor(target) + path, { ...init, headers, cache: 'no-store', signal: AbortSignal.timeout(10_000) });
    // The daemon's own 404 ("unknown device"), not the cloud's "machine not found".
    if (r.status === 404) return /unknown device/i.test(await r.text()) ? 'not-approved' : 'unknown';
    if (!r.ok) return 'unknown';
    const d = (await r.json()) as { status?: string };
    return d.status === 'approved' ? 'approved' : 'not-approved';
  },
});
const UI_EVENTS_SSE_PROBE_MIN_INTERVAL_MS = 10_000;
let uiEventsSseProbeAt: number | null = null;
let uiEventsSseProbeInFlight = false;
// In-memory fallback for api.deviceId() when localStorage is unavailable
// (private mode / storage-denied webviews) — stable within the page's life.
let sessionDeviceId: string | null = null;

const clearCCUsageClientCache = () => {
  for (const entry of ccUsageClientInflight.values()) entry.controller.abort();
  ccUsageClientCache.clear();
  ccUsageClientInflight.clear();
  ccUsageClientGeneration.clear();
};

export type BackendId = 'wsl' | 'win';

// A captured snapshot of the mutable routing state above. Task state fetched
// under one scope must not be actionable after configureApi/setBackend routes
// mutations elsewhere (a task shown from machine A staying approvable against
// machine B). `key` deliberately excludes the token: a Supabase session
// refresh on the same machine must not invalidate task state.
export interface RoutingScope {
  prefix: string;
  backendPrefix: string;
  key: string;
}

let principalEpoch = 0;
let principalIdentity: string | null = null;
let principalAccess = true;
export function routingPrincipalAllowed(): boolean {
 const access=getMachineAccessState();
 const denied=access.blocked && !!access.machineId && (prefix === `/m/${access.machineId}` || prefix === `/m/${encodeURIComponent(access.machineId)}`);
 return principalAccess && !denied;
}
export function setApiPrincipalAccess(allowed:boolean): void { if(principalAccess === allowed)return;principalAccess=allowed;notifyRoutingScopeChange(); }
export function routingPrincipalEpoch(): number { return principalEpoch; }
export function setApiPrincipal(identity: string | null, invalidate = false): void {
  if (!invalidate && identity === principalIdentity) return;
  principalIdentity = identity; principalAccess = false; principalEpoch += 1;
  closePrivateRelayChannels();
  notifyRoutingScopeChange();
}
export function currentRoutingScope(): RoutingScope {
  return { prefix, backendPrefix, key: `${prefix}|${backendPrefix}|${principalEpoch}` };
}

const routingScopeListeners = new Set<() => void>();

/** Fires after the machine prefix or backend prefix changes (never on a
 *  token-only change). Returns an unsubscribe. */
export function onRoutingScopeChange(listener: () => void): () => void {
  routingScopeListeners.add(listener);
  return () => { routingScopeListeners.delete(listener); };
}

const notifyRoutingScopeChange = () => {
  routingScopeListeners.forEach((listener) => listener());
};

export function configureApi(opts: { prefix?: string; token?: string | null }) {
  const nextPrefix = opts.prefix !== undefined ? opts.prefix : prefix;
  const nextToken = opts.token !== undefined ? opts.token : token;
  if (nextPrefix !== prefix || nextToken !== token) clearCCUsageClientCache();
  const prefixChanged = nextPrefix !== prefix;
  const accessContextChanged = prefixChanged || nextToken !== token;
  const signedOut = nextToken === null && token !== null;
  if (opts.prefix !== undefined) prefix = opts.prefix;
  if (opts.token !== undefined) token = opts.token;
  // A channel the cloud admitted under a previous machine or sign-in must not
  // outlive it. A refreshed token for the same sign-in keeps the channel (as
  // it keeps open pane sockets); an account switch goes through
  // setApiPrincipal.
  if (prefixChanged || signedOut) closePrivateRelayChannels();
  if (accessContextChanged) {
    resetMachineAccess();
    uiEventsSseProbeAt = null;
    uiEventsSseProbeInFlight = false;
  }
  if (prefixChanged) notifyRoutingScopeChange();
}

/**
 * Router-mode only. Sets the current backend for subsequent API calls
 * (and WS URLs built via `apiOrigin()`). The router authenticates each
 * backend internally, so the Tauri frontend doesn't need a token here.
 */
export function setBackend(b: BackendId | null) {
  const next = b === null ? '' : `/b/${b}`;
  if (next === backendPrefix) return;
  clearCCUsageClientCache();
  backendPrefix = next;
  notifyRoutingScopeChange();
}

/** Current backend id, or null if not in router mode. */
export function currentBackend(): BackendId | null {
  if (backendPrefix === '/b/wsl') return 'wsl';
  if (backendPrefix === '/b/win') return 'win';
  return null;
}

export function currentToken(): string | null { return token; }

// Returns the per-machine prefix when in cloud mode (e.g. "/m/<mid>")
// or an empty string locally. Callers use this to decide whether to
// rewrite URLs (e.g. iframe src for localhost dev servers → go through
// the /api/proxy/ reverse-proxy route on the daemon).
export function currentPrefix(): string { return prefix; }

// Current API routing prefix, including the active dual-backend
// selector when router mode is enabled.
export function currentApiPrefix(): string { return prefix + backendPrefix; }

// Error thrown by the j() response decoder. Carries the HTTP status so
// callers can branch on it (e.g. FileTile distinguishing a 403 secret
// refusal from a 404). `message` stays the server's human-readable body
// for 4xx, so existing `(e as Error).message` consumers are unaffected.
/** Whether err is the daemon refusing this device (approved devices only). */
export function isDeviceNotApprovedError(err: unknown): boolean {
  if (err instanceof TypeError && err.cause instanceof PrivateRelayApprovalNeeded) return true;
  return err instanceof ApiError && err.status === 401
    && (err.body as { error?: unknown } | undefined)?.error === 'device_not_approved';
}

export class ApiError extends Error {
  status: number;
  body?: unknown;
  constructor(status: number, message: string, body?: unknown) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.body = body;
  }
}

/** `ApiError.body.code` for a scoped mutation refused client-side because the
 *  captured routing scope no longer matches (see apiFetchScoped). Nothing was
 *  sent, so no backend can ever report this — the UI must. */
export const WORKFLOW_SCOPE_MISMATCH = 'workflow_scope_mismatch';

/** Reads the stable `code` off an ApiError body, when the thrower set one. */
export function apiErrorCode(error: unknown): string | undefined {
  if (!(error instanceof ApiError)) return undefined;
  const body = error.body;
  if (typeof body === 'object' && body !== null && 'code' in body) {
    const code = (body as { code?: unknown }).code;
    if (typeof code === 'string') return code;
  }
  return undefined;
}

/**
 * Reads the wait off the daemon's 429 recheck body. The recheck spends real
 * money, so its rate limit is a fact the UI has to be able to state — a bare
 * "429" would read as a bug rather than as the guard doing its job.
 */
export function recheckRetryAfterSeconds(error: unknown): number | undefined {
  if (!(error instanceof ApiError) || error.status !== 429) return undefined;
  const body = error.body;
  if (typeof body === 'object' && body !== null && 'retryAfterSeconds' in body) {
    const seconds = (body as { retryAfterSeconds?: unknown }).retryAfterSeconds;
    if (typeof seconds === 'number' && Number.isFinite(seconds)) return seconds;
  }
  return undefined;
}

export interface WorkflowOrigin {
  /** composer | mcp | api | agent | revert | recovery. */
  source: string;
  paneId?: string;
  /** Display title of the origin pane, resolved server-side: the pane's LIVE
   *  header title while it exists, the creation-time snapshot once it is gone,
   *  and a short pane id for tasks filed before the field existed. */
  paneTitle?: string;
  /** Whether the origin pane is still in the live pane registry. Present only
   *  for pane origins — absent means "not a pane", never "closed". */
  paneAlive?: boolean;
  workspace?: string;
  /** Set when source === 'agent': which managed agent filed this, and in which run. */
  agentId?: string;
  /** Managed-agent display name, joined by the daemon so the client needs no
   *  agent-directory fetch to name an agent-filed task. */
  agentName?: string;
  runId?: string;
}

export interface WorkflowRoleBinding {
  runtime: string;
  model?: string;
  effort?: string;
  permissionPreset?: string;
  cacheTtl?: 'auto' | '5m' | '1h';
}

export const WORKFLOW_MODEL_TIERS = ['fast', 'standard', 'premium', 'frontier'] as const;
export type WorkflowModelTier = typeof WORKFLOW_MODEL_TIERS[number];

export interface WorkflowRuntimeInfo {
  id: string;
  provider: string;
  caps: {
    provider: string;
    billingMode: string;
    supportsUsage: boolean;
    supportsMcp: boolean;
    supportsNativeSubagents: boolean;
    supportsSandbox: boolean;
    supportsScopedWrite?: boolean;
    supportsStructuredOutput: boolean;
    supportsResume: boolean;
    supportsPane: boolean;
    supportsEffort?: boolean;
    effortLevels?: string[];
    promptCacheTtlControl?: 'supported' | 'unsupported' | 'unknown';
    supportedRoles?: string[];
    supportsToolIsolation?: boolean;
    supportsEvidenceOnly?: boolean;
    supportsBrowserDriver?: boolean;
    streamsLiveOutput?: boolean;
  };
}

export interface WorkflowModelCatalogEntry {
	id: string;
	provider: string;
	displayName: string;
	docsUrl: string;
	caps: WorkflowRuntimeInfo['caps'];
	models: Array<{ id: string; tier: WorkflowModelTier; effort?: WorkflowModelEffortCatalog }>;
	effort: WorkflowModelEffortCatalog;
}

export interface WorkflowModelEffortCatalog {
	supported: boolean;
	nativeValues: string[];
	presets?: Partial<Record<'quick' | 'balanced' | 'thorough' | 'max', string>>;
}

export interface WorkflowProviderInfo {
  slug: string;
  vendor: string;
  displayName: string;
  docsUrl: string;
  runtimes: { id: string; family: string; billing: string; binaryName: string }[];
}

export interface WorkflowQuotaWindow {
	window: '5h' | '7d';
	percentRemaining: number;
	resetsAtEpoch: number;
}

export type WorkflowRuntimeSecretStatus = {
	env: string;
	required: boolean;
	configured: boolean;
};

export interface WorkflowRuntimeHealth {
	id: string;
	provider: string;
	signedIn: boolean | null;
	identity: { email: string; plan: string };
	quota: WorkflowQuotaWindow[];
	lastProbeAtEpoch: number;
	/** `manual_recheck` = an operator's "Test now" — a real run, not a cache read. */
	probeSource: 'cache' | 'live' | 'manual_recheck';
	lastInvocation: { atEpoch: number; ok: boolean; failureKind?: string } | null;
	status: 'not_installed' | 'healthy' | 'degraded' | 'quota_exhausted' | 'auth_error' | 'billing_error' | 'signed_out' | 'unknown';
	cooldownUntilEpoch?: number;
	probeErr?: string;
	/** null = this runtime exposes no installed-state locator (fails open). */
	installed: boolean | null;
	binaryPath?: string;
	installedCheckedAtEpoch?: number;
	secrets?: WorkflowRuntimeSecretStatus[];
	breaker?: { state: 'closed' | 'open' | 'half_open'; openedAtEpoch?: number; failures: number; lastKind?: string };
	/** Absent when connected to a daemon predating fallback consent. */
	credentialState?: 'ok' | 'missing' | 'reauth_required' | 'exhausted_until' | 'unknown';
	credentialUntilEpoch?: number;
}

export type WorkflowFallbackPolicyValue = 'ask' | 'auto_compatible' | 'never';
export interface WorkflowFallbackPolicyProvenance { value: WorkflowFallbackPolicyValue | 'legacy_auto'; source: string; scope: 'task' | 'settings' | 'engine' | 'legacy'; routeConsent?: WorkflowFallbackRouteConsent }
export interface WorkflowFallbackRouteAuthorization { allowCriticalReview?: boolean; allowPromptStructuredOutput?: boolean; allowUnknownCost?: boolean; maxTaskInvocations: number }
export interface WorkflowFallbackRouteConsent { role: string; from: WorkflowRoleBinding; to: WorkflowRoleBinding; grantedAt: string; source: 'decision' | 'settings'; taskId?: string; authorization?: WorkflowFallbackRouteAuthorization }
export interface WorkflowFallbackHopFacts {
	reason: string; detail?: string; failureKind?: string; from: WorkflowRoleBinding; to: WorkflowRoleBinding;
	fromProvider?: string; toProvider?: string; crossProvider: boolean; fromTier?: string; toTier?: string; tierKnown: boolean; tierDelta: 'same'|'higher'|'lower'|'unknown'; floor?: string; belowFloor: boolean;
	fromEffortIntent: string; toEffortIntent: string; effortKnown: boolean; effortReduced: boolean; fromPermission: string; toPermission: string; permissionEqual: boolean;
	requiredCapabilities?: string[]; lostCapabilities?: string[]; costRatio?: number; costSource: string; costRatioMax: number; costKnown: boolean; costWithinBound: boolean; spentUsd: number; spentCostIncomplete?: boolean; ceilingUsd?: number; nextInvocationUsd?: number; nextCostSource?: string; projectedOverCeiling: boolean;
	credentialState: string; credentialUntilEpoch?: number; toAccountClass?: string; toBillingIdentity?: string; breakerState: string; stageRisk: string; resolvedTier?: string; chainSource?: string; authoredChainSource?: string; priorEngagements: number; priorAttempts: number; compatible: boolean; incompatible?: string[];
	routeAuthorization?: WorkflowFallbackRouteAuthorization; authorizationHash?: string; authorizationDenials?: string[]; unknownCostAccepted?: boolean; taskInvocationCount?: number; taskInvocationLimit?: number;
	/** Optional additive routing-plane fields (older daemon payloads omit them). */
	plane?: 'availability' | 'quality'; failureScope?: WorkflowFailureScope; failedAccountMasked?: string; recoveryCondition?: string; qualityTrigger?: string;
}
export interface WorkflowFailureScope {
	version?: number; kind?: string; identity?: string; runtime?: string; provider?: string; resetsAtEpoch?: number; source?: string; evidence?: string;
}
export interface WorkflowRouteCandidate {
	index: number; binding: WorkflowRoleBinding; eligible: boolean; reasonCode?: string; detail?: string; provider?: string; accountMasked?: string; role?: string; tier?: string; profileVersion?: string; nativeEffort?: string;
}
/** The per-role routing decision attached to a runtime-health resolution row. */
export interface WorkflowResolutionDecision {
	plane?: 'availability' | 'quality';
	reason?: string;
	trigger?: string;
	evidence?: string;
	effectiveEffort?: string;
	profileVersion?: string;
	policySource?: string;
	failureScope?: WorkflowFailureScope;
	recoveryCondition?: string;
	candidates?: WorkflowRouteCandidate[];
}
export interface WorkflowFallbackDecision {
	id: string; stage: string; role: string; baseRole: string; fromIndex: number; toIndex: number; from: WorkflowRoleBinding; to: WorkflowRoleBinding; reason: string; detail?: string; error?: string; resumeAt?: string; degraded?: boolean; floor?: string;
	facts: WorkflowFallbackHopFacts; skipped?: Array<{index:number; binding:WorkflowRoleBinding; reason:string}>; policy: WorkflowFallbackPolicyProvenance; consequence: {ifAccepted:string; ifDeclined:string; declinedPath:string}; state: 'proposed'|'accepted'|'declined'|'parked'|'auto'|'policy_never'; proposedAt: string; decidedAt?: string; answer?: 'use_once'|'always'|'never'|'park'; actor?: string; note?: string; settingsWritten?: boolean; settingsDiff?: {path:string;before:string;after:string;hash:string}; attempt: number;
	/** Optional additive routing-plane fields. */
	plane?: 'availability' | 'quality'; failureScope?: WorkflowFailureScope; recoveryCondition?: string; trigger?: string; evidence?: string; effectiveEffort?: string; maxSteps?: number; profileVersion?: string; policySource?: string; candidates?: WorkflowRouteCandidate[];
}

export interface WorkflowRoleResolution {
	role: string;
	/** `installed` null = never asked / no installed-state locator (fails open). */
	chain: { runtime: string; model?: string; effort?: string; cacheTtl?: 'auto' | '5m' | '1h'; effortSource?: 'task' | 'settings' | 'workflow_default' | 'legacy_unpinned' | 'task_profile' | 'task_profile'; effortSupported?: boolean; health: string; installed: boolean | null; available: boolean }[];
	/** Which layer produced this chain. The runtime-health panel has no task, so `task` never appears there. */
	source: 'task' | 'settings' | 'lineup' | 'engine' | 'task_profile';
	/** Exact resolver rung: settings-chain-base, lineup-fallback-exact, etc. */
	sourceDetail?: string;
	effective: { runtime: string; model?: string; cacheTtl?: 'auto' | '5m' | '1h' };
	/** The compiled chain for this role with every task/settings rung stripped (§8.2). Absent on daemons older than S6. */
	lineup?: { runtime: string; model?: string; cacheTtl?: 'auto' | '5m' | '1h' }[];
	/** Optional routing decision for this role (plane/failure scope/recovery). */
	fallbackDecision?: WorkflowResolutionDecision;
}

export interface WorkflowRuntimesHealth {
	runtimes: WorkflowRuntimeHealth[];
	resolution: WorkflowRoleResolution[];
	diskLow?: boolean;
	diskFreeMB?: number;
	diskFloorMB?: number;
	workflowEffort?: { default: string; source: 'settings' | 'shipped' };
}

/**
 * One completed operator recheck ("Test now"): a real, tiny paid run whose
 * outcome the daemon recorded, plus the refreshed health payload. `ok:false`
 * with `providerError` is a COMPLETED recheck — the provider answered, and it
 * still refuses.
 */
export interface WorkflowRuntimeRecheck extends WorkflowRuntimesHealth {
	ok: boolean;
	providerError?: string;
}

export interface ConfigurationAuditEntry {
  id: string;
  at: string;
  action: 'adopt_coordinator_template' | 'set_supervised_automation';
  status: 'applying' | 'applied' | 'failed';
  callerPane: string;
  summary: string;
  authorizationEvidence?: string;
  fingerprint?: string;
  detail?: string;
}

export interface AutonomyOverride {
  enabled: boolean;
  acknowledge: string;
  policy: 'refile' | 'archive';
  maxPerPass: number;
  fullAutomation: boolean;
  resolveWithoutEvidence: boolean;
  acknowledgeAgentReports: boolean;
}

export interface WorkflowSettings {
  modelPolicy?: { blockedFamilies?: string[] };
  localOpenAI?: { baseUrl?: string; model?: string };
  // Explicit resource-cap overrides. An omitted field inherits the env var and
  // then the adaptive default — see queue.resolvedCaps for what actually won.
	resources?: {
 adaptiveHostDiskFloor?:boolean; rotateWSLCrashDumps?:boolean;
		maxConcurrentTasks?: number;
		maxHeavyOps?: number;
		heavyCapFloor?: number;
		maxSuites?: number;
    // Managed-agent and task-schedule runs share this pool and bookkeeping reserve.
    maxAgentRuns?: number;
    reservedBookkeepingRuns?: number;
    memFloorMB?: number;
    diskFloorMB?: number;
	hostDiskFloorMB?: number;
	hostDiskFloorPct?: number;
	hostDiskHysteresisMB?: number;
	browserHostReserveMB?: number;
    previewCacheCapMB?: number;
	adaptiveHeavy?: boolean;
	environmentEscalateHours?: number;
  };
  // Opt-in publish cadence (default off). The train only ships work that is
  // already landed on origin/main; approvals and merges stay human.
  releaseTrain?: {
    enabled?: boolean;
    minFunctionalLandings?: number;
    maxUnreleasedAgeHours?: number;
    quietRequired?: boolean;
    notifyVetoMinutes?: number;
  };
}

export interface WorkflowTrueYoloUsage {
  tokens: number;
  costUsd: number;
  elapsedSeconds: number;
  actions: number;
  successorDepth: number;
  repairs: number;
  reverifies: number;
  modelSwitches: number;
  mergeAttempts: number;
  releaseAttempts: number;
  rollbackAttempts: number;
}

export interface WorkflowAuthorityReason {
  kind: 'human_only' | 'outside_grant' | 'record_only';
  code: string;
  message: string;
  requiredScopes?: string[];
}

export interface WorkflowRecommendationScope {
  repoPath: string;
  taskId?: string;
  campaignId?: string;
  changedPaths: string[];
  externalScope?: string;
}

export interface WorkflowRecommendationConfidence {
  level: 'low' | 'medium' | 'high';
  score: number;
  source: string;
  basis: string;
}

export interface WorkflowRecommendationReversibility {
  class: 'reversible' | 'destructive_recoverable' | 'irreversible' | 'human_only' | string;
  rollbackAction?: string;
  evidence: WorkflowProposalEvidence[];
}

export interface WorkflowRecommendationExpected {
  tokens?: number;
  costUsd?: number;
  durationSeconds?: number;
  unknownReason?: string;
}

export interface WorkflowRecommendationDependency {
  kind: string;
  ref: string;
  requiredState: string;
}

export interface WorkflowRecommendationEnvelope {
  schemaVersion: 1;
  actionKind: string;
  scope: WorkflowRecommendationScope;
  authorityClass: string;
  evidence: WorkflowProposalEvidence[];
  confidence: WorkflowRecommendationConfidence;
  reversibility: WorkflowRecommendationReversibility;
  expected: WorkflowRecommendationExpected;
  dependencies: WorkflowRecommendationDependency[];
  idempotencyKey: string;
  expiresAt: string;
  authorityReason?: WorkflowAuthorityReason;
}

export interface WorkflowTrueYoloDependencyState {
  kind: string;
  ref: string;
  state: string;
}

export interface WorkflowTrueYoloDecision {
  envelopeHash: string;
  grantHash?: string;
  configHash?: string;
  grantId?: string;
  proposer: string;
  engineActor: string;
  evaluatedAt: string;
  outcome: 'human_only' | 'outside_grant' | 'record_only';
  executable: boolean;
  reason: WorkflowAuthorityReason;
  evidence: WorkflowProposalEvidence[];
  candidateCommit?: string;
  candidateTree?: string;
  dependencyStates: WorkflowTrueYoloDependencyState[];
  preActionUsage?: WorkflowTrueYoloUsage;
}

export type WorkflowTemplatePermission = 'read_only' | 'docs_write' | 'workspace_write';

export interface WorkflowGatePolicy {
  id: string;
  name: string;
  gates: Array<'merge' | 'plan'>;
  deliverables: Array<'change' | 'plan' | 'report'>;
  tiers: Array<'small' | 'standard' | 'complex' | 'critical'>;
  repos: string[];
  maxRepairRounds?: number;
  maxFiles?: number;
  maxChangedLines?: number;
  vetoMinutes?: number;
  planContinue?: boolean;
  protectedPaths?: string[];
  allowAuthorityChanges?: boolean;
}

export interface ReleaseTrainPreview {
  evaluatedAt: string;
  ref: string;
  headSha?: string;
  version?: string;
  lastReleaseVersion?: string;
  functionalLandings: number;
  minFunctionalLandings: number;
  oldestFunctionalAt?: string;
  ageHours: number;
  maxAgeHours: number;
  landingsRemaining?: number; // min − landings, clamped at 0; daemon-derived
  ageTriggerAt?: string; // oldestFunctionalAt + maxAgeHours (RFC3339); daemon-derived, absent when age unknown
  engine: { running: number; pending: number; awaitingApproval: number; quiet: boolean };
  eligible: boolean;
  reasons: string[] | null;
  wouldDepartAt?: string;
  restartNote?: string;
}

export interface ReleaseTrainEvent {
  seq: number;
  type: string;
  at: string;
  trainId?: string;
  data?: Record<string, unknown>;
}

export interface ReleaseTrainRun {
  id: string;
  version: string;
  sourceSha?: string;
  status: string;
  declaredAt: string;
  departAt: string;
  /** The release cut publish.sh declared for this train, once it has shipped. */
  cutId?: string;
  publishedAt?: string;
  logPath?: string;
  /** The release shipped, but the daemon restarted before train bookkeeping completed. */
  supervisionLost?: boolean;
  publishNote?: string;
}

export type ReleaseTrainPhase =
  | 'disabled' | 'idle' | 'declared' | 'holding' | 'publishing' | 'parked' | 'restart_pending';

export type ReleaseTrainPublishState =
  | 'idle' | 'queued' | 'testing' | 'failed' | 'retrying' | 'published';

export interface ReleaseTrainStatus {
  /** Separate from release-scope readiness: health of the latest publish attempt. */
  publishState?: ReleaseTrainPublishState;
  policy: {
    enabled: boolean;
    minFunctionalLandings: number;
    maxUnreleasedAgeHours: number;
    quietRequired: boolean;
    notifyVetoMinutes: number;
  };
  state: {
    phase: ReleaseTrainPhase;
    train?: ReleaseTrainRun | null;
    parked?: {
      reason: string;
      head?: string;
      at: string;
      failureClass?: string;
      evidence?: string;
      sourceSha?: string;
      attempt?: number;
      maxAttempts?: number;
      retryEligible?: boolean;
      nextRetryAt?: string;
    } | null;
    restartPending?: { version: string; since: string } | null;
  };
  preview: ReleaseTrainPreview | null;
  events: ReleaseTrainEvent[] | null;
}

export interface WorkflowMilestone {
  index: number;
  title: string;
  prompt: string;
  sourceTaskId: string;
  subdeliverable?: string;
  acceptanceCriteria?: string[];
  declaredBaseSha?: string;
}

// Landed-work summary produced by the retired task engine's shipped sweep:
// userFacing is the sentence the Shipped view reads out, engineering the
// implementation-level companion shown in the task detail.
export interface WorkflowShippedSummary {
  userFacing: string;
  engineering?: string;
}

export interface WorkflowNarration {
  at: string;
  stage?: string;
  trigger?: string;
  text: string;
}

export interface WorkflowFailureAdvisory {
  at: string;
  stage?: string;
  signature: string;
  occurrences: number;
  diagnosisDoc?: string;
  fixTaskId?: string;
  fixState?: string;
  disposition?: string;
  text: string;
}

export interface WorkflowTaskPreview {
  id: string;
  url: string;
  state: 'starting' | 'running' | 'idleStopped' | 'expired' | 'error';
  mode?: string;
  recipeArtifactId?: string;
  expiresAt?: string;
}

export interface PreviewEligibility {
  eligible: boolean;
  mode?: string;
  recipeArtifactId?: string;
  reason?: string;
}

// One published release as recorded by the release ledger. publishedAt dates
// the release-grouped Shipped history.
export interface WorkflowReleaseLedgerEntry {
  version: string;
  repoKey?: string;
  sourceSha: string;
  publishedAt: string;
  source: string;
}

export type WorkflowHealthVerdict = 'progressing' | 'waiting' | 'slow' | 'recovering' | 'stuck' | 'needs_action';
export interface WorkflowHealthExpected {
  p50Sec: number;
  p90Sec: number;
  samples: number;
  confidence: 'cohort' | 'tier';
}
export interface WorkflowTaskHealth {
  verdict: WorkflowHealthVerdict;
  reason: string;
  headline: string;
  since: string;
  checkedAt: string;
  command?: string;
  stageElapsedSec?: number;
  expected?: WorkflowHealthExpected;
  lastActivityAt?: string;
  lastActivityKind?: string;
  silenceSec?: number;
  timeoutRemainingSec?: number;
  idleRemainingSec?: number;
  waitRelease?: string;
  attempt?: number;
  attemptMax?: number;
  autoActionAt?: string;
  nextAction: { kind: 'none' | 'automatic' | 'human'; text: string };
  evidence?: string[];
}

export interface WorkflowOwnerRetirement {
  paneId: string;
  successorId: string;
  currentSuccessorId?: string;
  linkAt: string;
  phase: 'deferred' | 'closing' | 'retired' | 'reactivated';
  reason?: string;
  attempts: number;
  updatedAt: string;
  nextRetryAt?: string;
  conversationId?: string;
  historyAvailable: boolean;
  historyArtifact?: string;
}
export interface TaskOwnerState {
  maxDecisions?: number;
  paneId?: string; workspace?: string;
  retirement?: WorkflowOwnerRetirement;
  workMode?: 'direct';
  authoring?: {
    epoch: string;
    phase: 'preparing' | 'editing' | 'submitted' | 'complete';
    authorBinding?: WorkflowRoleBinding;
    beginVersion?: number;
    beginSpecDigest?: string;
    submittedVersion?: number;
    attemptId?: string;
    artifactId?: string;
    summary?: string;
    checkpointSha?: string;
  };
  version: number; turns: number; question?: string;
  questionEpoch?: string; questionAttemptId?: string;
  capabilityInventoryRecorded?: boolean;
  runtimeDiagnostic?: { kind: 'claude_weekly_limit'; active: boolean; paneId: string; pid: number; sessionId?: string; fingerprint: string; resetDisplay?: string; observedAt: string; clearedAt?: string };
  entries: { author: string; body: string; action?: string; artifactId?: string; at: string }[];
}
export interface OwnerNavigation {
  recovery?: { state: string; responsible: string; reason: string; nextRetryAt?: string };
  version: 1; eventSequence: number; canDispatch: boolean;
  nextAction: { action: string; reason: string };
  attentionKind?: string; collaborationPaused: boolean; needsUser: boolean;
  terminalAvailability: 'available'|'suspended'|'missing'|'background'|'history'|'identity_mismatch'|'unknown';
  meaningfulUpdate?: { source: 'narration'|'owner_decision'|'instruction'|'task_update'; id: string; text: string; at: string };
}
export interface WorkflowTaskSummary {
  processCleanupPending?: boolean;
  processCleanupError?: string;
  retirement?: { operationId: string; phase: string };
  ownerNavigation?: OwnerNavigation;
  executionMode?: 'owner' | 'terminal_owner';
  owner?: TaskOwnerState;
  id: string;
  title: string;
  preview?: WorkflowTaskPreview;
  status: string;
  stage?: string;
  narration?: WorkflowNarration;
  failureAdvisory?: WorkflowFailureAdvisory;
  health?: WorkflowTaskHealth;
  // Present on list summaries so pre-classification tasks can match the same
  // tier bucket the stage-stats endpoint uses.
  requestedTier?: string;
  resolvedTier?: string;
  deliverable?: string;
  stagePlan?: string[];
  templateId?: string;
  templateVersion?: string;
  templateCompilerVersion?: string;
  templateDecisionKind?: string;
  templatePlanSummary?: {
    stagePlan: string[];
    permission?: WorkflowTemplatePermission;
    checks?: string[];
    humanDecisionKinds?: string[];
  };
  repoPath: string;
  origin?: WorkflowOrigin;
  /** Daemon-owned per-pane open-work classification (rules in
   *  daemon/workflow_pane_work.go). Absent when the row does not count toward
   *  its pane's delegated-work badge; the client only counts occurrences. */
  paneWork?: 'running' | 'needs_you' | 'open';
  archived?: boolean;
  failureKind?: string;
  failureDetail?: string;
  failureClass?: 'environment' | string;
  environmentWait?: WorkflowEnvironmentWait;
  environmentParks?: number;
  retryOf?: string;
  refileOf?: string;
  supersededBy?: string;
  successorFate?: {
    kind: 'waiting' | 'landed' | 'dead';
    taskId?: string;
    landedSha?: string;
  };
  supersededLink?: {
    /** Known values are human, filer, agent, proposal, retry, and reconciliation;
     * empty or absent means the event predates provenance. */
    kind: string;
    agentId?: string;
    runId?: string;
    paneId?: string;
    at?: string;
  };
  resumeAt?: string;
  resumeCount?: number;
  lastResumedAt?: string;
  /** Lifetime count of resumes caused by a managed agent, and who caused the last one. */
  agentResumeCount?: number;
  lastResumedBy?: string;
  updatedAt: string;
  createdAt: string;
  disposition?: WorkflowDisposition;
  landOutcome?: string;
  landBlocker?: 'dirty_tree' | 'rebase_conflict' | 'reverify_failed' | 'no_checks' | 'head_churn' | 'not_running' | 'assertion_failed' | 'assertion_unverified' | string;
  landHold?: {
    attempt: number;
    maxAttempts: number;
    nextRetryAt: string;
    blocker: string;
    detail: string;
  };
  // Shared id of the batched landing this task merged through: several
  // approved tasks verified once as one candidate and landed together.
  landTrainId?: string;
  reconciliationOf?: string;
  relandLane?: {
    parentTaskId: string;
    blocker: string;
    approvedPatchId: string;
    approvedCandidateSha: string;
    verifiedBaseSha?: string;
    replay?: {
      mode: 'automatic' | 'conflicted';
      from: string;
      onto: string;
      replayedSha?: string;
      commits?: number;
      conflicts?: string[];
      at: string;
    };
    review?: {
      skip: boolean;
      kind?: string;
      reasons?: string[];
      requiredRun: number;
      at: string;
    };
  };
  // Set only by the human mark-resolved closure of a failed task: the work
  // reached the repository outside the engine and this is the evidence. Its
  // presence on a cancelled task is what makes the board read "resolved"
  // rather than "cancelled".
  resolution?: { commit: string; note?: string; at: string; resolvedBy?: string; successorTaskId?: string };
  janitorCancel?: {
    proposedAt?: string;
    state: 'scheduled' | 'vetoed' | 'cancelled' | 'withdrawn' | 'needs_human';
    decideAt?: string;
    reason: string;
    evidenceTaskId?: string;
    evidenceCommit?: string;
    fingerprint?: string;
    evidencePaths?: string[];
    subjectPaths?: string[];
  };
  engineClosure?: {
    kind: 'retire_stranded' | 'resolve_failed' | 'retire_dependency_failed';
    proposedAt: string;
    decideAt: string;
    state: 'scheduled' | 'vetoed' | 'withdrawn' | 'executed';
    reason: string;
    evidenceTaskId?: string;
    evidenceCommit?: string;
    proofClass?: string;
    fingerprint: string;
  };
  supersessionEvidence?: {
    class: 'content_subsumed' | 'patch_id_match' | string;
    commit: string;
    coveredByTaskId?: string;
    branchTip: string;
    headSha: string;
    probedAt: string;
  };
  supersessionVeto?: { at: string; branchTip: string };
  recovery?: WorkflowRecoverySummary;
  proposals?: WorkflowProposal[];
  // Phase-2 guardian audit: replay-derived action records the task detail pane
  // renders and can reverse.
  throughputGuardianActions?: WorkflowGuardianAction[];
  decision?: WorkflowDecision;
  fallbackDecision?: Pick<WorkflowFallbackDecision, 'id'|'role'|'state'|'reason'|'from'|'to'>;
  triageEligible?: boolean;
  triageSummary?: WorkflowTriageSummary;
  requiresRelease?: boolean;
  // Landed-work provenance: the shipped sweep's summary and the release the
  // task went out in. An empty shippedInRelease on a landed task means it is
  // merged but not yet published.
  shippedSummary?: WorkflowShippedSummary;
  shippedInRelease?: string;
  landWaiting?: string;
  resourceWaiting?: string;
  waitingFor?: string;
  // Why a live task is not progressing: running | runnable | capacity_queued
  // | dependency_blocked | overlap_serialized | awaiting_approval |
  // quota_paused | budget_paused | landing_wait | resource_wait | verify_queued. Empty for terminal tasks.
  queueState?: string;
	verifyQueueAhead?: number;
	verifyQueueEstimateMin?: number;
  // Screenshots were requested but the passing verification round produced
  // none — the approval gate shows a loud warning.
  screensMissing?: boolean;
  captureOutcome?: WorkflowScreenshotCaptureOutcome;
  browserDecision?: WorkflowBrowserDecision;
  browserVerdict?: WorkflowBrowserVerdictSummary;
  browserVerdictStale?: boolean;
  browserAttention?: boolean;
  blockedBy?: string[];
  dependsOn?: string[];
  // Refines failureKind=timeout: a hang vs the absolute cap elapsing.
  timeoutKind?: 'idle_timeout' | 'hard_timeout';
  approval?: WorkflowApproval;
  preauth?: WorkflowPreauthState;
  autonomyWait?: AutonomyWait;
  campaignId?: string;
  proposedCampaignId?: string;
  composedOf?: string[];
  milestones?: WorkflowMilestone[];
  acceptanceCriteria?: string[];
  compositeConfirmationId?: string;
  // Submit-time "this looks like an existing task" note. Advisory only: it
  // never blocked or changed the submission, and a human can dismiss it.
  similarityAdvisory?: WorkflowSimilarityAdvisory;
  optimisticStart?: WorkflowOptimisticStart;
  dependencyAdvisory?: WorkflowDependencyAdvisory;
  dependencyReleases?: WorkflowDependencyRelease[];
  blockedBehind?: number;
  rootBlockers?: string[];
  stops?: WorkflowTaskStop[];
  // Derived read-model fields (daemon workflow_summary.go), computed from the
  // in-memory task snapshot for the list endpoint only. All optional: absent
  // means unknown (legacy/pre-metadata records), never zero.
  startedAt?: string;
  endedAt?: string;
  stagesDone?: number;
  stagesTotal?: number;
  approvalRequestedAt?: string;
  stageStartedAt?: string;
  idleSec?: number;
  lastOutputAt?: string;
  invocationCount?: number;
  repairCount?: number;
  activeRole?: string;
  activeRuntime?: string;
  activeModel?: string;
  activeSince?: string;
  tokensIn?: number;
  tokensOut?: number;
  tokensKnown?: number;
  estimatedCostUsd?: number;
  costKnown?: number;
  branch?: string;
  approvalDecision?: string;
  planGate?: boolean;
  judgeVerdict?: string;
  verifyState?: string;
  explanation?: string;
}

export interface WorkflowEnvironmentWait {
  stage?: string;
  kind?: 'host_disk' | 'resource_admission' | 'runtime_binary_missing' | 'runtime_node_missing' | string;
  blocker?: string;
  fix?: string;
  wakeCondition?: string;
  runtime?: string;
  runtimeDetail?: string;
  admissionClass?: string;
  resourceConditions?: string[];
  generation?: number;
  since?: string;
  escalateAt?: string;
  probeCount?: number;
  lastProbeAt?: string;
  lastProbeEvidence?: string;
  reparkCount?: number;
  resumedAt?: string;
  waitedMs?: number;
  resumeReason?: string;
  resumeEvidence?: string;
  capabilityChecks?: number;
  capabilityProbe?: WorkflowCapabilityBatch;
}

export interface WorkflowCapabilityBatch {
  id: string;
  stage: string;
  runtime: string;
  requirementDigest: string;
  contextDigest: string;
  worktreeHead?: string;
  startedAt: string;
  deadline: string;
  finishedAt?: string;
  results?: Array<{ name: string; selector: string; status: string; exitCode?: number; errno?: string; evidence?: string; observedAt: string }>;
}

export interface WorkflowSimilarTaskRef {
  taskId: string;
  title: string;
  score: number;
  reason: string;
  // The match already landed on main recently, rather than being in flight —
  // absent on refs recorded before the daemon scored landed tasks at all.
  landed?: boolean;
  // Recently failed/cancelled twin offered for an explicit human refile link.
  failed?: boolean;
}

export interface WorkflowSimilarityAdvisory {
  policyVersion: number;
  similar: WorkflowSimilarTaskRef[];
  dismissed?: boolean;
}

export interface WorkflowOptimisticStart {
  parents: Array<{ taskId: string; tip: string; landOutcome?: string }>;
  base: 'parent_branch' | 'main_merge';
  policy?: string;
  startedAt: string;
}

export interface WorkflowDependencyAdvisory {
  policyVersion: number;
  items: Array<{ dependencyId: string; referenced: boolean; blockedBehind?: number; message?: string }>;
}

export interface WorkflowDependencyRelease {
  dependencyId: string;
  reason: string;
  releasedBy: string;
  agentId?: string;
  runId?: string;
  at: string;
}

export type WorkflowDisposition = 'needs_decision' | 'recovering' | 'waiting' | 'history' | 'superseded' | 'archived' | 'active' | 'done';

export interface WorkflowRecoveryAttempt {
  at: string;
  policy: string;
  action: string;
  reason: string;
  blockerSignature?: string;
  failureClass?: string;
  evidenceSignature?: string;
  evidenceClass?: string;
  failureNames?: string[];
  sourceArtifactId?: string;
  stage?: string;
  requiresHuman: boolean;
  successorTaskId?: string;
}

export interface WorkflowRecoverySummary {
  attempts: number;
  lastAction?: string;
  lastReason?: string;
  lastAt?: string;
  requiresHuman: boolean;
  coarseSignature?: string;
  evidenceSignature?: string;
  evidenceClass?: string;
  failureClass?: string;
  failureNames?: string[];
  diagnosisStatus?: string;
  sourceArtifactId?: string;
  decisionRoute?: 'extend' | 'escalate' | 'retry_recommended';
  decisionReason?: string;
  priorEvidenceOccurrences?: number;
  priorPolicyOccurrences?: number;
  remainingExtensions?: number;
  remainingRepairLoops?: number;
  landReserveAvailable?: boolean;
}

export interface WorkflowProposalEvidence {
  kind: 'artifact' | 'triage_run' | 'diagnosis_doc' | 'supersession_commit' | 'recovery_entry'
    | 'task_snapshot' | 'queue_state' | 'stage_progress' | 'land_lock' | 'land_train' | 'resource_admission' | 'dependency' | 'successor'
    | string;
  ref: string;
  note?: string;
}

export interface WorkflowGuardianRecommendation {
  detectionFingerprint: string;
  class: 'stale_queue_projection' | 'orphaned_landing_lock' | 'orphaned_landing_train' | 'resource_queue_starvation' | 'landed_reconciliation_successor' | 'failed_dependency_live_successor' | 'failure_class_fix_active' | string;
  proposedRemedy: 'review_queue_projection' | 'review_landing_lock' | 'review_landing_train' | 'review_resource_admission' | 'retire_with_fresh_proof' | 'authorized_resume_or_retry' | 'resume_after_active_fix' | string;
  requiredAuthority: 'human_or_operator' | 'fresh_proof_retirement' | 'human_or_orchestrator_authorized_recovery' | 'guardian_durable_proof' | string;
  observedAt: string;
  clearedAt?: string;
  clearReason?: string;
  actionKind?: WorkflowGuardianActionKind;
  proof?: WorkflowGuardianProof;
}

export type WorkflowGuardianActionKind = 'dependency_rearm' | 'active_fix_resume' | 'resolve_reconciliation' | string;

export interface WorkflowGuardianDependencyRewrite {
  originalTaskId: string;
  successorTaskId: string;
  lineageRootId: string;
  eventIds?: string[];
}

export interface WorkflowGuardianProof {
  targetEventSeq: number;
  failureGeneration?: string;
  failedStage?: string;
  failureSignature?: string;
  originalDependencyId?: string;
  successorTaskId?: string;
  lineageRootTaskId?: string;
  lineageEventIds?: string[];
  dependencyRewrites?: WorkflowGuardianDependencyRewrite[];
  reconciliationTaskIds?: string[];
  conflictFingerprint?: string;
  fixTaskId?: string;
  fixCommit?: string;
  fixReleaseEventId?: string;
  fixActivationEventId?: string;
  daemonBuildCommit?: string;
  daemonInstanceId?: string;
  daemonStartGeneration?: string;
  proposalReason?: string;
}

export interface WorkflowGuardianProjection {
  status: string;
  stage?: string;
  error?: string;
  failureKind?: string;
  failureClass?: string;
  dependsOn?: string[];
  blockedBy?: string[];
  budgets: WorkflowBudgets;
  proposals?: WorkflowProposal[];
  supersededBy?: string;
  resolution?: { commit: string; note?: string; at: string; resolvedBy?: string; successorTaskId?: string };
  recovery?: Array<Record<string, unknown>>;
  resumeCount?: number;
  autoResumeCount?: number;
  lastResumedBy?: string;
  lastResumedAt?: string;
  terminalAt?: string;
  archived?: boolean;
  approval?: WorkflowApproval;
}

export type WorkflowGuardianActionState = 'shadowed' | 'proof_rejected' | 'rate_limited' | 'authorized' | 'applied' | 'failed' | 'reverted' | 'revert_blocked';

export interface WorkflowGuardianAction {
  id: string;
  idempotencyKey: string;
  recommendationFingerprint: string;
  class: string;
  actionKind: WorkflowGuardianActionKind;
  targetTaskIds: string[];
  proofEventIds?: string[];
  proofDigest: string;
  proof: WorkflowGuardianProof;
  priorState?: WorkflowGuardianProjection;
  postState?: WorkflowGuardianProjection;
  postStateDigest?: string;
  relatedPostStateDigests?: Record<string, string>;
  mode: 'shadow' | 'governed' | string;
  state: WorkflowGuardianActionState;
  authorizedAt?: string;
  appliedAt?: string;
  revertedAt?: string;
  failure?: string;
  createdAt: string;
  updatedAt: string;
}

export interface WorkflowProposal {
  id: string;
  kind: 'refile_with_context' | 'retire_as_superseded' | 'extend_repair' | 'cancel' | 'needs_human_judgment' | 'quarantine_test' | 'throughput_recommendation';
  status: 'proposed' | 'accepted' | 'auto_executed' | 'dismissed' | 'invalidated';
  rationale: string;
  draftPrompt?: string;
  suggestedBudgets?: Partial<WorkflowBudgets>;
  evidence?: WorkflowProposalEvidence[];
  recommendation?: WorkflowRecommendationEnvelope;
  authorityEvaluations?: WorkflowTrueYoloDecision[];
  fingerprint: string;
  proposedBy: string;
  createdAt: string;
  lastConfirmedAt?: string;
  reaffirmations?: number;
  questionKey?: string;
  decidedAt?: string;
  decidedBy?: string;
  decisionGrant?: string;
  decisionReason?: string;
  executedTaskId?: string;
  autoDecideAt?: string;
  autoState?: 'scheduled' | 'vetoed' | 'cancelled';
  dismissPath?: string;
  guardian?: WorkflowGuardianRecommendation;
  autoDenial?: WorkflowReasonCodeFields;
}

export interface WorkflowDecision {
	kind: 'collaboration' | 'merge_decision' | 'plan_decision' | 'approval' | 'recovery_exhausted' | 'landing_pending' | 'land_escalated' | 'release_blocked' | 'superseded_retire' | 'janitor-cancel' | 'janitor_relevance' | 'engine-closure' | 'proposal-auto' | 'proposal_refile' | 'proposal_retire' | 'proposal_extend_repair' | 'proposal_cancel' | 'proposal_needs_human' | 'fallback_consent' | 'proposal_quarantine' | 'proposal_guardian';
  title: string;
  outcomeSought: string;
  whatHappened: string;
  needToAct: boolean;
  recommendedAction: { label: string; apiPath: string; consequence: string };
  secondaryAction?: { label: string; apiPath: string; consequence: string };
  furtherMergeDecision: boolean;
  taskId: string;
  lineage: string[];
  recoveryAttempts?: WorkflowRecoveryAttempt[];
  prefillCommit?: string;
  prefillNote?: string;
  decideAt?: string;
  proposal?: WorkflowProposal;
  guardian?: WorkflowProposal;
  fallback?: WorkflowFallbackDecision & { answerPath: string; alwaysDiff: {path:string;before:string;after:string;hash:string}; submitTimeDegradedAccepted: boolean };
}

export interface WorkflowTriageSummary {
  action?: string;
  source?: string;
  artifactId?: string;
  error?: string;
  at?: string;
  stale?: boolean;
  resolvedAt?: string;
  detail?: string;
}

export interface TriageRecommendation {
  action: 'reland' | 'rescue-merge' | 'refile' | 'resume' | 'retry' | 'archive-superseded' | 'human';
  reasoning: string;
  confidence?: number;
  successorTaskId?: string;
  resolutions?: Array<{ file: string; sketch: string }>;
  suggestedPrompt?: string;
  suggestedBudgets?: WorkflowBudgets & { maxRefuters?: number };
  humanReason?: string;
}

export interface WorkflowShipCheckEvidence {
  verdict: 'green' | 'yellow' | 'red';
  source?: 'worktree' | 'repo';
  reason?: string;
  runDate?: string;
  flows: { total: number; passed: number; partial: number; failed: number };
  defects?: Array<{ id: string; summary: string }>;
  reportArtifactId?: string;
}

export type WorkflowBrowserMode = 'required' | 'advisory' | 'skipped';
export type WorkflowBrowserVerdict = 'green' | 'yellow' | 'red' | 'infra';

export interface WorkflowBrowserFlowSummary {
  total: number;
  passed: number;
  partial: number;
  failed: number;
  blocked: number;
}

export interface WorkflowBrowserVerdictSummary {
  verdict: WorkflowBrowserVerdict;
  mode: WorkflowBrowserMode;
  reason: string;
  artifactId: string;
  reportArtifactId?: string;
  runArtifactId?: string;
  headSha: string;
  patchId: string;
  contextFreePatchId: string;
  buildDigest: string;
  criteriaVersion: string;
  flows: WorkflowBrowserFlowSummary;
  defects?: Array<{ id: string; flowId: string; summary: string; evidenceRefs?: string[] }>;
  recordedAt: string;
}

export interface WorkflowBrowserDecision {
  mode: WorkflowBrowserMode;
  reasons?: string[];
  matchedPaths?: string[];
  requestedMode?: string;
}

export interface WorkflowApproval {
  requiredBy?: string;
  decisionKind?: string;
  templateContractDigest?: string;
  requestedAt?: string;
  decidedAt?: string;
  decision?: string;
  note?: string;
  judgeVerdict?: string;
  judgeReasoning?: string;
  judgeFocusAreas?: string[];
  planGate?: boolean;
  continued?: boolean;
  decidedBy?: string;
  autoPolicy?: string;
  reGated?: boolean;
  carry?: WorkflowApprovalCarryProvenance;
  batchId?: string;
  patchId?: string;
  shipCheck?: WorkflowShipCheckEvidence;
  browser?: WorkflowBrowserVerdictSummary;
  // Snapshot-derived at a plan gate: the exact stages "continue to
  // implementation" appends. Absent on legacy tasks and outside the gate.
  continuationStages?: string[];
  planDocPath?: string;
}

export interface WorkflowReasonCodeFields {
  code?: string;
  authorityClass?: 'human_only'|'configurable'|'safety_bound'|'external_wait'|'operator_pause'|'engine_wait'|'automation_gap'|'unknown';
  retry?: 'auto'|'manual'|'none';
  reversibility?: 'reversible'|'irreversible';
}

export interface WorkflowPreauthState extends WorkflowReasonCodeFields {
  /** Repository/user-plane class, or gate:<configured-policy-id>. */
  class?: string;
  policyHash?: string;
  scheduledAt?: string;
  decideAt?: string;
  /** ineligible means an active autopilot plane evaluated the task but declined to arm; reason says why. */
  state?: 'scheduled' | 'vetoed' | 'cancelled' | 'approved' | 'superseded_by_human' | 'ineligible';
  reason?: string;
  clearedFloors?: string[];
  source?: 'policy' | 'carry';
  carry?: WorkflowApprovalCarryProvenance;
  vetoSeconds?: number;
}

export interface WorkflowApprovalCarryProvenance {
  rootTaskId: string;
  originalApprovedAt?: string;
  lineageKind: string;
  proofKind: string;
  evidenceDigest: string;
  specDigest: string;
  identicalPaths?: string[];
}

export interface WorkflowScreenshotCaptureOutcome {
  requested: boolean;
  produced: boolean;
  failureCode?: string;
  reason?: string;
  infrastructure?: boolean;
  previewPassed?: boolean;
}

export interface WorkflowTaskStop extends WorkflowReasonCodeFields { plane: 'park'|'approval'|'land'|'dependency'|'proposal'|'triage'|'recovery'|'failure' }

export interface WorkflowBudgets {
  stageTimeoutSec: number;
  maxRepairLoops: number;
  maxInvocations: number;
  maxArtifactRetries: number;
}
// Live queue breakdown: why each non-terminal task is where it is, and how
// full the worker pool really is — so "cap 10 with 1 running and 11 pending"
// explains itself instead of looking broken.
export interface WorkflowStatsQueue {
  cap: number;
  running: number;
  runnable: number;
  capacityQueued: number;
  dependencyBlocked: number;
  overlapSerialized: number;
  awaitingApproval: number;
  quotaPaused: number;
  /** Human-owned per-invocation fallback parks; absent on older daemons. */
  fallbackPaused?: number;
  collaborationPaused?: number;
  /** Typed machine faults waiting on a verified recovery predicate. */
  environmentWait?: number;
  landingWait: number;
  resourceWait: number;
	verifyQueuedTasks?: number;
  heavyCap: number;
  heavyRunning: number;
	heavyCapEff?: number;
	heavyMax?: number;
	heavyLandQueued?: number;
	heavyQueued?: number;
	suiteCap?: number;
	suiteInflight?: number;
	suiteQueueDepth?: number;
	cpuReserved?: number;
	cpuBudget?: number;
	nproc?: number;
	heavyController?: WorkflowHeavyController;
  resourceDeferrals: number;
  memFloorMB: number;
  diskFloorMB: number;
  diskFreeMB: number;
  diskLow: boolean;
  hostDiskKnown?: boolean;
  hostDiskState?: string;
  hostDiskVolume?: string;
  hostDiskFreeMB?: number;
  hostDiskTotalMB?: number;
  hostDiskFloorMB?: number;
  hostDiskLow?: boolean;
  diskBlockingLayer?: 'none' | 'guest' | 'host' | 'both';
  hostDiskDeferrals?: number;
  hostDiskOffender?: string;
  hostDiskUnexplainedLossMB?: number;
  // Effective value plus the precedence layer that produced it, so a stale
  // env override is visible instead of silently governing admission.
  resolvedCaps?: Partial<Record<WorkflowResourceKnob, { value: number; source: WorkflowCapSource }>> & {
	adaptiveHeavy?: { value: boolean; source: WorkflowCapSource };
	costAwareAdmission?: { value: boolean; source: WorkflowCapSource };
  };
  // Measured resource-gate queue time (since daemon boot). Optional because a
  // daemon older than this measurement omits them entirely.
  resourceWaitMemSec?: number;
  resourceWaitDiskSec?: number;
  resourceWaitHeavySec?: number;
	resourceWaitCostSec?: number;
	costAware?: boolean;
	costAwareActive?: boolean;
	costAwareInactiveReason?: string;
	heavyReservedMB?: number;
	heavyHeadroomMB?: number;
	heavyWeights?: WorkflowStatsHeavyWeight[];
  // All causes over the last hour.
  resourceWaitRecentSec?: number;
  utilization: number;
}

export interface WorkflowStatsHeavyWeight {
  repo: string;
  command: string;
  estimateMB: number;
  samples: number;
  updatedAt: string;
  estimateCPU?: number;
  cpuSamples?: number;
  ageSec?: number;
}

export type SystemLevel = 'ok' | 'warn' | 'critical' | 'unknown';
export interface SystemReason { key: string; level: SystemLevel; text: string }
export interface SystemProcessCost { procs: number; rssMB: number | null; cpuPct: number | null }
export interface SystemPaneRow extends SystemProcessCost {
  id: string; title: string; agent: string; kind: string; role: string; status: string; state: string;
  pid: number; agentRssMB: number | null;
}
export interface SystemWorkerRow extends SystemProcessCost {
  taskId: string; taskTitle: string; stage: string; role: string; runtime: string; pid: number; startedAt: string;
}
export type SystemStorageProtection = 'managed' | 'read-only' | 'protected';
export type SystemCleanupOwner = 'none' | 'workflow_gc' | 'retention' | 'go_cache_janitor' | 'generated_storage_janitor';
export interface SystemDiskBreakdownRow {
  name: string; bytes: number; complete: boolean;
  label?: string; category?: string; apparentBytes?: number; allocatedBytes?: number | null;
  measured?: boolean; protection?: SystemStorageProtection; cleanup?: SystemCleanupOwner;
  targetCount?: number; scannedAt?: string | null; reasons?: string[];
}
export interface SystemGrowthDelta {
  category: string; label: string; baselineBytes: number; latestBytes: number; deltaBytes: number; noise: boolean;
}
export interface SystemGrowthWindow {
  window: string; available: boolean; continuity: 'ok' | 'no_baseline' | 'stale' | 'gap' | 'reboot' | 'incomplete';
  baselineAt?: string | null; latestAt?: string | null; reason?: string; deltas?: SystemGrowthDelta[];
}
export interface SystemDiskGrowth { day: SystemGrowthWindow; week: SystemGrowthWindow }
export interface SystemCleanupSkip { reason: string; count: number }
export interface SystemCleanupOperation {
  at: string; kind: string; source?: string; layer: 'guest' | 'host'; executed: boolean; dryRun: boolean;
  complete: boolean; verified: boolean; apparentBytes?: number | null; allocatedBytes?: number | null;
  reclaimedBytes?: number | null; estimatedBytes?: number | null;
  guestFreeBeforeBytes?: number | null; guestFreeAfterBytes?: number | null; guestFreeDeltaBytes?: number | null;
  hostFreeBeforeBytes?: number | null; hostFreeAfterBytes?: number | null;
  vhdxBeforeBytes?: number | null; vhdxAfterBytes?: number | null;
  skipped?: SystemCleanupSkip[]; detail?: string;
}
export interface SystemCleanupSummary {
  scannedAt?: string | null; complete: boolean;
  reclaimableBytes?: number | null; reclaimableTmpBytes?: number | null; reclaimableWorktreeBytes?: number | null;
  reclaimedBytes?: number | null; previewOnly: boolean; scope: string;
  operations: SystemCleanupOperation[]; skips?: SystemCleanupSkip[];
  state?: string; message?: string;
}
export interface SystemHostDisk {
  available: boolean; state: string; detail: string; volume: string; distro: string;
  freeMB: number | null; totalMB: number | null; freeBytes?: number | null; totalBytes?: number | null;
  vhdxAllocatedBytes?: number | null; vhdxApparentBytes?: number | null; dumpBytes?: number | null;
  offenderScannedAt?: string | null;
  floorMB: number | null; floorPct: number;
  hysteresisMB: number; low: boolean; vhdxPath: string; vhdxPhysicalMB: number | null;
  vhdxLogicalMB: number | null; sparse: boolean | null; sampledAt: string | null; runbook: string;
  unexplainedLossMB?: number | null; vhdxGrowthMB?: number | null; lossWindowMin?: number;
  offender?: { label: string; path: string; mb: number; files: number; dumps: number; complete: boolean; text: string } | null;
}
export interface SystemRuntime {
  id: string; provider: string; status: string; cooldownUntilEpoch?: number; installed: boolean | null;
  lastInvocation?: { at?: string; outcome?: string; failureKind?: string } | null;
}
export interface SystemStatus {
  generatedAt: string; sampledAt: string; treeSampledAt: string; sampleIntervalSec: number;
  platform: { os: string; procfs: boolean };
  level: { overall: SystemLevel; reason?: string; reasons: SystemReason[]; areas: Record<string, SystemLevel> };
  daemon: { version: string; pid: number; uptimeSec: number; startedAt: string; rssMB: number | null; goHeapMB: number | null; goRuntimeMB?: number | null; guardUsedMB?: number | null; goroutines: number | null; cpuPct: number | null; memGuardMB: number; webVersion: string; backendID: string; shellVersion: string };
  host: { cores: number; cpuPct: number | null; loadAvg1: number | null; memTotalMB: number | null; memAvailableMB: number | null; memFloorMB: number; provenance?: string; pressure?: WorkflowPressure | null; windows?: { status: string; freeMB?: number | null; totalMB?: number | null; sampledAt?: string | null; ageSec?: number; error?: string; provenance?: string } | null };
  footprint: { totalRssMB: number | null; daemonRssMB: number | null; panesRssMB: number | null; workersRssMB: number | null; paneCount: number; workerCount: number; procs: number; cpuPct: number | null; complete: boolean };
  panes: SystemPaneRow[]; workers: SystemWorkerRow[];
  disk: { layer: 'local' | 'guest'; blockingLayer: 'none' | 'guest' | 'host' | 'both'; anyLow: boolean; host: SystemHostDisk | null; path: string; freeMB: number | null; totalMB: number | null; freeBytes?: number | null; usedBytes?: number | null; totalBytes?: number | null; growth?: SystemDiskGrowth | null; cleanup?: SystemCleanupSummary | null; floorMB: number; low: boolean; measuredAt: string | null; breakdown: SystemDiskBreakdownRow[] | null; breakdownAt: string | null; measuring: boolean; fresh: boolean; nextMeasureAfter: string | null; goCacheBytes: number | null; worktreesBytes: number | null };
  engine: { available: boolean; queue: WorkflowStatsQueue; runningInvocations: number; decide: number; gcLastSweepAt: string | null };
  runtimes: SystemRuntime[] | null;
  automation: {
    agents: { total: number; paused: number; running: number; blocked: number; errors24h: number; nextRunAt: string | null };
    schedules: { enabled: number; disabled: number; running: number; errors24h: number; nextFireAt: string | null };
    builtins: Array<{ id: string; label: string; on: boolean; detail?: string; present?: boolean; running?: boolean }>;
    autonomy: { globalPaused: boolean; pausedCount: number };
    warden: { enabled: boolean; lastRunAt: string | null; lastRunStatus: string };
  };
  surface: { addr: string; ports: { total: number; paneOwned: number }; previews: { active: number }; cloud: { configured: boolean; hasCreds: boolean; credsInvalid: boolean; relayState: string } };
  release: { running: string; latest: string; available: boolean; applying: boolean; lastSuccessMs: number; lastError: string; shellCurrent: string; shellLatest: string; shellAvailable: boolean; workflowDeferState?: 'draining' | 'backoff'; workflowNextRetryMs?: number; workflowInflight?: number; workflowDeferAttempts?: number; readiness: { state: string; unreleasedTotal: number; functionalCommitCount: number; oldestFunctionalUnreleasedAt?: string; lastReleaseVersion: string } | null; train?: { enabled: boolean; phase: string } };
}
export type WorkflowResourceKnob = 'maxConcurrentTasks' | 'maxHeavyOps' | 'maxSuites' | 'maxAgentRuns' | 'reservedBookkeepingRuns' | 'memFloorMB' | 'diskFloorMB' | 'hostDiskFloorMB' | 'hostDiskFloorPct' | 'hostDiskHysteresisMB' | 'browserHostReserveMB';
export type WorkflowCapSource = 'settings' | 'env' | 'adaptive' | 'controller' | 'settings-floor';
export interface WorkflowPressureHost { status: string; freeMB?: number | null; totalMB?: number | null; ageSec?: number; provenance?: string; error?: string }
export interface WorkflowPressure {
  at: string; memAvailableMB?: number | null; memTotalMB?: number | null; diskFreeMB?: number | null;
  memPSISome10?: number | null; memPSISome60?: number | null; memPSIFull10?: number | null; memPSIFull60?: number | null;
  cpuPSISome60?: number | null; ioPSIFull60?: number | null; swapOutPerSec?: number | null; oomKillDelta?: number | null;
  loadAvg1?: number | null; nproc?: number; provenance?: string; host?: WorkflowPressureHost;
}
export interface WorkflowHeavyController {
  mode: 'shadow' | 'governed' | 'frozen' | 'pinned' | 'unavailable'; phase: string; capEff: number; wouldCap?: number; heavyMax: number;
  absoluteMax?: number; advisoryMax?: number; desiredCap?: number; capacityState?: string;
  staticCap: number; staticSource: WorkflowCapSource;
  /** Machine-derived dynamic governed ceiling (formerly fixed at 4). */
  governedMax: number; reason: string; frozen: boolean; frozenSource?: string;
  pinned: boolean; pinnedSource?: string; greenSec: number; redSec: number; cap4Eligible: boolean;
  shadowObservedSec: number; shadowRequiredSec: number; governedEvidenceSec: number; governedRequiredSec: number;
  nextCap?: number; nextCapEligible?: boolean; nextCapBlockedBy?: string;
  nextCapProgress?: { saturatedSec: number; requiredSaturatedSec: number; successes: number; requiredSuccesses: number };
  demand?: { heavyInUse: number; heavyQueued: number; saturated: boolean };
  throughput?: { capEff: number; prevCap: number; ratePerHour: number; prevRatePerHour: number; p50Ms: number; prevP50Ms: number; samples: number; prevSamples: number; comparable: boolean };
  lastPromotionReason?: string; lastRollbackReason?: string; overrideActive?: boolean; overrideSource?: string;
  floorCap?: number; floorActive?: boolean;
  capEvidence?: Array<{ cap: number; saturatedGreenSec: number; busySec: number; successes: number; completions?: number; admissions: number; waitedAdmissions: number; heavyWaitMs: number; p50Ms?: number; ratePerHour?: number; retryHoldUntil?: string | null }>;
  rollbackHoldUntil?: string | null; rollbackHoldKind?: string; lastRampAt?: string | null;
  ramps?: Array<{ at: string; from: number; to: number; reason: string; phase: string; shadow?: boolean }>;
  rollbacks?: Array<{ at: string; from: number; to: number; reason: string; kind?: string; evidence?: string[]; phase: string }>;
  pressure?: WorkflowPressure | null; pressureSampledAt?: string | null; pressureHistory?: WorkflowPressure[];
  evaluation?: { buckets: Array<{ capEff: number; admissions: number; waited: number; waitedShare: number; heavyWaitSec: number; landAdmissions: number; landWaitSec: number; landMaxWaitSec: number }>; shadowProposals?: Array<{ wouldCap: number; admissions: number }> };
}
export interface HostStoragePayload {
  available: boolean;
  state?: string;
  sample?: SystemHostDisk;
  policy?: Record<'floorMB' | 'floorPct' | 'hysteresisMB' | 'browserReserveMB', { value: number; source: WorkflowCapSource }>;
  pressure?: { guestFreeMB: number; guestFloorMB: number; guestLow: boolean; hostLow: boolean; blockingLayer: string; unexplainedLossMB?: number; offender?: string };
  events?: Array<Record<string, unknown> & { at: string; kind: string; source?: string }>;
  capacitySamples?: Array<Record<string, unknown> & { at: string; kind: string; source?: string }>;
  operations?: SystemCleanupOperation[];
}

export type PortList = Port[] & {warning?: string};
async function decodePorts(response: Response): Promise<PortList> {
  const ports = await j<Port[]>(response);
  const warning = response.headers.get('X-Solu-Ports-Warning') || undefined;
  return Object.assign(ports,{warning});
}

export async function j<T>(r: Response): Promise<T> {
  if (!r.ok) {
    const bodyText = (await r.text()).trim();
    let body: unknown;
    if (bodyText) {
      try {
        body = JSON.parse(bodyText);
      } catch {
        // Plain-text error response from an older endpoint.
      }
    }
    // For user errors (4xx) prefer the server's message alone — it's already
    // human-readable. Only prepend the HTTP status for 5xx.
    if (r.status >= 400 && r.status < 500 && bodyText) throw new ApiError(r.status, bodyText, body);
    throw new ApiError(r.status, `${r.status} ${r.statusText}${bodyText ? ': ' + bodyText : ''}`, body);
  }
  if (r.status === 204) return undefined as T;
  return r.json() as Promise<T>;
}

// A stalled relay can hang a GET/POST indefinitely — no TCP error arrives for a
// long time, so whatever awaits the fetch hangs with it. The most painful
// victim was the workspace switch, which wedged FOREVER on the autosave POST /
// listPanes in its await chain (global switching + Ctrl+Shift+R dead, but
// already-open pane WebSockets kept working). All streaming goes through
// EventSource, so every apiFetch is a bounded request/response — cap it so a
// stall rejects with a readable timeout instead of hanging. Callers that own
// their own AbortController (e.g. usageFetch) pass a signal and opt out of this default.
const API_FETCH_TIMEOUT_MS = 30_000;
// POST /api/update/apply-shell: the daemon's installer download may take up to
// 15 minutes (daemon/updater_download.go) and its launch check about 30 s more.
export const SHELL_APPLY_TIMEOUT_MS = 16 * 60_000;

async function fetchBounded(url: string, init: RequestInit, headers: Headers): Promise<Response> {
  const requestPrefix = prefix;
  const requestToken = token;
  const machineScoped = requestPrefix !== '' && url.startsWith(requestPrefix);
  if (machineScoped && !shouldAllowMachineRequest()) {
    throw new ApiError(401, 'no access to this machine (client-blocked)');
  }

  let response: Response;
  // Respect a caller-supplied signal — it already manages abort/timeout.
  // Enrolled machines with the private relay channel turned on go through it
  // (lib/privateRelay); everything else, and any channel that cannot open,
  // takes the ordinary fetch.
  const send = (u: string, i: RequestInit) => (machineScoped ? relayFetch(u, i) : fetch(u, i));
  if (init.signal) {
    response = await send(url, { ...init, headers });
  } else {
    const ctrl = new AbortController();
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      ctrl.abort();
    }, API_FETCH_TIMEOUT_MS);
    try {
      response = await send(url, { ...init, headers, signal: ctrl.signal });
    } catch (error: unknown) {
      if (timedOut && error instanceof DOMException && error.name === 'AbortError') {
        throw new ApiError(0, 'request timed out');
      }
      throw error;
    } finally {
      clearTimeout(timer);
    }
  }

  // The daemon refusing this device (approved devices, daemon
  // trusted_devices.go) is not an account problem: ask the person to approve
  // it instead of marking the machine unreachable.
  if (machineScoped && response.status === 401 && await isDeviceNotApproved(response)) {
    const routed = /^(\/m\/[^/]+)(\/b\/[^/]+)?/.exec(new URL(url, location.href).pathname);
    const target = routed ? relayTarget(routed[1], routed[2] ?? '') : null;
    if (target) requestDeviceApproval(target);
    return response;
  }
  // Ignore a response from an access context the user switched away from
  // while it was in flight. configureApi already reset the new context.
  if (machineScoped && prefix === requestPrefix && token === requestToken) {
    await reportMachineScopeStatus(response.status, {
      machineId: requestPrefix.slice('/m/'.length),
      token: requestToken,
    });
  }
  return response;
}

/** Fired when a machine's daemon refuses this device until it is approved. */
export const DEVICE_APPROVAL_EVENT = 'soluterminal:device-approval-needed';

// The refusal can come before the approval prompt has mounted (the first load
// after a reload), so the last one is also kept for the prompt to pick up.
let pendingDeviceApproval: string | null = null;

/** Ask the person to approve this device on target (the approval prompt). */
export function requestDeviceApproval(target: string): void {
  pendingDeviceApproval = target;
  window.dispatchEvent(new CustomEvent(DEVICE_APPROVAL_EVENT, { detail: { target } }));
}

/** The machine a refusal asked this device to be approved on, if any; cleared once read. */
export function takePendingDeviceApproval(): string | null {
  const target = pendingDeviceApproval;
  pendingDeviceApproval = null;
  return target;
}

async function isDeviceNotApproved(response: Response): Promise<boolean> {
  try {
    const body = (await response.clone().json()) as { error?: string };
    return body?.error === 'device_not_approved';
  } catch {
    return false;
  }
}

// Approved devices: a relayed request to a machine this device has enrolled
// with carries a signature over the target, method, path, canonical query,
// time, nonce and body hash. Local requests and unenrolled machines are
// unchanged. A signing failure leaves the request unsigned; the daemon decides.
// The daemon checks the decoded path (Go's r.URL.Path), so that is what is
// signed: /api/workspaces/My%20Work and /api/workspaces/My Work sign alike.
function decodedPath(path: string): string {
  try {
    return decodeURIComponent(path);
  } catch {
    return path;
  }
}

// The cloud drops these from the query before forwarding (cloud/proxy.go), so
// the daemon never sees them and they cannot be part of the signature.
function cloudForwardedQuery(daemonPath: string, rawQuery: string): string {
  if (!rawQuery) return '';
  const q = new URLSearchParams(rawQuery);
  q.delete('ticket');
  if (!daemonPath.startsWith('/api/proxy/')) q.delete('access_token');
  return q.toString();
}

function relaySigningTarget(scopePrefix: string, scopeBackendPrefix: string): string | null {
  const target = relayTarget(scopePrefix, scopeBackendPrefix);
  if (!target || !deviceEnrolledFor(target) || typeof crypto === 'undefined' || !crypto.subtle) return null;
  return target;
}

async function signRelayRequest(
  target: string,
  path: string,
  init: RequestInit,
  headers: Headers,
): Promise<RequestInit> {
  const q = path.indexOf('?');
  const daemonPath = q >= 0 ? path.slice(0, q) : path;
  const rawQuery = cloudForwardedQuery(daemonPath, q >= 0 ? path.slice(q + 1) : '');
  let body = new Uint8Array();
  let signedInit = init;
  if (init.body != null) {
    if (typeof init.body === 'string') {
      body = new TextEncoder().encode(init.body);
    } else {
      // FormData, Blob, URLSearchParams…: serialise once so the bytes sent are
      // the bytes signed, keeping the content type the browser chose.
      const req = new Request('http://local.invalid/', { method: init.method ?? 'POST', body: init.body, headers });
      body = new Uint8Array(await req.arrayBuffer());
      const type = req.headers.get('content-type');
      if (type && !headers.has('content-type')) headers.set('content-type', type);
      signedInit = { ...init, body };
    }
  }
  try {
    const sig = await signDeviceRequest(target, init.method ?? 'GET', decodedPath(daemonPath), rawQuery, body);
    for (const [k, v] of Object.entries(deviceSignatureHeaders(sig))) headers.set(k, v);
  } catch {
    // Unsigned: an "any devices" daemon accepts it; an "approved" one asks.
  }
  return signedInit;
}

/**
 * Sign a WebSocket/EventSource URL for an enrolled machine (they cannot set
 * headers). Returns the URL unchanged for local routing or unenrolled machines.
 */
export async function signStreamUrl(url: string, scope?: RoutingScope): Promise<string> {
  const scopePrefix = scope?.prefix ?? prefix;
  const scopeBackendPrefix = scope?.backendPrefix ?? backendPrefix;
  const target = relaySigningTarget(scopePrefix, scopeBackendPrefix);
  if (!target) return url;
  try {
    const u = new URL(url, location.href);
    const routed = scopePrefix + scopeBackendPrefix;
    const daemonPath = u.pathname.startsWith(routed) ? u.pathname.slice(routed.length) : u.pathname;
    const sig = await signDeviceRequest(target, 'GET', decodedPath(daemonPath), cloudForwardedQuery(daemonPath, u.search.replace(/^\?/, '')), new Uint8Array());
    return withDeviceSignatureQuery(url, sig);
  } catch {
    return url;
  }
}

/** Whether streams for scope need signing (an enrolled relayed machine). */
export function streamSigningNeeded(scope?: RoutingScope): boolean {
  return relaySigningTarget(scope?.prefix ?? prefix, scope?.backendPrefix ?? backendPrefix) !== null;
}

export function apiFetch(path: string, init?: RequestInit): Promise<Response> {
  const headers = new Headers(init?.headers);
  if (token) headers.set('Authorization', `Bearer ${token}`);
  const requestPrefix = prefix;
  const requestBackendPrefix = backendPrefix;
  const target = relaySigningTarget(requestPrefix, requestBackendPrefix);
  // Unenrolled and local requests take the same synchronous path as always.
  if (!target) return fetchBounded(requestPrefix + requestBackendPrefix + path, init ?? {}, headers);
  return signRelayRequest(target, path, init ?? {}, headers)
    .then((signedInit) => fetchBounded(requestPrefix + requestBackendPrefix + path, signedInit, headers));
}

export function retryMachineAccess(): Promise<Response> {
  forceProbeNow();
  return apiFetch('/api/health');
}

export function noteUiEventsSseFailure(): void {
  if (prefix === '' || getMachineAccessState().blocked || uiEventsSseProbeInFlight) return;
  const now = typeof performance !== 'undefined' && typeof performance.now === 'function'
    ? performance.now()
    : Date.now();
  if (uiEventsSseProbeAt !== null && now - uiEventsSseProbeAt < UI_EVENTS_SSE_PROBE_MIN_INTERVAL_MS) return;
  uiEventsSseProbeAt = now;
  uiEventsSseProbeInFlight = true;
  void apiFetch('/api/health')
    .catch(() => {})
    .finally(() => { uiEventsSseProbeInFlight = false; });
}

// Fetch pinned to a captured RoutingScope: refuses before any network I/O when
// routing has moved on (the request would silently hit a different machine),
// and builds the URL from the captured values, never the mutable module vars.
function apiFetchScoped(scope: RoutingScope, path: string, init?: RequestInit): Promise<Response> {
  if (!routingPrincipalAllowed() || scope.key !== currentRoutingScope().key) {
    // Machine-readable code, not just prose: this refusal happens BEFORE any
    // network I/O, so it is the one approval failure the daemon can never
    // report. The Tasks UI branches on it to render specific, persistent
    // guidance instead of a transient toast that a human can miss entirely.
    return Promise.reject(new ApiError(
      0,
      'This task was loaded from a different machine or backend than the one now selected. Reopen it from the current Tasks list to act on it.',
      { code: WORKFLOW_SCOPE_MISMATCH },
    ));
  }
  const headers = new Headers(init?.headers);
  if (token) headers.set('Authorization', `Bearer ${token}`);
  const target = relaySigningTarget(scope.prefix, scope.backendPrefix);
  if (!target) return fetchBounded(scope.prefix + scope.backendPrefix + path, init ?? {}, headers);
  return signRelayRequest(target, path, init ?? {}, headers)
    .then((signedInit) => fetchBounded(scope.prefix + scope.backendPrefix + path, signedInit, headers));
}

// Workflow task mutations take an optional captured scope (the Tasks UI pins
// them to the snapshot the user is looking at); non-UI callers omit it and get
// the plain current-routing apiFetch.
export function workflowFetch(scope: RoutingScope | undefined, path: string, init?: RequestInit): Promise<Response> {
  return scope ? apiFetchScoped(scope, path, init) : apiFetch(path, init);
}

// ── Resilient GET for the cc-usage calls ────────────────────────────────────
// The usage path is web → daemon → cloud over the relay; a single transient
// 502/503/504 (relay reconnect window, a slow cloud query) used to become a
// permanent error wall because j()/apiFetch had no timeout/retry. This wrapper
// gives those reads a bounded timeout per attempt + a short retry/backoff on
// the transient class only. 4xx (notably 403 — the member-detail privacy
// boundary) still throws immediately so callers' status branching is intact.
//
// Contract assumed of the daemon/cloud side:
//   • transient failures surface as 502 / 503 / 504 (we retry those);
//   • a served-stale snapshot returns 200 + header `X-Solu-Usage-Stale: 1`
//     (we flag the parsed body with `_stale: true`);
//   • an optional `Retry-After` header (seconds) hints the backoff (capped).

// Per-attempt request timeout. The relay can stall a GET for a while during a
// reconnect; 20s is generous enough to ride a brief stall but short enough to
// fail over to a retry instead of hanging the spinner forever.
const USAGE_TIMEOUT_MS = 20_000;
// Backoff before attempts 2/3/4. Three retries (four total attempts) covers a
// typical relay reconnect window without dragging a genuinely-down backend.
const USAGE_BACKOFF_MS = [500, 2_000, 5_000];
// Cap on a server-supplied Retry-After so a bogus/huge value can't wedge the UI.
const USAGE_RETRY_AFTER_CAP_MS = 10_000;
// HTTP statuses we treat as transient (worth a retry). Everything else —
// success, 4xx, other 5xx — resolves/throws on the first attempt.
const USAGE_RETRYABLE_STATUS = new Set([502, 503, 504]);

const sleep = (ms: number) => new Promise<void>((res) => setTimeout(res, ms));

// sleep that rejects early with an AbortError when `signal` aborts, so the retry
// backoff of an abandoned fetch (caller unmounted / scope switched) doesn't run
// to completion before the next attempt's abort check stops the loop.
function abortableSleep(ms: number, signal?: AbortSignal): Promise<void> {
  if (!signal) return sleep(ms);
  return new Promise<void>((resolve, reject) => {
    if (signal.aborted) return reject(new DOMException('aborted', 'AbortError'));
    const onAbort = () => {
      clearTimeout(t);
      reject(new DOMException('aborted', 'AbortError'));
    };
    const t = setTimeout(() => {
      signal.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    signal.addEventListener('abort', onAbort, { once: true });
  });
}

// Parse a Retry-After header (seconds form only; the cloud emits an int) into a
// capped millisecond delay, or null if absent/unparseable.
function parseRetryAfterMs(h: string | null): number | null {
  if (!h) return null;
  const secs = Number(h.trim());
  if (!Number.isFinite(secs) || secs < 0) return null;
  return Math.min(secs * 1000, USAGE_RETRY_AFTER_CAP_MS);
}

// Fetch `path` (GET) and decode JSON, retrying the transient class. Reads the
// body INSIDE the loop so every retry gets a fresh, unconsumed Response (j()
// consumes the body, so a single Response can't be re-decoded). On success,
// stamps `_stale` from the X-Solu-Usage-Stale header. Throws ApiError after the
// retries are exhausted (or immediately on a non-retryable status) so callers
// can still show the wall as a last resort.
// noCache (the page's explicit Refresh) asks the daemon to skip its 60s fresh
// cache; its stale fallback on a cloud error still applies.
async function usageFetch<T extends { _stale?: boolean }>(path: string, signal?: AbortSignal, noCache = false): Promise<T> {
  let lastErr: unknown;
  // attempt 0 = initial, then one per backoff entry → 4 attempts total.
  for (let attempt = 0; attempt <= USAGE_BACKOFF_MS.length; attempt++) {
    // A caller that abandoned this fetch (component unmounted / scope switched)
    // aborts the signal: stop immediately rather than running the rest of the
    // retry chain — previously the orphaned chain ran for ~27s and was discarded.
    if (signal?.aborted) throw new DOMException('aborted', 'AbortError');
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), USAGE_TIMEOUT_MS);
    // Link an external abort to the per-attempt controller so an in-flight GET
    // is cancelled mid-request, not just between attempts.
    const onExternalAbort = () => ctrl.abort();
    if (signal) signal.addEventListener('abort', onExternalAbort, { once: true });
    try {
      const r = await apiFetch(path, noCache ? { signal: ctrl.signal, headers: { 'Cache-Control': 'no-cache' } } : { signal: ctrl.signal });
      clearTimeout(timer);
      if (!r.ok) {
        // Non-retryable (4xx and non-transient 5xx): decode + throw now so
        // callers branch on status (403 → privacy fallback) without delay.
        if (!USAGE_RETRYABLE_STATUS.has(r.status)) return (await j<T>(r));
        // Retryable status: build the ApiError for the eventual throw, then
        // either back off (honoring Retry-After) or give up after the last try.
        const body = (await r.text()).trim();
        lastErr = new ApiError(r.status, `${r.status} ${r.statusText}${body ? ': ' + body : ''}`);
        if (attempt === USAGE_BACKOFF_MS.length) break;
        const ra = parseRetryAfterMs(r.headers?.get('Retry-After') ?? null);
        await abortableSleep(ra ?? USAGE_BACKOFF_MS[attempt]!, signal);
        continue;
      }
      const out = await j<T>(r);
      if (out && r.headers?.get('X-Solu-Usage-Stale') === '1') out._stale = true;
      return out;
    } catch (e) {
      clearTimeout(timer);
      // An external abort surfaces as the per-attempt ctrl aborting — stop
      // retrying immediately so we don't keep firing after the caller left.
      if (signal?.aborted) throw new DOMException('aborted', 'AbortError');
      // A non-retryable ApiError thrown by j() (4xx) propagates immediately.
      if (e instanceof ApiError && !USAGE_RETRYABLE_STATUS.has(e.status)) throw e;
      // Network error or abort-timeout: retryable. Keep it and back off / give up.
      lastErr = e;
      if (attempt === USAGE_BACKOFF_MS.length) break;
      await abortableSleep(USAGE_BACKOFF_MS[attempt]!, signal);
    } finally {
      if (signal) signal.removeEventListener('abort', onExternalAbort);
    }
  }
  throw lastErr;
}

// A dashboard does not need real-time precision. Five minutes prevents a page
// reopen from paying the cold aggregation again; the longer stale horizon lets
// the UI keep last-known data visible while a background/network refresh is in
// flight. `ccUsageCached` still refreshes anything beyond the fresh TTL.
const USAGE_CLIENT_FRESH_TTL_MS = 5 * 60_000;
const USAGE_CLIENT_STALE_TTL_MS = 24 * 60 * 60_000;

type CCUsagePayload = { _stale?: boolean; generated_at?: string };

type CCUsageCacheEntry<T extends CCUsagePayload> = {
  value: T;
  fetchedAt: number;
};

type CCUsageInflightEntry = {
  key: string;
  promise: Promise<CCUsagePayload>;
  controller: AbortController;
  subscribers: Set<symbol>;
  generation: number;
};

const ccUsageClientCache = new Map<string, CCUsageCacheEntry<CCUsagePayload>>();
const ccUsageClientInflight = new Map<string, CCUsageInflightEntry>();
const ccUsageClientGeneration = new Map<string, number>();

function ccUsageCacheKey(path: string): string {
  return `${prefix}|${backendPrefix}|${token ? 'auth' : 'anon'}|${path}`;
}

// (Codex provider) Append the provider filter ONLY when it narrows. "all" (the
// default) leaves the path byte-identical to the pre-provider URL, so the client
// cache keys and older clouds behave exactly as before.
function withUsageProvider(path: string, provider?: UsageProvider): string {
  if (!provider || provider === 'all') return path;
  return path + '&provider=' + encodeURIComponent(provider);
}

// (Local-day grouping) The browser's IANA zone, e.g. "Asia/Singapore". The
// cloud buckets days/heatmap cells in this zone and echoes the resolved value
// as `tz`. Guarded: an environment without a usable Intl zone falls back to
// "UTC" (the server's own default), so the request is never malformed.
export function usageTimeZone(): string {
  try {
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    if (typeof tz === 'string' && tz.trim() !== '') return tz;
  } catch {
    // fall through
  }
  return 'UTC';
}

// Appended LAST on every usage path (after any provider filter). The zone is
// part of the path, so it is also part of the client cache key — a zone change
// never serves another zone's days. Older clouds ignore the param.
function withUsageTz(path: string): string {
  return path + '&tz=' + encodeURIComponent(usageTimeZone());
}

export function ccUsagePath(window: string, provider?: UsageProvider): string {
  return withUsageTz(
    withUsageProvider('/api/cc-usage/summary?window=' + encodeURIComponent(window), provider),
  );
}

export function ccUsageTeamPath(window: string, teamId?: string, provider?: UsageProvider): string {
  let path = '/api/cc-usage/team-summary?window=' + encodeURIComponent(window);
  if (teamId) path += '&team_id=' + encodeURIComponent(teamId);
  return withUsageTz(withUsageProvider(path, provider));
}

function ccUsageTeamContextPath(window: string): string {
  return withUsageTz(
    '/api/cc-usage/team-summary?window=' + encodeURIComponent(window) + '&context=1',
  );
}

export function ccUsageMemberPath(
  userId: string,
  window: string,
  teamId?: string,
  provider?: UsageProvider,
): string {
  let path =
    '/api/cc-usage/member-summary?user_id=' +
    encodeURIComponent(userId) +
    '&window=' +
    encodeURIComponent(window);
  if (teamId) path += '&team_id=' + encodeURIComponent(teamId);
  return withUsageTz(withUsageProvider(path, provider));
}

function ccUsagePeek<T extends CCUsagePayload>(path: string, maxAgeMs = USAGE_CLIENT_STALE_TTL_MS): T | null {
  const entry = ccUsageClientCache.get(ccUsageCacheKey(path));
  if (!entry || Date.now() - entry.fetchedAt > maxAgeMs) return null;
  return entry.value as T;
}

function attachCCUsageSubscriber<T extends CCUsagePayload>(
  entry: CCUsageInflightEntry,
  signal?: AbortSignal,
): Promise<T> {
  const subscriber = Symbol('cc-usage-subscriber');
  entry.subscribers.add(subscriber);

  return new Promise<T>((resolve, reject) => {
    let settled = false;
    const cleanup = () => {
      entry.subscribers.delete(subscriber);
      if (signal) signal.removeEventListener('abort', onAbort);
    };
    const onAbort = () => {
      if (settled) return;
      settled = true;
      cleanup();
      if (entry.subscribers.size === 0) {
        entry.controller.abort();
        // Remove immediately, not only in promise.finally: React StrictMode
        // can unsubscribe then resubscribe in the same tick. A new subscriber
        // must start a fresh request rather than attach to the aborted flight.
        if (ccUsageClientInflight.get(entry.key) === entry) {
          ccUsageClientInflight.delete(entry.key);
        }
      }
      reject(new DOMException('aborted', 'AbortError'));
    };

    if (signal?.aborted) {
      onAbort();
      return;
    }
    if (signal) signal.addEventListener('abort', onAbort, { once: true });

    entry.promise.then(
      (value) => {
        if (settled) return;
        settled = true;
        cleanup();
        resolve(value as T);
      },
      (error) => {
        if (settled) return;
        settled = true;
        cleanup();
        reject(error);
      },
    );
  });
}

function ccUsageCached<T extends CCUsagePayload>(
  path: string,
  opts: { force?: boolean; signal?: AbortSignal } = {},
): Promise<T> {
  const key = ccUsageCacheKey(path);
  if (!opts.force) {
    const cached = ccUsagePeek<T>(path, USAGE_CLIENT_FRESH_TTL_MS);
    if (cached) return Promise.resolve(cached);
    const inflight = ccUsageClientInflight.get(key);
    if (inflight) return attachCCUsageSubscriber<T>(inflight, opts.signal);
  }

  const controller = new AbortController();
  const generation = (ccUsageClientGeneration.get(key) ?? 0) + 1;
  ccUsageClientGeneration.set(key, generation);
  const entry: CCUsageInflightEntry = {
    key,
    controller,
    generation,
    subscribers: new Set(),
    promise: Promise.resolve({}), // replaced immediately below
  };
  const pending = usageFetch<T>(path, controller.signal, !!opts.force)
    .then((value) => {
      // A forced refresh may supersede an older request. Only the newest
      // generation may populate the cache; old subscribers still receive their
      // own response, but it can never overwrite newer dashboard state.
      if (ccUsageClientGeneration.get(key) === generation) {
        ccUsageClientCache.set(key, { value, fetchedAt: Date.now() });
      }
      return value;
    })
    .finally(() => {
      if (ccUsageClientInflight.get(key) === entry) ccUsageClientInflight.delete(key);
    });
  entry.promise = pending;
  ccUsageClientInflight.set(key, entry);
  return attachCCUsageSubscriber<T>(entry, opts.signal);
}

function ccUsagePrefetch<T extends CCUsagePayload>(path: string): void {
  if (ccUsagePeek<T>(path, USAGE_CLIENT_FRESH_TTL_MS)) return;
  if (ccUsageClientInflight.has(ccUsageCacheKey(path))) return;
  void ccUsageCached<T>(path).catch(() => {});
}

/**
 * Like apiFetch but explicitly targets a specific backend in router
 * mode. Outside router mode, falls back to the default-origin fetch.
 * Used by the sidebar to fetch each backend's workspace/pane list on
 * startup without disturbing the currently-active backend prefix.
 */
function apiFetchBackend(backend: BackendId, path: string, init?: RequestInit): Promise<Response> {
  const headers = new Headers(init?.headers);
  if (token) headers.set('Authorization', `Bearer ${token}`);
  const target = relaySigningTarget(prefix, `/b/${backend}`);
  if (!target) return fetchBounded(`${prefix}/b/${backend}${path}`, init ?? {}, headers);
  return signRelayRequest(target, path, init ?? {}, headers)
    .then((signedInit) => fetchBounded(`${prefix}/b/${backend}${path}`, signedInit, headers));
}

async function health(): Promise<Health> {
  try {
    return await apiFetch('/api/health').then(j<Health>);
  } catch (err) {
    if (!isRouterMode()) throw err;
    const active = currentBackend() ?? 'wsl';
    const other: BackendId = active === 'wsl' ? 'win' : 'wsl';
    const [activeResult, otherResult] = await Promise.allSettled([
      apiFetchBackend(active, '/api/health').then(j<Health>),
      apiFetchBackend(other, '/api/health').then(j<Health>),
    ]);
    if (activeResult.status === 'fulfilled') return activeResult.value;
    if (otherResult.status === 'fulfilled') return otherResult.value;
    throw err;
  }
}

// Relay tickets (cloud/relay_ticket.go) keep the sign-in token out of URLs
// that cannot carry a header: EventSource streams, navigations and the
// sandboxed dev-server iframe. Only relayed (/m/) URLs need one.
export type RelayTicketKind = 'once' | 'proxy';

export function relayNeedsTicket(url: string): boolean {
  return token !== null && url.startsWith('/m/');
}

// Mint a ticket for an exact relay path ('once': single use, 30 s) or a
// dev-server port scope ('proxy': that port only, ≤1 h). The request carries
// the token in the Authorization header; the URL only ever gets the ticket.
export async function mintRelayTicket(url: string, kind: RelayTicketKind): Promise<string> {
  const path = url.split(/[?#]/, 1)[0];
  const res = await rootFetch('/api/relay/ticket', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path, kind }),
  });
  if (!res.ok) throw new ApiError(res.status, `relay ticket: HTTP ${res.status}`);
  const body = (await res.json()) as { ticket?: unknown };
  if (typeof body.ticket !== 'string' || body.ticket === '') throw new ApiError(res.status, 'relay ticket: empty response');
  return body.ticket;
}

// rootFetch is apiFetch without the per-machine prefix — used for
// endpoints the cloud serves itself (transcription) instead of
// relaying to the user's local daemon. In local mode `prefix` is
// empty so this behaves identically to apiFetch.
export function rootFetch(path: string, init?: RequestInit): Promise<Response> {
  const headers = new Headers(init?.headers);
  if (token) headers.set('Authorization', `Bearer ${token}`);
  return fetch(path, { ...init, headers });
}

// Runtime-tunable PTY enter-gap config served by the daemon (see
// daemon/pty_submit.go + GET /api/enter-gap). claudeMs is the Claude/raw
// body→\r gap (default 400), defaultMs the bracketed-paste gap (default
// 100). Older daemons predate this endpoint (404/error) — callers must keep
// the compiled-in ptySubmit.ts defaults when the fetch fails.
export interface EnterGapConfig {
  claudeMs: number;
  defaultMs: number;
}

// Claude Code CLI bootstrap status, returned by POST /api/setup/claude/recheck
// (and mirrored on GET /api/health). See daemon/claude_bootstrap.go.
export interface ClaudeBootstrapStatus {
  claudeChecked: boolean;
  claudeInstalled: boolean;
  claudeVersion: string;
  claudeAuthed: boolean;
}

// The OS-correct Claude Code installer command, returned by GET
// /api/setup/claude/install-command — spawned in a pane by the web. `display`
// is the human-readable one-liner for disclosure. See daemon/claude_bootstrap.go.
export interface ClaudeInstallSpec {
  supported: boolean;
  cmd: string[];
  display: string;
  note: string;
}

// Muse Code CLI bootstrap status, returned by POST /api/setup/muse/recheck
// (and mirrored on GET /api/health). See daemon/muse_bootstrap.go.
export interface MuseBootstrapStatus {
  museChecked: boolean;
  museInstalled: boolean;
  museVersion: string;
  museAuthed: boolean;
}

// The OS-correct Muse Code installer command, returned by GET
// /api/setup/muse/install-command — spawned in a pane by the web.
export interface MuseInstallSpec {
  supported: boolean;
  cmd: string[];
  display: string;
  note: string;
}

// Codex CLI bootstrap status, returned by POST /api/setup/codex/recheck
// (and mirrored on GET /api/health). See daemon/codex_bootstrap.go. The
// install command has the same shape as Claude's (ClaudeInstallSpec).
export interface CodexBootstrapStatus {
  codexChecked: boolean;
  codexInstalled: boolean;
  codexVersion: string;
  codexAuthed: boolean;
}

// Reporter loop: one of the caller's filed reports with live pipeline status
// (cloud feedback_reports.go via the daemon proxy).
export interface FeedbackReport {
  number: number;
  title: string;
  filedAt: string;
  status: 'new' | 'triaged' | 'needs-info' | 'in-progress' | 'fix-drafted' | 'fixed' | 'unknown';
  ask?: string; // the triage bot's specific request, present when needs-info
}

// Reporter loop: one feedback-pipeline EVENT for the in-app inbox (cloud
// feedback_reports.go handleFeedbackInbox via the daemon proxy). Pure-DB on
// the server, cheap enough to poll for the footer chip.
export interface FeedbackEvent {
  number: number;
  title: string;
  event: 'needs-info' | 'fixed';
  message: string;
  at: string;
  readAt?: string;
}

// Reporter loop: one report's conversation for the inbox thread view.
export interface FeedbackThreadEvent {
  event: 'filed' | 'needs-info' | 'fixed' | 'reply' | string;
  message: string;
  at: string;
}
export interface FeedbackThread {
  number: number;
  title: string;
  filedAt: string;
  events: FeedbackThreadEvent[];
}

// In-app feedback (bug / idea / question). The client sends only intent; the
// daemon gathers + redacts context and forwards to the cloud, which files a
// GitHub issue. See daemon/feedback.go + cloud/feedback.go.
export interface FeedbackAttachment {
  name: string;
  mime: 'image/png' | 'image/jpeg' | 'image/gif' | 'image/webp';
  // RAW base64 (no "data:...;base64," prefix); the daemon measures + forwards it.
  data: string;
}

export interface FeedbackReq {
  type: 'bug' | 'idea' | 'question';
  title: string;
  body: string;
  paneId?: string;
  includeSnapshot?: boolean;
  clientVersion?: string;
  attachments?: FeedbackAttachment[];
}

export interface FeedbackResult {
  ok: boolean;
  number: number;
  url: string;
  // How many attachments the daemon/cloud actually committed vs skipped
  // (bad MIME / oversize / over-count / commit failure are lenient — the
  // text report still files). Optional so older responses still parse.
  attachmentsStored?: number;
  attachmentsFailed?: number;
  // The cloud kept the report and files it once GitHub accepts it.
  queued?: boolean;
}

// ── Agents cockpit (operator-only) ───────────────────────────────────────────
// GET /api/agents/status — on-demand daemon fetch-through of the fix agent's
// local status.json + the cloud's GitHub pipeline summary (see
// daemon/agents_api.go + cloud/agents.go). Non-operators get the empty shape
// ({operator:false}); old daemons lack the endpoint (agentsStatus normalizes
// their SPA-fallback 200 to a 404) — callers must treat both as "hidden",
// never as an error (useAgentsStatus.ts owns that mapping).
// SECURITY: titles/verdict prose/reasons in this payload are UNTRUSTED GitHub
// text — render as text nodes only; `url` fields are never used as hrefs (the
// UI rebuilds links from the integer ids).

// One workflow's latest run, as aggregated by the cloud.
export interface AgentsWorkflowRun {
  ok: boolean;
  conclusion: string;
  // Null when the workflow has never run (conclusion "none").
  ageS: number | null;
  // Present (true) on dispatch-only workflows (investigate.yml) so the UI
  // labels them "manual" instead of implying a scheduled cadence.
  manual?: boolean;
  url: string;
}

// Issue/PR pair the fix agent gave up on (repair-stopped; `fix-stuck` label).
export interface AgentsStuckIssue {
  issue: number;
  title?: string;
  pr?: number;
  ageS?: number;
  url?: string;
}

// One open fix/issue-* PR. `commits > 1` means CI-repair commits landed that
// the adversarial reviewer never saw (the repair loop never re-reviews).
export interface AgentsQueueRow {
  pr: number;
  issue: number;
  // "fix-agent" today; the P3 babysitter adds new values — UI must not assume.
  source: string;
  title: string;
  state: 'draft' | 'ready';
  ci: 'green' | 'red' | 'pending';
  verdictSource: 'marker' | 'prose' | 'ambiguous' | 'none';
  // Omitted by the cloud (not null) when verdictSource is 'ambiguous'/'none'.
  verdict?: {
    recommendation: string;
    ready: boolean;
    confidence: string;
    concerns: string[];
  } | null;
  adds: number;
  dels: number;
  files: number;
  commits: number;
  waitingSince: string;
  url: string;
}

// Cloud GET /api/agents/summary payload, relayed verbatim inside the daemon
// response. Each of workflows/counts/queue is independently nullable with a
// sibling *Error reason so a token-scope gap renders as explicit "unknown",
// never as a green-looking empty section. Non-operator responses carry only
// {operator:false} (empty-shape convention) — every other field may be absent.
export interface CloudAgentsSummary {
  operator: boolean;
  // owner/name slugs for the popover's GitHub links (absent on older clouds).
  sourceRepo?: string;
  feedbackRepo?: string;
  generatedAt?: string | null;
  workflows?: {
    triage: AgentsWorkflowRun;
    projectSync: AgentsWorkflowRun;
    investigate: AgentsWorkflowRun;
  } | null;
  workflowsError?: string | null;
  counts?: {
    needsTriage: number;
    triaged: number;
    rootCaused: number;
    fixDrafted: number;
    // Null when nothing is currently triaged.
    triagedOldestAgeS: number | null;
    // Investigation-eligible backlog (type:bug, not needs-info) — ideas sit
    // at "triaged" forever by design and must not read as demand. Optional:
    // older clouds don't send these.
    triagedBugs?: number;
    triagedBugsOldestAgeS?: number | null;
    countsTruncated: boolean;
  } | null;
  countsError?: string | null;
  stuck?: AgentsStuckIssue[] | null;
  queue?: AgentsQueueRow[] | null;
  queueTruncated?: boolean;
  queueError?: string | null;
  // PR-babysitter reviews across ALL watched repos with LIVE CI + stale flag —
  // the phase-2 "merge-ready across every reviewed PR" surface (vs the local
  // status.json's last-run ~3). Absent on clouds without AGENTS_PR_REPOS.
  prReviews?: AgentsPRReview[] | null;
  prReviewsError?: string | null;
  // Set when the cloud is serving a stale cache after a GitHub error.
  staleAt?: string | null;
  staleReason?: string | null;
}

// One reviewed open PR from the cloud (verdict from the review marker; ci +
// stale are live). mergeReady = approved · CI green · review current · ready.
export interface AgentsPRReview {
  repo: string;
  n: number;
  title: string;
  url: string;
  verdict: 'approve' | 'comment' | 'request-changes' | string;
  confidence: string;
  findings: number;
  ci: 'green' | 'red' | 'pending' | 'none';
  state: 'draft' | 'ready';
  stale: boolean;
  mergeReady: boolean;
}

// Local fix-agent state read from ~/.solu/fix-agent/status.json (+ lock/
// disabled files). Display only — never an input to any privileged decision.
export interface AgentsFixAgent {
  present: boolean;
  running: boolean;
  disabled: boolean;
  result?: 'ok' | 'warn' | 'fail' | 'disabled' | null;
  finishedAt?: string | null;
  ageS?: number | null;
  nextRunAt?: string | null;
  stale?: boolean;
  stuck?: { issue: number; pr?: number; reason?: string }[] | null;
}

// One PR the babysitter handled in its LAST run. `url` is rebuilt by the
// daemon from the validated repo slug + integer PR number.
export interface AgentsPrReview {
  repo: string;
  n: number;
  outcome: 'reviewed' | 'plan' | 'error' | string;
  reason?: string;
  verdict?: 'approve' | 'comment' | 'request-changes' | string;
  confidence?: string;
  findings: number;
  url: string;
}

// Local PR-babysitter state read from ~/.solu/pr-agent/status.json (+ lock/
// disabled files). Display only, no GitHub token involved.
export interface AgentsPrAgent {
  present: boolean;
  running: boolean;
  disabled: boolean;
  mode?: 'apply' | 'dry-run' | null;
  result?: 'ok' | 'warn' | 'fail' | 'disabled' | null;
  finishedAt?: string | null;
  ageS?: number | null;
  nextRunAt?: string | null;
  stale?: boolean;
  counts?: {
    repos: number; openPrs: number; eligible: number; unreviewed: number;
    reviewed: number; skipped: number; errors: number;
  } | null;
  reviews?: AgentsPrReview[] | null;
  // The merge-gate watch: "down" when the reviewer's schedule is enabled but
  // no run has completed for 30 minutes. Absent on older daemons.
  heartbeat?: AgentsPrHeartbeat | null;
}

export interface AgentsPrHeartbeat {
  state: 'off' | 'ok' | 'down';
  lastCompletedAt?: string | null;
  downSince?: string | null;
  detectedAt?: string | null;
  detail?: string;
  scheduleId?: string;
}

export interface AgentsStatus {
  fixAgent: AgentsFixAgent | null;
  // Local PR-babysitter leg; absent on daemons older than v0.0.833.
  prAgent?: AgentsPrAgent | null;
  // Cloud summary, or null when unpaired/403/unreachable.
  cloud: CloudAgentsSummary | null;
  // Stamped once by cloud and propagated VERBATIM through every cache hop —
  // the one honest staleness clock ("checked Xs ago"). Null if cloud null.
  generatedAt: string | null;
  fetchedAt: string;
}

// GitHub App "Connect GitHub" status (daemon proxy → cloud github_app.go).
// configured:false = the cloud has no App registered → the feature is
// invisible (PAT-mode behavior unchanged). accountLogin is UNTRUSTED text
// (render as text nodes only); the manage link is REBUILT client-side from
// installationId + accountType, never taken from the payload.
export interface GithubAppStatus {
  configured: boolean;
  connected: boolean;
  installationId?: number;
  accountLogin?: string;
  accountType?: string;
  suspended?: boolean;
  repos?: string[];
  repoCount?: number;
  // Repo list temporarily unknown (GitHub unreachable) — connection itself
  // still stands.
  reposError?: string;
}

// --- Agent bootstrap (in-app zero-terminal setup for built-in agents) ------
export interface AgentBootstrapEnvField {
  key: string;
  label: string;
  hint?: string;
  secret: boolean;
  required: boolean;
}
export interface AgentBootstrapAgent {
  id: string;
  title: string;
  present: boolean;
  codeSource: 'git' | 'embedded' | 'local' | 'missing';
  envReady: boolean;
  envKeys: string[]; // key NAMES only — values never leave the daemon
  scheduled: boolean;
  scheduleId?: string;
  apply: boolean;
  env: AgentBootstrapEnvField[];
}
export interface AgentsBootstrap {
  supported: boolean;
  reason?: string;
  embeddedCode: boolean;
  checks: { git: boolean; gh: boolean; bwrap: boolean; node: boolean; claudeSubscription?: string };
  agents: AgentBootstrapAgent[];
}
export interface AgentsBootstrapApplyInput {
  agent: string;
  env?: Record<string, string>;
  repos?: { app: string; slug: string }[];
  schedule?: boolean;
}

// --- Autonomy overview (GET /api/autonomy, daemon/autonomy_api.go) ----------
// The complete answer to "what acts on its own here?" — see
// docs/DESIGN-agents-surface.md §1.4. Strictly read-only: every row names the
// surface that OWNS the setting and links there, rather than editing it here.

// Where a row's owner lives. The daemon emits this closed enum and the client
// maps it to navigation, so a row can never carry a bare URL.
export type AutonomyDeepLink =
  | 'settings:warden'
  | 'settings:tasks'
  | 'settings:devices'
  | 'settings:agent'
  | 'sidebar:credentials'
  | 'sidebar:schedules'
  | 'sidebar:agents'
  | `agent-authority:${string}`
  | `repo-file:${string}`
  // A controller whose only switch is the daemon's environment (the memory
  // guard). Shown as plain text, like a checked-in file: there is no screen
  // that could change it, and a link that pretends otherwise is worse.
  | `env:${string}`
  // A PER-PANE setting whose control lives on that pane's context menu (the
  // delegated-work announcements). Text-only for the same reason: there is no
  // single screen this panel could open that would change it.
  | `pane-menu:${string}`;

export interface AutonomyRow {
  id: string;
  label: string;
  on: boolean;
  owner: string;
  deepLink: AutonomyDeepLink;
  // A fail-safe the user CANNOT disable, disclosed inline under the row
  // (today: a required update applies whatever the self-update preference
  // says). Never render this as a toggle — that would be the exact lie the
  // overview exists to prevent.
  alwaysOnNote?: string;
  detail?: string;
  // Built-in automations only.
  present?: boolean;
  running?: boolean;
  // Legacy task schedules only.
  cron?: string;
  code?: string;
  authorityClass?: WorkflowReasonCodeFields['authorityClass'];
}

// --- Managed agents (GET /api/agents, daemon/agents_crud_api.go) -----------
// The named objects the Agents section renders (docs/DESIGN-agents-surface.md
// §9 S2). One authority ships today — "report-only" — and the card says so.

export interface AgentTrigger {
  kind: 'cron' | 'event';
  cron?: string;
  event?: string;
  filter?: string;
  debounceSec?: number;
}

export interface AgentRun {
  at: string;
  status: 'fired' | 'skipped' | 'error' | 'quota_paused' | 'credential_paused';
  detail?: string;
  logFile?: string;
  summary?: string;
  runId?: string;
  triggerContext?: string;
  runtime?: string;
  provider?: string;
  model?: string;
  effort?: string;
  routingSource?: AgentRoutingSource;
  chainIndex?: number;
  fallbackSource?: string;
  fallbackReason?: string;
  fallbackPolicy?: AgentFallbackPolicy;
  account?: string;
  credentialAccounts?: string[];
  billingIdentity?: string;
  billingSource?: string;
  exitStatus?: string;
  resetAt?: string;
  routeAttempts?: AgentRouteAttempt[];
}

export type AgentFallbackPolicy = 'never' | 'quota_only' | 'availability';
export type AgentRoutingSource = 'legacy' | 'template' | 'override';

export interface AgentRuntimeBinding {
  runtime: string;
  model?: string;
  effort?: string;
}

export interface AgentRuntimePolicy {
  primary: AgentRuntimeBinding;
  fallbacks?: AgentRuntimeBinding[];
  fallbackPolicy: AgentFallbackPolicy;
}

export interface AgentRuntimePause {
  kind: 'quota_paused' | 'credential_paused';
  route: AgentRuntimeBinding;
  detail: string;
  resetAt?: string;
  updatedAt: string;
}

export interface AgentRouteAttempt extends AgentRuntimeBinding {
  provider?: string;
  chainIndex: number;
  status: string;
  failureKind?: string;
  detail?: string;
  exitStatus?: string;
  resetAt?: string;
  account?: string;
  quotaScope?: string;
  billingIdentity?: string;
  billingSource?: string;
  credentialAccounts?: string[];
}

export interface AgentRuntimeCapabilities {
  provider?: string;
  billingMode?: string;
  supportsMcp?: boolean;
  supportsSandbox?: boolean;
  supportsToolIsolation?: boolean;
  supportsEffort?: boolean;
  effortLevels?: string[];
  supportsManagedAgents?: boolean;
}

export interface AgentRuntimeRoute extends AgentRuntimeBinding {
  provider?: string;
  capabilities: AgentRuntimeCapabilities;
  ready: boolean;
  readiness?: string;
  billingIdentity?: string;
  billingSource?: string;
}

// One row of the daemon's grant registry (docs/DESIGN-agents-surface.md A.2).
// The UI never carries its own copy of these labels or consequence lines: the
// card and the daemon's enforcement must read the same array, or a chip could
// promise something the daemon does not do.
export interface AgentGrantDef {
  id: string;
  label: string;
  consequence: string;
  tools: string[];
  /** Engine-owned consent; it deliberately mints no agent tools. */
  engine?: boolean;
  caps?: {
    fileTasks?: number;
    resumesPerTask?: number;
    retiresPerRun?: number;
    resolvesPerRun?: number;
    archivesPerRun?: number;
    linkRefilesPerRun?: number;
    proposalsPerRun?: number;
  };
}

// --- Agent templates (GET /api/agents/templates, daemon/agent_templates.go) --
// The five shipped templates (docs/DESIGN-agents-surface.md §4). Daemon-owned
// so the picker's policy text and the text the RUN executes are the same
// string; the client never carries its own copy of either.

/** A grant a template will offer once the work behind it lands — rendered
 *  greyed and non-interactive with its note (§9 S4). It is deliberately NOT in
 *  the grant registry: there is no tool, so there is nothing a toggle could
 *  honestly turn on. */
export interface AgentPendingGrant {
  id: string;
  label: string;
  note: string;
}

/** One version's reviewable content. The Template tab diffs two of these when
 *  a template updates (§4.5). */
export interface AgentTemplateSnapshot {
  version: number;
  policy: string;
  defaultGrants: string[];
  allowedGrants: string[];
  routing?: AgentRuntimePolicy;
}

export interface AgentNotifyShape {
  mode?: string;
  quietHours?: { start?: string; end?: string; tz?: string };
  needsYou?: boolean;
}

export interface AgentTemplate {
  id: string;
  version: number;
  name: string;
  emoji: string;
  role: string;
  policy: string;
  defaultGrants: string[];
  allowedGrants: string[];
  pendingGrants: AgentPendingGrant[];
  triggers: AgentTrigger[];
  notify?: AgentNotifyShape;
  timeoutMin?: number;
  model?: string;
  routing?: AgentRuntimePolicy;
  /** Previous versions, so an agent's ADOPTED version still resolves after a
   *  bump — §4.5's "never auto-migrate" needs the old text to still exist. */
  history: AgentTemplateSnapshot[];
}

export interface AgentTemplatesPayload {
  templates: AgentTemplate[];
  /** agentReadTools() itself — the read list behind the review page's "Looks,
   *  never touches" disclosure (§4.3 step 2). */
  baseReadTools: string[];
  readToolsByTemplate?: Record<string,string[]>;
}

/** The create/PATCH body, mirroring daemon AgentInput: every field optional so
 *  a PATCH can leave one alone. An omitted `grants` must never read as "clear
 *  the grants" — and it is exactly that property that makes "Adopt update"
 *  (which sends only adoptedVersion) structurally unable to widen authority. */
export interface AgentInput {
  name?: string;
  templateId?: string;
  adoptedVersion?: number;
  dismissedVersion?: number;
  forked?: boolean;
  role?: string;
  scope?: { repoPath?: string; repoPaths?: string[]; workspace?: string };
  promptOverride?: string;
  grants?: string[];
  authority?: string;
  triggers?: AgentTrigger[];
  notify?: AgentNotifyShape;
  timeoutMin?: number;
  model?: string;
  routingOverride?: AgentRuntimePolicy;
  paused?: boolean;
}

export interface Agent {
  id: string;
  name: string;
  templateId?: string;
  adoptedVersion?: number;
  /** The template version whose update banner the user dismissed with "Keep
   *  current" (§4.5). The banner returns on the next bump. */
  dismissedVersion?: number;
  forked?: boolean;
  role?: string;
  scope: { repoPath?: string; repoPaths?: string[]; workspace?: string };
  promptOverride?: string;
  // THE array (§2.4): the card's chips and the run's capability token read the
  // same field. Non-empty requires authority 'scoped'; each id resolves in the
  // registry served by api.agentGrants().
  grants: string[];
  authority: string;
  triggers: AgentTrigger[];
  notify?: AgentNotifyShape;
  timeoutMin?: number;
  model?: string;
  routingOverride?: AgentRuntimePolicy;
  runtimePause?: AgentRuntimePause;
  paused: boolean;
  createdAt: string;
  updatedAt: string;
  lastRun?: string;
  counters: { runs: number; actions: number; denied: number };
  scheduleIds?: string[];
  // The task-kind schedule this agent was imported FROM, for authority
  // 'legacy-unrestricted' (§8.3 Phase 1). That schedule is still the trigger
  // and still the run vehicle; this record is its card. Deleting the agent
  // hands the schedule back to the Schedules pane — the migration's rollback.
  sourceScheduleId?: string;
  runs?: AgentRun[];
  // Daemon-computed card fields.
  nextRun?: string;
  running?: boolean;
  // The fail-closed state: this machine's Claude CLI cannot enforce the deny
  // list, so every run is refused. Rendered as a banner — never as an agent
  // that merely happens to be idle.
  blocked?: boolean;
  blockedReason?: string;
  // Built on a workflow-engine template that was removed: it can never run
  // again, so the card offers only Remove.
  retired?: boolean;
  globalPaused?: boolean;
  effectiveReadTools?: string[];
  effectiveRouting?: AgentRuntimePolicy;
  routingSource?: AgentRoutingSource;
  runtimeReady?: boolean;
  runtimeReadiness?: string;
  runtimeRoutes?: AgentRuntimeRoute[];
  runtimeOptions?: AgentRuntimeRoute[];
}

// One row of the per-agent activity feed (docs/DESIGN-agents-surface.md §6.1).
// The daemon merges the action ledger with the run history into one chronology,
// so `kind` says which half of this shape carries the meaning. Artifacts are
// the row's evidence — each one is a deep link the UI must offer.
export interface AgentActivityRow {
  at: string;
  runId?: string;
  kind: 'action' | 'run';
  // Action rows.
  action?: string;
  grant?: string;
  result?: string;
  target?: { taskId?: string };
  artifacts?: {
    taskEventSeq?: number;
    commitSha?: string;
    releaseVersion?: string;
    runLogFile?: string;
  };
  // Run rows.
  status?: string;
  detail?: string;
  summary?: string;
  logFile?: string;
  triggerContext?: string;
  runtime?: string;
  provider?: string;
  model?: string;
  effort?: string;
  routingSource?: AgentRoutingSource;
  chainIndex?: number;
  fallbackSource?: string;
  fallbackReason?: string;
  fallbackPolicy?: AgentFallbackPolicy;
  account?: string;
  credentialAccounts?: string[];
  billingIdentity?: string;
  billingSource?: string;
  exitStatus?: string;
  resetAt?: string;
  routeAttempts?: AgentRouteAttempt[];
}

export interface AgentReport {
  agentId: string;
  agentName: string;
  runId: string;
  logFile?: string;
  at: string;
  title: string;
  preview?: string;
  // Older daemons serialize a zero-item report as null; treat absent/null as empty.
  items?: { taskId: string; decision?: string }[] | null;
  // A built-in agent alert (PR reviewer down, held fixes) carries its own
  // markdown instead of a run log.
  kind?: 'builtin';
  body?: string;
}

// The PATCH body for an agent. Every field optional and omitted-means-unchanged,
// mirroring the daemon's AgentInput: sending a partial must never read as
// "clear everything I left out".
export interface AgentUpdateInput {
  name?: string;
  role?: string;
  promptOverride?: string;
  scope?: { repoPath?: string; repoPaths?: string[]; workspace?: string };
  triggers?: AgentTrigger[];
  notify?: AgentNotifyShape;
  timeoutMin?: number;
  model?: string;
  paused?: boolean;
}

// The five row groups, in §1.4 order. A named union rather than
// `keyof AutonomyPayload`, which also covers the scalar pause fields below —
// callers that iterate groups must not have to narrow those away.
export type AutonomyGroupKey = 'agents' | 'policies' | 'controllers' | 'builtins' | 'legacySchedules';

export interface AutonomyPayload {
  // Always [] until the managed-agent store lands; the key is present from day
  // one so the client shape never changes underneath this section.
  agents: AutonomyRow[];
  policies: AutonomyRow[];
  controllers: AutonomyRow[];
  builtins: AutonomyRow[];
  legacySchedules: AutonomyRow[];
  // The §6.5 global-pause state, on the same payload as the rows so the banner
  // and the pause dialog cannot disagree about what is paused. Optional on the
  // type only because an older daemon's payload lacks them.
  globalPaused: boolean;
  pausedSet: string[];
  // Rows whose own store refused the pause/resume write. Present only on the
  // pause/resume responses.
  pauseErrors?: string[];
}

export type AutonomyWaitClass = 'veto' | 'hard_human_decision' | 'budget_quota' | 'merge_conflict' | 'controller_paused' | 'policy_mismatch' | 'automation_gap';

export interface AutonomyWait {
  taskId: string;
  title?: string;
  class: AutonomyWaitClass;
  reason: string;
  nextAction: string;
  decideAt?: string;
  policyMatch?: boolean;
  source?: string;
}

export interface EffectiveAutonomy {
  generatedAt: string;
  approval: {
    level: number;
    levelName: string;
    levelSource: string;
    vetoMinutes: number;
    maxPerHour: number;
    approvalsLastHour: number;
    ledgerRetryAt?: string;
    clearedFloors: string[];
    pendingCountdowns: number;
    gate: { granted: boolean; agentId?: string; policyCount: number; policies: WorkflowGatePolicy[] };
    circuitBreaker: { tripped: boolean; reason?: string };
  };
  pauses: { globalPaused: boolean; pausedSet: string[] };
  controllers: {
    proposalAutopilot: boolean;
    triage: boolean;
    triageAutoApply: boolean;
    boardJanitor: boolean;
    releaseTrain: boolean;
    regressionWatch: boolean;
    autoApplyClasses: Record<string, boolean>;
    unavailable?: boolean;
  };
  browser: { flagOn: boolean; flagOffCause?: string };
  repos: Array<{
    path: string;
    optOut: boolean;
    optOutSource?: string;
    policyMatches: boolean;
    preauthorizeEnabled: boolean;
    browserRequired: boolean;
    browserRequireTiers?: string[];
    browserAutoGreen: boolean;
  }>;
  waits: AutonomyWait[];
  waitCounts: Record<AutonomyWaitClass, number>;
  hardGatesRemaining: number;
}

// DeepSeek per-key usage/spend (official CSV import + soft budget) and the
// account-wide balance probe. See docs/PLAN-deepseek-usage-spend-2026-09-15.md.
export type HarnessUsage = {
  requests: number; cacheHitInputTokens: number; cacheMissInputTokens: number; outputTokens: number;
  cost: number; unknownAmounts: number;
};
export type HarnessKeyUsage = HarnessUsage & {
  apiKeyName: string;
  models?: ({ model: string } & HarnessUsage)[];
  budget?: number;
  overBudget?: boolean;
  // Self-metered DSH estimate (see the daemon's session-log meter). Estimated
  // marks a key whose only cost so far is that estimate.
  meterCost?: number;
  meterTokens?: number;
	// Live meter values are canonical CNY estimates even when an imported CSV
	// reports a different currency. Unknown model/calendar tokens stay unpriced.
	meterCurrency?: string;
	meterUnpricedTokens?: number;
  estimated?: boolean;
};
export type HarnessUsageReport = {
  importedAt: number; sources: string[]; rowCount: number; currency: string; costTotal: number;
  keys: HarnessKeyUsage[];
};
export type HarnessBalanceReport = {
  status: string; available: boolean;
  balances: { currency: string; total: string; granted: string; toppedUp: string }[];
};

export type DeepSeekPricingRates = {
  cachedInput: number;
  uncachedInput: number;
  output: number;
};
export type DeepSeekPricingSource = { label: string; url: string; issuedDate?: string };
export type DeepSeekPricingReport = {
  generatedAt: string;
  currency: 'CNY';
  unit: 'per 1M tokens' | string;
  requestedModel?: string;
  canonicalModel?: string;
  priced: boolean;
  state: 'peak' | 'off-peak' | 'schedule-only';
  holidayVerified: boolean;
  holiday: boolean;
  scheduleWouldPeak: boolean;
  savingsPercent?: number;
  currentRates?: DeepSeekPricingRates;
  peakRates?: DeepSeekPricingRates;
  offPeakRates?: DeepSeekPricingRates;
  nextChange?: { state: 'peak' | 'off-peak'; at: string };
  catalog: { model: string; peak: DeepSeekPricingRates; offPeak: DeepSeekPricingRates }[];
  policy: { timezone: string; windows: string[]; halfOpen: boolean; source: DeepSeekPricingSource };
  pricing: { verifiedDate: string; sources: DeepSeekPricingSource[] };
  calendar: { version: string; updatedDate: string; coverageFrom: number; coverageTo: number; sources: DeepSeekPricingSource[] };
};

const workflowSettingsRevisions = new WeakMap<WorkflowSettings, string>();

async function workflowSettingsResponse(response: Response): Promise<WorkflowSettings> {
  const settings = await j<WorkflowSettings>(response);
  const revision = response.headers.get("ETag");
  if (revision) workflowSettingsRevisions.set(settings, revision);
  return settings;
}

export interface FeedbackStatus {
  available: boolean;
  /** Why feedback can't be sent from here: no account, or an account this
   * computer isn't connected to. */
  reason?: 'signed_out' | 'unpaired';
}

export interface PrivacySettings {
  usageUpload: boolean;
  usageUploadEnvDisabled: boolean;
  crashReports: boolean;
  crashReportsEnvDisabled: boolean;
}

// Remote devices (daemon/trusted_devices.go): devices approved to use this
// machine through the cloud relay, and whether only they may.
export interface RemoteDevice {
  id: string;
  name: string;
  status: 'pending' | 'approved';
  code: string;
  /** The computer's identity key (base64 SPKI), returned at enrolment. */
  daemonIdentity?: string;
  /** The approval code also covered the computer's key (V2). */
  verifiesComputer?: boolean;
  createdAt: string;
  approvedAt?: string;
  lastSeenAt?: string;
}

export interface RemoteDevicesState {
  policy: 'any' | 'approved';
  devices: RemoteDevice[];
}

// Agent permissions (daemon/caller_authority.go): whether agents in panes may
// approve for the person, and until when.
export interface AgentAuthoritySettings {
  panes: boolean;
  until?: string;
  active: boolean;
}

/** One thing the Claude Code / Codex integration writes (daemon/agent_integration_api.go). */
export interface AgentIntegrationChange {
  what: string;
  path: string;
}

export interface AgentIntegrationCleanup {
  what: string;
  path?: string;
  result: string;
  error?: string;
}

/** The opt-in for adding SoluTerminal's tools to Claude Code and Codex machine-wide. */
export interface AgentIntegrationSettings {
  state: '' | 'on' | 'off';
  source?: string;
  effective: 'on' | 'off' | 'ask';
  needsAnswer: boolean;
  noticePending: boolean;
  operatorDefault: boolean;
  changes: AgentIntegrationChange[];
  cleanup?: AgentIntegrationCleanup[];
}

// API calls of optional features (see extensions.ts). Each
// ./private/**/*.api.ts default-exports its methods and declares them on
// FeatureApi, so callers keep using `api.<method>`. A tree without ./private
// has none. The modules only build their method objects at load time: they
// are evaluated before this module's body (they import apiFetch from here).
export interface FeatureApi {}

const featureApis = import.meta.glob<{ default: object }>('./private/**/*.api.ts', { eager: true });

const coreApi = {
  health,
  healthBackend: (backend: BackendId) => apiFetchBackend(backend, '/api/health').then(j<Health>),
  // Capability checks must address the selected scope, never health()'s
  // fallback to another healthy backend in router mode.
  healthScoped: (scope: RoutingScope) => apiFetchScoped(scope, '/api/health', { cache: 'no-store' }).then(j<Health & { features?: string[] }>),
  workflowReleaseLedger: (scope?: RoutingScope) =>
    workflowFetch(scope, '/api/workflow/release-ledger').then(j<{ releases: WorkflowReleaseLedgerEntry[] }>),
  workflowRuntimes: (scope?: RoutingScope) => workflowFetch(scope, '/api/workflow/runtimes').then(j<WorkflowRuntimeInfo[]>),
	workflowProviders: () => apiFetch('/api/workflow/providers').then(j<WorkflowProviderInfo[]>),
	workflowModelCatalog: (scope?: RoutingScope) => workflowFetch(scope, '/api/workflow/models/catalog').then(j<WorkflowModelCatalogEntry[]>),
	workflowRuntimeSecrets: (runtime: string) =>
		apiFetch(`/api/workflow/runtimes/${encodeURIComponent(runtime)}/secrets`)
			.then(j<{ runtime: string; secrets: WorkflowRuntimeSecretStatus[] }>),
	workflowRuntimesProbe: (body: { runtime?: string; force?: boolean }) =>
		apiFetch('/api/workflow/runtimes/probe', {
			method: 'POST',
			headers: { 'content-type': 'application/json' },
			body: JSON.stringify(body),
		}).then(j<WorkflowRuntimesHealth>),
	// The operator's "Test now": ONE tiny paid run, fired only by an explicit
	// click. Never poll this. A 429 carries retryAfterSeconds on the ApiError
	// body (read it with recheckRetryAfterSeconds).
	workflowRuntimeRecheck: (id: string) =>
		apiFetch(`/api/workflow/runtimes/${encodeURIComponent(id)}/recheck`, {
			method: 'POST',
		}).then(j<WorkflowRuntimeRecheck>),
  workflowReleaseCheck: async (): Promise<ReleaseCheck> => {
    const r = await apiFetch('/api/workflow/release-check');
    if (r.ok && !(r.headers.get('content-type') || '').includes('json')) {
      throw new ApiError(404, 'release-check endpoint not available (old daemon)');
    }
    return j<ReleaseCheck>(r);
  },
  // Old daemons have no release-train endpoint; they answer the SPA shell, so
  // detect that the same way workflowReleaseCheck does and surface a 404.
  getReleaseTrain: async (refresh = false): Promise<ReleaseTrainStatus> => {
    const r = await apiFetch(`/api/workflow/release-train${refresh ? '?refresh=1' : ''}`);
    if (r.ok && !(r.headers.get('content-type') || '').includes('json')) {
      throw new ApiError(404, 'release-train endpoint not available (old daemon)');
    }
    return j<ReleaseTrainStatus>(r);
  },
  vetoReleaseTrain: (note?: string) =>
    apiFetch('/api/workflow/release-train/veto', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(note ? { note } : {}),
    }).then(j<ReleaseTrainStatus>),
  getConfigurationHistory: async (signal?: AbortSignal): Promise<ConfigurationAuditEntry[]> => {
    const response = await apiFetch('/api/configuration/history', { signal });
    if (response.ok && !(response.headers.get('content-type') || '').includes('json')) {
      throw new ApiError(404, 'Configuration history is unavailable on this daemon');
    }
    return j<ConfigurationAuditEntry[]>(response);
  },
  getWorkflowSettings: (signal?: AbortSignal, scope?: RoutingScope) => workflowFetch(scope, '/api/workflow/settings', { signal }).then(workflowSettingsResponse),
  setWorkflowSettings: (settings: WorkflowSettings, expectedSettings?: WorkflowSettings, scope?: RoutingScope) =>
    workflowFetch(scope, '/api/workflow/settings', {
      method: 'PUT',
      headers: { 'content-type': 'application/json', ...(expectedSettings && workflowSettingsRevisions.has(expectedSettings) ? { 'If-Match': workflowSettingsRevisions.get(expectedSettings)! } : {}) },
      body: JSON.stringify(settings),
    }).then(workflowSettingsResponse),

  // Re-probe for the Claude Code CLI (+ auth) on demand and return the fresh
  // status. Used to force a fresh probe when the setup checklist opens (GET
  // /api/health returns a probe cached up to ~5 min). Local-only on the daemon
  // (403 over the relay).
  claudeRecheck: (): Promise<ClaudeBootstrapStatus> =>
    apiFetch('/api/setup/claude/recheck', { method: 'POST' }).then(j<ClaudeBootstrapStatus>),

  // Backend-pinned variant for the post-install recheck loop: apiFetch resolves
  // the router prefix at CALL time, so a loop armed for the WSL side would
  // silently start probing the Windows daemon after a mid-loop backend switch
  // (and stop on the wrong side's status). null = single-backend (plain fetch).
  claudeRecheckOn: (backend: BackendId | null): Promise<ClaudeBootstrapStatus> =>
    (backend
      ? apiFetchBackend(backend, '/api/setup/claude/recheck', { method: 'POST' })
      : apiFetch('/api/setup/claude/recheck', { method: 'POST' })
    ).then(j<ClaudeBootstrapStatus>),

  // Fetch the OS-correct Claude Code installer command (to spawn in a pane).
  // Local-only on the daemon (403 over the relay).
  claudeInstallCommand: (): Promise<ClaudeInstallSpec> =>
    apiFetch('/api/setup/claude/install-command').then(j<ClaudeInstallSpec>),

  // Muse Code CLI bootstrap status, returned by POST /api/setup/muse/recheck
  // (and mirrored on GET /api/health). See daemon/muse_bootstrap.go.
  museRecheck: (): Promise<MuseBootstrapStatus> =>
    apiFetch('/api/setup/muse/recheck', { method: 'POST' }).then(j<MuseBootstrapStatus>),

  // Backend-pinned variant for the post-install recheck loop (same rationale
  // as claudeRecheckOn). null = single-backend (plain fetch).
  museRecheckOn: (backend: BackendId | null): Promise<MuseBootstrapStatus> =>
    (backend
      ? apiFetchBackend(backend, '/api/setup/muse/recheck', { method: 'POST' })
      : apiFetch('/api/setup/muse/recheck', { method: 'POST' })
    ).then(j<MuseBootstrapStatus>),

  // Fetch the OS-correct Muse Code installer command (to spawn in a pane).
  // Local-only on the daemon (403 over the relay). Unsupported on Windows
  // host-native (supported=false) — the UI falls back to manual guidance.
  museInstallCommand: (): Promise<MuseInstallSpec> =>
    apiFetch('/api/setup/muse/install-command').then(j<MuseInstallSpec>),

  // Codex CLI bootstrap (daemon/codex_bootstrap.go). Local-only on the daemon
  // (403 over the relay). The -On variant pins the backend for the
  // post-install poll, as claudeRecheckOn does.
  codexRecheck: (): Promise<CodexBootstrapStatus> =>
    apiFetch('/api/setup/codex/recheck', { method: 'POST' }).then(j<CodexBootstrapStatus>),
  codexRecheckOn: (backend: BackendId | null): Promise<CodexBootstrapStatus> =>
    (backend
      ? apiFetchBackend(backend, '/api/setup/codex/recheck', { method: 'POST' })
      : apiFetch('/api/setup/codex/recheck', { method: 'POST' })
    ).then(j<CodexBootstrapStatus>),
  codexInstallCommand: (): Promise<ClaudeInstallSpec> =>
    apiFetch('/api/setup/codex/install-command').then(j<ClaudeInstallSpec>),

  // Fetch the daemon's resolved enter-gap timings. Used by loadEnterGapConfig
  // below; exposed on `api` for symmetry / testing. May reject (404 on older
  // daemons) — the caller swallows and keeps ptySubmit's safe defaults.
  enterGapConfig: () => apiFetch('/api/enter-gap').then(j<EnterGapConfig>),

  listPanes: () => apiFetch('/api/panes').then(j<PaneInfo[]>),
  listPanesScoped: (scope: RoutingScope, signal?: AbortSignal) => workflowFetch(scope, '/api/panes', {signal}).then(j<PaneInfo[]>),

  /** Router-mode: fetch panes from BOTH backends in parallel, tag each. */
  listPanesAllBackends: async (): Promise<Array<PaneInfo & { backend?: BackendId }> & { completeBackends?: BackendId[] }> => {
    const [wsl, win] = await Promise.allSettled([
      apiFetchBackend('wsl', '/api/panes').then(j<PaneInfo[]>),
      apiFetchBackend('win', '/api/panes').then(j<PaneInfo[]>),
    ]);
    if (wsl.status === 'rejected' && win.status === 'rejected') {
      throw wsl.reason ?? win.reason;
    }
    const wslPanes = wsl.status === 'fulfilled' ? wsl.value : [];
    const winPanes = win.status === 'fulfilled' ? win.value : [];
    return Object.assign([
      ...wslPanes.map((p) => ({ ...p, backend: 'wsl' as const })),
      ...winPanes.map((p) => ({ ...p, backend: 'win' as const })),
    ], { completeBackends: [wsl.status === 'fulfilled' ? 'wsl' : null, win.status === 'fulfilled' ? 'win' : null].filter((backend): backend is BackendId => backend !== null) });
  },

  // Panes created from the app are the user's; a restore keeps its saved origin.
  // After a workspace restore brings panes back under new ids, point the
  // restored panes' "started by" and lead links at them (daemon pane_origin.go).
  remapPaneOrigins: (remap: Record<string, string>, scope?: RoutingScope) =>
    workflowFetch(scope, '/api/pane-origins/remap', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ remap }),
    }).then(j<{ updated: number }>),

  createPane: (body: CreatePaneReq, scope?: RoutingScope) =>
    workflowFetch(scope, '/api/panes', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body.restore ? body : { startedBy: 'user', ...body }),
    }).then(j<PaneInfo>),

  // Reporter loop: my filed reports + reply (reply text lands in the issue
  // BODY so the pipeline's edited-retriage can unpark a needs-info report).
  feedbackMine: () =>
    apiFetch('/api/feedback/mine').then(j<{ reports: FeedbackReport[] }>),
  feedbackInbox: () =>
    apiFetch('/api/feedback/inbox').then(j<{ events: FeedbackEvent[] }>),
  feedbackInboxRead: (number: number) =>
    apiFetch('/api/feedback/inbox/read', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ number }),
    }).then(() => undefined),
  feedbackThread: (number: number) =>
    apiFetch(`/api/feedback/thread?number=${number}`).then(j<FeedbackThread>),
  feedbackReply: (number: number, text: string) =>
    apiFetch('/api/feedback/reply', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ number, text }),
    }).then(j<{ ok: boolean }>),

  // Whether feedback can be sent from this computer (daemon/feedback.go).
  // null = older daemon without the route: the form behaves as before.
  feedbackStatus: async (): Promise<FeedbackStatus | null> => {
    const res = await apiFetch('/api/feedback/status');
    if (res.status === 404) return null;
    if (!res.ok) throw new Error(`feedback status: ${res.status}`);
    return res.json();
  },
  submitFeedback: (body: FeedbackReq) =>
    apiFetch('/api/feedback', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    }).then(j<FeedbackResult>),

  systemStatus: async (opts?: { measure?: boolean }, signal?: AbortSignal): Promise<SystemStatus> => {
    const r = await apiFetch(`/api/system/status${opts?.measure ? '?measure=1' : ''}`, { signal });
    if (r.ok && !(r.headers.get('content-type') || '').includes('json')) {
      throw new ApiError(404, 'system status not available (old daemon)');
    }
    return j<SystemStatus>(r);
  },

  hostStorage: async (signal?: AbortSignal): Promise<HostStoragePayload> => {
    const r = await apiFetch('/api/system/host-storage', { signal });
    if (r.ok && !(r.headers.get('content-type') || '').includes('json')) throw new ApiError(404, 'host storage not available (old daemon)');
    return j<HostStoragePayload>(r);
  },
  hostStorageRefresh: (): Promise<HostStoragePayload> => apiFetch('/api/system/host-storage/refresh', { method: 'POST' }).then(j<HostStoragePayload>),
  agentRunLog: (id: string, file: string) =>
    apiFetch(`/api/agents/${id}/runlog?file=${encodeURIComponent(file)}`).then((r) => r.text()),
  // "Make this an agent" (§8.3 Phase 1): give an existing task schedule a card
  // WITHOUT changing how it runs. The schedule keeps firing through the same
  // path; it just stops appearing in the Schedules pane, because the card now
  // owns its pause, its Run now and its history. Deleting the agent undoes it.
  importSchedule: (scheduleId: string) =>
    apiFetch('/api/agents/import-schedule', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ scheduleId }),
    }).then(j<Agent>),

  // GitHub App connection status (see GithubAppStatus). Old daemons lack the
  // endpoint — their SPA-fallback 200 normalizes to a 404, which callers must
  // treat as "feature hidden", never as an error.
  githubAppStatus: async (): Promise<GithubAppStatus> => {
    const r = await apiFetch('/api/github/app/status');
    if (r.ok && !(r.headers.get('content-type') || '').includes('json')) {
      throw new ApiError(404, 'github app endpoint not available (old daemon)');
    }
    return j<GithubAppStatus>(r);
  },

  // Finish a connect that started from GitHub's side (no signed state): the
  // logged-in user explicitly claims the installation id.
  githubAppClaim: (installationId: number) =>
    apiFetch('/api/github/app/claim', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ installationId }),
    }).then(j<GithubAppStatus>),

  /**
   * Convert a degraded pane (nested child-session claude, no transcript) to a
   * clean top-level session. mode "sibling" spawns a clean pane beside it and
   * leaves the original; "inplace" respawns the same pane clean (destructive —
   * needs confirm). See daemon/api_migrate.go.
   */
  migratePaneClean: (id: string, body: { mode?: 'sibling' | 'inplace'; confirm?: boolean }) =>
    apiFetch(`/api/panes/${id}/migrate-clean`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    }),

  /** Bulk-migrate every degraded pane. dryRun returns a preview. */
  migrateCleanAll: (body: {
    dryRun?: boolean;
    mode?: 'sibling' | 'inplace';
    confirmAll?: boolean;
    staggerMs?: number;
  }) =>
    apiFetch('/api/panes/migrate-clean-all', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    }).then(j<{ dryRun?: boolean; mode?: string; affected?: Array<{ paneId: string; title: string; degradedReason: string }>; summary?: { total: number }; status?: string; count?: number }>),

  deletePane: (id: string, scope?: RoutingScope) =>
    workflowFetch(scope, `/api/panes/${id}`, { method: 'DELETE' }).then(j<void>),

  resizePane: (id: string, cols: number, rows: number) =>
    apiFetch(`/api/panes/${id}/resize`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ cols, rows }),
    }).then(j<void>),

  // Force a clean repaint on the pane's running TUI. Same op as the
  // WS {type:"redraw"} message — fires the daemon's full screen
  // clear + size-bump dance. Called by loadWorkspace after mount on
  // every claude/codex pane to scrub back-buffer vs emulator
  // divergence picked up while streaming during the workspace switch.
  redrawPane: (id: string) =>
    apiFetch(`/api/panes/${id}/redraw`, { method: 'POST' }).then(j<void>),

  renamePane: (id: string, title: string, scope?: RoutingScope) =>
    workflowFetch(scope, `/api/panes/${id}/rename`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ title }),
    }).then(j<PaneInfo>),

  // Handle and working-on line (daemon/pane_identity.go). An omitted field is
  // kept, "" clears it; a taken handle answers 409 naming its holder.
  setPaneIdentity: (id: string, change: { handle?: string; workingOn?: string }, scope?: RoutingScope) =>
    workflowFetch(scope, `/api/panes/${id}/identity`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(change),
    }).then(j<{ identity: PaneIdentity | null }>),

  // A name for the pane from your own Claude Code (Haiku), read from its
  // conversation or prompts; nothing is changed (daemon/pane_auto_name.go).
  suggestPaneName: (id: string) =>
    apiFetch(`/api/panes/${id}/name/suggest`, { method: 'POST' }).then(j<{ name: string }>),

  // Opt-in: name a still-generic Claude/Codex pane after its first prompt.
  getPaneNaming: () => apiFetch('/api/settings/pane-naming').then(j<{ autoName: boolean }>),
  setPaneNaming: (autoName: boolean) =>
    apiFetch('/api/settings/pane-naming', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ autoName }),
    }).then(j<{ autoName: boolean }>),

  // Dismiss the pane's review result (daemon pane_review.go); the review
  // ledger and GitHub's checks are not affected.
  clearPaneReview: (id: string) =>
    apiFetch(`/api/panes/${id}/review`, { method: 'DELETE' }).then((r) => {
      if (!r.ok) throw new Error(`Could not clear the result (${r.status})`);
    }),

  setPaneAutoMode: (id: string, autoMode: '' | 'always' | 'cautious', scope?: RoutingScope) =>
    workflowFetch(scope, `/api/panes/${id}/auto-mode`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ autoMode }),
    }).then(j<PaneInfo>),

  // announce is tri-state: true/false record an explicit choice, null clears it
  // back to the pane's role default.
  setPaneAnnounceDelegation: (id: string, announce: boolean | null, scope?: RoutingScope) =>
    workflowFetch(scope, `/api/panes/${id}/announce-delegation`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ announce }),
    }).then(j<PaneInfo>),

  setPaneFlag: (id: string, flagged: boolean, memo: string, scope?: RoutingScope) =>
    workflowFetch(scope, `/api/panes/${id}/flag`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ flagged, memo }),
    }).then(j<PaneInfo>),

  // Upload a pasted image. Body is the raw image bytes; the browser's
  // File/Blob object already carries the correct MIME type. Daemon
  // writes it to ~/.soluterminal/pastes/ and returns the absolute path
  // we can feed to Claude Code as `@<path>`.
  pasteImage: (id: string, blob: Blob, scope?: RoutingScope) =>
    workflowFetch(scope, `/api/panes/${id}/paste-image`, {
      method: 'POST',
      headers: { 'content-type': blob.type || 'application/octet-stream' },
      body: blob,
    }).then(j<{ path: string; bytes: number }>),

  // Upload a dropped non-image file (daemon paste_file.go). Body is the raw
  // bytes; X-Filename (URL-encoded) only supplies the extension, which the
  // daemon checks against its allowlist (415 otherwise). The saved file gets
  // a random name under ~/.soluterminal/pastes/ and the absolute path comes back.
  pasteFile: (id: string, file: File, scope?: RoutingScope) =>
    workflowFetch(scope, `/api/panes/${id}/paste-file`, {
      method: 'POST',
      headers: {
        'content-type': file.type || 'application/octet-stream',
        'X-Filename': encodeURIComponent(file.name),
      },
      body: file,
    }).then(j<{ path: string; bytes: number }>),

  listWorkspaces:() => apiFetch('/api/workspaces').then(j<Workspace[]>),
  listWorkspaceSummaries: () => apiFetch('/api/workspaces?summary=1').then(j<WorkspaceSummary[]>),

  /**
   * Router-mode helper: fetch workspaces from BOTH backends in parallel
   * and tag each with its backend. Outside router mode returns just
   * the default-origin list, untagged. The sidebar uses the tags to
   * show backend-aware grouping + to know which prefix to flip to when
   * the user clicks a workspace.
   */
  listWorkspacesAllBackends: async (): Promise<Array<Workspace & { backend?: BackendId }>> => {
    const [wsl, win] = await Promise.allSettled([
      apiFetchBackend('wsl', '/api/workspaces').then(j<Workspace[]>),
      apiFetchBackend('win', '/api/workspaces').then(j<Workspace[]>),
    ]);
    if (wsl.status === 'rejected' && win.status === 'rejected') {
      throw wsl.reason ?? win.reason;
    }
    const wslWorkspaces = wsl.status === 'fulfilled' ? wsl.value : [];
    const winWorkspaces = win.status === 'fulfilled' ? win.value : [];
    return [
      ...wslWorkspaces.map((w) => ({ ...w, backend: 'wsl' as const })),
      ...winWorkspaces.map((w) => ({ ...w, backend: 'win' as const })),
    ];
  },

  listWorkspaceSummariesAllBackends: async (): Promise<Array<WorkspaceSummary & { backend?: BackendId }>> => {
    const [wsl, win] = await Promise.allSettled([
      apiFetchBackend('wsl', '/api/workspaces?summary=1').then(j<WorkspaceSummary[]>),
      apiFetchBackend('win', '/api/workspaces?summary=1').then(j<WorkspaceSummary[]>),
    ]);
    if (wsl.status === 'rejected' && win.status === 'rejected') {
      throw wsl.reason ?? win.reason;
    }
    const wslWorkspaces = wsl.status === 'fulfilled' ? wsl.value : [];
    const winWorkspaces = win.status === 'fulfilled' ? win.value : [];
    return [
      ...wslWorkspaces.map((w) => ({ ...w, backend: 'wsl' as const })),
      ...winWorkspaces.map((w) => ({ ...w, backend: 'win' as const })),
    ];
  },

  saveWorkspace: (w: Workspace, scope?: RoutingScope) =>
    workflowFetch(scope,'/api/workspaces', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(w),
    }).then(j<Workspace>),

  movePaneToWorkspace: (source: string, paneId: string, destination: string, paneMoveVersion: number, scope?: RoutingScope) =>
    workflowFetch(scope, `/api/workspaces/${encodeURIComponent(source)}/move-pane`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ paneId, destination, paneMoveVersion, direction: 'horizontal' }),
    }).then(j<{ source: Workspace; destination: Workspace }>),

  deleteWorkspace: (name: string) =>
    apiFetch(`/api/workspaces/${encodeURIComponent(name)}`, { method: 'DELETE' }).then(j<void>),

  renameWorkspace: (oldName: string, newName: string) =>
    apiFetch(`/api/workspaces/${encodeURIComponent(oldName)}/rename`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ newName }),
    }).then(j<void>),

  setWorkspaceContextPack: (name: string, contextPack: string) =>
    apiFetch(`/api/workspaces/${encodeURIComponent(name)}/context-pack`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ contextPack }),
    }).then(j<Workspace>),

  listPorts: () => apiFetch('/api/ports').then(decodePorts),

  probePort: (port: Port) => {
    const selector = port.previewPath?.split('~')[1]?.replace(/\/$/, '');
    if (!selector || !/^[A-Za-z0-9_-]+$/.test(selector)) return Promise.reject(new Error('Refresh Services before checking this listener.'));
    const init = {method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({port:port.port,selector})};
    return (port.backend ? apiFetchBackend(port.backend,'/api/ports/probe',init) : apiFetch('/api/ports/probe',init)).then(j<Port>);
  },

  /** Router-mode: fetch ports from BOTH backends, tag each with backend. */
  listPortsAllBackends: async (): Promise<PortList> => {
    const [wsl, win] = await Promise.allSettled([
      apiFetchBackend('wsl', '/api/ports').then(decodePorts),
      apiFetchBackend('win', '/api/ports').then(decodePorts),
    ]);
    if (wsl.status === 'rejected' && win.status === 'rejected') {
      throw wsl.reason ?? win.reason;
    }
    const wslPorts = wsl.status === 'fulfilled' ? wsl.value : [];
    const winPorts = win.status === 'fulfilled' ? win.value : [];
    return Object.assign([
      ...wslPorts.map((p) => ({ ...p, backend: 'wsl' as const })),
      ...winPorts.map((p) => ({ ...p, backend: 'win' as const })),
    ], {warning:[wsl.status === 'rejected' ? 'WSL services unavailable.' : wsl.value.warning,win.status === 'rejected' ? 'Windows services unavailable.' : win.value.warning].filter(Boolean).join(' ') || undefined});
  },

  listDirs: (q?: string) => {
    const qs = q ? `?q=${encodeURIComponent(q)}` : '';
    return apiFetch(`/api/dirs${qs}`).then(j<DirCandidate[]>);
  },

  paneLiveCwd: (id: string) =>
    apiFetch(`/api/panes/${id}/cwd`).then(j<{ cwd: string }>).then((r) => r.cwd),

  // What MCP servers Claude Code would see if launched from `cwd`.
  // Merges ~/.claude.json (user + per-project) with .mcp.json walks.
  listMcpServers: (cwd: string) =>
    apiFetch(`/api/mcp/servers?cwd=${encodeURIComponent(cwd)}`).then(j<McpServer[]>),

  readFile: (path: string) =>
    apiFetch(`/api/file?path=${encodeURIComponent(path)}`).then(j<{
      path: string;
      lines: string[];
      mtime: number;
      size: number;
      isEnv: boolean;
      trailingNewline: boolean;
    }>),

  listDir: (path: string) =>
    apiFetch(`/api/dir/list?path=${encodeURIComponent(path)}`).then(j<{
      path: string;
      parent: string;
      entries: { name: string; isDir: boolean; size: number }[];
    }>),

  searchFiles: (
    root: string,
    q: string,
    limit = 30,
    kind: 'files' | 'dirs' | 'all' = 'files',
  ) => {
    const qs = new URLSearchParams({
      root,
      q,
      limit: String(limit),
    });
    // Only set kind when non-default so old daemons that predate the
    // param ignore the key cleanly. (They already ignore unknown query
    // params, but keeping the URL minimal makes diffs easier to read.)
    if (kind !== 'files') qs.set('kind', kind);
    return apiFetch(`/api/files/search?${qs.toString()}`).then(j<{
      root: string;
      hits: { path: string; name: string; rel: string; isDir?: boolean }[];
      walked: number;
      capped: boolean;
    }>);
  },

  // Tells the daemon "user has seen this pane's attention prompt; reset
  // the per-pane notification dedup state so the next fresh transition
  // can fire again". Best-effort — server's own dedup will catch up if
  // this drops. Wired to focus + visible (App.tsx).
  ackPaneNotification: (id: string) =>
    apiFetch(`/api/panes/${encodeURIComponent(id)}/ack`, { method: 'POST' })
      .then(j<void>)
      .catch(() => { /* best-effort */ }),

  // Activity feed snapshot — the daemon keeps a ring buffer (~200
  // events) of recent uiBus messages so a freshly-opened Activity view
  // has history to show even though SSE only delivers events it
  // subscribed to live. Oldest-first; client sorts/filters for display.
  activity: () =>
    apiFetch('/api/activity').then(j<{
      events: Array<{
        type: string;
        paneId?: string;
        paneTitle?: string;
        ts?: number;
        tool?: string;
        hint?: string;
        fromState?: string;
        toState?: string;
        durationMs?: number;
        path?: string;
        url?: string;
        text?: string;
      }>;
    }>),

  // Fetch a pane's scrollback as a plain-text transcript (ANSI stripped,
  // with a metadata header). The daemon responds with a Content-
  // Disposition header that includes a suggested filename; we read it
  // here so the caller can use the same name for the Blob download.
  exportPaneTranscript: async (id: string): Promise<{ filename: string; text: string }> => {
    const res = await apiFetch(`/api/panes/${id}/export`);
    if (!res.ok) {
      const body = (await res.text()).trim();
      throw new Error(body || `${res.status} ${res.statusText}`);
    }
    // Pull filename out of Content-Disposition. Server format is
    // `attachment; filename="slug-YYYY-MM-DD.txt"`.
    const disp = res.headers.get('Content-Disposition') || '';
    const m = /filename="([^"]+)"/.exec(disp);
    const filename = m?.[1] || `pane-${id}.txt`;
    const text = await res.text();
    return { filename, text };
  },

  supportBundle: async (paneId?: string): Promise<SupportBundleResponse> => {
    const qs = paneId ? `?paneId=${encodeURIComponent(paneId)}` : '';
    const res = await apiFetch(`/api/support/bundle${qs}`);
    if (res.status === 403) throw new SupportBundleRemoteError();
    if (!res.ok) throw new Error((await res.text()).trim() || `${res.status} ${res.statusText}`);
    return res.json() as Promise<SupportBundleResponse>;
  },

  // Cross-pane content search. Scans every running pane's stripped
  // scrollback for `q` (case-insensitive substring) and returns matches
  // grouped by pane. Server caps: 8 matches per pane, 40 overall — UI
  // doesn't need more, and unbounded scans would stream megabytes for
  // common tokens. Empty `q` returns no results without hitting the scan
  // path (cheap bootstrap for the palette's search mode).
  searchPanes: (q: string) => {
    const qs = new URLSearchParams({ q });
    return apiFetch(`/api/panes/search?${qs.toString()}`).then(j<{
      query: string;
      totalMatches: number;
      capped: boolean;
      results: Array<{
        paneId: string;
        paneTitle: string;
        paneCmd: string[];
        status: string;
        matchCount: number;
        matches: Array<{ line: number; start: number; end: number; text: string }>;
      }>;
    }>);
  },

  writeFileLine: (path: string, lineNo: number, newText: string, expectedMtime: number) =>
    apiFetch('/api/file/line', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ path, lineNo, newText, expectedMtime }),
    }).then(j<{ mtime: number }>),

  // Read-only git diff for the DiffTile. `path` may be a repo dir or a file
  // within one; `staged` selects the index diff (git diff --cached). Over the
  // cloud relay, secret files are excluded from the diff text and flagged.
  gitDiff: (path: string, staged: boolean) => {
    const qs = new URLSearchParams({ path });
    if (staged) qs.set('staged', '1');
    return apiFetch(`/api/git/diff?${qs.toString()}`).then(j<{
      repo: string;
      branch: string;
      staged: boolean;
      files: { path: string; status: string; added: number; removed: number; secret: boolean }[];
      diff: string;
      truncated: boolean;
      hiddenSecret: number;
    }>);
  },

  // Recently closed panes on this backend (daemon/closed_panes.go).
  closedPanes: () => apiFetch('/api/panes/closed').then(j<{ panes: ClosedPane[] }>),

  // Reopen a closed pane: the daemon rebuilds it from its own snapshot and
  // resumes the conversation. cols/rows = the live viewport, best-effort.
  reopenClosedPane: (id: string, cols?: number, rows?: number) =>
    apiFetch(`/api/panes/closed/${encodeURIComponent(id)}/reopen`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(cols && rows ? { cols, rows } : {}),
    }).then(j<PaneInfo>),

  // Past Claude/Codex conversations started in `cwd`, newest first
  // (daemon/sessions_browse.go). Interactive sessions only unless automated.
  recentSessions: (cwd: string, automated = false) => {
    const qs = new URLSearchParams({ cwd });
    if (automated) qs.set('automated', '1');
    return apiFetch(`/api/sessions/recent?${qs.toString()}`).then(j<{ sessions: BrowsedSession[] }>);
  },

  // Terminal-mode agent turns for a pane, newest first (daemon/turns.go).
  paneTurns: (paneId: string) =>
    apiFetch(`/api/panes/${encodeURIComponent(paneId)}/turns`).then(j<{
      turns: TurnRecord[];
      activeStartedMs?: number;
    }>),

  // One turn's recorded diff — the same shape as gitDiff plus turn metadata.
  paneTurnDiff: (paneId: string, n: number) =>
    apiFetch(`/api/panes/${encodeURIComponent(paneId)}/turns/${n}/diff`).then(j<{
      repo: string;
      branch: string;
      staged: boolean;
      files: { path: string; status: string; added: number; removed: number; secret: boolean }[];
      diff: string;
      truncated: boolean;
      hiddenSecret: number;
      turn: number;
      startedMs: number;
      endedMs: number;
      overlapPanes?: string[] | null;
      skippedUntracked?: number;
    }>),

  // Files agents recently CREATED or EDITED (from the Claude Code Edit/Write
  // hooks), newest first. Distinct from the client-side recent-OPEN list — this
  // is "what the agent just touched → one-tap open". Over the cloud relay,
  // secret/credential paths are omitted server-side.
  recentFiles: () =>
    apiFetch('/api/recent-files').then(j<{
      files: {
        path: string; name: string; dir: string; tool: string;
        paneId?: string; paneTitle?: string; ts: number;
      }[];
    }>),

  // Subprotocol list for authenticated WebSockets — pass as the second
  // arg: `new WebSocket(url, api.wsProtocols())`. The browser WebSocket
  // API can't set an Authorization header, and a JWT in the query string
  // leaks into proxy/access logs, so in cloud mode the token rides the
  // handshake as a "bearer.<jwt>" Sec-WebSocket-Protocol entry instead
  // (JWTs are base64url — all valid subprotocol characters). The cloud's
  // extractTokenWithSource consumes it; 'solu-auth' is the echoable name
  // the server selects in the 101, so the secret is never reflected back.
  // Reads the CURRENT module `token`, so each reconnect picks up a
  // rotated JWT. Returns undefined (never []) when there's no token so
  // the constructor skips subprotocol negotiation entirely — local-mode
  // daemons must stay reachable without any offer.
  wsProtocols: (): string[] | undefined =>
    token ? ['solu-auth', 'bearer.' + token] : undefined,

  // deviceId is this browser profile's persistent identity, sent on every
  // pane WS so the daemon can tell "the pane's owner is this same device's
  // abandoned socket" (safe to displace — the ghost-owner lockout after
  // daemon-update restarts + page reloads) from "a genuinely different
  // device holds input" (locked, truthfully). Survives page reloads and
  // app updates; per-profile, so a phone or another computer never shares
  // it. Falls back to a per-session id when localStorage is unavailable.
  deviceId: (): string => {
    // crypto.randomUUID is secure-context-only (missing over plain-http LAN
    // access), so fall back to getRandomValues-based hex.
    const newId = () => {
      if (typeof crypto.randomUUID === 'function') return crypto.randomUUID();
      const bytes = crypto.getRandomValues(new Uint8Array(16));
      return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
    };
    try {
      const key = 'solu.device-id';
      let v = localStorage.getItem(key);
      if (!v) {
        v = newId();
        localStorage.setItem(key, v);
      }
      return v;
    } catch {
      if (!sessionDeviceId) sessionDeviceId = newId();
      return sessionDeviceId;
    }
  },

  // wsUrl includes the routing prefix; auth rides the WebSocket
  // handshake, not the URL (see wsProtocols above).
  //
  // PTY sizing is now ownership-based (see Pane.ownerSubID in
  // daemon/pane.go). No per-request flag needed — first-to-connect
  // auto-claims, later clients send {type:"claim"} to take over.
  wsUrl: (id: string, cols: number, rows: number, modeOrOpts?: 'grid' | { mode?: 'grid'; skipReplay?: boolean; since?: number; epoch?: number; mobile?: boolean; scope?: RoutingScope; visible?: boolean; retirementViewerId?: string; termCols?: number; termRows?: number }) => {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    const mode = typeof modeOrOpts === 'string' ? modeOrOpts : modeOrOpts?.mode;
    const skipReplay = typeof modeOrOpts === 'object' ? !!modeOrOpts.skipReplay : false;
    const since = typeof modeOrOpts === 'object' ? modeOrOpts.since : undefined;
    const epoch = typeof modeOrOpts === 'object' ? modeOrOpts.epoch : undefined;
    const mobile = typeof modeOrOpts === 'object' ? !!modeOrOpts.mobile : false;
    // Include backendPrefix so the WS upgrade hits the pane's actual
    // daemon via the router. Panes belong to one backend — without
    // this prefix, WS would always route to the router's default
    // (WSL) and panes on Windows couldn't stream their I/O.
    const route = typeof modeOrOpts === 'object' ? modeOrOpts.scope ?? currentRoutingScope() : currentRoutingScope();
    let url = `${proto}://${location.host}${route.prefix}${route.backendPrefix}/api/panes/${id}/io?cols=${cols}&rows=${rows}`;
    if (typeof modeOrOpts === 'object' && modeOrOpts.visible === false) url += `&visible=0`;
    if (typeof modeOrOpts === 'object' && modeOrOpts.retirementViewerId) url += `&retirement_viewer=${encodeURIComponent(modeOrOpts.retirementViewerId)}`;
    // The terminal's real size, when it differs from the seeded cols/rows:
    // the daemon lines its reattach repaint up with it (no duplicated tail).
    if (typeof modeOrOpts === 'object' && modeOrOpts.termCols && modeOrOpts.termRows) {
      url += `&term_cols=${modeOrOpts.termCols}&term_rows=${modeOrOpts.termRows}`;
    }
    if (mode === 'grid') url += `&mode=grid`;
    if (skipReplay) url += `&skip_replay=1`;
    url += `&client=web`;
    url += `&device=${encodeURIComponent(api.deviceId())}`;
    if (mobile) url += `&mobile=1`;
    // Driven browsers (Playwright/Selenium set navigator.webdriver)
    // self-identify so the daemon excludes them from PTY sizing — a
    // 1280×720 test viewport must not shrink a human's terminal.
    if (typeof navigator !== 'undefined' && navigator.webdriver) url += `&automation=1`;
    // Exact-resume token: the ring offset this client had received up
    // to, plus the daemon ring epoch it belongs to. The daemon replays
    // precisely the missed bytes (see daemon/ws.go) instead of either
    // skipping the gap (stale surface) or model-painting (ghost risk).
    if (skipReplay && since !== undefined && since >= 0 && epoch) {
      url += `&since=${since}&epoch=${epoch}`;
    }
    return url;
  },

  // SSE endpoint for daemon → UI events (e.g. "Claude asked to preview
  // this file"). Include backendPrefix so in router mode the SSE stream
  // comes from the currently-active backend. Without this, events from
  // Windows would never reach the frontend while WSL was active. The URL
  // never carries a credential: open it with openUiEvents().
  uiEventsUrl: () => `${prefix}${backendPrefix}/api/ui/events`,

  // Open the UI event stream. EventSource can't set headers, and a sign-in
  // token in the URL is kept by the cloud host's request log, so through the
  // relay the URL carries a single-use 30 s ticket instead (see
  // cloud/relay_ticket.go). Local daemons need no credential in the URL.
  openUiEvents: (): EventStream => {
    const url = api.uiEventsUrl();
    const sign = streamSigningNeeded() ? (u: string) => signStreamUrl(u) : undefined;
    // Opted in and pinned: the channel only, never the plaintext relay.
    if (privateRelayTarget(prefix, backendPrefix)) return openPrivateRelayEventStream(url, { prefix, backendPrefix });
    return openTicketedEventSource(url, relayNeedsTicket(url) ? () => mintRelayTicket(url, 'once') : null, sign);
  },

  // Native renderer WebSocket (replaced an SSE stream). WS lives in a
  // SEPARATE browser connection pool (the one wsUrl/terminal panes use), so
  // visible native panes don't exhaust the ~6 HTTP/1.1 connections-per-host on
  // the Local desktop (which broke reload + health → "daemon disconnect").
  // Absolute ws/wss URL like wsUrl (WebSocket needs a full origin); auth rides
  // the Sec-WebSocket-Protocol handshake (see wsProtocols), not the URL.
  nativeWsUrl: (id: string) =>
    `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}${prefix}${backendPrefix}/api/panes/${id}/native/ws`,
  nativeHistory: (id: string, since: number) =>
    apiFetch(`/api/panes/${id}/native/history?since=${since}`)
      .then(j<import('./native/nativeEvents').NativeHistoryResponse>),
  nativeInput: (id: string, text: string, localMsgId?: string) =>
    apiFetch(`/api/panes/${id}/native/input`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(localMsgId ? { text, localMsgId } : { text }),
    }).then(j<import('./native/nativeEvents').NativeInputResponse>),
  setPaneComposerState: (id: string, nonEmpty: boolean) =>
    apiFetch(`/api/panes/${id}/composer-state`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ nonEmpty }),
    }),
  resolveNativeApproval: (id: string, reqId: string, allow: boolean, denyReason?: string) =>
    apiFetch(`/api/panes/${id}/approvals/${reqId}/resolve`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(denyReason ? { allow, denyReason } : { allow }),
    }).then(j<import('./native/nativeEvents').ResolveApprovalResponse>),
  nativeInterrupt: (id: string) =>
    apiFetch(`/api/panes/${id}/native/interrupt`, { method: 'POST' })
      .then(j<import('./native/nativeEvents').NativeInterruptResponse>),
  // Per-pane tool "always allow" list. GET reads the current set; POST mutates it
  // ({tool, action:'add'|'remove'}) and returns the UPDATED set. A tool on this
  // list auto-approves future permission_requests for that tool. Match by exact
  // tool name. See daemon/native_api.go (allowlist endpoints).
  nativeAllowlist: (id: string) =>
    apiFetch(`/api/panes/${id}/native/allowlist`)
      .then(j<import('./native/nativeEvents').NativeAllowlistResponse>),
  nativeSetAllowlist: (id: string, tool: string, action: 'add' | 'remove') =>
    apiFetch(`/api/panes/${id}/native/allowlist`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ tool, action }),
    }).then(j<import('./native/nativeEvents').NativeAllowlistResponse>),
  // Flip a live pane between the xterm terminal renderer and the native
  // renderer in place. kind:'' (or 'terminal') restores the terminal body;
  // kind:'native' switches to the structured conversation view. The daemon
  // updates the pane DTO's kind, which the panes SSE/poll propagates back so
  // the render dispatch (gated on pane.kind==='native') swaps the body.
  //
  // `opts` carries the WIRE CONTRACT's deferred-switch fields (back-compat: no
  // opts ⇒ today's immediate behavior):
  //   • when:'now'      — interrupt the current turn and switch immediately.
  //   • when:'deferred' — arm a LOSSLESS switch that fires when the turn finishes
  //                       (the daemon returns a DTO carrying pendingSwitch).
  //   • cancel:true     — disarm a previously-armed deferred switch.
  //   • cols/rows       — the client's live measured viewport (wire parity with
  //                       resumePane). Usually absent: a Focus pane has no
  //                       mounted xterm to measure, so the daemon spawns the
  //                       switched terminal at persisted dims marked unverified
  //                       and the first attach re-measures + nudges a redraw.
  switchPaneMode: (
    id: string,
    kind: '' | 'native',
    opts?: { when?: 'now' | 'deferred'; cancel?: boolean; cols?: number; rows?: number },
    scope?: RoutingScope,
  ) =>
    workflowFetch(scope,`/api/panes/${id}/switch-mode`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({
        kind,
        ...(opts?.when ? { when: opts.when } : {}),
        ...(opts?.cancel ? { cancel: true } : {}),
        ...(opts?.cols && opts?.rows ? { cols: opts.cols, rows: opts.rows } : {}),
      }),
    }).then(j<PaneInfo>),

  // Reconnect a Codex pane to live account switching. when:'idle' queues the
  // reconnect for the next idle prompt instead of refusing a busy pane
  // (armed:true); cancel:true withdraws a queued one.
  reconnectCodexAuth: (id: string, opts?: { when?: 'now' | 'idle'; cancel?: boolean }) =>
    apiFetch(`/api/panes/${id}/codex-live-auth`, {
      method: 'POST',
      ...(opts ? { headers: { 'content-type': 'application/json' }, body: JSON.stringify(opts) } : {}),
    }).then(j<{ paneId: string; state?: string; alreadyConnected?: boolean; armed?: boolean; waiting?: string }>),

  // Resume a suspended (exited-but-resumable) Claude pane in place — same pane
  // id, a fresh `claude --resume <sessionId>` process. Returns the new running
  // PaneInfo (with a CHANGED pid, which drives the client's reconnect/replay).
  // cols/rows are the client's live measured viewport — the daemon spawns the
  // resumed PTY at that width so it comes up correct instead of at a stale
  // panes.json size. Omitted (native panes / no live term) → daemon falls back
  // to the persisted dims marked unverified, and the first attach re-measures.
  resumePane: (id: string, cols?: number, rows?: number, scope?: RoutingScope, cwd?: string) =>
    workflowFetch(scope, `/api/panes/${id}/resume`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ ...(cols && rows ? { cols, rows } : {}), ...(cwd ? { cwd } : {}) }),
    }).then(j<PaneInfo>),

  // Restart a native (Focus) pane's headless agent in place — kill it and resume
  // the same Claude session or Codex thread under the SAME pane id — reclaiming
  // the long-lived process's heap while keeping the conversation. Returns the new running
  // PaneInfo (CHANGED pid, which drives the client's reconnect/replay, same as
  // resume). Native-only + settle-gated server-side (409 if busy / no conversation id).
  refreshPane: (id: string) =>
    apiFetch(`/api/panes/${id}/refresh`, { method: 'POST' }).then(j<PaneInfo>),

  // Restart THIS pane's agent in place on the installed binary, any pane kind:
  // kill the old pane-agent and spawn a new one under the SAME pane id, resuming
  // the same conversation (Claude session / Codex thread / Muse id / harness
  // sidecar). ?fresh=1 starts a brand-new conversation. Settle-gated server-side
  // (409 if busy). A pane with no durable conversation returns 409 with
  // body.needsFresh=true, which the caller confirms and retries with fresh=true.
  // force=1 is the confirmed user override of the busy settle gate.
  restartPane: (id: string, fresh = false, force = false, scope?: RoutingScope) => {
    const params = [fresh ? 'fresh=1' : '', force ? 'force=1' : ''].filter(Boolean).join('&');
    return workflowFetch(scope, `/api/panes/${id}/restart${params ? `?${params}` : ''}`, { method: 'POST' }).then(j<PaneInfo>);
  },

  // Change a native pane's reasoning-effort level. Like refreshPane this
  // respawns the headless claude (`--resume` + the new --effort), so it returns
  // a PaneInfo with a CHANGED pid that drives the client's reconnect/replay.
  // Native-only + settle-gated server-side (409 if busy / no session). A no-op
  // (same level) returns the current pane without respawning.
  setPaneEffort: (id: string, effort: PaneEffort) =>
    apiFetch(`/api/panes/${id}/effort`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ effort }),
    }).then(j<PaneInfo>),

  setPaneContextPack: (id: string, contextPack: string, cols?: number, rows?: number, scope?: RoutingScope) =>
    workflowFetch(scope, `/api/panes/${id}/context-pack`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ contextPack, cols, rows }),
    }).then(j<PaneInfo>),

  // Set or remove a pane role. This respawns the supported Claude/Codex host:
  // an existing conversation is resumed and a session-less host starts fresh.
  // The changed pid drives reconnect/replay; busy panes return 409 and a true
  // no-op returns the current pane without restarting.
  setPaneRole: (id: string, role: PaneRole | '', cols?: number, rows?: number, scope?: RoutingScope) =>
    workflowFetch(scope, `/api/panes/${id}/role`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(cols && rows ? { role, cols, rows } : { role }),
    }).then(j<PaneInfo>),

  orchestratorHosts: () => apiFetch('/api/orchestrator-hosts').then(j<OrchestratorHostsInfo>),

  // Change a native pane's Claude model. Same respawn contract as
  // setPaneEffort (`--resume` + the new --model, changed pid, 409 when busy).
  // Passing a model also REWRITES the session's stored model, so the choice
  // survives future bare resumes/switches — this is the only model control for
  // Focus panes (headless claude has no /model command).
  setPaneModel: (id: string, model: string) =>
    apiFetch(`/api/panes/${id}/model`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ model }),
    }).then(j<PaneInfo>),

  // Schedules — recurring prompt injection into a pane, or a built-in
  // agent run (kind=agent). See daemon/schedules.go for semantics.
  listSchedules: () => apiFetch('/api/schedules').then(j<Schedule[]>),
  // One row — what a `schedules-changed` event naming a schedule refetches.
  // 404 = gone (deleted, or a hidden agent trigger).
  getSchedule: (id: string) => apiFetch(`/api/schedules/${encodeURIComponent(id)}`).then(j<Schedule>),

  // Built-in runnable agents for the schedule dialog's picker. Old
  // daemons don't serve this — callers treat any failure as "no agents".
  listScheduleAgents: () => apiFetch('/api/schedules/agents').then(j<ScheduleAgent[]>),

  createSchedule: (input: ScheduleInput) =>
    apiFetch('/api/schedules', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(input),
    }).then(j<Schedule>),

  updateSchedule: (id: string, input: ScheduleInput) =>
    apiFetch(`/api/schedules/${id}`, {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(input),
    }).then(j<Schedule>),

  deleteSchedule: (id: string) =>
    apiFetch(`/api/schedules/${id}`, { method: 'DELETE' }).then(j<void>),

  setScheduleEnabled: (id: string, enabled: boolean) =>
    apiFetch(`/api/schedules/${id}/${enabled ? 'enable' : 'disable'}`, {
      method: 'POST',
    }).then(j<Schedule>),

  fireSchedule: (id: string) =>
    apiFetch(`/api/schedules/${id}/fire`, { method: 'POST' }).then(j<Schedule>),

  snoozeSchedule: (id: string) =>
    apiFetch(`/api/schedules/${id}/snooze`, { method: 'POST' }).then(j<Schedule>),

  // Claude account credentials. listClaudeAccounts is CHEAP (cached, no
  // token cost) — safe to call on section expand. refreshClaude is the
  // ONLY call that costs tokens (~1/account, freshly probes usage) —
  // call it only on an explicit user click. NEVER auto-poll it.
  // Team-only feature flags (daemon/entitlements.go). null = older daemon
  // without the endpoint, which predates the beta gate and shows everything.
  getEntitlements: async (accountToken?: string | null): Promise<import('./types').Entitlements | null> => {
    const res = await apiFetch('/api/entitlements', {
      headers: accountToken ? { 'X-Solu-Account-Token': accountToken } : undefined,
    });
    if (res.status === 404) return null;
    if (!res.ok) throw new Error(`entitlements: ${res.status}`);
    return res.json();
  },
  // Where smart approvals come from on this computer and the last daily
  // allowance the cloud reported (daemon/aria_jev_cloud.go). null = older
  // daemon or unreachable.
  smartApprovalsStatus: async (): Promise<{ source: 'local' | 'cloud' | 'none'; paused?: string; remaining?: number; limit?: number } | null> => {
    try {
      const res = await apiFetch('/api/smart-approvals');
      if (!res.ok) return null;
      return await res.json();
    } catch {
      return null;
    }
  },
  // Anonymous daily check-in preference (daemon/checkin). null = older
  // daemon without the endpoint, so the toggle stays hidden.
  getCheckinSettings: async (): Promise<{ enabled: boolean } | null> => {
    const res = await apiFetch('/api/settings/checkin');
    if (res.status === 404) return null;
    if (!res.ok) throw new Error(`checkin settings: ${res.status}`);
    return res.json();
  },
  setCheckinSettings: (enabled: boolean): Promise<{ enabled: boolean }> =>
    apiFetch('/api/settings/checkin', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ enabled }),
    }).then(j<{ enabled: boolean }>),
  // Usage-statistics and crash-report choices (daemon/privacy_settings.go).
  // null = older daemon without the endpoint, so the rows stay hidden.
  // null = older daemon without the endpoint, so the row and prompt stay hidden.
  getAgentIntegration: async (): Promise<AgentIntegrationSettings | null> => {
    const res = await apiFetch('/api/settings/agent-integration');
    if (res.status === 404) return null;
    if (!res.ok) throw new Error(`agent integration: ${res.status}`);
    return res.json();
  },
  setAgentIntegration: (state: 'on' | 'off'): Promise<AgentIntegrationSettings> =>
    apiFetch('/api/settings/agent-integration', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ state }),
    }).then(j<AgentIntegrationSettings>),
  ackAgentIntegrationNotice: (): Promise<AgentIntegrationSettings> =>
    apiFetch('/api/settings/agent-integration/notice-ack', { method: 'POST' }).then(j<AgentIntegrationSettings>),
  getPrivacySettings: async (): Promise<PrivacySettings | null> => {
    const res = await apiFetch('/api/settings/privacy');
    if (res.status === 404) return null;
    if (!res.ok) throw new Error(`privacy settings: ${res.status}`);
    return res.json();
  },
  // null = older daemon without the personal view.
  getPersonalView: async (): Promise<import('./types').PersonalViewSettings | null> => {
    const res = await apiFetch('/api/view');
    if (res.status === 404) return null;
    if (!res.ok) throw new Error(`personal view: ${res.status}`);
    return res.json();
  },
  setPersonalView: (personal: boolean): Promise<import('./types').PersonalViewSettings> =>
    apiFetch('/api/view', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ personal }),
    }).then(j<import('./types').PersonalViewSettings>),
  // null = older daemon without the endpoint, so the group stays hidden.
  listRemoteDevices: async (): Promise<RemoteDevicesState | null> => {
    const res = await apiFetch('/api/remote-devices');
    if (res.status === 404) return null;
    if (!res.ok) throw new Error(`remote devices: ${res.status}`);
    return res.json();
  },
  // Enrolment and status go to the exact daemon (machine/backend) that asked:
  // each backend keeps its own list of approved devices.
  enrollRemoteDevice: (target: string, name: string, publicKey: string): Promise<RemoteDevice> =>
    rootFetch(`${relayPrefixFor(target)}/api/remote-devices/enroll`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ name, publicKey, codeVersion: 2 }),
    }).then(j<RemoteDevice>),
  remoteDeviceStatus: (target: string, id: string): Promise<RemoteDevice> =>
    rootFetch(`${relayPrefixFor(target)}/api/remote-devices/${encodeURIComponent(id)}/status`).then(j<RemoteDevice>),
  approveRemoteDevice: (id: string): Promise<RemoteDevice> =>
    apiFetch(`/api/remote-devices/${encodeURIComponent(id)}/approve`, { method: 'POST' }).then(j<RemoteDevice>),
  removeRemoteDevice: (id: string): Promise<void> =>
    apiFetch(`/api/remote-devices/${encodeURIComponent(id)}`, { method: 'DELETE' }).then((r) => {
      if (!r.ok) throw new Error(`remove device: ${r.status}`);
    }),
  setRemoteAccessPolicy: (policy: 'any' | 'approved'): Promise<RemoteDevicesState> =>
    apiFetch('/api/remote-devices/policy', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ policy }),
    }).then(j<RemoteDevicesState>),
  // null = older daemon without the endpoint, so the row stays hidden.
  getAgentAuthority: async (): Promise<AgentAuthoritySettings | null> => {
    const res = await apiFetch('/api/settings/agent-authority');
    if (res.status === 404) return null;
    if (!res.ok) throw new Error(`agent permissions: ${res.status}`);
    return res.json();
  },
  // hours 0 = until turned off.
  setAgentAuthority: (change: { panes: boolean; hours?: number }): Promise<AgentAuthoritySettings> =>
    apiFetch('/api/settings/agent-authority', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(change),
    }).then(j<AgentAuthoritySettings>),
  setPrivacySettings: (
    change: Partial<Pick<PrivacySettings, 'usageUpload' | 'crashReports'>>,
  ): Promise<PrivacySettings> =>
    apiFetch('/api/settings/privacy', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(change),
    }).then(j<PrivacySettings>),
  getRecoverySettings: (): Promise<RecoverySettings> =>
    apiFetch('/api/recovery/settings').then(j<RecoverySettings>),
  putRecoverySettings: (body: Partial<RecoverySettings>): Promise<RecoverySettings> =>
    apiFetch('/api/recovery/settings', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    }).then(j<RecoverySettings>),
  getNativeSettings: (): Promise<NativeSettings> =>
    apiFetch('/api/native/settings').then(j<NativeSettings>),
  putNativeSettings: (body: Partial<NativeSettings>): Promise<NativeSettings> =>
    apiFetch('/api/native/settings', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    }).then(j<NativeSettings>),
  // Warden — opt-in fleet caretaker. Settings + status + run-now + run-log.
  // The daemon enforces its own tool scope; these are the local control plane.
  getWardenSettings: (): Promise<import('./types').WardenSettings> =>
    apiFetch('/api/warden/settings').then(j<import('./types').WardenSettings>),
  setWardenSettings: (
    body: Partial<import('./types').WardenSettings>,
  ): Promise<import('./types').WardenSettings> =>
    apiFetch('/api/warden/settings', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    }).then(j<import('./types').WardenSettings>),
  getWardenStatus: (): Promise<import('./types').WardenStatus> =>
    apiFetch('/api/warden/status').then(j<import('./types').WardenStatus>),
  // Fire an on-demand fleet brief. Works even while disabled (a manual tool);
  // { started:false } when a run is already in flight or the hourly cap is hit.
  wardenRunNow: (): Promise<{ started: boolean; reason?: string }> =>
    apiFetch('/api/warden/run-now', { method: 'POST' }).then(
      j<{ started: boolean; reason?: string }>,
    ),
  // Full text of one Warden run (raw markdown, same dialect as task reports).
  wardenRunLog: (name: string): Promise<string> =>
    apiFetch(`/api/warden/runs/${encodeURIComponent(name)}`).then((r) => r.text()),
  getUpdateSettings: (): Promise<UpdateSettings> =>
    apiFetch('/api/update/settings').then(j<UpdateSettings>),
  putUpdateSettings: async (body: Partial<UpdateSettings>): Promise<UpdateSettings> => {
    const init: RequestInit = {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    };
    if (!isRouterMode()) {
      return apiFetch('/api/update/settings', init).then(j<UpdateSettings>);
    }

    // Update preference is product-wide, not backend-specific. Persist it on
    // both daemons in Dual mode so an inactive Windows/WSL sibling does not
    // remain on the default `off` policy forever. The daemon-side background
    // poll then applies each signed update independently when that backend is
    // idle, even when no webview is currently pointed at it.
    const active = currentBackend() ?? 'wsl';
    const sibling: BackendId = active === 'wsl' ? 'win' : 'wsl';
    const [activeSettings] = await Promise.all([
      apiFetchBackend(active, '/api/update/settings', init).then(j<UpdateSettings>),
      apiFetchBackend(sibling, '/api/update/settings', init).then(j<UpdateSettings>),
    ]);
    return activeSettings;
  },

  // Self-update — see daemon/updater.go. status reads cached state,
  // check forces a fresh manifest fetch, apply triggers download +
  // swap + restart-via-Tauri-respawn. apply() resolves before the
  // daemon exits (response flushed first), so callers should treat
  // resolution as "the daemon is about to die".
  getUpdateStatus: () =>
    apiFetch('/api/update/status').then(j<import('./types').UpdateStatus>),
  checkForUpdate: () =>
    apiFetch('/api/update/check', { method: 'POST' })
      .then(j<import('./types').UpdateStatus>),
  // Preview / Stable channel (owner's PC only; see PreviewChannelStatus).
  getPreviewChannel: () =>
    apiFetch('/api/update/preview').then(j<import('./types').PreviewChannelStatus>),
  installPreviewChannel: (target: 'preview' | 'stable') =>
    apiFetch('/api/update/preview/install', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ target }),
    }).then(j<{ started: boolean }>),
  // force=true is reserved for the explicitly-labelled user override of the
  // daemon's Focus and workflow-drain gates. Normal typed 409 holds retry
  // without force so all server-side checks remain authoritative.
  applyUpdate: (force?: boolean) =>
    apiFetch(`/api/update/apply${force ? '?force=1' : ''}`, { method: 'POST' })
      .then(j<{ applying: boolean }>),
  // Download + launch the Tauri-shell installer (NSIS .exe on Windows).
  // Daemon stays alive; the installer prompts for UAC, closes the app,
  // installs over it, and (per Tauri NSIS template) relaunches.
  // The daemon answers only after it downloaded the whole installer (on the
  // team channel, streamed through the cloud) and saw it start, which can take
  // minutes. The default 30 s bound would abort the request, and with it the
  // daemon's download, partway through.
  // silent: the unattended update (daemon/updater_shell_silent.go). The
  // daemon stages the same verified installer, then a courier installs it with
  // no window once this app exits; the answer carries silent:true. It never
  // combines with a forced downgrade.
  applyShellUpdate: (forceDowngrade = false, opts?: { silent?: boolean; appPid?: number | null }) => {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), SHELL_APPLY_TIMEOUT_MS);
    const query = opts?.silent ? '?silent=1' : forceDowngrade ? '?force=1' : '';
    // The silent update's courier waits on the app's own process when the
    // page knows it; the query stays exact, so an older daemon ignores it.
    const init: RequestInit = opts?.silent && opts.appPid
      ? { method: 'POST', signal: ctrl.signal, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ appPid: opts.appPid }) }
      : { method: 'POST', signal: ctrl.signal };
    return apiFetch(`/api/update/apply-shell${query}`, init)
      .then(j<{ applying: boolean; installerPath?: string; silent?: boolean }>)
      .finally(() => clearTimeout(timer));
  },
  getRollbackTargets: () =>
    apiFetch('/api/update/rollback')
      .then(j<import('./types').RollbackTargetsResponse>),
  rollbackUpdate: (request: { targetVersion: string; holdHours?: number; force?: boolean }) =>
    apiFetch('/api/update/rollback', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ...request, confirm: true }),
    }).then(j<{ applying: boolean; targetVersion: string; hold?: import('./types').RollbackVersionHold }>),
  releaseUpdateHold: () =>
    apiFetch('/api/update/rollback/hold', { method: 'DELETE' })
      .then(j<import('./types').UpdateStatus>),
  revealShellInstaller: () =>
    apiFetch('/api/update/shell/reveal', { method: 'POST' })
      .then(j<{ ok: boolean; path?: string }>),

  // tzName/tzOffsetMinutes describe the DAEMON's timezone (cron runs in the
  // machine's wall clock, not the browser's) — absent on older daemons.
  previewCron: (cron: string, count = 5) =>
    apiFetch(
      `/api/schedules/preview?cron=${encodeURIComponent(cron)}&count=${count}`,
    ).then(j<{ fires: string[]; tzName?: string; tzOffsetMinutes?: number }>),

  // Captured output of one agent-kind schedule run (see ScheduleRun.logFile).
  scheduleRunLog: (id: string, file: string) =>
    apiFetch(
      `/api/schedules/${id}/runlog?file=${encodeURIComponent(file)}`,
    ).then((r) => r.text()),

  // Advance the reports-reader read marker (monotonic daemon-side, so a
  // stale tab can't un-read newer runs). Fire-and-forget from the reader.
  markScheduleRead: (id: string, at: string) =>
    apiFetch(`/api/schedules/${id}/read`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ at }),
    }).then(() => undefined),

  // Raw PTY write into a pane. Useful for "send this prompt now"
  // without creating a recurring schedule.
  injectPane: (id: string, text: string, scope?: RoutingScope) =>
    workflowFetch(scope, `/api/panes/${id}/inject`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ text }),
    }).then(j<{ bytes: number }>),

  actOnUnsentMessage: (id: string, messageId: string, action: 'send' | 'clear') =>
    apiFetch(`/api/panes/${id}/unsent-message`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ id: messageId, action }),
    }).then(j<{ outcome: 'submitted' | 'cleared' | 'left_in_composer' | 'sent_unconfirmed' }>),

  submitPaneMessage: (id: string, text: string) =>
    apiFetch(`/api/panes/${id}/submit-message`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ text }),
    }).then(j<{ bytes: number; outcome: 'submitted' | 'left_in_composer' | 'sent_unconfirmed' }>),

  // ARIA autoresponder controls. pause flips the global kill switch;
  // status fetches paused state + auto-fires in the last hour.
  ariaStatus: () =>
    apiFetch('/api/aria/status').then(j<{
      paused: boolean;
      auto_fires_hr: number;
    }>),
  ariaAutoPending: () =>
    apiFetch('/api/aria/auto-pending').then(j<{ pending: Array<{
      id: string;
      pane_id: string;
      pane_title: string;
      input: string;
      order_id: string;
      auto_countdown_ms: number;
      deadline_at_ms: number;
    }> }>),
  ariaPause: (paused: boolean) =>
    apiFetch('/api/aria/pause', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ paused }),
    }).then(j<{
      paused: boolean;
      auto_fires_hr: number;
    }>),
  ariaResetCap: () =>
    apiFetch('/api/aria/cap/reset', { method: 'POST' }).then(j<{
      paused: boolean;
      auto_fires_hr: number;
    }>),
  // Cloud-synced per-user UI preferences (sidebar layout etc). The cloud
  // serves these itself (rootFetch — no per-machine relay). Returns an empty
  // {} + null timestamp when the user has nothing saved yet.
  getUserPrefs: () =>
    rootFetch('/api/user/prefs').then(j<{ prefs: Record<string, unknown>; updated_at: string | null }>),
  putUserPrefs: (prefs: Record<string, unknown>) =>
    rootFetch('/api/user/prefs', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ prefs }),
    }).then(j<{ prefs: Record<string, unknown>; updated_at: string }>),
  // Product-update email consent (marketingConsent.ts). Served by the cloud;
  // the desktop daemon forwards it with the signed-in user's own token.
  getMarketingConsent: (accountToken?: string | null) =>
    rootFetch(
      '/api/user/marketing-consent',
      accountToken ? { headers: { Authorization: `Bearer ${accountToken}` } } : undefined,
    ).then(j<import('./marketingConsent').MarketingConsentState>),
  putMarketingConsent: (consent: boolean, wordingVersion: string) =>
    rootFetch('/api/user/marketing-consent', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ consent, source: 'app-settings', wording_version: wordingVersion }),
    }).then(j<import('./marketingConsent').MarketingConsentState>),
  postMarketingConsent: (
    body: { consent: boolean; source: string; wording_version: string },
    accountToken?: string | null,
  ) =>
    rootFetch('/api/user/marketing-consent', {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        ...(accountToken ? { Authorization: `Bearer ${accountToken}` } : {}),
      },
      body: JSON.stringify(body),
    }).then(j<import('./marketingConsent').MarketingConsentState>),
  // Per-account Claude Code usage summary (Usage Phase 2). Daemon proxies
  // this to the cloud (machine token added server-side), which reads the
  // soluterminal_cc_usage table. Works in both local and cloud mode,
  // `window` is one of 7d|30d|90d|all.
  // See docs/USAGE-PHASE2-CONTRACT.md and the CCUsageSummary type above.
  // `provider` (Codex provider contract) narrows to one CLI; omitted / "all"
  // sends the unfiltered URL.
  ccUsageSummary: (window: string, signal?: AbortSignal, provider?: UsageProvider) =>
    usageFetch<CCUsageSummary>(ccUsagePath(window, provider), signal),
  ccUsageSummaryCached: (
    window: string,
    opts?: { force?: boolean; signal?: AbortSignal; provider?: UsageProvider },
  ) => ccUsageCached<CCUsageSummary>(ccUsagePath(window, opts?.provider), opts),
  ccUsageSummaryPeek: (window: string, provider?: UsageProvider) =>
    ccUsagePeek<CCUsageSummary>(ccUsagePath(window, provider)),
  ccUsageSummaryPrefetch: (window: string, provider?: UsageProvider) =>
    ccUsagePrefetch<CCUsageSummary>(ccUsagePath(window, provider)),
  // Cheap team-membership probe. New clouds return CCUsageTeamContext; older
  // clouds ignore context=1 and return a full summary, which the UI can reuse.
  ccUsageTeamContext: (window: string, signal?: AbortSignal) =>
    usageFetch<CCUsageTeamContext | CCUsageTeamSummary>(ccUsageTeamContextPath(window), signal),
  ccUsageTeamContextCached: (
    window: string,
    opts?: { force?: boolean; signal?: AbortSignal },
  ) => ccUsageCached<CCUsageTeamContext | CCUsageTeamSummary>(ccUsageTeamContextPath(window), opts),
  ccUsageTeamContextPeek: (window: string) =>
    ccUsagePeek<CCUsageTeamContext | CCUsageTeamSummary>(ccUsageTeamContextPath(window)),
  // (Phase 3) Per-TEAM Claude Code usage rollup. Same dual-auth + daemon-proxy
  // path as ccUsageSummary; the cloud joins soluterminal_cc_usage to the
  // existing team model at query time. `window` is one of 7d|30d|90d|all;
  // `teamId` is optional (defaults server-side to the caller's primary team).
  // The response carries the per-account summary shape PLUS team/teams/by_member
  // (the latter privacy-gated server-side). See docs/USAGE-PHASE2-CONTRACT.md
  // §"Phase 3 — Team summary" and the CCUsageTeamSummary type above.
  ccUsageTeamSummary: (
    window: string,
    teamId?: string,
    signal?: AbortSignal,
    provider?: UsageProvider,
  ) => {
    return usageFetch<CCUsageTeamSummary>(ccUsageTeamPath(window, teamId, provider), signal);
  },
  ccUsageTeamSummaryCached: (
    window: string,
    teamId?: string,
    opts?: { force?: boolean; signal?: AbortSignal; provider?: UsageProvider },
  ) =>
    ccUsageCached<CCUsageTeamSummary>(ccUsageTeamPath(window, teamId, opts?.provider), opts),
  ccUsageTeamSummaryPeek: (window: string, teamId?: string, provider?: UsageProvider) =>
    ccUsagePeek<CCUsageTeamSummary>(ccUsageTeamPath(window, teamId, provider)),
  ccUsageTeamSummaryPrefetch: (window: string, teamId?: string, provider?: UsageProvider) =>
    ccUsagePrefetch<CCUsageTeamSummary>(ccUsageTeamPath(window, teamId, provider)),
  // (Phase 3.1) ONE member's FULL per-account detail — the owner drill-down from
  // a team `by_member` row. Same dual-auth + daemon-proxy path as the other
  // cc-usage calls. The server AUTHORIZES (own detail, or the caller owns a team
  // the user belongs to) and returns 403 otherwise; callers branch on the
  // ApiError status to show "not authorized" and fall back to the team view.
  // `window` is one of 7d|30d|90d|all; `teamId` is optional (narrows the
  // ownership check to that team). The response is the per-account summary shape
  // PLUS a `member` meta block. See docs/USAGE-PHASE2-CONTRACT.md
  // §"Phase 3.1 — Member detail" and the CCUsageMemberSummary type above.
  ccUsageMemberSummary: (
    userId: string,
    window: string,
    teamId?: string,
    signal?: AbortSignal,
    provider?: UsageProvider,
  ) => {
    return usageFetch<CCUsageMemberSummary>(ccUsageMemberPath(userId, window, teamId, provider), signal);
  },
  ccUsageMemberSummaryCached: (
    userId: string,
    window: string,
    teamId?: string,
    opts?: { force?: boolean; signal?: AbortSignal; provider?: UsageProvider },
  ) =>
    ccUsageCached<CCUsageMemberSummary>(
      ccUsageMemberPath(userId, window, teamId, opts?.provider),
      opts,
    ),
  ccUsageMemberSummaryPeek: (
    userId: string,
    window: string,
    teamId?: string,
    provider?: UsageProvider,
  ) => ccUsagePeek<CCUsageMemberSummary>(ccUsageMemberPath(userId, window, teamId, provider)),
  ariaConfirm: (id: string, approve: boolean) =>
    apiFetch('/api/aria/confirm', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ id, approve }),
    }),
  // Per-pane ring buffer of the last few autonomous-eval outcomes
  // (newest first). Powers the "eval log" popover the panel opens off
  // each auto-mode pane row so the user can see WHY ARIA declined /
  // errored / fired instead of only seeing a "backed off" badge.
  ariaPaneEvals: (paneId: string) =>
    apiFetch(`/api/aria/panes/${encodeURIComponent(paneId)}/evals`).then(j<{
      pane_id: string;
      outcomes: Array<{
        at: string;
        pane_id: string;
        kind: string;
        reason?: string;
        tail_snapshot?: string;
        model?: string;
        latency_ms: number;
        tool_calls: number;
        send_input?: string;
      }>;
    }>),
  // Cross-pane decision feed: most recent auto-approval outcomes across ALL
  // panes (newest first), each enriched with pane title/kind/auto-mode. The
  // single place to answer "auto-approval is on but this prompt didn't go
  // through" — esp. native panes parked on AskUserQuestion, which ARIA never
  // auto-answers and which otherwise leave no trace.
  ariaRecent: (limit = 100) =>
    apiFetch(`/api/aria/recent?limit=${limit}`).then(j<{
      decisions: Array<{
        at: string;
        pane_id: string;
        kind: string;
        reason?: string;
        tail_snapshot?: string;
        model?: string;
        latency_ms: number;
        tool_calls: number;
        send_input?: string;
        pane_title?: string;
        pane_kind?: string;
        auto_mode?: string;
      }>;
    }>),
};

// A feature may only add methods: replacing a core one would change it for
// every caller, so it fails at load instead.
export function mergeFeatureApis<T extends object>(core: T, features: object[]): T {
  for (const feature of features) {
    for (const key of Object.keys(feature)) {
      if (key in core) throw new Error(`a feature API redefines api.${key}`);
    }
    Object.assign(core, feature);
  }
  return core;
}

export const api = mergeFeatureApis(
  coreApi,
  Object.keys(featureApis).sort().map((path) => featureApis[path].default),
) as typeof coreApi & FeatureApi;

// Fetch the daemon's resolved enter-gap timings and push them into ptySubmit
// so every submit path (voice auto-send, broadcast, mobile quick-reply, ARIA)
// honors any SOLU_ENTER_GAP_* env override without a rebuild. Best-effort and
// fail-safe: if the call rejects (older daemon → 404, cloud relay, network
// blip) ptySubmit keeps its compiled-in 400/100 defaults, so a submit before
// (or without) this load is never broken. Idempotent — safe to re-run on
// reconnect / machine switch.
export async function loadEnterGapConfig(): Promise<void> {
  try {
    const scope = currentRoutingScope().key;
    const cfg = await api.enterGapConfig();
    if (currentRoutingScope().key === scope) setEnterGapConfig(cfg);
  } catch {
    /* keep ptySubmit's safe defaults */
  }
}

// main.tsx loads this after preview transport and backend routing initialize.
// Importing API helpers must not make requests before those boundaries exist.
