import { useEffect, useState } from 'react';
import { IconAlertTriangle, IconKey, IconSparkle } from './ui/icons';
import { api } from '../api';
import { useDaemonHealth, refreshDaemonHealth } from '../lib/useDaemonHealth';
import { isDesktopShell } from '../cloudPair';
import { useClaudeInstall, ClaudeInstallConfirmCard } from '../claudeBootstrap';
import { activeBackendFromUrl, isRouterMode } from '../dualBackend';
import { setupBannerHidden, hideSetupBanner } from '../lib/setupBannerPrefs';
import { useStore } from '../store';
import { useTeamFeatures } from '../entitlements';
import { sectionOffered } from './sidebar/sidebarSections';
import { openCodexSignIn } from '../codexBootstrap';
import { AGENT_NAMES, agentReadiness, startAgent, type StartableAgent } from '../agentStart';
import { effectivePaneAgent } from '../lib/agentKind';
import {
  AGENT_NEXT_STEP_EVENT,
  agentNextStepPending,
  firstAgentStarted,
  markFirstAgentStarted,
} from '../lib/agentNextStep';

// Always-on agent setup banner. First, the lasting next step after an agent
// install: one start card for Claude Code and Codex that stays (a dismiss
// lasts the session) until the first agent pane has run on this computer, then
// never returns. The installer pane only says to open a new pane, the install
// toasts last seconds, and the empty window's Start buttons vanish once any
// pane is open. Otherwise, the two states that dead-end a Claude pane: not
// installed, and installed-but-not-signed-in.
//
// WHY THIS EXISTS: the setup checklist (which also surfaces this) only opens on
// FIRST RUN (localStorage `solu.seenChecklist`), and that flag survives a
// reinstall — localStorage at 127.0.0.1:8787 isn't cleared when the desktop app
// is uninstalled. So a returning user who lacks `claude` (or hasn't signed in)
// never re-saw the checklist and got NO prompt. This banner closes that gap: it
// watches the daemon's live claude status and surfaces the guided install / the
// sign-in nudge whenever they're needed — independent of first-run state.
//
// Scope: desktop-shell only (install/login act on the LOCAL daemon — 403 over
// the relay — and are never relevant on the phone/web). In cloud launch mode the
// webview's /api/health is the cloud server, which has no claude* fields →
// claudeChecked is undefined → the banner stays hidden. The "not signed in"
// state never fires on macOS: the daemon reports authed conservatively there
// (Keychain creds aren't file-readable), so we never false-nag a signed-in Mac.

type Need = 'start' | 'install' | 'login' | null;

const START_ORDER: StartableAgent[] = ['claude', 'codex'];

type BackendId = 'wsl' | 'win';

function activeBackendForBanner(storeBackend: BackendId | null): BackendId | null {
  if (!isRouterMode()) return null;
  return storeBackend ?? activeBackendFromUrl();
}

// Names the side only in Dual mode, where Linux (WSL) and Windows PowerShell
// each need their own install and sign-in; elsewhere there is one side.
function activeBackendLabel(activeBackend: BackendId | null): string | null {
  if (!isRouterMode()) return null;
  return activeBackend === 'win' ? 'Windows PowerShell' : 'Linux (WSL)';
}

function healthBelongsToActiveBackend(
  health: ReturnType<typeof useDaemonHealth>['health'],
  activeBackend: BackendId | null,
): boolean {
  if (!isRouterMode() || !activeBackend || !health) return true;
  return health.backendID === activeBackend;
}

// hidden: the setup checklist (or the welcome tour before it) is open and
// already shows the same install / sign-in step.
export function AgentSetupBanner({ hidden = false }: { hidden?: boolean } = {}) {
  const { health } = useDaemonHealth();
  const storeActiveBackend = useStore((s) => s.activeBackend);
  // Matches the daemon's check-in rule: a Claude Code or Codex pane, not the
  // installer shell. An exited pane (e.g. a CLI that failed to launch) is not
  // a start.
  const agentRunning = useStore((s) =>
    Object.values(s.panes).some((p) => {
      if (p.status !== 'running') return false;
      const agent = effectivePaneAgent(p);
      return agent === 'claude' || agent === 'codex';
    }),
  );
  // An available Credentials section owns the install/status row.
  const team = useTeamFeatures();
  const activeBackend = activeBackendForBanner(storeActiveBackend);
  const activeKey = activeBackend ?? 'local';
  // Session-scoped dismissal (NOT persisted), keyed to the NEED that was
  // dismissed. Dismissing the install nudge suppresses only the install state;
  // when claude is then installed and the state flips to "login", the banner
  // returns for the new (un-dismissed) need. It self-clears once claude is
  // installed AND signed in. Re-shows next launch if still unresolved.
  const [dismissedKey, setDismissedKey] = useState<string | null>(null);
  const { installing, pendingInstall, startInstall, confirmInstall, cancelInstall, loginClaude } =
    useClaudeInstall({ onSpawn: (kind) => setDismissedKey(`${activeKey}:${kind}`) });
  const [startPending, setStartPending] = useState(() => agentNextStepPending(activeKey));
  const [starting, setStarting] = useState<StartableAgent | null>(null);
  useEffect(() => {
    const sync = () => setStartPending(agentNextStepPending(activeKey));
    sync();
    window.addEventListener(AGENT_NEXT_STEP_EVENT, sync);
    return () => window.removeEventListener(AGENT_NEXT_STEP_EVENT, sync);
  }, [activeKey]);

  // The first agent pane retires the start card for good, on every side. It
  // is recorded even with no card pending, so an install later on doesn't
  // offer a first start to someone who has already run an agent.
  useEffect(() => {
    if (agentRunning && !firstAgentStarted()) markFirstAgentStarted();
  }, [agentRunning]);

  const desktop = isDesktopShell();

  // Force ONE fresh probe on mount so a brand-new launch reflects ground truth
  // promptly (the cached health probe can be up to ~5 min stale; the boot probe
  // may also still be in flight). Skip it only when claude is fully ready
  // (installed AND signed in) — otherwise a fresh read is load-bearing. Read at
  // mount only; we deliberately don't re-run on every 10s health poll. 403 over
  // relay / 404 on an older daemon is swallowed, leaving the polled value intact.
  useEffect(() => {
    if (!desktop) return;
    if (
      healthBelongsToActiveBackend(health, activeBackend) &&
      health?.claudeInstalled === true &&
      health?.claudeAuthed === true
    ) return;
    let alive = true;
    void api
      .claudeRecheck()
      .then(() => { if (alive) refreshDaemonHealth(); })
      .catch(() => { /* keep the polled health value */ });
    return () => { alive = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [desktop, activeBackend]);

  if (!desktop || hidden) return null;

  // In Dual/router mode a backend switch happens in-place. The shared health
  // singleton can still hold the previous backend's last value for a moment; do
  // not render a Claude prompt from WSL while the UI label says PowerShell (or
  // vice versa). The effect above forces a fresh recheck for the active side.
  if (!healthBelongsToActiveBackend(health, activeBackend)) return null;

  // The start card offers each agent the daemon has seen installed on this
  // side; one that went away again (uninstalled) has nothing to start.
  const startable = START_ORDER.filter((agent) =>
    agent === 'claude'
      ? health?.claudeChecked === true && health?.claudeInstalled === true
      : health?.codexChecked === true && health?.codexInstalled === true,
  );
  // Only act once the daemon has actually probed (claudeChecked === true) — never
  // a premature "not installed" during the boot-probe window. Order: the start
  // card (an agent is ready to go), then install (the harder blocker), then
  // sign-in.
  const need: Need =
    startPending && !agentRunning && startable.length > 0
      ? 'start'
      : health?.claudeChecked !== true
        ? null
        : health?.claudeInstalled === false
          ? 'install'
          : health?.claudeAuthed === false
            ? 'login'
            : null;
  const needKey = need ? `${activeKey}:${need}` : null;
  // Durable opt-out ("Don't show again") outranks every need. The Credentials
  // CLI row always shows install status, so hiding never strands reinstall —
  // and the row offers "Remind me" to bring the banner back.
  // The start card has no durable opt-out: it ends at the first agent start.
  if (!need || dismissedKey === needKey) return null;
  if (need !== 'start' && setupBannerHidden('claude')) return null;

  const dismiss = () => setDismissedKey(needKey);
  const dontShowAgain = () => { hideSetupBanner('claude'); setDismissedKey(needKey); };
  const backendLabel = activeBackendLabel(activeBackend);

  if (need === 'start') {
    // A signed-out agent gets its sign-in pane, which also starts it (the
    // sign-in flows keep watching until the sign-in lands); otherwise the same
    // start as the empty window's buttons.
    const start = (agent: StartableAgent) => {
      if (starting) return;
      if (agentReadiness(agent, health, activeBackend) === 'signed-out') {
        if (agent === 'claude') loginClaude();
        else openCodexSignIn();
        return;
      }
      setStarting(agent);
      startAgent(agent)
        .catch((e: unknown) => {
          useStore.getState().toast(`Could not start ${AGENT_NAMES[agent]}: ${(e as Error).message}`, {
            kind: 'error',
            icon: '!',
          });
        })
        .finally(() => setStarting(null));
    };
    return (
      <div className="phone-access-banner" role="status">
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8, flex: '1 1 auto', minWidth: 0 }}>
          <span className="phone-access-banner-text">
            <span aria-hidden><IconSparkle size={13} style={{ verticalAlign: '-2px' }} /></span> Next, start your first agent{backendLabel ? ` in ${backendLabel}` : ''}.
          </span>
          <span className="phone-access-banner-actions">
            {startable.map((agent) => (
              <button
                key={agent}
                type="button"
                className="phone-access-banner-btn primary"
                disabled={starting !== null}
                onClick={() => start(agent)}
              >
                {starting === agent ? 'Starting…' : `Start ${AGENT_NAMES[agent]}`}
              </button>
            ))}
            <button
              type="button"
              className="phone-access-banner-btn ghost"
              title="Hide for now (it comes back until you start an agent)"
              aria-label="Dismiss"
              onClick={dismiss}
            >
              ×
            </button>
          </span>
        </div>
      </div>
    );
  }

  return (
    <div className="phone-access-banner" role="status">
      <div style={{ display: 'flex', flexDirection: 'column', gap: pendingInstall ? 6 : 0, flex: '1 1 auto', minWidth: 0 }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8 }}>
          {need === 'install' ? (
            <span className="phone-access-banner-text">
              <span aria-hidden><IconAlertTriangle size={13} style={{ verticalAlign: '-2px' }} /></span> Claude Code isn’t installed yet{backendLabel ? ` in ${backendLabel}` : ''}.
            </span>
          ) : (
            <span className="phone-access-banner-text">
              <span aria-hidden><IconKey size={13} style={{ verticalAlign: '-2px' }} /></span> Claude Code isn’t signed in yet{backendLabel ? ` in ${backendLabel}` : ''}.
              {backendLabel && (
                <span className="phone-access-banner-sub"> Linux (WSL) and Windows PowerShell each need their own sign-in.</span>
              )}
            </span>
          )}
          <span className="phone-access-banner-actions">
            {need === 'install' ? (
              <button
                type="button"
                className="phone-access-banner-btn primary"
                disabled={installing}
                onClick={startInstall}
              >
                {installing ? 'Preparing…' : 'Install'}
              </button>
            ) : (
              <button
                type="button"
                className="phone-access-banner-btn primary"
                onClick={loginClaude}
              >
                Log in
              </button>
            )}
            <button
              type="button"
              className="phone-access-banner-btn ghost"
              title="Dismiss (returns next launch if Claude Code still isn’t ready)"
              aria-label="Dismiss"
              onClick={dismiss}
            >
              ×
            </button>
            <button
              type="button"
              className="phone-access-banner-btn ghost"
              title={team && sectionOffered('credentials')
                ? "Don't remind me again (install status stays in Credentials)"
                : "Don't remind me again on this computer"}
              aria-label="Don’t show again"
              onClick={dontShowAgain}
            >
              Don’t show again
            </button>
          </span>
        </div>
        {pendingInstall && (
          <ClaudeInstallConfirmCard
            spec={pendingInstall}
            onConfirm={confirmInstall}
            onCancel={cancelInstall}
          />
        )}
      </div>
    </div>
  );
}
