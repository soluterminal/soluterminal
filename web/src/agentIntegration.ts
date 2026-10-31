import { useEffect } from 'react';
import { api } from './api';
import { isRelayedSession, useCloud } from './cloud';
import { useStore } from './store';
import { useSettings } from './settings';
import { confirmDialog } from './lib/dialog';
import { getDaemonHealthState } from './lib/useDaemonHealth';
import { setupStartedWithin } from './lib/setupActivity';
import { INSTALL_PANE_TITLE, LOGIN_PANE_TITLE } from './claudeBootstrap';
import { CODEX_INSTALL_PANE_TITLE, CODEX_LOGIN_PANE_TITLE } from './codexBootstrap';
import { useAgentInstallPrompt } from './agentStart';
import { connectPromptPending } from './accountConnect';
import type { Health } from './types';

// Opt-in for adding SoluTerminal's tools to Claude Code and Codex on this
// computer (daemon/agent_integration.go). Asked once on the desktop after the
// welcome tour; "Not now" is a real "off" that the daemon keeps, so it is never
// asked again automatically. An install that already had the registration
// keeps it and gets a one-time notice instead. Settings → Devices holds the
// switch.

// Panes SoluTerminal starts always get its tools per process
// (daemon/agent_integration_inject.go), so agents in SoluTerminal already work
// together without this. The copy says what the switch really adds: the same
// tools for Claude Code and Codex started outside SoluTerminal, through four
// entries in their settings files.
export const AGENT_INTEGRATION_TITLE = 'Let Claude Code and Codex use SoluTerminal everywhere on this PC?';
export const AGENT_INTEGRATION_BODY = [
  "Agents you start in SoluTerminal already work together. You don't need this for that.",
  'This also lets Claude Code and Codex started outside SoluTerminal (in another terminal, or typed in a ' +
    'Windows PowerShell pane) see and message your panes. It only talks to this PC.',
  'It adds four entries to their settings files, including Claude Code hooks. Turning it off in Settings, ' +
    'or uninstalling, removes them and keeps a backup of each file.',
].join('\n\n');
export const AGENT_INTEGRATION_NOTICE =
  'SoluTerminal is set up in Claude Code and Codex everywhere on this PC, including ones started outside ' +
  'SoluTerminal (four entries in their settings files). Turning this off in Settings → Devices removes them.';
export const AGENT_INTEGRATION_ON_TOAST =
  'Done. Claude Code and Codex started outside SoluTerminal can now use it. You can turn this off in Settings → Devices.';
export const AGENT_INTEGRATION_OFF_TOAST =
  "Nothing was added. Agents you start in SoluTerminal still work together. You can turn this on in Settings → Devices.";

function openAgentIntegrationSettings(): void {
  useSettings.getState().openSettings('devices');
}

let asked = false;

/** Asks the first-run question, or shows the existing-install notice, once. */
export async function askAgentIntegrationOnce(): Promise<void> {
  if (asked) return;
  asked = true;
  let current;
  try {
    current = await api.getAgentIntegration();
  } catch {
    return;
  }
  if (!current) return;
  const toast = useStore.getState().toast;
  if (current.noticePending) {
    toast(AGENT_INTEGRATION_NOTICE, {
      kind: 'info',
      ttlMs: 12000,
      action: { label: 'Settings', run: openAgentIntegrationSettings },
    });
    void api.ackAgentIntegrationNotice().catch(() => undefined);
    return;
  }
  if (!current.needsAnswer) return;
  const yes = await confirmDialog({
    title: AGENT_INTEGRATION_TITLE,
    message: AGENT_INTEGRATION_BODY,
    confirmLabel: 'Turn on',
    cancelLabel: 'Not now',
  });
  try {
    // Escape and a click outside count as "Not now" too.
    await api.setAgentIntegration(yes ? 'on' : 'off');
    toast(
      yes ? AGENT_INTEGRATION_ON_TOAST : AGENT_INTEGRATION_OFF_TOAST,
      { kind: 'info', ttlMs: 7000 },
    );
  } catch (e) {
    toast(`Couldn't save that choice: ${String(e)}`, { kind: 'warn', ttlMs: 8000 });
  }
}

// The question waits while the user is in the middle of setting up an agent:
// it used to pop up over the sign-in pane the checklist's "Log in" had just
// opened. Busy means an install or sign-in step started in the last
// SETUP_QUIET_MS, an install card from a Start button is open, or an install
// or sign-in pane is still open for an agent that isn't ready yet.
export const SETUP_QUIET_MS = 2 * 60_000;
export const SETUP_RECHECK_MS = 15_000;

const SETUP_PANES: Record<string, 'claude' | 'codex'> = {
  [INSTALL_PANE_TITLE]: 'claude',
  [LOGIN_PANE_TITLE]: 'claude',
  [CODEX_INSTALL_PANE_TITLE]: 'codex',
  [CODEX_LOGIN_PANE_TITLE]: 'codex',
};

function agentSetUp(agent: 'claude' | 'codex', health: Health | null): boolean {
  if (!health) return false;
  return agent === 'claude'
    ? health.claudeInstalled === true && health.claudeAuthed === true
    : health.codexInstalled === true && health.codexAuthed === true;
}

export function setupInProgress(now: number = Date.now()): boolean {
  if (setupStartedWithin(SETUP_QUIET_MS, now)) return true;
  // "Connect this PC to your account?" is being asked: don't replace it.
  if (connectPromptPending()) return true;
  if (useAgentInstallPrompt.getState().agent !== null) return true;
  const health = getDaemonHealthState().health;
  return Object.values(useStore.getState().panes).some((p) => {
    const agent = SETUP_PANES[p.title ?? ''];
    return agent !== undefined && !agentSetUp(agent, health);
  });
}

/**
 * Runs the question once the first-run screens are out of the way and no
 * agent setup is under way, re-checking every SETUP_RECHECK_MS until then.
 * Only on this computer's own window: a phone or the cloud relay can't
 * change it. The desktop also boots in cloud mode (its daemon serves the
 * Supabase config), but with isLocal set, so "relayed" is the test, not mode.
 */
export function useAgentIntegrationPrompt(ready: boolean): void {
  const remote = useCloud(isRelayedSession);
  useEffect(() => {
    if (!ready || remote) return;
    if (!setupInProgress()) {
      void askAgentIntegrationOnce();
      return;
    }
    const timer = window.setInterval(() => {
      if (setupInProgress()) return;
      window.clearInterval(timer);
      void askAgentIntegrationOnce();
    }, SETUP_RECHECK_MS);
    return () => window.clearInterval(timer);
  }, [ready, remote]);
}

export function resetAgentIntegrationPromptForTests(): void {
  asked = false;
}
