import type { SoluBackendGlobal } from '../wslSetup';

// Pure row builders for the setup checklist, split out so the wording and
// state rules are unit-testable without rendering the dialog.

export type RowStatus = 'ok' | 'warn' | 'pending' | 'bad' | 'optional';

export interface ChecklistRow {
  key: string;
  label: string;
  detail: string;
  status: RowStatus;
  // A small tag after the label, e.g. "Optional".
  badge?: string;
  // When present, renders an inline button that fixes the row or opens the
  // surface that does.
  fix?: { label: string; onClick: () => void };
}

// Normalised probe state for one agent; null = not fetched yet.
export interface AgentState {
  checked: boolean;
  installed: boolean;
  version: string;
  authed: boolean;
}

export type AgentId = 'claude' | 'codex';

export const AGENT_LABEL: Record<AgentId, string> = { claude: 'Claude Code', codex: 'Codex' };

// What each agent needs to sign in. Kept with the rows so the help pages and
// the app say the same thing.
export const AGENT_PLAN: Record<AgentId, string> = {
  claude: 'Works with a Claude Pro, Max, Team or Enterprise plan, or an Anthropic API key.',
  codex: 'Works with a ChatGPT Plus, Pro, Business, Edu or Enterprise plan, or an OpenAI API key.',
};

// `claude --version` prints "2.1.306 (Claude Code)"; `codex --version` prints
// "codex-cli 0.164.0". Show just the number.
export function cleanAgentVersion(v: string): string {
  return v
    .replace(/\s*\(Claude Code\)\s*$/i, '')
    .replace(/^codex(-cli)?\s+/i, '')
    .trim();
}

export function agentReady(s: AgentState | null | 'unsupported'): boolean {
  return !!s && s !== 'unsupported' && s.checked && s.installed && s.authed;
}

export function agentRow(
  agent: AgentId,
  s: AgentState | null,
  opts: { installing: boolean; preparing: boolean; onInstall: () => void; onLogin: () => void },
): ChecklistRow {
  const label = AGENT_LABEL[agent];
  if (s === null || !s.checked) {
    // Never a premature "Not installed" while the first probe is in flight.
    return { key: agent, label, detail: 'Checking…', status: 'pending' };
  }
  if (!s.installed) {
    if (opts.installing) {
      return {
        key: agent,
        label,
        detail: 'Installing in a new pane. This turns green by itself when it’s done.',
        status: 'pending',
      };
    }
    return {
      key: agent,
      label,
      detail: `Not installed yet. ${AGENT_PLAN[agent]}`,
      status: 'bad',
      fix: { label: opts.preparing ? 'Preparing…' : 'Review install', onClick: opts.onInstall },
    };
  }
  const version = s.version ? ` (version ${cleanAgentVersion(s.version)})` : '';
  if (!s.authed) {
    return {
      key: agent,
      label,
      detail: `Installed${version}. Log in to start using it. ${AGENT_PLAN[agent]}`,
      status: 'warn',
      fix: { label: 'Log in', onClick: opts.onLogin },
    };
  }
  return { key: agent, label, detail: `Ready to use${version}.`, status: 'ok' };
}

// The Account row. Not signing in is a valid choice, so a signed-out user
// sees a neutral "Optional" row, never a green tick.
export function accountRow(
  mode: 'cloud' | 'local' | 'detecting',
  detail: string,
  signedIn: boolean,
  relayed: boolean,
  fix?: ChecklistRow['fix'],
): ChecklistRow {
  if (mode === 'detecting') return { key: 'mode', label: 'Account', detail, status: 'pending' };
  if (signedIn || relayed) return { key: 'mode', label: 'Account', detail, status: 'ok' };
  return { key: 'mode', label: 'Account', detail, status: 'optional', badge: 'Optional', fix };
}

const WSL = 'Linux (WSL)';
const POWERSHELL = 'Windows PowerShell';

function osFromHealth(paneEnvironment: string | undefined, shellPlatform: string | undefined): string | null {
  switch (paneEnvironment) {
    case 'wsl': return WSL;
    case 'windows': return POWERSHELL;
    case 'linux': return 'Linux';
    case 'macos': return 'macOS';
  }
  const s = (shellPlatform ?? '').toLowerCase();
  if (s.startsWith('win')) return POWERSHELL;
  if (s.startsWith('linux')) return 'Linux';
  if (s.startsWith('darwin')) return 'macOS';
  return null;
}

// "Where agents run": the environment new panes start in. The desktop shell's
// backend global is authoritative; health fields are the fallback for a
// browser tab. onSetUpWsl is passed only where the desktop app can run WSL
// setup; elsewhere the row is status only.
export function whereAgentsRunRow(input: {
  backend: SoluBackendGlobal | null;
  healthBackendID?: string;
  paneEnvironment?: string;
  shellPlatform?: string;
  onSetUpWsl?: () => void;
}): ChecklistRow | null {
  const { backend, healthBackendID, paneEnvironment, shellPlatform, onSetUpWsl } = input;
  const base = { key: 'backend', label: 'Where agents run' };
  // With a WSL distro already on this PC there is nothing to set up: the
  // same action just switches to it.
  const hasDistro = backend?.wslDistro === true;
  const setUp = onSetUpWsl
    ? { label: hasDistro ? `Use ${WSL}` : `Set up ${WSL}`, onClick: onSetUpWsl }
    : undefined;
  const actual = backend?.actual;
  if (actual === 'host' && backend?.fallback && backend.requested !== 'host') {
    return {
      ...base,
      detail: `${POWERSHELL}, because ${WSL} didn’t start. Your agents still work here.`,
      status: 'warn',
      fix: setUp,
    };
  }
  if (actual === 'host') {
    return {
      ...base,
      detail: hasDistro
        ? `${POWERSHELL}. ${WSL} is on this PC too.`
        : `${POWERSHELL}. Setting up ${WSL} is optional.`,
      status: 'ok',
      fix: setUp,
    };
  }
  if (actual === 'wsl') return { ...base, detail: WSL, status: 'ok' };
  if (actual === 'dual') return { ...base, detail: `${WSL} and ${POWERSHELL}`, status: 'ok' };
  if (healthBackendID === 'wsl') return { ...base, detail: WSL, status: 'ok' };
  if (healthBackendID === 'win' || healthBackendID === 'host') return { ...base, detail: POWERSHELL, status: 'ok' };
  const os = osFromHealth(paneEnvironment, shellPlatform);
  return os ? { ...base, detail: os, status: 'ok' } : null;
}
