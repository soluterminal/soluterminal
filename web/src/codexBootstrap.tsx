import { useState } from 'react';
import { useStore } from './store';
import { api, type ClaudeInstallSpec } from './api';
import { refreshDaemonHealth } from './lib/useDaemonHealth';
import { isRouterMode, activeBackendFromUrl, type BackendId } from './dualBackend';
import { markSetupStarted } from './lib/setupActivity';
import { setAgentNextStep } from './lib/agentNextStep';

// Codex install + sign-in actions for the setup checklist. The parallel of
// claudeBootstrap.tsx (same two-step disclosure: Review install → confirm card
// → run in a new pane → re-check → Sign in), kept as its own copy per the
// per-provider convention. Status comes from the daemon's cached probe
// (daemon/codex_bootstrap.go) via the recheck endpoint and /api/health.

export const CODEX_INSTALL_PANE_TITLE = 'Install Codex';
export const CODEX_LOGIN_PANE_TITLE = 'Sign in to Codex';

// Faster than Claude's first check: the checklist stays open while Codex
// installs, so the row should turn green within seconds. Each recheck is one
// coalesced login shell on the daemon. Total window ~9 min.
export const CODEX_RECHECK_BACKOFF_MS = [
  5_000, 5_000, 5_000, 10_000, 10_000, 15_000, 20_000, 30_000, 45_000, 60_000, 90_000, 120_000, 120_000,
];
let recheckToken = 0;

export function scheduleCodexRecheck(kind: 'install' | 'login'): void {
  // Pin the backend at schedule time so a mid-loop switch in Dual mode can't
  // re-aim the polls at the other daemon (see scheduleClaudeRecheck).
  const backend: BackendId | null = isRouterMode()
    ? (useStore.getState().activeBackend ?? activeBackendFromUrl())
    : null;
  scheduleLoop(kind, backend);
}

function scheduleLoop(kind: 'install' | 'login', backend: BackendId | null): void {
  const token = ++recheckToken;
  const step = (i: number) => {
    if (token !== recheckToken || i >= CODEX_RECHECK_BACKOFF_MS.length) return;
    window.setTimeout(() => {
      if (token !== recheckToken) return;
      void api
        .codexRecheckOn(backend)
        .then((st) => {
          refreshDaemonHealth();
          const done = kind === 'install' ? st.codexInstalled === true : st.codexAuthed === true;
          if (!done) {
            step(i + 1);
          } else if (kind === 'install') {
            // Installed: keep the start card on screen after the toast is gone.
            setAgentNextStep(backend ?? 'local', true);
            if (st.codexAuthed !== true) {
              // Say so once, offer the next step, and keep watching for a
              // sign-in done straight in the installer pane.
              useStore.getState().toast(
                'Codex is installed. Next, log in. Panes that were already open won’t see Codex, so use a new pane.',
                { kind: 'success', icon: '✓', ttlMs: 12000, action: { label: 'Log in', run: () => openCodexSignIn() } },
              );
              scheduleLoop('login', backend);
            }
          }
        })
        .catch((e: unknown) => {
          const status = (e as { status?: number } | null)?.status;
          // 403 (relay) / 404 (older daemon): no recheck here at all.
          if (status === 403 || status === 404) return;
          step(i + 1);
        });
    }, CODEX_RECHECK_BACKOFF_MS[i]);
  };
  step(0);
}

// openCodexSignIn opens a new pane running `codex`, whose first run walks the
// user through signing in. Module-level so the "installed" toast can offer it.
export function openCodexSignIn(onSpawn?: () => void): void {
  markSetupStarted();
  const existing = Object.values(useStore.getState().panes).find((p) => p.title === CODEX_LOGIN_PANE_TITLE);
  if (existing) {
    useStore.getState().toast('A Codex sign-in pane is already open.', { kind: 'info', ttlMs: 6000 });
    scheduleCodexRecheck('login');
    onSpawn?.();
    return;
  }
  void useStore
    .getState()
    .createPane({ cwd: '', cmd: ['codex'], title: CODEX_LOGIN_PANE_TITLE })
    .then(() => {
      useStore.getState().toast(
        'Codex opened in a new pane. Choose “Sign in with ChatGPT” and finish in the browser window that opens. If no window opens, open the link shown in the pane.',
        { kind: 'info', ttlMs: 14000 },
      );
      scheduleCodexRecheck('login');
      onSpawn?.();
    })
    .catch(() => {
      useStore.getState().toast('Couldn’t open a pane for signing in. Please try again.', {
        kind: 'info',
        icon: '!',
        ttlMs: 8000,
      });
    });
}

// useCodexInstall owns the confirm-card lifecycle. onSpawn fires after a
// helper pane opens so the caller can react (the checklist stays open for an
// install and closes for sign-in, which needs typing in the pane).
export function useCodexInstall(opts?: { onSpawn?: (kind: 'install' | 'login') => void }): {
  installing: boolean;
  pendingInstall: ClaudeInstallSpec | null;
  startInstall: () => void;
  confirmInstall: () => void;
  cancelInstall: () => void;
  loginCodex: () => void;
} {
  const onSpawn = opts?.onSpawn;
  const [installing, setInstalling] = useState(false);
  const [pendingInstall, setPendingInstall] = useState<ClaudeInstallSpec | null>(null);

  const startInstall = () => {
    if (installing) return;
    setInstalling(true);
    void api
      .codexInstallCommand()
      .then((spec) => {
        if (!spec.supported) {
          useStore.getState().toast('Codex can’t be installed automatically here. See soluterminal.com/help.', {
            kind: 'info',
            icon: '!',
            ttlMs: 9000,
          });
          return;
        }
        setPendingInstall(spec);
      })
      .catch((e: unknown) => {
        const status = (e as { status?: number } | null)?.status;
        useStore.getState().toast(
          status === 403
            ? 'Install Codex from the computer itself, not from your phone.'
            : 'Couldn’t reach SoluTerminal to start the installer. Please try again.',
          { kind: 'info', icon: '!', ttlMs: 8000 },
        );
      })
      .finally(() => setInstalling(false));
  };

  const confirmInstall = () => {
    const spec = pendingInstall;
    if (!spec) return;
    setPendingInstall(null);
    markSetupStarted();
    const existing = Object.values(useStore.getState().panes).find((p) => p.title === CODEX_INSTALL_PANE_TITLE);
    if (existing) {
      useStore.getState().toast('Codex is already installing. See the “Install Codex” pane.', {
        kind: 'info',
        ttlMs: 7000,
      });
      scheduleCodexRecheck('install');
      onSpawn?.('install');
      return;
    }
    void useStore
      .getState()
      .createPane({ cwd: '', cmd: spec.cmd, title: CODEX_INSTALL_PANE_TITLE })
      .then(() => {
        useStore.getState().toast('Installing Codex in a new pane. This list updates by itself when it’s done.', {
          kind: 'info',
          ttlMs: 9000,
        });
        scheduleCodexRecheck('install');
        onSpawn?.('install');
      })
      .catch(() => {
        useStore.getState().toast('Couldn’t open the installer pane. Please try again.', {
          kind: 'info',
          icon: '!',
          ttlMs: 8000,
        });
      });
  };

  const cancelInstall = () => setPendingInstall(null);
  const loginCodex = () => openCodexSignIn(() => onSpawn?.('login'));

  return { installing, pendingInstall, startInstall, confirmInstall, cancelInstall, loginCodex };
}
