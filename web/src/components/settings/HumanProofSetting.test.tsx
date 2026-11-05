import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { LocalProofStatus } from '../../lib/localProof';

const status: { current: LocalProofStatus | null } = { current: null };

vi.mock('../../lib/localProof', async (orig) => {
  const real = await orig<typeof import('../../lib/localProof')>();
  return {
    ...real,
    refreshLocalProof: vi.fn(async () => status.current),
    localProofStatus: vi.fn(() => status.current),
    approveThisBrowserFirst: vi.fn(async () => undefined),
    askToApproveThisBrowser: vi.fn(async () => 'ABCD-EFGH-JKMN-PQRS'),
  };
});

import { api } from '../../api';
import { approveThisBrowserFirst, askToApproveThisBrowser } from '../../lib/localProof';
import { HumanProofBanner } from '../HumanProofBanner';
import { HUMAN_PROOF_LABEL, HumanProofSetting, REQUIRE_NEEDS_FULL_DEVICE, approvalProvenance } from './HumanProofSetting';

const base: LocalProofStatus = {
  mode: 'report', effective: 'report', target: 'local/dmn_t', serverTime: Date.now(), envOff: false,
  approvedDevices: 0, canBootstrap: true,
};

beforeEach(() => { status.current = { ...base }; });
afterEach(() => vi.restoreAllMocks());

describe('HumanProofSetting', () => {
  it('offers the first-device approval while nothing is approved', async () => {
    render(<HumanProofSetting />);
    fireEvent.click(await screen.findByRole('button', { name: 'Approve this browser' }));
    await waitFor(() => expect(approveThisBrowserFirst).toHaveBeenCalledWith(''));
    expect(screen.getByText(/If you lose every approved device/)).toHaveTextContent("add SOLU_HUMAN_PROOF=off to ~/.solu/env (WSL, Linux or macOS) or %USERPROFILE%\\.solu\\env (Windows-native), then restart SoluTerminal's background service. If your desktop tray has Restart background service, choose it (on Windows it restarts only the WSL service; if it refuses, use the steps below). Otherwise: with WSL, quit SoluTerminal from the tray icon, run wsl --shutdown in Windows PowerShell (this stops everything running in WSL, including your agents) and reopen SoluTerminal; on Linux run systemctl --user restart soluterminal.service, or if you start it yourself, stop and start it again; on macOS run launchctl kickstart -k gui/$(id -u)/com.soluterminal.daemon, or if you start it yourself, stop and start it again; on Windows without WSL, quit SoluTerminal from the tray icon and reopen it. Then approve a device in Settings › Privacy & security, remove the SOLU_HUMAN_PROOF=off line, and restart the same way again. While that line is there, any browser or program on this computer can approve or remove devices.");
    const select = screen.getByRole('combobox', { name: HUMAN_PROOF_LABEL });
    expect((select.querySelector('option[value="require"]') as HTMLOptionElement).disabled).toBe(true);
  });

  it('asks for approval from another device once one is approved, and shows the code', async () => {
    status.current = { ...base, canBootstrap: false, approvedDevices: 1 };
    vi.mocked(askToApproveThisBrowser).mockImplementationOnce(async () => {
      status.current = { ...status.current!, device: 'pending' };
      return 'ABCD-EFGH-JKMN-PQRS';
    });
    render(<HumanProofSetting />);
    expect(await screen.findByText("This screen isn't an approved device yet. Choose Ask to approve this browser, then approve it from a browser or app you've already approved. Recovery steps are below if you're locked out.")).toBeInTheDocument();
    fireEvent.click(await screen.findByRole('button', { name: 'Ask to approve this browser' }));
    await waitFor(() => expect(askToApproveThisBrowser).toHaveBeenCalled());
    expect(await screen.findByText(/ABCD-EFGH-JKMN-PQRS/)).toBeInTheDocument();
  });

  it('lets an approved browser require approved devices after listing them', async () => {
    status.current = { ...base, canBootstrap: false, approvedDevices: 1, device: 'approved' };
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'any', devices: [
      { id: 'dev_a', name: 'Edge on Windows', status: 'approved', createdAt: '', approvedVia: 'first-device', surface: 'local' },
    ] });
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true);
    const set = vi.spyOn(api, 'setHumanProofMode').mockResolvedValue({ ...base, mode: 'require', effective: 'require' });
    render(<HumanProofSetting />);
    const select = await screen.findByRole('combobox', { name: HUMAN_PROOF_LABEL });
    fireEvent.change(select, { target: { value: 'require' } });
    await waitFor(() => expect(set).toHaveBeenCalledWith('require'));
    expect(confirm.mock.calls[0][0]).toContain('Edge on Windows (asked at this computer, approved as the first device)');
  });

  it('never lists an alerts-only device as one that can act for you', async () => {
    status.current = { ...base, canBootstrap: false, approvedDevices: 1, device: 'approved' };
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'any', devices: [
      { id: 'dev_a', name: 'Edge on Windows', status: 'approved', createdAt: '', approvedVia: 'first-device', surface: 'local' },
      { id: 'dev_w', name: 'Desktop window', status: 'approved', scope: 'alerts', createdAt: '', approvedVia: 'device:dev_a', surface: 'local' },
    ] });
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true);
    const set = vi.spyOn(api, 'setHumanProofMode').mockResolvedValue({ ...base, mode: 'require', effective: 'require' });
    render(<HumanProofSetting />);
    fireEvent.change(await screen.findByRole('combobox', { name: HUMAN_PROOF_LABEL }), { target: { value: 'require' } });
    await waitFor(() => expect(set).toHaveBeenCalledWith('require'));
    expect(confirm.mock.calls[0][0]).toContain('Edge on Windows');
    expect(confirm.mock.calls[0][0]).not.toContain('Desktop window');
  });

  it('refuses to require approved devices when the list shows only an alerts-only device', async () => {
    // Defence in depth: the list read for the confirmation has no full device.
    status.current = { ...base, canBootstrap: false, approvedDevices: 1, device: 'approved' };
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'any', devices: [
      { id: 'dev_w', name: 'Desktop window', status: 'approved', scope: 'alerts', createdAt: '', surface: 'local' },
    ] });
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true);
    const set = vi.spyOn(api, 'setHumanProofMode');
    render(<HumanProofSetting />);
    fireEvent.change(await screen.findByRole('combobox', { name: HUMAN_PROOF_LABEL }), { target: { value: 'require' } });
    expect(await screen.findByText(REQUIRE_NEEDS_FULL_DEVICE)).toBeInTheDocument();
    expect(confirm).not.toHaveBeenCalled();
    expect(set).not.toHaveBeenCalled();
  });

  it('reports actions accepted without an approved device', async () => {
    status.current = { ...base, unproven: { count: 3, at: new Date().toISOString(), action: 'answer an agent’s request' } };
    render(<HumanProofSetting />);
    expect(await screen.findByText(/3 actions were accepted/)).toBeInTheDocument();
  });

  it('says when the check is off at this computer', async () => {
    status.current = { ...base, envOff: true };
    render(<HumanProofSetting />);
    expect(await screen.findByText(/Turned off at this computer/)).toHaveTextContent('Remove the line from ~/.solu/env (WSL, Linux or macOS) or %USERPROFILE%\\.solu\\env (Windows-native) and restart the background service the same way to use this setting.');
    expect(screen.getByRole('combobox', { name: HUMAN_PROOF_LABEL })).toBeDisabled();
  });
});

describe('approvalProvenance', () => {
  it('names the approving device when known', () => {
    expect(approvalProvenance({ approvedVia: 'device:dev_a', surface: 'local' }, { dev_a: 'Edge on Windows' }))
      .toBe('asked at this computer, approved from “Edge on Windows”');
    expect(approvalProvenance({ approvedVia: 'relay-device:dev_p', surface: 'local' }))
      .toBe('asked at this computer, approved from another approved device through the relay');
    expect(approvalProvenance({ approvedVia: 'headers', surface: 'relay' }))
      .toBe('asked through the relay, approved before approved devices were required');
  });
});

describe('HumanProofBanner', () => {
  it('stays up after a first-device approval until an approved browser acknowledges it', async () => {
    status.current = { ...base, notices: [{ id: 'dev_a', name: 'Edge on Windows', at: new Date().toISOString(), via: 'first-device' }] };
    const { rerender } = render(<HumanProofBanner />);
    expect(await screen.findByText(/First device approved on this computer: “Edge on Windows”/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'That was me' })).toBeNull();
    status.current = { ...status.current, device: 'approved' };
    const ack = vi.spyOn(api, 'acknowledgeFirstDevice').mockResolvedValue(base);
    rerender(<HumanProofBanner key="2" />);
    fireEvent.click(await screen.findByRole('button', { name: 'That was me' }));
    await waitFor(() => expect(ack).toHaveBeenCalled());
  });

  it('announces a device approved while the check was off, and says device management is open', async () => {
    status.current = { ...base, envOff: true, notices: [{ id: 'dev_b', name: 'Chrome on Windows', at: new Date().toISOString(), via: 'env-recovery' }] };
    render(<HumanProofBanner />);
    expect(await screen.findByText(/Approved while the device check was off: “Chrome on Windows”/)).toBeInTheDocument();
    expect(screen.getByText(/any\s+browser here can approve or remove devices/)).toHaveTextContent('When you’re done, remove the line and restart the background service (see Settings → Privacy & security).');
  });

  it('announces a removal made by a device approved without proof', async () => {
    status.current = { ...base, notices: [{ id: 'dev_c', name: 'Firefox', at: new Date().toISOString(), via: 'removed', removed: 'Pixel' }] };
    render(<HumanProofBanner />);
    expect(await screen.findByText(/“Firefox”, approved without an approved device’s proof, removed “Pixel”/)).toBeInTheDocument();
  });

  it('shows the env override and changes made outside the app', async () => {
    status.current = { ...base, lowered: 'require → off' };
    render(<HumanProofBanner />);
    expect(await screen.findByText(/changed outside the app \(require → off\)/)).toBeInTheDocument();
  });

  it('shows nothing when there is nothing to say', async () => {
    const { container } = render(<HumanProofBanner />);
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });
});
