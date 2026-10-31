
import { useEffect, useRef, useState } from 'react';
import { useShallow } from 'zustand/shallow';
import { useStore } from '../store';
import { useDaemonHealth } from '../lib/useDaemonHealth';
import { useShellUpdate } from '../lib/useCloudShellUpdate';
import { installerUpdate } from '../lib/installerUpdate';
import { toggleScrollDiag } from '../scrollDiag';
import { VersionPopover } from './VersionPopover';
import { isPreviewVersion, usePreviewChannel } from '../lib/previewChannel';
import { ReleasePopover } from './ReleasePopover';
import { PanesPopover } from './PanesPopover';
import { PortsPopover } from './PortsPopover';
import { slotList } from '../extensions';
import { ranAgo } from '../lib/ranAgo';
import { PhoneAccessChip } from './PhoneAccessChip';
import { useOperatorFeatures } from '../entitlements';
import { Tooltip } from './Tooltip';
import { IconArrowUp, IconClock, IconAlertTriangle, IconCloud, IconGauge } from './ui/icons';
import { useCloud } from '../cloud';
import { isEmbedded } from '../embed';
import { ApiError, api } from '../api';
import { primaryLabel } from './PortPanel';
import { isWebService, workspacePorts } from '../lib/ports';
import './statusbarChips.css';
import { deriveUpdateHoldPresentation, type Health, type ReleaseCheck } from '../types';
import type { ReleaseTrainStatus } from '../api';
import { PLAIN_SYSTEM_AREA } from './system/systemPlain';

const HEALTH_SHOW_OFFLINE_AFTER_MS = 30000;

function formatDrainCountdown(deadlineMs: number, nowMs: number): string {
  const total = Math.max(0, Math.ceil((deadlineMs - nowMs) / 1000));
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, '0')}`;
}

// Backend the desktop shell injects on the main window (see SHARED CONTRACTS
// in lib.rs). `fallback:true` means WSL was requested but host-native is what
// actually came up.
interface SoluBackend {
  requested?: 'wsl' | 'host' | 'dual';
  actual?: 'wsl' | 'host' | 'dual';
  fallback?: boolean;
}

function soluBackend(): SoluBackend | null {
  if (isEmbedded()) return null;
  const w = window as unknown as { __SOLU_BACKEND__?: SoluBackend; __SOLU_TAURI__?: boolean };
  if (w.__SOLU_BACKEND__) return w.__SOLU_BACKEND__;
  // Desktop shell present but the global wasn't injected (older shell) — let
  // the caller fall back to health.backendID. Outside the shell, return null
  // so the chip stays hidden on the browser tab / phone PWA.
  return w.__SOLU_TAURI__ ? {} : null;
}

// Human label for a backend id. WSL daemon == Linux; host/win == the
// Windows-native (PowerShell) backend; dual == the router fronting both.
function backendLabel(id: string | undefined): string {
  switch (id) {
    case 'wsl': return 'WSL (Linux)';
    case 'host':
    case 'win': return 'Windows (PowerShell)';
    case 'dual': return 'Dual (WSL + Windows)';
    default: return 'Backend';
  }
}

// Map a daemon shellPlatform ("linux-amd64" / "windows-amd64") to a backend id
// so we can still label the chip when the desktop shell didn't inject an
// `actual` (older shell), reading only health.
function backendFromPlatform(platform: string | undefined): string | undefined {
  if (!platform) return undefined;
  if (platform.startsWith('linux')) return 'wsl';
  if (platform.startsWith('windows')) return 'win';
  return undefined;
}

// Footer chip: which backend the daemon is running as ("WSL (Linux)" vs
// "Windows (PowerShell)"). Desktop-shell only — a browser tab / phone PWA
// has no backend choice, so it renders nothing (and no dangling separator).
// On a WSL→host fallback it tints warning + explains how to recover.
function BackendChip({ health }: { health: { backendID?: string; shellPlatform?: string } | null }) {
  const sb = soluBackend();
  const cloudMode = useCloud((s) => s.mode) === 'cloud';
  if (!sb) return null; // not the desktop shell

  // Cloud mode runs no LOCAL daemon, so the Tauri shell hardcodes
  // __SOLU_BACKEND__.actual to "host" (a placeholder). Left unchecked that
  // mislabels a remote WSL host as "Windows (PowerShell)". The real backend
  // is the REMOTE host's, carried by the relayed /api/health
  // (backendID / shellPlatform) — prefer it in cloud mode. Locally, keep
  // preferring the shell's `actual` (it's truthful there) with health as
  // fallback for older shells.
  const id = cloudMode
    ? (health?.backendID || backendFromPlatform(health?.shellPlatform))
    : (sb.actual || health?.backendID || backendFromPlatform(health?.shellPlatform));
  if (!id) return null; // cloud mode before the first health lands: hide rather than mislabel
  const fallback = !cloudMode && sb.fallback === true;
  // Dedup vs the top-left context pill, which already shows the selected backend
  // on local desktop. The footer chip earns its space only when it says
  // something the pill can't: a WSL→host fallback (actual ≠ selected), or the
  // remote host's backend in cloud mode.
  if (!cloudMode && !fallback) return null;
  const label = backendLabel(id);

  const title = cloudMode
    ? `Host backend: ${label}`
    : fallback
      ? 'WSL was requested but could not start — running the Windows-native backend instead. ' +
        'Fix: use the tray "Repair setup" item to re-validate WSL, or switch the backend.'
      : `Daemon backend: ${label}`;

  return (
    <>
      <span className="statusbar-sep">·</span>
      <Tooltip label={title} side="top">
        <span
          className="statusbar-backend"
          style={fallback ? { color: 'var(--warn)' } : undefined}
        >
          {fallback && <IconAlertTriangle size={12} />}
          {label}
        </span>
      </Tooltip>
    </>
  );
}

// System areas that only describe the workflow Tasks engine. A public beta
// install does not run it, so its health is noise there.
const WORKFLOW_SYSTEM_AREAS = new Set(['engine', 'runtimes']);

type SystemSummary = NonNullable<Health['system']>;

// The system verdict without the workflow areas. Recomputed from the
// per-area reasons when the daemon sends them; an older daemon only sends
// the headline, which is dropped when it names a workflow area.
export function systemWithoutWorkflow(system: SystemSummary): SystemSummary {
  const rank = { ok: 0, unknown: 1, warn: 2, critical: 3 } as const;
  if (system.reasons) {
    const kept = system.reasons.filter((r) => !WORKFLOW_SYSTEM_AREAS.has(r.key));
    if (kept.length === system.reasons.length) return system;
    const worst = kept.reduce<SystemSummary['level']>((level, r) => (rank[r.level] > rank[level] ? r.level : level), 'ok');
    const top = kept.find((r) => r.level === worst);
    return { ...system, level: worst, reason: top?.text, reasons: kept };
  }
  const area = system.reason?.split(':')[0] ?? '';
  return WORKFLOW_SYSTEM_AREAS.has(area) ? { ...system, level: 'ok', reason: undefined } : system;
}

// Plain words for each system area live in system/systemPlain.ts: the
// daemon's own reason text ("memory PSI some10 …") is for operators.

// Public installs only speak up when something is critical; warnings and
// partial data stay a quiet "System" (details remain on the System page).
export function plainSystemText(system: SystemSummary): string | null {
  if (system.level !== 'critical') return null;
  const top = system.reasons?.find((r) => r.level === 'critical');
  const key = top?.key ?? system.reason?.split(':')[0]?.trim() ?? '';
  return PLAIN_SYSTEM_AREA[key] ?? 'System needs attention';
}

// The long tail behind the headline reason, for the chip's tooltip.
export function systemReasonDetail(system: SystemSummary): string | undefined {
  return system.reasons?.find((r) => r.text === system.reason)?.detail || undefined;
}

function SystemChip({ health }: { health: Health | null }) {
  const team = useOperatorFeatures();
  const setOpen = useStore((s) => s.setSystemPageOpen);
  const reported = health?.system;
  const system = reported && !team ? systemWithoutWorkflow(reported) : reported;
  if (!system) return null;
  const age = ranAgo(system.sampledAt);
  const partial = system.level === 'unknown';
  let text: string;
  let title: string;
  if (team) {
    // The chip carries the short reason ("host disk: 102.9 GB free on C:\");
    // its long tail (VHDX, %TEMP%) rides in the tooltip, never in the bar.
    const detail = systemReasonDetail(system);
    text = system.level === 'ok' || partial ? 'System' : (system.reason || 'System needs attention');
    title = `${system.reason || (partial ? 'Partial data (unavailable on this platform)' : 'All monitored systems healthy')}${detail ? ` · ${detail}` : ''}${age ? ` · checked ${age} ago` : ''}`;
  } else {
    const plain = plainSystemText(system);
    text = plain ?? 'System';
    title = plain ? `${plain}. Click for details.` : 'Computer and app health. Click for details.';
  }
  // Public installs keep the chip neutral unless it is critical.
  const level = team || system.level === 'critical' ? system.level : 'ok';
  return (
    <>
      <span className="statusbar-sep">·</span>
      <Tooltip label={title} side="top">
        <button type="button" className={`statusbar-system ${level}`} aria-label={title} onClick={() => setOpen(true)}>
          <IconGauge size={13} />
          <span role="status" aria-live="polite">{text}</span>
        </button>
      </Tooltip>
    </>
  );
}

// Connection chip: the one first-class "am I on cloud or local hosting?"
// signal in the app chrome. Everything else here (backend chip, phone chip)
// describes WHAT is running; this one says HOW this client reaches it —
// direct to a local daemon, or relayed through soluterminal cloud to a
// (possibly remote) machine. Hidden while cloud.ts is still detecting so it
// never flashes a wrong answer.
function ConnChip() {
  const mode = useCloud((s) => s.mode);
  const isLocal = useCloud((s) => s.isLocal);
  const selectedMachineId = useCloud((s) => s.selectedMachineId);
  const machines = useCloud((s) => s.machines);
  if (mode === 'detecting') return null;
  // Dedup vs the top-left context pill, which already states hosting (⚡ Local).
  // In pure local mode this chip would just repeat that, so hide it. It earns
  // its space only in cloud mode, where it says something the pill can't:
  // whether this client is relayed to a remote machine (☁ name) or wired
  // directly to a local daemon.
  if (mode !== 'cloud') return null;

  const relayed = !isLocal;
  if (!relayed) {
    return (
      <>
        <span className="statusbar-sep">·</span>
        <Tooltip label="Local hosting — connected directly to the daemon on this machine" side="top">
          <span className="statusbar-conn is-local">
            local
          </span>
        </Tooltip>
      </>
    );
  }
  const machine = machines.find((m) => m.id === selectedMachineId);
  const name = machine?.name || 'cloud';
  return (
    <>
      <span className="statusbar-sep">·</span>
      <Tooltip
        label={`Cloud hosting — relayed through soluterminal cloud to ${machine?.name ? `"${machine.name}"` : 'the selected machine'}. Panes run on that machine.`}
        side="top"
      >
        <span className="statusbar-conn is-cloud">
          <IconCloud size={12} />
          {name}
        </span>
      </Tooltip>
    </>
  );
}

function abbreviateHome(p: string) {
  if (!p) return p;
  return p.replace(/^\/home\/[^/]+/, '~');
}

export function ReleaseChip({ health }: { health: Health | null }) {
  const readiness = health?.releaseReadiness;
  const [open, setOpen] = useState(false);
  const [releaseCheck, setReleaseCheck] = useState<ReleaseCheck | null | undefined>(undefined);
  // undefined = old daemon (no release-train endpoint) → the popover renders
  // exactly as before; null = present but unreadable.
  const [train, setTrain] = useState<ReleaseTrainStatus | null | undefined>(undefined);
  const btnRef = useRef<HTMLButtonElement | null>(null);
  const visible = readiness?.state === 'ok' && readiness.functionalCount > 0;

  useEffect(() => {
    if (!visible || localStorage.getItem('solu.debugOpenRelease') !== '1') return;
    localStorage.removeItem('solu.debugOpenRelease');
    setOpen(true);
  }, [visible]);

  useEffect(() => {
    if (!open) return;
    let current = true;
    setReleaseCheck(undefined);
    void api.workflowReleaseCheck().then(
      (check) => { if (current) setReleaseCheck(check); },
      (error: unknown) => {
        if (!current) return;
        setReleaseCheck(error instanceof ApiError && error.status === 404 ? undefined : null);
      },
    );
    return () => { current = false; };
  }, [open]);

  useEffect(() => {
    if (!open) return;
    let current = true;
    setTrain(undefined);
    // Refresh is a preview-only daemon read and is coalesced by its 15s TTL;
    // it never declares or departs a release train.
    void api.getReleaseTrain(true).then(
      (status) => { if (current) setTrain(status); },
      (error: unknown) => {
        if (!current) return;
        setTrain(error instanceof ApiError && error.status === 404 ? undefined : null);
      },
    );
    return () => { current = false; };
  }, [open]);

  if (!visible || !readiness) return null;
  const oldest = Date.parse(readiness.oldestUnreleasedAt);
  const warn = Number.isFinite(oldest) && Date.now() - oldest > 24 * 60 * 60 * 1000;
  const title = `${readiness.unreleasedTotal} unreleased commits; ${readiness.functionalCount} functional files + ${readiness.docsOnlyCount} documentation files changed since v${readiness.lastReleaseVersion}; daemon runs v${readiness.runningVersion}`;
  return (
    <>
      <span className="statusbar-sep">·</span>
      <Tooltip label={title} side="top">
        <button
          ref={btnRef}
          type="button"
          className={`statusbar-release${warn ? ' warn' : ''}`}
          aria-label={title}
          onClick={() => setOpen(!open)}
        >
          <IconArrowUp size={12} className="statusbar-release-glyph" />
          <span>release +{readiness.unreleasedTotal}</span>
        </button>
      </Tooltip>
      {open && btnRef.current && (
        <ReleasePopover
          anchor={btnRef.current}
          readiness={readiness}
          releaseCheck={releaseCheck}
          train={train}
          onVetoTrain={() => {
            void api.vetoReleaseTrain()
              .then(() => api.getReleaseTrain())
              .then(setTrain)
              .catch(() => {});
          }}
          onClose={() => setOpen(false)}
        />
      )}
    </>
  );
}

// Panes chip — the footer's pane-population counter.
//
// Purely "what's running" now: the "does anything need me?" job moved to the
// topbar attention bell (desktop) / MobileTabBar attention tab (mobile), so
// this chip no longer flips to "N need you" — that was a second, redundant
// alert for the same count. It reads "39 panes" with the running · exited
// breakdown in the tooltip; click opens the popover for the pane list +
// exited-pane bulk cleanup.
function PanesChip() {
  const { running, exited } = useStore(
    useShallow((s) => {
      let running = 0;
      let exited = 0;
      for (const id in s.panes) {
        if (s.panes[id].status === 'running') running++;
        else exited++;
      }
      return { running, exited };
    }),
  );
  const [open, setOpen] = useState(false);
  const btnRef = useRef<HTMLButtonElement | null>(null);

  const text = `${running} pane${running === 1 ? '' : 's'}`;
  const title = `${running} running${exited > 0 ? ` · ${exited} exited` : ''} — click for details`;
  const cls = 'statusbar-panes';
  return (
    <>
      <Tooltip label={title} side="top">
        <button
          ref={btnRef}
          type="button"
          className={cls}
          aria-label={title}
          onClick={() => setOpen(!open)}
        >
          <span role="status" aria-live="polite">{text}</span>
        </button>
      </Tooltip>
      {open && btnRef.current && (
        <PanesPopover anchor={btnRef.current} onClose={() => setOpen(false)} />
      )}
    </>
  );
}

// Dev-servers chip. The old `25 ports` counted every same-user listener the
// scanner sees — across BOTH backends in dual mode — so on a desktop the
// number was mostly VS Code / Docker / Slack noise and tracked nothing the
// user did. This counts pane-owned listeners only ("servers I started here")
// and hides at zero like every other quiet-when-empty footer chip; the raw
// list stays one click away (the popover hosts the full PortPanel, whose
// "Just my panes" filter covers the noisy rest).
function PortsChip() {
  const ports = useStore((s) => s.ports);
  const workspace = useStore(s => s.activeWorkspace);
  const backend = useStore(s => s.activeBackend);
  const workspaces = useStore(s => s.workspaces);
  const [open, setOpen] = useState(false);
  const btnRef = useRef<HTMLButtonElement | null>(null);
  const mine = workspacePorts(ports,workspace,backend,workspaces).filter(isWebService).sort((a,b) => a.port-b.port);
  if (mine.length === 0) return null;

  const shown = mine.slice(0, 3);
  const preview = shown.map((p) => `:${p.port} ${primaryLabel(p)}`).join(' · ');
  const more = mine.length - shown.length;
  const title =
    `Services in this workspace — ${preview}${more > 0 ? ` +${more} more` : ''}. ` +
    'Click for every listener.';
  return (
    <>
      <span className="statusbar-sep">·</span>
      <Tooltip label={title} side="top">
        <button
          ref={btnRef}
          type="button"
          className="statusbar-ports"
          aria-label={title}
          onClick={() => setOpen(!open)}
        >
          {mine.length} service{mine.length === 1 ? '' : 's'}
        </button>
      </Tooltip>
      {open && btnRef.current && (
        <PortsPopover anchor={btnRef.current} onClose={() => setOpen(false)} />
      )}
    </>
  );
}

// StatusBar is a thin always-visible row at the bottom of the app. Left side
// shows context about the currently focused pane; right side shows global
// info (workspace, counts, daemon health). All data is already in the store
// except /api/health which we poll every 10s.
export function StatusBar() {

  const teamFeatures = useOperatorFeatures();
  // Narrow subscription: only re-render when the FOCUSED pane's
  // displayed fields change (pane/port counts live in their own chips).
  // Selector returns PRIMITIVE fields only — useShallow compares
  // top-level via Object.is, so any nested object would fail the
  // equality check on each render. Flattening to primitives means
  // identical data returns a shallow-equal object → skip re-render.
  const {
    focusedTitle,
    focusedCwd,
    focusedLiveCwd,
    focusedFirstCmd,
    focusedState,
    focusedStatus,
    focusedPid,
  } = useStore(
    useShallow((s) => {
      const target=s.mainPresentation.kind === 'owner' ? s.mainPresentation.pane?.id : s.focusedPaneId;
      const p = target ? s.panes[target] : null;
      return {
        focusedTitle: p?.title ?? null,
        focusedCwd: p?.cwd ?? null,
        focusedLiveCwd: p?.liveCwd ?? null,
        focusedFirstCmd: p?.cmd?.[0] ?? null,
        focusedState: p?.state ?? null,
        focusedStatus: p?.status ?? null,
        focusedPid: p?.pid ?? null,
      };
    }),
  );
  const hasFocus = focusedTitle !== null || focusedStatus !== null;
  const showFocusedPid = typeof focusedPid === 'number' && focusedPid > 0;
  // Installer (desktop-shell) update, detected CLIENT-side so the chip lights up
  // ambiently on any topology — including this Linux/WSL daemon under a Windows
  // shell, where the daemon can't see the shell. No need to open the popover to
  // learn a new installer is out.
  const shellUpdate = useShellUpdate();
  const showDiagnostics = import.meta.env.DEV;

  // Shared daemon-health poll (singleton — see lib/useDaemonHealth.ts).
  // StatusBar and DaemonHealthBanner used to each run their own 10s
  // api.health() interval, doubling requests and drifting their offline
  // clocks apart; both now read this one source of truth.
  const { health, lastOkMs } = useDaemonHealth();
  const [nowMs, setNowMs] = useState(() => Date.now());
  const updateStatus = useStore((s) => s.updateStatus);
  const updating = updateStatus?.applying === true && updateStatus.workflowDeferred !== true;
  const updateRequestInFlight = useStore((s) => s.updateApplyRequestInFlight);
  const refreshUpdateStatus = useStore((s) => s.refreshUpdateStatus);
  const deferredUpdate = useStore((s) => s.deferredUpdate);
  const holdPresentation = deriveUpdateHoldPresentation(updateStatus, deferredUpdate);
  const popoverOpen = useStore((s) => s.versionPopoverOpen);
  const popoverSource = useStore((s) => s.versionPopoverSource);
  const setPopoverOpen = useStore((s) => s.setVersionPopoverOpen);
  const versionBtnRef = useRef<HTMLButtonElement | null>(null);
  const preview = usePreviewChannel();

  // The clock advances only for an offline timer or a visible quiet-window
  // retry countdown; healthy idle status bars remain render-stable.
  // (Mirrors DaemonHealthBanner's "only tick while visible" gate.)
  useEffect(() => {
    if (health && updateStatus?.workflowDeferred !== true && !holdPresentation.workflowBackoff) return;
    // Catch up immediately on transition to offline so the first render
    // shows the right elapsed time rather than a stale `nowMs`.
    setNowMs(Date.now());
    const clockTimer = setInterval(() => setNowMs(Date.now()), 1000);
    return () => clearInterval(clockTimer);
  }, [health, updateStatus?.workflowDeferred, holdPresentation.workflowBackoff,
    holdPresentation.workflowNextRetryMs]);

  const stateLabel = focusedState === 'awaiting-input'
    ? 'awaiting input'
    : focusedState === 'choice-prompt'
      ? 'approval needed'
      : focusedState || (focusedStatus === 'exited' ? 'exited' : '—');
  const showOffline = !updating && !health && nowMs - lastOkMs >= HEALTH_SHOW_OFFLINE_AFTER_MS;

  return (
    <div className="statusbar">
      <div className="statusbar-left">
        {hasFocus ? (
          <>
            <span className={`statusbar-dot state-${focusedState || 'idle'}`} />
            <span className="statusbar-strong">{focusedTitle || focusedFirstCmd || ''}</span>
            <span className="statusbar-sep">·</span>
            <Tooltip label={focusedLiveCwd || focusedCwd || ''} side="top">
              <span className="statusbar-path">
                {abbreviateHome(focusedLiveCwd || focusedCwd || '')}
              </span>
            </Tooltip>
            <span className="statusbar-sep">·</span>
            <span className="statusbar-dim">{stateLabel}</span>
            {showFocusedPid && (
              <>
                <span className="statusbar-sep">·</span>
                <span className="statusbar-dim">pid {focusedPid}</span>
              </>
            )}
          </>
        ) : null}
      </div>
      <div className="statusbar-right">
        {/* Panes aggregate — flips to "N need you" when panes are waiting
            on the user; popover carries the breakdown + exited cleanup. */}
        <PanesChip />
        {/* Pane-owned listeners only — renders its own leading separator,
            and nothing when no pane runs a server. */}
        <PortsChip />
        {/* Cloud-vs-local connection chip — renders its own leading
            separator; hidden only while cloud mode is still detecting. */}
        <ConnChip />
        {/* Desktop-only backend chip ("WSL (Linux)" vs "Windows
            (PowerShell)") — renders its own leading separator, and nothing
            outside the Tauri shell. Warning-tinted on a WSL→host fallback. */}
        <BackendChip health={health} />
        <SystemChip health={health} />
        {/* Desktop-only phone-access status — renders its own leading
            separator, and nothing outside the Tauri shell. */}
        <PhoneAccessChip />
        {/* Chips registered by optional features (extensions.ts) — each
            renders its own leading separator, or nothing. */}
        {slotList('statusBarChips').map((Chip, i) => <Chip key={i} />)}
        {/* Tasks console is team-only on the public beta. */}

        {/* Release train readiness is founder tooling. */}
        {teamFeatures && <ReleaseChip health={health} />}
        <span className="statusbar-sep">·</span>
        {updating ? (
          <span className="statusbar-dim">daemon updating…</span>
        ) : health ? (() => {
          // This chip is the ONE canonical "update available" signal (the old
          // top-left pill was deleted). When an update lands it goes from a dim
          // "solud {ver}" label to a full accent-tinted "↑ Update v{latest}"
          // with a single arrival pulse — loud enough to not miss, calm enough
          // to live with. Queued = a typed safety hold is active (breathing).
          // Same installer verdict as the banner and popover: one that carries
          // an older daemon than this one cannot install, so it lights nothing.
          const installerReady = installerUpdate(updateStatus, shellUpdate.current).actionable && shellUpdate.available;
          const hasUpdate = !!updateStatus?.available || installerReady;
          const workflowBackoff = holdPresentation.workflowBackoff;
          const workflowDeferred = updateStatus?.workflowDeferred === true;
          const queued = (!!deferredUpdate || workflowDeferred) && !!updateStatus?.available;
          const queuedCause = holdPresentation.cause;
          const retryDelta = Math.max(0, (holdPresentation.workflowNextRetryMs ?? nowMs) - nowMs);
          const retryLabel = retryDelta < 60_000
            ? `${Math.ceil(retryDelta / 1000)}s`
            : `${Math.ceil(retryDelta / 60_000)}m`;
          const workflowCount = holdPresentation.workflowInflight;
          const workflowDeadline = holdPresentation.workflowDeferDeadlineMs;
          const drainDeadline = workflowDeadline
            ? ` · ${formatDrainCountdown(workflowDeadline, nowMs)}`
            : '';
          const queuedLabel = workflowBackoff
            ? `Update deferred — retry in ${retryLabel}`
            : queuedCause === 'focus'
            ? `Focus${deferredUpdate?.paneId ? ` ${deferredUpdate.paneId}` : ''} active`
            : queuedCause === 'workflow'
              ? `Workflow drain${workflowCount ? ` (${workflowCount})` : ''}${drainDeadline}`
              : 'Waiting on active work';
          const queuedWorking = queued && (updateRequestInFlight || updating);
          // A daemon update shows its target version; a shell-ONLY (installer)
          // update is a separate track that usually trails the daemon, so a
          // version there reads as a downgrade — label it plainly instead.
          const shellOnly = !updateStatus?.available && installerReady;
          const cls = [
            'statusbar-version',
            hasUpdate ? 'statusbar-version-has-update' : '',
            queued ? 'statusbar-version-queued' : '',
          ].filter(Boolean).join(' ');
          return (
            <Tooltip
              // No tooltip while the popover is open: it would sit on top of it.
              label={popoverOpen ? '' :
                hasUpdate
                  ? queued
                    ? queuedLabel
                    : shellOnly
                    ? 'Desktop installer update available — click for details'
                    : `Update v${updateStatus?.latest} available — click for what's new`
                  : 'Version and updates'
              }
              side="top"
            >
              <button
                ref={versionBtnRef}
                type="button"
                className={cls}
                onClick={() => {
                  const next = !popoverOpen;
                  if (next) void refreshUpdateStatus();
                  setPopoverOpen(next);
                }}
              >
                {queued ? (
                  <>
                    <IconClock
                      size={12}
                      className={`statusbar-version-arrow${queuedWorking ? ' statusbar-version-spinner' : ''}`}
                    />
                    <span className="statusbar-version-label">{queuedLabel}</span>
                  </>
                ) : hasUpdate ? (
                  <>
                    <IconArrowUp size={12} className="statusbar-version-arrow" />
                    {shellOnly ? 'Update installer' : `Update v${updateStatus?.latest}`}
                  </>
                ) : (
                  <>
                    v{health.version}
                    {isPreviewVersion(health.version) && <span className="statusbar-preview-tag">PREVIEW</span>}
                    {(preview?.newChanges ?? 0) > 0 && !preview?.job && (
                      <span className="statusbar-preview-new" aria-label="A newer preview is ready" />
                    )}
                  </>
                )}
              </button>
            </Tooltip>
          );
        })() : showOffline ? (
          <span className="statusbar-dim statusbar-offline">● daemon unreachable</span>
        ) : (
          <span className="statusbar-dim">daemon reconnecting…</span>
        )}
        {popoverOpen && popoverSource === 'statusbar' && versionBtnRef.current && (
          <VersionPopover
            anchor={versionBtnRef.current}
            status={updateStatus}
            currentFallback={health?.version || ''}
            placement="up"
            onClose={() => setPopoverOpen(false)}
          />
        )}
        {showDiagnostics && (
          <>
            <span className="statusbar-sep">·</span>
            {/* Toggle for the resize-scroll diagnostic overlay. Clickable
                entry point in case Alt+Shift+D is eaten by xterm's focused
                textarea — this always works regardless of where focus is. */}
            <Tooltip label="Toggle scroll-resize diagnostic overlay (Alt+Shift+D)" side="top">
              <button
                type="button"
                onClick={toggleScrollDiag}
                style={{
                  background: 'transparent', border: 'none', padding: '0 4px',
                  color: 'var(--fg-dim)', fontSize: 10, cursor: 'pointer',
                  fontFamily: 'inherit',
                }}
              >diag</button>
            </Tooltip>
          </>
        )}
        {/* Feedback moved to the topbar utility toolbar (grouped with the other
            global actions); the footer is pure status now. Still reachable via
            the Ctrl+K "Send feedback" command. */}
      </div>
    </div>
  );
}
