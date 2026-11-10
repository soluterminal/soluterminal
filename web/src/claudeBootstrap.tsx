import { useState } from 'react';
import { useStore } from './store';
import { api, type ClaudeInstallSpec } from './api';
import { refreshDaemonHealth } from './lib/useDaemonHealth';
import { isRouterMode, activeBackendFromUrl, type BackendId } from './dualBackend';
import { markSetupStarted } from './lib/setupActivity';
import { setAgentNextStep } from './lib/agentNextStep';

// Shared Claude-Code bootstrap actions — the install (guided curl|bash with a
// disclosure confirm card) and sign-in flows. Extracted from SetupChecklist so
// the always-on AgentSetupBanner reuses the EXACT same, already-reviewed
// flow (no second copy to drift). Status reading lives with each caller: the
// checklist force-probes on open; the banner reads the shared daemon-health
// poll (useDaemonHealth) — both ultimately reflect the daemon's cached probe.

// Titles of the spawned helper panes — also used to de-dupe (don't open a
// second installer / sign-in pane while one is live).
export const INSTALL_PANE_TITLE = 'Install Claude Code';
export const LOGIN_PANE_TITLE = 'Sign in to Claude';

// After a helper pane spawns, poll the daemon's probe on a backoff until the
// state it was meant to change actually flips (install → installed; login →
// authed). Without this the ONLY refresh is the daemon's 5-minute ticker, so a
// user who just installed sat in a blind window: no sign-in nudge, and a Claude
// pane opened meanwhile dead-ended on an unauthenticated CLI. First check at
// 15s (the native installer typically finishes well inside that); total window
// ~8 min, past which we assume the attempt was abandoned and stop polling (the
// 5-min ticker and next launch still catch a late success). Each recheck is
// serialized daemon-side (claudeProbeMu), so this adds at most one login-shell
// probe per tick.
export const CLAUDE_RECHECK_BACKOFF_MS = [
  15_000, 20_000, 30_000, 45_000, 60_000, 90_000, 120_000, 120_000,
];
let recheckToken = 0;

export function scheduleClaudeRecheck(kind: 'install' | 'login'): void {
  // Pin the backend the flow was armed for AT SCHEDULE TIME: api.claudeRecheck
  // resolves the router prefix per call, so a mid-loop backend switch in Dual
  // mode would silently re-aim the polls at the other daemon — and stop the
  // watch on the WRONG side's status while the install it was armed for
  // completes unobserved. Single-backend setups pin null (plain fetch).
  const backend: BackendId | null = isRouterMode()
    ? (useStore.getState().activeBackend ?? activeBackendFromUrl())
    : null;
  scheduleRecheckLoop(kind, backend);
}

function scheduleRecheckLoop(kind: 'install' | 'login', backend: BackendId | null): void {
  const token = ++recheckToken; // a newer schedule supersedes any older loop
  const step = (i: number) => {
    if (token !== recheckToken || i >= CLAUDE_RECHECK_BACKOFF_MS.length) return;
    window.setTimeout(() => {
      if (token !== recheckToken) return;
      void api
        .claudeRecheckOn(backend)
        .then((st) => {
          // Push the fresh probe into the shared health poll so the banner /
          // checklist flip immediately (they read useDaemonHealth, not us).
          refreshDaemonHealth();
          const done = kind === 'install' ? st.claudeInstalled === true : st.claudeAuthed === true;
          if (!done) {
            step(i + 1);
            return;
          }
          // Installed: offer Start Claude Code until the first agent runs.
          if (kind === 'install') setAgentNextStep(backend ?? 'local', true);
          if (kind === 'install' && st.claudeAuthed !== true) {
            // Install landed but claude isn't signed in yet. The installer pane
            // drops to an interactive shell, and users often sign in right there
            // instead of clicking the banner's "Log in" — keep watching (same
            // pinned backend) so the nudge clears either way.
            scheduleRecheckLoop('login', backend);
          }
        })
        .catch((e: unknown) => {
          const status = (e as { status?: number } | null)?.status;
          // 403 (relay-forwarded) / 404 (older daemon): the recheck endpoint
          // isn't available on this surface at all — stop; the daemon's 5-min
          // ticker still owns those cases. Anything else is a transient daemon
          // hiccup (restart, socket blip) — keep polling.
          if (status === 403 || status === 404) return;
          step(i + 1);
        });
    }, CLAUDE_RECHECK_BACKOFF_MS[i]);
  };
  step(0);
}

// useClaudeInstall owns the install confirm-card lifecycle + the sign-in spawn.
// onSpawn fires after a helper pane is opened (or pointed at an existing one) so
// the caller can dismiss itself — the checklist closes; the banner hides for the
// session. The install is a deliberate TWO-STEP action: startInstall fetches the
// OS-correct command and stages a confirm card; confirmInstall spawns it only
// after the user has seen the exact command. We never pipe a remote script to a
// shell on a single click without disclosure.
export function useClaudeInstall(opts?: { onSpawn?: (kind: 'install' | 'login') => void }): {
  installing: boolean;
  pendingInstall: ClaudeInstallSpec | null;
  startInstall: () => void;
  confirmInstall: () => void;
  cancelInstall: () => void;
  loginClaude: () => void;
} {
  const onSpawn = opts?.onSpawn;
  const [installing, setInstalling] = useState(false);
  const [pendingInstall, setPendingInstall] = useState<ClaudeInstallSpec | null>(null);

  const startInstall = () => {
    if (installing) return;
    setInstalling(true);
    void api
      .claudeInstallCommand()
      .then((spec) => {
        if (!spec.supported) {
          useStore.getState().toast(
            'Automatic install isn’t available on this platform — install Claude Code manually, then re-check.',
            { kind: 'info', icon: '!', ttlMs: 9000 },
          );
          return;
        }
        setPendingInstall(spec);
        useStore.getState().toast('Review the installer command, then click "Run installer in new pane".', {
          kind: 'info',
          ttlMs: 7000,
        });
      })
      .catch((e: unknown) => {
        const status = (e as { status?: number } | null)?.status;
        useStore.getState().toast(
          status === 403
            ? 'Install must run on the machine itself, not over your phone/cloud link.'
            : 'Could not reach the daemon to start the installer.',
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
    // De-dupe: if an installer pane is already open, point the user at it rather
    // than spawning a second concurrent curl|bash.
    const existing = Object.values(useStore.getState().panes).find((p) => p.title === INSTALL_PANE_TITLE);
    if (existing) {
      useStore.getState().toast('An installer is already running — see the “Install Claude Code” pane.', {
        kind: 'info',
        ttlMs: 7000,
      });
      scheduleClaudeRecheck('install'); // re-arm: the pane may be close to done
      onSpawn?.('install');
      return;
    }
    void useStore
      .getState()
      .createPane({ cwd: '', cmd: spec.cmd, title: INSTALL_PANE_TITLE })
      .then(() => {
        useStore.getState().toast(
          'Installing Claude Code — watch the new pane. This updates automatically when it finishes.',
          { kind: 'info', ttlMs: 9000 },
        );
        scheduleClaudeRecheck('install');
        onSpawn?.('install');
      })
      .catch(() => {
        useStore.getState().toast('Couldn’t open the installer pane — check the daemon and try again.', {
          kind: 'info',
          icon: '!',
          ttlMs: 8000,
        });
      });
  };

  const cancelInstall = () => setPendingInstall(null);

  // Sign in to Claude: open a terminal `claude` pane, which triggers its login
  // flow (browser, or paste-code fallback in WSL/remote). Works over the relay
  // too (the pane is spawned on the host; the phone user pastes the code). After
  // login the probe/health detects authed → surfaces flip to Ready and MCP
  // re-registers (Phase 2 self-heal). macOS uses the Keychain so file-based auth
  // detection doesn't apply there (out of scope; we ship Linux/WSL/Windows).
  const loginClaude = () => {
    markSetupStarted();
    const existing = Object.values(useStore.getState().panes).find((p) => p.title === LOGIN_PANE_TITLE);
    if (existing) {
      useStore.getState().toast('A sign-in pane is already open.', { kind: 'info', ttlMs: 6000 });
      scheduleClaudeRecheck('login'); // re-arm: the user may be mid-login
      onSpawn?.('login');
      return;
    }
    void useStore
      .getState()
      .createPane({ cwd: '', cmd: ['claude'], title: LOGIN_PANE_TITLE })
      .then(() => {
        useStore.getState().toast(
          'Opened Claude — follow the sign-in prompt in the new pane (paste the code if a browser can’t open).',
          { kind: 'info', ttlMs: 10000 },
        );
        scheduleClaudeRecheck('login');
        onSpawn?.('login');
      })
      .catch(() => {
        useStore.getState().toast('Couldn’t open the sign-in pane — check the daemon and try again.', {
          kind: 'info',
          icon: '!',
          ttlMs: 8000,
        });
      });
  };

  return { installing, pendingInstall, startInstall, confirmInstall, cancelInstall, loginClaude };
}

// ClaudeInstallConfirmCard — the disclosure card shown before running the
// installer. Renders the EXACT command (spec.display) so a remote curl|bash is
// never run on a single click without the user seeing it first. Shared by the
// checklist and the banner, and by the Codex install (agentName="Codex").
export function ClaudeInstallConfirmCard({
  spec,
  onConfirm,
  onCancel,
  agentName = 'Claude Code',
}: {
  spec: ClaudeInstallSpec;
  onConfirm: () => void;
  onCancel: () => void;
  agentName?: string;
}) {
  return (
    <div
      style={{
        marginTop: 8,
        marginBottom: 8,
        padding: '10px 12px',
        border: '1px solid var(--border)',
        borderRadius: 6,
        background: 'var(--bg-elev, rgba(255,255,255,0.03))',
      }}
    >
      <div style={{ fontSize: 13, fontWeight: 600 }}>Run {agentName} installer?</div>
      <div style={{ fontSize: 12, color: 'var(--fg-dim)', margin: '6px 0', lineHeight: 1.45 }}>
        {spec.note} The next step opens a new pane and runs:
      </div>
      <code
        style={{
          display: 'block',
          fontSize: 11,
          fontFamily: 'var(--mono, monospace)',
          background: 'var(--bg, rgba(0,0,0,0.25))',
          padding: '6px 8px',
          borderRadius: 4,
          overflowX: 'auto',
          whiteSpace: 'pre',
        }}
      >{spec.display}</code>
      <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
        <button type="button" onClick={onConfirm} style={{ fontSize: 12, padding: '3px 10px' }}>
          Run installer in new pane
        </button>
        <button type="button" onClick={onCancel} style={{ fontSize: 12, padding: '3px 10px' }}>
          Cancel
        </button>
      </div>
    </div>
  );
}
