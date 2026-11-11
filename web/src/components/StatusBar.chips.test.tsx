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

describe('StatusBar chips', () => {
  beforeEach(() => {
    useEntitlements.setState({ loaded: true, team: true, operator: true });
    useStore.setState({ panes: {}, focusedPaneId: null, layout: null, ports: [], updateStatus: null, deferredUpdate: null });
    healthRef.current = { version: '1.2.3' } as Health;
  });

  afterEach(() => {
    presentation.on = false;
    selectedBackend.current = true;
    useCloud.setState({ mode: 'detecting', isLocal: false, machines: [], selectedMachineId: null });
    useEntitlements.setState({ loaded: false, team: false, operator: false });
  });

  it('shows no "local" chip on a direct connection', () => {
    useCloud.setState({ mode: 'cloud', isLocal: true });
    render(<StatusBar />);
    expect(screen.queryByText('local')).toBeNull();
    expect(document.querySelector('.statusbar-conn')).toBeNull();
  });

  it('keeps the relayed machine chip', () => {
    const machine = { id: 'm1', name: 'desk-pc', online: true } as CloudMachine;
    useCloud.setState({ mode: 'cloud', isLocal: false, machines: [machine], selectedMachineId: 'm1' });
    render(<StatusBar />);
    expect(document.querySelector('.statusbar-conn.is-cloud')).toHaveTextContent('desk-pc');
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
