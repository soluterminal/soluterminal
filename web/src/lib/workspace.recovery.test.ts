import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { useStore } from '../store';
import type { PaneDef, PaneInfo, Workspace } from '../types';
import { collectPaneIds } from './layout';
import { collectPaneDefs, dropUnconfirmedSeeds, failedPaneFromDef, isPresentPane, paneRestoreRequest, paneRetryRequest } from './workspace';

const def: PaneDef = { id: 'saved', cwd: '/tmp/deleted', cmd: ['bash'], title: 'Original conversation', agent: 'codex', codexThreadId: 'thread-original' };
const failed = () => failedPaneFromDef(def, new Error('working directory does not exist'));

afterEach(() => { vi.restoreAllMocks(); useStore.setState({ activeWorkspace: undefined, layout: null, panes: {}, workspaces: [] }); });

describe('workspace reboot recovery', () => {
  it('preserves the human automatic-answer lock through saved workspace restore', () => {
    const p = { ...failed(), neverAutoAnswer: true, autoMode: '' as const };
    const saved = collectPaneDefs({ type: 'pane', paneId: p.id }, { [p.id]: p })[0];
    expect(saved.neverAutoAnswer).toBe(true);
    expect(paneRestoreRequest(saved)).toMatchObject({ restore: true, neverAutoAnswer: true });
    expect(failedPaneFromDef(saved, new Error('offline')).neverAutoAnswer).toBe(true);
  });

  it('saves a resume recipe for Codex started inside Bash', () => {
    const p = { ...failed(), status: 'running', recoveryError: undefined } as PaneInfo;
    const saved = collectPaneDefs({ type: 'pane', paneId: p.id }, { [p.id]: p })[0];
    expect(saved.cmd).toEqual(['codex', 'resume', 'thread-original']);
    expect(paneRestoreRequest(saved)).toMatchObject({ restore: true, codexThreadId: 'thread-original' });
  });

  it('round-trips Muse identity and the ambiguous identity latch', () => {
    const p = { ...failed(), agent: 'muse', codexThreadId: undefined, museSessionId: 'muse-original', identityBlocked: true } as PaneInfo;
    const saved = collectPaneDefs({ type: 'pane', paneId: p.id }, { [p.id]: p })[0];
    expect(saved).toMatchObject({ museSessionId: 'muse-original', identityBlocked: true, cmd: ['bash'] });
  });

  it('keeps failed restores present and preserves the saved recipe through autosave', () => {
    const p = failed();
    expect(isPresentPane(p)).toBe(true);
    expect(collectPaneDefs({ type: 'pane', paneId: p.id }, { [p.id]: p })[0]).toMatchObject({ id: def.id, cwd: def.cwd, codexThreadId: def.codexThreadId });
  });

  it('keeps the recovery tile when the daemon next reports an empty pane list', async () => {
    const p = failed(); useStore.setState({ panes: { [p.id]: p }, layout: { type: 'pane', paneId: p.id } });
    vi.spyOn(api, 'listPanes').mockResolvedValue([]);
    await useStore.getState().refreshPanes();
    expect(useStore.getState().panes[p.id]?.recoveryError).toBe(p.recoveryError);
    expect(useStore.getState().layout).toEqual({ type: 'pane', paneId: p.id });
  });

  it('reconciles optimistic seeds against an authoritative empty list', () => {
    const p = failed(); const panes = new Map([[p.id, p]]);
    dropUnconfirmedSeeds(panes, new Set([p.id]), [], true);
    expect(panes.size).toBe(0);
  });

  it('keeps both layout slots when one restore succeeds and another fails', async () => {
    const ws: Workspace = {
      name: 'Recovery test', activeAt: new Date(0).toISOString(), paneIds: ['good', 'saved'],
      panes: [{ ...def, id: 'good', codexThreadId: 'thread-good' }, def],
      layout: { type: 'split', direction: 'horizontal', ratio: 0.5, a: { type: 'pane', paneId: 'good' }, b: { type: 'pane', paneId: 'saved' } },
    };
    useStore.setState({ panes: {}, workspaces: [ws], layout: null, activeWorkspace: undefined });
    vi.spyOn(api, 'listPanes').mockResolvedValue([]);
    vi.spyOn(api, 'saveWorkspace').mockResolvedValue(ws);
    vi.spyOn(api, 'createPane').mockImplementation(async (req) => {
      if (req.codexThreadId === 'thread-original') throw new Error('working directory does not exist');
      return { ...failed(), id: 'restored-good', cwd: '/work', status: 'running', recoveryError: undefined, restorePending: undefined };
    });
    await useStore.getState().loadWorkspace(ws.name);
    await vi.waitFor(() => expect(collectPaneIds(useStore.getState().layout)).toEqual(['restored-good', 'saved']));
    const state = useStore.getState();
    expect(state.panes.saved.recoveryError).toContain('directory');
    expect(collectPaneDefs(state.layout, state.panes)).toHaveLength(2);
  });

  it('opens the picker only on an explicit retry of an unidentified terminal', () => {
    const p = { ...failed(), codexThreadId: undefined, claudeSessionId: 'stale-other-provider' };
    expect(paneRestoreRequest({ ...def, codexThreadId: undefined }).restore).toBe(true);
    expect(paneRetryRequest(p)).toMatchObject({ restore: false, cmd: ['codex', '--no-alt-screen', 'resume', '--all'] });
    expect(paneRetryRequest({ ...p, kind: 'native' }).restore).toBe(true);
  });
});
