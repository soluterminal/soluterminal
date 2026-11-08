import { useEffect, useState } from 'react';
import { IconKey } from './ui/icons';
import { useDaemonHealth } from '../lib/useDaemonHealth';
import { isDesktopShell } from '../cloudPair';
import { activeBackendFromUrl, isRouterMode } from '../dualBackend';
import { codexNextStepPending, setCodexNextStep, openCodexSignIn, openCodexPane, type CodexNextStepSide } from '../codexBootstrap';
import { effectivePaneAgent } from '../lib/agentKind';
import { useStore } from '../store';

// Lasting next step after the Codex installer finishes. The installer pane only
// says "Open a new pane to use Codex" and the toast that offers "Log in" lasts
// 12 s, while the Start buttons vanish once any pane is open. This stays until
// the user dismisses it or a Codex pane is running — the parallel of
// ClaudeInstallBanner's lasting prompt. Desktop-shell only: opening a pane for
// the local daemon is not offered over the relay.
export function CodexNextStepBanner() {
  const { health } = useDaemonHealth();
  const codexRunning = useStore((s) =>
    Object.values(s.panes).some((p) => p.status === 'running' && effectivePaneAgent(p) === 'codex'),
  );
  const storeBackend = useStore((s) => s.activeBackend);
  // Dual mode keeps a separate Codex per side; the step belongs to the side
  // that was installed.
  const side: CodexNextStepSide = isRouterMode() ? (storeBackend ?? activeBackendFromUrl()) : 'local';
  const [pending, setPending] = useState(() => codexNextStepPending(side));
  useEffect(() => {
    const sync = () => setPending(codexNextStepPending(side));
    sync();
    window.addEventListener('solu:codex-next-step', sync);
    return () => window.removeEventListener('solu:codex-next-step', sync);
  }, [side]);

  // A running Codex pane means the step is done: stop showing it for good.
  useEffect(() => {
    if (pending && codexRunning) setCodexNextStep(side, false);
  }, [pending, codexRunning]);

  if (!isDesktopShell() || !pending || codexRunning) return null;
  // Codex went away again (uninstalled): nothing to start.
  if (health?.codexChecked === true && health?.codexInstalled === false) return null;

  const needsLogin = health?.codexAuthed !== true;
  return (
    <div className="phone-access-banner" role="status">
      <div style={{ display: 'flex', flexDirection: 'column', flex: '1 1 auto', minWidth: 0 }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8 }}>
          <span className="phone-access-banner-text">
            <span aria-hidden><IconKey size={13} style={{ verticalAlign: '-2px' }} /></span> Codex is installed.
            <span className="phone-access-banner-sub">
              {needsLogin ? ' Open a new pane to log in.' : ' Open a new pane to start it.'}
            </span>
          </span>
          <span className="phone-access-banner-actions">
            <button
              type="button"
              className="phone-access-banner-btn primary"
              onClick={() => (needsLogin ? openCodexSignIn() : openCodexPane())}
            >
              {needsLogin ? 'Log in' : 'Start Codex'}
            </button>
            <button
              type="button"
              className="phone-access-banner-btn ghost"
              title="Dismiss"
              aria-label="Dismiss"
              onClick={() => setCodexNextStep(side, false)}
            >
              ×
            </button>
          </span>
        </div>
      </div>
    </div>
  );
}
