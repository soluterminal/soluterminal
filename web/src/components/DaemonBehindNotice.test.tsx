import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Health } from '../types';
import { DaemonBehindNotice, daemonBehind } from './DaemonBehindNotice';

// ST-315: say plainly when this window is newer than the daemon it talks to.

const healthRef = vi.hoisted(() => ({ current: null as Health | null, scopeKey: '|' as string | null, bundleKind: 'ok' }));
const storeRef = vi.hoisted(() => ({ autoApply: 'when-idle' as string | undefined, required: false, available: true, latest: '0.0.1247' as string | undefined }));
vi.mock('../lib/useDaemonHealth', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../lib/useDaemonHealth')>()),
  useDaemonHealth: () => ({ health: healthRef.current, healthScopeKey: healthRef.scopeKey, lastOkMs: Date.now(), polling: false, bundleState: { kind: healthRef.bundleKind } }),
}));
vi.mock('../api', () => ({ currentRoutingScope: () => ({ prefix: '', backendPrefix: '', key: '|' }) }));
vi.mock('../store', () => ({
  useStore: (sel: (s: unknown) => unknown) => sel({
    updateSettings: { autoApply: storeRef.autoApply },
    updateStatus: { required: storeRef.required, available: storeRef.available, latest: storeRef.latest },
  }),
}));

const h = (version: string) => ({ status: 'ok', version, uptimeSec: 1, panes: 0 }) as Health;

beforeEach(() => { try { localStorage.clear(); } catch { /* */ } });
afterEach(() => {
  healthRef.current = null; healthRef.scopeKey = '|'; healthRef.bundleKind = 'ok';
  storeRef.autoApply = 'when-idle'; storeRef.required = false; storeRef.available = true; storeRef.latest = '0.0.1247';
});

describe('daemonBehind', () => {
  it('is the daemon version only when the window is newer', () => {
    expect(daemonBehind('0.0.1246', '0.0.1247')).toBe('0.0.1246');
    expect(daemonBehind('0.0.1247', '0.0.1247')).toBeNull();
    expect(daemonBehind('0.0.1248', '0.0.1247')).toBeNull();
    expect(daemonBehind('dev', '0.0.1247')).toBeNull();
    expect(daemonBehind('0.0.1246', 'dev')).toBeNull();
    expect(daemonBehind(undefined, '0.0.1247')).toBeNull();
  });
});

describe('DaemonBehindNotice', () => {
  it('names both versions and that the update applies when idle', () => {
    healthRef.current = h('0.0.1246');
    render(<DaemonBehindNotice uiVersion="0.0.1247" />);
    const note = screen.getByRole('status');
    expect(note).toHaveTextContent('The background service update is still pending');
    expect(note).toHaveTextContent('this window is 0.0.1247, the service is 0.0.1246');
    expect(note).toHaveTextContent('Some newer actions are hidden until the service catches up');
    expect(note).toHaveTextContent('it updates itself the next time your agents are idle');
  });

  it('points at Update now when automatic updates are off', () => {
    healthRef.current = h('0.0.1246');
    storeRef.autoApply = 'off';
    render(<DaemonBehindNotice uiVersion="0.0.1247" />);
    expect(screen.getByRole('status')).toHaveTextContent('choose Update now when SoluTerminal offers the update');
  });

  it('promises no update the daemon has not seen', () => {
    healthRef.current = h('0.0.1246');
    storeRef.available = false;
    storeRef.latest = '0.0.1246';
    render(<DaemonBehindNotice uiVersion="0.0.1247" />);
    const note = screen.getByRole('status');
    expect(note).toHaveTextContent('The background service is older than this window');
    expect(note).toHaveTextContent("it hasn't seen the update yet and picks it up on its next update check");
  });

  it('stays out of the way for another scope and while a version banner shows', () => {
    healthRef.current = h('0.0.1246');
    healthRef.scopeKey = 'b|win';
    const { unmount } = render(<DaemonBehindNotice uiVersion="0.0.1247" />);
    expect(screen.queryByRole('status')).toBeNull();
    unmount();
    healthRef.scopeKey = '|';
    healthRef.bundleKind = 'unknown';
    render(<DaemonBehindNotice uiVersion="0.0.1247" />);
    expect(screen.queryByRole('status')).toBeNull();
  });

  it('stays quiet when the versions match or the daemon is newer, and once dismissed', () => {
    healthRef.current = h('0.0.1247');
    const { unmount } = render(<DaemonBehindNotice uiVersion="0.0.1247" />);
    expect(screen.queryByRole('status')).toBeNull();
    unmount();
    healthRef.current = h('0.0.1246');
    const second = render(<DaemonBehindNotice uiVersion="0.0.1247" />);
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByRole('status')).toBeNull();
    second.unmount();
    render(<DaemonBehindNotice uiVersion="0.0.1247" />);
    expect(screen.queryByRole('status')).toBeNull();
  });
});
