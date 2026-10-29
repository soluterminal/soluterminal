import type { PaneInfo } from '../types';

export type AgentKind = 'claude' | 'codex' | 'muse';

// Infer an agent from a direct command or the daemon's own durable login-shell
// wrapper. Descendant-process flags are deliberately excluded: an arbitrary
// shell running Codex does not necessarily carry enough continuity metadata.
export function agentFromCommand(command: string | readonly string[]): AgentKind | null {
  const first = typeof command === 'string'
    ? command.trim().split(/\s+/, 1)[0] ?? ''
    : command[0] ?? '';
  const executable = first.replace(/\\/g, '/').split('/').pop()?.toLowerCase().replace(/\.exe$/, '') ?? '';
  if (executable === 'claude') return 'claude';
  if (executable === 'codex') return 'codex';
  if (executable === 'muse') return 'muse';
  // Pane argv may be the daemon's durable bash -lc wrapper rather than the
  // original bare provider command. Match only its shell-quoted executable
  // tokens so arbitrary shell prose mentioning a provider is not classified.
  if (Array.isArray(command)) {
    const wrapped = command.join(' ');
    if (/(?:^|\s)'claude'(?:\s|;|$)/.test(wrapped)) return 'claude';
    if (/(?:^|\s)'codex'(?:\s|;|$)/.test(wrapped)) return 'codex';
    if (/(?:^|\s)'muse'(?:\s|;|$)/.test(wrapped)) return 'muse';
  }
  return null;
}

// Pane.agent is authoritative when present. An unambiguous command wins over a
// stale thread id, mirroring the daemon's load-time identity heal. A durable
// Codex thread id remains the fallback for shell-launched Codex; omitted agent
// on a native pane remains the legacy Claude encoding.
export function effectivePaneAgent(
  pane: Pick<PaneInfo, 'agent' | 'cmd' | 'kind' | 'codexThreadId'>,
): AgentKind | null {
  if (pane.agent === 'claude' || pane.agent === 'codex' || pane.agent === 'muse') return pane.agent;
  const commandAgent = agentFromCommand(pane.cmd);
  if (commandAgent) return commandAgent;
  if (pane.codexThreadId) return 'codex';
  return pane.kind === 'native' ? 'claude' : null;
}

// The agent a pane's "is waiting" alert names. Past the pane's own identity it
// falls back to the live process flags (Claude wins when both Claude and Codex
// are seen, as before), and to null for a CLI the app doesn't recognise, so
// the alert says "Agent" instead of assuming Claude.
export function waitingAgentKind(
  pane: Pick<PaneInfo, 'agent' | 'cmd' | 'kind' | 'codexThreadId' | 'hasClaude' | 'hasCodex' | 'hasMuse'>,
): AgentKind | null {
  const known = effectivePaneAgent(pane);
  if (known) return known;
  if (pane.hasClaude) return 'claude';
  if (pane.hasCodex) return 'codex';
  if (pane.hasMuse) return 'muse';
  return null;
}

export function agentLabel(agent: AgentKind | null): string {
  if (agent === 'muse') return 'Muse';
  return agent === 'codex' ? 'Codex' : agent === 'claude' ? 'Claude' : 'Agent';
}
