import type { PaneInfo } from '../types';
import { effectivePaneAgent } from './agentKind';

// The id that resumes this pane's conversation: a Codex thread, or a Claude or
// Muse session. Null until the pane has one that can be resumed.
export function paneConversation(pane: PaneInfo): { id: string; noun: 'thread' | 'session' } | null {
  if (!pane.resumeReady) return null;
  const agent = effectivePaneAgent(pane) ?? (pane.codexThreadId ? 'codex' : pane.claudeSessionId ? 'claude' : null);
  const id = agent === 'codex' ? pane.codexThreadId : agent === 'muse' ? pane.museSessionId : pane.claudeSessionId;
  return id ? { id, noun: agent === 'codex' ? 'thread' : 'session' } : null;
}
