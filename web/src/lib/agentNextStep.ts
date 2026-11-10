import type { BackendId } from '../dualBackend';

// Durable "an agent was just installed, start it" step behind the start card
// in AgentSetupBanner. Armed when an install of Claude Code or Codex is seen
// finishing; retired for good once the first agent pane has run on this
// computer. localStorage so it outlives the install toasts and an app restart.

export type AgentNextStepSide = BackendId | 'local';

export const AGENT_NEXT_STEP_EVENT = 'solu:agent-next-step';

const PENDING_KEY = 'solu.agentNextStep';
// Set by builds that offered the step for Codex only; still honoured.
const LEGACY_CODEX_KEY = 'solu.codexNextStep';
const STARTED_KEY = 'solu.firstAgentStarted';

// One flag per side (Dual mode keeps separate agents on WSL and PowerShell),
// so an install on one side shows the step on that side only.
const pendingKey = (side: AgentNextStepSide) => `${PENDING_KEY}.${side}`;
const legacyKey = (side: AgentNextStepSide) => `${LEGACY_CODEX_KEY}.${side}`;

function notify(): void {
  window.dispatchEvent(new Event(AGENT_NEXT_STEP_EVENT));
}

export function firstAgentStarted(): boolean {
  try {
    return window.localStorage.getItem(STARTED_KEY) === '1';
  } catch {
    return false;
  }
}

export function markFirstAgentStarted(): void {
  try {
    window.localStorage.setItem(STARTED_KEY, '1');
  } catch {
    // Private mode etc: the running pane still hides the card this session.
  }
  notify();
}

export function agentNextStepPending(side: AgentNextStepSide): boolean {
  if (firstAgentStarted()) return false;
  try {
    const ls = window.localStorage;
    return ls.getItem(pendingKey(side)) === '1' || ls.getItem(legacyKey(side)) === '1';
  } catch {
    return false;
  }
}

export function setAgentNextStep(side: AgentNextStepSide, on: boolean): void {
  try {
    const ls = window.localStorage;
    if (on) {
      ls.setItem(pendingKey(side), '1');
    } else {
      ls.removeItem(pendingKey(side));
      ls.removeItem(legacyKey(side));
    }
  } catch {
    // Private mode etc: the install toast still showed once.
  }
  notify();
}
