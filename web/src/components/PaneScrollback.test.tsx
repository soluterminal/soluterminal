import { act, cleanup, render } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { currentRoutingScope } from '../api';
import { acquirePaneHost, isPaneHostParked } from '../lib/paneHosts';
import { clearTerminalSessions, ensureTerminalSession } from '../lib/terminalViewSessions';
import { useStore } from '../store';
import type { Layout, PaneInfo } from '../types';
import { Pane } from './Pane';
import { PaneSlot } from './PaneSlot';

// The real xterm, with each created terminal captured so a test can read the
// scrollback limit the Pane set on it.
const terminals = vi.hoisted(() => [] as Array<{ options: { scrollback?: number } }>);
vi.mock('@xterm/xterm', async (importOriginal) => {
  const real = await importOriginal<typeof import('@xterm/xterm')>();
  class CapturedTerminal extends real.Terminal {
    constructor(options?: ConstructorParameters<typeof real.Terminal>[0]) {
      super(options);
      terminals.push(this);
    }
  }
  return { ...real, Terminal: CapturedTerminal };
});
vi.mock('./DelegatedWorkChip', () => ({ DelegatedWorkChip: () => null }));
vi.mock('../lib/signedStreams', () => ({
  openWebSocket: () => ({ readyState: WebSocket.CONNECTING, send: vi.fn(), close: vi.fn() }),
  privateRelaySocketsFor: () => [],
}));

function paneInfo(id: string): PaneInfo {
  return {
    id, pid: 1, cwd: '/project', cmd: ['/bin/bash'], title: id,
    status: 'running', state: 'idle', createdAt: '2026-11-12', cols: 80, rows: 24,
    lastActivityMs: 0, hasClaude: false,
  };
}

/** A workspace layout holding `count` panes. */
function workspaceOf(count: number, first = 'p'): Layout {
  let layout: Layout = { type: 'pane', paneId: `${first}-0` };
  for (let i = 1; i < count; i++) {
    layout = { type: 'split', direction: 'horizontal', ratio: 0.5, a: layout, b: { type: 'pane', paneId: `${first}-${i}` } } as Layout;
  }
  return layout;
}

function showWorkspace(layout: Layout) {
  act(() => useStore.setState({ layout }));
}

/** Mounts a Pane the way WarmPaneLayer does: into its persistent host, which
 *  starts in the hidden parking lot. `onScreen` gives it a real PaneSlot. */
function mountPane(id: string, onScreen: boolean) {
  const pane = paneInfo(id);
  act(() => useStore.setState((s) => ({ panes: { ...s.panes, [id]: pane } })));
  const session = ensureTerminalSession(currentRoutingScope(), pane);
  const host = acquirePaneHost(session.key);
  let slot: ReturnType<typeof render> | null = null;
  /** The grid shows this pane: a slot adopts its host (fires the reattach). */
  const show = () => { slot = render(<PaneSlot paneId={id} sessionKey={session.key} />); };
  /** The grid stops showing it: the slot unmounts and parks the host. */
  const park = () => { slot?.unmount(); slot = null; };
  if (onScreen) show();
  const before = terminals.length;
  render(<Pane pane={pane} paneIndex={1} session={session} />, { container: host });
  expect(terminals.length).toBe(before + 1);
  expect(isPaneHostParked(session.key)).toBe(!onScreen);
  return { term: terminals[before], session, show, park };
}

beforeEach(() => {
  terminals.length = 0;
  vi.stubGlobal('matchMedia', () => ({ matches: false, addListener() {}, removeListener() {} }));
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue({
    measureText: () => ({ width: 8 }),
  } as unknown as CanvasRenderingContext2D);
  vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible');
  useStore.setState({
    mainPresentation: { kind: 'workspace' }, navigationGeneration: 0,
    panes: {}, layout: workspaceOf(3), focusedPaneId: null, zoomedPaneId: null, parkedPaneIds: [],
  });
});

afterEach(() => {
  cleanup();
  clearTerminalSessions();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

it('keeps the scrollback of a pane parked from another workspace when a large workspace is shown', () => {
  // Both terminals start in a 3-pane workspace: the full 20000 lines.
  const parked = mountPane('parked-pane', false);
  const visible = mountPane('visible-pane', true);
  expect(parked.term.options.scrollback).toBe(20000);
  expect(visible.term.options.scrollback).toBe(20000);

  // Visiting a workspace with more than 16 panes.
  showWorkspace(workspaceOf(17, 'big'));
  // Control: a pane on screen still follows the grid it is shown in.
  expect(visible.term.options.scrollback).toBe(5000);
  // The parked pane is not in that grid and keeps its history.
  expect(parked.term.options.scrollback).toBe(20000);

  showWorkspace(workspaceOf(6, 'mid'));
  expect(visible.term.options.scrollback).toBe(12000);
  expect(parked.term.options.scrollback).toBe(20000);
});

it('leaves a parked pane alone in both directions, and sizes it for its grid when it comes back on screen', () => {
  showWorkspace(workspaceOf(17, 'big'));
  const pane = mountPane('worker-pane', false);
  expect(pane.term.options.scrollback).toBe(5000);

  // Another, small workspace on screen must not resize it either.
  showWorkspace(workspaceOf(2, 'small'));
  expect(pane.term.options.scrollback).toBe(5000);

  // Shown in that grid with no further change to the pane count: it takes
  // the limit of the grid it is now in.
  pane.show();
  expect(isPaneHostParked(pane.session.key)).toBe(false);
  expect(pane.term.options.scrollback).toBe(20000);

  // Hidden again (zoom on a sibling, a workspace switch) while the grid grows.
  pane.park();
  expect(isPaneHostParked(pane.session.key)).toBe(true);
  showWorkspace(workspaceOf(17, 'grown'));
  expect(pane.term.options.scrollback).toBe(20000);

  // Back on screen in the grown grid, again with the count unchanged: the
  // memory limit of a large grid applies to it like to its neighbours.
  pane.show();
  expect(pane.term.options.scrollback).toBe(5000);
});
