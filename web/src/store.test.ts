import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Layout, PaneInfo, Workspace } from './types';
import { api, ApiError, currentRoutingScope } from './api';
import {
  ariaObservationFingerprint,
  computeParkedPaneIds,
  isCachedFirstWorkspaceSwitchEnabled,
  mergePaneInfos,
  resolveWorkspaceInitialLayout,
  shouldBlankWorkspaceSwitch,
  useStore,
  terminalSessionKey,
  workspaceSwitchTouchesCodex,
} from './store';
import { layoutHasLeaf, leafCount, paneIdsInOrder, pruneLayout } from './lib/layout';
import { collectPaneDefs, dropUnconfirmedSeeds, isPresentPane, isRunningPane, optimisticPaneFromDef, pickWorkspaceByName } from './lib/workspace';
import { computeNativeWorking } from './native/nativeTranscript';

const pane = (id: string): Layout => ({ type: 'pane', paneId: id });
const split = (a: Layout, b: Layout, dir: 'horizontal' | 'vertical' = 'horizontal'): Layout => ({
  type: 'split',
  direction: dir,
  ratio: 0.5,
  a,
  b,
});

beforeEach(() => {
  vi.restoreAllMocks();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('leafCount', () => {
  it('returns 0 for a null layout', () => {
    expect(leafCount(null)).toBe(0);
  });

  it('treats every non-split node as a single leaf', () => {
    expect(leafCount(pane('p1'))).toBe(1);
    expect(leafCount({ type: 'browser', browserId: 'b1' })).toBe(1);
    expect(leafCount({ type: 'file', fileId: 'f1' })).toBe(1);
  });

  it('sums leaves recursively across nested splits', () => {
    const tree = split(pane('p1'), split(pane('p2'), pane('p3')));
    expect(leafCount(tree)).toBe(3);
  });

  it('handles deeply unbalanced trees correctly', () => {
    const tree = split(split(split(pane('a'), pane('b')), pane('c')), pane('d'));
    expect(leafCount(tree)).toBe(4);
  });
});

describe('paneIdsInOrder', () => {
  it('returns an empty array for a null layout', () => {
    expect(paneIdsInOrder(null)).toEqual([]);
  });

  it('returns the single pane id for a leaf', () => {
    expect(paneIdsInOrder(pane('only'))).toEqual(['only']);
  });

  it('walks the tree left-to-right depth-first', () => {
    const tree = split(pane('p1'), split(pane('p2'), pane('p3')));
    expect(paneIdsInOrder(tree)).toEqual(['p1', 'p2', 'p3']);
  });

  it('skips browser/file leaves', () => {
    const tree = split(pane('p1'), split({ type: 'browser', browserId: 'b1' }, pane('p2')));
    expect(paneIdsInOrder(tree)).toEqual(['p1', 'p2']);
  });
});

describe('layoutHasLeaf', () => {
  it('finds pane, browser, and file leaves in nested layouts', () => {
    const tree = split(
      pane('p1'),
      split({ type: 'browser', browserId: 'b1' }, { type: 'file', fileId: 'f1' }, 'vertical'),
    );

    expect(layoutHasLeaf(tree, 'p1')).toBe(true);
    expect(layoutHasLeaf(tree, 'b1')).toBe(true);
    expect(layoutHasLeaf(tree, 'f1')).toBe(true);
    expect(layoutHasLeaf(tree, 'missing')).toBe(false);
    expect(layoutHasLeaf(null, 'p1')).toBe(false);
  });
});

describe('pruneLayout', () => {
  it('removes dead leaves and collapses empty split branches', () => {
    const tree = split(
      pane('keep'),
      split({ type: 'browser', browserId: 'drop-browser' }, { type: 'file', fileId: 'keep-file' }, 'vertical'),
    );
    expect(pruneLayout(tree, (id) => id === 'keep' || id === 'keep-file')).toEqual(
      split(pane('keep'), { type: 'file', fileId: 'keep-file' }),
    );
  });
});

describe('workspace helpers', () => {
  it('collects pane defs in layout order and skips missing panes', () => {
    const layout = split(pane('p1'), split(pane('missing'), pane('p2')));
    const panes: Record<string, PaneInfo> = {
      p1: { id: 'p1', cwd: '/one', cmd: ['bash'], title: 'one', cols: 80, rows: 24 } as PaneInfo,
      p2: { id: 'p2', cwd: '/two', cmd: ['claude'], title: 'two', cols: 120, rows: 40 } as PaneInfo,
    };
    expect(collectPaneDefs(layout, panes)).toEqual([
      { id: 'p1', cwd: '/one', cmd: ['bash'], title: 'one', cols: 80, rows: 24 },
      { id: 'p2', cwd: '/two', cmd: ['claude'], title: 'two', cols: 120, rows: 40 },
    ]);
  });

  it('prefers a backend-specific workspace when available', () => {
    const workspaces: Workspace[] = [
      { name: 'Default', layout: null, paneIds: [], activeAt: '1', backend: 'wsl' },
      { name: 'Default', layout: null, paneIds: [], activeAt: '2', backend: 'win' },
    ];
    expect(pickWorkspaceByName(workspaces, 'Default', 'win')).toBe(workspaces[1]);
    expect(pickWorkspaceByName(workspaces, 'Default', null)).toBe(workspaces[0]);
    expect(pickWorkspaceByName(workspaces, 'Missing', null)).toBeNull();
  });

  it('only treats running panes as alive for workspace restore', () => {
    const running = { id: 'live', status: 'running' } as PaneInfo;
    const exited = { id: 'dead', status: 'exited' } as PaneInfo;

    expect(isRunningPane(running)).toBe(true);
    expect(isRunningPane(exited)).toBe(false);
    expect(isRunningPane(undefined)).toBe(false);
  });

  it('treats suspended-resumable panes as present (no def-respawn twin)', () => {
    // After a full teardown (reboot) the daemon parks Claude panes as
    // exited+resumable for an in-place Resume. Layout alive-checks must keep
    // them: pruning + def-respawning creates a duplicate pane from stale argv
    // and strands the suspended pane outside every workspace.
    const suspended = { id: 's', status: 'exited', resumable: true } as PaneInfo;
    const dead = { id: 'd', status: 'exited' } as PaneInfo;
    const running = { id: 'r', status: 'running' } as PaneInfo;

    expect(isPresentPane(suspended)).toBe(true);
    expect(isPresentPane(running)).toBe(true);
    expect(isPresentPane(dead)).toBe(false);
    expect(isPresentPane(undefined)).toBe(false);
  });

  it('keeps suspended-resumable panes in the restored workspace layout', () => {
    const layout = split(pane('live'), pane('suspended'));
    const alive = mergePaneInfos({
      live: { id: 'live', status: 'running' } as PaneInfo,
      suspended: { id: 'suspended', status: 'exited', resumable: true } as PaneInfo,
    }, []);

    expect(resolveWorkspaceInitialLayout(layout, alive, {}, {})).toEqual(layout);
  });

  it('builds cached-first workspace layouts from running cached panes', () => {
    const layout = split(pane('plain'), pane('codex'));
    const alive = mergePaneInfos({
      plain: { id: 'plain', status: 'running', hasCodex: false } as PaneInfo,
      codex: { id: 'codex', status: 'running', hasCodex: true } as PaneInfo,
    }, []);

    expect(resolveWorkspaceInitialLayout(layout, alive, {}, {})).toEqual(layout);
  });

  it('keeps cached pane info when the fresh pane pass omits it', () => {
    const alive = mergePaneInfos({
      cached: { id: 'cached', status: 'running' } as PaneInfo,
    }, []);

    expect(isRunningPane(alive.get('cached'))).toBe(true);
  });

  it('lets fresh pane info replace stale cached pane info', () => {
    const alive = mergePaneInfos(
      { p1: { id: 'p1', status: 'exited' } as PaneInfo },
      [{ id: 'p1', status: 'running' } as PaneInfo],
    );

    expect(isRunningPane(alive.get('p1'))).toBe(true);
  });

  it('synthesizes an optimistic running pane from a saved def', () => {
    // The cached-first switch seeds a destination pane the global cache hasn't
    // globalized yet (3s poll lag) so the layout isn't pruned to empty. The seed
    // must read as running (no prune), carry the def's cwd/cmd/title (so the new
    // pane detects the right cwd), and default cols/rows when the def omits them.
    const seeded = optimisticPaneFromDef({
      id: 'p1', cwd: '/mnt/c/code', cmd: ['bash'], title: 'shell',
    });
    expect(isRunningPane(seeded)).toBe(true);
    expect(seeded.cwd).toBe('/mnt/c/code');
    expect(seeded.cmd).toEqual(['bash']);
    expect(seeded.title).toBe('shell');
    expect(seeded.cols).toBe(80);
    expect(seeded.rows).toBe(24);
    expect(seeded.state).toBe('idle'); // not 'busy' — no spurious activity badge
    // Explicit cols/rows from the def are preserved.
    const sized = optimisticPaneFromDef({
      id: 'p2', cwd: '/x', cmd: ['zsh'], title: 'z', cols: 120, rows: 40,
    });
    expect(sized.cols).toBe(120);
    expect(sized.rows).toBe(40);
  });

  it('carries kind through optimistic seeds so a native pane seeds as native', () => {
    const term = optimisticPaneFromDef({ id: 't', cwd: '/x', cmd: ['bash'], title: 't' });
    expect(term.kind).toBeUndefined(); // terminal
    const native = optimisticPaneFromDef({ id: 'n', cwd: '/x', cmd: ['claude'], title: 'n', kind: 'native' });
    expect(native.kind).toBe('native'); // renders as NativePane, not a terminal showing JSONL
  });

  it('drops only the unconfirmed seeds, keeps confirmed, and is no-op on an empty fetch', () => {
    const mk = (id: string): PaneInfo => ({ id, status: 'running' } as PaneInfo);
    const seededIds = new Set(['a', 'b', 'c']);
    // Fresh list confirms a and c (already replaced with real data by the caller);
    // b is absent → genuinely dead → must be deleted so the respawn loop recreates it.
    const alive = new Map<string, PaneInfo>([
      ['a', mk('a')], ['b', optimisticPaneFromDef({ id: 'b', cwd: '/x', cmd: ['bash'], title: 'b' })], ['c', mk('c')],
    ]);
    dropUnconfirmedSeeds(alive, seededIds, [mk('a'), mk('c')]);
    expect(alive.has('a')).toBe(true);
    expect(alive.has('b')).toBe(false); // unconfirmed seed dropped
    expect(alive.has('c')).toBe(true);
    // Empty/partial fetch is non-authoritative → keep ALL seeds (don't drop live panes).
    const alive2 = new Map<string, PaneInfo>([['z', optimisticPaneFromDef({ id: 'z', cwd: '/x', cmd: ['bash'], title: 'z' })]]);
    dropUnconfirmedSeeds(alive2, new Set(['z']), []);
    expect(alive2.has('z')).toBe(true);
  });

  it('resolves a layout whose panes were optimistically seeded (no prune to empty)', () => {
    // Regression: switching to a workspace whose panes aren't in the carried-over
    // cache must NOT prune the layout away. With seeding, resolveWorkspaceInitialLayout
    // keeps the layout intact (so loadWorkspace sets a non-null focusedPaneId)
    // WITHOUT any blocking listPanes fetch.
    const layout = split(pane('a'), pane('b'));
    const seeded = mergePaneInfos({}, []); // empty cache — nothing globalized yet
    for (const def of [
      { id: 'a', cwd: '/x', cmd: ['bash'], title: 'a' },
      { id: 'b', cwd: '/y', cmd: ['bash'], title: 'b' },
    ]) {
      seeded.set(def.id, optimisticPaneFromDef(def));
    }
    expect(resolveWorkspaceInitialLayout(layout, seeded, {}, {})).toEqual(layout);
  });

  it('uses the workspace blank phase by outgoing pane count', () => {
    const three = split(split(pane('a'), pane('b')), pane('c'));
    expect(shouldBlankWorkspaceSwitch(three)).toBe(true);
    expect(shouldBlankWorkspaceSwitch(split(pane('a'), pane('b')))).toBe(false);
    expect(shouldBlankWorkspaceSwitch(null)).toBe(false);
  });

  it('detects Codex panes before using cached-first workspace switching', () => {
    expect(workspaceSwitchTouchesCodex(
      pane('p1'),
      null,
      { p1: { id: 'p1', status: 'running', hasCodex: true } as PaneInfo },
    )).toBe(true);
    expect(workspaceSwitchTouchesCodex(
      null,
      pane('p3'),
      {},
      [{ id: 'p3', cmd: ['codex'] }],
    )).toBe(true);
    expect(workspaceSwitchTouchesCodex(
      pane('shell'),
      pane('claude'),
      {
        shell: { id: 'shell', status: 'running', cmd: ['bash'] } as PaneInfo,
        claude: { id: 'claude', status: 'running', cmd: ['claude'] } as PaneInfo,
      },
    )).toBe(false);
  });

  it('enables cached-first workspace switching by default, with a "0" opt-out', () => {
    localStorage.removeItem('solu.cachedFirstWorkspaceSwitch');
    expect(isCachedFirstWorkspaceSwitchEnabled()).toBe(true);
    localStorage.setItem('solu.cachedFirstWorkspaceSwitch', '0');
    expect(isCachedFirstWorkspaceSwitchEnabled()).toBe(false);
    localStorage.setItem('solu.cachedFirstWorkspaceSwitch', '1');
    expect(isCachedFirstWorkspaceSwitchEnabled()).toBe(true);
    localStorage.removeItem('solu.cachedFirstWorkspaceSwitch');
  });
});

describe('computeParkedPaneIds', () => {
  const running = (id: string, extra: Partial<PaneInfo> = {}): PaneInfo =>
    ({ id, status: 'running', cmd: ['bash'], ...extra }) as PaneInfo;
  const panes: Record<string, PaneInfo> = {
    a: running('a'),
    b: running('b'),
    c: running('c'),
    dead: { id: 'dead', status: 'exited', cmd: ['bash'] } as PaneInfo,
  };

  it('parks outgoing panes first, keeps prior parked, skips incoming', () => {
    const out = computeParkedPaneIds(['c'], ['a', 'b'], new Set(['b']), panes);
    expect(out).toEqual(['a', 'c']);
  });

  it('drops dead panes', () => {
    const out = computeParkedPaneIds([], ['a', 'dead'], new Set(), panes);
    expect(out).toEqual(['a']);
  });

  it('caps at the warm limit, newest-parked wins', () => {
    const many: Record<string, PaneInfo> = {};
    const outgoing: string[] = [];
    const prior: string[] = [];
    for (let i = 0; i < 10; i++) {
      many[`o${i}`] = running(`o${i}`);
      outgoing.push(`o${i}`);
      many[`p${i}`] = running(`p${i}`);
      prior.push(`p${i}`);
    }
    const out = computeParkedPaneIds(prior, outgoing, new Set(), many, 12);
    expect(out).toHaveLength(12);
    expect(out.slice(0, 10)).toEqual(outgoing);
    expect(out.slice(10)).toEqual(['p0', 'p1']);
  });

  it('dedupes a pane present in both outgoing and prior parked', () => {
    const out = computeParkedPaneIds(['a'], ['a'], new Set(), panes);
    expect(out).toEqual(['a']);
  });
});

describe('ariaObservationFingerprint', () => {
  it('is stable across thinking-spinner re-renders', () => {
    // Claude's thinking indicator + token count change every redraw,
    // and dedup must survive that. Same prompt with different chrome
    // should fingerprint identically.
    const a = [
      '✻ Pondering… (3.2k tokens, ctx: 28%)',
      '> Do you want to commit these changes?',
      '  1. Yes',
      '  2. No',
    ].join('\n');
    const b = [
      '✻ Crunching… (8.4k tokens, ctx: 41%)',
      '> Do you want to commit these changes?',
      '  1. Yes',
      '  2. No',
    ].join('\n');
    expect(ariaObservationFingerprint(a)).toBe(ariaObservationFingerprint(b));
  });

  it('produces different fingerprints for different prompts', () => {
    const a = '> Do you want to commit?\n  1. Yes\n  2. No';
    const b = '> Do you want to push?\n  1. Yes\n  2. No';
    expect(ariaObservationFingerprint(a)).not.toBe(ariaObservationFingerprint(b));
  });

  it('caps the fingerprint length so a giant tail can\'t blow up the dedup map', () => {
    const huge = Array.from({ length: 500 }, (_, i) => `line content number ${i}`).join('\n');
    expect(ariaObservationFingerprint(huge).length).toBeLessThanOrEqual(800);
  });
});

describe('bootstrap failure state', () => {
  it('records bootstrap errors instead of falling through to the empty state', async () => {
    vi.spyOn(api, 'listPanes').mockRejectedValue(new Error('daemon unreachable'));
    vi.spyOn(api, 'listWorkspaces').mockResolvedValue([]);
    vi.spyOn(api, 'listPorts').mockResolvedValue([]);

    useStore.setState({
      layout: null,
      loading: true,
      bootstrapError: null,
      daemonOnline: true,
    });

    await useStore.getState().bootstrap();

    expect(useStore.getState().loading).toBe(false);
    expect(useStore.getState().bootstrapError).toBe('daemon unreachable');
    expect(useStore.getState().daemonOnline).toBe(false);
  });

  it('clears a previous bootstrap error after a successful retry', async () => {
    vi.spyOn(api, 'listPanes').mockResolvedValue([]);
    vi.spyOn(api, 'listWorkspaces').mockResolvedValue([]);
    vi.spyOn(api, 'listPorts').mockResolvedValue([]);

    useStore.setState({
      layout: null,
      loading: false,
      bootstrapError: 'old failure',
    });

    await useStore.getState().bootstrap();

    expect(useStore.getState().loading).toBe(false);
    expect(useStore.getState().bootstrapError).toBeNull();
  });
});

describe('zoom is cleared when focus routes to new content', () => {
  it('openFileHere drops an active zoom so the new tile is visible', () => {
    useStore.setState({
      layout: split(pane('pane_a'), pane('pane_b')),
      zoomedPaneId: 'pane_a',
      focusedPaneId: 'pane_a',
    });

    useStore.getState().openFileHere('/tmp/notes.txt', 'pane_a');

    const s = useStore.getState();
    expect(s.zoomedPaneId).toBeNull();
    // The new file tile is focused, and it's now a visible leaf.
    expect(s.focusedPaneId).not.toBe('pane_a');
    expect(layoutHasLeaf(s.layout, s.focusedPaneId!)).toBe(true);
  });

  it('addPaneToLayout drops a zoom on a different pane (ARIA-spawn graft)', () => {
    useStore.setState({
      panes: {
        pane_a: { id: 'pane_a' } as PaneInfo,
        pane_new: { id: 'pane_new' } as PaneInfo,
      },
      layout: pane('pane_a'),
      zoomedPaneId: 'pane_a',
      focusedPaneId: 'pane_a',
    });

    useStore.getState().addPaneToLayout('pane_new');

    const s = useStore.getState();
    expect(s.zoomedPaneId).toBeNull();
    expect(s.focusedPaneId).toBe('pane_new');
  });
});

// Regression: switching workspace away and back used to revert a layout
// re-arrangement (e.g. "3 vertical") to the OLD arrangement. Root cause:
// the disk got the new layout but the in-memory `workspaces` cache kept the
// stale snapshot — a same-panes re-arrangement is invisible to the
// summary-based poll (paneId set is unchanged), and loadWorkspace restores
// from that cache. Fix: write through to the cache on every save.
describe('layout re-arrangement survives a workspace switch', () => {
  it('mergeSavedWorkspaceIntoCache folds the saved layout into the matching entry', () => {
    const newLayout = split(pane('p1'), pane('p2'), 'vertical');
    useStore.setState({
      activeBackend: null,
      workspaces: [
        { name: 'A', layout: pane('p1'), paneIds: ['p1'], summary: true } as Workspace,
        { name: 'B', layout: pane('pb'), paneIds: ['pb'], summary: false } as Workspace,
      ],
    });

    useStore.getState().mergeSavedWorkspaceIntoCache({
      name: 'A',
      layout: newLayout,
      paneIds: ['p1', 'p2'],
    } as Workspace);

    const ws = useStore.getState().workspaces;
    const a = ws.find((w) => w.name === 'A')!;
    expect(a.layout).toEqual(newLayout);
    expect(a.paneIds).toEqual(['p1', 'p2']);
    expect(a.summary).toBe(false); // now has a real layout, no full-refetch needed
    // The other workspace is untouched.
    expect(ws.find((w) => w.name === 'B')!.layout).toEqual(pane('pb'));
  });

  it('autosave keeps the cached workspace layout in sync (no stale revert)', async () => {
    const oldLayout = split(pane('p1'), split(pane('p2'), pane('p3')), 'horizontal');
    // Echo the posted workspace, exactly like the daemon's putWorkspace.
    const saveSpy = vi
      .spyOn(api, 'saveWorkspace')
      .mockImplementation(async (w) => ({ ...w }) as Workspace);
    // The poll re-derives summaries from whatever the cache now holds, so it
    // can't undo the write-through (it would have, pre-fix, by keeping the
    // stale `...prev` layout for an unchanged paneId set).
    vi.spyOn(api, 'listWorkspaceSummaries').mockImplementation(
      async () =>
        useStore.getState().workspaces.map((w) => ({
          name: w.name,
          paneIds: w.paneIds,
          activeAt: w.activeAt,
          backend: w.backend,
          protected: w.protected,
        })) as never,
    );

    useStore.setState({
      activeWorkspace: 'A',
      activeBackend: null,
      layout: oldLayout,
      panes: {
        p1: { id: 'p1' } as PaneInfo,
        p2: { id: 'p2' } as PaneInfo,
        p3: { id: 'p3' } as PaneInfo,
      },
      workspaces: [
        { name: 'A', layout: oldLayout, paneIds: ['p1', 'p2', 'p3'], summary: false } as Workspace,
        { name: 'B', layout: pane('pb'), paneIds: ['pb'], summary: false } as Workspace,
      ],
    });

    vi.useFakeTimers();
    try {
      useStore.getState().applyLayoutPreset('rows');
      const newLayout = useStore.getState().layout;
      // sanity: the preset actually changed the arrangement
      expect(newLayout).not.toEqual(oldLayout);
      // fire the 400ms debounced autosave + flush its promise chain
      await vi.advanceTimersByTimeAsync(500);
    } finally {
      vi.useRealTimers();
    }

    expect(saveSpy).toHaveBeenCalled();
    const cachedA = pickWorkspaceByName(useStore.getState().workspaces, 'A', null);
    // The cache now matches what we'd restore on switch-back — the NEW
    // layout, not the stale `oldLayout`.
    expect(cachedA?.layout).toEqual(useStore.getState().layout);
    expect(cachedA?.layout).not.toEqual(oldLayout);
  });
});

// Prompt PARSING is tested daemon-side (daemon/prompt_recorder_test.go) —
// the web only mirrors what the daemon committed, via the "pane-prompt"
// SSE event (setPaneLastPrompts) and the level-based DTO resync in
// refreshPanes.
describe('setPaneLastPrompts', () => {
  it('replaces the pane history from an SSE event', () => {
    useStore.setState({ lastPrompts: {} });

    useStore.getState().setPaneLastPrompts('pane_sse', ['newest', 'older']);

    expect(useStore.getState().lastPrompts[terminalSessionKey(currentRoutingScope(), 'pane_sse')]).toEqual(['newest', 'older']);
  });

  it('preserves the array reference when content is unchanged', () => {
    const initial = ['same prompt'];
    const key = terminalSessionKey(currentRoutingScope(), 'pane_ref');
    useStore.setState({ lastPrompts: { [key]: initial } });
    const beforeMap = useStore.getState().lastPrompts;

    useStore.getState().setPaneLastPrompts('pane_ref', ['same prompt']);

    // No-op update: both the per-pane array AND the map keep their
    // refs, so selectors keyed on them don't re-render.
    expect(useStore.getState().lastPrompts).toBe(beforeMap);
    expect(useStore.getState().lastPrompts[key]).toBe(initial);
  });

  it('updates a single pane without disturbing others', () => {
    const other = ['other pane prompt'];
    const otherKey = terminalSessionKey(currentRoutingScope(), 'pane_other');
    useStore.setState({ lastPrompts: { [otherKey]: other } });

    useStore.getState().setPaneLastPrompts('pane_target', ['fresh']);

    const s = useStore.getState().lastPrompts;
    expect(s[terminalSessionKey(currentRoutingScope(), 'pane_target')]).toEqual(['fresh']);
    expect(s[otherKey]).toBe(other);
  });
});

// The synchronous Term↔Focus switch ("now") used to bind the "Switching
// renderer…" overlay AND the success/failure verdict solely to the switch-mode
// POST response. Killing+respawning the very pane being switched can lose/delay
// that response even though the daemon completed and persisted the switch — so
// the overlay stranded (or, with the 30s fetch cap, falsely rolled back with an
// error toast on a switch that actually succeeded). switchPaneMode now
// reconciles against the authoritative pane state: a respawn lands a NEW pid
// (propagated by the panes poll/SSE); the authoritative kind tells success from
// a rolled-back respawn; a timeout is the final backstop. These guard that.
describe('switchPaneMode reconciles the renderer switch off authoritative pane state', () => {
  const paneInfo = (id: string, over: Partial<PaneInfo> = {}): PaneInfo =>
    ({ id, cwd: '/x', cmd: ['claude'], kind: '', status: 'running', state: 'idle', pid: 111, ...over } as PaneInfo);

  it('clears the overlay and keeps the target kind when a respawn is observed despite a lost POST', async () => {
    // The switch-mode POST never resolves (its response was lost in the respawn).
    vi.spyOn(api, 'switchPaneMode').mockReturnValue(new Promise<PaneInfo>(() => {}));
    useStore.setState({ layout: null, panes: { p1: paneInfo('p1') }, paneModeSwitching: {}, parkedPaneIds: [] });

    const pending = useStore.getState().switchPaneMode('p1', 'native'); // do NOT await
    // Optimistic flip + overlay are applied synchronously.
    expect(useStore.getState().panes.p1.kind).toBe('native');
    expect(useStore.getState().paneModeSwitching.p1).toBe(true);

    // Authoritative poll/SSE delivers the respawn: a NEW pid (kind already native).
    useStore.setState((s) => ({ panes: { ...s.panes, p1: { ...s.panes.p1, pid: 222 } } }));

    const result = await pending;
    expect((result as PaneInfo).kind).toBe('native');
    expect(useStore.getState().panes.p1.kind).toBe('native'); // not rolled back
    expect('p1' in useStore.getState().paneModeSwitching).toBe(false); // overlay cleared
  });

  it('fast path: a normal POST response merges its DTO and clears the overlay (no regression)', async () => {
    vi.spyOn(api, 'switchPaneMode').mockResolvedValue(paneInfo('p1', { kind: 'native', pid: 222, nativeEpoch: 2 }));
    useStore.setState({ layout: null, panes: { p1: paneInfo('p1') }, paneModeSwitching: {}, parkedPaneIds: [] });

    const result = await useStore.getState().switchPaneMode('p1', 'native');
    expect((result as PaneInfo).pid).toBe(222);
    expect(useStore.getState().panes.p1.nativeEpoch).toBe(2); // DTO merged
    expect('p1' in useStore.getState().paneModeSwitching).toBe(false);
  });

  it('rolls back the kind, toasts, and clears the overlay when the daemon fails and rolls back', async () => {
    vi.spyOn(api, 'switchPaneMode').mockRejectedValue(new Error('boom'));
    const toastSpy = vi.spyOn(useStore.getState(), 'toast').mockImplementation(() => {});
    useStore.setState({ layout: null, panes: { p1: paneInfo('p1') }, paneModeSwitching: {}, parkedPaneIds: [] });

    const pending = useStore.getState().switchPaneMode('p1', 'native');
    // The daemon's rollback respawns the ORIGINAL pane: new pid, kind back to ''.
    useStore.setState((s) => ({ panes: { ...s.panes, p1: { ...s.panes.p1, pid: 222, kind: '' } } }));

    await expect(pending).rejects.toThrow();
    expect(useStore.getState().panes.p1.kind).toBe(''); // rolled back to terminal
    expect(toastSpy).toHaveBeenCalled();
    expect('p1' in useStore.getState().paneModeSwitching).toBe(false);
  });

  it('timeout backstop: a lost POST with no respawn still clears the overlay (never strands)', async () => {
    vi.useFakeTimers();
    vi.spyOn(api, 'switchPaneMode').mockReturnValue(new Promise<PaneInfo>(() => {}));
    vi.spyOn(useStore.getState(), 'toast').mockImplementation(() => {});
    useStore.setState({ layout: null, panes: { p1: paneInfo('p1') }, paneModeSwitching: {}, parkedPaneIds: [] });

    const pending = useStore.getState().switchPaneMode('p1', 'native');
    pending.catch(() => {}); // a silent no-op switch resolves to a failure; swallow it
    expect(useStore.getState().paneModeSwitching.p1).toBe(true);

    // Neither the POST nor a respawn ever arrives → the SWITCH_RECONCILE_TIMEOUT_MS
    // (20s) backstop fires and the overlay clears unconditionally.
    await vi.advanceTimersByTimeAsync(20_100);
    expect('p1' in useStore.getState().paneModeSwitching).toBe(false);
    vi.useRealTimers();
  });
});

// The native Focus spinner reads pane.agentMidTurn, which the daemon publishes
// via the edge-fired `pane-state` SSE event. A settle (working→done) that fires
// while SSE is briefly dropped is lost forever, so the spinner sticks on a turn
// the daemon already finished. refreshPanes is the level resync (wired into
// es.onopen + the 60s fallback in App.tsx) that heals it. This guards that the
// resync actually flips a stale flag — and that computeNativeWorking then settles.
describe('refreshPanes keeps a pane whose resume is in flight', () => {
  it('does not drop the tile while the daemon respawns it, and shows the new failure', async () => {
    const lost = { id: 'pane_lost', cwd: '/gone', cmd: ['bash'], kind: '', status: 'exited', state: 'idle',
      resumable: true, recoveryError: 'The previous shell ended during shutdown.' } as PaneInfo;
    useStore.setState({ layout: { type: 'pane', paneId: 'pane_lost' }, panes: { pane_lost: lost }, parkedPaneIds: [] });
    let fail!: (e: Error) => void;
    vi.spyOn(api, 'resumePane').mockReturnValue(new Promise<PaneInfo>((_, reject) => { fail = reject; }));
    const resumed = useStore.getState().resumePane('pane_lost', 80, 24, '/gone').catch(() => null);

    // The daemon has freed the id mid-respawn, so the list omits the pane.
    vi.spyOn(api, 'listPanes').mockResolvedValue([]);
    await useStore.getState().refreshPanes();
    expect(useStore.getState().panes.pane_lost).toBeDefined();

    fail(new Error('working directory does not exist: /gone'));
    await resumed;
    expect(useStore.getState().panes.pane_lost.recoveryError).toBe('working directory does not exist: /gone');

    // Once the request settles, a pane the daemon no longer lists is dropped as before.
    await useStore.getState().refreshPanes();
    expect(useStore.getState().panes.pane_lost).toBeUndefined();
  });
});

describe('refreshPanes heals a stale native agentMidTurn (stuck Focus spinner)', () => {
  it('clears agentMidTurn true→false from the level DTO so the spinner settles', async () => {
    const stuck = {
      id: 'pane_stuck', cwd: '/x', cmd: ['claude'], kind: 'native',
      status: 'running', state: 'busy', agentMidTurn: true,
    } as PaneInfo;
    useStore.setState({ layout: null, panes: { pane_stuck: stuck }, parkedPaneIds: [] });

    // With the stale flag, the spinner reads "working" (non-null status).
    expect(
      computeNativeWorking([], 0, useStore.getState().panes.pane_stuck.agentMidTurn, 'pane_stuck'),
    ).not.toBeNull();

    // Daemon now reports the turn finished; the live pane-state SSE that would
    // have carried this was lost during an SSE gap. The level resync heals it.
    vi.spyOn(api, 'listPanes').mockResolvedValue([
      { ...stuck, state: 'idle', agentMidTurn: false } as PaneInfo,
    ]);

    await useStore.getState().refreshPanes();

    const healed = useStore.getState().panes.pane_stuck;
    expect(healed.agentMidTurn).toBe(false);
    // …and the Focus spinner now settles (no working status).
    expect(computeNativeWorking([], 0, healed.agentMidTurn, 'pane_stuck')).toBeNull();
  });
});

describe('refreshPanes tracks delegated pending DTO changes', () => {
  it('invalidates a flag-owner-only change without churning stable owners', async () => {
    const before = {
      id: 'pane_owner', cwd: '/x', cmd: ['claude'], title: 'worker',
      status: 'running', state: 'idle', flagged: true, flagMemo: 'Review', flagOwner: 'warden',
    } as PaneInfo;
    useStore.setState({ panes: { [before.id]: before }, layout: null, parkedPaneIds: [] });
    vi.spyOn(api, 'listPanes').mockResolvedValue([{ ...before, flagOwner: 'user' }]);
    await useStore.getState().refreshPanes();
    const changed = useStore.getState().panes[before.id];
    expect(changed).not.toBe(before);
    expect(changed.flagOwner).toBe('user');
    await useStore.getState().refreshPanes();
    expect(useStore.getState().panes[before.id]).toBe(changed);
  });

  it('invalidates only when delegatedPending changes', async () => {
    const before = {
      id: 'pane_pending', cwd: '/x', cmd: ['claude'], title: 'orch',
      status: 'running', state: 'idle', delegatedPending: undefined,
    } as PaneInfo;
    useStore.setState({ panes: { [before.id]: before }, layout: null, parkedPaneIds: [] });
    const pending = { count: 1, needsYou: 0, oldestAt: 1 };
    vi.spyOn(api, 'listPanes').mockResolvedValue([{ ...before, delegatedPending: pending }]);
    await useStore.getState().refreshPanes();
    const changed = useStore.getState().panes[before.id];
    expect(changed).not.toBe(before);
    expect(changed.delegatedPending).toEqual(pending);

    vi.mocked(api.listPanes).mockResolvedValue([{ ...before, delegatedPending: { ...pending } }]);
    await useStore.getState().refreshPanes();
    expect(useStore.getState().panes[before.id]).toBe(changed);
  });
});

describe('refreshPanes tracks managed Pi process detection', () => {
  it('invalidates when hasPi flips so the auto-approve badge can appear', async () => {
    const before = {
      id: 'pane_pi', cwd: '/x', cmd: ['node', 'launch.mjs', 'pi'], title: 'Pi',
      status: 'running', state: 'idle', hasClaude: false, hasPi: false,
    } as PaneInfo;
    useStore.setState({ panes: { [before.id]: before }, layout: null, parkedPaneIds: [] });

    // The daemon's process scan finds the managed Pi approval extension a beat
    // after spawn, so hasPi is the ONLY changed field on this poll. Without the
    // samePaneInfo guard the stale DTO would survive and the auto badge would
    // never render.
    vi.spyOn(api, 'listPanes').mockResolvedValue([{ ...before, hasPi: true }]);
    await useStore.getState().refreshPanes();
    const detected = useStore.getState().panes[before.id];
    expect(detected).not.toBe(before);
    expect(detected.hasPi).toBe(true);

    // Repeating the same DTO keeps object identity (no render churn).
    vi.mocked(api.listPanes).mockResolvedValue([{ ...before, hasPi: true }]);
    await useStore.getState().refreshPanes();
    expect(useStore.getState().panes[before.id]).toBe(detected);
  });
});

describe('refreshPanes tracks agent turn changes', () => {
  it('invalidates when a turn finishes so the header chip can update', async () => {
    const before = {
      id: 'pane_turn', cwd: '/x', cmd: ['claude'], title: 'claude',
      status: 'running', state: 'busy', hasClaude: true,
      turn: { active: true, startedMs: 1000, count: 0 },
    } as PaneInfo;
    useStore.setState({ panes: { [before.id]: before }, layout: null, parkedPaneIds: [] });

    // Only `turn` changes on this poll (state already idle-equal is not
    // required): active → finished with change stats.
    const finished = {
      ...before,
      turn: { active: false, count: 1, last: { n: 1, startedMs: 1000, endedMs: 9000, files: 2, added: 5, removed: 0 } },
    };
    vi.spyOn(api, 'listPanes').mockResolvedValue([finished]);
    await useStore.getState().refreshPanes();
    const got = useStore.getState().panes[before.id];
    expect(got).not.toBe(before);
    expect(got.turn?.last?.files).toBe(2);

    vi.mocked(api.listPanes).mockResolvedValue([{ ...finished, turn: { ...finished.turn } }]);
    await useStore.getState().refreshPanes();
    expect(useStore.getState().panes[before.id]).toBe(got);
  });
});

describe('refreshPanes tracks DeepSeek client process detection', () => {
  it('invalidates when hasDeepseek flips so the auto-approve badge can appear', async () => {
    const before = {
      id: 'pane_ds', cwd: '/x', cmd: ['bash'], title: 'shell',
      status: 'running', state: 'idle', hasClaude: false, hasDeepseek: false,
    } as PaneInfo;
    useStore.setState({ panes: { [before.id]: before }, layout: null, parkedPaneIds: [] });

    // The daemon's process scan finds `deepseek-terminal.mjs` on the child argv
    // a beat after the client spawns, so hasDeepseek is the ONLY changed field
    // on this poll. Without the samePaneInfo guard the stale DTO would survive
    // and the auto badge would never render.
    vi.spyOn(api, 'listPanes').mockResolvedValue([{ ...before, hasDeepseek: true }]);
    await useStore.getState().refreshPanes();
    const detected = useStore.getState().panes[before.id];
    expect(detected).not.toBe(before);
    expect(detected.hasDeepseek).toBe(true);

    // Repeating the same DTO keeps object identity (no render churn).
    vi.mocked(api.listPanes).mockResolvedValue([{ ...before, hasDeepseek: true }]);
    await useStore.getState().refreshPanes();
    expect(useStore.getState().panes[before.id]).toBe(detected);
  });
});

describe('refreshPanes applies discovered Codex conversation identity', () => {
  it('accepts agent, thread id, and resume readiness even when process state is unchanged', async () => {
    const before = {
      id: 'pane_codex', cwd: '/x', cmd: ['bash'], title: 'codex',
      status: 'running', createdAt: '2026-07-12T00:00:00.000Z',
      cols: 80, rows: 24, pid: 123, lastActivityMs: 100,
      state: 'idle', hasClaude: false, hasCodex: true,
    } as PaneInfo;
    useStore.setState({ layout: null, panes: { pane_codex: before }, parkedPaneIds: [] });

    vi.spyOn(api, 'listPanes').mockResolvedValue([{
      ...before,
      agent: 'codex',
      codexThreadId: 'thread-discovered',
      resumeReady: true,
    }]);

    await useStore.getState().refreshPanes();

    expect(useStore.getState().panes.pane_codex).toMatchObject({
      agent: 'codex',
      codexThreadId: 'thread-discovered',
      resumeReady: true,
    });

    vi.mocked(api.listPanes).mockResolvedValue([{
      ...before,
      agent: 'codex',
      identityBlocked: true,
    }]);
    await useStore.getState().refreshPanes();
    expect(useStore.getState().panes.pane_codex.identityBlocked).toBe(true);
    expect(useStore.getState().panes.pane_codex.codexThreadId).toBeUndefined();
  });
});

// The focus-steal guard is safe-by-default: the pane focus effects steal the
// keyboard caret only when lastUserFocusId === the pane (a DELIBERATE selection).
// setFocus (every click / keyboard-nav / palette / reveal / spawn) stamps it;
// every BACKGROUND focusedPaneId change (ARIA spawn, MCP open, diff/file/browser
// tile, workspace switch) must leave it untouched so it cannot yank the caret out
// of a composer the user is mid-typing in. These guard that invariant at the
// source — the regression that re-opens the steal is a background path stamping
// lastUserFocusId (or setFocus failing to).
describe('focus-steal guard: lastUserFocusId tracks only DELIBERATE selection', () => {
  const mkPane = (id: string): PaneInfo =>
    ({ id, cwd: '/x', cmd: ['claude'], kind: 'native', status: 'running', state: 'idle' } as PaneInfo);

  it('setFocus stamps lastUserFocusId (deliberate selection is allowed to grab the caret)', () => {
    useStore.setState({
      layout: split(pane('A'), pane('B')),
      panes: { A: mkPane('A'), B: mkPane('B') },
      focusedPaneId: 'A', lastUserFocusId: null, parkedPaneIds: [],
    });
    useStore.getState().setFocus('B');
    expect(useStore.getState().focusedPaneId).toBe('B');
    expect(useStore.getState().lastUserFocusId).toBe('B');
  });

  it('a background graft (addPaneToLayout / ARIA spawn) selects the pane WITHOUT stamping lastUserFocusId', () => {
    // User deliberately picked A and is mid-typing there.
    useStore.setState({
      layout: split(pane('A'), pane('B')),
      panes: { A: mkPane('A'), B: mkPane('B') },
      focusedPaneId: 'A', lastUserFocusId: 'A', zoomedPaneId: null, parkedPaneIds: [],
    });
    // ARIA grafts/re-selects pane B in the background.
    useStore.getState().addPaneToLayout('B');
    // Selection moves to B…
    expect(useStore.getState().focusedPaneId).toBe('B');
    // …but lastUserFocusId stays A, so B's focus effect (lastUserFocusId !== pane.id
    // && isTypingElsewhere) leaves the user's caret in A.
    expect(useStore.getState().lastUserFocusId).toBe('A');
  });

  it('consumeUserFocus is one-shot: the caret grab does not survive a later re-render/reconnect', () => {
    // User deliberately selected A; its focus effect grabbed the caret and then
    // consumed the marker.
    useStore.setState({
      layout: split(pane('A'), pane('B')),
      panes: { A: mkPane('A'), B: mkPane('B') },
      focusedPaneId: 'A', lastUserFocusId: 'A', parkedPaneIds: [],
    });
    useStore.getState().consumeUserFocus();
    // Marker cleared → a subsequent reconnect-driven re-render/remount of A finds
    // lastUserFocusId !== 'A', so it can only re-focus when the user isn't typing
    // elsewhere (the isTypingElsewhere gate) instead of re-asserting unconditionally.
    expect(useStore.getState().lastUserFocusId).toBeNull();
    // focusedPaneId is untouched — A is still the selected pane, just no longer a
    // pending caret-grab.
    expect(useStore.getState().focusedPaneId).toBe('A');
    // Idempotent: a second consume is a no-op.
    useStore.getState().consumeUserFocus();
    expect(useStore.getState().lastUserFocusId).toBeNull();
  });
});

describe('restartPane', () => {
  const paneInfo = (id: string): PaneInfo =>
    ({ id, cwd: '/x', cmd: ['claude'], status: 'running', state: 'idle' } as PaneInfo);

  it('merges the new-pid DTO so the pane reconnects', async () => {
    const before = paneInfo('pane-r');
    useStore.setState({ panes: { [before.id]: before }, layout: null, parkedPaneIds: [] });
    const next = { ...before, pid: 999 } as PaneInfo;
    const spy = vi.spyOn(api, 'restartPane').mockResolvedValue(next);
    const saved = await useStore.getState().restartPane(before.id);
    expect(spy).toHaveBeenCalledWith(before.id, false, false);
    expect(saved).toBe(next);
    expect(useStore.getState().panes[before.id].pid).toBe(999);
  });

  it('rethrows a needs-fresh refusal without touching the pane so the caller can confirm', async () => {
    const before = paneInfo('pane-r2');
    useStore.setState({ panes: { [before.id]: before }, layout: null, parkedPaneIds: [] });
    vi.spyOn(api, 'restartPane').mockRejectedValue(
      new ApiError(409, 'no durable conversation to resume', { needsFresh: true }),
    );
    await expect(useStore.getState().restartPane(before.id)).rejects.toThrow(/no durable conversation/);
    expect(useStore.getState().panes[before.id]).toBe(before);
  });

  it('sends fresh=true to start a new conversation', async () => {
    const before = paneInfo('pane-r3');
    useStore.setState({ panes: { [before.id]: before }, layout: null, parkedPaneIds: [] });
    const spy = vi.spyOn(api, 'restartPane').mockResolvedValue({ ...before, pid: 7 } as PaneInfo);
    await useStore.getState().restartPane(before.id, true);
    expect(spy).toHaveBeenCalledWith(before.id, true, false);
  });
});

describe('terminal owner reveal', () => {
  it('background owner reveal cannot graft or focus a legacy dashboard; binding remains explicit', () => {
    useStore.setState({
      panes: { owner_wft_test: { id: 'owner_wft_test', role: 'task_owner' } as PaneInfo },
      layout: split(pane('existing'), { type: 'tasks', tasksId: 'owner_wft_test', ownerTaskId: 'wft_test', selectedTaskId: 'wft_test', composing: false }),
      focusedPaneId: 'owner_wft_test', zoomedPaneId: null,
    });
    useStore.getState().addPaneToLayout('owner_wft_test');
    useStore.getState().addPaneToLayout('owner_wft_test');
    expect(useStore.getState().layout).toEqual(split(pane('existing'), { type: 'tasks', tasksId: 'owner_wft_test', ownerTaskId: 'wft_test', selectedTaskId: 'wft_test', composing: false }));
    expect(useStore.getState().focusedPaneId).toBe('owner_wft_test');
  });
});
