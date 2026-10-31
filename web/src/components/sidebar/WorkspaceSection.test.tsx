import * as paneStatus from '../../lib/paneStatus';
import { act, createEvent, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../../store';
import type { PaneInfo, Workspace } from '../../types';
import { WorkspaceSection } from './WorkspaceSection';

const dialogMocks = vi.hoisted(() => ({
  confirmDialog: vi.fn<(opts: { message: string; confirmLabel?: string; danger?: boolean }) => Promise<boolean>>(),
}));
vi.mock('../../lib/dialog', () => ({ confirmDialog: dialogMocks.confirmDialog }));

const now = new Date('2026-05-18T00:00:00.000Z');

function pane(overrides: Partial<PaneInfo> = {}): PaneInfo {
  return {
    id: 'pane-1',
    cwd: '/home/user/project',
    cmd: ['bash'],
    title: 'Deploy checks',
    status: 'running',
    createdAt: '2026-05-18T00:00:00.000Z',
    cols: 80,
    rows: 24,
    pid: 123,
    lastActivityMs: now.getTime() - 120_000,
    state: 'awaiting-input',
    hasClaude: true,
    ...overrides,
  };
}

function workspace(overrides: Partial<Workspace> = {}): Workspace {
  return {
    name: 'Launch Ops With A Very Long Workspace Name',
    layout: null,
    paneIds: ['pane-1'],
    activeAt: '2026-05-18T00:00:00.000Z',
    ...overrides,
  };
}

function renderWorkspaceSection(overrides: Partial<Parameters<typeof WorkspaceSection>[0]> = {}) {
  const panes = { 'pane-1': pane() };
  const named = [workspace()];
  const props: Parameters<typeof WorkspaceSection>[0] = {
    collapsed: false,
    named,
    panes,
    activeWorkspace: named[0].name,
    activeBackend: null,
    layout: null,
    focusedPaneId: null,
    wsLastViewedMs: { [named[0].name]: now.getTime() - 300_000 },
    creating: false,
    createDraft: '',
    createCancelRef: { current: false },
    dropTarget: null,
    renamingName: null,
    renameDraft: '',
    ctxMenu: null,
    ctxMenuRef: { current: null },
    onToggle: vi.fn(),
    onStartCreate: vi.fn(),
    onCommitCreate: vi.fn(),
    setCreateDraft: vi.fn(),
    setCreating: vi.fn(),
    setDropTarget: vi.fn(),
    setRenamingName: vi.fn(),
    setRenameDraft: vi.fn(),
    setCtxMenu: vi.fn(),
    onLoadWorkspace: vi.fn(),
    onRenameWorkspace: vi.fn().mockResolvedValue(undefined),
    onSetWorkspaceContextPack: vi.fn().mockResolvedValue(undefined),
    onDeleteWorkspace: vi.fn(),
    onClearLayout: vi.fn(),
    onReorderWorkspaces: vi.fn(),
    ...overrides,
  };

  return { ...render(<WorkspaceSection {...props} />), props };
}

describe('WorkspaceSection', () => {
  beforeEach(() => {
    localStorage.clear();
    dialogMocks.confirmDialog.mockReset();
    vi.useFakeTimers();
    vi.setSystemTime(now);
    useStore.setState({
      panes: {},
      paneTails: {},
      ariaDismissedFingerprints: {},
    });
  });

  afterEach(() => {
    useStore.setState({
      panes: {},
      paneTails: {},
      ariaDismissedFingerprints: {},
    });
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('counts the live panes of the active workspace before its autosave lands', () => {
    const panes = {
      a: pane({ id: 'a', state: 'idle' }),
      b: pane({ id: 'b', state: 'idle' }),
      c: pane({ id: 'c', state: 'idle' }),
    };
    const named = [workspace({ name: 'Default', paneIds: [] })];
    renderWorkspaceSection({
      named,
      panes,
      activeWorkspace: 'Default',
      wsLastViewedMs: {},
      layout: {
        type: 'split', direction: 'horizontal', ratio: 0.5,
        a: { type: 'pane', paneId: 'a' },
        b: { type: 'split', direction: 'vertical', ratio: 0.5, a: { type: 'pane', paneId: 'b' }, b: { type: 'pane', paneId: 'c' } },
      },
    });
    const row = screen.getByText('Default').closest('.ws-row');
    expect(row).toHaveAttribute('title', expect.stringContaining('3 panes'));
  });

  it('separates primary workspace name from secondary row metadata', () => {
    renderWorkspaceSection();

    const name = screen.getByText('Launch Ops With A Very Long Workspace Name');
    const row = name.closest('.ws-row');
    expect(row).not.toBeNull();
    expect(row).toHaveAttribute('title', expect.stringContaining('1 pane'));
    expect(row).toHaveAttribute('title', expect.stringContaining('last active 2m ago'));
    expect(row).toHaveAttribute('title', expect.stringContaining('waiting for input: Deploy checks'));

    expect(row?.querySelector('.ws-status')).not.toBeNull();
    expect(row?.querySelector('.ws-title-line .ws-name')).toHaveTextContent('Launch Ops With A Very Long Workspace Name');
    expect(row?.querySelector('.ws-meta')).toHaveTextContent('1 pane · last active 2m ago · waiting');
  });

  // The drop indicator and the drop itself must be driven by the SAME
  // halfway rule: hovering the bottom half of a row previews (and drops)
  // "after" it, the top half "before" it. Regression for the misleading
  // static top-edge line that showed "before" while the drop landed "after".
  it('drag indicator position matches where the drop actually lands', () => {
    const onReorderWorkspaces = vi.fn();
    const setDropTarget = vi.fn();
    const named = [
      workspace({ name: 'alpha', paneIds: [] }),
      workspace({ name: 'beta', paneIds: [] }),
    ];
    renderWorkspaceSection({ named, onReorderWorkspaces, setDropTarget, activeWorkspace: 'alpha' });

    const rect = { top: 0, bottom: 30, height: 30, left: 0, right: 200, width: 200, x: 0, y: 0, toJSON: () => ({}) };
    const row = screen.getByText('beta').closest('.ws-row') as HTMLElement;
    row.getBoundingClientRect = () => rect as DOMRect;
    const dataTransfer = {
      types: ['application/x-solu-workspace'],
      getData: () => 'alpha',
      setData: vi.fn(),
      dropEffect: 'move',
      effectAllowed: 'move',
    };

    // jsdom drag events don't honor a clientY init — pin it per-event.
    const fireAt = (type: 'dragOver' | 'drop', clientY: number) => {
      const ev = createEvent[type](row, { dataTransfer });
      Object.defineProperty(ev, 'clientY', { value: clientY });
      Object.defineProperty(ev, 'dataTransfer', { value: dataTransfer });
      fireEvent(row, ev);
    };

    // Bottom half → indicator previews "after".
    fireAt('dragOver', 25);
    expect(setDropTarget).toHaveBeenCalled();
    const updater = setDropTarget.mock.calls.at(-1)![0];
    expect(typeof updater === 'function' ? updater(null) : updater).toEqual({ key: 'beta', pos: 'after' });

    // Top half → indicator previews "before".
    fireAt('dragOver', 5);
    const updater2 = setDropTarget.mock.calls.at(-1)![0];
    expect(typeof updater2 === 'function' ? updater2(null) : updater2).toEqual({ key: 'beta', pos: 'before' });

    // Drop in the bottom half lands "after" — same rule as the indicator.
    fireAt('drop', 25);
    expect(onReorderWorkspaces).toHaveBeenCalledWith('alpha', 'beta', 'after');

    fireAt('drop', 5);
    expect(onReorderWorkspaces).toHaveBeenLastCalledWith('alpha', 'beta', 'before');
  });

  it('keeps delete clicks from loading the workspace', async () => {
    dialogMocks.confirmDialog.mockResolvedValue(true);
    const onLoadWorkspace = vi.fn();
    const onDeleteWorkspace = vi.fn();
    renderWorkspaceSection({ onLoadWorkspace, onDeleteWorkspace });

    await act(async () => { fireEvent.click(screen.getByTitle('Delete workspace')); });

    expect(onDeleteWorkspace).toHaveBeenCalledWith('Launch Ops With A Very Long Workspace Name');
    expect(onLoadWorkspace).not.toHaveBeenCalled();
  });

  it('confirms before the row × deletes, the same as right-click › Delete', async () => {
    const onDeleteWorkspace = vi.fn();
    const name = 'Launch Ops With A Very Long Workspace Name';
    const { rerender, props } = renderWorkspaceSection({ onDeleteWorkspace });

    dialogMocks.confirmDialog.mockResolvedValue(false);
    await act(async () => { fireEvent.click(screen.getByTitle('Delete workspace')); });
    expect(dialogMocks.confirmDialog).toHaveBeenLastCalledWith({ message: `Delete workspace "${name}"?`, confirmLabel: 'Delete', danger: true });
    expect(onDeleteWorkspace).not.toHaveBeenCalled();

    // The context-menu path shows the identical confirm.
    rerender(<WorkspaceSection {...props} ctxMenu={{ x: 0, y: 0, wsName: name }} />);
    await act(async () => { fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' })); });
    expect(dialogMocks.confirmDialog).toHaveBeenCalledTimes(2);
    expect(dialogMocks.confirmDialog).toHaveBeenLastCalledWith({ message: `Delete workspace "${name}"?`, confirmLabel: 'Delete', danger: true });
    expect(onDeleteWorkspace).not.toHaveBeenCalled();
  });
  it('persists collapsed branches, bubbles attention, and navigates visible rows', () => {
    const lead = pane({ id: 'lead', title: 'Lead', state: 'idle' });
    const worker = pane({ id: 'worker', title: 'Worker', state: 'choice-prompt', attention: 'needs-user', origin: { source: 'agent', byPaneId: 'lead', at: 1 } });
    const props = { named: [workspace({ name: 'Project', paneIds: ['lead'] })], panes: { lead, worker }, activeWorkspace: 'Project' };
    const first = renderWorkspaceSection(props);
    fireEvent.click(screen.getByRole('button', { name: 'Collapse Lead' }));
    expect(screen.getByRole('treeitem', { name: 'Lead' })).toHaveTextContent('1 worker');
    expect(screen.queryByRole('treeitem', { name: 'Worker' })).not.toBeInTheDocument();
    expect(first.container.querySelector('.workspace-pane-row .pane-tree-dot')).toHaveClass('is-needs-you');
    expect(first.container.querySelector('.ws-row .pane-tree-dot')).toHaveClass('is-needs-you');
    first.unmount();
    renderWorkspaceSection(props);
    const row = screen.getByRole('treeitem', { name: 'Lead' });
    expect(row).toHaveAttribute('aria-expanded', 'false');
    act(() => row.focus());
    fireEvent.keyDown(row, { key: 'ArrowRight' });
    fireEvent.keyDown(row, { key: 'ArrowDown' });
    expect(screen.getByRole('treeitem', { name: 'Worker' })).toHaveFocus();
    fireEvent.keyDown(document.activeElement!, { key: 'ArrowLeft' });
    expect(row).toHaveFocus();
  });

  it('confirms the finished-worker close and respects cancellation', async () => {
    const worker = pane({ id: 'worker', title: 'Finished worker', state: 'idle', origin: { source: 'agent', byPaneId: 'lead', at: 1 }, turn: { active: false, count: 1, last: { n: 1, startedMs: 1, endedMs: 2, files: 0, added: 0, removed: 0 } } });
    const killPane = vi.fn();
    act(() => useStore.setState({ killPane }));
    renderWorkspaceSection({ panes: { worker }, named: [workspace({ paneIds: ['worker'] })] });
    expect(screen.getByText('finished')).toBeVisible();
    dialogMocks.confirmDialog.mockResolvedValue(false);
    await act(async () => fireEvent.click(screen.getByRole('button', { name: 'Close Finished worker' })));
    expect(killPane).not.toHaveBeenCalled();
    dialogMocks.confirmDialog.mockResolvedValue(true);
    await act(async () => fireEvent.click(screen.getByRole('button', { name: 'Close Finished worker' })));
    expect(dialogMocks.confirmDialog).toHaveBeenLastCalledWith(expect.objectContaining({ message: 'Close "Finished worker"?', danger: true }));
    expect(killPane).toHaveBeenCalledWith('worker');
  });

  it('keeps the current-workspace row out of saved-workspace management', () => {
    const { container, rerender, props } = renderWorkspaceSection({ named: [workspace({ name: '_current', protected: true })], activeWorkspace: '_current' });
    const name = screen.getByText('Current workspace');
    fireEvent.doubleClick(name);
    expect(container.querySelector('.ws-rename-input')).toBeNull();
    expect(name.closest('.ws-row')).toHaveAttribute('draggable', 'false');
    rerender(<WorkspaceSection {...props} ctxMenu={{ x: 0, y: 0, wsName: '_current' }} />);
    expect(screen.queryByRole('menuitem', { name: 'Rename' })).not.toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: 'Context pack…' })).not.toBeInTheDocument();
  });

  it('keeps tree and status summaries cached while split ratios change', () => {
    const lead = pane({ id: 'lead', title: 'Lead', state: 'idle' });
    const worker = pane({ id: 'worker', title: 'Worker', state: 'idle', origin: { source: 'agent', byPaneId: 'lead', at: 1 } });
    const layout = { type: 'split' as const, direction: 'horizontal' as const, ratio: 0.3,
      a: { type: 'pane' as const, paneId: 'lead' }, b: { type: 'pane' as const, paneId: 'worker' } };
    const named = [workspace({ name: 'Project', layout, paneIds: ['lead', 'worker'] })];
    const status = vi.spyOn(paneStatus, 'paneRowStatus');
    const { rerender, props, container } = renderWorkspaceSection({ named, panes: { lead, worker }, activeWorkspace: 'Project', layout });
    status.mockClear();
    for (const ratio of [0.4, 0.6, 0.7]) {
      const resized = { ...layout, ratio };
      rerender(<WorkspaceSection {...props} layout={resized} named={[{ ...named[0], layout: resized }]} />);
    }
    expect(status).not.toHaveBeenCalled();
    rerender(<WorkspaceSection {...props} panes={{ lead, worker: { ...worker, state: 'busy' } }} />);
    expect(status).toHaveBeenCalled();
    expect(container.querySelector('.ws-row .pane-tree-dot')).toHaveClass('is-busy');
  });

});
