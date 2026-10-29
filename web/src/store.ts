import { holdTerminalPlacement, reconcileConfirmedTerminalPanes } from './lib/terminalViewSessions';
import { getMachineAccessState, subscribeMachineAccess } from './lib/machineAccess';
import { clearOwnerURL, type MainPresentation, type WorkspaceReturnContext } from './lib/ownerView';
import { currentRoutingScope, routingPrincipalAllowed, onRoutingScopeChange, type RoutingScope } from './api';
import { flushSync } from 'react-dom';
import { closedNeighbor, closedPaneLabel, rememberClosedNeighbor, type ClosedPane } from './lib/closedPanes';
import { create, type StateCreator } from 'zustand';
import { bindSavedBrowser, reconnectServiceBrowsers } from './lib/ports';
import type { BrowserServiceTarget } from './types';
import { isPreviewBuild } from './previewBoot';
import { isPreviewAcceptanceHost } from './previewAcceptanceShell';
import { createToastDialogSlice, type ToastDialogSlice } from './store/toastSlice';
import { api, ApiError, apiErrorCode, currentPrefix, isDeviceNotApprovedError, setBackend as apiSetBackend } from './api';
import { useCloud } from './cloud';
import { debugLog, isSoluDebugEnabled, lifecycleLog } from './debug';
import { activeBackendFromUrl, isRouterMode, pendingWorkspace } from './dualBackend';
import { subscribePushToCloud, unsubscribePushFromCloud, rotatePushSubscription, isPushSubStale } from './push';
import { DEFAULT_LIGHT_THEME_NAME, DEFAULT_THEME_NAME, findTheme, type TerminalTheme } from './themes';
import { APP_THEME_KEY, initialAppThemePref } from './appThemeDefault';
import { getVoiceMode, isTauri, setVoiceMode, type VoiceMode } from './voice';
import { isEmbedded } from './embed';
import { type NativeMetaBarFields, NATIVE_METABAR_DEFAULTS, sanitizeMetaBarFields } from './native/metaBarFields';
import { clearVitalsSnapshot } from './native/vitalsSnapshot';
import { prettyModelName } from './native/modelInfo';
import { hasAnyUnsentDraft, unsentDraftPaneId } from './native/useNativePane';
import { applyShellUpdateSilentNative, exitForUpdateNative, getAppProcessId, getShellVersion, isDesktopApp, type ShellUpdateNativeResult } from './tauri-browser';
import { isCloudLaunchMode, isVersionNewer } from './lib/useCloudShellUpdate';
import { daemonOnThisComputer, shellAutoApplyEnabled, shellUpdateNeedsDaemon } from './lib/shellUpdateRoute';
import { hasRegisteredUnsentInput } from './lib/unsentInput';
import type { BrowserInfo, FileTileInfo, Layout, McpServer, NativeSettings, OrchestratorHostInfo, PaneAutoMode, PaneEffort, PaneInfo, PaneRole, Port, RecoveryResumeMode, RecoverySettings, RollbackDecline, Schedule, ScheduleInput, ShellAutoRestart, UpdateAutoApplyMode, UpdateHold, UpdateSettings, UpdateStatus, WardenSettings, Workspace, WorkspaceSummary } from './types';
import { agentLabel, effectivePaneAgent } from './lib/agentKind';
import {
  buildNxMFromLeaves,
  buildPresetLayout,
  collectBrowserIds,
  collectDiffIds,
  collectTasksIds,
  collectFileIds,
  collectLeaves,
  collectPaneIds,
  insertLeaf,
  insertPane,
  layoutHasLeaf,
  detachManagedOwnerPresentation,
  leafId,
  movePaneInLayoutImpl,
  pruneLayout,
  remapLayoutIds,
  removePane,
  updateDiffLeaf,
  type LayoutPresetId,
} from './lib/layout';
import { collectBrowserInfos, collectFileInfos, collectPaneDefs, failedPaneFromDef, paneRestoreRequest, paneRetryRequest, dropUnconfirmedSeeds, isPresentPane, isRunningPane, optimisticPaneFromDef, pickWorkspaceByName } from './lib/workspace';
import { isMobileViewport } from './lib/mobile';
import { windowScopedKey, windowLabel, isPrimaryWindow } from './lib/windowIdentity';
import { checkOwnedByOther, claimWorkspace, releaseWorkspace, startOwnershipHeartbeat } from './lib/workspaceOwnership';
import { confirmDialog } from './lib/dialog';
import { acquireWorkspaceView, installWorkspaceHandoff, resumeWorkspaceView, suspendWorkspaceView, workspaceMovedTo, workspaceViewVisible } from './lib/workspaceHandoff';
import type { DropZone } from './lib/paneDnd';
import {
  bundleReloadTarget,
  classifyBundleParity,
  newestVersion,
  shouldAutoReloadBundle,
  type BundleParityState,
} from './lib/bundleParity';
import { getDaemonHealthState, refreshDaemonHealth, subscribeDaemonHealthState } from './lib/useDaemonHealth';
import { slotValue } from './extensions';
import { defaultBrowserUrl } from './lib/browserStart';
export type PaneSnapshotFn = (opts?: { screen?: boolean }) => string;
export type { DropZone } from './lib/paneDnd';
export { leafCount, paneIdsInOrder } from './lib/layout';
export type { LayoutPresetId } from './lib/layout';
export type { Toast, ToastKind, ToastPosition, DialogRequest, NotificationEntry } from './store/toastSlice';

export interface ApplyAllUpdatesResult {
  downgradeRefused: boolean;
  installedVersion?: string;
  installerVersion?: string;
}

// An explicitly-chosen scheme (even 'solu dark') always wins; only
// never-touched defaults follow the effective app appearance.
export function resolveTerminalTheme(s: {
  terminalThemeName: string;
  terminalThemeExplicit: boolean;
  appThemeLight: boolean;
}): TerminalTheme {
  if (!s.terminalThemeExplicit && s.appThemeLight) return findTheme(DEFAULT_LIGHT_THEME_NAME);
  return findTheme(s.terminalThemeName);
}

const CURRENT_WS = '_current';

// A daemon update (swap) jumbles a NATIVE pane that's mid-turn — the stream-json
// reattach can't cleanly rejoin a half-streamed message — while terminal panes recover
// via PTY redraw. So applyUpdate holds the swap only while a native turn is genuinely
// active, then retries normally so every daemon-side safety gate is re-evaluated.
export function nativePaneBusy(panes: Record<string, PaneInfo>): boolean {
  return Object.values(panes).some((p) => p.kind === 'native' && p.agentMidTurn === true);
}

function nativeBusyPaneId(panes: Record<string, PaneInfo>): string | undefined {
  return Object.values(panes).find((p) => p.kind === 'native' && p.agentMidTurn === true)?.id;
}

const DEFERRED_UPDATE_POLL_MS = 4000;
let deferredUpdateRetryTimer: number | null = null;
let deferredUpdateRetryGeneration = 0;
let deferredUpdateRetryVersion: string | undefined;
let deferredUpdateRetryTick = 0;

export function cancelDeferredUpdateRetry() {
  deferredUpdateRetryGeneration += 1;
  if (deferredUpdateRetryTimer !== null) {
    window.clearTimeout(deferredUpdateRetryTimer);
    deferredUpdateRetryTimer = null;
  }
}

function queueDeferredUpdate(next: UpdateHold, auto: boolean) {
  const get = useStore.getState;
  const version = get().updateStatus?.latest;
  const previous = get().deferredUpdate;
  const sameUpdate = !!previous
    && deferredUpdateRetryVersion === version
    && previous.rollbackTarget === next.rollbackTarget;
  const hold: UpdateHold = sameUpdate
    ? { ...previous, ...next, since: previous.since }
    : next;
  // Cause/detail changes update the already-mounted affordance in place.
  // Announce only the initial hold; Focus activity during a longer workflow
  // drain must not generate two new toasts on every cause round-trip.
  const announce = !sameUpdate;

  cancelDeferredUpdateRetry();
  if (!sameUpdate) deferredUpdateRetryTick = 0;
  deferredUpdateRetryVersion = version;
  useStore.setState({ deferredUpdate: hold });

  if (announce) {
    const operation = hold.rollbackTarget ? 'Rollback' : 'Update';
    const continuation = hold.rollbackTarget ? 'rolling back' : 'installing';
    let message: string;
    if (hold.cause === 'focus') {
      const pane = hold.paneId ? ` ${hold.paneId}` : '';
      message = `${operation} queued — Focus pane${pane}'s active turn must finish first`;
    } else if (hold.cause === 'workflow') {
      const count = hold.workflowInflight;
      const invocations = count
        ? `${count} in-flight workflow invocation${count === 1 ? '' : 's'}`
        : 'critical workflow invocations';
      const remaining = hold.workflowDeferDeadlineMs
        ? Math.ceil((hold.workflowDeferDeadlineMs - Date.now()) / 60000)
        : null;
      const deadline = remaining === null
        ? ''
        : remaining > 0
          ? ` (deadline in ${remaining}m)`
          : ' (drain deadline reached)';
      message = `${operation} queued — waiting for ${invocations} to drain${deadline}`;
    } else {
      message = auto
        ? `${operation} ready — ${continuation} when active work finishes`
        : `${operation} queued — ${continuation} when active work finishes`;
    }
    get().toast(message, { kind: 'info', icon: '↑', ttlMs: 6000 });
  }

  const generation = deferredUpdateRetryGeneration;
  const retryDelay = next.workflowNextRetryMs
    ? Math.max(DEFERRED_UPDATE_POLL_MS, next.workflowNextRetryMs - Date.now())
    : DEFERRED_UPDATE_POLL_MS;
  deferredUpdateRetryTimer = window.setTimeout(async () => {
    if (generation !== deferredUpdateRetryGeneration) return;
    deferredUpdateRetryTimer = null;
    let current = get().deferredUpdate;
    if (!current) return;
    if (deferredUpdateRetryVersion !== get().updateStatus?.latest) {
      cancelDeferredUpdateRetry();
      deferredUpdateRetryVersion = undefined;
      useStore.setState({ deferredUpdate: null });
      return;
    }

    deferredUpdateRetryTick += 1;
    if (current.cause === 'focus' && nativePaneBusy(get().panes)) {
      if (deferredUpdateRetryTick % 5 === 0) {
        await get().refreshPanes();
        current = get().deferredUpdate;
        if (!current) return;
      }
      if (nativePaneBusy(get().panes)) {
        queueDeferredUpdate(current, current.auto);
        return;
      }
    } else if (current.cause !== 'focus') {
      await get().refreshUpdateStatus();
      current = get().deferredUpdate;
      if (!current) return;
      if (current.workflowNextRetryMs && current.workflowNextRetryMs > Date.now()) {
        queueDeferredUpdate(current, current.auto);
        return;
      }
    }

    // An unattended swap held for other work also waits out text typed in a
    // compose box meanwhile: the swap reloads the page.
    if (current.auto && !current.rollbackTarget && hasUnsentInput()) {
      queueDeferredUpdate(current, current.auto);
      return;
    }

    try {
      if (current.rollbackTarget) {
        await get().rollbackUpdate({
          targetVersion: current.rollbackTarget,
          holdHours: current.rollbackHoldHours,
        });
      } else {
        await get().applyUpdate(current.auto ? { auto: true } : {});
      }
    } catch {
      const retry = get().deferredUpdate;
      if (retry && deferredUpdateRetryTimer === null) queueDeferredUpdate(retry, retry.auto);
    }
  }, retryDelay);
}

export const UPDATE_RECOVERY_POLL_MS = 500;
export const UPDATE_RECOVERY_TIMEOUT_MS = 30_000;

interface UpdateRecoveryController {
  generation: number;
  beforeVersion: string;
  targetVersion: string;
  deadlineMs: number;
  skipReload: boolean;
  pollTimer: number | null;
  deadlineTimer: number | null;
  promise: Promise<void>;
  resolve: () => void;
}

interface SettledUpdateRecovery {
  beforeVersion: string;
  targetVersion: string;
  maxApplyAttemptGeneration: number;
}

let updateRecoveryGeneration = 0;
let updateRecovery: UpdateRecoveryController | null = null;
let updateApplyAttemptGeneration = 0;
let updateAdmissionConfirmationGeneration = 0;
let settledUpdateRecovery: SettledUpdateRecovery | null = null;

// Older daemons briefly exposed their internal pre-admission reservation as
// applying=true. Their workflow hold fields are level state and therefore win
// over that legacy edge bit on every client and every response path.
export function normalizeUpdateStatus(status: UpdateStatus): UpdateStatus {
  return status.workflowDeferred && status.applying
    ? { ...status, applying: false }
    : status;
}

// Once-per-version guard + failure backoff for the unattended "install when idle"
// trigger (maybeAutoApplyUpdate). We record the version we last kicked off and
// when, so a repeated status poll doesn't re-fire it — but a transient apply
// failure retries after the cooldown, and a genuinely NEW version resets it.
//
// PERSISTED in localStorage, not module state: the swap path reloads the page
// (post-apply reload, stale-bundle self-heal), and a fresh page with a wiped
// module guard re-fired the apply inside the old daemon's ~600ms exit window —
// observed as a double download + double swap in e2e. The daemon now also
// refuses that window server-side (Updater.swapped), but not re-firing at all
// is the right client behavior. Memory fallback covers a throwing localStorage.
const AUTO_APPLY_RETRY_COOLDOWN_MS = 60 * 1000;
const AUTO_APPLIED_KEY = 'solu.update.autoApplied';
let autoApplyStampMem: { version: string; at: number } | null = null;
function readAutoApplyStamp(): { version: string; at: number } | null {
  try {
    const v = localStorage.getItem(AUTO_APPLIED_KEY);
    if (v) return JSON.parse(v) as { version: string; at: number };
  } catch {
    /* fall through to memory */
  }
  return autoApplyStampMem;
}
function writeAutoApplyStamp(stamp: { version: string; at: number }) {
  autoApplyStampMem = stamp;
  try {
    localStorage.setItem(AUTO_APPLIED_KEY, JSON.stringify(stamp));
  } catch {
    /* memory only */
  }
}

export const SHELL_AUTO_QUIET_MS = 120_000;
export const SHELL_AUTO_COUNTDOWN_MS = 60_000;
export const SHELL_AUTO_HOLD_MS = 3_600_000;
const SHELL_AUTO_RETRY_COOLDOWN_MS = 6 * 60 * 60 * 1000;
export const SHELL_AUTO_MAX_ATTEMPTS = 2;
// The daemon's courier started but found no desktop app to wait for
// (shell_silent_app_not_found, #739): nothing was installed and nothing was
// closed, so that try is not one of the SHELL_AUTO_MAX_ATTEMPTS. It is retried
// after SHELL_AUTO_APP_NOT_FOUND_RETRY_MS, at most
// SHELL_AUTO_APP_NOT_FOUND_PER_DAY times a day per version; past that it
// counts as an ordinary failed attempt, so a lasting mismatch cannot loop.
export const SHELL_AUTO_APP_NOT_FOUND_RETRY_MS = 15 * 60 * 1000;
export const SHELL_AUTO_APP_NOT_FOUND_PER_DAY = 3;
const SHELL_AUTO_DAY_MS = 24 * 60 * 60 * 1000;
const SHELL_AUTO_APPLIED_KEY = 'solu.shellUpdate.autoApplied';
const SHELL_AUTO_HOLD_KEY = 'solu.shellUpdate.holdUntil';
// The last unattended attempt that failed, so its reason can still be read in
// the version menu after the toast is gone.
const SHELL_AUTO_OUTCOME_KEY = 'solu.shellUpdate.lastOutcome';

// retryAt overrides at + SHELL_AUTO_RETRY_COOLDOWN_MS; appNotFoundAt holds the
// times the courier found no app (for the daily bound).
interface ShellAttemptStamp { version: string; at: number; attempts: number; retryAt?: number; appNotFoundAt?: number[] }
interface ShellAutoOutcome { version: string; at: number; code: string; error?: string }
interface ShellHoldStamp { version: string; until: number }
let shellAttemptStampMem: ShellAttemptStamp | null = null;
let shellHoldStampMem: ShellHoldStamp | null = null;
let shellAutoRestartTimer: number | null = null;
let shellAutoRestartArmedAtMs = 0;
let shellAutoCheckInFlight = false;
let lastUserInputMs = Date.now();

function readShellAttemptStamp(): ShellAttemptStamp | null {
  try {
    const value = localStorage.getItem(SHELL_AUTO_APPLIED_KEY);
    if (value) return JSON.parse(value) as ShellAttemptStamp;
  } catch { /* memory fallback */ }
  return shellAttemptStampMem;
}

function writeShellAttemptStamp(stamp: ShellAttemptStamp): void {
  shellAttemptStampMem = stamp;
  try { localStorage.setItem(SHELL_AUTO_APPLIED_KEY, JSON.stringify(stamp)); } catch { /* memory only */ }
}

function clearShellAttemptStamp(version: string): void {
  const current = readShellAttemptStamp();
  if (!current || current.version !== version) return;
  shellAttemptStampMem = null;
  try { localStorage.removeItem(SHELL_AUTO_APPLIED_KEY); } catch { /* memory already cleared */ }
}

// Whether the attempt stamp holds back an unattended try of version now.
function shellAttemptBlocked(stamp: ShellAttemptStamp | null, version: string, now: number): 'gave-up' | 'cooldown' | null {
  if (!stamp || stamp.version !== version) return null;
  if (stamp.attempts >= SHELL_AUTO_MAX_ATTEMPTS) return 'gave-up';
  if (now < shellAttemptRetryAt(stamp)) return 'cooldown';
  return null;
}

function shellAttemptRetryAt(stamp: ShellAttemptStamp): number {
  return typeof stamp.retryAt === 'number' ? stamp.retryAt : stamp.at + SHELL_AUTO_RETRY_COOLDOWN_MS;
}

function readShellAutoOutcome(): ShellAutoOutcome | null {
  try {
    const value = localStorage.getItem(SHELL_AUTO_OUTCOME_KEY);
    if (value) return JSON.parse(value) as ShellAutoOutcome;
  } catch { /* none */ }
  return null;
}

function writeShellAutoOutcome(outcome: ShellAutoOutcome | null): void {
  try {
    if (outcome) localStorage.setItem(SHELL_AUTO_OUTCOME_KEY, JSON.stringify(outcome));
    else localStorage.removeItem(SHELL_AUTO_OUTCOME_KEY);
  } catch { /* not kept */ }
}

function readShellHoldStamp(): ShellHoldStamp | null {
  try {
    const value = localStorage.getItem(SHELL_AUTO_HOLD_KEY);
    if (value) return JSON.parse(value) as ShellHoldStamp;
  } catch { /* memory fallback */ }
  return shellHoldStampMem;
}

function writeShellHoldStamp(stamp: ShellHoldStamp): void {
  shellHoldStampMem = stamp;
  try { localStorage.setItem(SHELL_AUTO_HOLD_KEY, JSON.stringify(stamp)); } catch { /* memory only */ }
}

// "Update now (app + installer)" is ONE click for two steps. applyAllUpdates
// launches the installer first (it must: updating the daemon first can make
// the carried installer a downgrade), and the installer closes and relaunches
// the app. Most installers carry an older daemon than the manifest's latest
// (daemon-only releases carry the last installer forward), so the relaunched
// app still had the daemon update waiting and the user had to click again.
// The click's intent survives the relaunch here; the next page load that sees
// the new desktop app finishes the daemon step itself. It never fires on the
// page that launched the installer (a cancelled installer leaves the app
// running and unchanged), and it lapses after UPDATE_CONTINUE_TTL_MS.
const UPDATE_CONTINUE_KEY = 'solu.update.continueAfterInstaller';
export const UPDATE_CONTINUE_TTL_MS = 2 * 60 * 60 * 1000;
const UPDATE_CONTINUE_PAGE = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;

interface UpdateContinuation { shellTarget: string; daemonTarget: string; at: number; page: string }
let updateContinueMem: UpdateContinuation | null = null;
let updateContinueCheckInFlight = false;

function readUpdateContinuation(): UpdateContinuation | null {
  try {
    const value = localStorage.getItem(UPDATE_CONTINUE_KEY);
    if (value) return JSON.parse(value) as UpdateContinuation;
  } catch { /* memory fallback */ }
  return updateContinueMem;
}

function writeUpdateContinuation(next: UpdateContinuation | null): void {
  updateContinueMem = next;
  try {
    if (next) localStorage.setItem(UPDATE_CONTINUE_KEY, JSON.stringify(next));
    else localStorage.removeItem(UPDATE_CONTINUE_KEY);
  } catch { /* memory only */ }
}

export function noteUserActivity(): void {
  lastUserInputMs = Math.max(Date.now(), lastUserInputMs + 1);
}

export function shellAutoApplyNeedsManual(version: string | undefined): boolean {
  if (!version) return false;
  const stamp = readShellAttemptStamp();
  return !!stamp && stamp.version === version && stamp.attempts > 0;
}

function clearShellAutoRestartTimer(): void {
  if (shellAutoRestartTimer !== null) {
    window.clearInterval(shellAutoRestartTimer);
    shellAutoRestartTimer = null;
  }
}

function disarmShellAutoRestart(): void {
  clearShellAutoRestartTimer();
  shellAutoRestartArmedAtMs = 0;
  if (useStore.getState().shellAutoRestart) useStore.setState({ shellAutoRestart: null });
}

// Text typed and not yet sent: a native pane's draft (kept per pane, mounted
// or not) or any other compose box on this page (lib/unsentInput.ts). Both
// unattended updates wait for it: the daemon swap reloads the page and the
// installer update restarts the app.
export function hasUnsentInput(): boolean {
  return hasAnyUnsentDraft(useStore.getState().panes) || hasRegisteredUnsentInput();
}

// The desktop app's installer update runs only for the app on this computer,
// talking to its own daemon. Not useCloud's mode: a signed-in local install
// reports 'cloud' too (shellUpdateRoute.ts), which kept this path off on every
// official install.
function shellAutoApplyAllowedHere(): boolean {
  return isDesktopApp() && daemonOnThisComputer() && !isMobileViewport();
}

// The silent installer update for this install's channel. On the team channel
// only the local daemon can fetch and verify the team installer; it stages it
// and a courier installs it with no window once the app exits. Elsewhere the
// app's own updater does the same from the public releases.
type SilentShellOutcome = ShellUpdateNativeResult & { interactiveLaunched?: boolean; paused?: boolean; appNotFound?: boolean };

async function applyShellUpdateSilently(status: UpdateStatus | null): Promise<SilentShellOutcome> {
  if (!shellUpdateNeedsDaemon(status)) return applyShellUpdateSilentNative(false);
  try {
    // The courier waits on this exact process; an app too old to say leaves
    // it to the courier's name match.
    const appPid = await getAppProcessId();
    const accepted = await api.applyShellUpdate(false, appPid ? { silent: true, appPid } : { silent: true });
    // A daemon without the silent mode opened the installer window instead.
    // Closing the app now would leave the user with a half-finished wizard.
    if (accepted.silent !== true) return { ok: false, interactiveLaunched: true };
    return { ok: true };
  } catch (error) {
    const body = error instanceof ApiError && error.body && typeof error.body === 'object'
      ? error.body as Record<string, unknown>
      : {};
    const code = apiErrorCode(error);
    // Paused by the operator between the last status read and now: a state,
    // not a failure.
    if (code === 'shell_silent_paused') return { ok: false, paused: true };
    // The courier found no desktop app to wait for; nothing was installed.
    if (code === 'shell_silent_app_not_found') {
      return { ok: false, appNotFound: true, error: typeof body.message === 'string' ? body.message : undefined };
    }
    return {
      ok: false,
      // A typed refusal gets the caller's own copy, not the raw HTTP text.
      error: code ? undefined : error instanceof Error ? error.message : String(error),
      downgradeRefused: code === 'shell_daemon_downgrade_refused',
      ...(code === 'update_held' ? { updateHeld: true } : {}),
      installedVersion: typeof body.installedVersion === 'string' ? body.installedVersion : undefined,
      installerVersion: typeof body.installerVersion === 'string'
        ? body.installerVersion
        : typeof body.heldVersion === 'string' ? body.heldVersion : undefined,
    };
  }
}

function shellHoldCopy(reason: string | undefined): string {
  switch (reason) {
    case 'agent mid-turn': return 'an agent is mid-turn';
    case 'approval pending': return 'an approval is waiting for you';
    case 'pane needs user input': return 'a pane is waiting for your answer';
    case 'workflow invocation in flight': return 'a workflow is still running';
    case 'unsent draft': return 'a message box has text you have not sent';
    default: return reason ?? 'active work';
  }
}

// paneId names the pane that holds the update, when one does.
function shellUpdateSafetyGate(): { idle: boolean; reason?: string; paneId?: string } {
  const state = useStore.getState();
  const busy = nativeBusyPaneId(state.panes);
  if (busy) return { idle: false, reason: 'agent mid-turn', paneId: busy };
  const approval = Object.values(state.panes).find((pane) => !!pane.pendingApproval);
  if (approval) return { idle: false, reason: 'approval pending', paneId: approval.id };
  const asking = Object.values(state.panes).find((pane) => pane.attention === 'needs-user');
  if (asking) return { idle: false, reason: 'pane needs user input', paneId: asking.id };
  if (state.updateStatus?.workflowDeferred || (state.updateStatus?.workflowInflight ?? 0) > 0) {
    return { idle: false, reason: 'workflow invocation in flight' };
  }
  const draftPane = unsentDraftPaneId(state.panes);
  if (draftPane) return { idle: false, reason: 'unsent draft', paneId: draftPane };
  if (hasRegisteredUnsentInput()) return { idle: false, reason: 'unsent draft' };
  return { idle: true };
}

function shellWaitPaneName(paneId: string | undefined): string {
  const pane = paneId ? useStore.getState().panes[paneId] : undefined;
  return pane?.titleOverride?.trim() || pane?.title?.trim() || 'a pane';
}

function shellClock(ms: number): string {
  return new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

// Whether the restart is paused by the operator (the signed control file's
// shell_auto_apply_paused, or its global pause) rather than by the channel's
// default. The daemon's reasons (UpdateControl.shellAutoApplyPaused) end in
// "by operator" exactly for those two; the default reads "... not enabled for
// this channel yet". Anything else, or no reason (an older daemon), is not
// treated as an operator pause, so the line stays hidden.
export function shellAutoApplyPausedByOperator(status: UpdateStatus | null | undefined): boolean {
  return status?.shellAutoApplyPaused === true && /\bby operator$/.test(status.shellAutoApplyPausedReason ?? '');
}

export interface ShellAutoApplyWait {
  // waiting: it will restart by itself once this clears. retry: a try failed
  // and the next one waits for the cooldown. gave-up / paused: it will not
  // restart by itself; Update now still works.
  kind: 'waiting' | 'retry' | 'gave-up' | 'paused';
  reason: string;
  // The last failed try's reason, when one is known.
  lastError?: string;
}

// Why the unattended installer update has not run yet: the first of the
// checks maybeAutoApplyShellUpdate makes, in the same order, that holds it
// back, in plain words for the version menu. null when nothing holds it (or
// it does not apply here at all).
export function shellAutoApplyWaitReason(now = Date.now()): ShellAutoApplyWait | null {
  const state = useStore.getState();
  const status = state.updateStatus;
  if (!shellAutoApplyAllowedHere()) return null;
  if (state.updateSettings?.shellAutoApply !== 'when-idle') return null;
  if (!status?.shellLatest || status.shellApplying || status.applying || state.shellAutoRestart) return null;
  const version = status.shellLatest;
  if (!shellAutoApplyEnabled(status)) {
    // Only an operator's pause is news. The channel's own default (public,
    // where automatic restarts are not on yet and the setting is hidden) says
    // nothing at all.
    return shellAutoApplyPausedByOperator(status) ? { kind: 'paused', reason: 'paused' } : null;
  }
  if (shellUpdateNeedsDaemon(status) && !status.shellAvailable) return { kind: 'waiting', reason: 'installer not ready yet' };
  if (status.holdVersion && status.holdVersion === version) return null;
  const hold = readShellHoldStamp();
  if (hold && hold.version === version && hold.until > now) {
    return { kind: 'waiting', reason: `postponed until ${shellClock(hold.until)}` };
  }
  const attempt = readShellAttemptStamp();
  const blocked = shellAttemptBlocked(attempt, version, now);
  if (blocked && attempt) {
    const outcome = readShellAutoOutcome();
    const launch = status.shellLaunch?.state === 'launch-failed' ? status.shellLaunch.error : undefined;
    const lastError = outcome?.version === version && outcome.code === 'shell_silent_app_not_found'
      ? 'the last try did not find the app'
      : launch || (outcome?.version === version ? outcome.error : undefined);
    if (blocked === 'gave-up') return { kind: 'gave-up', reason: `gave up after ${SHELL_AUTO_MAX_ATTEMPTS} tries`, lastError };
    return { kind: 'retry', reason: `cooldown until ${shellClock(shellAttemptRetryAt(attempt))}`, lastError };
  }
  const gate = shellUpdateSafetyGate();
  if (!gate.idle) {
    switch (gate.reason) {
      case 'agent mid-turn': return { kind: 'waiting', reason: `agent working in ${shellWaitPaneName(gate.paneId)}` };
      case 'approval pending':
      case 'pane needs user input':
        return { kind: 'waiting', reason: `approval or question waiting in ${shellWaitPaneName(gate.paneId)}` };
      case 'workflow invocation in flight': return { kind: 'waiting', reason: 'workflow running' };
      case 'unsent draft':
        return { kind: 'waiting', reason: `unsent text in ${gate.paneId ? shellWaitPaneName(gate.paneId) : 'a message box'}` };
      default: return { kind: 'waiting', reason: gate.reason ?? 'active work' };
    }
  }
  if (now - lastUserInputMs < SHELL_AUTO_QUIET_MS) return { kind: 'waiting', reason: 'quiet time left' };
  return null;
}

// The version menu's line for the "install when idle" desktop update: what it
// is waiting for, and once a try failed or it gave up, no promise that it
// restarts by itself (#739).
export function shellAutoWaitLine(wait: ShellAutoApplyWait | null, enabled: boolean): string {
  const withError = (line: string) => (wait?.lastError ? `${line} · ${wait.lastError}` : line);
  if (!wait) return enabled ? '↻ Restarts automatically when idle' : '';
  switch (wait.kind) {
    case 'waiting': return `↻ Restarts automatically when idle — waiting: ${wait.reason}`;
    case 'retry': return withError(`Automatic restart did not work — waiting: ${wait.reason}`);
    case 'gave-up': return withError(`Automatic restart ${wait.reason} — use Update now`);
    case 'paused': return 'Automatic restart is paused — use Update now';
  }
}

export function shellUpdateIdleGate(): { idle: boolean; reason?: string } {
  const safe = shellUpdateSafetyGate();
  if (!safe.idle) return safe;
  if (Date.now() - lastUserInputMs < SHELL_AUTO_QUIET_MS) {
    return { idle: false, reason: 'recent user activity' };
  }
  return { idle: true };
}

type Sender = (bytes: Uint8Array | string) => boolean;

let cloudPaneRefreshDropGuard = 0;
const paneFlagSeq = new Map<string, number>();
const paneRenameSeq = new Map<string, number>();
const paneAutoModeSeq = new Map<string, number>();
const paneAnnounceDelegationSeq = new Map<string, number>();
const paneEffortSeq = new Map<string, number>();
const paneModelSeq = new Map<string, number>();
const paneRenamePending = new Map<string, string>();
// Panes with a resume request in flight. The daemon frees a pane's id while it
// respawns, so a pane-list refresh in that window would drop the tile and show
// "Session ended" until the next refresh brings it back.
const panesResuming = new Set<string>();
const paneActivityLastSet = new Map<string, number>();
const paneActivityPending = new Map<string, number>();
const paneActivityTimers = new Map<string, number>();
const PANE_ACTIVITY_MIN_INTERVAL_MS = 500;

// Shape of events stored in the activity log (mirrors the daemon's
// uiEvent fields we care about). Every optional field is populated by
// some subset of event types; the UI uses `type` as a discriminator
// when rendering rows.
export interface ActivityEvent {
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
  // Cwd reported by Claude Code on tool-call events; surfaced in the
  // Activity row as a fallback label when pane attribution fails, so
  // the user can see "Claude was in /foo when this ran" even if we
  // couldn't match it to one of our panes.
  cwd?: string;
}

const ACTIVITY_LOG_MAX = 500;

/** Latest captured tail for a pane. One per pane (keyed map in the
 *  store), updated whenever the daemon emits an aria-observation SSE
 *  event for the pane OR the panel bootstrap pulls a snapshot of all
 *  panes currently in attention state.
 *
 *  This is the SINGLE SOURCE OF TRUTH for attention rendering: both
 *  the topbar bell and the ARIA panel cards derive from the same
 *  selector that joins live `panes` (current state) with `paneTails`
 *  (latest captured text) and applies `ariaDismissedFingerprints`.
 *  Before this refactor the bell read polled pane.state and the panel
 *  read an SSE-pushed array, and they drifted constantly. */
export interface AriaTail {
  paneId: string;
  paneTitle: string;
  /** "awaiting-input" | "choice-prompt" — what kind of attention. */
  toState: string;
  /** Last N lines of the pane at capture time, ANSI-stripped. */
  text: string;
  /** Server clock, Unix millis. */
  ts: number;
}

// When the user dismisses an observation, remember its
// (paneId, fingerprint) for this long so the same question doesn't
// immediately resurface every time the pane flaps state. 10 minutes
// is long enough to suppress same-session re-fires but short enough
// that a genuinely new question of the exact same shape (rare) will
// eventually get through.
export const ARIA_DISMISSAL_TTL_MS = 10 * 60 * 1000;

/** Build a stable fingerprint from an observation's tail so duplicate
 *  firings (same question, same command) can be merged and dismissals
 *  can persist across re-fires — WITHOUT conflating genuinely different
 *  approvals that happen to share a question like "Do you want to
 *  proceed?".
 *
 *  Strategy: take the last ~10 content lines (chrome-stripped,
 *  decoration-stripped) and join them. A specific approval's prompt
 *  includes the command + its description + the question + the
 *  option list — 10 lines is enough to uniquely capture that shape.
 *  Different commands produce different fingerprints; re-renders of
 *  the same command produce the same fingerprint (time-variant chrome
 *  is filtered out).
 *
 *  Kept lightweight here
 *  — the matching logic is stable and the coupling isn't worth a new
 *  shared module yet.
 */
export function ariaObservationFingerprint(text: string): string {
  const boxChars = /[╭╮╰╯─│┤├┬┴┐┌└┘━┃┏┓┗┛═║╔╗╚╝]/g;
  const decorative = /^[\s•·\-_=*<>]+$/;
  const isDynamicChrome = (l: string): boolean => {
    // Claude's thinking indicators and token counts change on every
    // re-render; stripping them lets a fingerprint stay stable across
    // the pane-state flaps that fire duplicate observations.
    if (l.includes('✻')) return true;
    if (/↓\s*[\d.]+\s*k?\s*tokens/i.test(l)) return true;
    if (/^[A-Z][a-z]+(ed|ing|zed|zing|ling|ting|ning|ming)…/.test(l)) return true;
    if (/\bctx\s*:\s*\d+\s*%/i.test(l)) return true;
    return false;
  };
  const cleaned = text
    .split('\n')
    .map((l) => l.replace(boxChars, ' ').replace(/\s+/g, ' ').trim())
    .filter((l) => l.length >= 3 && !decorative.test(l) && !isDynamicChrome(l));
  return cleaned.slice(-10).join(' | ').toLowerCase().slice(0, 800);
}

/** A pending autonomous action — ARIA's background evaluator decided
 *  to send_input on a standing order's behalf. Rendered with a
 *  countdown; auto-fires after `countdownMs` unless the user cancels
 *  within the window. Mirrors the Confirm type used for manual sends
 *  but separated because auto-actions don't live inside a chat
 *  message. */
export interface AriaAutoPending {
  confirmId: string;    // same id the daemon's /api/aria/confirm uses
  paneId: string;
  paneTitle: string;
  input: string;
  orderId: string;
  startedAt: number;    // client clock at arrival, for countdown math
  deadlineAt?: number;  // daemon clock deadline if supplied by the event
  countdownMs: number;  // usually 5000
  status: 'pending' | 'firing' | 'sent' | 'cancelled' | 'failed' | 'timeout';
  error?: string;
}

const ARIA_AUTO_PENDING_MAX = 10;

export type AppThemePref = 'dark' | 'light' | 'system';

// Store slices of optional features (see extensions.ts). Each
// ./private/**/*.slice.ts default-exports a slice creator and adds its fields
// to FeatureSlices; a tree without ./private has none.
export interface FeatureSlices {}

const featureSlices = import.meta.glob<{ default: StateCreator<State, [], [], object> }>(
  './private/**/*.slice.ts',
  { eager: true },
);

export interface State extends ToastDialogSlice, FeatureSlices {
  // Desktop-only AI voice alerts for high-signal events (approval needed,
  // reply finished, schedule firing soon, schedule fired). 'off' by default;
  // 'sonia' / 'ryan' pick the British female / male voice. Not surfaced
  // on mobile — the phone's system ringtone is already adequate.
  voiceAlertMode: VoiceMode;
  setVoiceAlertMode: (m: VoiceMode) => void;

  panes: Record<string, PaneInfo>;
  layout: Layout | null;
  /** Pane ids kept warm (xterm + WS alive, parked off-screen) after
   *  leaving their workspace, so switching back is a DOM reattach
   *  instead of a full remount + daemon replay. LRU, capped at
   *  WARM_PANE_CAP, desktop only. See WarmPaneLayer. */
  parkedPaneIds: string[];
  ports: Port[];
  portsError: string | null;
  portsUpdatedAt: number | null;
  workspaces: Workspace[];
  schedules: Schedule[];
  orchestratorHosts: OrchestratorHostInfo[] | null;
  loadOrchestratorHosts: () => Promise<void>;

  // Restart recovery controls what the daemon does with resumable Claude panes
  // after a host reboot / WSL shutdown. Manual is the safe default: panes stay
  // visible as resumable tiles until the user clicks Resume.
  recoverySettings: RecoverySettings | null;
  loadRecoverySettings: () => Promise<void>;
  setRecoveryResumeMode: (mode: RecoveryResumeMode) => Promise<void>;
  // Per-install self-update preference. 'when-idle' lets refreshUpdateStatus
  // auto-apply the daemon swap once no native pane is mid-turn (see
  // maybeAutoApplyUpdate); 'off' (default) keeps the notify-then-click banner.
  updateSettings: UpdateSettings | null;
  loadUpdateSettings: () => Promise<void>;
  setUpdateAutoApply: (mode: UpdateAutoApplyMode) => Promise<void>;
  setUpdateShellAutoApply: (mode: UpdateAutoApplyMode) => Promise<void>;
  // Per-machine native (Focus) toggles. promptSuggestions gates the composer
  // ghost text AND the daemon --prompt-suggestions argv (opt-in, default OFF).
  nativeSettings: NativeSettings | null;
  loadNativeSettings: () => Promise<void>;
  setNativePromptSuggestions: (enabled: boolean) => Promise<void>;
  // Persist the default renderer for NEW Claude sessions ('' terminal |
  // 'native' Focus). Daemon-stored (native_settings.json) so every device
  // the user opens agrees; the new-session dialog preselects it.
  setNativeDefaultClaudeMode: (mode: '' | 'native') => Promise<void>;
  // Warden — opt-in fleet caretaker (daemon/warden_settings.go). Default OFF.
  // saveWardenSettings takes a partial patch (enable, model, delay, cap) and
  // applies optimistically with rollback, mirroring setUpdateAutoApply.
  wardenSettings: WardenSettings | null;
  loadWardenSettings: () => Promise<void>;
  saveWardenSettings: (patch: Partial<WardenSettings>) => Promise<void>;

  mainPresentation: MainPresentation;
  ownerReturnContext: WorkspaceReturnContext | null;
  navigationGeneration: number;
  managedOwnerBindings: ReadonlyMap<string,string>;
  detachOwnerPresentation: (bindings: ReadonlyMap<string,string>, scope: RoutingScope) => void;
  returnToWorkspace: () => void;
  activeWorkspace: string;
  activeWorkspaceMoveVersion: number;
  focusedPaneId: string | null;
  // The pane the user most recently selected DELIBERATELY (click, keyboard nav,
  // command palette, reveal, or a fresh spawn the user asked for). Stamped by
  // setFocus and the explicit-spawn paths; left untouched by every background
  // focusedPaneId change (MCP open, ARIA spawn, diff/file/browser tile, workspace
  // switch). The pane focus effects steal the keyboard caret ONLY when this
  // equals the pane — so background activity can re-select a pane WITHOUT yanking
  // the caret out of a composer the user is mid-typing in. Safe-by-default: any
  // new path that forgets to set this simply won't steal focus.
  lastUserFocusId: string | null;
  // Pane most recently brought on-screen by revealPane. The Pane
  // component renders a ~1.5s accent-outlined pulse so the eye can
  // catch the target after a cross-workspace jump — without this, the
  // layout shift can leave the user scanning for which pane to look at.
  recentlyRevealedPaneId: string | null;
  loading: boolean;
  bootstrapError: string | null;
  /** The current bootstrapError is the computer refusing this browser. */
  bootstrapNeedsApproval: boolean;

  /** Bring a pane to the user's eye wherever it currently lives.
   *  - In the current layout → set focus + scroll into view.
   *  - In a named workspace → load that workspace, then focus.
   *  - In Background (orphan, not in any named workspace) → adopt into
   *    the current layout, then focus.
   *  - Dead/missing → toast a soft warning. */
  revealPane: (paneId: string) => Promise<void>;

  // Custom user-sorted order of workspace names. Names not in this list
  // fall back to alphabetical order after the sorted ones.
  wsOrder: string[];
  setWsOrder: (order: string[]) => void;

  // Per-workspace "last viewed" timestamps, used to show an unread badge
  // in the sidebar when any pane in a workspace has activity newer than
  // the user's last visit.
  wsLastViewedMs: Record<string, number>;
  markWorkspaceViewed: (name: string) => void;

  // Name of the currently selected terminal colour theme (see themes.ts).
  // Persisted to localStorage; applied to all xterm instances live.
  terminalThemeName: string;
  terminalThemeExplicit: boolean;
  setTerminalThemeName: (name: string) => void;
  setTerminalThemeAuto: () => void;

  // App chrome colour theme. Persisted per browser.
  appTheme: AppThemePref;
  setAppTheme: (t: AppThemePref) => void;
  /** EFFECTIVE lightness — appTheme resolved against the OS, written only by
   *  App.tsx's appearance effect. */
  appThemeLight: boolean;
  setAppThemeLight: (v: boolean) => void;

  // Pane font-size (px) and line-height (unitless ratio) — drive BOTH
  // renderers. Terminal: Pane.tsx applies them to xterm options + calls fit()
  // so the PTY resizes cleanly. Focus (native): NativePane derives a proportional
  // scale (font = value/14, line-height = value/1.2) and sets --native-font-scale /
  // --native-line-height-scale on the pane root, so the prose ramp scales and the
  // Focus reading size lands ~visually-aligned with the terminal (see NativePane
  // for the 14-anchor rationale). (Name kept `terminal*` for back-compat with the
  // persisted localStorage keys.) Clamped on the setter, not the reader, so a bad
  // value self-heals.
  terminalFontSize: number;
  setTerminalFontSize: (px: number) => void;
  // Zoom factor for the chrome (sidebar + topbar). 1.0 = native size;
  // doesn't affect pane terminals (those have their own font-size knob).
  uiScale: number;
  setUiScale: (n: number) => void;
  terminalLineHeight: number;
  setTerminalLineHeight: (lh: number) => void;

  // Focus-mode prose size as a px OFFSET over the Terminal font size. The Focus
  // (native) renderer is a proportional font, so it reads smaller than the mono
  // terminal at equal px; this knob tunes the compensating gap. 1 = default
  // (≈ the old size/14 anchor, ~+1px); 0 = Focus prose renders at the same px as
  // the Terminal. NativePane turns this into --native-focus-scale on the pane
  // root, so the whole Focus type ramp lands at term+offset. Terminal is untouched.
  focusFontOffset: number;
  setFocusFontOffset: (px: number) => void;

  // Pane layout mode. 'auto' = panes are equal-sized, computed from the
  // tree shape; resize handles are disabled. 'free' = legacy behaviour,
  // user drags handles to set ratios. Default 'auto' to sidestep the
  // codex/ratatui resize corruption family of bugs for users who don't
  // need fine-grained resizing.
  paneLayoutMode: 'auto' | 'free';
  setPaneLayoutMode: (m: 'auto' | 'free') => void;
  // Native bottom-bar "vitals" visibility — which read-only session indicators show
  // in the NativeMetaBar's right cluster (model / context% / tokens / cost / cache /
  // resets / cwd). GLOBAL + persisted (one JSON blob under solu.nativeMetaBar);
  // purely presentational (gates already-available client data), so no daemon
  // round-trip.
  nativeMetaBarFields: NativeMetaBarFields;
  setNativeMetaBarField: (field: keyof NativeMetaBarFields, on: boolean) => void;
  // Mouse copy/paste parity (X11 primary-selection convention). Both
  // default on — every mainstream terminal ships them by default.
  copyOnSelect: boolean;
  setCopyOnSelect: (v: boolean) => void;
  middleClickPaste: boolean;
  setMiddleClickPaste: (v: boolean) => void;
  // The prompt-history strip at the top of Claude panes (both renderers).
  // Presentational client toggle, persisted per-browser. Default on.
  showPromptHistory: boolean;
  setShowPromptHistory: (v: boolean) => void;
  // Restructure the current layout tree into a known preset shape
  // (e.g. cols, rows, 2x2). Preserves the current leaf order — no
  // pane is created or destroyed.
  applyLayoutPreset: (preset: LayoutPresetId) => void;
  // Reflow into an explicit rows×cols grid. If rows*cols equals the
  // current leaf count this is a pure rearrangement; if greater,
  // spawns shell panes to fill and appends them. Smaller is rejected
  // (the picker UI greys those cells out).
  applyGridLayout: (rows: number, cols: number) => Promise<void>;

  // User-rebound shortcuts. Maps action id (see keybindings.ts) to a
  // combo string ("ctrl+space", "alt+b", …). Absent entries mean "use
  // the default"; a rebound action's default combo no longer fires.
  // Persisted as one JSON blob so we avoid a per-action localStorage
  // write on each set.
  customKeybindings: Record<string, string>;
  // When on, Ctrl+<letter> app shortcuts yield to the terminal while it has
  // focus (readline / TUI keys win). Persisted per browser; default off.
  terminalKeysFirst: boolean;
  setTerminalKeysFirst: (on: boolean) => void;
  setKeybinding: (actionId: string, combo: string) => void;
  clearKeybinding: (actionId: string) => void;
  resetAllKeybindings: () => void;

  // Client-side activity log — a rolling window of recent uiBus events
  // (pane state transitions, MCP tool calls, schedule fires, etc.) for
  // the Activity dialog. Appended to as the SSE stream delivers events;
  // initially bootstrapped from GET /api/activity when the dialog opens
  // so history that pre-dates this browser session is still visible.
  // Capped at 500 — richer than the daemon's 200-event ring because a
  // long-running browser outlives daemon restarts.
  activityLog: ActivityEvent[];
  pushActivity: (ev: ActivityEvent) => void;
  bootstrapActivity: (events: ActivityEvent[]) => void;
  clearActivity: () => void;

  // Latest tail per pane — single source of truth for attention
  // rendering. Updated by SSE aria-observation events + the panel
  // mount bootstrap. Keyed by paneId so the store always reflects the
  // current question, never a stack of stale ones. The bell + panel
  // cards both derive from `useAttentionItems()` which joins this map
  // with live `panes` state and applies dismissedFingerprints.
  paneTails: Record<string, AriaTail>;
  /** Keyed by `${paneId}::${fingerprint}`, value is dismissal timestamp.
   *  An attention item whose tail's fingerprint matches a live entry
   *  in this map is suppressed for ARIA_DISMISSAL_TTL_MS. Memory-only
   *  — a reload wipes dismissals, which is fine (old questions are
   *  stale by then anyway). */
  ariaDismissedFingerprints: Record<string, number>;
  upsertPaneTail: (tail: AriaTail) => void;
  /** Add the pane's current tail-fingerprint to dismissals so the
   *  attention item disappears from both bell and panel until either
   *  the question changes (new fingerprint) or the TTL expires. The
   *  tail itself stays in the store — that's what lets the bell keep
   *  filtering correctly even after the panel card is gone. */
  dismissPaneAttention: (paneId: string) => void;
  /** Bulk version: dismiss every pane currently surfacing an attention
   *  card. Powers the panel's "clear all" button. */
  clearAllPaneAttention: () => void;

  // Autoresponder state — populated from /api/aria/status + SSE
  // aria-auto-* events. Pending actions render on pane badges
  // with a countdown; the user can cancel before it fires.
  ariaPaused: boolean;
  ariaAutoFiresHr: number;
  ariaAutoPending: AriaAutoPending[];
  // Rendered as the "monitoring" strip in the ARIA panel.
  setAriaPaused: (paused: boolean) => void;
  setAriaStatusSnapshot: (snapshot: { paused: boolean; auto_fires_hr: number }) => void;
  upsertAriaAutoPending: (p: AriaAutoPending) => void;
  resolveAriaAutoPending: (confirmId: string, outcome: { approved: boolean; error?: string; timeout?: boolean; cancelled?: boolean }) => void;
  removeAriaAutoPending: (confirmId: string) => void;
  // Optimistically clears a pane's `choice-prompt` state right after an
  // ARIA auto-approve resolves, so the quick-reply bar (gated on
  // `state === 'choice-prompt'`) hides immediately instead of lingering
  // until the next 3s refreshPanes poll. Purely optimistic — the poll
  // reconciles to ground truth (samePaneInfo keys on `state`).
  clearChoicePromptOptimistic: (paneId: string) => void;
  // Applies a real-time pane runtime-state from the daemon's "pane-state"
  // SSE event (daemon/activity.go), so the quick-reply bar and other
  // state-gated UI flip immediately instead of waiting up to 3s for the
  // next refreshPanes poll. The daemon's ToState string uses the exact
  // same encoding as PaneRuntimeState (busy/awaiting-input/choice-prompt/
  // recent/idle/exited — see daemon/state.go), so it's applied directly.
  // No-op (returns {}) if the pane is unknown or already in that state.
  // The 3s poll remains source-of-truth and self-heals missed SSE.
  applyPaneStateFromSSE: (
    paneId: string,
    state: import('./types').PaneRuntimeState,
    agentMidTurn?: boolean,
  ) => void;
  // Applies a real-time attention change from the daemon's "pane-attention"
  // SSE event (daemon/attention.go), so notification gating / the attention
  // badge flip immediately instead of waiting for the next refreshPanes
  // poll. The poll remains source-of-truth (samePaneInfo keys on attention)
  // and self-heals missed SSE.
  applyPaneAttentionFromSSE: (
    paneId: string,
    attention: NonNullable<import('./types').PaneInfo['attention']>,
    reason?: string,
  ) => void;

  // Zoom state — when set, PaneGrid renders ONLY this pane at full size
  // while the underlying layout tree is unchanged. Ephemeral (not
  // persisted); cleared when the zoomed pane is killed or when a
  // workspace is loaded.
  zoomedPaneId: string | null;
  toggleZoom: (paneId?: string) => void;
  // True while a pane-drag is in flight (any leaf type). BrowserPanes
  // watch this and hide their native child webview so the drop-zone
  // overlay is visible — without this, the OS-level native webview
  // paints on top of the React DOM and obscures the overlay.
  isDraggingPane: boolean;
  setDraggingPane: (v: boolean) => void;
  // True clone: new pane with same cwd + same cmd as the focused pane.
  // Splits in the given direction (default horizontal/right).
  cloneFocused: (direction?: 'horizontal' | 'vertical') => Promise<void>;
  // Client-side tracking of when the user last focused/viewed each pane.
  // Used to compute the "unread" indicator.
  lastViewedMs: Record<string, number>;
  // Client-side tracking of when each pane's WebSocket last received bytes.
  // Fresher than the server-side lastActivityMs between poll cycles.
  clientActivityMs: Record<string, number>;
  markActivity: (paneId: string) => void;

  // Recent submitted user inputs per pane (newest first, capped
  // daemon-side). Recorded by the DAEMON at the PTY write choke point
  // (daemon/prompt_recorder.go) so every transport — typing, voice
  // inject, ARIA, schedules, other devices — is captured, and every
  // client renders the same history. Fed level-based from the pane
  // DTO in refreshPanes and live via the "pane-prompt" SSE event.
  // Keyed by terminalSessionKey(scope, paneId), matching retained pane cores.
  // Array refs are preserved when content is unchanged — selectors key on them.
  lastPrompts: Record<string, string[]>;
  setPaneLastPrompts: (paneId: string, prompts: string[], scope?: RoutingScope) => void;

  // Registry of pane input senders (set by <Pane> when WS connects).
  // Used by the mobile key bar to dispatch keystrokes to the focused pane.
  senders: Record<string, Sender>;
  registerSender: (id: string, fn: Sender) => void;
  unregisterSender: (id: string, expected?: unknown) => void;
  sendToFocused: (bytes: Uint8Array | string) => boolean;

  // Per-pane snapshot getters (set by <Pane> when xterm mounts).
  // Returns a plain-text dump of the last ~50 lines of the pane's
  // visible buffer — used by voice-to-text to give the cleanup LLM
  // context about what the user is currently looking at. With
  // { screen: true } it returns only the live screen rows (no
  // scrollback), which menu-key confirmation needs (lib/menuKeys.ts).
  paneSnapshots: Record<string, PaneSnapshotFn>;
  registerPaneSnapshot: (id: string, fn: PaneSnapshotFn) => void;
  unregisterPaneSnapshot: (id: string, expected?: unknown) => void;
  getFocusedPaneSnapshot: () => string | null;

  // Per-pane claim callbacks. One "Take over" tap invokes every
  // registered callback so ALL panes in this browser switch
  // ownership together — otherwise the user would have to claim
  // each pane one by one.
  claimers: Record<string, () => void>;
  registerClaimer: (id: string, fn: () => void) => void;
  unregisterClaimer: (id: string, expected?: unknown) => void;
  claimAllPanes: () => void;

  // Per-pane redraw trigger. User-invoked from the pane header
  // button or via keybinding when a Ratatui-based TUI (codex) has
  // left ghost / duplicate cells after a stress-resize or workspace
  // switch-back. Wired via the pane's WebSocket as {type:"redraw"}
  // — daemon responds by injecting a screen-clear + size-bump so
  // the TUI emits a fresh full frame. Programmatic equivalent of
  // the "drag the splitter up and down" workaround.
  redrawers: Record<string, () => void>;
  registerRedrawer: (id: string, fn: () => void) => void;
  unregisterRedrawer: (id: string, expected?: unknown) => void;
  redrawPane: (id: string) => void;

  // Per-pane REALIGN trigger — the fix for panes that render as gibberish after
  // an app update or a cross-device takeover (the shared PTY's width no longer
  // matches this device's xterm grid). Unlike redraw (repaint at the same
  // width), the realigner re-stamps active sizing and force-resends the measured
  // width, so the daemon resizes the PTY back to this device's width and the TUI
  // repaints aligned. Programmatic equivalent of the open/close-in-app-browser
  // workaround. Wired per-pane from Pane.tsx; realignAllPanes hits every visible
  // one at once (the "Realign all" control / keybinding).
  realigners: Record<string, () => void>;
  registerRealigner: (id: string, fn: () => void) => void;
  unregisterRealigner: (id: string, expected?: unknown) => void;
  realignPane: (id: string) => void;
  realignAllPanes: () => void;

  // Hard "refresh the app" (Ctrl+Shift+R). Deterministic JS reload that
  // doesn't depend on the WebView2 accelerator (which behaves inconsistently
  // when a Focus pane's composer textarea has focus) and forces a fresh shell
  // fetch via a versioned URL (a plain reload can re-serve the cached
  // index.html). See reloadForFreshBundle.
  reloadApp: (targetVersion?: string) => void;

  // Per-pane ownership state — true = this browser owns, false =
  // another device owns (show overlay + block input), null/missing
  // = unknown (haven't received the server's initial ownership
  // frame yet; don't flash overlay). Centralized so a single
  // session-level overlay can render from PaneGrid instead of
  // painting one per pane.
  paneOwnership: Record<string, boolean>;
  // Ownership provenance from the daemon's owner field: 'other' = a peer
  // device really holds input; 'none' = nobody does (daemon restarted or
  // the owner disconnected). Missing = unknown (old daemon, or no frame
  // yet). Drives truthful lock-overlay copy: "in use somewhere else" must
  // not be shown for a pane nobody owns.
  paneOwnerState: Record<string, 'you' | 'none' | 'other'>;
  paneOwnershipTokens: Record<string, unknown>;
  setPaneOwned: (id: string, owned: boolean, ownerState?: 'you' | 'none' | 'other', token?: unknown) => void;
  clearPaneOwnership: (id: string, expected?: unknown) => void;

  // "Claim intent" — flipped to true when the user explicitly taps
  // "Take over"; reset to false when any pane reports owned=false
  // (another device took over). Drives mobile auto-claim-on-WS-open:
  // we auto-claim fresh panes ONLY when the user has indicated
  // this browser should own the session. Without this, mobile
  // silently stole ownership on every tab switch after the initial
  // overlay — the UX bug the user rightly flagged.
  claimIntent: boolean;
  setClaimIntent: (v: boolean) => void;

  // Is the daemon reachable from this browser? Flipped by the 3s
  // refresh tick (store.ts bottom): true if any poll succeeded, false
  // if all failed. Drives the green/red dot in the topbar.
  daemonOnline: boolean;

  // Notifications
  notifyEnabled: boolean;
  setNotifyEnabled: (v: boolean) => Promise<boolean>;
  /** Banner setup modal visibility. Opened on demand via the 📤
   *  button next to the bell (never auto-popped as of v0.0.118).
   *  Dismissed by the modal calling setBannerSetupOpen(false). */
  bannerSetupOpen: boolean;
  setBannerSetupOpen: (v: boolean) => void;
  /** Feedback modal visibility. Opened from the status-bar Feedback chip or
   *  the command palette "Send feedback" action. In the store (not App
   *  useState) so any chrome element can trigger it without prop-drilling. */
  feedbackOpen: boolean;
  // Which view the modal opens on. 'mine' = the "My reports" list — the
  // inbox's feedback rows land there so the reply box is one tap away.
  feedbackView: 'new' | 'mine';
  setFeedbackOpen: (v: boolean, view?: 'new' | 'mine') => void;
  /** Local diagnostic preview/download dialog. Kept in the store so Help,
   *  the command palette, and legacy/new Settings surfaces share one action. */
  supportBundleOpen: boolean;
  setSupportBundleOpen: (v: boolean) => void;
  /** Agents-cockpit popover visibility (anchored to the StatusBar ◇ chip).
   *  In the store so the command palette can open it too. All agent/fleet
   *  DATA stays outside zustand, in the useAgentsStatus singleton. */
  agentsPanelOpen: boolean;
  setAgentsPanelOpen: (v: boolean) => void;
  systemPageOpen: boolean;
  setSystemPageOpen: (v: boolean) => void;
  phoneAccessOpen: boolean;
  setPhoneAccessOpen: (v: boolean) => void;
  /** The version/update popover is opened from two places — the bottom-right
   *  StatusBar chip and the top-bar update pill. `versionPopoverSource` records
   *  which trigger opened it so the popover renders anchored to THAT element
   *  (dropping down from the top pill, up from the bottom chip) instead of
   *  always jumping to the bottom-right corner. */
  versionPopoverOpen: boolean;
  versionPopoverSource: 'statusbar' | 'topbar';
  setVersionPopoverOpen: (v: boolean) => void;
  openVersionPopoverFrom: (source: 'statusbar' | 'topbar') => void;

  // Idempotent liveness re-POST of the push subscription. Call on app
  // bootstrap, machine selection, and service-worker upgrades. The
  // cloud's subscription store is in-memory (see cloud/push.go), so
  // without this the first cloud restart or PWA re-open silently
  // kills push delivery while the 🔔 icon keeps lying about it. No-ops
  // unless we're in cloud mode with a session, vapid key, and the
  // user has opted in.
  resubscribePush: () => Promise<boolean>;

  bootstrap: () => Promise<void>;

  /** Currently-active backend in router mode; null when single-backend. */
  activeBackend: 'wsl' | 'win' | null;
  /**
   * Swap active backend in-app without a full page reload. Clears all
   * per-backend state (panes, ports, workspaces, layout), disposes
   * xterm/WebGL by unmounting Pane components via flushSync, flips
   * api.ts's backendPrefix, updates the URL via history.replaceState,
   * and re-bootstraps against the new backend. Replaces the old
   * window.location.href reload which leaked WebGL contexts in
   * WebView2 across navigation.
   */
  switchBackend: (b: 'wsl' | 'win', workspaceName?: string, opts?: {preserveOwnerLink?:boolean}) => Promise<void>;
  refreshPanes: () => Promise<void>;
  refreshPorts: () => Promise<void>;
  refreshWorkspaces: (opts?: { full?: boolean }) => Promise<void>;
  // Write-through cache: after persisting a workspace, fold the saved
  // layout/paneIds back into the matching `workspaces` entry so a later
  // loadWorkspace restores what we just wrote — not a stale snapshot.
  mergeSavedWorkspaceIntoCache: (saved: Workspace) => void;
  refreshSchedules: () => Promise<void>;
  /** Refetch ONE schedule (a run started or landed) and patch it in place. */
  refreshSchedule: (id: string) => Promise<void>;

  // Self-update — daemon polls GitHub Releases every 15 min and
  // publishes "update-available" via the SSE bus when a new version
  // appears. updateStatus mirrors GET /api/update/status; null means
  // we haven't fetched yet (no badge rendered). applyUpdate kicks off
  // the download → swap → restart dance; the daemon exits ~400ms
  // after responding so the user's reconnect logic handles the gap.
  updateStatus: UpdateStatus | null;
  // True only while POST /api/update/apply is awaiting admission. This powers
  // compact spinners, never the full-screen swap overlay.
  updateApplyRequestInFlight: boolean;
  refreshUpdateStatus: () => Promise<void>;
  // SSE is only a prompt to re-check the authoritative level endpoint. A
  // replayed/lost frame can neither mount nor strand the update overlay.
  confirmUpdateApplyingEvent: (targetVersion?: string) => Promise<void>;
  abortUpdateRecovery: () => void;
  // Forces a fresh manifest check on the daemon (POST /api/update/check),
  // bypassing the cached 30s poll — used by the popover's "Check now".
  checkForUpdate: () => Promise<void>;
  // applyUpdate swaps the daemon (download → swap → exit). Typed holds distinguish
  // an active Focus/native turn from the daemon's workflow-invocation drain.
  // force:true skips both only after a deliberate user override.
  // auto:true is the unattended "install when idle" path (maybeAutoApplyUpdate).
  // Unlike force, it never bypasses either gate.
  applyUpdate: (opts?: { skipReload?: boolean; force?: boolean; auto?: boolean }) => Promise<void>;
  rollbackDecline: RollbackDecline | null;
  rollbackUpdate: (opts: { targetVersion: string; holdHours?: number; force?: boolean; skipReload?: boolean }) => Promise<void>;
  releaseUpdateHold: () => Promise<void>;
  // Set while the daemon is holding an update for active work. Drives the stable
  // queued affordance and the single non-forced retry controller.
  deferredUpdate: UpdateHold | null;
  // "Install when idle" trigger: if updateSettings.autoApply === 'when-idle' and
  // an update is available, kick off applyUpdate({auto:true}) — once per version,
  // with a failure backoff. Called from refreshUpdateStatus (bootstrap + poll + SSE).
  maybeAutoApplyUpdate: () => void;
  // Desktop-shell update countdown. It is armed only after the complete idle
  // gate passes and is always visible while an automatic relaunch is imminent.
  shellAutoRestart: ShellAutoRestart | null;
  maybeAutoApplyShellUpdate: () => void;
  applyShellUpdateNow: (opts?: { userInitiated?: boolean }) => Promise<void>;
  postponeShellAutoRestart: () => void;
  restartShellNowForUpdate: () => Promise<void>;
  // Tauri-shell installer update — downloads the .exe and launches it.
  // Daemon stays alive; NSIS handles closing the running shell. The
  // badge stays interactive (not frozen): the user might cancel the
  // UAC prompt, in which case nothing changes and they can retry.
  applyShellUpdate: (opts?: { forceDowngrade?: boolean }) => Promise<void>;
  // Monotonic combined-update flow. A component may supply the native shell
  // launcher for cloud/WSL topology; ordering and downgrade policy stay here.
  applyAllUpdates: (opts?: { applyShell?: () => Promise<void>; shellActionable?: boolean }) => Promise<ApplyAllUpdatesResult | undefined>;
  // Finishes the daemon half of a combined update once the installer has
  // relaunched the app (see UPDATE_CONTINUE_KEY). Runs on every status refresh.
  continueUpdateAfterInstaller: () => Promise<void>;

  // MCP servers configured for a given pane cwd. Cached per-cwd so
  // switching focus between panes that share a project doesn't refetch.
  // Read-only mirror of ~/.claude.json + .mcp.json walks; the sidebar
  // calls refreshMcpServers when the focused pane's cwd changes.
  mcpServersByCwd: Record<string, McpServer[]>;
  refreshMcpServers: (cwd: string) => Promise<void>;
  createSchedule: (input: ScheduleInput) => Promise<Schedule>;
  updateSchedule: (id: string, input: ScheduleInput) => Promise<Schedule>;
  deleteSchedule: (id: string) => Promise<void>;
  setScheduleEnabled: (id: string, enabled: boolean) => Promise<void>;
  fireSchedule: (id: string) => Promise<void>;
  snoozeSchedule: (id: string) => Promise<void>;

  createPane: (opts: { cwd: string; cmd: string[]; title?: string; kind?: 'native'; agent?: 'claude' | 'codex' | 'muse'; claudeSessionId?: string; codexThreadId?: string; provider?: 'anthropic' | 'glm'; effort?: PaneEffort; model?: string; role?: PaneRole; contextPack?: string }) => Promise<PaneInfo>;
  renamePane: (id: string, title: string) => Promise<PaneInfo | null>;
  /** Set or clear the pane's @handle and working-on line; throws the
   *  daemon's message (e.g. a taken handle) for the caller to show. */
  setPaneIdentity: (id: string, change: { handle?: string; workingOn?: string }) => Promise<void>;
  // Flip a live pane between the xterm terminal renderer (kind '') and the
  // native conversation renderer (kind 'native') in place. Optimistically
  // patches pane.kind so the render dispatch swaps the body immediately, then
  // reconciles with the daemon DTO; rolls back on failure.
  //
  // `opts` selects the WIRE CONTRACT mode (back-compat: no opts ⇒ immediate):
  //   • when:'now' (or omitted) — interrupt-and-switch now; optimistic kind flip
  //     + paneModeSwitching like before.
  //   • when:'deferred' — arm a LOSSLESS switch: NO optimistic flip, NO
  //     paneModeSwitching; merges the returned DTO (which carries pendingSwitch).
  //   • cancel:true — disarm a previously-armed deferred switch: optimistically
  //     null pendingSwitch, POST, reconcile.
  switchPaneMode: (
    id: string,
    kind: '' | 'native',
    opts?: { when?: 'now' | 'deferred'; cancel?: boolean },
  ) => Promise<PaneInfo | null>;
  // Per-pane "renderer switch in flight" flag. switchPaneMode sets this true
  // for the duration of the daemon round-trip (the kill+respawn grace) and
  // clears it in finally. The pane-header toggle reads it to render a
  // disabled/spinner state so the flip never looks like a silent stall.
  // Memory-only; an entry is `true` only while a switch is pending.
  paneModeSwitching: Record<string, boolean>;
  setPaneFlag: (id: string, flagged: boolean, memo: string) => Promise<PaneInfo | null>;
  // Set a pane's ARIA auto-approval mode with an optimistic local patch so the
  // header badge / palette flip instantly instead of waiting on the ~3s pane
  // poll. Reconciles with the daemon's returned pane; rolls back + toasts on
  // failure. No-ops (returns the current pane) when already in `mode`.
  setPaneAutoMode: (id: string, mode: PaneAutoMode) => Promise<PaneInfo | null>;
  // Turn the "[solu] delegated work" status line on or off for a pane, or pass
  // null to clear the override back to the pane's role default. Optimistic like
  // setPaneAutoMode so the menu checkbox flips on click, not on the next poll.
  setPaneAnnounceDelegation: (id: string, announce: boolean | null) => Promise<PaneInfo | null>;
  // Returns true if the daemon DELETE succeeded, false otherwise. On
  // failure the pane is restored to local state so the user can retry.
  // Pass { silent: true } to suppress the per-pane error toast — the
  // batch caller (killAllBackground) shows a single summary instead.
  killPane: (id: string, opts?: { silent?: boolean }) => Promise<boolean>;
  // Recently closed panes (newest first, 24h, persisted per browser) and the
  // action that brings one back — same cwd/command, agent session resumed.
  recentlyClosedPanes: ClosedPane[];
  refreshClosedPanes: () => Promise<void>;
  reopenClosedPane: (id?: string) => Promise<void>;
  // Resume a suspended (exited-but-resumable) host pane in place. Optimistically
  // flips the pane to running + clears `resumable` so the Session-ended card /
  // terminal overlay swaps to a connecting state and the Resume button disables
  // instantly; POSTs; merges the returned running DTO (whose NEW pid drives the
  // terminal reconnect backstop + the native pid cache-reset). Rolls back +
  // toasts on failure so the card returns for a retry.
  resumePane: (id: string, cols?: number, rows?: number, cwd?: string) => Promise<PaneInfo | null>;
  // Restart a RUNNING native Claude or Codex pane's agent in place to reclaim
  // its heap while keeping the conversation. Mirrors resumePane: POSTs /refresh
  // and merges the new-pid DTO to drive native reconnect/replay. Toasts on failure.
  refreshPane: (id: string) => Promise<PaneInfo | null>;
  // Restart THIS pane's agent in place, any pane kind, on the installed binary
  // (POST /api/panes/{id}/restart): same pane id, resumed conversation; fresh=true
  // starts a brand-new conversation. Merges the new-pid DTO so terminal/native
  // reconnect. A pane with no durable conversation returns ApiError 409 with
  // body.needsFresh — the caller confirms and retries with fresh=true; on any
  // other failure the caller toasts. force=true is the confirmed user override
  // of the busy settle gate. Use restartPaneFlow for the standard UX.
  restartPane: (id: string, fresh?: boolean, force?: boolean) => Promise<PaneInfo | null>;
  // Change a native pane's reasoning-effort level. Optimistic badge flip +
  // new-pid reconcile (the daemon respawns `claude --resume` with the new
  // --effort). No-op when the level is unchanged. Toasts on success/failure.
  setPaneEffort: (id: string, effort: PaneEffort) => Promise<PaneInfo | null>;
  // Attach/detach a local context pack. The daemon respawns the agent in place
  // and returns the new-pid pane DTO.
  setPaneContextPack: (id: string, contextPack: string) => Promise<PaneInfo | null>;
  // Set/remove the orchestrator role. The daemon respawns in place because the
  // role's system brief is injected by the host adapter; the returned DTO is merged
  // only after that respawn succeeds.
  setPaneRole: (id: string, role: PaneRole | '') => Promise<PaneInfo | null>;
  // Change a native pane's Claude model (respawn with --model; also rewrites
  // the session's stored model so future bare resumes keep it). Optimistic
  // field flip + new-pid reconcile, mirroring setPaneEffort.
  setPaneModel: (id: string, model: string) => Promise<PaneInfo | null>;

  // Internal browser tiles (iframes rendering localhost services).
  // Managed client-side — daemon knows nothing about them. URL + title
  // persist via workspace autosave same way terminal panes do.
  browsers: Record<string, BrowserInfo>;
  openBrowserHere: (url: string, anchorPaneId?: string | null, direction?: 'horizontal' | 'vertical', service?: BrowserServiceTarget) => void;
  setBrowserUrl: (id: string, url: string) => void;
  setBrowserTitle: (id: string, title: string) => void;
  closeBrowserInLayout: (id: string) => void;
  // Single-instance toggle for the internal browser (Alt+B / topbar button):
  // open one if none exists, else close the one that's open. The browser is a
  // peek-at-localhost affordance, not a primary surface, so we cap it at one.
  toggleBrowser: (url?: string) => void;
  // Close the single browser tile if open; quiet no-op otherwise. Serves the
  // daemon's close-browser UI event (the agent's close_browser MCP tool) and
  // the close half of toggleBrowser.
  closeBrowser: () => void;

  // File tiles — inline read-with-line-edit viewer. Like browsers,
  // managed client-side; path persists via workspace autosave.
  files: Record<string, FileTileInfo>;
  recentFilePaths: string[]; // localStorage-backed recent-open list
  openFileHere: (path: string, anchorPaneId?: string | null, direction?: 'horizontal' | 'vertical') => void;
  setFilePath: (id: string, path: string) => void;
  closeFileInLayout: (id: string) => void;

  // Diff tiles — read-only "review what changed" view of a git repo. Self-
  // contained: the repo path + staged flag live in the layout leaf (see the
  // Layout type), so there is no store map. Opening pushes a leaf and the
  // tile re-fetches the diff on mount; it persists across reloads for free.
  openDiffHere: (path: string, staged?: boolean, anchorPaneId?: string | null, direction?: 'horizontal' | 'vertical') => void;
  setDiffStaged: (diffId: string, staged: boolean) => void;
  // Open (or retarget) a diff tile on one agent turn of a pane. n omitted =
  // the pane's latest completed turn. Reuses a diff tile already showing a
  // turn of the same pane instead of stacking tiles.
  openTurnDiff: (paneId: string, n?: number) => void;
  // Point a diff tile at a turn, or back at the live working tree (null).
  setDiffTurn: (diffId: string, turn: { paneId: string; n: number } | null) => void;
  closeDiffInLayout: (id: string) => void;

  // Directory navigation memory for the Files panel. Decoupled from
  // recentFilePaths so the user can jump to "where was that folder?"
  // without having opened a file inside it yet.
  //
  // filesAnchor pins the Files section to a specific directory,
  // overriding the usual "follow the focused pane's cwd" behavior.
  // Set via QuickOpen's directory-hit or the sidebar's recent-dirs list;
  // null means "follow focused pane".
  recentDirs: string[];
  filesAnchor: string | null;
  pushRecentDir: (path: string) => void;
  setFilesAnchor: (path: string | null) => void;

  setLayout: (l: Layout | null) => void;
  setFocus: (id: string | null) => void;
  // One-shot consume of lastUserFocusId. The pane focus effects call this right
  // after they grab the caret for a deliberate selection, so a later re-render or
  // remount (e.g. a WebSocket reconnect storm) can't re-assert focus and yank the
  // caret back out of wherever the user has since moved it.
  consumeUserFocus: () => void;
  splitFocused: (direction: 'horizontal' | 'vertical', opts: { cwd: string; cmd: string[]; title?: string; kind?: 'native'; agent?: 'claude' | 'codex' | 'muse'; claudeSessionId?: string; codexThreadId?: string; provider?: 'anthropic' | 'glm'; effort?: PaneEffort; model?: string; role?: PaneRole; contextPack?: string }) => Promise<void>;
  // One-click "launch claude here": live-cwd of the given pane + cmd=['claude'], split right.
  launchClaudeHere: (paneId: string, direction?: 'horizontal' | 'vertical') => Promise<void>;
  // One-click "launch shell here": same but spawns $SHELL (daemon default).
  launchShellHere: (paneId: string, direction?: 'horizontal' | 'vertical') => Promise<void>;
  closePaneInLayout: (paneId: string) => void;
  movePaneInLayout: (source: string, target: string, zone: DropZone) => void;
  movePaneToWorkspace: (paneId: string, destination: string, scope?: RoutingScope) => Promise<void>;

  saveWorkspace: (name: string) => Promise<void>;
  loadWorkspace: (name: string, takeOver?: boolean) => Promise<void>;
  deleteWorkspace: (name: string) => Promise<void>;
  renameWorkspace: (oldName: string, newName: string) => Promise<void>;
  setWorkspaceContextPack: (name: string, contextPack: string) => Promise<void>;

  // Clear the current layout without killing any panes. They remain alive
  // in the daemon and show up in the "Background sessions" sidebar list.
  clearLayout: () => void;
  // Add an existing alive pane into the current layout (splits focused
  // pane right if one exists, else becomes the root).
  addPaneToLayout: (paneId: string, direction?: 'horizontal' | 'vertical') => void;
}


// Shared core of launchClaudeHere / launchShellHere: fetch the live cwd of
// the anchor pane, spawn a new pane with the given cmd, and splice it into
// the layout next to the anchor.
async function launchCmdHere(
  get: () => State,
  set: (partial: Partial<State> | ((s: State) => Partial<State>)) => void,
  paneId: string,
  direction: 'horizontal' | 'vertical',
  opts: { cmd: string[]; title: string },
) {
  const origin = autosaveSnapshotFromState(get());
  let cwd = get().panes[paneId]?.cwd ?? '';
  try {
    const live = await api.paneLiveCwd(paneId);
    if (live) cwd = live;
  } catch { /* fall back to stored cwd */ }

  const p = await api.createPane({
    cwd,
    cmd: opts.cmd,
    title: opts.title,
    ...paneWorkspaceClaimForName(origin?.name, direction, paneId),
  });
  if (origin && get().activeWorkspace !== origin.name) {
    await persistCreatedPaneToOriginWorkspace(get, origin, p, direction, paneId);
    return;
  }
  set((s) => ({
    panes: { ...s.panes, [p.id]: p },
    layout: insertPaneOrAppend(s.layout, p.id, direction, paneId),
    focusedPaneId: p.id,
    lastUserFocusId: p.id, // deliberate spawn — grab the caret for the new pane
    // Clear zoom so the new pane is visible, not focused-but-hidden behind
    // the zoomed pane (input to an invisible tile).
    zoomedPaneId: null,
    // Stamp lastViewedMs so the new pane doesn't flash an "unread" marker
    // when its first bytes arrive.
    lastViewedMs: { ...s.lastViewedMs, [p.id]: Date.now() },
  }));
  scheduleAutosave(get);
  await flushAutosave(get, undefined, { throwOnError: true });
}

// --- debounced autosave ---
//
// Autosave writes to whichever workspace is currently "active" (either a
// named workspace the user loaded, or the unnamed `_current` fallback).
// This means edits you make while viewing "foo" persist INTO "foo" — not
// into some unrelated _current buffer as the previous version did.
//
// On workspace switch we flush the pending save first (see flushAutosave)
// so the outgoing workspace doesn't lose late edits.
let saveTimer: number | null = null;
const workspaceSaves = new Set<Promise<unknown>>();
function trackWorkspaceSave<T>(save: Promise<T>): Promise<T> {
  workspaceSaves.add(save);
  void save.then(() => workspaceSaves.delete(save), () => workspaceSaves.delete(save));
  return save;
}
let refreshSchedulesInFlight: Promise<void> | null = null;
const refreshScheduleInFlight = new Map<string, Promise<void>>();
// Rows invalidated while a fetch that may predate the change was in flight
// (e.g. a run ended while its "running…" row was being fetched). Each gets
// one trailing fetch once nothing is in flight for it.
const scheduleRowsDirty = new Set<string>();

function scheduleAutosave(get: () => State) {
  if (workspaceMovedTo(get().activeWorkspace)) return;
  if (saveTimer) window.clearTimeout(saveTimer);
  saveTimer = window.setTimeout(() => {
    doAutosave(get);
  }, 400);
}

// Drop any pending autosave without flushing it. Used by deleteWorkspace
// so a queued save doesn't immediately recreate the workspace we just
// asked the daemon to remove.
function cancelAutosave() {
  if (saveTimer === null) return;
  window.clearTimeout(saveTimer);
  saveTimer = null;
}

function reportWorkspaceMoveConflict(get: () => State, name: string, version: number | undefined, scope: RoutingScope, error: unknown) {
  if (!(error instanceof ApiError) || error.status !== 409 || scope.key !== currentRoutingScope().key) return;
  const state = get();
  if (state.activeWorkspace !== name || state.activeWorkspaceMoveVersion !== (version ?? 0)) return;
  const message = `“${name}” changed after a pane move in another view. Layout changes here are not being saved. Reload the latest layout to continue.`;
  if (state.toasts.some((toast) => toast.message === message)) return;
  state.toast(message, {
    kind: 'warn', ttlMs: 0,
    action: { label: 'Reload layout', run: () => {
      const generation = get().navigationGeneration;
      const valid = () => scope.key === currentRoutingScope().key && get().activeWorkspace === name
        && get().navigationGeneration === generation;
      void (async () => {
        if (!valid()) return;
        const confirmed = await confirmDialog({
          title: 'Reload workspace layout?',
          message: 'This replaces your unsaved layout edits with the latest saved layout. All sessions keep running.',
          confirmLabel: 'Reload layout',
        });
        if (!confirmed || !valid()) return;
        await get().refreshWorkspaces({ full: true });
        if (!valid()) return;
        const latest = pickWorkspaceByName(get().workspaces, name, get().activeBackend);
        if (!latest || (latest.paneMoveVersion ?? 0) <= get().activeWorkspaceMoveVersion) {
          get().toast('The latest layout could not be loaded. Please try again.', { kind: 'error' });
          return;
        }
        // The user chose the saved layout. Do not flush the obsolete one
        // again while switching to that same workspace's latest version.
        cancelAutosave();
        await get().loadWorkspace(name);
      })().catch(() => {
        if (valid()) get().toast('The latest layout could not be loaded. Please try again.', { kind: 'error' });
      });
    } },
  });
}

function doAutosave(get: () => State) {
  const s = get();
  const scope = currentRoutingScope();
  if (!s.activeWorkspace || s.mainPresentation.kind !== "workspace" || workspaceMovedTo(s.activeWorkspace)) return;
  // Refuse to persist a NAMED workspace with a null layout. This is
  // almost always a transient mid-switch state (loadWorkspace's blank
  // phase flips activeWorkspace to the destination and sets layout=null
  // for 200ms before the real layout lands). A timer firing in that
  // window would otherwise wipe the destination's paneIds — the exact
  // path that orphaned 6 panes of one workspace on 2026-04-27.
  // _current is the auto-save scratchpad, so empty there is legitimate.
  if (s.layout === null && s.activeWorkspace !== '_current') return;
  // Skip while a self-update is in flight: the daemon is about to die
  // for ~1-2s and any save would either hit a 502 (router can't reach
  // backend) or — worse — succeed against a half-restarted daemon
  // with stale in-memory state. The post-update bootstrap re-pulls
  // everything anyway.
  if (s.updateStatus?.applying) return;
  trackWorkspaceSave(api.saveWorkspace({
    name: s.activeWorkspace,
    paneMoveVersion: s.activeWorkspaceMoveVersion,
    layout: s.layout,
    paneIds: collectPaneIds(s.layout),
    panes: collectPaneDefs(s.layout, s.panes),
    browsers: collectBrowserInfos(s.layout, s.browsers),
    files: collectFileInfos(s.layout, s.files),
    activeAt: new Date().toISOString(),
  },scope))
    // Write through to the cached `workspaces` entry first, so a fast
    // workspace switch-and-back restores THIS layout (a summary poll can't
    // detect a same-panes re-arrangement and would keep the stale layout).
    // Then refresh the list so sidebar counters and the "claimed by named
    // workspace" filter (Background sessions) see the just-persisted changes.
    .then((saved) => {
      if(scope.key !== currentRoutingScope().key)return;
      get().mergeSavedWorkspaceIntoCache(saved);
      return get().refreshWorkspaces();
    })
    .catch((error) => reportWorkspaceMoveConflict(get, s.activeWorkspace, s.activeWorkspaceMoveVersion, scope, error));
}

// Snapshot of the state needed to persist one workspace. Callers that
// flush across a workspace switch MUST capture this BEFORE flipping
// activeWorkspace — otherwise flushAutosave would read the new
// workspace's name/layout and silently overwrite it with the outgoing
// edits (or, worse, drop them entirely).
type AutosaveSnapshot = {
  name: string;
  paneMoveVersion?: number;
  layout: Layout | null;
  panes: Record<string, PaneInfo>;
  browsers: Record<string, BrowserInfo>;
  files: Record<string, FileTileInfo>;
};

function autosaveSnapshotFromState(s: State): AutosaveSnapshot | undefined {
  if (!s.activeWorkspace) return undefined;
  return {
    name: s.activeWorkspace,
    paneMoveVersion: s.activeWorkspaceMoveVersion,
    layout: s.layout,
    panes: s.panes,
    browsers: s.browsers,
    files: s.files,
  };
}

function paneWorkspaceClaimForName(
  name: string | undefined,
  direction: 'horizontal' | 'vertical' | null,
  target: string | null,
): Pick<Parameters<typeof api.createPane>[0], 'workspace' | 'workspaceTarget' | 'workspaceSplit'> {
  if (!name || name === CURRENT_WS) return {};
  return {
    workspace: name,
    workspaceTarget: target ?? undefined,
    workspaceSplit: direction ?? undefined,
  };
}

function paneWorkspaceClaim(
  s: State,
  direction: 'horizontal' | 'vertical' | null,
  target: string | null,
): Pick<Parameters<typeof api.createPane>[0], 'workspace' | 'workspaceTarget' | 'workspaceSplit'> {
  return paneWorkspaceClaimForName(s.activeWorkspace, direction, target);
}

function insertPaneOrAppend(
  layout: Layout | null,
  paneId: string,
  direction: 'horizontal' | 'vertical' | null,
  target: string | null,
): Layout {
  const leaf: Layout = { type: 'pane', paneId };
  if (!layout) return leaf;
  if (direction && target && layoutHasLeaf(layout, target)) {
    return insertPane(layout, paneId, direction, target);
  }
  return {
    type: 'split',
    direction: direction ?? 'horizontal',
    ratio: 0.5,
    a: layout,
    b: leaf,
  };
}

async function saveWorkspaceSnapshot(
  get: () => State,
  snapshot: AutosaveSnapshot,
  opts: { refreshWorkspaces?: boolean } = {},
) {
  const scope = currentRoutingScope();
  if (workspaceMovedTo(snapshot.name)) return;
  const saved = await trackWorkspaceSave(api.saveWorkspace({
    name: snapshot.name,
    paneMoveVersion: snapshot.paneMoveVersion,
    layout: snapshot.layout,
    paneIds: collectPaneIds(snapshot.layout),
    panes: collectPaneDefs(snapshot.layout, snapshot.panes),
    browsers: collectBrowserInfos(snapshot.layout, snapshot.browsers),
    files: collectFileInfos(snapshot.layout, snapshot.files),
    activeAt: new Date().toISOString(),
  },scope)).catch((error) => {
    reportWorkspaceMoveConflict(get, snapshot.name, snapshot.paneMoveVersion, scope, error);
    throw error;
  });
  if(scope.key !== currentRoutingScope().key)return;
  // Write through to the cached entry even when refreshWorkspaces is
  // skipped (the workspace-switch flush passes refreshWorkspaces:false for
  // speed). Without this the outgoing layout we just persisted stays stale
  // in the cache and switching back restores the OLD arrangement.
  get().mergeSavedWorkspaceIntoCache(saved);
  if (opts.refreshWorkspaces !== false) {
    await get().refreshWorkspaces();
  }
}

async function persistCreatedPaneToOriginWorkspace(
  get: () => State,
  origin: AutosaveSnapshot | undefined,
  pane: PaneInfo,
  direction: 'horizontal' | 'vertical' | null,
  target: string | null,
) {
  if (!origin) return;
  const layout = insertPaneOrAppend(origin.layout, pane.id, direction, target);
  await saveWorkspaceSnapshot(get, {
    ...origin,
    layout,
    panes: { ...origin.panes, [pane.id]: pane },
  });
}

function sameStringArray(a: string[], b: string[]): boolean {
  if (a === b) return true;
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return false;
  }
  return true;
}

function samePaneInfo(a: PaneInfo, b: PaneInfo): boolean {
  return a.id === b.id
    && a.cwd === b.cwd
    && sameStringArray(a.cmd, b.cmd)
    && a.title === b.title
    && a.status === b.status
    && a.recoveryError === b.recoveryError
    && a.restorePending === b.restorePending
    // Account adoption can change while an idle terminal emits no output.
    && sameJsonValue(a.credential, b.credential)
    && sameJsonValue(a.codexAuth, b.codexAuth)
    && a.createdAt === b.createdAt
    && a.exitCode === b.exitCode
    && (a.resumable ?? false) === (b.resumable ?? false)
    && (a.resumeReady ?? false) === (b.resumeReady ?? false)
    && (a.identityBlocked ?? false) === (b.identityBlocked ?? false)
    && a.cols === b.cols
    && a.rows === b.rows
    && a.pid === b.pid
    && a.lastActivityMs === b.lastActivityMs
    && a.state === b.state
    // attention can flip while `state` holds steady (auto-pending →
    // needs-user on a decline/escalation); without these the poll would keep
    // the stale pane and the alert gating would never see the flip.
    && (a.attention ?? '') === (b.attention ?? '')
    && (a.attentionReason ?? '') === (b.attentionReason ?? '')
    // The unsent-message chip appears and clears with no other pane change.
    && sameJsonValue(a.unsentMessage, b.unsentMessage)
    // A review result lands with no other pane change.
    && sameJsonValue(a.review, b.review)
    // Restores rewrite a pane's starter/lead links with no other change.
    && sameJsonValue(a.origin, b.origin)
    // A handle or working-on line changes with no other pane change.
    && sameJsonValue(a.identity, b.identity)
    // An agent's message lands with no other pane change.
    && sameJsonValue(a.lastAgentMessage, b.lastAgentMessage)
    && a.hasClaude === b.hasClaude
    && a.hasCodex === b.hasCodex
    && a.hasMuse === b.hasMuse
    // hasPi flips true a beat after spawn (the daemon's process scan finds the
    // managed Pi approval extension on the child argv). Without it here,
    // refreshPanes would keep the stale DTO and the auto-approve badge would
    // never appear on a managed Pi pane.
    && (a.hasPi ?? false) === (b.hasPi ?? false)
    // hasDeepseek flips true a beat after spawn (the daemon's process scan
    // finds the DeepSeek terminal client on the child argv). Without it here,
    // refreshPanes would keep the stale DTO and the auto-approve badge would
    // never appear on a DeepSeek pane.
    && (a.hasDeepseek ?? false) === (b.hasDeepseek ?? false)
    && a.liveCwd === b.liveCwd
    && a.backend === b.backend
    && a.autoMode === b.autoMode
    && a.announceDelegation === b.announceDelegation
    && a.announceDelegationEvents === b.announceDelegationEvents
    && (a.contextPack ?? '') === (b.contextPack ?? '')
    && (a.contextPackStatus ?? '') === (b.contextPackStatus ?? '')
    && a.autoModeLastFiredMs === b.autoModeLastFiredMs
    && a.ariaBackoffStreak === b.ariaBackoffStreak
    && a.flagged === b.flagged
    && a.flagMemo === b.flagMemo
    && (a.flagOwner ?? '') === (b.flagOwner ?? '')
    && a.claudeSessionId === b.claudeSessionId
    // Codex thread discovery can land after process detection (`hasCodex` is
    // already true). These identity fields must independently invalidate the
    // cached DTO or refreshPanes would discard the newly recoverable thread and
    // the Terminal→Focus toggle would never appear.
    && (a.agent ?? '') === (b.agent ?? '')
    && (a.codexThreadId ?? '') === (b.codexThreadId ?? '')
    && (a.museSessionId ?? '') === (b.museSessionId ?? '')
    // Renderer kind + the two switch-state fields. Without these a deferred
    // switch that fires on an otherwise-idle pane (flips `kind`, nulls
    // `pendingSwitch`, may set `switchNotice`) changes NO compared field, so
    // refreshPanes keeps the stale pane and the pending indicator never clears.
    && (a.kind ?? '') === (b.kind ?? '')
    && sameJsonValue(a.pendingSwitch, b.pendingSwitch)
    // The buffered-update chip must invalidate the cached pane DTO.
    && sameJsonValue(a.delegatedPending, b.delegatedPending)
    // Turn tracking (working timer → "Δ files" chip, finished-turn unread).
    && sameJsonValue(a.turn, b.turn)
    && sameJsonValue(a.switchNotice, b.switchNotice)
    // agentMidTurn can flip true→false while `state` still reads 'busy' (the ~3s
    // post-turn redraw tail): without it here, refreshPanes would keep the stale
    // pane and the switch confirm wouldn't update to one-click on settle.
    && (a.agentMidTurn ?? false) === (b.agentMidTurn ?? false)
    // nativeEpoch changes on a daemon restart WITHOUT a pid/kind change (the agent
    // survives the swap), so without it here refreshPanes would keep the stale pane
    // and useNativePane's reset effect would never see the new epoch — the native
    // pane would jumble the re-seq'd reattach instead of resetting + re-seeding.
    && (a.nativeEpoch ?? 0) === (b.nativeEpoch ?? 0)
    // workflowStatus appears/changes/clears entirely on the poll (no SSE edge),
    // so without it here a background workflow's progress (agentsDone advancing,
    // lastActivityMs ticking, the final clear) would change no compared field and
    // refreshPanes would keep the stale pane — the footer would freeze.
    && sameJsonValue(a.workflowStatus, b.workflowStatus)
    && promptsEqual(a.lastPrompts, b.lastPrompts);
}

function sameJsonValue(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  return JSON.stringify(a) === JSON.stringify(b);
}

function workspaceCacheKey(w: { name: string; backend?: 'wsl' | 'win' }): string {
  return `${w.backend ?? ''}:${w.name}`;
}

function sameWorkspaceSummary(a: Workspace, b: WorkspaceSummary): boolean {
  return a.name === b.name
    && a.backend === b.backend
    && a.activeAt === b.activeAt
    && !!a.protected === !!b.protected
    && (a.contextPack ?? '') === (b.contextPack ?? '')
    && (a.paneMoveVersion ?? 0) === (b.paneMoveVersion ?? 0)
    && sameStringArray(a.paneIds, b.paneIds);
}

function mergeWorkspaceSummaries(current: Workspace[], summaries: WorkspaceSummary[]): Workspace[] {
  const byKey = new Map(current.map((w) => [workspaceCacheKey(w), w]));
  return summaries.map((summary) => {
    const prev = byKey.get(workspaceCacheKey(summary));
    if (prev) {
      if (sameWorkspaceSummary(prev, summary)) return prev;
      const cachedLayoutPaneIds = collectPaneIds(prev.layout);
      const fullCacheStale = !sameStringArray(cachedLayoutPaneIds, summary.paneIds)
        || (prev.paneMoveVersion ?? 0) !== (summary.paneMoveVersion ?? 0);
      return {
        ...prev,
        name: summary.name,
        paneIds: summary.paneIds,
        activeAt: summary.activeAt,
        backend: summary.backend,
        protected: summary.protected,
        contextPack: summary.contextPack,
        paneMoveVersion: summary.paneMoveVersion,
        summary: prev.summary || fullCacheStale,
      };
    }
    return {
      name: summary.name,
      layout: null,
      paneIds: summary.paneIds,
      activeAt: summary.activeAt,
      backend: summary.backend,
      protected: summary.protected,
      contextPack: summary.contextPack,
      paneMoveVersion: summary.paneMoveVersion,
      summary: true,
    };
  });
}

export function mergePaneInfos(
  cached: Record<string, PaneInfo>,
  fresh: PaneInfo[],
): Map<string, PaneInfo> {
  const aliveById = new Map(Object.entries(cached));
  for (const p of fresh) aliveById.set(p.id, p);
  return aliveById;
}

export function paneMapFromAliveById(aliveById: Map<string, PaneInfo>): Record<string, PaneInfo> {
  const out: Record<string, PaneInfo> = {};
  for (const [id, p] of aliveById) out[id] = p;
  return out;
}

export function resolveWorkspaceInitialLayout(
  layout: Layout | null,
  aliveById: Map<string, PaneInfo>,
  browsers: Record<string, BrowserInfo>,
  files: Record<string, FileTileInfo>,
): Layout | null {
  return layout
    ? pruneLayout(layout, (id) => isPresentPane(aliveById.get(id)) || !!browsers[id] || !!files[id])
    : null;
}

export function shouldBlankWorkspaceSwitch(outgoingLayout: Layout | null): boolean {
  return outgoingLayout ? collectPaneIds(outgoingLayout).length >= 3 : false;
}

function cmdLooksLikeCodex(cmd?: string[]): boolean {
  const first = cmd?.[0] ?? '';
  const base = first.split(/[\\/]/).pop() ?? first;
  return base === 'codex' || base === 'codex.exe';
}

function cmdLooksLikeMuse(cmd?: string[]): boolean {
  const first = cmd?.[0] ?? '';
  const base = first.split(/[\\/]/).pop() ?? first;
  return base === 'muse' || base === 'muse.exe';
}

function paneLooksCodex(p?: PaneInfo): boolean {
  return !!p && (!!p.hasCodex || !!p.hasMuse || cmdLooksLikeCodex(p.cmd) || cmdLooksLikeMuse(p.cmd));
}

export function workspaceSwitchTouchesCodex(
  outgoingLayout: Layout | null,
  incomingLayout: Layout | null,
  panes: Record<string, PaneInfo>,
  incomingDefs?: Array<{ id: string; cmd: string[] }>,
): boolean {
  const ids = new Set([...collectPaneIds(outgoingLayout), ...collectPaneIds(incomingLayout)]);
  const defs = new Map((incomingDefs ?? []).map((d) => [d.id, d]));
  for (const id of ids) {
    if (paneLooksCodex(panes[id])) return true;
    if (cmdLooksLikeCodex(defs.get(id)?.cmd) || cmdLooksLikeMuse(defs.get(id)?.cmd)) return true;
  }
  return false;
}

function removePaneFromWorkspaceCache(
  workspaces: Workspace[],
  activeWorkspace: string,
  activeBackend: 'wsl' | 'win' | null,
  paneId: string,
): Workspace[] {
  if (!activeWorkspace) return workspaces;
  let changed = false;
  const next = workspaces.map((w) => {
    if (w.name !== activeWorkspace) return w;
    if (activeBackend && w.backend && w.backend !== activeBackend) return w;
    const layout = removePane(w.layout ?? null, paneId);
    const paneIds = (w.paneIds ?? []).filter((id) => id !== paneId);
    const panes = (w.panes ?? []).filter((p) => p.id !== paneId);
    if (layout === w.layout && paneIds.length === (w.paneIds ?? []).length && panes.length === (w.panes ?? []).length) {
      return w;
    }
    changed = true;
    return {
      ...w,
      layout,
      paneIds,
      panes,
      activeAt: new Date().toISOString(),
      summary: false,
    };
  });
  return changed ? next : workspaces;
}

function sameWorkspaceListByRef(a: Workspace[], b: Workspace[]): boolean {
  if (a === b) return true;
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return false;
  }
  return true;
}

function lifecycleLayoutIds(layout: Layout | null): string[] {
  return layout ? collectPaneIds(layout) : [];
}

function perfNow(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}

function debugPerf(label: string, startedAt: number, detail?: unknown, thresholdMs = 80): void {
  if (!isSoluDebugEnabled()) return;
  const elapsed = perfNow() - startedAt;
  if (elapsed < thresholdMs) return;
  debugLog(`[perf] ${label}`, `${elapsed.toFixed(1)}ms`, detail ?? '');
}

// Cancel any pending autosave and flush it synchronously. Used before
// switching workspaces so in-flight edits don't get dropped.
//
// Pass `snapshot` when the caller is about to (or already has) changed
// the store's activeWorkspace — the snapshot pins the flush to the
// outgoing workspace. With no snapshot, the current store state is used.
async function flushAutosave(
  get: () => State,
  snapshot?: AutosaveSnapshot,
  opts: { refreshWorkspaces?: boolean; throwOnError?: boolean } = {},
) {
  if (saveTimer === null) return;
  window.clearTimeout(saveTimer);
  saveTimer = null;
  let src: AutosaveSnapshot;
  if (snapshot) {
    src = snapshot;
  } else {
    const s = get();
    if (!s.activeWorkspace) return;
    src = {
      name: s.activeWorkspace,
      paneMoveVersion: s.activeWorkspaceMoveVersion,
      layout: s.layout,
      panes: s.panes,
      browsers: s.browsers,
      files: s.files,
    };
  }
  try {
    await saveWorkspaceSnapshot(get, src, { refreshWorkspaces: opts.refreshWorkspaces });
  } catch (e) {
    if (opts.throwOnError) throw e;
  }
}

const NOTIFY_KEY = 'solu.notifyEnabled';
const notifyDefault = (() => {
  try { return localStorage.getItem(NOTIFY_KEY) === '1'; } catch { return false; }
})();

const CACHED_FIRST_WORKSPACE_SWITCH_KEY = 'solu.cachedFirstWorkspaceSwitch';
// Default ON since the perf pass: the cached pane list is refreshed by
// the 3s poll and Phase 2 reconciles against a fresh listPanes anyway,
// so blocking first paint on a listPanes round-trip bought nothing for
// the common case. Set localStorage 'solu.cachedFirstWorkspaceSwitch'
// to '0' to restore the conservative blocking fetch. Codex-touching
// switches always take the conservative path regardless (see
// loadWorkspace).
export function isCachedFirstWorkspaceSwitchEnabled(): boolean {
  try { return localStorage.getItem(CACHED_FIRST_WORKSPACE_SWITCH_KEY) !== '0'; } catch { return true; }
}

// Warm pane keep-alive across workspace switches. When enabled
// (desktop only; '0' opts out), panes leaving the active layout are
// "parked": their xterm + WebSocket stay alive in a hidden container,
// so switching back reattaches the existing DOM instead of paying
// remount + daemon replay + SIGWINCH repaints. See WarmPaneLayer.
const WARM_PANES_KEY = 'solu.warmPanes';
export const WARM_PANE_CAP = 12;
export function warmPanesEnabled(): boolean {
  try {
    if (localStorage.getItem(WARM_PANES_KEY) === '0') return false;
  } catch { /* default on */ }
  return !isMobileViewport();
}

// Sampled once per session: PaneGrid (slot vs direct render),
// WarmPaneLayer (portal ownership), and loadWorkspace (parking) must
// all agree — flapping mid-session (e.g. a browser window dragged
// across the mobile breakpoint) would remount every terminal.
let warmPanesActiveSample: boolean | null = null;
export function warmPanesActive(): boolean {
  if (warmPanesActiveSample === null) warmPanesActiveSample = warmPanesEnabled();
  return warmPanesActiveSample;
}

// LRU for parked panes: most-recently-parked first (the outgoing
// workspace's panes), then previously parked ones, skipping anything
// in the incoming layout or dead. Overflow beyond
// `cap` falls off and unmounts normally.
export function computeParkedPaneIds(
  prevParked: string[],
  outgoingIds: string[],
  incomingIds: Set<string>,
  panes: Record<string, PaneInfo>,
  cap = WARM_PANE_CAP,
): string[] {
  const next: string[] = [];
  const seen = new Set<string>();
  const order = [...outgoingIds, ...prevParked];
  // Pass 1: PIN running codex panes regardless of `cap`. A codex pane runs in
  // the normal buffer and replays up to ~2 MiB on a COLD reattach, which
  // saturates the single shared relay connection — the root of the "Aria"
  // workspace's slow switches, false "daemon unreachable", and wedged reloads.
  // Keeping it parked (WS + xterm alive) makes switch-back a pure DOM move with
  // NO replay, so the codex pane must never be the one evicted. These don't
  // count against the cap (codex panes are few; the cheap shell/Claude panes
  // are what the cap bounds).
  for (const id of order) {
    if (seen.has(id) || incomingIds.has(id)) continue;
    const p = panes[id];
    if (!p || p.status !== 'running' || !paneLooksCodex(p)) continue;
    seen.add(id);
    next.push(id);
  }
  // Pass 2: fill the remaining (non-codex) running panes up to `cap`, by
  // recency (outgoing first, then previously-parked).
  let nonCodex = 0;
  for (const id of order) {
    if (seen.has(id) || incomingIds.has(id)) continue;
    const p = panes[id];
    if (!p || p.status !== 'running') continue;
    seen.add(id);
    if (nonCodex < cap) { next.push(id); nonCodex += 1; }
  }
  return next;
}

// One-shot cleanup: prompt history moved daemon-side (served in the
// pane DTO, persisted in panes.json), so the old per-client
// localStorage copy is dead weight. Safe to run every load.
try { localStorage.removeItem('solu.lastPrompts'); } catch { /* ignore */ }

// promptsEqual treats two histories as identical content so reconcile
// paths can preserve the previous array reference — selectors key on it.
function promptsEqual(a: string[] | undefined, b: string[] | undefined): boolean {
  if (a === b) return true;
  if (!a || !b || a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return false;
  }
  return true;
}

// Histories use the same scoped identity as persistent terminal cores. A
// snapshot can prune only its own scope; another backend's retained history
// must survive. Reconcile even when the pane metadata itself is unchanged.
function reconcilePanePromptHistory(
  previous: Record<string, string[]>, panes: PaneInfo[], scope: RoutingScope,
  complete: boolean,
): Record<string, string[]> {
  let next = previous;
  const liveKeys = new Set<string>();
  const remove = (key: string) => {
    if (!(key in next)) return;
    if (next === previous) next = { ...previous };
    delete next[key];
  };
  for (const pane of panes) {
    const key = terminalSessionKey(scope, pane.id);
    liveKeys.add(key);
    const prompts = pane.lastPrompts;
    if (!prompts?.length) {
      remove(key);
    } else if (!promptsEqual(previous[key], prompts)) {
      if (next === previous) next = { ...previous };
      next[key] = prompts;
    }
  }
  if (complete) {
    for (const key of Object.keys(previous)) {
      // JSON's opening array/string prefix is unambiguous, including when a
      // machine prefix contains quotes. No raw-ID aliases are consulted.
      if (key.startsWith(`[${JSON.stringify(scope.key)},`) && !liveKeys.has(key)) remove(key);
    }
  }
  return next;
}

// Stale-bundle recovery is based on the shell origin's independent webdist
// provenance, not the active API daemon version. During dual-backend rollout
// WSL can be new while the Windows shell is still old; reloading in that gap is
// pointless. Once the shell origin advertises new bytes, bounded attempts are
// safe. A packaged mismatch never reloads.
// reloadForFreshBundle — reload in a way that actually fetches the NEW shell.
// A plain window.location.reload() in WebView2 re-serves the cached index.html
// (which still points at the old chunks), so the stale-bundle self-heal would
// loop-once-then-give-up without ever advancing. Navigating to a one-time
// versioned URL changes the HTTP cache key → guaranteed network fetch of a fresh
// index.html. (Belt-and-suspenders with the daemon's no-store shell headers.)
// How long to wait for a reload navigation to actually commit before assuming
// it's wedged and retrying. A reload nav over a saturated cloud origin (e.g. a
// codex replay starving the connection pool) can hang with NO error — leaving
// the user with a "reload does nothing" state until they kill the app. The
// codex warm-pinning above removes that saturation at the source; this is the
// belt-and-suspenders so a stuck reload self-heals instead of silently hanging.
const RELOAD_WATCHDOG_MS = 6_000;
const RELOAD_MAX_ATTEMPTS = 3;

// Purging caches before navigating: a `_v` query is enough to re-fetch
// index.html (it is served no-store), but a webview that has the shell in a
// service-worker cache will keep handing back the OLD index — and with it the
// old hashed asset names — no matter how many times we navigate. Observed
// 2026-08-09: the update banner survived repeated Reload clicks because the
// page kept booting the previous bundle. Best-effort and never blocking: if
// the purge hangs or is unsupported, we navigate anyway.
async function purgeShellCaches(): Promise<void> {
  if (isEmbedded()) return; // The standalone tab owns this origin's SW and caches.
  const jobs: Promise<unknown>[] = [];
  try {
    if (typeof caches !== 'undefined') {
      jobs.push(caches.keys().then((keys) => Promise.all(keys.map((k) => caches.delete(k)))));
    }
  } catch { /* no Cache API — nothing to purge */ }
  try {
    const sw = navigator.serviceWorker;
    if (sw) jobs.push(sw.getRegistrations().then((rs) => Promise.all(rs.map((r) => r.unregister()))));
  } catch { /* no SW — nothing to unregister */ }
  if (jobs.length === 0) return;
  await Promise.race([
    Promise.allSettled(jobs),
    new Promise((resolve) => window.setTimeout(resolve, 1500)),
  ]);
}

function reloadForFreshBundle(daemonVersion: string | undefined): void {
  // Remember what we tried to reload TO. If the next boot still disagrees with
  // the daemon, the reload did not take and telling the user to click it again
  // is a lie — the banner escalates to "restart the app" instead.
  try { sessionStorage.setItem('solu.reloadAttemptedFor', daemonVersion ?? ''); } catch { /* private mode */ }
  // Hard cross-navigation throttle, belt-and-suspenders with the per-target
  // attempt cap: even if a parity signal keeps flipping (shell and running
  // bundle disagreeing across a rebuild), never navigate more than once per
  // 15s. Prevents a self-reload storm (2026-09-20) while still letting a
  // genuine fresh bundle land.
  try {
    const last = Number(sessionStorage.getItem('solu.freshBundleReloadAt') ?? '0');
    if (Number.isFinite(last) && Date.now() - last < 15_000) return;
    sessionStorage.setItem('solu.freshBundleReloadAt', String(Date.now()));
  } catch { /* private mode: fall through to the attempt-capped path */ }
  const nav = () => {
    try {
      // Fresh `_v` per attempt → guaranteed new HTTP cache key (a plain reload
      // re-serves the cached index.html in WebView2). A new location assignment
      // also cancels any still-pending (stuck) navigation and starts clean.
      const url = new URL(window.location.href);
      url.searchParams.set('_v', `${daemonVersion ?? 'x'}-${Date.now()}`);
      window.location.replace(url.toString());
    } catch {
      try { window.location.reload(); } catch { /* nothing more we can do */ }
    }
  };
  let attempts = 1;
  const retry = () => {
    // If this fires, the previous navigation never committed (the page is still
    // alive). Timers are discarded the instant a nav commits, so reaching here
    // means it's genuinely stuck — cancel it and retry.
    if (attempts >= RELOAD_MAX_ATTEMPTS) return; // give up; manual restart needed
    attempts += 1;
    nav();
    window.setTimeout(retry, RELOAD_WATCHDOG_MS);
  };
  nav();
  window.setTimeout(retry, RELOAD_WATCHDOG_MS);
}

export function shouldReloadForStaleBundle(bundleState: BundleParityState): boolean {
  return shouldAutoReloadBundle(bundleState);
}

let pendingBundleReloadTarget = '';
export function scheduleStaleBundleReload(bundleState: BundleParityState): boolean {
  const target = bundleReloadTarget(bundleState);
  if (!target || pendingBundleReloadTarget === target) return false;
  if (!shouldReloadForStaleBundle(bundleState)) return false;
  pendingBundleReloadTarget = target;
  window.setTimeout(() => {
    void purgeShellCaches().finally(() => {
      reloadForFreshBundle(target);
      // A committed navigation destroys this module. If it never commits, let
      // the next health cycle make another bounded high-level attempt.
      window.setTimeout(() => {
        if (pendingBundleReloadTarget === target) pendingBundleReloadTarget = '';
      }, RELOAD_WATCHDOG_MS * RELOAD_MAX_ATTEMPTS + 1_000);
    });
  }, 400);
  return true;
}

function waitForShellBundle(targetVersion: string | undefined): boolean {
  const observed = getDaemonHealthState().bundleState;
  const refined = classifyBundleParity({
    runningBundleVersion: observed.runningBundleVersion,
    shellDaemonVersion: observed.shellDaemonVersion,
    shellWebVersion: observed.shellWebVersion,
    targetVersion: newestVersion(observed.targetVersion, targetVersion),
  });
  const scheduled = scheduleStaleBundleReload(refined);
  if (!scheduled) refreshDaemonHealth();
  return scheduled;
}

function recoveryReachedSuccess(controller: UpdateRecoveryController, status: UpdateStatus): boolean {
  if (!status.current) return false;
  if (controller.targetVersion && status.current === controller.targetVersion) return true;
  return !!controller.beforeVersion && status.current !== controller.beforeVersion;
}

function detachUpdateRecovery(): UpdateRecoveryController | null {
  const controller = updateRecovery;
  updateRecovery = null;
  updateRecoveryGeneration += 1;
  if (controller?.pollTimer !== null && controller?.pollTimer !== undefined) {
    window.clearTimeout(controller.pollTimer);
    controller.pollTimer = null;
  }
  if (controller?.deadlineTimer !== null && controller?.deadlineTimer !== undefined) {
    window.clearTimeout(controller.deadlineTimer);
    controller.deadlineTimer = null;
  }
  return controller;
}

function invalidatePendingUpdateAdmissionConfirmations(): void {
  updateAdmissionConfirmationGeneration += 1;
}

function rememberSettledUpdateRecovery(controller: UpdateRecoveryController | null): void {
  const status = useStore.getState().updateStatus;
  const targetVersion = controller?.targetVersion ?? status?.latest ?? '';
  const beforeVersion = controller?.beforeVersion ?? status?.current ?? '';
  if (targetVersion || beforeVersion) {
    settledUpdateRecovery = {
      beforeVersion,
      targetVersion,
      maxApplyAttemptGeneration: updateApplyAttemptGeneration,
    };
  }
  invalidatePendingUpdateAdmissionConfirmations();
}

function settledRecoveryBlocks(
  targetVersion: string,
  applyAttemptGeneration?: number,
): boolean {
  const settled = settledUpdateRecovery;
  if (!settled) return false;
  const sameTarget = !settled.targetVersion || !targetVersion
    || settled.targetVersion === targetVersion;
  if (!sameTarget) {
    settledUpdateRecovery = null;
    return false;
  }
  if (
    applyAttemptGeneration !== undefined
    && applyAttemptGeneration > settled.maxApplyAttemptGeneration
  ) {
    // Only a newly-started POST can establish a new attempt for the same
    // target. Old HTTP responses and later status/SSE observations remain
    // attached to the settled attempt and cannot restart its 30s deadline.
    settledUpdateRecovery = null;
    return false;
  }
  return true;
}

function clearUpdateRecovery(status?: UpdateStatus): void {
  const controller = detachUpdateRecovery();
  invalidatePendingUpdateAdmissionConfirmations();
  if (status) {
    useStore.setState({ updateStatus: { ...normalizeUpdateStatus(status), applying: false } });
  } else {
    useStore.setState((state) => ({
      updateStatus: state.updateStatus
        ? { ...state.updateStatus, applying: false }
        : state.updateStatus,
    }));
  }
  controller?.resolve();
}

function completeUpdateRecovery(status: UpdateStatus): void {
  const controller = detachUpdateRecovery();
  if (!controller) return;
  rememberSettledUpdateRecovery(controller);
  cancelDeferredUpdateRetry();
  deferredUpdateRetryVersion = undefined;
  const fresh = { ...normalizeUpdateStatus(status), applying: false };
  useStore.setState({ updateStatus: fresh, deferredUpdate: null });
  if (controller.skipReload) {
    useStore.getState().toast(`Updated to ${fresh.current}`, { kind: 'success', icon: '✓' });
  } else {
    const reloading = waitForShellBundle(fresh.current);
    useStore.getState().toast(
      reloading ? `Updated to ${fresh.current} — reloading…` : `Updated to ${fresh.current} — finishing the shell update…`,
      { kind: 'success', icon: '✓' },
    );
  }
  controller.resolve();
}

function timeoutUpdateRecovery(): void {
  const controller = detachUpdateRecovery();
  if (!controller) return;
  rememberSettledUpdateRecovery(controller);
  useStore.setState((state) => ({
    updateStatus: state.updateStatus
      ? { ...state.updateStatus, applying: false }
      : state.updateStatus,
  }));
  useStore.getState().toast('Update applied but daemon did not return — check logs or restart the app', {
    kind: 'warn',
    icon: '⚠',
  });
  controller.resolve();
}

function scheduleUpdateRecoveryPoll(controller: UpdateRecoveryController): void {
  if (updateRecovery !== controller || controller.generation !== updateRecoveryGeneration) return;
  const remaining = controller.deadlineMs - Date.now();
  if (remaining <= 0) {
    timeoutUpdateRecovery();
    return;
  }
  controller.pollTimer = window.setTimeout(() => {
    controller.pollTimer = null;
    void pollUpdateRecovery(controller);
  }, Math.min(UPDATE_RECOVERY_POLL_MS, remaining));
}

async function pollUpdateRecovery(controller: UpdateRecoveryController): Promise<void> {
  if (updateRecovery !== controller || controller.generation !== updateRecoveryGeneration) return;
  try {
    const fresh = normalizeUpdateStatus(await api.getUpdateStatus());
    if (updateRecovery !== controller || controller.generation !== updateRecoveryGeneration) return;
    if (fresh.workflowDeferred) {
      clearUpdateRecovery(fresh);
      return;
    }
    if (recoveryReachedSuccess(controller, fresh)) {
      completeUpdateRecovery(fresh);
      return;
    }
    // The predecessor can answer applying=false between Apply returning and
    // releasing its listener. Preserve the admitted handoff until a successor,
    // workflow hold, abort event, or the original deadline settles it.
    useStore.setState({ updateStatus: { ...fresh, applying: true } });
  } catch {
    // Relay reconnects and predecessor shutdown are expected during handoff.
    // They cannot extend the original deadline.
  }
  scheduleUpdateRecoveryPoll(controller);
}

function beginUpdateRecovery(opts: {
  beforeVersion?: string;
  targetVersion?: string;
  skipReload?: boolean;
  admissionConfirmed?: boolean;
  applyAttemptGeneration?: number;
}): Promise<void> {
  const state = useStore.getState();
  const status = state.updateStatus;
  if (status?.workflowDeferred && !opts.admissionConfirmed) {
    clearUpdateRecovery(status);
    return Promise.resolve();
  }
  const targetVersion = opts.targetVersion ?? status?.latest ?? '';
  const beforeVersion = opts.beforeVersion ?? status?.current ?? '';
  if (settledRecoveryBlocks(targetVersion, opts.applyAttemptGeneration)) {
    useStore.setState((current) => ({
      updateStatus: current.updateStatus
        ? { ...current.updateStatus, applying: false }
        : current.updateStatus,
    }));
    return Promise.resolve();
  }
  if (targetVersion && beforeVersion === targetVersion && status?.applying !== true) {
    return Promise.resolve();
  }
  if (updateRecovery) {
    const sameTarget = updateRecovery.targetVersion === targetVersion
      || !updateRecovery.targetVersion
      || !targetVersion;
    if (sameTarget) {
      if (!updateRecovery.targetVersion) updateRecovery.targetVersion = targetVersion;
      updateRecovery.skipReload = updateRecovery.skipReload || opts.skipReload === true;
      return updateRecovery.promise;
    }
    clearUpdateRecovery();
  }

  cancelDeferredUpdateRetry();
  deferredUpdateRetryVersion = undefined;
  const generation = ++updateRecoveryGeneration;
  let resolve = () => {};
  const promise = new Promise<void>((done) => { resolve = done; });
  const controller: UpdateRecoveryController = {
    generation,
    beforeVersion,
    targetVersion,
    deadlineMs: Date.now() + UPDATE_RECOVERY_TIMEOUT_MS,
    skipReload: opts.skipReload === true,
    pollTimer: null,
    deadlineTimer: null,
    promise,
    resolve,
  };
  updateRecovery = controller;
  controller.deadlineTimer = window.setTimeout(() => {
    if (updateRecovery === controller && controller.generation === updateRecoveryGeneration) {
      timeoutUpdateRecovery();
    }
  }, UPDATE_RECOVERY_TIMEOUT_MS);
  useStore.setState((current) => ({
    deferredUpdate: null,
    updateStatus: current.updateStatus
      ? {
          ...current.updateStatus,
          latest: targetVersion || current.updateStatus.latest,
          applying: true,
          ...(opts.admissionConfirmed ? {
            workflowDeferred: false,
            workflowDeferReason: undefined,
            workflowDeferSinceMs: undefined,
            workflowDeferDeadlineMs: undefined,
            workflowInflight: undefined,
            workflowDeferAttempts: undefined,
            workflowNextRetryMs: undefined,
            workflowDeferState: undefined,
          } : {}),
        }
      : { current: beforeVersion, latest: targetVersion || undefined, available: true, applying: true },
  }));
  scheduleUpdateRecoveryPoll(controller);
  return promise;
}

function syncDeferredHoldFromStatus(status: UpdateStatus): UpdateHold | null {
  const current = useStore.getState();
  const versionChanged =
    !!current.deferredUpdate &&
    !!deferredUpdateRetryVersion &&
    !!status.latest &&
    status.latest !== deferredUpdateRetryVersion;
  if (versionChanged) {
    cancelDeferredUpdateRetry();
    deferredUpdateRetryVersion = undefined;
    return null;
  }
  const hold = current.deferredUpdate;
  if (!hold || hold.cause !== 'workflow') return hold;
  const drainEnded = status.workflowDeferred !== true;
  return {
    ...hold,
    reason: drainEnded ? undefined : (status.workflowDeferReason || hold.reason),
    workflowInflight: drainEnded ? undefined : (status.workflowInflight ?? hold.workflowInflight),
    workflowDeferDeadlineMs: drainEnded
      ? undefined
      : (status.workflowDeferDeadlineMs ?? hold.workflowDeferDeadlineMs),
    workflowNextRetryMs: status.workflowDeferState === 'backoff'
      ? (status.workflowNextRetryMs ?? hold.workflowNextRetryMs)
      : undefined,
  };
}

function observeAuthoritativeUpdateStatus(
  received: UpdateStatus,
  eventTargetVersion?: string,
): 'normal' | 'recovering' | 'completed' | 'workflow-hold' {
  const status = normalizeUpdateStatus(received);
  const deferredUpdate = syncDeferredHoldFromStatus(status);
  if (status.workflowDeferred) {
    clearUpdateRecovery(status);
    useStore.setState({ updateStatus: { ...status, applying: false }, deferredUpdate });
    return 'workflow-hold';
  }
  if (updateRecovery) {
    if (recoveryReachedSuccess(updateRecovery, status)) {
      completeUpdateRecovery(status);
      return 'completed';
    }
    useStore.setState({ updateStatus: { ...status, applying: true }, deferredUpdate });
    return 'recovering';
  }
  const targetVersion = eventTargetVersion ?? status.latest ?? '';
  const eventAlreadyCompleted = !!eventTargetVersion && status.current === eventTargetVersion;
  if (status.applying && !eventAlreadyCompleted) {
    // Install the level as non-applying first. beginUpdateRecovery performs
    // the admitted transition synchronously unless this target already
    // reached a terminal timeout/abort/completion state.
    useStore.setState({ updateStatus: { ...status, applying: false }, deferredUpdate });
    void beginUpdateRecovery({
      beforeVersion: status.current,
      targetVersion,
    });
    return useStore.getState().updateStatus?.applying ? 'recovering' : 'normal';
  }
  useStore.setState({ updateStatus: { ...status, applying: false }, deferredUpdate });
  return 'normal';
}

// Tests reset this module-owned controller between fake-clock cases. Product
// code uses the store's abortUpdateRecovery action instead.
export function resetUpdateRecoveryForTests(): void {
  clearUpdateRecovery();
  settledUpdateRecovery = null;
  updateApplyAttemptGeneration = 0;
}

const WS_ORDER_KEY = 'solu.wsOrder';
const wsOrderDefault: string[] = (() => {
  try {
    const raw = localStorage.getItem(WS_ORDER_KEY);
    return raw ? (JSON.parse(raw) as string[]) : [];
  } catch { return []; }
})();

const WS_VIEWED_KEY = 'solu.wsLastViewed';
const wsLastViewedDefault: Record<string, number> = (() => {
  try {
    const raw = localStorage.getItem(WS_VIEWED_KEY);
    return raw ? (JSON.parse(raw) as Record<string, number>) : {};
  } catch { return {}; }
})();

// The workspace the user was actively viewing in their last session.
// Persisted on every activeWorkspace change (see subscribe at bottom of
// file) so bootstrap can restore it across reloads — notably the full
// page reload triggered by auto-update, but also Tauri restart / F5.
// Before this existed, bootstrap fell back to "freshest activeAt",
// which silently landed you in whichever workspace you most recently
// EDITED rather than where you were SITTING.
const ACTIVE_WS_KEY = 'solu.activeWorkspace';
// A superseded bootstrap may finish a request, but cannot publish its layout
// or clear the loading state of a newer backend/bootstrap operation.
let bootstrapSequence = 0;
let portsRefreshSequence = 0;
let portsAppliedSequence = 0;
// Per-window namespaced key: secondary (multi-monitor) windows restore
// their OWN last-viewed workspace, not a shared one. The primary window
// keeps the legacy un-suffixed key (backward compat — see windowScopedKey).
const activeWorkspaceRemembered: string | null = (() => {
  try { return localStorage.getItem(windowScopedKey(ACTIVE_WS_KEY)); } catch { return null; }
})();

const THEME_KEY = 'solu.terminalTheme';
const themeDefault: string = (() => {
  try { return localStorage.getItem(THEME_KEY) || DEFAULT_THEME_NAME; }
  catch { return DEFAULT_THEME_NAME; }
})();
const themeExplicitDefault: boolean = (() => {
  try { return localStorage.getItem(THEME_KEY) != null; }
  catch { return false; }
})();

// New installs follow the OS; existing installs without a choice stay Dark.
// See appThemeDefault.ts (and the pre-paint script in index.html).
const appThemeDefault: AppThemePref = (() => {
  try { return initialAppThemePref(localStorage); } catch { return 'dark'; }
})();
const appThemeLightDefault: boolean = (() => {
  try {
    return appThemeDefault === 'light'
      || (appThemeDefault === 'system'
        && typeof window !== 'undefined'
        && typeof window.matchMedia === 'function'
        && window.matchMedia('(prefers-color-scheme: light)').matches);
  } catch { return false; }
})();

// Font size / line height persistence. Defaults match the legacy
// Terminal constructor values (13 px, 1.2) so existing users see no
// visual change on upgrade. Clamps: 10–22 px covers phone to 4K
// monitor without becoming pixel-art or absurd; 1.0–1.8 line-height
// covers tight (code review) to airy (readability) without breaking
// xterm's cell calculations.
const FONT_SIZE_KEY = 'solu.terminalFontSize';
const LINE_HEIGHT_KEY = 'solu.terminalLineHeight';
const UI_SCALE_KEY = 'solu.uiScale';
const FOCUS_FONT_OFFSET_KEY = 'solu.focusFontOffset';
const FONT_SIZE_MIN = 10;
const FONT_SIZE_MAX = 22;
const LINE_HEIGHT_MIN = 1.0;
const LINE_HEIGHT_MAX = 1.8;
const FOCUS_OFFSET_MIN = 0;
const FOCUS_OFFSET_MAX = 8;
function clampFontSize(n: number): number {
  if (!Number.isFinite(n)) return 14;
  return Math.max(FONT_SIZE_MIN, Math.min(FONT_SIZE_MAX, Math.round(n)));
}
function clampLineHeight(n: number): number {
  if (!Number.isFinite(n)) return 1.2;
  return Math.max(LINE_HEIGHT_MIN, Math.min(LINE_HEIGHT_MAX, Math.round(n * 10) / 10));
}
function clampFocusOffset(n: number): number {
  if (!Number.isFinite(n)) return 0.4;
  // Snap to 0.1 steps so the pref accepts fine offsets (e.g. +1.3).
  return Math.max(FOCUS_OFFSET_MIN, Math.min(FOCUS_OFFSET_MAX, Math.round(n * 10) / 10));
}
const fontSizeDefault: number = (() => {
  try {
    const raw = localStorage.getItem(FONT_SIZE_KEY);
    if (!raw) return 14;
    return clampFontSize(parseInt(raw, 10));
  } catch { return 14; }
})();
const lineHeightDefault: number = (() => {
  try {
    const raw = localStorage.getItem(LINE_HEIGHT_KEY);
    if (!raw) return 1.2;
    return clampLineHeight(parseFloat(raw));
  } catch { return 1.2; }
})();
const focusFontOffsetDefault: number = (() => {
  try {
    const raw = localStorage.getItem(FOCUS_FONT_OFFSET_KEY);
    if (raw == null) return 0.4;
    return clampFocusOffset(parseFloat(raw));
  } catch { return 0.4; }
})();

// UI scale (zoom factor for sidebar + topbar chrome). Continuous to
// match the terminal font-size knob above it. Range covers comfortably-
// dense (0.7) to large-display (1.4); step 0.05 lets the user dial in
// fractional pixel imperfections without typing decimals.
const UI_SCALE_MIN = 0.7;
const UI_SCALE_MAX = 1.4;
function clampUiScale(n: number): number {
  if (!Number.isFinite(n)) return 1.0;
  return Math.max(UI_SCALE_MIN, Math.min(UI_SCALE_MAX, Math.round(n * 20) / 20));
}
const uiScaleDefault: number = (() => {
  try {
    const raw = localStorage.getItem(UI_SCALE_KEY);
    if (!raw) return 1.0;
    return clampUiScale(parseFloat(raw));
  } catch { return 1.0; }
})();
const PANE_LAYOUT_MODE_KEY = 'solu.paneLayoutMode';
const paneLayoutModeDefault: 'auto' | 'free' = (() => {
  try {
    const raw = localStorage.getItem(PANE_LAYOUT_MODE_KEY);
    if (raw === 'free' || raw === 'auto') return raw;
    return 'auto';
  } catch { return 'auto'; }
})();
// Native bottom-bar vitals visibility — one JSON blob (structured object, so it
// follows the customKeybindings blob idiom, not the scalar prefs). Missing/corrupt
// → sane defaults; a partial blob is filled in per-field by sanitizeMetaBarFields.
const NATIVE_METABAR_KEY = 'solu.nativeMetaBar';
const nativeMetaBarDefault: NativeMetaBarFields = (() => {
  try {
    const raw = localStorage.getItem(NATIVE_METABAR_KEY);
    if (!raw) return { ...NATIVE_METABAR_DEFAULTS };
    return sanitizeMetaBarFields(JSON.parse(raw));
  } catch { return { ...NATIVE_METABAR_DEFAULTS }; }
})();

// Mouse copy/paste parity, persisted per-browser. Absent key → on, so
// existing installs adopt the convention; '0' is the explicit opt-out.
const COPY_ON_SELECT_KEY = 'solu.copyOnSelect';
const MIDDLE_CLICK_PASTE_KEY = 'solu.middleClickPaste';
function readBoolPref(key: string): boolean {
  try {
    const raw = localStorage.getItem(key);
    return raw === null ? true : raw === '1';
  } catch { return true; }
}

const copyOnSelectDefault: boolean = readBoolPref(COPY_ON_SELECT_KEY);
const middleClickPasteDefault: boolean = readBoolPref(MIDDLE_CLICK_PASTE_KEY);

// The prompt-history strip at the top of Claude panes (PaneLastPrompt, both
// renderers). Absent key → on (the long-standing behavior); '0' hides it.
const SHOW_PROMPT_HISTORY_KEY = 'solu.showPromptHistory';
const showPromptHistoryDefault: boolean = readBoolPref(SHOW_PROMPT_HISTORY_KEY);

const KEYBINDINGS_KEY = 'solu.customKeybindings';
const keybindingsDefault: Record<string, string> = (() => {
  try {
    const raw = localStorage.getItem(KEYBINDINGS_KEY);
    if (!raw) return {};
    const parsed = JSON.parse(raw);
    // Defensive: reject anything that isn't string→string so a
    // malformed localStorage blob doesn't crash App.tsx's onKey.
    if (!parsed || typeof parsed !== 'object') return {};
    const out: Record<string, string> = {};
    for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
      if (typeof k === 'string' && typeof v === 'string' && v) out[k] = v;
    }
    return out;
  } catch { return {}; }
})();

// Backstop for ending the renderer-switch overlay when the switch-mode POST
// response is lost or slow. The kill+respawn of the very pane being switched
// can drop its own switch-mode response even though the switch landed and
// persisted server-side, so we never let that POST alone drive the overlay.
// Must exceed the daemon's worst-case respawn made visible by the 3s panes
// poll (settle ≤1.5s + up to 3 spawn attempts ≈5.5s + ≤3s poll) yet stay under
// api.ts API_FETCH_TIMEOUT_MS (30s).
const SWITCH_RECONCILE_TIMEOUT_MS = 20_000;

// Resolve once the panes poll/SSE shows pane `id` respawned — a NEW valid pid
// distinct from `prevPid`. A Term↔Focus switch ALWAYS kills and respawns the
// agent (both directions), so a changed pid is the authoritative "the switch
// actually landed" signal, independent of the (possibly lost) switch-mode POST
// response. Resolves true on observing the respawn, false on timeout.
function waitForPaneRespawn(
  id: string,
  prevPid: number | undefined,
  timeoutMs: number,
): Promise<boolean> {
  const respawned = () => {
    const p = useStore.getState().panes[id];
    return !!p && typeof p.pid === 'number' && p.pid > 0 && p.pid !== prevPid;
  };
  if (respawned()) return Promise.resolve(true);
  return new Promise<boolean>((resolve) => {
    let done = false;
    const finish = (v: boolean) => {
      if (done) return;
      done = true;
      unsub();
      clearTimeout(timer);
      resolve(v);
    };
    const unsub = useStore.subscribe(() => {
      if (respawned()) finish(true);
    });
    const timer = setTimeout(() => finish(false), timeoutMs);
  });
}

// Build the spawn request used by Clone pane / Alt+Enter. Role is deliberately
// included only when set so a clone of an orchestrator is faithful while the
// request shape for every existing plain pane stays unchanged.
export function cloneSpawnOpts(src: PaneInfo, cwd: string) {
  return {
    cwd,
    cmd: src.cmd,
    title: src.title,
    ...(src.agent ? { agent: src.agent } : {}),
    ...(src.role ? { role: src.role } : {}),
    ...(src.contextPack ? { contextPack: src.contextPack } : {}),
  };
}

const PANE_LAST_VIEWED_KEY = 'solu.paneLastViewed';
const PANE_LAST_VIEWED_CAP = 300;
function loadPaneLastViewed(): Record<string, number> {
  try {
    const raw = localStorage.getItem(PANE_LAST_VIEWED_KEY);
    const parsed = raw ? JSON.parse(raw) : null;
    if (!parsed || typeof parsed !== 'object') return {};
    const out: Record<string, number> = {};
    for (const [k, v] of Object.entries(parsed)) if (typeof v === 'number') out[k] = v;
    return out;
  } catch {
    return {};
  }
}
export const useStore = create<State>((set, get, storeApi) => ({
  panes: {},
  browsers: {},
  layout: null,
  parkedPaneIds: [],
  ports: [],
  portsError: null,
  portsUpdatedAt: null,
  workspaces: [],
  schedules: [],
  orchestratorHosts: null,
  recoverySettings: null,
  nativeSettings: null,
  updateSettings: null,
  wardenSettings: null,
  updateStatus: null,
  updateApplyRequestInFlight: false,
  deferredUpdate: null,
  rollbackDecline: null,
  shellAutoRestart: null,
  mcpServersByCwd: {},
  activeWorkspace: CURRENT_WS,
  activeWorkspaceMoveVersion: 0,
  activeBackend: isRouterMode() ? activeBackendFromUrl() : null,
  mainPresentation: { kind: 'workspace' },
  ownerReturnContext: null,
  navigationGeneration: 0,
  focusedPaneId: null,
  lastUserFocusId: null,
  recentlyRevealedPaneId: null,
  loading: true,
  bootstrapError: null,
  bootstrapNeedsApproval: false,
  lastViewedMs: loadPaneLastViewed(),
  clientActivityMs: {},
  lastPrompts: {},
  paneModeSwitching: {},
  senders: {},
  claimers: {},
  redrawers: {},
  realigners: {},
  paneOwnership: {},
  paneOwnerState: {},
  paneOwnershipTokens: {},
  claimIntent: false,
  notifyEnabled: notifyDefault,
  bannerSetupOpen: false,
  setBannerSetupOpen: (v) => set({ bannerSetupOpen: v }),
  feedbackOpen: false,
  feedbackView: 'new',
  // The modal is retired — feedback now lives in the inbox panel. The setter
  // keeps its signature (Topbar / Ctrl+K / tests) and re-routes: compose for
  // a new report, the feed for 'mine' (threads live there now).
  setFeedbackOpen: (v, view) => {
    set({ feedbackOpen: false, feedbackView: view ?? 'new' });
    if (v) {
      window.dispatchEvent(new CustomEvent('solu:open-inbox', {
        detail: { view: view === 'mine' ? 'feed' : 'compose' },
      }));
    }
  },
  supportBundleOpen: false,
  setSupportBundleOpen: (v) => set({ supportBundleOpen: v }),
  agentsPanelOpen: false,
  setAgentsPanelOpen: (v) => set({ agentsPanelOpen: v }),
  systemPageOpen: false,
  setSystemPageOpen: (v) => set({ systemPageOpen: v }),
  phoneAccessOpen: false,
  setPhoneAccessOpen: (v) => set({ phoneAccessOpen: v }),
  versionPopoverOpen: false,
  versionPopoverSource: 'statusbar',
  // Opening via the generic setter defaults the anchor to the bottom-right
  // StatusBar chip (WhatsNewToast, the banner, the chip itself all use this);
  // the top-bar pill calls openVersionPopoverFrom('topbar') to anchor up top.
  setVersionPopoverOpen: (v) =>
    set(v ? { versionPopoverOpen: true, versionPopoverSource: 'statusbar' } : { versionPopoverOpen: false }),
  openVersionPopoverFrom: (source) => set({ versionPopoverOpen: true, versionPopoverSource: source }),
  daemonOnline: true, // optimistic; flipped by the poll tick below

  openBrowserHere: (url, anchorPaneId, direction, service) => {
    // Single-instance: the internal browser exists to peek at a localhost
    // service, not as a primary surface, so we never spawn a second tile. If
    // one is already open, REUSE it — navigate it to the new URL and focus it
    // — so e.g. clicking a different detected port drives the one browser
    // instead of stacking tiles. (The anchor/direction args only matter when
    // creating the first one.)
    const openIds = collectBrowserIds(get().layout);
    if (openIds.length > 0) {
      const id = openIds[0];
      set((s) => {
        const b = s.browsers[id];
        return {
          browsers: b ? { ...s.browsers, [id]: { ...b, url, service } } : s.browsers,
          focusedPaneId: id, // select the reused tile (no lastUserFocusId stamp — see below)
          zoomedPaneId: null,
        };
      });
      scheduleAutosave(get);
      return;
    }
    const id = 'browser_' + Math.random().toString(36).slice(2, 10);
    const info: BrowserInfo = { id, url, service, createdAt: new Date().toISOString() };
    set((s) => {
      const leaf: Layout = { type: 'browser', browserId: id };
      let nextLayout: Layout;
      if (!s.layout) {
        nextLayout = leaf;
      } else if (anchorPaneId) {
        // Explicit anchor: split at that leaf (preserves legacy API for
        // any caller that wants a specific adjacency).
        nextLayout = insertLeaf(s.layout, leaf, direction ?? 'horizontal', anchorPaneId);
      } else {
        // Default (URL-click from PortPanel, open-browser messages):
        // append as a full-height right column so the browser lives
        // alongside all existing panes without halving a specific one.
        // 0.6/0.4 keeps terminals usable while still giving the browser
        // enough width for most sites.
        nextLayout = {
          type: 'split',
          direction: 'horizontal',
          ratio: 0.6,
          a: s.layout,
          b: leaf,
        };
      }
      return {
        browsers: { ...s.browsers, [id]: info },
        layout: nextLayout,
        focusedPaneId: id, // select the new tile (works with any leaf id); background
        // activity — lastUserFocusId is NOT stamped, so the focus effect won't yank
        // the caret if this fires while the user is mid-typing elsewhere.
        // Clear any active zoom — otherwise the new tile is focused but
        // hidden behind the zoomed pane, so input goes to an invisible
        // tile. (setFocus repoints zoom; these direct-focus paths don't.)
        zoomedPaneId: null,
      };
    });
    scheduleAutosave(get);
  },
  setBrowserUrl: (id, url) => {
    set((s) => {
      const b = s.browsers[id];
      if (!b) return {};
      let service = b.service;
      try { if (service && new URL(service.url).origin !== new URL(url).origin) service = undefined; } catch { service = undefined; }
      return { browsers: { ...s.browsers, [id]: { ...b, url, service } } };
    });
    scheduleAutosave(get);
  },
  setBrowserTitle: (id, title) => {
    set((s) => {
      const b = s.browsers[id];
      if (!b || b.title === title) return {};
      return { browsers: { ...s.browsers, [id]: { ...b, title } } };
    });
  },
  closeBrowserInLayout: (id) => {
    set((s) => {
      const { [id]: _removed, ...rest } = s.browsers;
      const layout = removePane(s.layout, id);
      // If focus was on the closed browser, advance it to ANY
      // surviving leaf (prefer a terminal pane, fall back to any
      // browser/file). Leaving a stale focus makes the next
      // openBrowserHere/openFileHere silently no-op because
      // insertLeaf can't find the (now-deleted) target in the tree.
      const remainingIds = layout
        ? [
            ...collectPaneIds(layout),
            ...collectBrowserIds(layout),
            ...collectFileIds(layout),
            ...collectDiffIds(layout),
            ...collectTasksIds(layout),
          ]
        : [];
      const focusedPaneId =
        s.focusedPaneId === id ? remainingIds[0] ?? null : s.focusedPaneId;
      return {
        browsers: rest,
        layout,
        focusedPaneId,
        zoomedPaneId: s.zoomedPaneId === id ? null : s.zoomedPaneId,
      };
    });
    scheduleAutosave(get);
  },
  toggleBrowser: (url) => {
    const openIds = collectBrowserIds(get().layout);
    if (openIds.length > 0) {
      // Already open → Alt+B / button closes it (there's only ever one).
      get().closeBrowser();
    } else {
      get().openBrowserHere(url ?? defaultBrowserUrl(get().ports));
    }
  },
  closeBrowser: () => {
    const openIds = collectBrowserIds(get().layout);
    if (openIds.length > 0) get().closeBrowserInLayout(openIds[0]);
  },

  files: {},
  recentFilePaths: (() => {
    try { return JSON.parse(localStorage.getItem('solu.recentFiles') || '[]'); }
    catch { return []; }
  })(),
  openFileHere: (path, anchorPaneId, direction = 'horizontal') => {
    const id = 'file_' + Math.random().toString(36).slice(2, 10);
    const info: FileTileInfo = { id, path, createdAt: new Date().toISOString() };
    set((s) => {
      const target = anchorPaneId ?? s.focusedPaneId ?? null;
      const leaf: Layout = { type: 'file', fileId: id };
      const nextLayout = s.layout
        ? insertLeaf(s.layout, leaf, target ? direction : null, target)
        : leaf;
      // Maintain LRU-ish recent list (max 15).
      const recent = [path, ...s.recentFilePaths.filter((p) => p !== path)].slice(0, 15);
      try { localStorage.setItem('solu.recentFiles', JSON.stringify(recent)); } catch { /* ignore */ }
      return {
        files: { ...s.files, [id]: info },
        layout: nextLayout,
        focusedPaneId: id, // background activity (MCP preview_file) — lastUserFocusId
        // not stamped, so the focus effect keeps the user's caret put mid-typing.
        recentFilePaths: recent,
        // Clear zoom so the new file tile isn't focused-but-hidden behind
        // the zoomed pane (input to an invisible tile).
        zoomedPaneId: null,
      };
    });
    scheduleAutosave(get);
  },
  setFilePath: (id, path) => {
    set((s) => {
      const f = s.files[id];
      if (!f) return {};
      const recent = [path, ...s.recentFilePaths.filter((p) => p !== path)].slice(0, 15);
      try { localStorage.setItem('solu.recentFiles', JSON.stringify(recent)); } catch { /* ignore */ }
      return {
        files: { ...s.files, [id]: { ...f, path } },
        recentFilePaths: recent,
      };
    });
    scheduleAutosave(get);
  },
  closeFileInLayout: (id) => {
    set((s) => {
      const { [id]: _removed, ...rest } = s.files;
      const layout = removePane(s.layout, id);
      // Same stale-focus guard as closeBrowserInLayout — otherwise
      // the next open*Here silently no-ops against a dead target.
      const remainingIds = layout
        ? [
            ...collectPaneIds(layout),
            ...collectBrowserIds(layout),
            ...collectFileIds(layout),
            ...collectDiffIds(layout),
            ...collectTasksIds(layout),
          ]
        : [];
      const focusedPaneId =
        s.focusedPaneId === id ? remainingIds[0] ?? null : s.focusedPaneId;
      return {
        files: rest,
        layout,
        focusedPaneId,
        zoomedPaneId: s.zoomedPaneId === id ? null : s.zoomedPaneId,
      };
    });
    scheduleAutosave(get);
  },

  openDiffHere: (path, staged = false, anchorPaneId, direction = 'horizontal') => {
    const id = 'diff_' + Math.random().toString(36).slice(2, 10);
    set((s) => {
      const target = anchorPaneId ?? s.focusedPaneId ?? null;
      // Self-contained leaf: repo path + staged flag travel in the layout.
      const leaf: Layout = { type: 'diff', diffId: id, path, staged };
      const nextLayout = s.layout
        ? insertLeaf(s.layout, leaf, target ? direction : null, target)
        : leaf;
      return {
        layout: nextLayout,
        focusedPaneId: id,
        // Clear zoom so the new tile isn't focused-but-hidden behind a
        // zoomed pane (same guard as openFileHere/openBrowserHere).
        zoomedPaneId: null,
      };
    });
    scheduleAutosave(get);
  },
  setDiffStaged: (diffId, staged) => {
    set((s) => {
      const layout = updateDiffLeaf(s.layout, diffId, { staged });
      return layout === s.layout ? {} : { layout };
    });
    scheduleAutosave(get);
  },
  openTurnDiff: (paneId, n) => {
    const s = get();
    const pane = s.panes[paneId];
    const turnN = n ?? pane?.turn?.last?.n;
    if (!pane || !turnN) return;
    const path = pane.turn?.last?.repo || pane.liveCwd || pane.cwd;
    const existing = collectLeaves(s.layout).find(
      (l) => l.type === 'diff' && l.turn?.paneId === paneId,
    );
    if (existing && existing.type === 'diff') {
      set((st) => ({
        layout: updateDiffLeaf(st.layout, existing.diffId, { turn: { paneId, n: turnN }, path }),
        focusedPaneId: existing.diffId,
        zoomedPaneId: null,
      }));
      scheduleAutosave(get);
      return;
    }
    const id = 'diff_' + Math.random().toString(36).slice(2, 10);
    set((st) => {
      const leaf: Layout = { type: 'diff', diffId: id, path, staged: false, turn: { paneId, n: turnN } };
      const nextLayout = st.layout ? insertLeaf(st.layout, leaf, 'horizontal', paneId) : leaf;
      return { layout: nextLayout, focusedPaneId: id, zoomedPaneId: null };
    });
    scheduleAutosave(get);
  },
  setDiffTurn: (diffId, turn) => {
    set((s) => {
      const layout = updateDiffLeaf(s.layout, diffId, { turn: turn ?? undefined });
      return layout === s.layout ? {} : { layout };
    });
    scheduleAutosave(get);
  },
  closeDiffInLayout: (id) => {
    set((s) => {
      const layout = removePane(s.layout, id);
      // Same stale-focus guard as closeFileInLayout — advance focus to any
      // surviving leaf so the next open*Here doesn't no-op against a dead id.
      const remainingIds = layout
        ? [
            ...collectPaneIds(layout),
            ...collectBrowserIds(layout),
            ...collectFileIds(layout),
            ...collectDiffIds(layout),
            ...collectTasksIds(layout),
          ]
        : [];
      const focusedPaneId =
        s.focusedPaneId === id ? remainingIds[0] ?? null : s.focusedPaneId;
      return {
        layout,
        focusedPaneId,
        zoomedPaneId: s.zoomedPaneId === id ? null : s.zoomedPaneId,
      };
    });
    scheduleAutosave(get);
  },

  managedOwnerBindings: new Map(),
  detachOwnerPresentation: (bindings,scope) => {
    if(scope.key !== currentRoutingScope().key)return;
    set(state=>{
      const confirmedBindings=new Map([...state.managedOwnerBindings,...bindings]);
      if(!confirmedBindings.size)return {};
      const layout=detachManagedOwnerPresentation(state.layout,confirmedBindings);
      const workspaces=state.workspaces.map(workspace=>{
        if(isRouterMode() && workspace.backend !== state.activeBackend)return workspace;
        const next=detachManagedOwnerPresentation(workspace.layout,confirmedBindings);
        const paneIds=(workspace.paneIds ?? []).filter(id=>!confirmedBindings.has(id));
        const panes=workspace.panes?.filter(pane=>!confirmedBindings.has(pane.id));
        if(next === workspace.layout && paneIds.length === (workspace.paneIds?.length ?? 0) && panes?.length === workspace.panes?.length)return workspace;
        return {...workspace,layout:next,paneIds,panes};
      });
      const focus=state.focusedPaneId === null ? null : layoutHasLeaf(layout,state.focusedPaneId) ? state.focusedPaneId : collectLeaves(layout).map(leafId).find(Boolean) ?? null;
      return {managedOwnerBindings:confirmedBindings,layout,workspaces,focusedPaneId:focus,lastUserFocusId:state.lastUserFocusId === null ? null : layoutHasLeaf(layout,state.lastUserFocusId) ? state.lastUserFocusId : focus,zoomedPaneId:state.zoomedPaneId && layoutHasLeaf(layout,state.zoomedPaneId) ? state.zoomedPaneId : null};
    });
    // Migration-only presentation: the authoritative daemon commits sanitation.
    // No client owner-layout autosave, process control, or implicit view selection.
  },
  returnToWorkspace: () => {
    const state = get(); const context = state.ownerReturnContext;
    const restoredGeneration=state.navigationGeneration+1;
    const valid = context?.scope.key === currentRoutingScope().key && context.workspace === state.activeWorkspace;
    const focus = valid && context.focusedPaneId && layoutHasLeaf(state.layout, context.focusedPaneId)
      ? context.focusedPaneId : state.focusedPaneId && layoutHasLeaf(state.layout,state.focusedPaneId) ? state.focusedPaneId : collectLeaves(state.layout).map(leafId).find(Boolean) ?? null;
    set({ mainPresentation: { kind: 'workspace' }, ownerReturnContext: null,
      navigationGeneration: restoredGeneration,
      ...(valid ? { focusedPaneId: focus, lastUserFocusId: context.lastUserFocusId,
        zoomedPaneId: context.zoomedPaneId && layoutHasLeaf(state.layout, context.zoomedPaneId) ? context.zoomedPaneId : null } : {}) });
    if (valid && context.element?.isConnected) requestAnimationFrame(() => requestAnimationFrame(() => {
      if(get().navigationGeneration !== restoredGeneration || get().activeWorkspace !== context.workspace || context.scope.key !== currentRoutingScope().key || get().mainPresentation.kind !== 'workspace')return;
      if(isMobileViewport() && context.element?.matches('textarea,input,[contenteditable="true"]'))return;
      context.element?.focus({preventScroll:true});
    }));
  },
  recentDirs: (() => {
    try { return JSON.parse(localStorage.getItem('solu.recentDirs') || '[]'); }
    catch { return []; }
  })(),
  filesAnchor: (() => {
    try { return localStorage.getItem('solu.filesAnchor') || null; }
    catch { return null; }
  })(),
  pushRecentDir: (path) => {
    set((s) => {
      const next = [path, ...s.recentDirs.filter((p) => p !== path)].slice(0, 10);
      try { localStorage.setItem('solu.recentDirs', JSON.stringify(next)); } catch { /* ignore */ }
      return { recentDirs: next };
    });
  },
  setFilesAnchor: (path) => {
    set({ filesAnchor: path });
    try {
      if (path) localStorage.setItem('solu.filesAnchor', path);
      else localStorage.removeItem('solu.filesAnchor');
    } catch { /* ignore */ }
  },

  ...createToastDialogSlice(set, get, storeApi),
  voiceAlertMode: getVoiceMode(),
  setVoiceAlertMode: (m) => {
    setVoiceMode(m);
    set({ voiceAlertMode: m });
  },
  ...(Object.assign(
    {},
    ...Object.keys(featureSlices).sort().map((path) => featureSlices[path].default(set, get, storeApi)),
  ) as FeatureSlices),

  wsOrder: wsOrderDefault,
  setWsOrder: (order) => {
    try { localStorage.setItem(WS_ORDER_KEY, JSON.stringify(order)); } catch {}
    set({ wsOrder: order });
  },

  wsLastViewedMs: wsLastViewedDefault,
  markWorkspaceViewed: (name) => {
    set((s) => {
      const next = { ...s.wsLastViewedMs, [name]: Date.now() };
      try { localStorage.setItem(WS_VIEWED_KEY, JSON.stringify(next)); } catch {}
      return { wsLastViewedMs: next };
    });
  },

  terminalThemeName: themeDefault,
  terminalThemeExplicit: themeExplicitDefault,
  setTerminalThemeName: (name) => {
    // Validate — fall back to default if an unknown theme was passed.
    const valid = findTheme(name).name;
    try { localStorage.setItem(THEME_KEY, valid); } catch {}
    set({ terminalThemeName: valid, terminalThemeExplicit: true });
  },
  setTerminalThemeAuto: () => {
    try { localStorage.removeItem(THEME_KEY); } catch {}
    set({ terminalThemeName: DEFAULT_THEME_NAME, terminalThemeExplicit: false });
  },

  appTheme: appThemeDefault,
  setAppTheme: (t) => {
    const v: AppThemePref = t === 'light' || t === 'system' ? t : 'dark';
    if (!isEmbedded()) try { localStorage.setItem(APP_THEME_KEY, v); } catch {}
    set({ appTheme: v });
  },
  appThemeLight: appThemeLightDefault,
  setAppThemeLight: (v) => set({ appThemeLight: v }),

  terminalFontSize: fontSizeDefault,
  setTerminalFontSize: (px) => {
    const v = clampFontSize(px);
    try { localStorage.setItem(FONT_SIZE_KEY, String(v)); } catch {}
    set({ terminalFontSize: v });
  },
  terminalLineHeight: lineHeightDefault,
  setTerminalLineHeight: (lh) => {
    const v = clampLineHeight(lh);
    try { localStorage.setItem(LINE_HEIGHT_KEY, String(v)); } catch {}
    set({ terminalLineHeight: v });
  },
  focusFontOffset: focusFontOffsetDefault,
  setFocusFontOffset: (px) => {
    const v = clampFocusOffset(px);
    try { localStorage.setItem(FOCUS_FONT_OFFSET_KEY, String(v)); } catch {}
    set({ focusFontOffset: v });
  },
  uiScale: uiScaleDefault,
  setUiScale: (n) => {
    const v = clampUiScale(n);
    try { localStorage.setItem(UI_SCALE_KEY, String(v)); } catch {}
    set({ uiScale: v });
  },

  paneLayoutMode: paneLayoutModeDefault,
  setPaneLayoutMode: (m) => {
    try { localStorage.setItem(PANE_LAYOUT_MODE_KEY, m); } catch {}
    set({ paneLayoutMode: m });
  },

  nativeMetaBarFields: nativeMetaBarDefault,
  setNativeMetaBarField: (field, on) => set((s) => {
    const next = { ...s.nativeMetaBarFields, [field]: on };
    try { localStorage.setItem(NATIVE_METABAR_KEY, JSON.stringify(next)); } catch {}
    return { nativeMetaBarFields: next };
  }),

  copyOnSelect: copyOnSelectDefault,
  setCopyOnSelect: (v) => {
    try { localStorage.setItem(COPY_ON_SELECT_KEY, v ? '1' : '0'); } catch {}
    set({ copyOnSelect: v });
  },
  middleClickPaste: middleClickPasteDefault,
  setMiddleClickPaste: (v) => {
    try { localStorage.setItem(MIDDLE_CLICK_PASTE_KEY, v ? '1' : '0'); } catch {}
    set({ middleClickPaste: v });
  },
  showPromptHistory: showPromptHistoryDefault,
  setShowPromptHistory: (v) => {
    try { localStorage.setItem(SHOW_PROMPT_HISTORY_KEY, v ? '1' : '0'); } catch {}
    set({ showPromptHistory: v });
  },
  applyLayoutPreset: (preset) => {
    set((s) => {
      const next = buildPresetLayout(s.layout, preset);
      if (!next || next === s.layout) return s;
      return { layout: next };
    });
    scheduleAutosave(get);
  },
  applyGridLayout: async (rows, cols) => {
    const total = rows * cols;
    if (total <= 0) return;
    const s0 = get();
    const origin = autosaveSnapshotFromState(s0);
    const existingLeaves = collectLeaves(s0.layout);
    if (total < existingLeaves.length) return; // picker shouldn't allow this
    const delta = total - existingLeaves.length;

    // Resolve a cwd for the new shell panes. Mirrors launchCmdHere:
    // prefer the focused pane's live cwd, fall back to its stored cwd,
    // empty string lets the daemon pick its default ($HOME).
    let cwd = '';
    const focusedId = s0.focusedPaneId;
    if (focusedId) {
      cwd = s0.panes[focusedId]?.cwd ?? '';
      try {
        const live = await api.paneLiveCwd(focusedId);
        if (live) cwd = live;
      } catch { /* keep stored cwd */ }
    }

    // Spawn new panes sequentially. Sequential rather than Promise.all
    // because the daemon's createPane path touches shared state (port
    // registry, pane map) and parallel bursts of up to 25 spawns aren't
    // worth the race risk. On failure we stop early and reflow whatever
    // we got — buildNxMFromLeaves tolerates a partial fill.
    const newPanes: PaneInfo[] = [];
    for (let i = 0; i < delta; i++) {
      try {
        const p = await api.createPane({
          cwd,
          cmd: [],
          title: 'shell',
          ...paneWorkspaceClaim(s0, 'horizontal', s0.focusedPaneId),
        });
        newPanes.push(p);
      } catch (e) {
        // eslint-disable-next-line no-console
        console.error('[applyGridLayout] createPane failed', e);
        // Without this the picker just stops spinning with no new panes,
        // which reads as "nothing happened" rather than a failed spawn.
        const msg = (e as Error)?.message || String(e);
        get().toast(
          newPanes.length > 0
            ? `Opened ${newPanes.length} of ${delta} panes — couldn't open the rest: ${msg}`
            : `Couldn't open new panes: ${msg}`,
          { kind: 'error', icon: '⚠', ttlMs: 8000 },
        );
        break;
      }
    }

    if (origin && get().activeWorkspace !== origin.name) {
      const allLeaves: Layout[] = [
        ...collectLeaves(origin.layout),
        ...newPanes.map((p) => ({ type: 'pane' as const, paneId: p.id })),
      ];
      const next = buildNxMFromLeaves(allLeaves, rows, cols);
      if (next) {
        await saveWorkspaceSnapshot(get, {
          ...origin,
          layout: next,
          panes: newPanes.reduce(
            (acc, p) => ({ ...acc, [p.id]: p }),
            origin.panes,
          ),
        });
      }
      return;
    }

    set((st) => {
      const allLeaves: Layout[] = [
        ...collectLeaves(st.layout),
        ...newPanes.map((p) => ({ type: 'pane' as const, paneId: p.id })),
      ];
      const next = buildNxMFromLeaves(allLeaves, rows, cols);
      if (!next) return st;
      const panesUpdate = { ...st.panes };
      for (const p of newPanes) panesUpdate[p.id] = p;
      // Force auto so cells render equal-area; without it a fresh grid
      // inherits stored ratios from prior splits and looks lopsided.
      try { localStorage.setItem(PANE_LAYOUT_MODE_KEY, 'auto'); } catch {}
      return {
        panes: panesUpdate,
        layout: next,
        paneLayoutMode: 'auto',
        focusedPaneId: newPanes.length > 0 ? newPanes[0].id : st.focusedPaneId,
        lastViewedMs: newPanes.reduce(
          (acc, p) => ({ ...acc, [p.id]: Date.now() }),
          st.lastViewedMs,
        ),
      };
    });
    scheduleAutosave(get);
    await flushAutosave(get, undefined, { throwOnError: true });
  },

  customKeybindings: keybindingsDefault,
  terminalKeysFirst: (() => {
    try { return localStorage.getItem('solu.terminalKeysFirst') === '1'; } catch { return false; }
  })(),
  setTerminalKeysFirst: (on) => {
    try { localStorage.setItem('solu.terminalKeysFirst', on ? '1' : '0'); } catch { /* ignore */ }
    set({ terminalKeysFirst: on });
  },
  setKeybinding: (actionId, combo) => {
    set((s) => {
      const next = { ...s.customKeybindings, [actionId]: combo };
      try { localStorage.setItem(KEYBINDINGS_KEY, JSON.stringify(next)); } catch {}
      return { customKeybindings: next };
    });
  },
  clearKeybinding: (actionId) => {
    set((s) => {
      if (!(actionId in s.customKeybindings)) return s;
      const next = { ...s.customKeybindings };
      delete next[actionId];
      try { localStorage.setItem(KEYBINDINGS_KEY, JSON.stringify(next)); } catch {}
      return { customKeybindings: next };
    });
  },
  resetAllKeybindings: () => {
    try { localStorage.removeItem(KEYBINDINGS_KEY); } catch {}
    set({ customKeybindings: {} });
  },

  activityLog: [],
  pushActivity: (ev) => {
    set((s) => {
      const next = [...s.activityLog, ev];
      // Trim from the front when we exceed the cap. The log is
      // newest-last so slicing from the tail keeps the latest events.
      if (next.length > ACTIVITY_LOG_MAX) {
        return { activityLog: next.slice(next.length - ACTIVITY_LOG_MAX) };
      }
      return { activityLog: next };
    });
  },
  bootstrapActivity: (events) => {
    // Replace rather than merge — the /api/activity snapshot is the
    // authoritative recent history. Trim to the client cap so a bursty
    // daemon ring doesn't exceed our own limit on the boot path.
    const trimmed = events.length > ACTIVITY_LOG_MAX
      ? events.slice(events.length - ACTIVITY_LOG_MAX)
      : events;
    set({ activityLog: trimmed });
  },
  clearActivity: () => set({ activityLog: [] }),

  paneTails: {},
  ariaDismissedFingerprints: {},
  upsertPaneTail: (tail) => {
    set((s) => {
      // Prune stale dismissals so the map stays bounded. Cheap to do
      // on every upsert; the map is bounded by pane count and TTL.
      const now = Date.now();
      const dismissals: Record<string, number> = {};
      for (const [k, v] of Object.entries(s.ariaDismissedFingerprints)) {
        if (now - v < ARIA_DISMISSAL_TTL_MS) dismissals[k] = v;
      }
      // Skip the write if the existing tail is the same text — avoids
      // a re-render on every poll cycle when nothing meaningful changed.
      const existing = s.paneTails[tail.paneId];
      if (existing && existing.text === tail.text && existing.toState === tail.toState) {
        // Refresh ts so "X ago" ticks but don't trip useSyncExternalStore
        // (object identity changes only when text/state changed).
        return { ariaDismissedFingerprints: dismissals };
      }
      return {
        paneTails: { ...s.paneTails, [tail.paneId]: tail },
        ariaDismissedFingerprints: dismissals,
      };
    });
  },
  dismissPaneAttention: (paneId) => {
    set((s) => {
      const tail = s.paneTails[paneId];
      if (!tail) return {};
      // Add the current question's fingerprint to dismissals. We do
      // NOT remove the tail — the bell needs it to keep matching
      // dismissals after this. The tail will naturally get replaced
      // when the daemon emits a new observation for the pane.
      const key = `${paneId}::${ariaObservationFingerprint(tail.text)}`;
      return {
        ariaDismissedFingerprints: {
          ...s.ariaDismissedFingerprints,
          [key]: Date.now(),
        },
      };
    });
  },
  clearAllPaneAttention: () => set((s) => {
    const now = Date.now();
    const dismissals = { ...s.ariaDismissedFingerprints };
    for (const tail of Object.values(s.paneTails)) {
      dismissals[`${tail.paneId}::${ariaObservationFingerprint(tail.text)}`] = now;
    }
    return { ariaDismissedFingerprints: dismissals };
  }),

  ariaPaused: false,
  ariaAutoFiresHr: 0,
  ariaAutoPending: [],
  setAriaPaused: (paused) => set({ ariaPaused: paused }),
  setAriaStatusSnapshot: (snap) => set({
    ariaPaused: snap.paused,
    ariaAutoFiresHr: snap.auto_fires_hr ?? 0,
  }),
  upsertAriaAutoPending: (p) => {
    set((s) => {
      const existing = s.ariaAutoPending.find((x) => x.confirmId === p.confirmId);
      if (existing) {
        // Merge — preserves startedAt so the countdown doesn't
        // restart on re-emits.
        return {
          ariaAutoPending: s.ariaAutoPending.map((x) => x.confirmId === p.confirmId ? { ...x, ...p, startedAt: x.startedAt } : x),
        };
      }
      const next = [p, ...s.ariaAutoPending];
      const pending = next.filter((x) => x.status === 'pending' || x.status === 'firing');
      const resolved = next.filter((x) => x.status !== 'pending' && x.status !== 'firing').slice(0, ARIA_AUTO_PENDING_MAX);
      return { ariaAutoPending: [...pending, ...resolved] };
    });
  },
  resolveAriaAutoPending: (confirmId, outcome) => {
    set((s) => ({
      ariaAutoPending: s.ariaAutoPending.map((x) => {
        if (x.confirmId !== confirmId) return x;
        let status: AriaAutoPending['status'] = 'sent';
        if (outcome.timeout) status = 'timeout';
        else if (outcome.error) status = 'failed';
        else if (outcome.cancelled) status = 'cancelled';
        else if (!outcome.approved) status = 'cancelled';
        return { ...x, status, error: outcome.error };
      }),
    }));
    // Auto-hide resolved cards after 5s so the panel doesn't keep
    // piling them up; they're already in the activityLog for history.
    setTimeout(() => {
      useStore.getState().removeAriaAutoPending(confirmId);
    }, 5000);
  },
  removeAriaAutoPending: (confirmId) => {
    set((s) => ({ ariaAutoPending: s.ariaAutoPending.filter((x) => x.confirmId !== confirmId) }));
  },
  clearChoicePromptOptimistic: (paneId) => {
    set((s) => {
      const pane = s.panes[paneId];
      if (!pane || pane.state !== 'choice-prompt') return {};
      // Flip to 'busy' — the auto-approved input was just submitted, so
      // the agent is working again. The 3s refreshPanes poll reconciles
      // to whatever the daemon actually reports.
      return { panes: { ...s.panes, [paneId]: { ...pane, state: 'busy' } } };
    });
  },

  applyPaneStateFromSSE: (paneId, state, agentMidTurn) => {
    set((s) => {
      const pane = s.panes[paneId];
      if (!pane) return {};
      // agentMidTurn (the native working-flag) now rides this SSE so the spinner
      // flips with the transition instead of lagging the 3s poll. Apply if EITHER
      // the coarse state or the working-flag changed; else no-op so unrelated pane
      // subscribers don't re-render.
      const amtChanged = agentMidTurn !== undefined && pane.agentMidTurn !== agentMidTurn;
      if (pane.state === state && !amtChanged) return {};
      const next = { ...pane, state };
      if (amtChanged) next.agentMidTurn = agentMidTurn;
      return { panes: { ...s.panes, [paneId]: next } };
    });
  },

  applyPaneAttentionFromSSE: (paneId, attention, reason) => {
    set((s) => {
      const pane = s.panes[paneId];
      if (!pane) return {};
      const nextReason = reason || undefined;
      if (pane.attention === attention && pane.attentionReason === nextReason) return {};
      return { panes: { ...s.panes, [paneId]: { ...pane, attention, attentionReason: nextReason } } };
    });
  },

  zoomedPaneId: null,
  isDraggingPane: false,
  setDraggingPane: (v) => set({ isDraggingPane: v }),
  toggleZoom: (paneId) => {
    const s = get();
    if (s.zoomedPaneId) {
      set({ zoomedPaneId: null });
      return;
    }
    // Any layout leaf — pane, file, browser, diff, or tasks — can zoom. The
    // grid already renders all leaf types through its zoomed single-leaf path.
    if (collectLeaves(s.layout).length < 2) return;
    const target = paneId ?? s.focusedPaneId;
    if (!target || !layoutHasLeaf(s.layout, target)) return;
    set({ zoomedPaneId: target, focusedPaneId: target, lastUserFocusId: target });
  },

  cloneFocused: async (direction = 'horizontal') => {
    const s = get();
    const target = s.focusedPaneId;
    if (!target) return;
    const src = s.panes[target];
    if (!src) return;
    // Use the live cwd (where the shell actually is) so clones track
    // cd'd directories instead of the original cwd the pane was created in.
    let cwd = src.cwd;
    try {
      const live = await api.paneLiveCwd(target);
      if (live) cwd = live;
    } catch { /* ignore — fall back to stored cwd */ }
    await get().splitFocused(direction, cloneSpawnOpts(src, cwd));
  },

  markActivity: (paneId) => {
    const now = Date.now();
    const last = paneActivityLastSet.get(paneId) ?? 0;
    if (now - last < PANE_ACTIVITY_MIN_INTERVAL_MS) {
      paneActivityPending.set(paneId, now);
      if (!paneActivityTimers.has(paneId)) {
        const delay = Math.max(0, PANE_ACTIVITY_MIN_INTERVAL_MS - (now - last));
        const h = window.setTimeout(() => {
          paneActivityTimers.delete(paneId);
          const pending = paneActivityPending.get(paneId);
          paneActivityPending.delete(paneId);
          if (pending == null) return;
          paneActivityLastSet.set(paneId, pending);
          set((s) => ({
            clientActivityMs: { ...s.clientActivityMs, [paneId]: pending },
          }));
        }, delay);
        paneActivityTimers.set(paneId, h);
      }
      return;
    }
    paneActivityLastSet.set(paneId, now);
    set((s) => ({
      clientActivityMs: { ...s.clientActivityMs, [paneId]: now },
    }));
  },

  setPaneLastPrompts: (paneId, prompts, scope = currentRoutingScope()) => {
    if (scope.key !== currentRoutingScope().key || !routingPrincipalAllowed()) return;
    const key = terminalSessionKey(scope, paneId);
    set((s) => {
      if (promptsEqual(s.lastPrompts[key], prompts)) return {};
      return { lastPrompts: { ...s.lastPrompts, [key]: prompts } };
    });
  },

  registerSender: (id, fn) => {
    set((s) => ({ senders: { ...s.senders, [id]: fn } }));
  },
  unregisterSender: (id, expected) => {
    set((s) => {
      if (expected && s.senders[id] !== expected) return {};
      const { [id]: _removed, ...rest } = s.senders;
      return { senders: rest };
    });
  },

  registerClaimer: (id, fn) => {
    set((s) => ({ claimers: { ...s.claimers, [id]: fn } }));
  },
  unregisterClaimer: (id, expected) => {
    set((s) => {
      if (expected && s.claimers[id] !== expected) return {};
      const { [id]: _removed, ...rest } = s.claimers;
      return { claimers: rest };
    });
  },
  claimAllPanes: () => {
    // Set claim intent first — from here on, freshly-opened WSes
    // on this browser will auto-claim their panes.
    set({ claimIntent: true });
    const s = get();
    // Only claim panes in the CURRENT layout. With warm-pane keep-alive,
    // parked panes from other workspaces keep registered claimers; firing
    // those would silently steal ownership of a background pane (e.g. one
    // the phone is actively using) — the documented silent-reclaim
    // anti-pattern. The "Take over" button must only affect what's
    // on screen. Parked panes re-claim lazily on re-adoption via
    // claimIntent if the user actually returns to them.
    const visible = new Set(effectiveVisiblePaneIds(s).map(id => terminalSessionKey(currentRoutingScope(), id)));
    for (const [id, fn] of Object.entries(s.claimers)) {
      if (!visible.has(id)) continue;
      try { fn(); } catch { /* ignore — best effort */ }
    }
  },

  registerRedrawer: (id, fn) => {
    set((s) => ({ redrawers: { ...s.redrawers, [id]: fn } }));
  },
  unregisterRedrawer: (id, expected) => {
    set((s) => {
      if (expected && s.redrawers[id] !== expected) return {};
      const { [id]: _removed, ...rest } = s.redrawers;
      return { redrawers: rest };
    });
  },
  redrawPane: (id) => {
    const fn = get().redrawers[id];
    if (fn) {
      try { fn(); } catch (e) { console.warn('[solu] redrawer threw:', e); }
    }
  },

  registerRealigner: (id, fn) => {
    set((s) => ({ realigners: { ...s.realigners, [id]: fn } }));
  },
  unregisterRealigner: (id, expected) => {
    set((s) => {
      if (expected && s.realigners[id] !== expected) return {};
      const { [id]: _removed, ...rest } = s.realigners;
      return { realigners: rest };
    });
  },
  realignPane: (id) => {
    const fn = get().realigners[id];
    if (fn) {
      try { fn(); } catch (e) { console.warn('[solu] realigner threw:', e); }
    }
  },
  realignAllPanes: () => {
    const s = get();
    // Only the on-screen panes. A background/parked view must not resend its
    // width (it would fight the active device for the shared PTY) — mirrors
    // claimAllPanes' visible-only rule. Parked panes realign lazily on
    // re-adoption anyway (their own reattach hook fires a forced pass).
    const visible = new Set(effectiveVisiblePaneIds(s).map(id => terminalSessionKey(currentRoutingScope(), id)));
    for (const [id, fn] of Object.entries(s.realigners)) {
      if (!visible.has(id)) continue;
      try { fn(); } catch { /* best effort */ }
    }
  },

  reloadApp: (targetVersion?: string) => {
    // The version arg only seeds the cache-busting key; reloadForFreshBundle
    // appends Date.now() regardless, so a fresh fetch is guaranteed. Purge any
    // cached shell first — a stale service-worker entry outlives a plain
    // navigation and would boot the same old bundle again.
    void purgeShellCaches().finally(() => reloadForFreshBundle(targetVersion ?? __SOLU_WEB_VERSION__));
  },

  setPaneOwned: (id, owned, ownerState, token) => {
    set((s) => {
      const prev = s.paneOwnership[id];
      const prevState = s.paneOwnerState[id];
      if (prev === owned && prevState === ownerState && s.paneOwnershipTokens[id] === token) return {};
      // Only reset claimIntent on a genuine displacement
      // (owned → not-owned transition). Initial "you just joined
      // this pane, you don't own it" broadcasts have prev=undefined
      // and MUST NOT reset claimIntent, otherwise every pane
      // switch on mobile would re-gate the overlay: onopen would
      // auto-claim, but the server's initial owned=false arrives
      // before the claim is processed and would wipe the intent.
      const displaced = prev === true && owned === false;
      const nextState = { ...s.paneOwnerState };
      if (ownerState) nextState[id] = ownerState;
      else delete nextState[id]; // old daemon: no provenance — legacy copy
      return {
        paneOwnership: { ...s.paneOwnership, [id]: owned },
        paneOwnerState: nextState,
        paneOwnershipTokens: { ...s.paneOwnershipTokens, [id]: token },
        claimIntent: displaced ? false : s.claimIntent,
      };
    });
  },

  setClaimIntent: (v) => set({ claimIntent: v }),
  clearPaneOwnership: (id, expected) => {
    set((s) => {
      if (expected !== undefined && s.paneOwnershipTokens[id] !== expected) return {};
      const { [id]: _removed, ...rest } = s.paneOwnership;
      const { [id]: _removedState, ...restState } = s.paneOwnerState;
      const { [id]: _removedToken, ...restTokens } = s.paneOwnershipTokens;
      return { paneOwnership: rest, paneOwnerState: restState, paneOwnershipTokens: restTokens };
    });
  },
  sendToFocused: (bytes) => {
    const state = get();
    const focusedPaneId = effectiveTerminalTarget(state);
    const senders = state.senders;
    if (!focusedPaneId) return false;
    const send = senders[terminalSessionKey(currentRoutingScope(), focusedPaneId)] ?? senders[focusedPaneId];
    if (!send) return false;
    // Debug log (enable with `localStorage.setItem('solu.debug','1')`).
    // Helps diagnose duplicate-keystroke reports (e.g. keybar digit
    // appearing both as Claude choice-select AND in the next input box).
    if (isSoluDebugEnabled()) {
      const preview = typeof bytes === 'string' ? bytes : new TextDecoder().decode(bytes);
      debugLog('[solu] sendToFocused ->', JSON.stringify(preview), 'pane=', focusedPaneId);
    }
    return send(bytes);
  },

  paneSnapshots: {},
  registerPaneSnapshot: (id, fn) => {
    set((s) => ({ paneSnapshots: { ...s.paneSnapshots, [id]: fn } }));
  },
  unregisterPaneSnapshot: (id, expected) => {
    set((s) => {
      if (expected && s.paneSnapshots[id] !== expected) return {};
      const { [id]: _removed, ...rest } = s.paneSnapshots;
      return { paneSnapshots: rest };
    });
  },
  getFocusedPaneSnapshot: () => {
    const state = get();
    const focusedPaneId = effectiveTerminalTarget(state);
    const paneSnapshots = state.paneSnapshots;
    if (!focusedPaneId) return null;
    const fn = paneSnapshots[terminalSessionKey(currentRoutingScope(), focusedPaneId)] ?? paneSnapshots[focusedPaneId];
    if (!fn) return null;
    try { return fn(); } catch { return null; }
  },

  setNotifyEnabled: async (v) => {
    if (isEmbedded()) {
      set({ notifyEnabled: v });
      try { localStorage.setItem(NOTIFY_KEY, v ? '1' : '0'); } catch {}
      return v;
    }
    const cloud = useCloud.getState();
    const cloudMode = cloud.mode === 'cloud';
    const bearer = cloud.session?.access_token ?? null;

    // Turning off — unsubscribe from the cloud's push if in cloud mode.
    if (!v) {
      set({ notifyEnabled: false });
      try { localStorage.setItem(NOTIFY_KEY, '0'); } catch {}
      if (cloudMode && bearer) {
        try { await unsubscribePushFromCloud(bearer); } catch { /* best effort */ }
      }
      return false;
    }

    // Turning on — acquire permission. Two paths:
    //   - Tauri desktop: WebView2 silently refuses the Web Notification
    //     API (perm starts as 'denied' and requestPermission() is a
    //     no-op), so we route through @tauri-apps/plugin-notification
    //     which prompts Windows' native Action Center permission UI.
    //   - Browser / cloud PWA: use the standard Web Notification API.
    if (isTauri()) {
      try {
        const { isPermissionGranted, requestPermission } = await import('@tauri-apps/plugin-notification');
        let granted = await isPermissionGranted();
        if (!granted) {
          const perm = await requestPermission();
          granted = perm === 'granted';
        }
        if (!granted) return false;
      } catch (e) {
        // Plugin call blew up (IPC not available, invoke rejected).
        // Surface minimal signal; caller's Topbar toggleNotify will
        // explain to the user.
        console.warn('[solu] tauri notification plugin failed', e);
        return false;
      }
    } else {
      if (typeof Notification === 'undefined') {
        return false;
      }
      if (Notification.permission === 'default') {
        const perm = await Notification.requestPermission();
        if (perm !== 'granted') return false;
      } else if (Notification.permission === 'denied') {
        // Can't re-prompt from JS once the user has blocked; they must
        // unblock in browser site settings.
        return false;
      }
    }

    // In cloud mode, actually subscribe to web push so we get
    // notifications on this device even when the app isn't open. Local
    // mode relies on App.tsx's in-page Notification scheduler.
    //
    // Wrapped in try/catch so a browser exception (invalid
    // applicationServerKey, push service unreachable, etc.) surfaces
    // to the user via a toast instead of a silent unhandled promise
    // rejection. Before this, a 32-byte VAPID "public" key (wrong
    // shape — should be 65) caused pushManager.subscribe to throw
    // InvalidAccessError, which propagated up from here into the
    // onClick handler where React just swallowed it — user saw the
    // bell stay muted with no alert and no explanation. Never again.
    if (cloudMode && cloud.vapidPublicKey && bearer && !isTauri()) {
      // Manual bell-on ALWAYS forces a fresh FCM endpoint. An explicit
      // toggle is the user's way of saying "please make notifications
      // work right now" — often because they suspect the current sub
      // is stale or throttled — so honouring it with a reused endpoint
      // would be wrong. `rotatePushSubscription` revokes the current
      // browser sub, POSTs /api/push/unsubscribe for the old endpoint,
      // then subscribes + POSTs the new one. Resets per-endpoint FCM
      // throttle state and guarantees the cloud holds a matching row.
      let ok = false;
      try {
        ok = await rotatePushSubscription(cloud.vapidPublicKey, bearer);
      } catch (e) {
        const msg = (e as Error)?.message || String(e);
        console.error('[solu] push subscribe threw:', e);
        get().toast(
          `Couldn't enable push: ${msg}`,
          { kind: 'error', icon: '⚠', ttlMs: 8000 },
        );
        return false;
      }
      if (!ok) {
        // Permission granted but subscribe returned false (e.g. cloud
        // POST 4xx, VAPID key rejected by the browser without
        // throwing). Surface a generic notice so the bell-stays-muted
        // state isn't a mystery.
        get().toast(
          'Push subscribe didn\'t complete — notifications stay in-app only. Check cloud VAPID config.',
          { kind: 'warn', icon: '⚠', ttlMs: 8000 },
        );
        return false;
      }
    }

    set({ notifyEnabled: true });
    try { localStorage.setItem(NOTIFY_KEY, '1'); } catch {}
    // Banner-setup hint. Fires every time the user deliberately toggles
    // the bell from OFF to ON — that's always a moment of intent where
    // reminding them about the 📤 button is welcome, not naggy. The
    // equivalent toast in App.tsx mount-useEffect is the one-shot
    // fallback for users who were already subscribed before the hint
    // existed; it uses the localStorage flag so it doesn't repeat on
    // every reload. Here we ALSO set the flag to suppress that mount
    // fallback (toggle-on is the strongest signal we can show it).
    // Tauri skipped: OS-native notifications don't have this channel-
    // importance wrinkle.
    // Without an account there is no phone push (or Test button) to mention.
    if (cloudMode && bearer && !isTauri()) {
      get().toast(
        'Notifications on. Want them as banners on your phone? Tap the Test button next to the bell.',
        { kind: 'info', icon: '🔔', ttlMs: 10_000 },
      );
      try { localStorage.setItem('solu.banner-info-toast-seen', '1'); } catch { /* ignore */ }
    }
    return true;
  },

  resubscribePush: async () => {
    // Gate: notifyEnabled OFF → nothing to do. Not in cloud mode → local
    // mode uses in-page Notification(), no web push involved. Missing
    // bearer or vapid → cloud isn't configured or user isn't signed in.
    const s = get();
    if (!s.notifyEnabled) return false;
    const cloud = useCloud.getState();
    if (cloud.mode !== 'cloud') return false;
    const bearer = cloud.session?.access_token ?? null;
    const vapid = cloud.vapidPublicKey;
    if (!bearer || !vapid) return false;

    // Idempotent: subscribePushToCloud re-uses the browser's existing
    // Push subscription when the VAPID key matches, and only re-POSTs
    // the endpoint to the cloud's in-memory store. Safe to call on
    // every bootstrap / SW-activate / machine-switch without cost
    // beyond a single HTTP round-trip.
    //
    // Rotation: if the current endpoint is older than ROTATION_MAX_AGE_MS
    // (see push.ts), force a fresh subscription instead of reusing. This
    // sidesteps Android per-endpoint throttling — Doze, FCM rate caps,
    // Pixel Notification Cooldown — which silently drops pushes to a
    // stale endpoint while webpush-go still reports success. The manual
    // workaround is "toggle bell off+on"; this automates it on every
    // app open past the 4h mark.
    try {
      if (isPushSubStale()) {
        return await rotatePushSubscription(vapid, bearer);
      }
      return await subscribePushToCloud(vapid, bearer);
    } catch {
      return false;
    }
  },

  switchBackend: async (b, workspaceName, opts) => {
    const scope=currentRoutingScope(),generation=get().navigationGeneration;
    const state0 = get();
    const outgoingSnapshot: AutosaveSnapshot | undefined = state0.activeWorkspace
      ? {
          name: state0.activeWorkspace,
          paneMoveVersion: state0.activeWorkspaceMoveVersion,
          layout: state0.layout,
          panes: state0.panes,
          browsers: state0.browsers,
          files: state0.files,
        }
      : undefined;

    // A cross-backend workspace click does not go through loadWorkspace,
    // so it needs the same outgoing autosave flush here. Otherwise a pane
    // split made moments before clicking a workspace on the other backend
    // can be lost when the state is cleared below.
    await flushAutosave(get, outgoingSnapshot, { refreshWorkspaces: false });

    if(scope.key !== currentRoutingScope().key || generation !== get().navigationGeneration)return;
    // Step 1: synchronously clear all per-backend state. flushSync
    // forces React to commit + unmount all Pane components NOW (so
    // their cleanup fires, disposing xterm + WebGL addons cleanly)
    // before we re-render with new panes. Without flushSync, React
    // batches and we could see old + new panes coexist briefly,
    // accumulating WebGL contexts — the leak that forced this whole
    // switchBackend rewrite in the first place.
    flushSync(() => {
      set({
        layout: null,
        panes: {},
        workspaces: [],
        ports: [],
        schedules: [],
        browsers: {},
        files: {},
        activeWorkspace: CURRENT_WS,
        activeWorkspaceMoveVersion: 0,
        focusedPaneId: null,
        zoomedPaneId: null,
        activeBackend: b,
        loading: true,
        // Parked panes belong to the outgoing backend — unmount them
        // too (their WS URLs would route to the wrong daemon).
        parkedPaneIds: [],
      });
    });

    // Step 2: flip api.ts's in-module backendPrefix + update the URL so
    // a future hard-refresh lands on the same backend. replaceState
    // does NOT reload — the page stays alive, our single React tree
    // continues.
    apiSetBackend(b);
    if(!opts?.preserveOwnerLink)clearOwnerURL(false);
    try {
      const url = new URL(window.location.href);
      url.searchParams.set('b', b);
      if (workspaceName) {
        url.searchParams.set('ws', workspaceName);
      } else {
        url.searchParams.delete('ws');
      }
      window.history.replaceState(null, '', url.toString());
    } catch {
      /* ignore URL-update failures */
    }

    // Step 3: re-run bootstrap to fetch the new backend's data.
    // bootstrap reads pendingWorkspace() from the URL, so the `?ws=`
    // param set above lands the user inside the clicked workspace.
    await get().bootstrap();
  },

  bootstrap: async () => {
    const scope = currentRoutingScope();
    const generation = get().navigationGeneration;
    const sequence = ++bootstrapSequence;
    const portsSequence = ++portsRefreshSequence;
    const bootstrapCurrent = () => sequence === bootstrapSequence
      && scope.key === currentRoutingScope().key
      && generation === get().navigationGeneration;
    const startedAt = perfNow();
    set({ loading: true, bootstrapError: null, bootstrapNeedsApproval: false });
    try {
      // In dual-backend router mode, fetch panes / ports / workspaces
      // from BOTH daemons so the sidebar's status aggregation covers
      // the inactive backend too. Schedules stay single-backend for
      // now (less status-driven).
      const [panes, wss, ports] = await Promise.all([
        isRouterMode() ? api.listPanesAllBackends() : api.listPanes(),
        isRouterMode() ? api.listWorkspacesAllBackends() : api.listWorkspaces(),
        isRouterMode() ? api.listPortsAllBackends() : api.listPorts(),
      ]);
      if (!bootstrapCurrent()) return;
      // Update status is non-critical to first paint — fire it off but
      // don't block the bootstrap on it. The SSE bus will refresh it
      // again whenever the daemon's 15-min poll detects a new version.
      void get().refreshUpdateStatus();
      void get().loadRecoverySettings();
      void get().loadNativeSettings();
      void get().loadUpdateSettings();
      void get().loadWardenSettings();
      void get().loadOrchestratorHosts();
      const paneMap: Record<string, PaneInfo> = {};
      for (const p of panes) { if (!p.backend || !get().activeBackend || p.backend === get().activeBackend) paneMap[p.id] = p; }

      // Pick the workspace to land in, in decreasing priority:
      //   1. `?ws=<name>` URL param — set by switchActiveBackend when the
      //      user clicks a cross-backend workspace, so a single click
      //      gets them fully into that workspace after the reload.
      //   2. localStorage "last viewed" — pinned on every
      //      activeWorkspace change (see subscribe at end of file) so a
      //      reload (auto-update, Tauri restart, F5) drops the user
      //      back where they were sitting, not where they most
      //      recently EDITED.
      //   3. Freshest activeAt — the historical fallback; kicks in on
      //      first-ever bootstrap (no localStorage yet) and covers
      //      edge cases like the remembered workspace having been
      //      deleted since last session.
      const requestedWs = pendingWorkspace();
      const activeBackend = isRouterMode() ? activeBackendFromUrl() : null;
      const explicit = requestedWs ? pickWorkspaceByName(wss, requestedWs, activeBackend) : null;
      const remembered = !explicit && activeWorkspaceRemembered
        ? pickWorkspaceByName(wss, activeWorkspaceRemembered, activeBackend)
        : null;
      const sortedByActive = [...wss].sort(
        (a, b) => new Date(b.activeAt).getTime() - new Date(a.activeAt).getTime(),
      );
      // Explicit type: `sortedByActive[0]` is typed `Workspace` (no
      // noUncheckedIndexedAccess), so the inferred type would be `Workspace`
      // and the `= undefined` skip below would be a tsc error. The runtime
      // already handles undefined (preferred?.layout etc.).
      let preferred: Workspace | undefined = explicit ?? remembered ?? sortedByActive[0];

      // Multi-monitor: a freshly-spawned SECONDARY window has no explicit
      // (?ws=) or remembered (per-window key) workspace, so it would
      // otherwise fall to the freshest activeAt — almost always the
      // workspace the PRIMARY window is currently showing, putting two
      // windows on the same panes (one PTY size → width-thrash). Open it
      // to a blank scratch instead; the user then picks a workspace, which
      // goes through loadWorkspace's ownership guard. On RELOAD, remembered
      // is set (its own namespaced key), so this skip does not apply and
      // the window restores its own workspace. The primary keeps today's
      // behaviour untouched.
      if (!explicit && !remembered && !isPrimaryWindow()) {
        preferred = undefined;
      }

      let activeWorkspaceName = CURRENT_WS;
      let layout: Layout | null = null;

      // When the user explicitly asked for a specific workspace (via
      // ?ws=<name>) OR we're restoring their remembered last-viewed
      // workspace, make it active even if its layout is empty — they
      // chose to be there. Subsequent autosaves populate an empty one.
      // First run: the daemon seeds an empty protected "Default". Land in it
      // rather than the unnamed `_current` buffer, or the sidebar lists
      // "Default · 0 panes" while every new pane autosaves somewhere else.
      const firstRunEmpty = !explicit && !remembered && preferred && preferred.name !== CURRENT_WS
        && !preferred.layout && !wss.some((w) => w.name === CURRENT_WS && w.layout)
        ? preferred
        : null;
      const landing = explicit ?? remembered ?? firstRunEmpty;
      if (landing) {
        activeWorkspaceName = landing.name;
      }

      // Build the browser/file id sets FIRST so pruneLayout's alive
      // check can admit them — without this, any browser or file leaf
      // in the saved layout was getting silently pruned because the
      // alive() predicate only looked at panes. That's why a browser
      // tile reliably vanished after a Tauri restart even though its
      // info did round-trip through workspaces.json.
      const savedBrowserIds = new Set<string>((preferred?.browsers ?? []).map((b) => b.id));
      const savedFileIds = new Set<string>((preferred?.files ?? []).map((f) => f.id));
      const isAlive = (id: string) =>
        isPresentPane(paneMap[id]) || savedBrowserIds.has(id) || savedFileIds.has(id);

      if (preferred && preferred.layout) {
        activeWorkspaceName = preferred.name;
        layout = pruneLayout(preferred.layout, isAlive);
        // Respawn dead panes from inline defs (same logic as loadWorkspace).
        if (preferred.layout) {
          const idRemap = new Map<string, string>();
          const referenced = preferred.layout ? collectPaneIds(preferred.layout) : [];
          for (const oldId of referenced) {
            if (!bootstrapCurrent()) return;
            // Present covers suspended-resumable: the daemon parked the pane
            // for an in-place Resume — respawning a twin from the stale def
            // would duplicate (or fork) the conversation. Keep the slot.
            if (isPresentPane(paneMap[oldId])) continue;
            const def = preferred.panes?.find((d) => d.id === oldId);
            if (!def) {
              paneMap[oldId] = failedPaneFromDef({ id: oldId, cwd: "", cmd: [], title: "Saved pane" }, new Error("No launch recipe was saved for this pane."));
              continue;
            }
            if (def.role === 'task_owner' || get().managedOwnerBindings.has(oldId)) continue;
            try {
              const fresh = await api.createPane(paneRestoreRequest(def), scope);
              if (!bootstrapCurrent()) return;
              idRemap.set(oldId, fresh.id);
              paneMap[fresh.id] = fresh;
              lifecycleLog('store.bootstrap.respawn-pane', {
                workspace: preferred.name,
                oldId,
                newId: fresh.id,
                cmd0: def.cmd?.[0] ?? '',
              });
            } catch (error) {
              paneMap[oldId] = failedPaneFromDef(def, error);
            }
          }
          if (idRemap.size > 0) {
            void api.remapPaneOrigins(Object.fromEntries(idRemap), scope).catch(() => {});
          }
          if (preferred.layout) {
            const remapped = remapLayoutIds(preferred.layout, idRemap);
            layout = pruneLayout(remapped, isAlive);
            lifecycleLog('store.bootstrap.final-remap', {
              workspace: preferred.name,
              remap: Array.from(idRemap.entries()),
              finalIds: lifecycleLayoutIds(layout),
            });
          }
        }
      }

      // Stack live orphan panes into the layout when it'd otherwise be
      // blank. Covers two cases:
      //   1. Fresh install: no saved workspaces AND live panes exist
      //      (typically means the user had panes from a prior install
      //      that outlived their workspaces.json).
      //   2. User clicked an EMPTY named workspace (e.g. the auto-
      //      created "Default" on Windows) while live panes sit
      //      behind `_current` — without adoption they'd see a blank
      //      screen and think the click didn't work.
      // Orphans are panes claimed by no named workspace other than
      // the one we're landing in; `_current` doesn't count as a
      // claim. In router mode we only consider orphans on the
      // currently-active backend.
      const landingEmpty = landing;
      const needsOrphanFill =
        !layout && (wss.length === 0 || (landingEmpty && !landingEmpty.layout));
      if (needsOrphanFill) {
        const otherNamed = new Set<string>();
        for (const w of wss) {
          if (w.name === '_current') continue;
          if (landingEmpty && w.name === landingEmpty.name) continue;
          for (const id of w.paneIds ?? []) otherNamed.add(id);
        }
        const active = isRouterMode() ? activeBackendFromUrl() : null;
        const orphans = Object.values(paneMap).filter((p) => {
          if (!isRunningPane(p) || p.role === 'task_owner' || get().managedOwnerBindings.has(p.id)) return false;
          if (otherNamed.has(p.id)) return false;
          if (active && p.backend && p.backend !== active) return false;
          return true;
        });
        if (orphans.length === 1) {
          layout = { type: 'pane', paneId: orphans[0].id };
        } else if (orphans.length > 1) {
          layout = orphans.reduce<Layout | null>((acc, p) => {
            if (!acc) return { type: 'pane', paneId: p.id };
            return {
              type: 'split',
              direction: 'vertical',
              ratio: 0.5,
              a: acc,
              b: { type: 'pane', paneId: p.id },
            };
          }, null);
        }
      }

      // Restore browser tiles — they're client-side only, so there's no
      // server-side "alive check" needed. Whatever the workspace saved,
      // we trust and render.
      const browserMap: Record<string, BrowserInfo> = {};
      if (preferred?.browsers) {
        for (const b of preferred.browsers) browserMap[b.id] = bindSavedBrowser(b,scope.prefix);
      }
      const fileMap: Record<string, FileTileInfo> = {};
      if (preferred?.files) {
        for (const f of preferred.files) fileMap[f.id] = f;
      }

      if (!bootstrapCurrent()) return;
      const completeBackends = (panes as typeof panes & { completeBackends?: string[] }).completeBackends;
      set({
        panes: paneMap,
        lastPrompts: reconcilePanePromptHistory(get().lastPrompts, Object.values(paneMap), scope,
          !isRouterMode() || !!completeBackends?.includes(activeBackendFromUrl() ?? '')),
        browsers: browserMap,
        files: fileMap,
        layout,
        workspaces: wss,
        ...(portsSequence >= portsAppliedSequence ? {
          ports,
          portsError: (ports as import('./api').PortList).warning ?? null,
          portsUpdatedAt: Date.now(),
        } : {}),
        activeWorkspace: activeWorkspaceName,
        activeWorkspaceMoveVersion: pickWorkspaceByName(wss, activeWorkspaceName, activeBackendFromUrl())?.paneMoveVersion ?? 0,
        activeBackend: isRouterMode() ? activeBackendFromUrl() : null,
        focusedPaneId: layout ? collectPaneIds(layout)[0] ?? null : null,
        loading: false,
        bootstrapError: null,
      });
      portsAppliedSequence = Math.max(portsAppliedSequence,portsSequence);
      // The task feed can settle before bootstrap's workspace response.
      // Reapply its exact bindings so a stale response cannot regraft owners.
      get().detachOwnerPresentation(get().managedOwnerBindings, scope);
      debugPerf('bootstrap', startedAt, {
        panes: panes.length,
        workspaces: wss.length,
        ports: ports.length,
        activeWorkspace: activeWorkspaceName,
      }, 120);
      void get().refreshSchedules();
    } catch (e) {
      if (!bootstrapCurrent()) return;
      if (!isPreviewBuild) console.error('bootstrap failed', e);
      const message = e instanceof Error ? e.message : String(e);
      set({
        loading: false,
        bootstrapError: message || 'Bootstrap failed',
        // Decided by the error that failed THIS load, so a late refusal from
        // an earlier one can't relabel a different failure.
        bootstrapNeedsApproval: isDeviceNotApprovedError(e),
        daemonOnline: false,
      });
      debugPerf('bootstrap failed', startedAt, e, 0);
    } finally {
      if (sequence === bootstrapSequence && scope.key === currentRoutingScope().key) {
        set({ loading: false });
      }
    }
  },

  refreshPanes: async () => {
    const startedAt = perfNow();
    try {
    // In router mode, aggregate panes from BOTH backends so workspace
    // attention badges (waiting / choice-prompt / activity) reflect
    // the non-active backend's live state too. Pane IDs are random
    // hex and collision-safe across daemons.
    const scope = currentRoutingScope();
    const received = isRouterMode() ? await api.listPanesAllBackends() : await api.listPanes();
    if (scope.key !== currentRoutingScope().key) return;
    const completeBackends = (received as typeof received & {completeBackends?: string[]}).completeBackends;
    if(isRouterMode() && completeBackends && !completeBackends.includes(get().activeBackend ?? '')) return;
    const panes = received.filter(p => !p.backend || !get().activeBackend || p.backend === get().activeBackend);
    const current = get().panes;
    const layoutIds = collectPaneIds(get().layout);
    const nextIds = new Set(panes.map((p) => p.id));
    const droppedLayoutIds = layoutIds.filter((id) => current[id] && !nextIds.has(id));
    if (
      currentPrefix()
      && Object.keys(current).length > 0
      && droppedLayoutIds.length > 0
      && cloudPaneRefreshDropGuard < 2
    ) {
      cloudPaneRefreshDropGuard++;
      lifecycleLog('store.refreshPanes.drop-guard', {
        current: Object.keys(current).length,
        next: panes.length,
        droppedLayoutIds,
        guardCount: cloudPaneRefreshDropGuard,
      });
      debugPerf('refreshPanes cloud drop guarded', startedAt, {
        current: Object.keys(current).length,
        next: panes.length,
        droppedLayoutIds,
      }, 0);
      return;
    }
    cloudPaneRefreshDropGuard = 0;
    if(!isRouterMode() || completeBackends?.includes(get().activeBackend ?? '')) {
      reconcileConfirmedTerminalPanes(scope, panes);
      const view=get().mainPresentation;
      if(view.kind === 'owner' && view.scope.key === scope.key && view.pane && !nextIds.has(view.pane.id))
        set({navigationGeneration:get().navigationGeneration+1,mainPresentation:{...view,pane:undefined,error:'Owner terminal is no longer available. Inspect task evidence or retry lookup.'}});
    }
    const map: Record<string, PaneInfo> = {};
    let changed = Object.keys(current).length !== panes.length;
    for (const p of panes) {
      const pendingTitle = paneRenamePending.get(p.id);
      const next = pendingTitle == null ? p : { ...p, title: pendingTitle };
      const prev = current[p.id];
      if (prev && samePaneInfo(prev, next)) {
        map[p.id] = prev;
      } else {
        map[p.id] = next;
        changed = true;
      }
    }
    for (const p of Object.values(current)) {
      if (p.restorePending && p.recoveryError && !map[p.id]) map[p.id] = p;
      if (panesResuming.has(p.id) && !map[p.id]) map[p.id] = p;
    }
    changed = changed || Object.keys(map).length !== Object.keys(current).length;
    const previousPrompts = get().lastPrompts;
    const nextPrompts = reconcilePanePromptHistory(previousPrompts, panes, scope,
      !isRouterMode() || !!completeBackends?.includes(get().activeBackend ?? ''));
    if (!changed) {
      if (nextPrompts !== previousPrompts) set({ lastPrompts: nextPrompts });
      debugPerf('refreshPanes unchanged', startedAt, { panes: panes.length }, 80);
      return;
    }
    // Prune warm-parked ids whose panes died or vanished. parkedPaneIds
    // is otherwise only recomputed on workspace switch, so a parked pane
    // that exits on its own (or is killed by another client / MCP) would
    // keep a dead xterm mounted in the hidden parking lot indefinitely.
    // Preserve the array reference when nothing changed — selectors key
    // on it.
    const prevParked = get().parkedPaneIds;
    const nextParked = prevParked.filter((id) => map[id]?.status === 'running');
    set({
      panes: map,
      ...(nextParked.length !== prevParked.length ? { parkedPaneIds: nextParked } : {}),
      ...(nextPrompts !== previousPrompts ? { lastPrompts: nextPrompts } : {}),
    });
    debugPerf('refreshPanes changed', startedAt, { panes: panes.length }, 50);
    } catch {
      // Daemon unreachable (offline, restarting, cross-origin refusal):
      // keep the last-known pane map; the 3s poll and SSE reconnect retry.
    }
  },

  refreshPorts: async () => {
    const scope = currentRoutingScope().key;
    const sequence = ++portsRefreshSequence;
    try {
      const ports = isRouterMode()
        ? await api.listPortsAllBackends()
        : await api.listPorts();
      // Skip the set when the port list is structurally identical — this
      // poll runs every 3s and ports rarely change, so an unconditional
      // set re-renders every `ports` subscriber on each tick for nothing.
      // Mirrors the change-guards in refreshPanes / refreshWorkspaces.
      if (scope !== currentRoutingScope().key || sequence < portsAppliedSequence) return;
      portsAppliedSequence = sequence;
      const previousBrowsers = get().browsers;
      const browsers = reconnectServiceBrowsers(previousBrowsers, ports, currentPrefix());
      set({
        ...(sameJsonValue(get().ports,ports) ? {} : {ports}),
        ...(browsers === previousBrowsers ? {} : {browsers}),
        portsError:(ports as import('./api').PortList).warning ?? null,
        portsUpdatedAt:Date.now(),
      });
      if (browsers !== previousBrowsers) scheduleAutosave(get);
    } catch {
      if (scope === currentRoutingScope().key && sequence >= portsAppliedSequence) {
        portsAppliedSequence = sequence;
        set({portsError:'Could not refresh services. Check the connection and try Refresh.'});
      }
    }
  },

  refreshWorkspaces: async (opts) => {
    const scope = currentRoutingScope();
    const startedAt = perfNow();
    try {
    if (opts?.full) {
      const wss = isRouterMode()
        ? await api.listWorkspacesAllBackends()
        : await api.listWorkspaces();
      if(scope.key !== currentRoutingScope().key)return;
      if (sameJsonValue(get().workspaces, wss)) {
        debugPerf('refreshWorkspaces full unchanged', startedAt, { workspaces: wss.length }, 80);
        return;
      }
      set({ workspaces: wss });
      debugPerf('refreshWorkspaces full changed', startedAt, { workspaces: wss.length }, 50);
      return;
    }
    const summaries = isRouterMode()
      ? await api.listWorkspaceSummariesAllBackends()
      : await api.listWorkspaceSummaries();
    if(scope.key !== currentRoutingScope().key)return;
    const merged = mergeWorkspaceSummaries(get().workspaces, summaries);
    if (sameWorkspaceListByRef(get().workspaces, merged)) {
      debugPerf('refreshWorkspaces summary unchanged', startedAt, { workspaces: summaries.length }, 80);
      return;
    }
    set({ workspaces: merged });
    debugPerf('refreshWorkspaces summary changed', startedAt, { workspaces: summaries.length }, 50);
    } catch {
      // Daemon unreachable: keep the last-known workspace list; the poll retries.
    }
  },

  mergeSavedWorkspaceIntoCache: (saved) => {
    set((s) => {
      // The save always targets the active backend (apiFetch routes there),
      // so the cached entry to update is the one keyed by that backend. In
      // non-router mode backend is null and the key collapses to ":name".
      const backend = isRouterMode() ? s.activeBackend : null;
      const targetKey = workspaceCacheKey({ name: saved.name, backend: backend ?? undefined });
      let found = false;
      const next = s.workspaces.map((w) => {
        if (workspaceCacheKey(w) !== targetKey) return w;
        found = true;
        // Fold in exactly what we persisted. `summary: false` + the real
        // layout means a later loadWorkspace restores this, not a stale
        // snapshot. Keep the client-side backend tag (the daemon echo
        // doesn't carry it).
        return { ...w, ...saved, backend: w.backend, summary: false };
      });
      if (!found) return s;
      return { workspaces: next };
    });
  },

  refreshSchedules: async () => {
    if (refreshSchedulesInFlight) return refreshSchedulesInFlight;
    refreshSchedulesInFlight = (async () => {
      try {
        const s = await api.listSchedules();
        set({ schedules: s });
      } catch {
        // Non-fatal; retry happens on the next poll or uiBus event.
      } finally {
        refreshSchedulesInFlight = null;
        for (const id of [...scheduleRowsDirty]) {
          if (refreshScheduleInFlight.has(id)) continue;
          scheduleRowsDirty.delete(id);
          void get().refreshSchedule(id);
        }
      }
    })();
    return refreshSchedulesInFlight;
  },

  // A run event names its schedule, so one ~KB row comes back instead of the
  // whole list (~160 KB with 38 schedules' run histories) — previously every
  // run made every open window reload everything, twice. A full refresh
  // already in flight covers the row, so ride it.
  refreshSchedule: async (id) => {
    const pending = refreshScheduleInFlight.get(id) ?? refreshSchedulesInFlight;
    if (pending) {
      // That response may predate this change: fetch the row once more after.
      scheduleRowsDirty.add(id);
      return pending;
    }
    const p = (async () => {
      try {
        const row = await api.getSchedule(id);
        const list = get().schedules;
        const at = list.findIndex((s) => s.id === id);
        if (at === -1) {
          set({ schedules: [...list, row] });
        } else if (!sameJsonValue(list[at], row)) {
          const next = list.slice();
          next[at] = row;
          set({ schedules: next });
        }
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) {
          const list = get().schedules;
          if (list.some((s) => s.id === id)) set({ schedules: list.filter((s) => s.id !== id) });
        } else if (e instanceof ApiError && (e.status === 405 || e.status === 400)) {
          // An older daemon without the single-row route: fall back.
          await get().refreshSchedules();
        }
        // Otherwise non-fatal; the next event or reconnect resyncs.
      } finally {
        refreshScheduleInFlight.delete(id);
        if (scheduleRowsDirty.delete(id) && !refreshSchedulesInFlight) void get().refreshSchedule(id);
      }
    })();
    refreshScheduleInFlight.set(id, p);
    return p;
  },



  loadRecoverySettings: async () => {
    try {
      const settings = await api.getRecoverySettings();
      set({ recoverySettings: settings });
    } catch {
      // Older daemons simply won't expose this endpoint; keep Preferences usable.
    }
  },
  loadOrchestratorHosts: async () => {
    try {
      const info = await api.orchestratorHosts();
      set({ orchestratorHosts: info.hosts });
    } catch {
      // Older daemons have no endpoint. The UI's default keeps Claude working
      // and presents Codex as disabled until availability can be verified.
      set({ orchestratorHosts: slotValue('paneRoleHosts')?.legacyDaemonHosts ?? [] });
    }
  },
  loadNativeSettings: async () => {
    try {
      const settings = await api.getNativeSettings();
      set({ nativeSettings: settings });
    } catch {
      // Older daemons won't expose /api/native/settings; keep Preferences usable.
    }
  },
  setNativePromptSuggestions: async (enabled) => {
    const prev = get().nativeSettings;
    const optimistic: NativeSettings = { ...prev, promptSuggestions: enabled };
    set({ nativeSettings: optimistic });
    try {
      const settings = await api.putNativeSettings(optimistic);
      set({ nativeSettings: settings });
    } catch (e) {
      set({ nativeSettings: prev });
      get().toast(`Couldn't save prompt-suggestions setting: ${(e as Error).message}`, {
        kind: 'error',
        icon: '!',
        ttlMs: 5000,
      });
    }
  },
  setNativeDefaultClaudeMode: async (mode) => {
    const prev = get().nativeSettings;
    const optimistic: NativeSettings = {
      promptSuggestions: prev?.promptSuggestions ?? false,
      defaultClaudeMode: mode,
    };
    set({ nativeSettings: optimistic });
    try {
      const settings = await api.putNativeSettings(optimistic);
      set({ nativeSettings: settings });
    } catch (e) {
      set({ nativeSettings: prev });
      get().toast(`Couldn't save default-mode setting: ${(e as Error).message}`, {
        kind: 'error',
        icon: '!',
        ttlMs: 5000,
      });
    }
  },
  setRecoveryResumeMode: async (mode) => {
    const prev = get().recoverySettings;
    const optimistic: RecoverySettings = { resumeAfterRestart: mode };
    set({ recoverySettings: optimistic });
    try {
      const settings = await api.putRecoverySettings(optimistic);
      set({ recoverySettings: settings });
    } catch (e) {
      set({ recoverySettings: prev });
      get().toast(`Couldn't save restart recovery setting: ${(e as Error).message}`, {
        kind: 'error',
        icon: '!',
        ttlMs: 5000,
      });
      throw e;
    }
  },
  loadUpdateSettings: async () => {
    try {
      const settings = await api.getUpdateSettings();
      set({ updateSettings: settings });
      // A freshly-loaded 'when-idle' pref may already have an update waiting
      // (badge lit before the pref arrived) — evaluate immediately, don't wait
      // for the next poll.
      get().maybeAutoApplyUpdate();
      get().maybeAutoApplyShellUpdate();
    } catch {
      // Older daemons won't expose /api/update/settings; keep Settings usable.
    }
  },
  loadWardenSettings: async () => {
    try {
      set({ wardenSettings: await api.getWardenSettings() });
    } catch {
      // Older daemons won't expose /api/warden/settings; keep Settings usable.
    }
  },
  saveWardenSettings: async (patch) => {
    const prev = get().wardenSettings;
    // Optimistic: merge the patch onto the current view (or a disabled base).
    const optimistic: WardenSettings = { enabled: false, ...(prev ?? {}), ...patch };
    set({ wardenSettings: optimistic });
    try {
      set({ wardenSettings: await api.setWardenSettings(patch) });
    } catch (e) {
      set({ wardenSettings: prev });
      get().toast(`Couldn't save Warden setting: ${(e as Error).message}`, {
        kind: 'error',
        icon: '!',
        ttlMs: 5000,
      });
      throw e;
    }
  },
  setUpdateAutoApply: async (mode) => {
    const prev = get().updateSettings;
    const optimistic: UpdateSettings = { ...(prev ?? {}), autoApply: mode };
    set({ updateSettings: optimistic });
    try {
      const settings = await api.putUpdateSettings(optimistic);
      set({ updateSettings: settings });
      // Turning it on with an update already available should apply now, not on
      // the next 30s poll.
      get().maybeAutoApplyUpdate();
    } catch (e) {
      set({ updateSettings: prev });
      get().toast(`Couldn't save automatic-updates setting: ${(e as Error).message}`, {
        kind: 'error',
        icon: '!',
        ttlMs: 5000,
      });
      throw e;
    }
  },
  setUpdateShellAutoApply: async (mode) => {
    const prev = get().updateSettings;
    const optimistic: UpdateSettings = {
      autoApply: prev?.autoApply ?? 'off',
      shellAutoApply: mode,
    };
    set({ updateSettings: optimistic });
    if (mode === 'off') disarmShellAutoRestart();
    try {
      const settings = await api.putUpdateSettings(optimistic);
      set({ updateSettings: settings });
      get().maybeAutoApplyShellUpdate();
    } catch (e) {
      set({ updateSettings: prev });
      get().toast(`Couldn't save desktop-restart setting: ${(e as Error).message}`, {
        kind: 'error',
        icon: '!',
        ttlMs: 5000,
      });
      throw e;
    }
  },

  refreshUpdateStatus: async () => {
    try {
      const u = normalizeUpdateStatus(await api.getUpdateStatus());
      const observation = observeAuthoritativeUpdateStatus(u);
      if (observation === 'normal' && !u.shellApplying) {
        // The reload above only fires when this page OBSERVED the
        // applying→done transition. A backgrounded/throttled webview
        // (Tauri WebView2 especially) can sleep through the swap and
        // keep running the previous bundle indefinitely — UI features
        // then silently lag the daemon. Detect it by version mismatch
        // and self-heal on the next status refresh.
        const observed = getDaemonHealthState().bundleState;
        const bundleState = classifyBundleParity({
          runningBundleVersion: observed.runningBundleVersion,
          shellDaemonVersion: observed.shellDaemonVersion,
          shellWebVersion: observed.shellWebVersion,
          targetVersion: newestVersion(observed.targetVersion, u.current),
        });
        if (scheduleStaleBundleReload(bundleState)) {
          get().toast(`Interface updated to ${bundleReloadTarget(bundleState)} — reloading…`, { kind: 'success', icon: '✓' });
        }
      }
      // "Install when idle": if the user opted in, kick off the swap now that we
      // have fresh status. Runs on every refresh (bootstrap + 30s poll + the
      // update-available SSE) and no-ops unless there's a new version to apply;
      // its once-per-version guard keeps repeated polls from re-firing.
      get().maybeAutoApplyUpdate();
      get().maybeAutoApplyShellUpdate();
      void get().continueUpdateAfterInstaller();
    } catch {
      // Daemon temporarily unreachable (mid-restart, network blip); the
      // SSE "update-available" frame will retrigger us, and the user
      // can also click the badge again to retry.
    }
  },

  confirmUpdateApplyingEvent: async (targetVersion) => {
    // The SSE frame is deliberately non-authoritative: router/cloud replay and
    // reconnect can deliver it after an aborted or completed attempt. A fresh
    // level read is the only way it can enter the shared bounded controller.
    const confirmationGeneration = updateAdmissionConfirmationGeneration;
    try {
      const fresh = normalizeUpdateStatus(await api.getUpdateStatus());
      if (confirmationGeneration !== updateAdmissionConfirmationGeneration) return;
      observeAuthoritativeUpdateStatus(fresh, targetVersion);
    } catch {
      // Unreachable status proves no admission. Normal health/status polling
      // will resync after the relay or daemon comes back.
    }
  },

  abortUpdateRecovery: () => {
    const controller = detachUpdateRecovery();
    rememberSettledUpdateRecovery(controller);
    useStore.setState((state) => ({
      updateStatus: state.updateStatus
        ? { ...state.updateStatus, applying: false }
        : state.updateStatus,
    }));
    controller?.resolve();
  },

  maybeAutoApplyUpdate: () => {
    const st = get();
    const s = st.updateStatus;
    // A REQUIRED update (the daemon is below the operator's min_daemon_version
    // floor) force-enables the when-idle behavior even for "Notify me" users:
    // the swap is seamless, and the alternative is the blocking modal. Same
    // safety gate either way — never over a mid-turn native pane.
    if (st.updateSettings?.autoApply !== 'when-idle' && !s?.required) return;
    // `available` already reflects the daemon's staged-rollout + kill-switch
    // gating (suppressed when blocked), so we inherit that for free.
    if (!s?.available || !s.latest) return;
    if (s.holdVersion && s.holdVersion === s.latest) return;
    if (s.applying || s.workflowDeferred || st.deferredUpdate) return; // already swapping / queued
    // The swap reloads the page, which loses a compose box's unsent text. Wait
    // until it is sent or cleared: the next status refresh (30 s poll, SSE)
    // re-evaluates, and no stamp is written, so nothing is used up meanwhile.
    if (hasUnsentInput()) return;
    const v = s.latest;
    // Once per version — but a transient failure retries after the cooldown, and
    // a NEW version (different `latest`) always gets a fresh attempt.
    const stamp = readAutoApplyStamp();
    if (stamp && stamp.version === v && Date.now() - stamp.at < AUTO_APPLY_RETRY_COOLDOWN_MS) return;
    writeAutoApplyStamp({ version: v, at: Date.now() });
    // auto:true — applyUpdate holds (never forces) behind a mid-turn native pane.
    // Swallow rejections: this is the unattended path, so a refusal (daemon
    // already mid-swap, another device won the race, network blip) gets no
    // toast — the stamp cooldown retries on a later poll if it mattered.
    void st.applyUpdate({ auto: true }).catch(() => {});
  },

  maybeAutoApplyShellUpdate: () => {
    const before = get();
    const status = before.updateStatus;
    if (!shellAutoApplyAllowedHere()) return;
    if (before.updateSettings?.shellAutoApply !== 'when-idle') return;
    if (!status?.shellLatest || status.shellApplying || status.applying) return;
    if (!shellAutoApplyEnabled(status)) return;
    // Through the daemon only when it verified it can install for this
    // computer's app (a plain install.sh daemon has no installer to offer).
    if (shellUpdateNeedsDaemon(status) && !status.shellAvailable) return;
    if (status.holdVersion && status.holdVersion === status.shellLatest) return;
    if (status.current && isVersionNewer(status.current, status.shellLatest)) return;
    if (before.shellAutoRestart || shellAutoCheckInFlight) return;

    const now = Date.now();
    const hold = readShellHoldStamp();
    if (hold && hold.version === status.shellLatest && hold.until > now) return;
    if (shellAttemptBlocked(readShellAttemptStamp(), status.shellLatest, now)) return;
    if (!shellUpdateIdleGate().idle) return;

    shellAutoCheckInFlight = true;
    void (async () => {
      try {
        const installed = await getShellVersion();
        const current = get();
        const fresh = current.updateStatus;
        if (!installed || !fresh?.shellLatest || !isVersionNewer(fresh.shellLatest, installed)) return;
        if (fresh.holdVersion && fresh.holdVersion === fresh.shellLatest) return;
        if (!shellAutoApplyAllowedHere()) return;
        if (current.updateSettings?.shellAutoApply !== 'when-idle') return;
        if (shellUpdateNeedsDaemon(fresh) && !fresh.shellAvailable) return;
        if (!shellAutoApplyEnabled(fresh)) return;
        if (fresh.shellApplying || fresh.applying || current.shellAutoRestart) return;
        if (fresh.current && isVersionNewer(fresh.current, fresh.shellLatest)) return;
        const freshHold = readShellHoldStamp();
        if (freshHold && freshHold.version === fresh.shellLatest && freshHold.until > Date.now()) return;
        if (shellAttemptBlocked(readShellAttemptStamp(), fresh.shellLatest, Date.now())) return;
        if (!shellUpdateIdleGate().idle) return;

        shellAutoRestartArmedAtMs = Date.now();
        set({
          shellAutoRestart: {
            version: fresh.shellLatest,
            deadlineMs: shellAutoRestartArmedAtMs + SHELL_AUTO_COUNTDOWN_MS,
          },
        });
        clearShellAutoRestartTimer();
        shellAutoRestartTimer = window.setInterval(() => {
          const state = useStore.getState();
          const armed = state.shellAutoRestart;
          if (!armed) {
            clearShellAutoRestartTimer();
            return;
          }
          if (
            state.updateSettings?.shellAutoApply !== 'when-idle' ||
            state.updateStatus?.shellLatest !== armed.version ||
            state.updateStatus?.holdVersion === armed.version ||
            !shellAutoApplyEnabled(state.updateStatus) ||
            !shellUpdateIdleGate().idle ||
            lastUserInputMs > shellAutoRestartArmedAtMs
          ) {
            disarmShellAutoRestart();
            return;
          }
          if (Date.now() >= armed.deadlineMs) {
            void state.applyShellUpdateNow();
          }
        }, 1000);
      } finally {
        shellAutoCheckInFlight = false;
      }
    })();
  },

  applyShellUpdateNow: async (opts) => {
    const state = get();
    const version = state.shellAutoRestart?.version ?? state.updateStatus?.shellLatest;
    if (!version) return;
    if (state.updateStatus?.holdVersion === version) {
      disarmShellAutoRestart();
      return;
    }
    const gate = opts?.userInitiated ? shellUpdateSafetyGate() : shellUpdateIdleGate();
    if (!gate.idle) {
      disarmShellAutoRestart();
      // A click on "Restart now" must not just make the banner vanish.
      if (opts?.userInitiated) {
        get().toast(`Restart held: ${shellHoldCopy(gate.reason)}. It restarts automatically once that clears.`, {
          kind: 'info', icon: '↑', ttlMs: 8000,
        });
      }
      return;
    }
    if (!shellAutoApplyAllowedHere() || (!opts?.userInitiated && !shellAutoApplyEnabled(state.updateStatus))) {
      disarmShellAutoRestart();
      return;
    }
    disarmShellAutoRestart();
    const previous = readShellAttemptStamp();
    const sameVersion = previous && previous.version === version ? previous : null;
    writeShellAttemptStamp({
      version,
      at: Date.now(),
      attempts: sameVersion ? sameVersion.attempts + 1 : 1,
      ...(sameVersion?.appNotFoundAt ? { appNotFoundAt: sameVersion.appNotFoundAt } : {}),
    });

    set((current) => ({
      updateStatus: current.updateStatus
        ? { ...current.updateStatus, shellApplying: true }
        : current.updateStatus,
    }));
    const outcome = await applyShellUpdateSilently(state.updateStatus);
    if (outcome.paused) {
      // Not an attempt and not an error: the manual banner takes over. Put
      // back the count from before this call, so earlier failures still count.
      if (previous && previous.version === version) writeShellAttemptStamp(previous);
      else clearShellAttemptStamp(version);
      if (opts?.userInitiated) {
        get().toast('Automatic desktop restarts are paused for this install. Use Update now to install the desktop update.', {
          kind: 'info', icon: '↑', ttlMs: 8000,
        });
      }
      set((current) => ({
        updateStatus: current.updateStatus
          ? { ...current.updateStatus, shellApplying: false, shellAutoApplyPaused: true }
          : current.updateStatus,
      }));
      void get().refreshUpdateStatus();
      return;
    }
    if (!outcome.ok) {
      set((current) => ({
        updateStatus: current.updateStatus
          ? { ...current.updateStatus, shellApplying: false }
          : current.updateStatus,
      }));
      const failedAt = Date.now();
      if (outcome.appNotFound) {
        writeShellAutoOutcome({ version, at: failedAt, code: 'shell_silent_app_not_found', ...(outcome.error ? { error: outcome.error } : {}) });
        const notFound = [...(sameVersion?.appNotFoundAt ?? []).filter((at) => failedAt - at < SHELL_AUTO_DAY_MS), failedAt];
        if (notFound.length <= SHELL_AUTO_APP_NOT_FOUND_PER_DAY) {
          // Nothing was installed or closed: not one of the attempts. Try
          // again in a quarter of an hour.
          writeShellAttemptStamp({
            version,
            at: failedAt,
            attempts: sameVersion?.attempts ?? 0,
            retryAt: failedAt + SHELL_AUTO_APP_NOT_FOUND_RETRY_MS,
            appNotFoundAt: notFound,
          });
          get().toast('Automatic desktop update did not start. It tries again in 15 minutes.', {
            kind: 'info', icon: '↑', ttlMs: 8000,
          });
          return;
        }
        // Too many today: this one counts as a failed attempt.
        writeShellAttemptStamp({ version, at: failedAt, attempts: (sameVersion?.attempts ?? 0) + 1, appNotFoundAt: notFound });
      } else {
        writeShellAutoOutcome({
          version,
          at: failedAt,
          code: outcome.downgradeRefused ? 'shell_daemon_downgrade_refused' : outcome.updateHeld ? 'update_held' : outcome.interactiveLaunched ? 'interactive' : 'failed',
          ...(outcome.error ? { error: outcome.error } : {}),
        });
      }
      const message = outcome.downgradeRefused
        ? `Desktop update refused: installed daemon ${outcome.installedVersion ?? 'unknown'} is newer than bundled daemon ${outcome.installerVersion ?? version}. Automatic updates never force a downgrade.`
        : outcome.updateHeld
          ? outcome.error ?? `Desktop version ${outcome.installerVersion ?? version} is paused by a rollback hold. Resume updates before retrying.`
        : outcome.interactiveLaunched
          ? 'The desktop installer opened — follow its prompts to finish the update.'
        : 'Automatic desktop update could not start — use Update now to finish manually.';
      get().toast(message, { kind: 'warn', icon: '⚠', ttlMs: 8000 });
      return;
    }

    writeShellAutoOutcome(null);
    get().toast('Restarting to finish the update…', { kind: 'info', icon: '↑', ttlMs: 6000 });
    window.setTimeout(() => {
      const exitGate = opts?.userInitiated ? shellUpdateSafetyGate() : shellUpdateIdleGate();
      if (!exitGate.idle) {
        clearShellAttemptStamp(version);
        set((current) => ({
          updateStatus: current.updateStatus
            ? { ...current.updateStatus, shellApplying: false }
            : current.updateStatus,
        }));
        get().toast('Automatic restart deferred because active work appeared.', {
          kind: 'info', icon: '↑', ttlMs: 6000,
        });
        return;
      }
      void exitForUpdateNative().then((exiting) => {
        if (exiting) return;
        set((current) => ({
          updateStatus: current.updateStatus
            ? { ...current.updateStatus, shellApplying: false }
            : current.updateStatus,
        }));
        get().toast('Could not restart automatically — use Update now to finish manually.', {
          kind: 'warn', icon: '⚠', ttlMs: 8000,
        });
      });
    }, 1500);
  },

  postponeShellAutoRestart: () => {
    const version = get().shellAutoRestart?.version ?? get().updateStatus?.shellLatest;
    if (!version) return;
    writeShellHoldStamp({ version, until: Date.now() + SHELL_AUTO_HOLD_MS });
    disarmShellAutoRestart();
  },

  restartShellNowForUpdate: async () => {
    await get().applyShellUpdateNow({ userInitiated: true });
  },

  checkForUpdate: async () => {
    // POST /api/update/check forces the daemon to re-fetch the manifest now
    // rather than waiting for its 30s background poll. Mirror the result into
    // local state so the popover reflects the fresh "last checked" + verdict.
    try {
      const u = normalizeUpdateStatus(await api.checkForUpdate());
      observeAuthoritativeUpdateStatus(u);
    } catch {
      // Network blip / daemon mid-restart — leave the cached status in place;
      // the user can click again.
    }
  },

  applyUpdate: async (opts) => {
    const skipReload = opts?.skipReload === true;
    const auto = opts?.auto === true;

    if (get().updateApplyRequestInFlight) {
      // A retry can race another apply caller after its timer has fired. Never
      // leave a still-visible hold without a controller just because the
      // in-flight request won deduplication.
      const existing = get().deferredUpdate;
      if (existing && deferredUpdateRetryTimer === null) queueDeferredUpdate(existing, existing.auto);
      return;
    }

    // The local gate is responsive UI, not an authority boundary. After it
    // settles the retry stays non-forced so the daemon can enforce workflow
    // drain and re-check both gates immediately before the swap.
    if (!opts?.force && nativePaneBusy(get().panes)) {
      const paneId = nativeBusyPaneId(get().panes);
      queueDeferredUpdate({
        cause: 'focus',
        since: Date.now(),
        auto,
        paneId,
        reason: paneId
          ? `a Focus pane is mid-turn (pane ${paneId})`
          : 'a Focus pane is mid-turn',
      }, auto);
      return;
    }

    // Only the explicit override clears a hold before admission. Normal retries
    // retain the same mounted hold while the server answers, allowing a Focus →
    // workflow transition to update in place without a blank frame.
    if (opts?.force) {
      cancelDeferredUpdateRetry();
      deferredUpdateRetryVersion = undefined;
      set({ deferredUpdate: null });
    }

    const applyAttemptGeneration = ++updateApplyAttemptGeneration;
    set({ updateApplyRequestInFlight: true });
    // Persist any pending debounced workspace save BEFORE the daemon
    // starts its exit dance — otherwise the last resize / layout edit
    // is silently lost, either because:
    //   (a) the 400ms debounce timer hasn't fired yet and doAutosave's
    //       `if (applying) return` guard below will kill it once it
    //       does, or
    //   (b) the timer fired but the fire-and-forget HTTP save is still
    //       in flight when the daemon exits and 502s it.
    // flushAutosave awaits the save roundtrip, so we know the new
    // daemon will read this state on its next bootstrap.
    const before = get().updateStatus;
    const beforeVersion = before?.current ?? '';
    const targetVersion = before?.latest;
    let admittedRecovery: Promise<void> | null = null;
    try {
      await flushAutosave(get);
      const accepted = await api.applyUpdate(opts?.force === true);
      // A successful HTTP status alone is insufficient: older/alternate
      // transports may return an acknowledgement body before admission. Only
      // the daemon's explicit applying:true body joins the bounded controller.
      if (accepted.applying === true) {
        admittedRecovery = beginUpdateRecovery({
          beforeVersion,
          targetVersion,
          skipReload,
          admissionConfirmed: true,
          applyAttemptGeneration,
        });
      }
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        // A delayed/replayed SSE confirmation can overlap this request. The
        // typed refusal is definitive: cancel recovery before installing the
        // truthful hold so applying is never observable after the 409.
        const controller = detachUpdateRecovery();
        // A typed refusal settles only this POST. It invalidates any
        // confirmation GET that overlapped the request, but it must not block
        // a later independently admitted swap for the same release (for
        // example one started by another connected client).
        invalidatePendingUpdateAdmissionConfirmations();
        useStore.setState((state) => ({
          updateStatus: state.updateStatus
            ? { ...state.updateStatus, applying: false }
            : state.updateStatus,
        }));
        controller?.resolve();
        const body = e.body && typeof e.body === 'object'
          ? e.body as Record<string, unknown>
          : {};
        if (apiErrorCode(e) === 'update_held') {
          cancelDeferredUpdateRetry();
          deferredUpdateRetryVersion = undefined;
          set((state) => ({
            deferredUpdate: null,
            updateStatus: state.updateStatus ? {
              ...state.updateStatus,
              applying: false,
              holdVersion: typeof body.heldVersion === 'string' ? body.heldVersion : state.updateStatus.holdVersion,
              holdReason: typeof body.holdReason === 'string' ? body.holdReason : state.updateStatus.holdReason,
              holdExpiresMs: typeof body.holdExpiresMs === 'number' ? body.holdExpiresMs : state.updateStatus.holdExpiresMs,
            } : state.updateStatus,
          }));
          return;
        }
        const isQuietDefer = body.deferred === true;
        const cause = body.cause === 'focus' || body.cause === 'workflow'
          ? body.cause
          : isQuietDefer
            ? 'workflow'
            : 'unknown';
        const reason = typeof body.reason === 'string'
          ? body.reason
          : typeof body.error === 'string'
            ? body.error
            : e.body === undefined
              ? e.message
              : undefined;
        // Old daemons may return only the legacy plain-text reason. Preserve a
        // useful pane identifier without inferring a typed Focus cause from
        // prose; legacy holds remain safely generic.
        const legacyPaneMatch = cause === 'unknown' && reason
          ? reason.match(/\(pane\s+([^)]+)\)/i)
          : null;
        queueDeferredUpdate({
          cause,
          since: Date.now(),
          auto,
          reason,
          paneId: typeof body.paneId === 'string'
            ? body.paneId
            : legacyPaneMatch?.[1],
          workflowInflight:
            typeof body.workflowInflight === 'number'
              ? body.workflowInflight
              : typeof body.inflight === 'number' ? body.inflight : undefined,
          workflowDeferDeadlineMs:
            typeof body.workflowDeferDeadlineMs === 'number'
              ? body.workflowDeferDeadlineMs
              : undefined,
          workflowNextRetryMs:
            typeof body.nextRetryMs === 'number' ? body.nextRetryMs : undefined,
        }, auto);
        return;
      }
      const existing = get().deferredUpdate;
      if (existing) queueDeferredUpdate(existing, existing.auto);
      throw e;
    } finally {
      set({ updateApplyRequestInFlight: false });
    }
    if (admittedRecovery) await admittedRecovery;
  },

  rollbackUpdate: async (opts) => {
    const before = get().updateStatus;
    const beforeVersion = before?.current ?? '';
    set({ rollbackDecline: null, updateApplyRequestInFlight: true });
    const applyAttemptGeneration = ++updateApplyAttemptGeneration;
    let admittedRecovery: Promise<void> | null = null;
    try {
      await flushAutosave(get);
      const accepted = await api.rollbackUpdate({
        targetVersion: opts.targetVersion,
        holdHours: opts.holdHours,
        force: opts.force === true,
      });
      if (accepted.applying === true) {
        admittedRecovery = beginUpdateRecovery({
          beforeVersion,
          targetVersion: opts.targetVersion,
          skipReload: opts.skipReload === true,
          admissionConfirmed: true,
          applyAttemptGeneration,
        });
      }
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) {
        const controller = detachUpdateRecovery();
        invalidatePendingUpdateAdmissionConfirmations();
        controller?.resolve();
        const body = error.body && typeof error.body === 'object'
          ? error.body as Record<string, unknown>
          : {};
        if (apiErrorCode(error) === 'rollback_incompatible') {
          set({
            rollbackDecline: {
              store: String(body.store ?? 'state'),
              requiredReader: Number(body.requiredReader ?? 0),
              targetReader: Number(body.targetReader ?? 0),
              targetVersion: String(body.targetVersion ?? opts.targetVersion),
              message: String(body.message ?? 'This stored state is not compatible with the rollback target.'),
            },
            updateStatus: get().updateStatus ? { ...get().updateStatus!, applying: false } : null,
          });
          return;
        }
        if (apiErrorCode(error) === 'update_held') {
          set((state) => ({
            updateStatus: state.updateStatus ? {
              ...state.updateStatus,
              applying: false,
              holdVersion: typeof body.heldVersion === 'string' ? body.heldVersion : state.updateStatus.holdVersion,
              holdReason: typeof body.holdReason === 'string' ? body.holdReason : state.updateStatus.holdReason,
              holdExpiresMs: typeof body.holdExpiresMs === 'number' ? body.holdExpiresMs : state.updateStatus.holdExpiresMs,
            } : state.updateStatus,
          }));
          return;
        }
        const cause = body.cause === 'focus' || body.cause === 'workflow'
          ? body.cause
          : body.deferred === true ? 'workflow' : 'unknown';
        set((state) => ({
          updateStatus: state.updateStatus ? { ...state.updateStatus, applying: false } : state.updateStatus,
        }));
        queueDeferredUpdate({
          cause,
          since: Date.now(),
          auto: false,
          reason: typeof body.reason === 'string' ? body.reason : typeof body.error === 'string' ? body.error : undefined,
          paneId: typeof body.paneId === 'string' ? body.paneId : undefined,
          workflowInflight: typeof body.workflowInflight === 'number' ? body.workflowInflight : typeof body.inflight === 'number' ? body.inflight : undefined,
          workflowDeferDeadlineMs: typeof body.workflowDeferDeadlineMs === 'number' ? body.workflowDeferDeadlineMs : undefined,
          workflowNextRetryMs: typeof body.nextRetryMs === 'number' ? body.nextRetryMs : undefined,
          rollbackTarget: opts.targetVersion,
          rollbackHoldHours: opts.holdHours,
        }, false);
        return;
      }
      throw error;
    } finally {
      set({ updateApplyRequestInFlight: false });
    }
    if (admittedRecovery) await admittedRecovery;
  },

  releaseUpdateHold: async () => {
    try {
      const refreshed = normalizeUpdateStatus(await api.releaseUpdateHold());
      set({ updateStatus: refreshed, rollbackDecline: null });
    } catch (e) {
      get().toast(`Couldn't release the update hold: ${(e as Error).message}`, {
        kind: 'error',
        icon: '!',
        ttlMs: 5000,
      });
    }
  },

  applyShellUpdate: async (opts) => {
    // Defensive: in a relayed session the daemon is on a REMOTE machine, so this
    // would download + launch the installer THERE, not on this computer (the
    // "stuck at Launching installer…" trap). The UI suppresses the daemon-driven
    // shell action there (VersionPopover/StatusBar/Topbar); this guards a stale
    // or programmatic caller. The local shell updates via ShellUpdateBanner.
    // Not useCloud's mode: a local daemon with sign-in also reports 'cloud'.
    if (!daemonOnThisComputer()) {
      get().toast(
        'Installer updates apply to this computer — use the "Update now" banner at the top. In cloud mode the daemon is on another machine.',
        { kind: 'warn', icon: '⚠', ttlMs: 8000 },
      );
      return;
    }
    // Optimistic flip so the badge shows a spinner while the daemon
    // downloads the installer + launches it. We don't freeze the rest
    // of the UI like applyUpdate does — the daemon stays alive
    // throughout, and the user might cancel the UAC prompt, leaving
    // the app fully usable. We just want feedback during the ~1-3s
    // download + spawn window.
    set((s) => ({
      updateStatus: s.updateStatus
        ? { ...s.updateStatus, shellApplying: true }
        : s.updateStatus,
    }));
    let statusRefreshedAfterFailure = false;
    try {
      await api.applyShellUpdate(opts?.forceDowngrade === true);
      // Success means the installer process was spawned. The user is
      // about to see a UAC prompt and the NSIS UI. NSIS's Tauri
      // template handles closing this app and replacing it; if they
      // cancel, the app keeps running and can be re-clicked.
      get().toast('Installer launched — follow the prompts to complete update', {
        kind: 'info',
        icon: '↑',
        ttlMs: 10000,
      });
    } catch (e) {
      await get().refreshUpdateStatus();
      statusRefreshedAfterFailure = true;
      // refreshUpdateStatus intentionally swallows a transient fetch error. The
      // apply request itself has already returned, so never leave our optimistic
      // spinner stuck if that immediate status read happened to fail.
      if (get().updateStatus?.shellApplying) {
        set((s) => ({
          updateStatus: s.updateStatus ? { ...s.updateStatus, shellApplying: false } : s.updateStatus,
        }));
      }
      const launch = get().updateStatus?.shellLaunch;
      if (apiErrorCode(e) === 'shell_daemon_downgrade_refused') {
        const body = e instanceof ApiError && typeof e.body === 'object' && e.body !== null
          ? e.body as { installedVersion?: string; installerVersion?: string; message?: string }
          : {};
        get().toast(
          body.message ?? `Installer refused: installed daemon ${body.installedVersion ?? 'unknown'} is newer than bundled daemon ${body.installerVersion ?? 'unknown'}.`,
          { kind: 'warn', icon: '⚠', ttlMs: 12000 },
        );
      } else if (launch?.state === 'launch-failed') {
        get().toast(`Installer downloaded but didn't open — run it manually: ${launch.stagedPath ?? 'staged installer'}`, {
          kind: 'error',
          icon: '⚠',
          ttlMs: 15000,
        });
      } else {
        get().toast(`Installer download failed: ${(e as Error).message}`, {
          kind: 'error',
          icon: '⚠',
        });
      }
      throw e;
    } finally {
      // Daemon's shellApplying flag flips back false once ApplyShell
      // returns server-side. Re-fetch status so we mirror the daemon.
      if (!statusRefreshedAfterFailure) void get().refreshUpdateStatus();
    }
  },

  applyAllUpdates: async (opts) => {
    const status = get().updateStatus;
    const installer = status?.shellLatest;
    const cloudLaunchMode = isCloudLaunchMode();
    // Cloud update status belongs to the remote daemon, not this desktop.
    // Only the native updater can inspect the locally installed daemon and
    // decide whether this installer is monotonic on the current machine.
    const installedDaemon = cloudLaunchMode ? undefined : status?.current;
    const shellWouldDowngrade = !!(
      installer && installedDaemon && isVersionNewer(installedDaemon, installer)
    );
    const shellHeld = !!(installer && status?.holdVersion === installer);
    // The daemon always publishes shellLatest when a carried installer exists,
    // even when that installer is already installed. A launcher callback only
    // chooses how to apply an actionable update; it must not turn carried
    // metadata into a no-op reinstall.
    const shellUpdateRequested = !shellHeld && (!!status?.shellAvailable || opts?.shellActionable === true);

    if (installer && shellUpdateRequested && !shellWouldDowngrade) {
      // Launch the installer first. It carries daemon == shell.version; after
      // restart that daemon can safely finish any remaining daemon-only update.
      // Updating the daemon first recreated the real 0.0.932→0.0.916 incident.
      // The daemon half carries on after the relaunch. Only for a daemon on
      // this computer: in cloud launch mode or a relayed session the status
      // belongs to another machine's daemon, which this click never updates.
      if (status?.available && status.latest && !cloudLaunchMode && daemonOnThisComputer()) {
        writeUpdateContinuation({
          shellTarget: installer,
          daemonTarget: status.latest,
          at: Date.now(),
          page: UPDATE_CONTINUE_PAGE,
        });
      }
      try {
        await (opts?.applyShell ? opts.applyShell() : get().applyShellUpdate());
      } catch {
        return;
      }
      return;
    }

    let refusal: ApplyAllUpdatesResult | undefined;
    if (shellWouldDowngrade) {
      refusal = {
        downgradeRefused: true,
        installedVersion: installedDaemon,
        installerVersion: installer,
      };
      get().toast(
        `Installer held: installed daemon ${installedDaemon} is newer than bundled daemon ${installer}. The daemon update can proceed safely; the installer will not be forced.`,
        { kind: 'warn', icon: '⚠', ttlMs: 12000 },
      );
    }
    // applyAllUpdates is a local-desktop action in cloud launch mode. Never
    // turn it into an update/restart of the remote daemon backing this page.
    if (cloudLaunchMode) return refusal;
    if (!status?.available) return refusal;
    try {
      await get().applyUpdate();
    } catch {
      return refusal;
    }
    return refusal;
  },

  continueUpdateAfterInstaller: async () => {
    const intent = readUpdateContinuation();
    if (!intent) return;
    if (!intent.shellTarget || !intent.daemonTarget || Date.now() - intent.at > UPDATE_CONTINUE_TTL_MS) {
      writeUpdateContinuation(null);
      return;
    }
    // The page that launched the installer: the app has not been replaced.
    if (intent.page === UPDATE_CONTINUE_PAGE) return;
    if (!isDesktopApp() || isCloudLaunchMode() || !daemonOnThisComputer()) return;
    const before = get();
    const status = before.updateStatus;
    if (!status?.current) return;
    // Already there: the relaunched app may have updated its WSL daemon on its
    // own (desktop daemon.rs request_wsl_upgrade_once), or another client did.
    if (!isVersionNewer(intent.daemonTarget, status.current)) {
      writeUpdateContinuation(null);
      return;
    }
    // A swap or hold already in motion settles first; the next refresh decides.
    if (status.applying || status.workflowDeferred || before.deferredUpdate || before.updateApplyRequestInFlight) return;
    if (updateContinueCheckInFlight) return;
    updateContinueCheckInFlight = true;
    try {
      const installed = await getShellVersion();
      // The installer has not finished (or was cancelled): wait, until the TTL.
      if (!installed || isVersionNewer(intent.shellTarget, installed)) return;
      const fresh = get().updateStatus;
      // A just-started daemon has not fetched its manifest yet (no latest,
      // available false for its first seconds): unknown, not "nothing to do".
      if (!fresh?.latest) return;
      if (!fresh.available || (fresh.holdVersion && fresh.holdVersion === fresh.latest)) {
        writeUpdateContinuation(null);
        return;
      }
      if (fresh.applying || fresh.workflowDeferred || get().deferredUpdate || get().updateApplyRequestInFlight) return;
      writeUpdateContinuation(null);
      get().toast(`Desktop app updated — finishing the update to v${fresh.latest}…`, { kind: 'info', icon: '↑', ttlMs: 6000 });
      try {
        // Not forced: a mid-turn Focus pane or a workflow drain queues it
        // exactly like the click would have, with the same visible hold.
        await get().applyUpdate();
      } catch (e) {
        get().toast(`Couldn't finish the update automatically: ${(e as Error).message} — use Update now`, {
          kind: 'warn', icon: '⚠', ttlMs: 8000,
        });
      }
    } finally {
      updateContinueCheckInFlight = false;
    }
  },

  refreshMcpServers: async (cwd) => {
    if (!cwd) return;
    try {
      const servers = await api.listMcpServers(cwd);
      set((st) => ({ mcpServersByCwd: { ...st.mcpServersByCwd, [cwd]: servers } }));
    } catch {
      // Non-fatal — empty list shows the empty state, which is the
      // same outcome a missing ~/.claude.json would produce. Avoids
      // a toast burst if the daemon is mid-restart.
      set((st) => ({ mcpServersByCwd: { ...st.mcpServersByCwd, [cwd]: [] } }));
    }
  },

  createSchedule: async (input) => {
    const s = await api.createSchedule(input);
    set((st) => ({ schedules: [...st.schedules, s] }));
    return s;
  },

  updateSchedule: async (id, input) => {
    const s = await api.updateSchedule(id, input);
    set((st) => ({
      schedules: st.schedules.map((sc) => (sc.id === id ? s : sc)),
    }));
    return s;
  },

  deleteSchedule: async (id) => {
    // Optimistic remove — otherwise the row sits visible while the
    // DELETE is in flight, the user assumes the click missed, clicks
    // again, and the second request races behind the first and 404s
    // with "schedule not found".
    const prev = get().schedules;
    set((st) => ({ schedules: st.schedules.filter((sc) => sc.id !== id) }));
    try {
      await api.deleteSchedule(id);
    } catch (e) {
      // 404 means the schedule is already gone — that's the outcome we
      // wanted, so don't treat it as an error. Only roll back for real
      // failures (network, 5xx, etc.).
      const msg = String((e as Error).message || '');
      if (/not found/i.test(msg)) return;
      set({ schedules: prev });
      throw e;
    }
  },

  setScheduleEnabled: async (id, enabled) => {
    const s = await api.setScheduleEnabled(id, enabled);
    set((st) => ({
      schedules: st.schedules.map((sc) => (sc.id === id ? s : sc)),
    }));
  },

  fireSchedule: async (id) => {
    const s = await api.fireSchedule(id);
    set((st) => ({
      schedules: st.schedules.map((sc) => (sc.id === id ? s : sc)),
    }));
  },

  snoozeSchedule: async (id) => {
    const s = await api.snoozeSchedule(id);
    set((st) => ({
      schedules: st.schedules.map((sc) => (sc.id === id ? s : sc)),
    }));
  },

  createPane: async (opts) => {
    const s0 = get();
    const origin = autosaveSnapshotFromState(s0);
    const target = s0.focusedPaneId;
    const p = await api.createPane({
      ...opts,
      ...paneWorkspaceClaim(s0, target ? 'horizontal' : null, target),
    });
    if (origin && get().activeWorkspace !== origin.name) {
      await persistCreatedPaneToOriginWorkspace(
        get,
        origin,
        p,
        target ? 'horizontal' : null,
        target,
      );
      return p;
    }
    set((s) => ({
      panes: { ...s.panes, [p.id]: p },
      layout: insertPaneOrAppend(s.layout, p.id, s.focusedPaneId ? 'horizontal' : null, s.focusedPaneId),
      focusedPaneId: p.id,
      lastUserFocusId: p.id, // deliberate spawn — grab the caret for the new pane
      // Clear zoom so the new pane is visible, not focused-but-hidden.
      zoomedPaneId: null,
      lastViewedMs: { ...s.lastViewedMs, [p.id]: Date.now() },
    }));
    scheduleAutosave(get);
    await flushAutosave(get, undefined, { throwOnError: true });
    return p;
  },

  renamePane: async (id, title) => {
    const prev = get().panes[id];
    if (!prev) return null;
    const trimmed = title.trim();
    const nextTitle = trimmed || prev.cmd[0] || '';
    if (nextTitle === prev.title) return prev;

    const seq = (paneRenameSeq.get(id) ?? 0) + 1;
    paneRenameSeq.set(id, seq);
    paneRenamePending.set(id, nextTitle);

    set((s) => ({
      panes: s.panes[id] ? { ...s.panes, [id]: { ...s.panes[id], title: nextTitle } } : s.panes,
    }));
    scheduleAutosave(get);

    try {
      const saved = await api.renamePane(id, trimmed);
      if (paneRenameSeq.get(id) === seq) {
        paneRenamePending.delete(id);
        set((s) => ({
          panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes,
        }));
        scheduleAutosave(get);
      }
      return saved;
    } catch (e) {
      if (paneRenameSeq.get(id) === seq) {
        paneRenamePending.delete(id);
        set((s) => ({
          panes: s.panes[id] ? { ...s.panes, [id]: prev } : s.panes,
        }));
        scheduleAutosave(get);
      }
      throw e;
    }
  },

  setPaneIdentity: async (id, change) => {
    const { identity } = await api.setPaneIdentity(id, change);
    set((s) => ({
      panes: s.panes[id] ? { ...s.panes, [id]: { ...s.panes[id], identity: identity ?? undefined } } : s.panes,
    }));
    scheduleAutosave(get);
  },

  switchPaneMode: async (id, kind, opts) => {
    const scope=currentRoutingScope(),generation=get().navigationGeneration;
    const current=()=>scope.key === currentRoutingScope().key && generation === get().navigationGeneration;
    const prev = get().panes[id];
    if (!prev) return null;

    // --- cancel: disarm a previously-armed deferred switch ---
    // Optimistically null pendingSwitch (so the indicator clears at once), POST
    // with cancel:true, then reconcile with the returned DTO. No kind flip, no
    // paneModeSwitching.
    if (opts?.cancel) {
      set((s) => ({
        panes: s.panes[id] ? { ...s.panes, [id]: { ...s.panes[id], pendingSwitch: null } } : s.panes,
      }));
      try {
        const saved = await api.switchPaneMode(id, kind, { cancel: true },scope);
        if(!current())return null;
        set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes }));
        return saved;
      } catch (e) {
        if(!current())return null;
        // Restore the armed indicator so the user can retry the cancel.
        set((s) => ({
          panes: s.panes[id]
            ? { ...s.panes, [id]: { ...s.panes[id], pendingSwitch: prev.pendingSwitch ?? null } }
            : s.panes,
        }));
        get().toast(
          `Couldn't cancel the pending switch: ${(e as Error).message || 'request failed'}`,
          { kind: 'error', icon: '⚠', ttlMs: 5000 },
        );
        throw e;
      }
    }

    // --- deferred: arm a lossless switch that fires when the turn finishes ---
    // Do NOT optimistically flip kind and do NOT set paneModeSwitching — the
    // body stays put; we only POST and merge the DTO (which carries
    // pendingSwitch). The current renderer keeps rendering until the daemon
    // fires the switch on turn-end (propagated via the panes SSE/poll).
    if (opts?.when === 'deferred') {
      try {
        const saved = await api.switchPaneMode(id, kind, { when: 'deferred' },scope);
        if(!current())return null;
        set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes }));
        return saved;
      } catch (e) {
        if(!current())return null;
        get().toast(
          `Couldn't arm the deferred switch: ${(e as Error).message || 'request failed'}`,
          { kind: 'error', icon: '⚠', ttlMs: 5000 },
        );
        throw e;
      }
    }

    // --- now (or omitted): interrupt-and-switch immediately ---
    const prevKind = prev.kind ?? '';
    if (prevKind === kind) return prev;
    // Guard against a double-fire (palette + header racing the same pane):
    // if a switch is already pending for this pane, no-op.
    if (get().paneModeSwitching[id]) return prev;
    // The pre-switch agent pid. A respawn (either direction) lands a NEW pid —
    // the authoritative "the switch actually happened" signal (the optimistic
    // kind flip below changes kind but NOT pid, so kind alone can't tell a real
    // respawn from a no-op failure).
    const prevPid = prev.pid;

    // Optimistic: swap the body immediately. The render dispatch
    // (WarmPaneLayer / PaneGrid) is gated on pane.kind==='native', so patching
    // the field here flips Terminal↔Native without waiting for the round-trip.
    // Also flag the switch as in flight so the header toggle can show a
    // disabled/spinner state during the daemon kill+respawn grace. An override
    // ("switch now" on an armed pending switch) also clears pendingSwitch
    // optimistically so the indicator collapses straight into the flip.
    set((s) => ({
      panes: s.panes[id]
        ? { ...s.panes, [id]: { ...s.panes[id], kind, pendingSwitch: null } }
        : s.panes,
      paneModeSwitching: { ...s.paneModeSwitching, [id]: true },
    }));

    // Do NOT let the switch-mode POST alone drive the overlay or the
    // success/failure verdict. Killing + respawning THIS pane's agent can lose
    // or badly delay the POST's own response even though the switch landed and
    // persisted server-side — which previously stranded the "Switching
    // renderer…" overlay (and, with the 30s fetch cap, falsely rolled the
    // renderer back to Terminal with an error toast on a switch that actually
    // succeeded). Instead reconcile against the AUTHORITATIVE pane state: a
    // respawn lands a NEW pid (the panes poll/SSE propagates it), and the
    // authoritative kind tells success (===target) from a rolled-back respawn
    // (===prevKind). The POST is only a fast-path hint; a bounded timeout is the
    // final backstop so a lost response can never strand the overlay.
    const post = api
      .switchPaneMode(id, kind, opts?.when ? { when: opts.when } : undefined,scope)
      .then((saved) => ({ ok: true as const, saved }))
      .catch((error: unknown) => ({ ok: false as const, error }));
    const respawnP = waitForPaneRespawn(id, prevPid, SWITCH_RECONCILE_TIMEOUT_MS);

    try {
      const outcome = await Promise.race([
        post.then((r) => ({ via: 'post' as const, r })),
        respawnP.then((landed) => ({ via: 'respawn' as const, landed })),
      ]);

      if(!current())return null;
      // Fast path (unchanged behaviour for an idle pane): the POST returned its
      // DTO first — merge and return.
      if (outcome.via === 'post' && outcome.r.ok) {
        // Hoist out of the union before the closure — TS drops the discriminated
        // narrowing of `outcome.r` inside the nested set() callback.
        const saved = outcome.r.saved;
        if(!current())return null;
        set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes }));
        return saved;
      }

      // Either the POST errored, or a respawn was observed before any response
      // (lost/slow response). Judge by the authoritative pane state — a NEW pid
      // means the switch really happened; the authoritative kind says success
      // vs a rolled-back respawn. In the post-error case, wait once more so a
      // 500's rollback respawn has a chance to land (reuses the same promise —
      // no extra subscription).
      const landed = outcome.via === 'respawn' ? outcome.landed : await respawnP;
      if(!current())return null;
      const live = get().panes[id];
      if (landed && live && (live.kind ?? '') === kind) {
        // Respawned into the target kind → the switch landed despite a
        // lost/slow/errored response. Keep the optimistic flip.
        return live;
      }

      // No respawn into the target kind (daemon 500 → rollback to prevKind, or
      // no respawn at all): a genuine failure. Roll the body back and surface
      // it — the original failure contract.
      set((s) => ({
        panes: s.panes[id] ? { ...s.panes, [id]: { ...s.panes[id], kind: prevKind } } : s.panes,
      }));
      const errMsg =
        outcome.via === 'post' && !outcome.r.ok ? (outcome.r.error as Error)?.message : '';
      get().toast(`Couldn't switch renderer: ${errMsg || 'request failed'}`, {
        kind: 'error',
        icon: '⚠',
        ttlMs: 5000,
      });
      if (outcome.via === 'post' && !outcome.r.ok) throw outcome.r.error;
      throw new Error('switch-mode did not land');
    } finally {
      // Clear the in-flight flag regardless of outcome. Drop the key entirely
      // (rather than set false) so the map stays small and selectors that
      // read `!!paneModeSwitching[id]` see a clean false.
      if(current())set((s) => {
        if (!(id in s.paneModeSwitching)) return {};
        const next = { ...s.paneModeSwitching };
        delete next[id];
        return { paneModeSwitching: next };
      });
    }
  },

  setPaneFlag: async (id, flagged, memo) => {
    const prev = get().panes[id];
    if (!prev) return null;
    const seq = (paneFlagSeq.get(id) ?? 0) + 1;
    paneFlagSeq.set(id, seq);
    const optimistic: PaneInfo = {
      ...prev,
      flagged,
      flagMemo: flagged ? memo.trim() : '',
    };

    set((s) => ({
      panes: s.panes[id] ? { ...s.panes, [id]: { ...s.panes[id], ...optimistic } } : s.panes,
    }));
    scheduleAutosave(get);

    try {
      const saved = await api.setPaneFlag(id, flagged, memo);
      if (paneFlagSeq.get(id) === seq) {
        set((s) => ({
          panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes,
        }));
        scheduleAutosave(get);
      }
      return saved;
    } catch (e) {
      if (paneFlagSeq.get(id) === seq) {
        set((s) => ({
          panes: s.panes[id] ? { ...s.panes, [id]: prev } : s.panes,
        }));
        scheduleAutosave(get);
      }
      throw e;
    }
  },

  setPaneAutoMode: async (id, mode) => {
    const prev = get().panes[id];
    if (!prev) return null;
    if ((prev.autoMode ?? '') === mode) return prev;
    const seq = (paneAutoModeSeq.get(id) ?? 0) + 1;
    paneAutoModeSeq.set(id, seq);

    // Optimistic: flip the badge immediately so click→change feels instant, and
    // rapid cycling reads the just-applied mode rather than a ~3s-stale one.
    set((s) => ({
      panes: s.panes[id] ? { ...s.panes, [id]: { ...s.panes[id], autoMode: mode } } : s.panes,
    }));

    try {
      const saved = await api.setPaneAutoMode(id, mode);
      if (paneAutoModeSeq.get(id) === seq) {
        set((s) => ({
          panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes,
        }));
      }
      return saved;
    } catch (e) {
      // Roll back to the prior mode (only if no newer click superseded us).
      if (paneAutoModeSeq.get(id) === seq) {
        set((s) => ({
          panes: s.panes[id] ? { ...s.panes, [id]: prev } : s.panes,
        }));
      }
      get().toast(
        `Couldn't set auto-mode: ${(e as Error).message || 'request failed'}`,
        { kind: 'error', icon: '⚠', ttlMs: 5000 },
      );
      throw e;
    }
  },

  setPaneAnnounceDelegation: async (id, announce) => {
    const prev = get().panes[id];
    if (!prev) return null;
    const seq = (paneAnnounceDelegationSeq.get(id) ?? 0) + 1;
    paneAnnounceDelegationSeq.set(id, seq);

    // Optimistic: the checkbox flips on click. `announce === null` clears the
    // override, and the daemon owns the resulting effective value — so the
    // optimistic effective value is only a guess for the explicit cases, and
    // the returned DTO replaces it either way.
    set((s) => (s.panes[id]
      ? {
        panes: {
          ...s.panes,
          [id]: {
            ...s.panes[id],
            announceDelegationEvents: announce ?? undefined,
            announceDelegation: announce ?? s.panes[id].announceDelegation,
          },
        },
      }
      : { panes: s.panes }));

    try {
      const saved = await api.setPaneAnnounceDelegation(id, announce);
      if (paneAnnounceDelegationSeq.get(id) === seq) {
        set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes }));
      }
      return saved;
    } catch (e) {
      if (paneAnnounceDelegationSeq.get(id) === seq) {
        set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: prev } : s.panes }));
      }
      get().toast(
        `Couldn't change delegation announcements: ${(e as Error).message || 'request failed'}`,
        { kind: 'error', icon: '⚠', ttlMs: 5000 },
      );
      throw e;
    }
  },

  resumePane: async (id, cols, rows, cwd) => {
    const scope=currentRoutingScope(),generation=get().navigationGeneration;
    const current=()=>scope.key === currentRoutingScope().key && generation === get().navigationGeneration;
    const stored = get().panes[id];
    const prev = stored && cwd && stored.recoveryError ? { ...stored, cwd } : stored;
    if (!prev) return null;
    if (prev.restorePending) {
      try {
        if (!prev.cmd.length && !(prev.codexThreadId || prev.claudeSessionId || prev.museSessionId)) {
          throw new Error('No launch recipe was saved for this pane.');
        }
        const saved = await api.createPane({ ...paneRetryRequest(prev), cols: cols ?? prev.cols, rows: rows ?? prev.rows }, scope);
        if (!current()) return null;
        set((s) => {
          const panes = { ...s.panes }; delete panes[id]; panes[saved.id] = saved;
          const remap = new Map([[id, saved.id]]);
          return {
            panes, layout: s.layout ? remapLayoutIds(s.layout, remap) : s.layout,
            focusedPaneId: s.focusedPaneId === id ? saved.id : s.focusedPaneId,
            workspaces: s.workspaces.map((w) => ({ ...w,
              layout: w.layout ? remapLayoutIds(w.layout, remap) : w.layout,
              paneIds: w.paneIds.map((pid) => pid === id ? saved.id : pid),
              panes: w.panes?.map((d) => d.id === id ? collectPaneDefs({ type: 'pane', paneId: saved.id }, { [saved.id]: saved })[0] : d),
            })),
          };
        });
        scheduleAutosave(get);
        return saved;
      } catch (e) {
        set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: { ...prev, recoveryError: (e as Error).message } } : s.panes }));
        get().toast(`Couldn't restore pane: ${(e as Error).message}`, { kind: 'error', ttlMs: 5000 });
        throw e;
      }
    }
    // Optimistic: flip to running + clear resumable + exitCode so the
    // Session-ended card / terminal overlay swaps to a connecting state and the
    // Resume button disables immediately, without waiting on the round-trip.
    set((s) => ({
      panes: s.panes[id]
        ? { ...s.panes, [id]: { ...s.panes[id], status: 'running', resumable: false, exitCode: undefined } }
        : s.panes,
    }));
    panesResuming.add(id);
    try {
      const saved = await api.resumePane(id, cols, rows, scope, cwd);
      if(!current())return null;
      // The DTO carries the NEW pid (same pane id) — merging it triggers the
      // terminal respawn-reconnect backstop (Pane.tsx, keyed on pane.pid) and
      // the native pid cache-reset (useNativePane, keyed on pid).
      set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes }));
      return saved;
    } catch (e) {
      if(!current())return null;
      // Restore the exited+resumable pane so the card returns and the user can retry.
      set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: { ...prev, recoveryError: prev.recoveryError ? (e as Error).message : undefined } } : s.panes }));
      get().toast(
        `Couldn't resume session: ${(e as Error).message || 'the session may have expired'}`,
        { kind: 'error', icon: '⚠', ttlMs: 5000 },
      );
      throw e;
    } finally {
      panesResuming.delete(id);
    }
  },

  refreshPane: async (id) => {
    const prev = get().panes[id];
    if (!prev) return null;
    try {
      const saved = await api.refreshPane(id);
      // New pid (same pane id) → triggers the native pid cache-reset + replay
      // (useNativePane, keyed on pid), so the conversation re-seeds from
      // the provider's resume path. Same merge the exited-pane resume uses.
      set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes }));
      get().toast('Agent restarted — conversation kept', { kind: 'info', icon: '↻', ttlMs: 2500 });
      return saved;
    } catch (e) {
      get().toast(
        `Couldn't restart agent: ${(e as Error).message || 'try again'}`,
        { kind: 'error', icon: '⚠', ttlMs: 5000 },
      );
      throw e;
    }
  },

  restartPane: async (id, fresh = false, force = false) => {
    if (!get().panes[id]) return null;
    // No toast here on failure: a 409+needsFresh is a user choice, not an
    // error, and restartPaneFlow owns the confirm + error copy.
    const saved = await api.restartPane(id, fresh, force);
    // New pid (same pane id) → triggers the terminal respawn-reconnect and the
    // native pid cache-reset/replay, exactly like resume/refresh.
    set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes }));
    get().toast(fresh ? 'Pane restarted — new session' : 'Pane restarted — conversation kept', { kind: 'info', icon: '↻', ttlMs: 2500 });
    return saved;
  },

  setPaneEffort: async (id, effort) => {
    const prev = get().panes[id];
    if (!prev) return null;
    if ((prev.effort ?? '') === effort) return prev; // no-op, daemon also short-circuits
    const seq = (paneEffortSeq.get(id) ?? 0) + 1;
    paneEffortSeq.set(id, seq);

    // Optimistic: show the new level immediately. The daemon RESPAWNS the
    // headless claude (new pid), so on success we merge the returned DTO — its
    // changed pid drives the native pid cache-reset + replay (useNativePane),
    // re-seeding the conversation from `claude --resume`, exactly like refresh.
    set((s) => ({
      panes: s.panes[id] ? { ...s.panes, [id]: { ...s.panes[id], effort } } : s.panes,
    }));

    try {
      const saved = await api.setPaneEffort(id, effort);
      // Only reconcile + toast if no newer change superseded this one, so a
      // rapid re-pick doesn't flash a stale level.
      if (paneEffortSeq.get(id) === seq) {
        set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes }));
        get().toast(
          effort ? `Effort set to ${effort} — conversation kept` : 'Effort reset to default — conversation kept',
          { kind: 'info', icon: '⚡', ttlMs: 2500 },
        );
      }
      return saved;
    } catch (e) {
      // Roll back the optimistic level (only if no newer change superseded us).
      if (paneEffortSeq.get(id) === seq) {
        set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: prev } : s.panes }));
      }
      get().toast(
        `Couldn't change effort: ${(e as Error).message || 'request failed'}`,
        { kind: 'error', icon: '⚠', ttlMs: 5000 },
      );
      throw e;
    }
  },

  setPaneContextPack: async (id, contextPack) => {
    const prev = get().panes[id];
    if (!prev) return null;
    if ((prev.contextPack ?? '') === contextPack.trim()) return prev;
    try {
      const saved = await api.setPaneContextPack(id, contextPack, prev.cols, prev.rows);
      set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes }));
      get().toast(
        contextPack.trim() ? 'Context pack attached — agent restarted' : 'Context pack detached — agent restarted',
        { kind: 'info', icon: '↻', ttlMs: 3000 },
      );
      return saved;
    } catch (e) {
      get().toast(`Couldn't change context pack: ${(e as Error).message}`, { kind: 'error', icon: '⚠', ttlMs: 5000 });
      throw e;
    }
  },

  setPaneRole: async (id, role) => {
    const prev = get().panes[id];
    if (!prev) return null;
    if ((prev.role ?? '') === role) return prev;

    try {
      const saved = await api.setPaneRole(id, role, prev.cols, prev.rows);
      // Do not show the identity badge until the daemon confirms the
      // brief-carrying respawn actually landed.
      set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes }));
      get().toast(
        role
          ? `Converted to orchestrator — ${agentLabel(effectivePaneAgent(prev))} restarted`
          : `Orchestrator role removed — ${agentLabel(effectivePaneAgent(prev))} restarted`,
        { kind: 'info', icon: '↻', ttlMs: 3000 },
      );
      return saved;
    } catch (e) {
      get().toast(
        `Couldn't change role: ${(e as Error).message || 'request failed'}`,
        { kind: 'error', icon: '⚠', ttlMs: 5000 },
      );
      return null;
    }
  },

  setPaneModel: async (id, model) => {
    const prev = get().panes[id];
    if (!prev) return null;
    // NO client-side no-op on equality: prev.model is OUR explicit flag, not the
    // session's effective model (a ''-flag pane runs whatever --resume stored),
    // so an equal-looking request can still be a real change. The daemon
    // short-circuits the true no-op (same explicit flag) cheaply.
    const seq = (paneModelSeq.get(id) ?? 0) + 1;
    paneModelSeq.set(id, seq);

    // Optimistic flag flip; on success merge the returned DTO — its changed pid
    // drives the native reconnect + `claude --resume` replay, like effort.
    set((s) => ({
      panes: s.panes[id] ? { ...s.panes, [id]: { ...s.panes[id], model } } : s.panes,
    }));

    try {
      const saved = await api.setPaneModel(id, model);
      if (paneModelSeq.get(id) === seq) {
        set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: saved } : s.panes }));
        get().toast(`Model → ${prettyModelName(model) || model} · conversation kept`, {
          kind: 'info',
          icon: '⚡',
          ttlMs: 3000,
        });
      }
      return saved;
    } catch (e) {
      if (paneModelSeq.get(id) === seq) {
        set((s) => ({ panes: s.panes[id] ? { ...s.panes, [id]: prev } : s.panes }));
      }
      get().toast(
        `Couldn't change model: ${(e as Error).message || 'request failed'}`,
        { kind: 'error', icon: '⚠', ttlMs: 5000 },
      );
      throw e;
    }
  },

  killPane: async (id, opts) => {
    // Optimistic: drop the pane from local state FIRST so the UI updates
    // instantly. The DELETE on the daemon can take up to 3s waiting for
    // the agent to exit gracefully (daemon/pane.go:706); we don't want
    // the user staring at a still-visible pane during that window.
    //
    // Snapshot the bits we mutate so we can roll back if the DELETE
    // fails. Pre-fix this method silently swallowed errors and the
    // 3s refreshPanes poll re-surfaced any survivors — making batch
    // kills look complete when they weren't.
    const scope=currentRoutingScope(),generation=get().navigationGeneration;
    const current=()=>scope.key === currentRoutingScope().key && generation === get().navigationGeneration;
    const before = get();
    const prevPane = before.panes[id];
    const promptKey = terminalSessionKey(scope, id);
    const prevPrompt = before.lastPrompts[promptKey];
    const prevLayout = before.layout;
    const prevFocus = before.focusedPaneId;
    const prevZoom = before.zoomedPaneId;
    const prevParked = before.parkedPaneIds;
    set((s) => {
      const { [id]: _removed, ...rest } = s.panes;
      const { [promptKey]: _prompt, ...restPrompts } = s.lastPrompts;
      const layout = removePane(s.layout, id);
      const remainingIds = [
        ...collectPaneIds(layout), ...collectBrowserIds(layout),
        ...collectFileIds(layout), ...collectDiffIds(layout), ...collectTasksIds(layout),
      ];
      const workspaces = removePaneFromWorkspaceCache(
        s.workspaces,
        s.activeWorkspace,
        s.activeBackend,
        id,
      );
      return {
        panes: rest,
        lastPrompts: restPrompts,
        layout,
        workspaces,
        focusedPaneId: s.focusedPaneId === id ? remainingIds[0] ?? null : s.focusedPaneId,
        // Drop zoom if the zoomed pane was the one killed.
        zoomedPaneId: s.zoomedPaneId === id ? null : s.zoomedPaneId,
        parkedPaneIds: s.parkedPaneIds.includes(id)
          ? s.parkedPaneIds.filter((p) => p !== id)
          : s.parkedPaneIds,
      };
    });
    // Prune this pane's last-known Focus vitals snapshot (a no-op for a
    // terminal pane / a pane that never had one). Pane ids are unique forever,
    // so without this the solu.nativeVitals.* localStorage keys would accumulate
    // unbounded over a long-lived install.
    clearVitalsSnapshot(id);
    scheduleAutosave(get);
    try {
      await api.deletePane(id,scope);
      const {forgetTerminalSession,sessionKey}=await import('./lib/terminalViewSessions');
      forgetTerminalSession(sessionKey(scope,id));
      if(!current())return true;
      if (prevPane) {
        const order = collectPaneIds(prevLayout);
        const at = order.indexOf(id);
        rememberClosedNeighbor(id, at >= 0 ? order[at - 1] ?? order[at + 1] : undefined);
        void get().refreshClosedPanes();
        if (!opts?.silent) {
          get().toast(`Closed ${closedPaneLabel({ id, title: prevPane.title || prevPane.cmd.join(' ') })}`, {
            kind: 'info',
            ttlMs: 8000,
            logToRecent: false,
            action: { label: 'Undo', run: () => { void get().reopenClosedPane(id); } },
          });
        }
      }
      await flushAutosave(get, undefined, { throwOnError: true });
      await get().refreshWorkspaces({ full: true });
      return true;
    } catch (e) {
      if(!current())return false;
      // Restore the pane so the user can see it survived and retry.
      // Only restore fields that previously held the pane — don't
      // clobber later edits in unrelated state branches.
      set((s) => {
        const lastPrompts = prevPrompt !== undefined
          ? { ...s.lastPrompts, [promptKey]: prevPrompt }
          : s.lastPrompts;
        return {
          panes: prevPane ? { ...s.panes, [id]: prevPane } : s.panes,
          lastPrompts,
          layout: prevLayout,
          focusedPaneId: prevFocus,
          zoomedPaneId: prevZoom,
          // Restore warm-parked membership too — without this a parked
          // pane whose DELETE failed survives on the daemon but gets
          // evicted from the warm set (WS closed, xterm disposed).
          parkedPaneIds: prevParked,
        };
      });
      scheduleAutosave(get);
      if (!opts?.silent) {
        get().toast(
          `Couldn't kill session: ${(e as Error).message || 'request failed'}`,
          { kind: 'error', icon: '⚠', ttlMs: 5000 },
        );
      }
      return false;
    }
  },

  recentlyClosedPanes: [],
  refreshClosedPanes: async () => {
    try {
      const { panes } = await api.closedPanes();
      set({ recentlyClosedPanes: panes ?? [] });
    } catch { /* older daemon without the endpoint: nothing to reopen */ }
  },
  reopenClosedPane: async (id) => {
    await get().refreshClosedPanes();
    const list = get().recentlyClosedPanes;
    const entry = id ? list.find((c) => c.id === id) : list[0];
    if (!entry) {
      get().toast('No recently closed panes', { kind: 'info', ttlMs: 2500 });
      return;
    }
    // Land next to the old neighbour when it is still on screen.
    const s0 = get();
    const neighbor = closedNeighbor(entry.id);
    const anchor = neighbor && collectPaneIds(s0.layout).includes(neighbor) ? neighbor : s0.focusedPaneId;
    const dims = anchor ? s0.panes[anchor] : undefined;
    try {
      const p = await api.reopenClosedPane(entry.id, dims?.cols, dims?.rows);
      set((s) => ({
        panes: { ...s.panes, [p.id]: p },
        layout: insertPaneOrAppend(s.layout, p.id, anchor ? 'horizontal' : null, anchor),
        focusedPaneId: p.id,
        lastUserFocusId: p.id,
        zoomedPaneId: null,
        lastViewedMs: { ...s.lastViewedMs, [p.id]: Date.now() },
        recentlyClosedPanes: s.recentlyClosedPanes.filter((c) => c.id !== entry.id),
      }));
    } catch (e) {
      void get().refreshClosedPanes();
      get().toast(`Couldn't reopen ${closedPaneLabel(entry)}: ${(e as Error).message || 'request failed'}`, { kind: 'error', ttlMs: 5000 });
      return;
    }
    // The pane exists now; a layout-save hiccup must not read as a failed reopen.
    scheduleAutosave(get);
    await flushAutosave(get).catch(() => {});
  },

  setLayout: (l) => {
    // Diagnostic log removed in v0.0.187. Every splitter-drag frame
    // fires onLayout → setLayout (via PaneGrid), so a one-second
    // stress-drag of a nested layout pumps dozens of these per
    // frame — the log itself was the only noticeable symptom and
    // it made the browser console unusable. The underlying calls
    // are fine: the 400 ms autosave debounce keeps the daemon
    // write rate bounded, and the store updates themselves are
    // cheap. Restore the log temporarily if debugging a layout-
    // persistence regression.
    set({ layout: l });
    scheduleAutosave(get);
  },

  setFocus: (id) => set((s) => s.mainPresentation.kind === 'owner' ? {} : ({
    focusedPaneId: id,
    // Deliberate user focus change — stamp lastUserFocusId so the target pane's
    // focus effect routes DOM focus even if the user was mid-typing in the pane
    // they're navigating away from (keyboard nav / click must move the caret).
    lastUserFocusId: id,
    lastViewedMs: id ? { ...s.lastViewedMs, [id]: Date.now() } : s.lastViewedMs,
    // Zoom follows focus: if the user jumps to pane 3 via Alt+3 while
    // zoomed on pane 1, they expect to see pane 3 (not pane 1 with
    // invisible focus on 3). Only update if zoom was already active —
    // don't implicitly zoom when not zoomed.
    zoomedPaneId: s.zoomedPaneId && id ? id : s.zoomedPaneId,
  })),

  // One-shot: clear the deliberate-selection marker once a pane's focus effect
  // has used it to grab the caret. Guarded so it's a no-op (no extra render) when
  // already null. After this, only a fresh setFocus/spawn re-arms a caret grab —
  // reconnect re-renders fall through to the safe-by-default isTypingElsewhere gate.
  consumeUserFocus: () => set((s) => (s.lastUserFocusId !== null ? { lastUserFocusId: null } : {})),

  splitFocused: async (direction, opts) => {
    const s = get();
    const origin = autosaveSnapshotFromState(s);
    const target = s.focusedPaneId;
    const p = await api.createPane({
      ...opts,
      ...paneWorkspaceClaim(s, target ? direction : null, target),
    });
    if (origin && get().activeWorkspace !== origin.name) {
      await persistCreatedPaneToOriginWorkspace(get, origin, p, target ? direction : null, target);
      return;
    }
    set((st) => ({
      panes: { ...st.panes, [p.id]: p },
      layout: insertPaneOrAppend(st.layout, p.id, target ? direction : null, target),
      focusedPaneId: p.id,
      lastUserFocusId: p.id, // deliberate split — grab the caret for the new pane
      // Clear zoom so the freshly-split pane is visible, not focused-but-
      // hidden behind the zoomed pane.
      zoomedPaneId: null,
    }));
    scheduleAutosave(get);
    await flushAutosave(get, undefined, { throwOnError: true });
  },

  launchClaudeHere: async (paneId, direction = 'horizontal') => {
    await launchCmdHere(get, set, paneId, direction, { cmd: ['claude'], title: 'claude' });
  },
  launchShellHere: async (paneId, direction = 'horizontal') => {
    // Empty cmd tells the daemon to use $SHELL. Title defaults to 'shell'
    // (the user can rename via double-click).
    await launchCmdHere(get, set, paneId, direction, { cmd: [], title: 'shell' });
  },

  closePaneInLayout: (paneId) => {
    set((s) => {
      const layout = removePane(s.layout, paneId);
      const remainingIds = layout ? [
        ...collectPaneIds(layout), ...collectBrowserIds(layout),
        ...collectFileIds(layout), ...collectDiffIds(layout), ...collectTasksIds(layout),
      ] : [];
      return {
        layout,
        focusedPaneId: s.focusedPaneId === paneId ? remainingIds[0] ?? null : s.focusedPaneId,
        zoomedPaneId: s.zoomedPaneId === paneId ? null : s.zoomedPaneId,
        lastUserFocusId: s.lastUserFocusId === paneId ? null : s.lastUserFocusId,
      };
    });
    scheduleAutosave(get);
  },

  movePaneInLayout: (source, target, zone) => {
    set((s) => {
      if (!s.layout) return {};
      const next = movePaneInLayoutImpl(s.layout, source, target, zone);
      return { layout: next, focusedPaneId: source, lastUserFocusId: source };
    });
    scheduleAutosave(get);
  },

  movePaneToWorkspace: async (paneId, destination, requestedScope) => {
    const before = get();
    const source = before.activeWorkspace;
    const scope = requestedScope ?? currentRoutingScope();
    const generation = before.navigationGeneration;
    const pane = before.panes[paneId];
    const valid = () => scope.key === currentRoutingScope().key
      && get().navigationGeneration === generation && get().activeWorkspace === source
      && get().panes[paneId]?.pid === pane?.pid;
    if (!valid()) return;
    if (!source || source === CURRENT_WS) {
      before.toast('Save this pane in a named workspace before moving it.', { kind: 'warn' });
      return;
    }
    if (source === destination) return;
    if (!pane || pane.status !== 'running' || pane.role === 'task_owner' || before.managedOwnerBindings.has(paneId)
      || before.mainPresentation.kind !== 'workspace' || !collectPaneIds(before.layout).includes(paneId)) return;
    if (!before.workspaces.some((w) => w.name === destination && (!before.activeBackend || !w.backend || w.backend === before.activeBackend)) || destination === CURRENT_WS) {
      before.toast('The destination workspace is not available on this backend.', { kind: 'error' });
      return;
    }
    let acquired = false;
    let moved = false;
    let releasePlacement = () => {};
    try {
      // Drain saves already in flight, then persist a final source snapshot.
      // A durable version guard also rejects delayed writes from other views.
      await flushAutosave(get, undefined, { throwOnError: true });
      await Promise.all([...workspaceSaves]);
      if (!valid()) return;
      if (checkOwnedByOther(destination, windowLabel()) || workspaceMovedTo(destination)) {
        let result = await acquireWorkspaceView(destination, windowLabel(), false, valid);
        if (result === 'visible') {
          const confirmed = await confirmDialog({
            title: 'Workspace is open in another window',
            message: `Move "${destination}" here along with this pane? Its terminals and agents keep running.`,
            confirmLabel: 'Move here',
          });
          if (!confirmed || !valid()) return;
          result = await acquireWorkspaceView(destination, windowLabel(), true, valid);
        }
        if (result !== 'acquired') throw new Error('The destination window could not hand off its workspace.');
        acquired = true;
      }
      if (!valid()) return;
      cancelAutosave();
      const snapshot = autosaveSnapshotFromState(get());
      if (!snapshot) return;
      await saveWorkspaceSnapshot(get, snapshot, { refreshWorkspaces: false });
      if (!valid()) return;
      const result = await api.movePaneToWorkspace(source, paneId, destination, before.activeWorkspaceMoveVersion, scope);
      moved = true;
      if (scope.key !== currentRoutingScope().key) return;
      get().mergeSavedWorkspaceIntoCache(result.source);
      get().mergeSavedWorkspaceIntoCache(result.destination);
      // Only touch this view if the user has stayed here while the request ran.
      if (!valid()) {
        get().toast(`Moved pane to "${destination}"`, { kind: 'success', paneId });
        return;
      }
      const current = get();
      const remaining = removePane(current.layout, paneId);
      releasePlacement = holdTerminalPlacement(scope, paneId);
      set({
        layout: remaining,
        activeWorkspaceMoveVersion: result.source.paneMoveVersion ?? 0,
        focusedPaneId: current.focusedPaneId === paneId ? collectPaneIds(remaining)[0] ?? null : current.focusedPaneId,
        zoomedPaneId: current.zoomedPaneId === paneId ? null : current.zoomedPaneId,
        lastUserFocusId: current.lastUserFocusId === paneId ? null : current.lastUserFocusId,
      });
      // A layout edit made during the request belongs to the source. Let
      // loadWorkspace flush it with the updated version before navigating.
      if (remaining) scheduleAutosave(get);
      else cancelAutosave();
      await get().loadWorkspace(destination);
      if (scope.key !== currentRoutingScope().key) return;
      if (get().activeWorkspace === destination && collectPaneIds(get().layout).includes(paneId)) {
        get().setFocus(paneId);
        // Keep an explicit workspace deep link consistent across reloads.
        const url = new URL(window.location.href);
        if (url.searchParams.has('ws')) {
          url.searchParams.set('ws', destination);
          window.history.replaceState(null, '', url.toString());
        }
      }
      get().toast(`Moved pane to "${destination}"`, { kind: 'success', paneId });
    } catch (error) {
      if (scope.key === currentRoutingScope().key) get().toast(moved
        ? `Pane moved to "${destination}". Open that workspace to continue.`
        : `Couldn't move pane: ${String(error)}`, { kind: moved ? 'info' : 'error' });
    } finally {
      releasePlacement();
      if (acquired && (scope.key !== currentRoutingScope().key || get().activeWorkspace !== destination)) releaseWorkspace(destination, windowLabel());
    }
  },

  saveWorkspace: async (name) => {
    const s = get();
    if (name === s.activeWorkspace && workspaceMovedTo(name)) {
      get().toast('Move the workspace here before saving it.', { kind: 'info' });
      return;
    }
    const scope=currentRoutingScope(), generation=s.navigationGeneration;
    const w: Workspace = {
      name,
      paneMoveVersion: name === s.activeWorkspace ? s.activeWorkspaceMoveVersion : pickWorkspaceByName(s.workspaces, name, s.activeBackend)?.paneMoveVersion,
      layout: s.layout,
      paneIds: collectPaneIds(s.layout),
      panes: collectPaneDefs(s.layout, s.panes),
      browsers: collectBrowserInfos(s.layout, s.browsers),
      files: collectFileInfos(s.layout, s.files),
      activeAt: new Date().toISOString(),
    };
    await trackWorkspaceSave(api.saveWorkspace(w,scope));
    if(scope.key !== currentRoutingScope().key || generation !== get().navigationGeneration)return;
    // Make this the active workspace so subsequent autosaves land here
    // (and not in _current, which is how Save as… used to get "stuck").
    set({ activeWorkspace: name, activeWorkspaceMoveVersion: w.paneMoveVersion ?? 0 });
    await get().refreshWorkspaces({ full: true });
    get().toast(`Saved workspace "${name}"`, { kind: 'success', icon: '✓' });
  },

  loadWorkspace: async (name, takeOver = false) => {
    const wasOwner = get().mainPresentation.kind === 'owner';
    if (wasOwner) {clearOwnerURL();get().returnToWorkspace();}
    if (wasOwner && get().activeWorkspace === name) return;
    const generation = get().navigationGeneration + 1;
    set({ navigationGeneration: generation });
    const scope = currentRoutingScope();
    const navigationCurrent = () => generation === get().navigationGeneration && scope.key === currentRoutingScope().key;
    const startedAt = perfNow();
    let handoffAcquired = false;
    const releaseUnusedHandoff = () => {
      if (handoffAcquired && (get().activeWorkspace !== name || workspaceMovedTo(name))) {
        releaseWorkspace(name, windowLabel());
      }
    };
    if (name !== CURRENT_WS && (checkOwnedByOther(name, windowLabel()) || workspaceMovedTo(name))) {
      try {
        await flushAutosave(get, undefined, { throwOnError: true });
        await Promise.all([...workspaceSaves]);
        if (!navigationCurrent()) { releaseUnusedHandoff(); return; }
        let result = await acquireWorkspaceView(name, windowLabel(), takeOver, navigationCurrent);
        if (result === 'visible' && !takeOver) {
          const confirmed = await confirmDialog({
            title: 'Workspace is open in another window',
            message: `Move "${name}" here? Its terminals and agents will keep running.`,
            confirmLabel: 'Move here',
          });
          if (!confirmed || !navigationCurrent()) return;
          await flushAutosave(get, undefined, { throwOnError: true });
          await Promise.all([...workspaceSaves]);
          result = await acquireWorkspaceView(name, windowLabel(), true, navigationCurrent);
        }
        if (result !== 'acquired') {
          get().toast(result === 'save-failed'
            ? 'The other window could not save its workspace. Try again after saving there.'
            : 'The other window could not hand off this workspace. Refresh it and try again.', { kind: 'error' });
          return;
        }
        handoffAcquired = true;
        await get().refreshWorkspaces({ full: true });
        if (!navigationCurrent()) { releaseUnusedHandoff(); return; }
      } catch (error) {
        releaseUnusedHandoff();
        get().toast(`Couldn't move workspace: ${String(error)}`, { kind: 'error' });
        return;
      }
    }
    // Find the target workspace using CACHED state — avoids a round-trip
    // before the UI can react. Workspaces are also polled every 3s so the
    // cache is rarely more than a few seconds stale.
    let state0 = get();
    let w = pickWorkspaceByName(state0.workspaces, name, state0.activeBackend);
    let upgradedSummary = false;
    if (w?.summary) {
      upgradedSummary = true;
      try {
        await get().refreshWorkspaces({ full: true });
      } catch (e) {
        if (!navigationCurrent()) { releaseUnusedHandoff(); return; }
        // The full-workspace fetch stalled/timed-out (e.g. a relay hiccup). We
        // only hold the summary, so we can't safely resolve the layout — bail
        // with feedback instead of throwing out of the switch and leaving the
        // user wedged. The 3s poll / a retry will have the full data.
        releaseUnusedHandoff();
        lifecycleLog('store.loadWorkspace.summaryUpgradeFailed', { to: name, err: String(e) });
        get().toast(`Couldn't load workspace "${name}" — try again`, { kind: 'error' });
        return;
      }
      if (!navigationCurrent()) { releaseUnusedHandoff(); return; }
      state0 = get();
      w = pickWorkspaceByName(state0.workspaces, name, state0.activeBackend);
    }
    if (!w) { releaseUnusedHandoff(); return; }
    lifecycleLog('store.loadWorkspace.start', {
      from: state0.activeWorkspace,
      to: name,
      upgradedSummary,
      outgoingIds: lifecycleLayoutIds(state0.layout),
      targetIds: lifecycleLayoutIds(w.layout ?? null),
      targetPaneDefs: w.panes?.map((p) => ({ id: p.id, cmd0: p.cmd?.[0] ?? '' })) ?? [],
      visibility: typeof document !== 'undefined' ? document.visibilityState : 'unknown',
    });

    // Pin any pending debounced save to the OUTGOING workspace before we
    // flip activeWorkspace. Without this, a resize made <400ms before a
    // workspace switch would flush against the incoming workspace's name
    // (and incoming layout), silently discarding the resize.
    const outgoingSnapshot: AutosaveSnapshot | undefined = state0.activeWorkspace
      ? {
          name: state0.activeWorkspace,
          paneMoveVersion: state0.activeWorkspaceMoveVersion,
          layout: state0.layout,
          panes: state0.panes,
          browsers: state0.browsers,
          files: state0.files,
        }
      : undefined;

    // Flush the pending autosave NOW, before we flip activeWorkspace.
    // Otherwise the queued 400ms timer can fire during the blank-phase
    // 200ms wait below, see the just-flipped activeWorkspace + layout=null
    // state, and POST {paneIds:[], layout:null} under the DESTINATION
    // workspace — orphaning every pane the destination owned into the
    // Background sessions list. Same class as v0.0.307's deleteWorkspace
    // fix; that one cancelled+pinned, this one does the same on switch.
    // Real-world hit on 2026-04-27: 6 panes lost from one workspace via
    // "Kill all background" after the orphan side-effect.
    //
    // flushAutosave cancels the armed timer synchronously and pins the
    // snapshot before its first await, so it's safe to start it here and
    // overlap the POST with the listPanes fetch below instead of paying
    // the two round-trips back to back. It swallows its own errors.
    const flushPromise = flushAutosave(get, outgoingSnapshot, { refreshWorkspaces: false });

    // Browser and file tiles are client-side — any browserId/fileId in
    // the saved workspace is "alive" as long as we have its info record.
    const browserMap: Record<string, BrowserInfo> = {};
    if (w.browsers) {
      for (const b of w.browsers) browserMap[b.id] = bindSavedBrowser(b,scope.prefix);
    }
    const fileMap: Record<string, FileTileInfo> = {};
    if (w.files) {
      for (const f of w.files) fileMap[f.id] = f;
    }

    // Phase 1: optimistic UI update. Start with cached panes, then
    // normally make one fresh live-pane pass before pruning the target
    // layout. In cached-first mode (default), non-Codex workspace
    // switches skip this blocking fresh pass and merge it in Phase 2
    // instead. Codex panes USED to force the conservative blocking path for
    // reattach stability — but that stability now lives in the warm-pane layer
    // (the codex pane's WS + xterm survive the switch, so a switch-back is a
    // pure DOM move, no replay) and the daemon-side live-repaint fix, NOT in a
    // pre-switch listPanes. So codex now goes cached-first too: blocking the
    // whole switch on a relay listPanes was the entire reason codex workspaces
    // (e.g. "Aria") took ~seconds to switch while others were instant.
    // touchesCodex is still used to gate the blank phase below; only an explicit
    // opt-out (solu.cachedFirstWorkspaceSwitch='0') now takes the blocking path.
    const touchesCodex = workspaceSwitchTouchesCodex(state0.layout, w.layout, state0.panes, w.panes);
    const cachedFirstSafe = isCachedFirstWorkspaceSwitchEnabled();
    const aliveById = mergePaneInfos(state0.panes, []);
    // Optimistically seed the destination's saved panes (as running) for any id
    // the carried-over global cache doesn't yet list. The cache is only
    // globalized by the 3s refreshPanes poll, so switching to a workspace whose
    // panes were just created/resumed (e.g. a workspace's slow /mnt/c
    // claude-resume panes) would otherwise have resolveWorkspaceInitialLayout
    // prune the WHOLE layout → empty grid → null focus → new-pane cwd detection
    // fails. Seeding keeps the layout intact and the focus set WITHOUT a blocking
    // listPanes round-trip on the switch (a previous fix forced a blocking fetch
    // here, which stalled over the cloud relay and made switches lag/hang).
    // Phase 2's fresh listPanes overwrites these seeds with real data, and any
    // seed the fresh list does NOT confirm is dropped so the respawn loop
    // recreates it. This now covers codex panes too (see cachedFirstSafe above).
    const seededIds = new Set<string>();
    const seedSavedPaneDefs = () => {
      if (!w.panes) return;
      for (const def of w.panes) {
        if (def.role === 'task_owner' || get().managedOwnerBindings.has(def.id)) continue;
        if (!aliveById.has(def.id)) {
          aliveById.set(def.id, optimisticPaneFromDef(def));
          seededIds.add(def.id);
        }
      }
    };
    if (cachedFirstSafe) {
      seedSavedPaneDefs();
    } else {
      // Explicit opt-out only (solu.cachedFirstWorkspaceSwitch='0'): the
      // conservative blocking fresh fetch before resolving the layout, bounded
      // by the global apiFetch timeout. The default path — including codex —
      // no longer comes here; warm panes + the daemon repaint fix + Phase 2's
      // reconcile cover codex reattach stability without blocking the switch.
      try {
        const freshPanes = isRouterMode() ? await api.listPanesAllBackends() : await api.listPanes();
        for (const p of freshPanes) if (!p.backend || `/b/${p.backend}` === scope.backendPrefix) aliveById.set(p.id, p);
      } catch {
        // Keep cached data — a failed refresh must not empty the target.
      }
    }
    // The autosave POST (flushPromise) is intentionally NOT awaited. flushAutosave
    // cancels the timer and pins the outgoing snapshot SYNCHRONOUSLY before its
    // first await, and swallows its own errors — so the switch needs nothing from
    // the POST landing. Awaiting it put an unbounded relay POST on the switch's
    // critical path: exactly the stall that wedged switching FOREVER whenever ARIA
    // kept the autosave timer armed. Let it complete (or time out) in the
    // background; mergeSavedWorkspaceIntoCache reconciles the outgoing cache when
    // it lands.
    void flushPromise.catch(() => {});
    let initialLayout = detachManagedOwnerPresentation(resolveWorkspaceInitialLayout(w.layout ?? null, aliveById, browserMap, fileMap), get().managedOwnerBindings);
    lifecycleLog('store.loadWorkspace.resolved', {
      to: name,
      cachedFirstSafe,
      aliveIds: Array.from(aliveById.keys()),
      initialIds: lifecycleLayoutIds(initialLayout),
      visibility: typeof document !== 'undefined' ? document.visibilityState : 'unknown',
    });

    // If the target workspace is empty AND there are live orphan panes
    // on the same backend that belong to no other named workspace, adopt
    // them. Same logic as bootstrap, applied here so clicking an empty
    // workspace never "does nothing".
    let didAdopt = false;
    if (!initialLayout && w.name !== '_current') {
      const otherNamed = new Set<string>();
      for (const other of state0.workspaces) {
        if (other.name === '_current' || other.name === w.name) continue;
        for (const id of other.paneIds ?? []) otherNamed.add(id);
      }
      // The cached workspaces list reflects DISK state, which lags the
      // in-memory layout until the 400ms autosave debounce fires. If
      // the user adds/splits a pane in A and clicks B before that
      // debounce lands, A's new pane isn't yet in A's on-disk paneIds
      // — and without this guard it would be misclassified as an
      // orphan and adopted into B (persisted immediately via the
      // didAdopt save below). Trust in-memory truth over the cache.
      if (
        state0.activeWorkspace &&
        state0.activeWorkspace !== w.name &&
        state0.activeWorkspace !== '_current' &&
        state0.layout
      ) {
        for (const id of collectPaneIds(state0.layout)) otherNamed.add(id);
      }
      const active = isRouterMode() ? activeBackendFromUrl() : null;
      const orphans = Object.values(state0.panes).filter((p) => {
        if (p.role === 'task_owner' || get().managedOwnerBindings.has(p.id) || otherNamed.has(p.id)) return false;
        if (active && p.backend && p.backend !== active) return false;
        if (p.status !== 'running') return false;
        return true;
      });
      if (orphans.length === 1) {
        initialLayout = { type: 'pane', paneId: orphans[0].id };
        didAdopt = true;
      } else if (orphans.length > 1) {
        initialLayout = orphans.reduce<Layout | null>((acc, p) => {
          if (!acc) return { type: 'pane', paneId: p.id };
          return {
            type: 'split',
            direction: 'vertical',
            ratio: 0.5,
            a: acc,
            b: { type: 'pane', paneId: p.id },
          };
        }, null);
        didAdopt = true;
      }
    }

    // Blank phase: when the OUTGOING workspace had 3+ panes, unmounting
    // them all simultaneously while at the same time mounting the
    // incoming workspace's panes interleaves WS-close events and
    // WS-subscribe events on the daemon, leaving codex / ratatui-class
    // TUIs in a corrupted ratatui-back-buffer-vs-xterm divergence state.
    //
    // The fix: separate the close burst from the open burst with a
    // brief blank period. Set layout=null first (which only unmounts
    // outgoing panes, no new mounts), wait for the daemon's close-side
    // bookkeeping to settle, then set the actual incoming layout
    // (which only mounts new panes). The two operation classes no
    // longer interleave on the daemon side.
    //
    // Only triggered when the outgoing workspace has 3+ panes — below
    // that threshold the bug doesn't manifest, and skipping the wait
    // preserves the snappy feel of small-workspace switches.
    //
    // Two further gates since the perf pass:
    //   - codex only: the close/subscribe interleave corruption this
    //     guards against is a codex/ratatui normal-buffer failure mode;
    //     Claude/shell panes never exhibited it, so they keep the
    //     instant switch.
    //   - actually-unmounting codex panes only: with the warm layer,
    //     most outgoing panes park (WS stays open — no close burst at
    //     all). But panes beyond the warm cap get evicted — those DO
    //     close in the same commit that mounts the incoming workspace,
    //     so a codex pane among them still warrants the separation.
    if (!navigationCurrent()) { releaseUnusedHandoff(); return; }
    const warmActive = warmPanesActive();
    const mergedPaneMap = paneMapFromAliveById(aliveById);
    const outgoingIdsForPark = state0.layout ? collectPaneIds(state0.layout) : [];
    const incomingIdsForPark = new Set(initialLayout ? collectPaneIds(initialLayout) : []);
    const parkedNext = warmActive
      ? computeParkedPaneIds(get().parkedPaneIds, outgoingIdsForPark, incomingIdsForPark, mergedPaneMap)
      : [];
    const unmountingCodex = outgoingIdsForPark.some((id) =>
      !incomingIdsForPark.has(id)
      && !parkedNext.includes(id)
      && paneLooksCodex(mergedPaneMap[id]));
    const needsBlankPhase = shouldBlankWorkspaceSwitch(state0.layout)
      && touchesCodex
      && unmountingCodex;

    if (needsBlankPhase) {
      lifecycleLog('store.loadWorkspace.blank-phase', {
        from: state0.activeWorkspace,
        to: name,
        outgoingIds: lifecycleLayoutIds(state0.layout),
        incomingIds: lifecycleLayoutIds(initialLayout),
        cachedFirstSafe,
        visibility: typeof document !== 'undefined' ? document.visibilityState : 'unknown',
      });
      // Unmount outgoing panes only. activeWorkspace flips so the
      // sidebar highlights the destination immediately even though the
      // grid is briefly blank.
      set({
        panes: mergedPaneMap,
        layout: null,
        browsers: browserMap,
        files: fileMap,
        activeWorkspace: name,
        activeWorkspaceMoveVersion: w.paneMoveVersion ?? 0,
        focusedPaneId: null,
        zoomedPaneId: null,
        parkedPaneIds: parkedNext,
      });
      await new Promise<void>((r) => setTimeout(r, 200));
      // If the user clicked another workspace during the blank phase,
      // abandon — that newer click's loadWorkspace is now in flight.
      if (!navigationCurrent() || get().activeWorkspace !== name) {
        lifecycleLog('store.loadWorkspace.abandon-after-blank', {
          to: name,
          activeWorkspace: get().activeWorkspace,
        });
        return;
      }
      // Now mount the destination panes.
      set({
        panes: mergedPaneMap,
        layout: initialLayout,
        focusedPaneId: initialLayout ? collectPaneIds(initialLayout)[0] ?? null : null,
      });
    } else {
      set({
        panes: mergedPaneMap,
        layout: initialLayout,
        browsers: browserMap,
        files: fileMap,
        activeWorkspace: name,
        activeWorkspaceMoveVersion: w.paneMoveVersion ?? 0,
        focusedPaneId: initialLayout ? collectPaneIds(initialLayout)[0] ?? null : null,
        // Clear any active zoom — it was scoped to the outgoing workspace.
        zoomedPaneId: null,
        parkedPaneIds: parkedNext,
      });
    }

    resumeWorkspaceView(name);
    if (handoffAcquired) get().claimAllPanes();
    get().detachOwnerPresentation(get().managedOwnerBindings, scope);

// Persist the adoption IMMEDIATELY (not via debounced autosave)
    // so the adopted layout survives even if the user clicks away
    // before the 400ms debounce would fire.
    if (didAdopt && initialLayout) {
      try {
        await trackWorkspaceSave(api.saveWorkspace({
          name,
          paneMoveVersion: w.paneMoveVersion,
          layout: initialLayout,
          paneIds: collectPaneIds(initialLayout),
          panes: collectPaneDefs(initialLayout, state0.panes),
          browsers: collectBrowserInfos(initialLayout, browserMap),
          files: collectFileInfos(initialLayout, fileMap),
          activeAt: new Date().toISOString(),
        }, scope));
      } catch { /* best-effort */ }
    }
    if (!navigationCurrent()) return;
    // Mark as just-viewed so the unread badge clears right away.
    get().markWorkspaceViewed(name);
    debugPerf('loadWorkspace visible', startedAt, {
      name,
      paneIds: initialLayout ? collectPaneIds(initialLayout).length : 0,
      blankPhase: needsBlankPhase,
      adopted: didAdopt,
      upgradedSummary,
    }, 80);
    lifecycleLog('store.loadWorkspace.visible', {
      name,
      paneIds: lifecycleLayoutIds(initialLayout),
      blankPhase: needsBlankPhase,
      adopted: didAdopt,
      upgradedSummary,
      elapsedMs: Math.round(perfNow() - startedAt),
    });

    // Phase 2 (background): respawn dead panes and repair the layout.
    // No await-before-setState — UI is already showing the new workspace.
    // (The outgoing workspace's pending autosave was already flushed at
    // the top of this function, before activeWorkspace flipped.)
    (async () => {
      const referenced = w.layout ? collectPaneIds(w.layout) : [];
      const idRemap = new Map<string, string>();
      let respawned = 0;
      let dropped = 0;

      if (cachedFirstSafe) {
        try {
          const freshPanes = isRouterMode()
            ? await api.listPanesAllBackends()
            : await api.listPanes();
          for (const p of freshPanes) if (!p.backend || `/b/${p.backend}` === scope.backendPrefix) aliveById.set(p.id, p);
          // Drop any Phase-1 seed the fresh list didn't confirm so the respawn
          // loop below recreates it (confirmed seeds were just replaced with
          // real data; a no-pane fetch is treated as non-authoritative).
          dropUnconfirmedSeeds(aliveById, seededIds, freshPanes, !isRouterMode());
        } catch {
          // Keep the cached-first view. Respawn repair below still uses
          // any cached running pane to avoid duplicate sessions.
        }
      }

      // Respawns run concurrently — each is an HTTP round-trip + PTY
      // spawn, and serializing them made a dead 4-pane workspace pay
      // 4× the latency before panes became interactive.
      await Promise.all(referenced.map(async (oldId) => {
        // Present covers suspended-resumable (see bootstrap): never respawn a
        // def-twin of a pane the daemon parked for an in-place Resume.
        if (isPresentPane(aliveById.get(oldId))) return;
        const def = w.panes?.find((d) => d.id === oldId);
        if (!navigationCurrent()) return;
        if (!def) {
          aliveById.set(oldId, failedPaneFromDef({ id: oldId, cwd: "", cmd: [], title: "Saved pane" }, new Error("No launch recipe was saved for this pane.")));
          dropped++; return;
        }
        if (def.role === 'task_owner' || get().managedOwnerBindings.has(oldId)) return;
        try {
          const fresh = await api.createPane(paneRestoreRequest(def), scope);
          if (!navigationCurrent()) return;
          idRemap.set(oldId, fresh.id);
          aliveById.set(fresh.id, fresh);
          respawned++;
          lifecycleLog('store.loadWorkspace.respawn-pane', {
            workspace: name,
            oldId,
            newId: fresh.id,
            cmd0: def.cmd?.[0] ?? '',
          });
        } catch (e) {
          console.warn('respawn failed for', oldId, e);
          aliveById.set(oldId, failedPaneFromDef(def, e));
          dropped++;
        }
      }));

      // Only mutate layout + arm autosave if the user is STILL viewing
      // the workspace we loaded. If they've clicked away to workspace C
      // while Phase 2's respawn was in flight, landing this finalLayout
      // would write B's respawned layout under C's activeWorkspace slot
      // — the exact cross-workspace leak we already had to patch in
      // v0.0.86 for Phase 1.
      if (!navigationCurrent() || get().activeWorkspace !== name) {
        lifecycleLog('store.loadWorkspace.abandon-settle', {
          to: name,
          activeWorkspace: get().activeWorkspace,
          respawned,
          dropped,
        });
        return;
      }

      // Reapply the layout when Phase 2 changed the picture: respawns
      // happened (ids remapped), OR the fresh listPanes revealed that
      // Phase 1's cached-first prune wrongly dropped a pane that is in
      // fact running (created seconds ago by another client, so the 3s
      // cache hadn't seen it). Without the second condition the pruned
      // pane was never re-added — and the unconditional autosave below
      // then PERSISTED the wrong prune, permanently orphaning an alive
      // pane into Background sessions.
      //
      // Guard against clobbering user edits made during Phase 2's
      // in-flight window: only reapply if the current layout still
      // matches what Phase 1 set (same pane-id set).
      if (idRemap.size > 0) {
        void api.remapPaneOrigins(Object.fromEntries(idRemap), scope).catch(() => {});
      }
      if (w.layout) {
        const remapped = idRemap.size > 0 ? remapLayoutIds(w.layout, idRemap) : w.layout;
        const finalLayout = detachManagedOwnerPresentation(pruneLayout(
          remapped,
          (id) => isPresentPane(aliveById.get(id)) || !!browserMap[id] || !!fileMap[id],
        ), get().managedOwnerBindings);
        const currentIds = collectPaneIds(get().layout);
        const phase1Ids = collectPaneIds(initialLayout);
        const finalIds = collectPaneIds(finalLayout);
        const userEditedDuringPhase2 =
          currentIds.length !== phase1Ids.length || currentIds.some((id, i) => id !== phase1Ids[i]);
        const layoutChanged =
          finalIds.length !== phase1Ids.length || finalIds.some((id, i) => id !== phase1Ids[i]);
        if (!userEditedDuringPhase2 && (layoutChanged || dropped > 0)) {
          const paneMap: Record<string, PaneInfo> = {};
          for (const [id, p] of aliveById) paneMap[id] = p;
          set({
            panes: paneMap,
            layout: finalLayout,
            focusedPaneId: finalLayout ? collectPaneIds(finalLayout)[0] ?? null : null,
          });
          lifecycleLog('store.loadWorkspace.final-remap', {
            workspace: name,
            remap: Array.from(idRemap.entries()),
            finalIds: lifecycleLayoutIds(finalLayout),
            recovered: finalIds.filter((id) => !phase1Ids.includes(id)),
          });
        }
      }

      scheduleAutosave(get);

      if (respawned > 0) {
        get().toast(
          `Loaded "${name}" — respawned ${respawned} pane${respawned === 1 ? '' : 's'}`,
          { kind: 'success', icon: '↻' },
        );
      }
      if (dropped > 0) {
        get().toast(
          `Loaded "${name}" — ${dropped} pane${dropped === 1 ? '' : 's'} could not be restored`,
          { kind: 'warn', icon: '⚠', ttlMs: 5000 },
        );
      }
      // No toast on the common case (everything was already alive) — the
      // UI switch itself is feedback enough.
      debugPerf('loadWorkspace settled', startedAt, {
        name,
        respawned,
        dropped,
      }, 120);
      lifecycleLog('store.loadWorkspace.settled', {
        name,
        respawned,
        dropped,
        elapsedMs: Math.round(perfNow() - startedAt),
      });
    })();
  },

  deleteWorkspace: async (name) => {
    // Drop any pending autosave first — if the user clicks X while a 400ms
    // debounced save is queued, it would fire after the DELETE returns and
    // resurrect the workspace via api.saveWorkspace → daemon Put.
    cancelAutosave();
    // If we're deleting the workspace we're currently viewing, flip
    // activeWorkspace off it BEFORE the API call. Otherwise any autosave
    // armed by layout/pane events during/after the await still writes
    // under the deleted name and recreates it on disk.
    if (get().activeWorkspace === name) {
      set({ activeWorkspace: CURRENT_WS, activeWorkspaceMoveVersion: 0 });
    }
    await api.deleteWorkspace(name);
    // Remove from saved order too.
    const order = get().wsOrder.filter((n) => n !== name);
    get().setWsOrder(order);
    await get().refreshWorkspaces({ full: true });
    get().toast(`Deleted workspace "${name}"`, { kind: 'info', icon: '×' });
  },

  renameWorkspace: async (oldName, newName) => {
    const trimmed = newName.trim();
    if (!trimmed || trimmed === oldName || trimmed === CURRENT_WS) return;
    try {
      await api.renameWorkspace(oldName, trimmed);
    } catch (e) {
      get().toast(
        `Couldn't rename: ${(e as Error).message}`,
        { kind: 'error', icon: '⚠', ttlMs: 5000 },
      );
      return;
    }
    // Migrate the local persisted state so the new name keeps its order
    // position and last-viewed timestamp.
    set((s) => {
      const newOrder = s.wsOrder.map((n) => (n === oldName ? trimmed : n));
      try { localStorage.setItem(WS_ORDER_KEY, JSON.stringify(newOrder)); } catch {}
      const viewedCopy = { ...s.wsLastViewedMs };
      if (oldName in viewedCopy) {
        viewedCopy[trimmed] = viewedCopy[oldName];
        delete viewedCopy[oldName];
        try { localStorage.setItem(WS_VIEWED_KEY, JSON.stringify(viewedCopy)); } catch {}
      }
      return {
        wsOrder: newOrder,
        wsLastViewedMs: viewedCopy,
        activeWorkspace: s.activeWorkspace === oldName ? trimmed : s.activeWorkspace,
      };
    });
    await get().refreshWorkspaces({ full: true });
    get().toast(`Renamed to "${trimmed}"`, { kind: 'success', icon: '✓' });
  },

  setWorkspaceContextPack: async (name, contextPack) => {
    const saved = await api.setWorkspaceContextPack(name, contextPack);
    get().mergeSavedWorkspaceIntoCache(saved);
    await get().refreshWorkspaces();
    get().toast(
      contextPack.trim() ? `Context pack attached to "${name}"` : `Context pack detached from "${name}"`,
      { kind: 'success', icon: '✓', ttlMs: 2500 },
    );
  },

  clearLayout: () => {
    // Only panes that no saved workspace claims end up under Detached
    // sessions. Creating a workspace from a saved one clears the layout too,
    // and there every pane stays in its workspace, so a toast would be false.
    const s = get();
    const claimed = new Set<string>();
    for (const w of s.workspaces) {
      if (w.name === CURRENT_WS) continue;
      for (const id of w.paneIds) claimed.add(id);
    }
    const detached = collectPaneIds(s.layout).filter((id) => !claimed.has(id)).length;
    set({
      layout: null,
      focusedPaneId: null,
      activeWorkspace: CURRENT_WS,
      activeWorkspaceMoveVersion: 0,
    });
    scheduleAutosave(get);
    if (detached > 0) {
      get().toast(
        `Cleared layout. ${detached} pane${detached === 1 ? ' is' : 's are'} still running under Detached sessions.`,
        { kind: 'info', icon: '○' },
      );
    }
  },

  addPaneToLayout: (paneId, direction = 'horizontal') => {
    if (get().panes[paneId]?.role === 'task_owner' || get().managedOwnerBindings.has(paneId)) return;
    set((s) => {
      // Clear zoom so the (re)focused pane is visible, not focused-but-
      // hidden behind a zoomed pane — e.g. an ARIA-spawned pane grafted
      // in while the user is zoomed on something else.
      const clearZoom = s.zoomedPaneId && s.zoomedPaneId !== paneId ? { zoomedPaneId: null } : {};
      // Already in layout? No-op (but still surface it).
      const layout = s.layout;
      const already = collectPaneIds(layout).includes(paneId);
      // ARIA auto-mode grafts a spawned pane in here — may land mid-typing.
      // lastUserFocusId is deliberately NOT stamped (this is background activity,
      // not a user focus gesture), so the focus effect keeps the user's caret put.
      if (already) return { layout, focusedPaneId: paneId, ...clearZoom };
      const target = s.focusedPaneId;
      const newLayout = insertPaneOrAppend(layout, paneId, target ? direction : null, target);
      return { layout: newLayout, focusedPaneId: paneId, ...clearZoom };
    });
    scheduleAutosave(get);
  },

  revealPane: async (paneId) => {
    const s = get();
    const pane = s.panes[paneId];
    if (!pane) {
      s.toast("Can't reveal — pane is gone", { kind: 'warn', icon: '⚠', ttlMs: 3000 });
      return;
    }
    // Flag this pane as "just revealed" so Pane.tsx can run a ~3s
    // outline pulse. Cleared on a timer; we guard against races where
    // a second reveal came in by only clearing if the flag is still us.
    const markRevealed = () => {
      set({ recentlyRevealedPaneId: paneId });
      setTimeout(() => {
        if (get().recentlyRevealedPaneId === paneId) {
          set({ recentlyRevealedPaneId: null });
        }
      }, 3000);
    };
    // 1. Cross-backend: switch to the pane's backend if we're in router
    //    mode and it lives on the other side. switchBackend already
    //    accepts an optional workspace name to load after the swap, so
    //    we pass the owning workspace if we can find one — saves a
    //    second state update on arrival.
    const owner = s.workspaces.find(
      (w) => w.name !== CURRENT_WS && (w.paneIds ?? []).includes(paneId),
    );
    if (isRouterMode() && pane.backend && pane.backend !== s.activeBackend) {
      await get().switchBackend(pane.backend, owner?.name);
      // The post-switch bootstrap refetches everything; finish focusing
      // the pane after the new state lands.
      const after = get();
      if (after.panes[paneId]) {
        if (!collectPaneIds(after.layout).includes(paneId)) {
          after.addPaneToLayout(paneId);
        }
        after.setFocus(paneId);
        scrollPaneIntoView(paneId);
        markRevealed();
      }
      return;
    }
    // 2. Already in the current layout → just focus + scroll.
    if (collectPaneIds(s.layout).includes(paneId)) {
      get().setFocus(paneId);
      scrollPaneIntoView(paneId);
      markRevealed();
      return;
    }
    // 3. Owned by a named workspace that isn't the active one → load it.
    if (owner && s.activeWorkspace !== owner.name) {
      await get().loadWorkspace(owner.name);
      get().setFocus(paneId);
      scrollPaneIntoView(paneId);
      markRevealed();
      return;
    }
    // 4. Orphan (Background session) or owned-by-active-ws-but-not-in-
    //    layout — adopt it into the current layout and focus.
    get().addPaneToLayout(paneId);
    get().setFocus(paneId);
    scrollPaneIntoView(paneId);
    markRevealed();
  },
}));

// "Last looked at" per pane, behind every unread marker and the turn chip's
// "new" tag. Two rules on top of the stamp setFocus already writes:
//   - the pane you LEAVE is stamped too: you were looking at it until now, so
//     a turn you watched finish is not "new" the moment you click away;
//   - the map is persisted per browser, so a reload doesn't turn every pane's
//     past activity into fresh news.
try {
  let prevFocused = useStore.getState().focusedPaneId;
  let prevViewed = useStore.getState().lastViewedMs;
  let saveTimer: ReturnType<typeof setTimeout> | null = null;
  useStore.subscribe((s) => {
    if (s.focusedPaneId !== prevFocused) {
      const left = prevFocused;
      prevFocused = s.focusedPaneId;
      if (left && s.panes[left] && document.visibilityState !== 'hidden') {
        useStore.setState((st) => ({ lastViewedMs: { ...st.lastViewedMs, [left]: Date.now() } }));
        return; // the nested set re-enters this subscriber for the save
      }
    }
    if (s.lastViewedMs === prevViewed) return;
    prevViewed = s.lastViewedMs;
    if (saveTimer) clearTimeout(saveTimer);
    saveTimer = setTimeout(() => {
      const entries = Object.entries(useStore.getState().lastViewedMs)
        .sort((a, b) => b[1] - a[1])
        .slice(0, PANE_LAST_VIEWED_CAP);
      try { localStorage.setItem(PANE_LAST_VIEWED_KEY, JSON.stringify(Object.fromEntries(entries))); } catch { /* ignore */ }
    }, 1000);
  });
} catch { /* SSR / no-window */ }

// Mirror activeWorkspace → localStorage on every change, so bootstrap()
// can restore it across a page reload (auto-update, Tauri restart, F5).
// Zustand subscriptions fire synchronously inside set(), so by the time
// applyUpdate cache-busts the WebView navigation after the key is pinned.
try {
  const myWindowLabel = windowLabel();
  let lastMirrored = useStore.getState().activeWorkspace;
  useStore.subscribe((s) => {
    if (s.activeWorkspace === lastMirrored) return;
    lastMirrored = s.activeWorkspace;
    // Per-window key so each window restores its OWN workspace across a
    // reload (auto-update / Tauri restart / F5). Primary keeps the legacy
    // un-suffixed key (windowScopedKey handles the special-case).
    try { localStorage.setItem(windowScopedKey(ACTIVE_WS_KEY), s.activeWorkspace); } catch { /* ignore */ }
    // Record ownership of the (named) workspace this window now shows so a
    // sibling can request a save-and-suspend handoff. A reloaded old view
    // must never overwrite the claim of the window that received it.
    try {
      const other = checkOwnedByOther(s.activeWorkspace, myWindowLabel);
      if (other) {
        suspendWorkspaceView(s.activeWorkspace, other);
        cancelAutosave();
        useStore.setState({ claimIntent: false });
      } else claimWorkspace(s.activeWorkspace, myWindowLabel);
    } catch { /* ignore */ }
  });
  // Keep this window's claim fresh (within the ownership TTL) so a sibling
  // never mistakes a live window's workspace for an abandoned one.
  try {
    startOwnershipHeartbeat(() => useStore.getState().activeWorkspace, myWindowLabel, workspaceViewVisible);
    installWorkspaceHandoff({
      label: myWindowLabel,
      active: () => useStore.getState().activeWorkspace,
      prepare: async () => {
        const before = useStore.getState();
        const scope = currentRoutingScope();
        await flushAutosave(() => useStore.getState(), undefined, { throwOnError: true });
        await Promise.all([...workspaceSaves]);
        const state = useStore.getState();
        if (
          state.navigationGeneration !== before.navigationGeneration ||
          state.activeWorkspace !== before.activeWorkspace ||
          currentRoutingScope().key !== scope.key ||
          state.mainPresentation.kind !== 'workspace'
        ) {
          throw new Error('Workspace is switching; try again.');
        }
        if (!state.layout) {
          const saved = pickWorkspaceByName(state.workspaces, state.activeWorkspace, state.activeBackend);
          if (!saved || saved.layout || saved.paneIds?.length) {
            throw new Error('Workspace is switching; try again.');
          }
          // A genuinely empty named workspace has nothing to flush. Never
          // write the transient null layout over an existing saved layout.
          return;
        }
        await saveWorkspaceSnapshot(() => useStore.getState(), {
          name: state.activeWorkspace,
          layout: state.layout,
          panes: state.panes,
          browsers: state.browsers,
          files: state.files,
        }, { refreshWorkspaces: false });
      },
      suspended: () => {
        cancelAutosave();
        useStore.setState((state) => ({
          claimIntent: false,
          focusedPaneId: null,
          navigationGeneration: state.navigationGeneration + 1,
        }));
      },
    });
  } catch { /* ignore */ }
} catch { /* SSR / no-window: nothing to mirror */ }

// A health poll is what observes the Windows shell finally swapping after WSL
// updated first. Re-evaluate on every completed poll so an activation gap never
// becomes a permanent one-shot suppression.
subscribeDaemonHealthState((next) => {
  if (next.polling || !next.health) return;
  const current = useStore.getState();
  if (current.updateStatus?.applying || current.updateStatus?.shellApplying) return;
  if (scheduleStaleBundleReload(next.bundleState)) {
    current.toast(`Interface updated to ${bundleReloadTarget(next.bundleState)} — reloading…`, { kind: 'success', icon: '✓' });
  }
});

// Nudges the pane element into view once it's mounted. Two rAF's so
// we wait past the layout commit that `addPaneToLayout` / `loadWorkspace`
// trigger — without the double-wait the query runs before the DOM has
// the new pane wrapper.
function scrollPaneIntoView(paneId: string) {
  if (typeof window === 'undefined' || typeof document === 'undefined') return;
  requestAnimationFrame(() => {
    requestAnimationFrame(() => {
      const el = document.querySelector(`[data-pane-id="${paneId}"]`);
      if (el && typeof (el as HTMLElement).scrollIntoView === 'function') {
        try {
          (el as HTMLElement).scrollIntoView({ behavior: 'smooth', block: 'nearest', inline: 'nearest' });
        } catch { /* smooth scroll unsupported → no-op */ }
      }
    });
  });
}

// Poll panes, ports, and workspaces periodically so the UI reflects
// external changes (another tab, daemon-side edits). Workspaces rarely
// change but polling them here keeps the sidebar counts self-healing
// even if a specific autosave → refresh chain races.
//
// Side effect: tracks daemon reachability as `daemonOnline`. If every
// poll in a cycle fails, the daemon is considered offline and the
// topbar dot flips red. The first successful poll flips it green.
async function runRefreshPoll() {
  const s = useStore.getState();
  // Don't fire refresh polls while a self-update is in flight — the
  // daemon is about to die, and the router will return 502 for
  // ~1-2s. Without this, every 3s tick floods the console with
  // 502s during the update window and the workspace/pane lists
  // briefly render empty, making the UI feel "broken". The store's
  // applyUpdate action re-bootstraps everything once the daemon is
  // back, so we don't lose anything by skipping.
  if (s.updateStatus?.applying) return;
  const results = await Promise.allSettled([
    s.refreshPanes(),
    s.refreshPorts(),
    s.refreshWorkspaces(),
  ]);
  const anyOk = results.some((r) => r.status === 'fulfilled');
  if (useStore.getState().daemonOnline !== anyOk) {
    useStore.setState({ daemonOnline: anyOk });
  }
}

// A mounted browser-acceptance preview boots into the deterministic read-only
// acceptance shell (see previewAcceptanceShell.ts) rather than the
// daemon-backed app, so starting these polls would only produce failed /api
// requests against a backend the acceptance exposure rejects. User previews
// (loopback or cloud hostname) and production, cloud, Tauri and dev bundles
// poll exactly as before.
const previewAcceptanceHost = typeof window !== 'undefined'
  && isPreviewBuild
  && isPreviewAcceptanceHost(window.location.hostname);
if (!previewAcceptanceHost) {
  setInterval(() => {
    // Skip polling while the tab/window is hidden — backgrounded tabs
    // don't need fresh pane/port/workspace state, and three GETs every 3s
    // across many parked tabs is wasted work + render churn. The
    // visibilitychange listener below fires one immediate refresh on
    // return so state is fresh the instant the tab is foregrounded again.
    if (typeof document !== 'undefined' && document.visibilityState === 'hidden') return;
    void runRefreshPoll();
  }, 3000);

  // On return to a visible tab, refresh immediately rather than waiting up
  // to 3s for the next interval tick.
  if (typeof document !== 'undefined') {
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible') void runRefreshPoll();
    });
  }
}

// DEV-only hook for local UI tooling — see the matching block in cloud.ts.
if (import.meta.env.DEV && typeof window !== 'undefined') {
  const w = window as unknown as { __soluDev?: Record<string, unknown> };
  w.__soluDev = { ...w.__soluDev, useStore };
}

export function terminalSessionKey(scope: RoutingScope, paneId: string): string { return JSON.stringify([scope.key, paneId]); }
export function effectiveVisiblePaneIds(s: { mainPresentation: MainPresentation; layout: Layout | null; zoomedPaneId?: string | null; focusedPaneId?: string | null }): string[] {
 if(!routingPrincipalAllowed())return [];
 if (s.mainPresentation.kind === 'owner') return !s.mainPresentation.history && s.mainPresentation.pane && !s.mainPresentation.loading ? [s.mainPresentation.pane.id] : [];
 if (s.zoomedPaneId) return collectPaneIds(s.layout).includes(s.zoomedPaneId) ? [s.zoomedPaneId] : [];
 if(isMobileViewport()) {
  const leaves=collectLeaves(s.layout),selected=leaves.find(leaf=>leafId(leaf) === s.focusedPaneId) ?? leaves[0];
  return selected?.type === 'pane' ? [selected.paneId] : [];
 }
 return collectPaneIds(s.layout);
}
export function effectiveTerminalTarget(s: { mainPresentation: MainPresentation; focusedPaneId: string | null; layout: Layout | null; panes: Record<string, PaneInfo> }): string | null {
 if(!routingPrincipalAllowed())return null;
 if (s.mainPresentation.kind === 'owner') return !s.mainPresentation.history && !s.mainPresentation.loading && !s.mainPresentation.error ? s.mainPresentation.pane?.id ?? null : null;
 return s.focusedPaneId && collectPaneIds(s.layout).includes(s.focusedPaneId) && s.panes[s.focusedPaneId]?.kind !== 'native' ? s.focusedPaneId : null;
}
let paneAliasScope = currentRoutingScope().key;
onRoutingScopeChange(() => {
 const nextScope=currentRoutingScope().key;
 if (useStore.getState().mainPresentation.kind === 'owner') useStore.getState().returnToWorkspace();
 if(nextScope !== paneAliasScope) {
  paneAliasScope=nextScope;
  portsAppliedSequence = ++portsRefreshSequence;
  // Metadata aliases are active-scope only; retained cores own their snapshots.
  // A new account/backend must bootstrap rather than reuse an ID-only alias.
  useStore.setState({panes:{},ports:[],portsError:null,portsUpdatedAt:null,workspaces:[],paneOwnership:{},paneOwnerState:{},paneOwnershipTokens:{},managedOwnerBindings:new Map()});
 }
});

subscribeMachineAccess(()=>{
 const access=getMachineAccessState(),scope=currentRoutingScope();
 if(!access.blocked || !access.machineId || ![ `/m/${access.machineId}`, `/m/${encodeURIComponent(access.machineId)}` ].includes(scope.prefix))return;
 useStore.setState(state=>({navigationGeneration:state.navigationGeneration+1,...(state.mainPresentation.kind === 'owner' ? {mainPresentation:{...state.mainPresentation,pane:undefined,loading:false,error:'Access to this backend is blocked. Reconnect through the existing account controls.'}} : {})}));
});
