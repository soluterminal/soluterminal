import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Health } from '../types';
import type { CloudMachine } from '../cloud';
import { useCloud } from '../cloud';
import { useEntitlements } from '../entitlements';
import { useStore } from '../store';
import { StatusBar } from './StatusBar';

const selectedBackend = vi.hoisted(() => ({ current: true }));
vi.mock('../api', () => ({
  backgroundServiceIsSelectedBackend: () => selectedBackend.current,
  onRoutingScopeChange: vi.fn(() => () => {}),
  currentRoutingScope: vi.fn(() => ({ prefix: '', backendPrefix: '', key: '|' })),
  currentPrefix: vi.fn(() => ''),
  api: { health: vi.fn().mockResolvedValue({ version: '1.2.3' }) },
}));

const healthRef = vi.hoisted(() => ({ current: null as Health | null }));
vi.mock('../lib/useDaemonHealth', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../lib/useDaemonHealth')>()),
  useDaemonHealth: () => ({ health: healthRef.current, healthScopeKey: null, lastOkMs: Date.now(), polling: false, bundleState: null }),
}));

const presentation = vi.hoisted(() => ({ on: false }));
vi.mock('../lib/hideIdentities', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../lib/hideIdentities')>()),
  usePresentationMode: () => presentation.on,
}));

const cpuHealth = {
  version: '1.2.3',
  system: {
    level: 'warn',
    reason: 'Background service CPU: 4.2 cores',
    reasons: [{ key: 'daemonCpu', level: 'warn', text: 'Background service CPU: 4.2 cores', detail: 'SoluTerminal itself, not your panes; of 8 cores on this machine; warns when it holds 2.0 or more for about 30 s' }],
  },
} as Health;

type W = { __SOLU_BACKEND__?: unknown; __SOLU_TAURI__?: boolean };
// What the desktop shell injects on its window (desktop/src-tauri lib.rs).
function shell(backend: { requested: string; actual: string; fallback: boolean; launchMode?: 'local' | 'cloud' }) {
  (window as unknown as W).__SOLU_BACKEND__ = backend;
  (window as unknown as W).__SOLU_TAURI__ = true;
}

describe('StatusBar chips', () => {
  beforeEach(() => {
    useEntitlements.setState({ loaded: true, team: true, operator: true });
    useStore.setState({ panes: {}, focusedPaneId: null, layout: null, ports: [], updateStatus: null, deferredUpdate: null });
    healthRef.current = { version: '1.2.3' } as Health;
  });

  afterEach(() => {
    presentation.on = false;
    selectedBackend.current = true;
    delete (window as unknown as W).__SOLU_BACKEND__;
    delete (window as unknown as W).__SOLU_TAURI__;
    useCloud.setState({ mode: 'detecting', isLocal: false, machines: [], selectedMachineId: null });
    useEntitlements.setState({ loaded: false, team: false, operator: false });
  });

  // The top-left pill already says Local, or Cloud · <machine> with its OS.
  it('does not repeat the pill: no machine chip and no backend label when relayed', () => {
    const machine = { id: 'm1', name: 'desk-pc', online: true } as CloudMachine;
    useCloud.setState({ mode: 'cloud', isLocal: false, machines: [machine], selectedMachineId: 'm1' });
    // A Cloud-mode window: the shell's `actual` is a placeholder, and its
    // fallback flag says nothing about the remote computer.
    shell({ requested: 'wsl', actual: 'host', fallback: true, launchMode: 'cloud' });
    healthRef.current = { version: '1.2.3', backendID: 'wsl', shellPlatform: 'linux-amd64' } as Health;
    render(<StatusBar />);
    expect(screen.queryByText('desk-pc')).toBeNull();
    expect(screen.queryByText('local')).toBeNull();
    expect(screen.queryByText('WSL (Linux)')).toBeNull();
    expect(screen.queryByText('Windows (PowerShell)')).toBeNull();
    expect(document.querySelector('.statusbar-conn')).toBeNull();
    expect(document.querySelector('.statusbar-backend')).toBeNull();
  });

  it('shows no backend label on a healthy local desktop, signed in or not', () => {
    shell({ requested: 'wsl', actual: 'wsl', fallback: false, launchMode: 'local' });
    healthRef.current = { version: '1.2.3', backendID: 'wsl', shellPlatform: 'linux-amd64' } as Health;
    useCloud.setState({ mode: 'cloud', isLocal: true });
    const view = render(<StatusBar />);
    expect(screen.queryByText('WSL (Linux)')).toBeNull();
    expect(document.querySelector('.statusbar-backend')).toBeNull();
    view.unmount();

    useCloud.setState({ mode: 'local', isLocal: false });
    render(<StatusBar />);
    expect(document.querySelector('.statusbar-backend')).toBeNull();
  });

  it('keeps the WSL→Windows fallback warning on a local desktop, with a signed-in account too', async () => {
    shell({ requested: 'wsl', actual: 'host', fallback: true, launchMode: 'local' });
    useCloud.setState({ mode: 'cloud', isLocal: true });
    render(<StatusBar />);
    const chip = screen.getByText('Windows (PowerShell)');
    expect(chip).toHaveClass('statusbar-backend');
    fireEvent.focus(chip);
    expect(await screen.findByRole('tooltip')).toHaveTextContent('WSL was requested but could not start');
  });

  it('on a shell that does not say how it was launched, waits for the cloud session before warning', () => {
    shell({ requested: 'wsl', actual: 'host', fallback: true });
    useCloud.setState({ mode: 'detecting', isLocal: false });
    const view = render(<StatusBar />);
    expect(document.querySelector('.statusbar-backend')).toBeNull();
    view.unmount();

    useCloud.setState({ mode: 'cloud', isLocal: false });
    const relayed = render(<StatusBar />);
    expect(document.querySelector('.statusbar-backend')).toBeNull();
    relayed.unmount();

    useCloud.setState({ mode: 'local', isLocal: false });
    render(<StatusBar />);
    expect(document.querySelector('.statusbar-backend')).not.toBeNull();
  });

  it('keeps the version chip and Feedback in the end group, outside the chips that clip', () => {
    const { container } = render(<StatusBar />);
    const right = container.querySelector('.statusbar-right')!;
    const end = container.querySelector('.statusbar-end')!;
    // The end group is the bar's last child, straight after the chip group.
    expect(right.nextElementSibling).toBe(end);
    expect(end.nextElementSibling).toBeNull();

    const version = container.querySelector('.statusbar-version')!;
    expect(version).toHaveTextContent('v1.2.3');
    expect(end.contains(version)).toBe(true);
    expect(right.contains(version)).toBe(false);
    expect(end.contains(screen.getByRole('button', { name: 'Feedback' }))).toBe(true);
    // The chips that may clip stay in the shrinking group.
    expect(right.querySelector('.statusbar-panes')).not.toBeNull();
    expect(end.querySelector('.statusbar-panes')).toBeNull();
  });

  it('keeps an available update in the end group', () => {
    useStore.setState({ updateStatus: { available: true, latest: '9.9.9', current: '1.2.3' } as never });
    const { container } = render(<StatusBar />);
    const chip = container.querySelector('.statusbar-version-has-update')!;
    expect(chip).toHaveTextContent('Update v9.9.9');
    expect(chip.closest('.statusbar-end')).not.toBeNull();
    expect(chip.closest('.statusbar-right')).toBeNull();
  });

  describe('refused pairing ("cloud: credentials invalid")', () => {
    const refused = {
      version: '1.2.3',
      system: {
        level: 'critical',
        reason: 'cloud: credentials invalid',
        reasons: [{ key: 'cloud', level: 'critical', text: 'cloud: credentials invalid' }],
      },
    } as Health;
    const devicesFix = 'This computer’s pairing is no longer accepted. Open Settings › Devices › Phone access, choose Sign in, then Pair again.';
    const signedOutFix = 'This computer’s pairing is no longer accepted. First choose Create free account or sign in under Settings › Account, then open Settings › Devices › Phone access, choose Sign in, then Pair again.';
    const browserFix = 'This computer’s pairing is no longer accepted. In the SoluTerminal app on this computer, open Settings › Devices › Phone access and choose Pair again.';

    it('tells operators how to fix it, naming the Devices row in the desktop app', async () => {
      (window as unknown as W).__SOLU_TAURI__ = true;
      healthRef.current = refused;
      render(<StatusBar />);
      const chip = screen.getByRole('button', { name: /cloud: credentials invalid/ });
      expect(chip).toHaveClass('critical');
      // The chip keeps the short reason; the fix rides in the tooltip and the
      // accessible name.
      expect(chip).toHaveTextContent('cloud: credentials invalid');
      expect(chip.getAttribute('aria-label')).toBe(`cloud: credentials invalid · ${devicesFix}`);
      fireEvent.focus(chip);
      expect(await screen.findByRole('tooltip')).toHaveTextContent(devicesFix);
    });

    it('in a browser tab, sends the person to the app on this computer', () => {
      healthRef.current = refused;
      render(<StatusBar />);
      const chip = screen.getByRole('button', { name: /cloud: credentials invalid/ });
      expect(chip.getAttribute('aria-label')).toBe(`cloud: credentials invalid · ${browserFix}`);
    });

    it('tells a public install the fix in plain words', () => {
      (window as unknown as W).__SOLU_TAURI__ = true;
      useEntitlements.setState({ loaded: true, team: false, operator: false, account: true });
      healthRef.current = refused;
      render(<StatusBar />);
      const chip = screen.getByRole('button', { name: /Cloud connection problem/ });
      expect(chip).toHaveTextContent('Cloud connection problem');
      expect(chip.getAttribute('aria-label')).toBe(`Cloud connection problem. ${devicesFix}`);
    });

    it('in the desktop app with no account, says to sign in first: the Phone access row is hidden until then', () => {
      (window as unknown as W).__SOLU_TAURI__ = true;
      useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
      healthRef.current = refused;
      render(<StatusBar />);
      const chip = screen.getByRole('button', { name: /Cloud connection problem/ });
      expect(chip.getAttribute('aria-label')).toBe(`Cloud connection problem. ${signedOutFix}`);
    });

    it('does not tell anyone to sign in while the account verdict is still loading', () => {
      (window as unknown as W).__SOLU_TAURI__ = true;
      useEntitlements.setState({ loaded: false, team: false, operator: false, account: false });
      healthRef.current = refused;
      render(<StatusBar />);
      const chip = screen.getByRole('button', { name: /Cloud connection problem/ });
      expect(chip.getAttribute('aria-label')).toBe(`Cloud connection problem. ${devicesFix}`);
    });

    it('tells operators the fix on a daemon that sends only the headline reason', () => {
      (window as unknown as W).__SOLU_TAURI__ = true;
      healthRef.current = { version: '1.2.3', system: { level: 'critical', reason: 'cloud: credentials invalid' } } as Health;
      render(<StatusBar />);
      const chip = screen.getByRole('button', { name: /cloud: credentials invalid/ });
      expect(chip.getAttribute('aria-label')).toBe(`cloud: credentials invalid · ${devicesFix}`);
    });

    it('adds no pairing fix when another area is the critical headline', () => {
      (window as unknown as W).__SOLU_TAURI__ = true;
      healthRef.current = { version: '1.2.3', system: { level: 'critical', reason: 'disk: 0.4 GB free' } } as Health;
      render(<StatusBar />);
      const chip = screen.getByRole('button', { name: /disk: 0\.4 GB free/ });
      expect(chip.getAttribute('aria-label')).not.toMatch(/pairing/);
    });

    it('adds no pairing fix to a relay that is only offline', () => {
      (window as unknown as W).__SOLU_TAURI__ = true;
      healthRef.current = {
        version: '1.2.3',
        system: { level: 'warn', reason: 'cloud: relay offline', reasons: [{ key: 'cloud', level: 'warn', text: 'cloud: relay offline' }] },
      } as Health;
      render(<StatusBar />);
      const chip = screen.getByRole('button', { name: /cloud: relay offline/ });
      expect(chip.getAttribute('aria-label')).not.toMatch(/pairing/);
    });
  });

  it('shows the service CPU in cores for operators; the tooltip says it is not the panes and names the one fix', async () => {
    healthRef.current = cpuHealth;
    render(<StatusBar />);
    const chip = screen.getByRole('button', { name: /Background service CPU: 4\.2 cores/ });
    expect(chip).toHaveClass('warn');
    fireEvent.focus(chip);
    const tip = await screen.findByRole('tooltip');
    expect(tip).toHaveTextContent('not your panes; of 8 cores on this machine');
    expect(tip).toHaveTextContent('click to open System and restart the background service');
  });

  it('promises no restart where the restart would reach a different service', async () => {
    selectedBackend.current = false;
    healthRef.current = cpuHealth;
    render(<StatusBar />);
    const chip = screen.getByRole('button', { name: /Background service CPU: 4\.2 cores/ });
    expect(chip.getAttribute('aria-label')).toMatch(/ · click to open System$/);
  });

  it('names no restart on a system warning that is not the service CPU', async () => {
    healthRef.current = {
      version: '1.2.3',
      system: { level: 'warn', reason: 'disk: 3.1 GB free', reasons: [{ key: 'disk', level: 'warn', text: 'disk: 3.1 GB free' }] },
    } as Health;
    render(<StatusBar />);
    const chip = screen.getByRole('button', { name: /disk: 3\.1 GB free/ });
    expect(chip.getAttribute('aria-label')).not.toMatch(/restart/);
  });

  it('falls back to the plain System chip in presentation mode', () => {
    presentation.on = true;
    healthRef.current = cpuHealth;
    render(<StatusBar />);
    expect(screen.queryByText(/Background service CPU/)).toBeNull();
    const chip = screen.getByRole('button', { name: /Computer and app health/ });
    expect(chip).toHaveTextContent('System');
    expect(chip).toHaveClass('ok');
  });
});
