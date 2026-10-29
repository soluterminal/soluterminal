import { useMemo, useState } from 'react';
import { useStore } from '../store';
import { altUrl, isDualBackend, isRouterMode, labelForUrl, primaryUrl, swapTarget } from '../dualBackend';
import { confirmDialog } from '../lib/dialog';
import { switchDesktopLaunchMode } from '../lib/launchModeSwitch';
import { useCloud } from '../cloud';
import { useRelayConfigured } from '../lib/relayConfigured';
import { IconBolt, IconCloud } from './ui/icons';
import { isEmbedded } from '../embed';

// Environment controls and state. The top bar's hosting chip (ContextPill)
// summarizes the current context (hosting · backend · side) and opens these
// segmented controls in a popover.
//
//   Row 1 — hosting: Local (direct to this machine's daemon) vs Cloud
//     (relayed through the soluterminal cloud account). In the desktop app
//     this is a SWITCH (same restart-level action as the tray "Switch to …"
//     item, via desktop_set_launch_mode); in a browser tab it's display-only
//     since the page's origin decides the hosting.
//   Row 2 — Windows desktop only: which daemon backend the shell runs
//     (Linux/WSL, Windows-native, Dual). Restart-level.
//   Row 3 — dual/router mode only: which live side is active (WSL vs
//     PowerShell). Instant.
//
// As of the nav-uplift the controls (BackendControls) and the hosting summary
// (useHostingLabel) are also embedded in the top-left ContextPill, which merges
// workspace identity with hosting into one control. BackendSwitcher itself is
// retained as the standalone tested chip and is DOM-identical to before.

type BackendId = 'wsl' | 'win';
type StartupMode = 'wsl' | 'host' | 'dual';

type SoluBackend = {
  requested?: StartupMode;
  actual?: StartupMode;
  fallback?: boolean;
  hostPlatform?: 'windows' | 'macos' | 'linux';
  launchMode?: 'local' | 'cloud';
  version?: string; // shell/installer version; absent on shells before it was added
};

type InvokeFn = (cmd: string, args?: Record<string, unknown>) => Promise<unknown>;

function desktopGlobals(): { backend: SoluBackend | null; invoke: InvokeFn | null; tauri: boolean } {
  if (isEmbedded()) return { backend: null, invoke: null, tauri: false };
  const w = window as unknown as {
    __SOLU_BACKEND__?: SoluBackend;
    __SOLU_TAURI__?: boolean;
    __TAURI_INTERNALS__?: { invoke?: InvokeFn };
  };
  const invoke = w.__SOLU_TAURI__ && typeof w.__TAURI_INTERNALS__?.invoke === 'function'
    ? w.__TAURI_INTERNALS__.invoke
    : null;
  return {
    backend: w.__SOLU_BACKEND__ ?? null,
    invoke,
    tauri: w.__SOLU_TAURI__ === true,
  };
}

function startupLabel(mode: StartupMode | undefined): string {
  switch (mode) {
    case 'wsl': return 'Linux';
    case 'host': return 'Windows';
    case 'dual': return 'Dual';
    default: return 'Mode';
  }
}

// The OS a shell/machine reports (health.shellPlatform or a cloud Machine's
// platform), e.g. "windows-amd64" / "linux-arm64" / "darwin-amd64" → a friendly
// word. Null for anything we don't recognize, so the pill just omits the glyph.
function osWordFromShellPlatform(p: string | undefined): string | null {
  if (!p) return null;
  const s = p.toLowerCase();
  if (s.startsWith('win')) return 'Windows';
  if (s.startsWith('linux')) return 'Linux';
  if (s.startsWith('darwin') || s.startsWith('mac') || s.startsWith('osx')) return 'macOS';
  return null;
}

// The desktop shell's own host OS (window.__SOLU_BACKEND__.hostPlatform).
function osWordFromHostPlatform(p: 'windows' | 'macos' | 'linux' | undefined): string | null {
  switch (p) {
    case 'windows': return 'Windows';
    case 'macos': return 'macOS';
    case 'linux': return 'Linux';
    default: return null;
  }
}

async function setDesktopBackendMode(invoke: InvokeFn | null, mode: StartupMode): Promise<void> {
  if (!invoke) throw new Error('Desktop command unavailable');
  await invoke('desktop_set_backend_mode', { mode });
}

// All derived flags + labels + handlers + pending state for the environment
// controls. Consumed by the BackendSwitcher chip (summary + fallback),
// BackendControls (the segmented rows), and the ContextPill (summary +
// actionable, to show hosting · backend · side and pick button-vs-static). Each
// consumer gets its own instance; the only shared surface is window/store
// globals, so they stay consistent by construction.
export function useBackendEnv() {
  const activeBackend = useStore((s) => s.activeBackend);
  const switchBackend = useStore((s) => s.switchBackend);
  const panes = useStore((s) => s.panes);
  const toast = useStore((s) => s.toast);
  const [pendingStartup, setPendingStartup] = useState<StartupMode | null>(null);
  const [pendingSide, setPendingSide] = useState<BackendId | null>(null);
  const [pendingConn, setPendingConn] = useState<'local' | 'cloud' | null>(null);

  const { backend: desktopBackend, invoke, tauri } = desktopGlobals();
  const routerMode = isRouterMode();
  const dualMode = isDualBackend();
  const showStartup = tauri
    && !!desktopBackend
    && desktopBackend.hostPlatform === 'windows'
    && desktopBackend.launchMode !== 'cloud';
  const showRouterSide = routerMode;
  const showLegacySwap = !routerMode && dualMode;

  const cloudMode = useCloud((s) => s.mode);
  const cloudIsLocal = useCloud((s) => s.isLocal);
  const machines = useCloud((s) => s.machines);
  const selectedMachineId = useCloud((s) => s.selectedMachineId);
  const machineName = useMemo(
    () => machines.find((m) => m.id === selectedMachineId)?.name ?? '',
    [machines, selectedMachineId],
  );
  const connActive: 'local' | 'cloud' = desktopBackend?.launchMode
    ?? (cloudMode === 'cloud' && !cloudIsLocal ? 'cloud' : 'local');
  // The Cloud side exists only when this build has a hosted cloud: its
  // daemon then has a relay (lib/relayConfigured). Without one the shell
  // refuses the switch, so the Hosting row isn't offered at all.
  const relayConfigured = useRelayConfigured();
  const cloudOffered = relayConfigured !== false || desktopBackend?.launchMode === 'cloud';
  const showConnSwitch = tauri && !!invoke && cloudOffered;
  const showConnDisplay = !showConnSwitch && cloudMode !== 'detecting';

  const paneCounts = useMemo(() => {
    let wsl = 0;
    let win = 0;
    for (const pane of Object.values(panes)) {
      if (pane.backend === 'wsl') wsl += 1;
      if (pane.backend === 'win') win += 1;
    }
    return { wsl, win };
  }, [panes]);

  const startup = desktopBackend?.requested ?? desktopBackend?.actual;
  const actual = desktopBackend?.actual;
  const fallback = desktopBackend?.fallback === true;
  const activeStartup = fallback ? actual : startup;

  // OS/backend word for an at-a-glance readout on the context pill (the pill
  // keeps its visible text short, so this rides along as a quiet secondary
  // token). Windows desktop reports the chosen backend (Linux/WSL · Windows ·
  // Dual); cloud reports the selected remote machine's OS; a Linux/macOS desktop
  // reports its own host OS. Null when unknown (still detecting / bare browser
  // tab) so the pill simply omits it.
  const selectedShellPlatform = machines.find((m) => m.id === selectedMachineId)?.shellPlatform;
  let osWord: string | null = null;
  if (showStartup && activeStartup) osWord = startupLabel(activeStartup);
  else if (connActive === 'cloud') osWord = osWordFromShellPlatform(selectedShellPlatform);
  else if (desktopBackend?.hostPlatform) osWord = osWordFromHostPlatform(desktopBackend.hostPlatform);

  const requestStartup = async (mode: StartupMode) => {
    if (mode === activeStartup || pendingStartup) return;
    const ok = await confirmDialog({
      title: `Switch to ${startupLabel(mode)}`,
      message: `Restart soluterminal in ${startupLabel(mode)} mode?\n\nThe desktop shell and daemon connections will restart.`,
      confirmLabel: 'Restart',
    });
    if (!ok) return;
    setPendingStartup(mode);
    try {
      await setDesktopBackendMode(invoke, mode);
    } catch (err) {
      setPendingStartup(null);
      toast(`Couldn't switch mode: ${(err as Error).message}`, { kind: 'error', icon: '!', ttlMs: 6000 });
    }
  };

  const requestSide = async (side: BackendId) => {
    if (side === activeBackend || pendingSide) return;
    setPendingSide(side);
    try {
      await switchBackend(side);
    } catch (err) {
      toast(`Couldn't switch side: ${(err as Error).message}`, { kind: 'error', icon: '!', ttlMs: 6000 });
    } finally {
      setPendingSide(null);
    }
  };

  const requestConn = async (target: 'local' | 'cloud') => {
    if (target === connActive || pendingConn || !invoke) return;
    // Shared with the cloud-mode installer prompts (lib/launchModeSwitch.ts).
    const switched = await switchDesktopLaunchMode(target, toast, () => setPendingConn(target));
    if (!switched) setPendingConn(null);
  };

  const legacyActiveLabel = labelForUrl(primaryUrl());
  const legacyTargetLabel = labelForUrl(altUrl());
  const legacyTarget = swapTarget();

  // A control is offered only when there's something to change. Everything
  // else is a display-only chip (browser tabs, where the origin fixes hosting).
  const actionable = showConnSwitch || showStartup || showRouterSide || showLegacySwap;
  const hostingLabel = connActive === 'cloud'
    ? `Cloud${machineName ? ` · ${machineName}` : ''}`
    : 'Local';
  // The hosting mark rides ahead of the compound summary as an SVG (it always
  // leads when shown); the label text itself stays glyph-free so it joins the
  // rest of the summary cleanly and reads plainly in aria.
  const hostingIcon = connActive === 'cloud'
    ? <IconCloud size={12} style={{ verticalAlign: '-2px' }} />
    : <IconBolt size={12} style={{ verticalAlign: '-2px' }} />;
  const hostingLeads = showConnSwitch || showConnDisplay;
  const summaryParts: string[] = [];
  if (showConnSwitch || showConnDisplay) summaryParts.push(hostingLabel);
  if (showStartup) summaryParts.push(startupLabel(activeStartup));
  if (showRouterSide) summaryParts.push(activeBackend === 'win' ? 'PowerShell' : 'WSL');
  if (showLegacySwap && !showConnSwitch && !showStartup) summaryParts.push(legacyActiveLabel);
  const summary = summaryParts.join(' · ') || 'Environment';
  const nothingToRender = !showConnSwitch && !showConnDisplay && !showStartup && !showRouterSide && !showLegacySwap;

  return {
    activeBackend,
    pendingStartup, pendingSide, pendingConn,
    showStartup, showRouterSide, showLegacySwap, showConnSwitch, showConnDisplay,
    connActive, machineName, paneCounts,
    actual, fallback, activeStartup, osWord,
    requestStartup, requestSide, requestConn,
    legacyActiveLabel, legacyTargetLabel, legacyTarget,
    actionable, summary, nothingToRender,
    hostingIcon, hostingLeads,
  };
}

// The segmented control rows only — no chip, no fallback note. Rendered inside
// the BackendSwitcher popover AND inside the ContextPill popover. Returns null
// when nothing is actionable (e.g. a browser tab, where hosting is fixed).
export function BackendControls() {
  const env = useBackendEnv();
  if (!env.actionable) return null;
  return (
    <div className="env-popover">
      {env.showConnSwitch && (
        <div className="env-popover-group">
          <div className="env-popover-label">Hosting</div>
          <div className="backend-mode-row" aria-label="Hosting">
            <button
              type="button"
              className={`backend-mode-seg backend-conn-local${env.connActive === 'local' ? ' active' : ''}`}
              disabled={env.connActive === 'local' || env.pendingConn !== null}
              title="Local — connect directly to the daemon on this computer"
              aria-label="Local"
              onClick={() => { void env.requestConn('local'); }}
            >
              {env.pendingConn === 'local' ? '...' : <><IconBolt size={13} />Local</>}
            </button>
            <button
              type="button"
              className={`backend-mode-seg backend-conn-cloud${env.connActive === 'cloud' ? ' active' : ''}`}
              disabled={env.connActive === 'cloud' || env.pendingConn !== null}
              title={`Cloud — connect through your soluterminal cloud account to any of your machines${env.machineName ? ` (currently: ${env.machineName})` : ''}`}
              aria-label="Cloud"
              onClick={() => { void env.requestConn('cloud'); }}
            >
              {env.pendingConn === 'cloud' ? '...' : <><IconCloud size={13} />Cloud</>}
            </button>
          </div>
        </div>
      )}

      {env.showStartup && (
        <div className="env-popover-group">
          <div className="env-popover-label">Backend</div>
          <div className="backend-mode-row" aria-label="Startup mode">
            {(['wsl', 'host', 'dual'] as StartupMode[]).map((mode) => (
              <button
                key={mode}
                type="button"
                className={`backend-mode-seg${mode === env.activeStartup ? ' active' : ''}`}
                disabled={mode === env.activeStartup || env.pendingStartup !== null}
                title={`Restart in ${startupLabel(mode)} mode`}
                onClick={() => { void env.requestStartup(mode); }}
              >
                {env.pendingStartup === mode ? '...' : startupLabel(mode)}
              </button>
            ))}
          </div>
        </div>
      )}

      {env.showRouterSide && (
        <div className="env-popover-group">
          <div className="env-popover-label">Active side</div>
          <div className="backend-mode-row backend-side-row" aria-label="Active dual side">
            <button
              type="button"
              className={`backend-mode-seg backend-side-wsl${env.activeBackend === 'wsl' ? ' active' : ''}`}
              disabled={env.activeBackend === 'wsl' || env.pendingSide !== null}
              title={`${env.paneCounts.wsl} WSL pane${env.paneCounts.wsl === 1 ? '' : 's'}`}
              onClick={() => { void env.requestSide('wsl'); }}
            >
              WSL
            </button>
            <button
              type="button"
              className={`backend-mode-seg backend-side-win${env.activeBackend === 'win' ? ' active' : ''}`}
              disabled={env.activeBackend === 'win' || env.pendingSide !== null}
              title={`${env.paneCounts.win} PowerShell pane${env.paneCounts.win === 1 ? '' : 's'}`}
              onClick={() => { void env.requestSide('win'); }}
            >
              PowerShell
            </button>
          </div>
        </div>
      )}

      {env.showLegacySwap && (
        <button
          type="button"
          className="backend-legacy-swap"
          title={`Now on ${env.legacyActiveLabel}. Open ${env.legacyTargetLabel}.`}
          disabled={!env.legacyTarget}
          onClick={() => {
            if (env.legacyTarget) window.location.href = env.legacyTarget;
          }}
        >
          {env.legacyActiveLabel} to {env.legacyTargetLabel}
        </button>
      )}
    </div>
  );
}
