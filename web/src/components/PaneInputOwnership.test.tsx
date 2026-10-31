import { act, cleanup, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { currentRoutingScope } from '../api';
import { useStore, terminalSessionKey } from '../store';
import { CLAIM_ACK_TIMEOUT_MS, INPUT_HOLD_MS } from '../lib/paneInputOwnership';
import * as interaction from '../lib/terminalInteractionState';
import { lifecycleLog } from '../debug';
import { Pane } from './Pane';
import type { PaneInfo } from '../types';

// Exercise Pane's real socket/claim/input wiring and store with a fake
// renderer and socket, without a daemon or real PTY.
const terminals = vi.hoisted(() => [] as Array<{ emit: (data: string) => void }>);
vi.mock('@xterm/xterm', () => ({ Terminal: class {
  cols = 100;
  rows = 30;
  options = {};
  element = document.createElement('div');
  textarea = document.createElement('textarea');
  buffer = { active: { type: 'normal', viewportY: 0, baseY: 0, length: 30, getLine: () => undefined }, onBufferChange: () => ({ dispose() {} }) };
  parser = { registerOscHandler: () => ({ dispose() {} }) };
  emit = (_data: string) => {};
  constructor() { terminals.push(this); }
  open(host: HTMLElement) { host.append(this.element, this.textarea); }
  onData(fn: (data: string) => void) { this.emit = fn; return { dispose() {} }; }
  onWriteParsed() { return { dispose() {} }; }
  onScroll() { return { dispose() {} }; }
  onLineFeed() { return { dispose() {} }; }
  onRender() { return { dispose() {} }; }
  onSelectionChange() { return { dispose() {} }; }
  registerLinkProvider() { return { dispose() {} }; }
  loadAddon() {}
  attachCustomKeyEventHandler() {}
  hasSelection() { return false; }
  focus() {}
  refresh() {}
  scrollToBottom() {}
  dispose() {}
} }));
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class { fit() {} } }));
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: class {} }));
vi.mock('@xterm/addon-search', () => ({ SearchAddon: class { onDidChangeResults() { return { dispose() {} }; } } }));
vi.mock('./PaneHeader', () => ({ PaneHeader: () => null }));
vi.mock('../debug', () => ({ debugLog: vi.fn(), lifecycleLog: vi.fn() }));

class Socket {
  static OPEN = 1;
  static instances: Socket[] = [];
  readyState = 0;
  bufferedAmount = 0;
  onopen: (() => void) | null = null;
  onclose: ((event: { code: number; reason: string; wasClean: boolean }) => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  sent: Array<string | Uint8Array> = [];
  constructor() { Socket.instances.push(this); }
  send(data: string | Uint8Array) { this.sent.push(data); }
  open() { this.readyState = 1; this.onopen?.(); }
  ownership(owned: boolean, owner: string) { this.onmessage?.({ data: JSON.stringify({ type: 'ownership', owned, owner }) }); }
  close() { this.readyState = 3; this.onclose?.({ code: 4000, reason: 'relay moved', wasClean: false }); }
  claims() { return this.sent.filter((x): x is string => typeof x === 'string').map(x => JSON.parse(x)).filter(x => x.type === 'claim').map(x => x.reason); }
  input() { return this.sent.filter((x): x is Uint8Array => typeof x !== 'string').map(x => new TextDecoder().decode(x)); }
}

const pane = { id: 'p1', title: 'Test', cwd: '/tmp', cmd: ['bash'], status: 'running', pid: 31, cols: 100, rows: 30, createdAt: '', state: 'idle', lastActivityMs: 0 } as PaneInfo;
const key = terminalSessionKey(currentRoutingScope(), pane.id);
let prior: ReturnType<typeof useStore.getState>;
const advance = (ms: number) => act(() => vi.advanceTimersByTime(ms));
const type = (data: string) => act(() => terminals[0].emit(data));
function mount() {
  render(<Pane pane={pane} paneIndex={1} />);
  const socket = Socket.instances[0];
  act(() => socket.open());
  return socket;
}
function reconnect(socket: Socket) {
  act(() => socket.close());
  advance(1500);
  const next = Socket.instances.at(-1)!;
  expect(next).not.toBe(socket);
  act(() => next.open());
  return next;
}

beforeEach(() => {
  prior = useStore.getState();
  vi.useFakeTimers();
  vi.stubGlobal('WebSocket', Socket);
  vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible');
  Object.defineProperty(navigator, 'webdriver', { configurable: true, get: () => false });
  vi.spyOn(console, 'warn').mockImplementation(() => {});
  Socket.instances = [];
  terminals.length = 0;
  useStore.setState({ panes: { p1: pane }, layout: { type: 'pane', paneId: 'p1' }, focusedPaneId: 'p1', mainPresentation: { kind: 'workspace' }, claimIntent: false, paneOwnership: {}, paneOwnerState: {}, paneOwnershipTokens: {} });
});
afterEach(() => {
  cleanup();
  useStore.setState(prior, true);
  vi.clearAllTimers();
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});

describe('Pane input ownership wiring', () => {
  it('preserves Take over intent after a slow reconnect claim and local hold expiry; only a real peer revokes it', () => {
    const first = mount();
    act(() => first.ownership(true, 'you'));
    act(() => useStore.getState().claimAllPanes());
    expect(useStore.getState().claimIntent).toBe(true);
    const next = reconnect(first);
    act(() => next.ownership(false, 'none'));
    type('slow');
    advance(INPUT_HOLD_MS);
    expect(useStore.getState().paneOwnership[key]).toBe(false);
    expect(useStore.getState().claimIntent).toBe(true);
    expect(next.input()).not.toContain('slow');
    act(() => next.ownership(false, 'other'));
    expect(useStore.getState().claimIntent).toBe(false);
  });

  it('ignores the initial ghost owner before touching pending claims, typing or retries', () => {
    const socket = mount();
    type('before');
    act(() => socket.ownership(false, 'other'));
    type('after');
    advance(CLAIM_ACK_TIMEOUT_MS - 1);
    expect(useStore.getState().paneOwnership[key]).toBeUndefined();
    act(() => socket.ownership(true, 'you'));
    expect(socket.input()).toEqual(['before', 'after']);
    expect(lifecycleLog).not.toHaveBeenCalledWith('pane.input.dropped', expect.anything());
  });

  it('keeps retrying an unanswered claim after the initial ghost frame', () => {
    const socket = mount();
    act(() => socket.ownership(false, 'other'));
    advance(CLAIM_ACK_TIMEOUT_MS);
    expect(socket.claims()).toEqual(['first-open', 'first-open']);
    act(() => socket.ownership(false, 'other'));
    expect(useStore.getState().paneOwnership[key]).toBe(false);
    advance(CLAIM_ACK_TIMEOUT_MS * 3);
    expect(socket.claims()).toHaveLength(2);
  });

  it('still recovers a deferred claim on an initial owner:none frame after becoming visible', () => {
    const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
    const socket = mount();
    expect(socket.claims()).toEqual([]);
    // A foreground edge can precede the visibility event. The initial
    // unowned frame must still reach the existing recovery branch.
    visibility.mockReturnValue('visible');
    act(() => socket.ownership(false, 'none'));
    expect(socket.claims()).toEqual(['adopt-unowned']);
    type('visible');
    act(() => socket.ownership(true, 'you'));
    expect(socket.input()).toContain('visible');
  });

  it('delivers typing for a 2.5 second reconnect grant without locking the pane', () => {
    const socket = mount();
    act(() => socket.ownership(true, 'you'));
    const next = reconnect(socket);
    act(() => next.ownership(false, 'none'));
    type('delayed');
    advance(2500);
    expect(useStore.getState().paneOwnership[key]).toBe(true);
    act(() => next.ownership(true, 'you'));
    expect(next.input()).toContain('delayed');
    expect(lifecycleLog).toHaveBeenCalledWith('pane.input.reclaimed', expect.objectContaining({ bytes: 7 }));
  });

  it('does not report held input as reclaimed when the terminal gate refuses it', () => {
    const socket = mount();
    type('blocked');
    const hold = vi.spyOn(interaction, 'holdTerminalInput');
    vi.spyOn(interaction, 'terminalInputAllowed').mockReturnValue(false);
    const cancel = vi.spyOn(interaction, 'cancelTerminalRetirement');
    act(() => socket.ownership(true, 'you'));
    expect(socket.input()).toEqual([]);
    expect(cancel).toHaveBeenCalledWith(key);
    expect(hold).not.toHaveBeenCalled();
    expect(lifecycleLog).not.toHaveBeenCalledWith('pane.input.reclaimed', expect.anything());
    expect(lifecycleLog).toHaveBeenCalledWith('pane.input.dropped', expect.objectContaining({ reason: 'delivery-refused', bytes: 7 }));
  });

  it('does not partially queue held chunks into a retirement fence on grant', () => {
    const socket = mount();
    type('first');
    type('second');
    const cancel = vi.fn();
    const flush = vi.fn(() => true);
    act(() => {
      expect(interaction.beginTerminalRetirement(key, {
        token: 'test-retirement', leaseMs: 5000, valid: () => true,
        pending: () => false, cancel, flush,
      })).toBe(true);
    });
    act(() => socket.ownership(true, 'you'));
    expect(socket.input()).toEqual([]);
    expect(cancel).toHaveBeenCalledTimes(1);
    expect(lifecycleLog).toHaveBeenCalledWith('pane.input.dropped', expect.objectContaining({ reason: 'delivery-refused', bytes: 11 }));
    act(() => interaction.releaseTerminalRetirement(key, 'test-retirement'));
    expect(flush).not.toHaveBeenCalled();
    expect(lifecycleLog).not.toHaveBeenCalledWith('pane.input.reclaimed', expect.anything());
  });

  it('never replays terminal CPR/DA responses made during a claim', () => {
    const socket = mount();
    type('\x1b[12;40R');
    type('typing');
    type('\x1b[?1;2c');
    act(() => socket.ownership(true, 'you'));
    expect(socket.input()).toEqual(['typing']);
  });
});
