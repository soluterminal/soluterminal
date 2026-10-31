import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { currentRoutingScope } from '../api';
import { acquirePaneHost } from '../lib/paneHosts';
import { captureSessionGesture, clearTerminalSessions, ensureTerminalSession, sessionVisible } from '../lib/terminalViewSessions';
import { useStore } from '../store';
import type { PaneInfo } from '../types';
import { Pane } from './Pane';

vi.mock('./DelegatedWorkChip', () => ({ DelegatedWorkChip: () => null }));
vi.mock('../lib/signedStreams', () => ({
  openWebSocket: () => ({ readyState: WebSocket.CONNECTING, send: vi.fn(), close: vi.fn() }),
  privateRelaySocketsFor: () => [],
}));

const pane: PaneInfo = {
  id: 'visible-pane', pid: 1, cwd: '/project', cmd: ['/bin/bash'], title: 'Reopen me',
  status: 'running', state: 'choice-prompt', createdAt: '2026-10-31', cols: 80, rows: 24,
  lastActivityMs: 0, hasClaude: false,
};

function restored() {
  window.dispatchEvent(new CustomEvent('solu:desktop-restored', { detail: { reason: 'shown', hideTracked: true } }));
}

beforeEach(() => {
  vi.stubGlobal('matchMedia', () => ({ matches: false, addListener() {}, removeListener() {} }));
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue({
    measureText: () => ({ width: 8 }),
  } as unknown as CanvasRenderingContext2D);
  vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible');
  restored();
  useStore.setState({
    mainPresentation: { kind: 'workspace' }, navigationGeneration: 0,
    panes: { [pane.id]: pane }, layout: { type: 'pane', paneId: pane.id },
    focusedPaneId: pane.id, zoomedPaneId: null, parkedPaneIds: [],
  });
});

afterEach(() => {
  cleanup();
  restored();
  clearTerminalSessions();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function mountPane() {
  const session = ensureTerminalSession(currentRoutingScope(), pane);
  const host = acquirePaneHost(session.key);
  document.body.appendChild(host);
  render(<Pane pane={pane} paneIndex={1} session={session} />, { container: host });
  const send = vi.fn();
  act(() => useStore.setState({ senders: { [session.key]: send } }));
  return { session, send };
}

it('restores the header and approval strip when the document becomes visible after the native show', () => {
  const { session, send } = mountPane();
  expect(screen.getByText('Reopen me')).toBeInTheDocument();
  expect(screen.getByText('#1')).toBeInTheDocument();
  expect(screen.getByRole('group', { name: 'Approval actions' })).toBeInTheDocument();
  const gesture = captureSessionGesture(session);
  expect(gesture()).toBe(true);

  act(() => {
    window.dispatchEvent(new CustomEvent('solu:desktop-will-background'));
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
    document.dispatchEvent(new Event('visibilitychange'));
  });
  expect(sessionVisible(session)).toBe(false);
  // A poll while hidden can render the controls away on the old implementation.
  act(() => useStore.setState({ panes: { [pane.id]: { ...pane } } }));
  act(restored);
  expect(screen.queryByText('Reopen me')).not.toBeInTheDocument();
  expect(screen.queryByRole('group', { name: 'Approval actions' })).not.toBeInTheDocument();
  expect(gesture()).toBe(false);

  act(() => {
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible');
    document.dispatchEvent(new Event('visibilitychange'));
  });
  expect(screen.getByText('Reopen me')).toBeInTheDocument();
  expect(screen.getByText('#1')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: '1 — first choice' }));
  expect(send).toHaveBeenCalledWith('1');
  expect(gesture()).toBe(false);
});

it('tracks native hide and restore even when the WebView document stays visible', () => {
  const { session, send } = mountPane();
  const reply = screen.getByRole('button', { name: '1 — first choice' });
  act(() => window.dispatchEvent(new CustomEvent('solu:desktop-will-background')));
  expect(document.visibilityState).toBe('visible');
  expect(sessionVisible(session)).toBe(false);
  expect(screen.queryByText('Reopen me')).not.toBeInTheDocument();
  expect(screen.queryByRole('group', { name: 'Approval actions' })).not.toBeInTheDocument();
  fireEvent.click(reply);
  expect(send).not.toHaveBeenCalled();

  act(restored);
  expect(sessionVisible(session)).toBe(true);
  expect(screen.getByText('Reopen me')).toBeInTheDocument();
  expect(screen.getByText('#1')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: '1 — first choice' }));
  expect(send).toHaveBeenCalledWith('1');
});
