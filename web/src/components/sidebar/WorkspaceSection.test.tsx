import * as paneStatus from '../../lib/paneStatus';
import { act, createEvent, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../../store';
import type { PaneInfo, Workspace } from '../../types';
import { WorkspaceSection } from './WorkspaceSection';
import { api } from '../../api';

const dialogMocks = vi.hoisted(() => ({
  confirmDialog: vi.fn<(opts: { message: string; confirmLabel?: string; danger?: boolean }) => Promise<boolean>>(),
  promptDialog: vi.fn<(opts: { title: string; defaultValue?: string }) => Promise<string | null>>(),
}));
vi.mock('../../lib/dialog', () => ({ confirmDialog: dialogMocks.confirmDialog, promptDialog: dialogMocks.promptDialog }));

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
  it('starts workers folded, bubbles attention, remembers the person\'s choice, and navigates visible rows', () => {
    const lead = pane({ id: 'lead', title: 'Lead', state: 'idle' });
    const worker = pane({ id: 'worker', title: 'Worker', state: 'choice-prompt', attention: 'needs-user', origin: { source: 'agent', byPaneId: 'lead', at: 1 } });
    const props = { named: [workspace({ name: 'Project', paneIds: ['lead'] })], panes: { lead, worker }, activeWorkspace: 'Project' };
    const first = renderWorkspaceSection(props);
    // A top-level pane's workers start folded, with nothing stored yet.
    expect(screen.getByRole('treeitem', { name: 'Lead' })).toHaveAttribute('aria-expanded', 'false');
    expect(localStorage.getItem('solu.sidebarTreeCollapsed.v1')).toBeNull();
    expect(screen.getByRole('treeitem', { name: 'Lead' })).toHaveTextContent('1 worker');
    expect(screen.queryByRole('treeitem', { name: 'Worker' })).not.toBeInTheDocument();
    // The lead itself is fine: a ring and a count say something below needs you.
    expect(first.container.querySelector('.workspace-pane-row .pane-tree-dot')).toHaveClass('is-needs-you-below');
    expect(screen.getByRole('treeitem', { name: 'Lead' }).querySelector('.pane-tree-attn-count')).toHaveTextContent('1●');
    expect(first.container.querySelector('.ws-row .pane-tree-dot')).toHaveClass('is-needs-you');
    // Opening it is remembered across a reload; folding it again is too.
    fireEvent.click(screen.getByRole('button', { name: 'Expand Lead' }));
    first.unmount();
    const second = renderWorkspaceSection(props);
    expect(screen.getByRole('treeitem', { name: 'Worker' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Collapse Lead' }));
    second.unmount();
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

  it('collapses and expands every branch from the header, and Alt+click folds a subtree', () => {
    const lead = pane({ id: 'lead', title: 'Lead', state: 'idle' });
    const worker = pane({ id: 'worker', title: 'Worker', state: 'idle', origin: { source: 'agent', byPaneId: 'lead', at: 1 } });
    const sub = pane({ id: 'sub', title: 'Sub', state: 'idle', origin: { source: 'agent', byPaneId: 'worker', leadPaneId: 'lead', at: 2 } });
    const other = pane({ id: 'other', title: 'Other shell', state: 'idle' });
    renderWorkspaceSection({
      named: [workspace({ name: 'Project', paneIds: ['lead'] }), workspace({ name: 'Second', paneIds: ['other'] }), workspace({ name: 'Empty', paneIds: [] })],
      panes: { lead, worker, sub, other }, activeWorkspace: 'Project',
    });
    // An empty workspace has nothing to open: no chevron button.
    expect(screen.queryByRole('button', { name: 'Collapse Empty' })).not.toBeInTheDocument();
    // The lead's workers start folded, so the header first offers Expand all.
    fireEvent.click(screen.getByRole('button', { name: 'Expand all' }));
    expect(screen.getByRole('treeitem', { name: 'Sub' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Collapse all' }));
    for (const name of ['Project', 'Second']) expect(screen.getByRole('treeitem', { name })).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('treeitem', { name: 'Lead' })).not.toBeInTheDocument();
    expect(JSON.parse(localStorage.getItem('solu.sidebarTreeCollapsed.v1')!)).toEqual({
      Project: true, 'Project:pane:lead': true, 'Project:pane:worker': true, Second: true,
    });
    fireEvent.click(screen.getByRole('button', { name: 'Expand all' }));
    expect(screen.getByRole('treeitem', { name: 'Sub' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Collapse all' })).toBeInTheDocument();
    // Alt+click closes the lead and everything under it; a plain reopen keeps
    // the inner branch closed, and Alt+click opens the whole subtree again.
    fireEvent.click(screen.getByRole('button', { name: 'Collapse Lead' }), { altKey: true });
    fireEvent.click(screen.getByRole('button', { name: 'Expand Lead' }));
    expect(screen.getByRole('treeitem', { name: 'Worker' })).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('treeitem', { name: 'Sub' })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Collapse Lead' }));
    fireEvent.click(screen.getByRole('button', { name: 'Expand Lead' }), { altKey: true });
    expect(screen.getByRole('treeitem', { name: 'Sub' })).toBeInTheDocument();
    // Alt+click on a workspace chevron folds it with every lead inside.
    fireEvent.click(screen.getByRole('button', { name: 'Collapse Project' }), { altKey: true });
    fireEvent.click(screen.getByRole('button', { name: 'Expand Project' }));
    expect(screen.getByRole('treeitem', { name: 'Lead' })).toHaveAttribute('aria-expanded', 'false');
  });

  it('opens (*) and closes (-) a row and its siblings from the keyboard', () => {
    const a = pane({ id: 'a', title: 'Lead A', state: 'idle' });
    const b = pane({ id: 'b', title: 'Lead B', state: 'idle' });
    const wa = pane({ id: 'wa', title: 'Worker A', state: 'idle', origin: { source: 'agent', byPaneId: 'a', at: 1 } });
    const wb = pane({ id: 'wb', title: 'Worker B', state: 'idle', origin: { source: 'agent', byPaneId: 'b', at: 2 } });
    const other = pane({ id: 'other', title: 'Other shell', state: 'idle' });
    renderWorkspaceSection({
      named: [workspace({ name: 'Project', paneIds: ['a', 'b'] }), workspace({ name: 'Second', paneIds: ['other'] })],
      panes: { a, b, wa, wb, other }, activeWorkspace: 'Project',
    });
    const leadA = screen.getByRole('treeitem', { name: 'Lead A' });
    act(() => leadA.focus());
    fireEvent.keyDown(leadA, { key: '-' });
    expect(leadA).toHaveAttribute('aria-expanded', 'false');
    expect(screen.getByRole('treeitem', { name: 'Lead B' })).toHaveAttribute('aria-expanded', 'false');
    expect(screen.getByRole('treeitem', { name: 'Project' })).toHaveAttribute('aria-expanded', 'true');
    fireEvent.keyDown(leadA, { key: '*' });
    expect(screen.getByRole('treeitem', { name: 'Worker A' })).toBeInTheDocument();
    expect(screen.getByRole('treeitem', { name: 'Worker B' })).toBeInTheDocument();
    // On a workspace row the siblings are the other workspaces.
    const project = screen.getByRole('treeitem', { name: 'Project' });
    act(() => project.focus());
    fireEvent.keyDown(project, { key: '-' });
    expect(screen.getByRole('treeitem', { name: 'Second' })).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('treeitem', { name: 'Lead A' })).not.toBeInTheDocument();
  });

  it('opens the workspace menu from the keyboard', () => {
    const setCtxMenu = vi.fn();
    renderWorkspaceSection({ setCtxMenu });
    const row = screen.getByRole('treeitem', { name: 'Launch Ops With A Very Long Workspace Name' });
    act(() => row.focus());
    fireEvent.keyDown(row, { key: 'F10', shiftKey: true });
    expect(setCtxMenu).toHaveBeenLastCalledWith(expect.objectContaining({ wsName: 'Launch Ops With A Very Long Workspace Name' }));
  });

  it('forgets choices for panes that have closed when it next saves', () => {
    localStorage.setItem('solu.sidebarTreeCollapsed.v1', JSON.stringify({ 'Project:pane:gone': false, 'Removed workspace': true, Project: 'junk' }));
    const lead = pane({ id: 'lead', title: 'Lead', state: 'idle' });
    const worker = pane({ id: 'worker', title: 'Worker', state: 'idle', origin: { source: 'agent', byPaneId: 'lead', at: 1 } });
    renderWorkspaceSection({ named: [workspace({ name: 'Project', paneIds: ['lead'] })], panes: { lead, worker }, activeWorkspace: 'Project' });
    fireEvent.click(screen.getByRole('button', { name: 'Expand Lead' }));
    expect(JSON.parse(localStorage.getItem('solu.sidebarTreeCollapsed.v1')!)).toEqual({ 'Project:pane:lead': false });
  });

  it('shows a lead role as a chip without repeating it in the title', () => {
    const lead = pane({ id: 'lead', title: '@cmo · marketing', state: 'idle', identity: { handle: 'cmo' }, origin: { source: 'user', at: 1, teamKind: 'lead', leadRole: 'marketing' } });
    const { container } = renderWorkspaceSection({ named: [workspace({ name: 'Project', paneIds: ['lead'] })], panes: { lead }, activeWorkspace: 'Project' });
    const row = container.querySelector('.workspace-pane-row')!;
    expect(row).toHaveClass('is-lead');
    expect(row.querySelector('.pane-tree-role-chip')).toHaveTextContent(/^marketing$/);
    expect(row.querySelector('.pane-tree-title')).toHaveTextContent(/^@cmo$/);
  });

  it('reads as the handle, then what the pane is doing, with status words for screen readers', () => {
    const lead = pane({ id: 'lead', title: '@ops · deploys', state: 'idle', identity: { handle: 'ops', workingOn: 'Rolling out 1.2' } });
    const asking = pane({ id: 'asking', title: '@db migrate', state: 'choice-prompt', attention: 'needs-user', identity: { handle: 'db' }, origin: { source: 'agent', byPaneId: 'lead', at: 1 } });
    const quiet = pane({ id: 'quiet', title: 'Plain worker', state: 'idle', origin: { source: 'agent', byPaneId: 'lead', at: 2 } });
    const ask = { id: 'a1', text: 'Back up the key', source: '@keys', createdAt: 1 };
    const posted = pane({ id: 'posted', title: '@keys rotate', state: 'idle', asks: [ask, { ...ask, id: 'a2' }], identity: { handle: 'keys' }, origin: { source: 'agent', byPaneId: 'lead', at: 3 } });
    renderWorkspaceSection({ named: [workspace({ name: 'Project', paneIds: ['lead'] })], panes: { lead, asking, quiet, posted }, activeWorkspace: 'Project' });
    const row = (name: string) => screen.getByRole('treeitem', { name }).querySelector(':scope > .workspace-pane-row')!;
    // Folded by default, the counts stand in for its rows.
    expect(row('@ops · deploys').querySelector('.pane-tree-count')).toHaveTextContent(/^3w$/);
    fireEvent.click(screen.getByRole('button', { name: 'Expand @ops · deploys' }));
    expect(row('@ops · deploys').querySelector('.pane-tree-desc')).toHaveTextContent(/^Rolling out 1\.2$/);
    expect(row('@db · migrate').querySelector('.pane-tree-desc')).toHaveTextContent(/^needs answer · migrate$/);
    expect(row('@keys · rotate').querySelector('.pane-tree-desc')).toHaveTextContent(/^2 asks · rotate$/);
    expect(row('Plain worker').querySelector('.pane-tree-desc')).toBeNull();
    expect(screen.getByRole('treeitem', { name: '@ops · deploys' })).toHaveAccessibleDescription('idle · 3 workers');
    expect(screen.getByRole('treeitem', { name: '@db · migrate' })).toHaveAccessibleDescription('needs answer');
    expect(screen.getByRole('treeitem', { name: '@keys · rotate' })).toHaveAccessibleDescription('2 asks');
    // Counts show only on a folded branch, where its rows are out of sight.
    // Open, the rows replace the counts; folded again, they come back.
    expect(row('@ops · deploys').querySelector('.pane-tree-count')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Collapse @ops · deploys' }));
    const count = row('@ops · deploys').querySelector('.pane-tree-count');
    expect(count).toHaveTextContent(/^3w$/);
    expect(count).toHaveAttribute('title', '3 workers');
    expect(row('@ops · deploys').querySelector('.pane-tree-attn-count')).toHaveTextContent('2●');
  });

  it('shows a lead another lead started as a peer, with its workers folded under it and its attention on its row', () => {
    const lead = pane({ id: 'lead', title: '@lead · launch', state: 'idle', identity: { handle: 'lead' }, origin: { source: 'user', at: 1, teamKind: 'lead', leadRole: 'launch' } });
    const qa = pane({ id: 'qa', title: '@qa · qa', state: 'idle', identity: { handle: 'qa' }, origin: { source: 'agent', byPaneId: 'lead', leadPaneId: 'lead', at: 2, teamKind: 'lead', leadRole: 'qa' } });
    const check = pane({ id: 'check', title: 'Run checks', state: 'choice-prompt', attention: 'needs-user', origin: { source: 'agent', byPaneId: 'qa', leadPaneId: 'lead', at: 3, teamKind: 'worker' } });
    renderWorkspaceSection({ named: [workspace({ name: 'Project', paneIds: ['lead'] })], panes: { lead, qa, check }, activeWorkspace: 'Project' });
    const qaItem = screen.getByRole('treeitem', { name: '@qa · qa' });
    // A peer of its starter, not nested under it, and not counted as its lead.
    expect(qaItem).toHaveAttribute('aria-level', '2');
    expect(screen.getByRole('treeitem', { name: '@lead · launch' })).not.toHaveAttribute('aria-expanded');
    // Who started it stays as provenance: tooltip and accessible description.
    expect(qaItem.querySelector(':scope > .workspace-pane-row')).toHaveAttribute('title', '@qa · qa\nStarted by @lead');
    expect(qaItem).toHaveAccessibleDescription('idle · 1 worker · started by @lead');
    // Its worker nests under it, folded by default, and still raises the flag.
    expect(qaItem).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('treeitem', { name: 'Run checks' })).not.toBeInTheDocument();
    expect(qaItem.querySelector('.pane-tree-dot')).toHaveClass('is-needs-you-below');
    expect(qaItem.querySelector('.pane-tree-attn-count')).toHaveTextContent('1●');
    fireEvent.click(screen.getByRole('button', { name: 'Expand @qa · qa' }));
    expect(screen.getByRole('treeitem', { name: 'Run checks' })).toHaveAttribute('aria-level', '3');
  });

  it('opens a pane menu on a tree row, from the mouse or the keyboard, that makes it a lead', async () => {
    const mine = pane({ id: 'mine', title: '@ops', state: 'idle', identity: { handle: 'ops' } });
    const setPaneTeam = vi.spyOn(api, 'setPaneTeam').mockResolvedValue({ origin: { source: 'user', at: 1, teamKind: 'lead', leadRole: 'ops' } });
    dialogMocks.promptDialog.mockResolvedValue('ops');
    const onLoadWorkspace = vi.fn();
    renderWorkspaceSection({ named: [workspace({ name: 'Project', paneIds: ['mine'] })], panes: { mine }, activeWorkspace: 'Project', onLoadWorkspace });
    const row = screen.getByRole('treeitem', { name: '@ops' });
    fireEvent.contextMenu(row.querySelector(':scope > .workspace-pane-row')!);
    expect(screen.getByRole('menu', { name: '@ops actions' })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: 'Make worker' })).not.toBeInTheDocument();
    expect(onLoadWorkspace).not.toHaveBeenCalled();
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(screen.queryByRole('menu', { name: '@ops actions' })).not.toBeInTheDocument();
    act(() => row.focus());
    fireEvent.keyDown(row, { key: 'F10', shiftKey: true });
    await act(async () => { fireEvent.click(screen.getByRole('menuitem', { name: 'Make lead…' })); });
    expect(dialogMocks.promptDialog).toHaveBeenCalledWith(expect.objectContaining({ title: 'Make lead', defaultValue: 'ops' }));
    expect(setPaneTeam).toHaveBeenCalledWith('mine', 'lead', 'ops', expect.anything());
    expect(screen.queryByRole('menu', { name: '@ops actions' })).not.toBeInTheDocument();
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
