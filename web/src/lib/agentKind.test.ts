import { describe, expect, it } from 'vitest';
import { agentFromCommand, agentLabel, effectivePaneAgent, waitingAgentKind } from './agentKind';

describe('agentKind helpers', () => {
  it('recognizes direct commands, paths, and Windows executables', () => {
    expect(agentFromCommand('codex --resume')).toBe('codex');
    expect(agentFromCommand(['/usr/local/bin/claude', '--resume'])).toBe('claude');
    expect(agentFromCommand(['muse'])).toBe('muse');
    expect(agentFromCommand(['C:\\tools\\codex.exe'])).toBe('codex');
    expect(agentFromCommand(['bash', '-lc', 'codex'])).toBeNull();
    expect(agentFromCommand(['unknown-agent'])).toBeNull();
  });

  it('prefers explicit identity and preserves legacy native Claude panes', () => {
    expect(effectivePaneAgent({ agent: 'codex', cmd: ['claude'], kind: 'native' })).toBe('codex');
    expect(effectivePaneAgent({ agent: 'claude', cmd: ['bash'], codexThreadId: 'stale-thread' })).toBe('claude');
    expect(effectivePaneAgent({ agent: 'muse', cmd: ['bash'] })).toBe('muse');
    expect(effectivePaneAgent({ cmd: [], kind: 'native' })).toBe('claude');
    expect(effectivePaneAgent({ cmd: ['bash'] })).toBeNull();
    expect(effectivePaneAgent({ cmd: ['unknown-agent'] })).toBeNull();
  });

  it('identifies a shell-launched Codex pane once its thread id is discovered', () => {
    expect(effectivePaneAgent({ cmd: ['bash'], codexThreadId: 'thread-123' })).toBe('codex');
  });

  it('lets an unambiguous Claude command beat a stale Codex thread id', () => {
    expect(effectivePaneAgent({ cmd: ['claude'], codexThreadId: 'stale' })).toBe('claude');
    expect(effectivePaneAgent({
      cmd: ['/bin/bash', '-lc', "init; 'claude' '--resume' 'session'; exec /bin/bash -i"],
      codexThreadId: 'stale',
    })).toBe('claude');
  });

  it('recognizes daemon login-shell wrappers without classifying shell prose', () => {
    expect(agentFromCommand(['/bin/bash', '-lc', "init; 'codex' 'resume' 'thread'; exec /bin/bash -i"])).toBe('codex');
    expect(agentFromCommand(['/bin/bash', '-lc', "printf 'codex docs'"])).toBeNull();
  });

  it('provides user-facing labels for known kinds and a neutral fallback', () => {
    expect(agentLabel('claude')).toBe('Claude');
    expect(agentLabel('codex')).toBe('Codex');
    expect(agentLabel('muse')).toBe('Muse');
    expect(agentLabel(null)).toBe('Agent');
  });

  it('names the waiting agent without assuming Claude for an unrecognised CLI', () => {
    const shell = { cmd: ['unknown-agent'], hasClaude: false };
    expect(waitingAgentKind(shell)).toBeNull();
    expect(agentLabel(waitingAgentKind(shell))).toBe('Agent');
    expect(waitingAgentKind({ ...shell, hasCodex: true })).toBe('codex');
    expect(waitingAgentKind({ ...shell, hasMuse: true })).toBe('muse');
    expect(waitingAgentKind({ ...shell, hasClaude: true })).toBe('claude');
    // Both seen: Claude, as the alert always chose.
    expect(waitingAgentKind({ ...shell, hasClaude: true, hasCodex: true })).toBe('claude');
    // The pane's own identity wins over the process flags.
    expect(waitingAgentKind({ cmd: ['codex'], hasClaude: true })).toBe('codex');
  });
});
