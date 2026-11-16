import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import type { Session } from '@supabase/supabase-js';
import { useCloud, type CloudMachine } from '../cloud';
import { CloudGate } from './CloudGate';
import { REFUSED_TEXT, STRICTEST_WINS_TEXT, WOULD_REFUSE_TEXT } from '../lib/machinePolicy';

const WINDOWS_UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130 Safari/537.36';

function seed(machines: CloudMachine[]) {
  useCloud.setState({
    mode: 'cloud',
    isLocal: false,
    session: { access_token: 'token', user: { email: 'me@example.com' } } as Session,
    authorised: true,
    machines,
    machinesLoaded: true,
    selectedMachineId: null,
    error: null,
    bootstrap: vi.fn(async () => {}),
    signOut: vi.fn(async () => {}),
    refreshMachines: vi.fn(async () => {}),
  });
}

const windowsPC: CloudMachine = {
  id: 'm_win',
  name: 'DESK',
  online: true,
  backends: [''],
  accountRequiresApproval: true,
  devicePolicy: [{ backend: '', policy: 'approved', platform: 'windows', connected: true }],
};

const strayWSL = (over: Partial<CloudMachine> = {}): CloudMachine => ({
  id: 'm_wsl',
  name: 'desk',
  online: true,
  backends: [''],
  accountRequiresApproval: true,
  devicePolicy: [{ backend: '', policy: 'unknown', connected: true, refused: true, wouldRefuse: true }],
  ...over,
});

const renderGate = () => render(<CloudGate><div>app</div></CloudGate>);

describe('Your machines: account-wide device policy', () => {
  beforeEach(() => {
    useCloud.setState(useCloud.getInitialState(), true);
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(WINDOWS_UA);
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      if (String(input).includes('/api/entitlements')) {
        return new Response(JSON.stringify({ tier: 'account', team: false }), { status: 200 });
      }
      return new Response(null, { status: 204 });
    });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    useCloud.setState(useCloud.getInitialState(), true);
  });

  it('shows platform, policy, the block, the duplicate name and the strictest-wins note', async () => {
    seed([windowsPC, strayWSL()]);
    renderGate();
    expect(await screen.findByText(/Windows · Only approved devices/)).toBeTruthy();
    expect(screen.getByText(/Policy unknown \(older version\)/)).toBeTruthy();
    expect(screen.getByText(REFUSED_TEXT)).toBeTruthy();
    expect(screen.getByText(/Two entries for “DESK”/)).toBeTruthy();
    expect(screen.getByText(STRICTEST_WINS_TEXT)).toBeTruthy();
    // A blocked machine can't be opened.
    const blocked = screen.getAllByRole('button').find((b) => b.className === 'cloud-gate-machine' && b.textContent?.includes('desk'));
    expect(blocked?.hasAttribute('disabled')).toBe(true);
    const reason = document.getElementById(blocked?.getAttribute('aria-describedby') ?? '');
    expect(reason?.textContent).toContain('Blocked:');
  });

  it('warns before enforcement without blocking', async () => {
    seed([windowsPC, strayWSL({ devicePolicy: [{ backend: '', policy: 'any', platform: 'linux-wsl', connected: true, wouldRefuse: true }] })]);
    renderGate();
    expect(await screen.findByText(WOULD_REFUSE_TEXT)).toBeTruthy();
    expect(screen.getByText(/Linux \(WSL\) · Any device/)).toBeTruthy();
    const row = screen.getAllByRole('button').find((b) => b.className === 'cloud-gate-machine' && b.textContent?.includes('desk'));
    expect(row?.hasAttribute('disabled')).toBe(false);
  });

  it('labels an offline daemon\'s policy as last reported', async () => {
    seed([windowsPC, strayWSL({ online: false, devicePolicy: [{ backend: '', policy: 'any', connected: false, wouldRefuse: true }] })]);
    renderGate();
    expect(await screen.findByText(/Any device \(last reported\)/)).toBeTruthy();
  });

  it('shows nothing new for a single machine with no reports', async () => {
    seed([{ id: 'm_1', name: 'solo', online: true, backends: ['host'] }]);
    renderGate();
    await screen.findByText('solo');
    expect(screen.queryByText(STRICTEST_WINS_TEXT)).toBeNull();
    expect(screen.queryByText(/Two entries/)).toBeNull();
  });
});
