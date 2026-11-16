import { describe, expect, it } from 'vitest';
import type { Layout, PaneInfo } from '../types';
import { collectPaneDefs, optimisticPaneFromDef } from './workspace';

describe('workspace agent identity round-trip', () => {
  it('preserves Codex thread and model metadata, and never saves a Focus kind', () => {
    const pane: PaneInfo = {
      id: 'codex-1', cwd: '/work', cmd: ['codex'], title: 'Codex', status: 'running',
      createdAt: new Date(0).toISOString(), cols: 100, rows: 30, pid: 1,
      lastActivityMs: 0, state: 'idle', hasClaude: false, kind: 'native',
      agent: 'codex', codexThreadId: 'thread-1', model: 'gpt-5.3-codex',
    };
    const layout: Layout = { type: 'pane', paneId: pane.id };
    const [def] = collectPaneDefs(layout, { [pane.id]: pane });
    expect(def).toMatchObject({
      agent: 'codex', codexThreadId: 'thread-1', model: 'gpt-5.3-codex',
    });
    expect(def).not.toHaveProperty('kind');
    expect(optimisticPaneFromDef({ ...def, kind: 'native' })).toMatchObject({
      agent: 'codex', codexThreadId: 'thread-1', model: 'gpt-5.3-codex',
    });
    expect(optimisticPaneFromDef({ ...def, kind: 'native' }).kind).toBeUndefined();
  });

  it('preserves the orchestrator role through def and optimistic seed', () => {
    const pane: PaneInfo = {
      id: 'orch-1', cwd: '/work', cmd: ['claude'], title: 'Orchestrator', status: 'running',
      createdAt: new Date(0).toISOString(), cols: 80, rows: 24, pid: 3,
      lastActivityMs: 0, state: 'idle', hasClaude: true, role: 'orchestrator',
    };
    const plain: PaneInfo = { ...pane, id: 'plain-1', title: 'claude', role: undefined };
    const layout: Layout = {
      type: 'split', direction: 'horizontal', ratio: 0.5,
      a: { type: 'pane', paneId: pane.id },
      b: { type: 'pane', paneId: plain.id },
    };
    const [def, plainDef] = collectPaneDefs(layout, { [pane.id]: pane, [plain.id]: plain });
    expect(def.role).toBe('orchestrator');
    expect(optimisticPaneFromDef(def).role).toBe('orchestrator');
    expect(plainDef.role).toBeUndefined();
  });

  it('preserves Claude resume identity independently', () => {
    const pane: PaneInfo = {
      id: 'claude-1', cwd: '/work', cmd: ['claude'], title: 'Claude', status: 'running',
      createdAt: new Date(0).toISOString(), cols: 80, rows: 24, pid: 2,
      lastActivityMs: 0, state: 'idle', hasClaude: true, kind: 'native',
      claudeSessionId: 'session-1', model: 'claude-opus-4-8',
    };
    const layout: Layout = { type: 'pane', paneId: pane.id };
    const [def] = collectPaneDefs(layout, { [pane.id]: pane });
    expect(def.claudeSessionId).toBe('session-1');
    expect(def.codexThreadId).toBeUndefined();
  });
});
