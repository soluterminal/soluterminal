import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { LocalProofStatus } from '../../lib/localProof';

const status: { current: LocalProofStatus | null } = { current: null };
const frame = vi.hoisted(() => ({ embedded: false }));
vi.mock('../../embed', async (orig) => ({ ...(await orig<typeof import('../../embed')>()), isEmbedded: () => frame.embedded }));

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

import { api, setBackend } from '../../api';
import { DialogHost } from '../DialogHost';
import { useStore } from '../../store';
import { approveThisBrowserFirst, askToApproveThisBrowser } from '../../lib/localProof';
import { HumanProofBanner } from '../HumanProofBanner';
import { HUMAN_PROOF_LABEL, HumanProofSetting, REQUIRE_NEEDS_FULL_DEVICE, approvalProvenance } from './HumanProofSetting';

const base: LocalProofStatus = {
  mode: 'report', effective: 'report', target: 'local/dmn_t', serverTime: Date.now(), envOff: false,
  approvedDevices: 0, canBootstrap: true,
};

beforeEach(() => { status.current = { ...base }; frame.embedded = false; });
afterEach(() => {
  act(() => { useStore.getState().closeDialog(null); setBackend(null); });
  vi.restoreAllMocks();
});

describe('HumanProofSetting', () => {
  it('offers the first-device approval while nothing is approved', async () => {
    render(<HumanProofSetting />);
    fireEvent.click(await screen.findByRole('button', { name: 'Approve this browser' }));
    await waitFor(() => expect(approveThisBrowserFirst).toHaveBeenCalledWith(''));
    expect(screen.getByText(/If you lose every approved device/)).toHaveTextContent("add SOLU_HUMAN_PROOF=off to ~/.solu/env (WSL, Linux or macOS) or %USERPROFILE%\\.solu\\env (Windows-native), then restart SoluTerminal's background service. If your desktop tray has Restart background service, choose it (on Windows it restarts only the WSL service; if it refuses, use the steps below). Otherwise: with WSL, quit SoluTerminal from the tray icon, run wsl --shutdown in Windows PowerShell (this stops everything running in WSL, including your agents) and reopen SoluTerminal; on Linux run systemctl --user restart soluterminal.service, or if you start it yourself, stop and start it again; on macOS run launchctl kickstart -k gui/$(id -u)/com.soluterminal.daemon, or if you start it yourself, stop and start it again; on Windows without WSL, quit SoluTerminal from the tray icon and reopen it. Then approve a device in Settings › Privacy & security, remove the SOLU_HUMAN_PROOF=off line, and restart the same way again. While that line is there, any browser or program on this computer can approve or remove devices.");
    const select = screen.getByRole('combobox', { name: HUMAN_PROOF_LABEL });
    expect((select.querySelector('option[value="require"]') as HTMLOptionElement).disabled).toBe(true);
  });

  it('in the Shinoque Code frame offers no first-device button and points at a browser at the computer', async () => {
    frame.embedded = true;
    render(<HumanProofSetting />);
    expect(await screen.findByText('Approve the first browser from a web browser on this computer, in Settings → Privacy & security.')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Approve this browser' })).toBeNull();
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

  it.each(['browser', 'async plugin', 'refused plugin'])('waits for an in-app choice before requiring devices (%s)', async (native) => {
    status.current = { ...base, canBootstrap: false, approvedDevices: 1, device: 'approved' };
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'any', devices: [
      { id: 'dev_a', name: 'Edge on Windows', status: 'approved', createdAt: '', approvedVia: 'first-device', surface: 'local' },
    ] });
    const confirm = vi.spyOn(window, 'confirm').mockImplementation((() => {
      if (native === 'refused plugin') return Promise.reject(new Error('dialog not allowed'));
      return native === 'async plugin' ? Promise.resolve(false) : true;
    }) as typeof window.confirm);
    const set = vi.spyOn(api, 'setHumanProofMode').mockResolvedValue({ ...base, mode: 'require', effective: 'require' });
    render(<><HumanProofSetting /><DialogHost /></>);
    const select = await screen.findByRole('combobox', { name: HUMAN_PROOF_LABEL });
    fireEvent.change(select, { target: { value: 'require' } });
    expect(await screen.findByRole('dialog')).toHaveTextContent('Edge on Windows (asked at this computer, approved as the first device)');
    expect(set).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(select).toBeEnabled());
    expect(select).toHaveValue('report');
    expect(set).not.toHaveBeenCalled();
    fireEvent.change(select, { target: { value: 'require' } });
    await screen.findByRole('dialog');
    fireEvent.click(screen.getByRole('button', { name: 'Confirm' }));
    await waitFor(() => expect(set).toHaveBeenCalledExactlyOnceWith('require'));
    expect(confirm).not.toHaveBeenCalled();
  });

  it('does not require devices when no dialog host is mounted', async () => {
    status.current = { ...base, canBootstrap: false, approvedDevices: 1, device: 'approved' };
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'any', devices: [
      { id: 'dev_a', name: 'Preview browser', status: 'approved', createdAt: '' },
    ] });
    const set = vi.spyOn(api, 'setHumanProofMode');
    render(<HumanProofSetting />);
    const select = await screen.findByRole('combobox', { name: HUMAN_PROOF_LABEL });
    fireEvent.change(select, { target: { value: 'require' } });
    await waitFor(() => expect(useStore.getState().dialog).not.toBeNull());
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(set).not.toHaveBeenCalled();
    act(() => useStore.getState().closeDialog(false));
    await waitFor(() => expect(select).toBeEnabled());
    expect(set).not.toHaveBeenCalled();
  });

  it('cancels a device-proof choice if the selected backend changes while the dialog waits', async () => {
    status.current = { ...base, canBootstrap: false, approvedDevices: 1, device: 'approved' };
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'any', devices: [
      { id: 'dev_a', name: 'Original computer browser', status: 'approved', createdAt: '' },
    ] });
    const set = vi.spyOn(api, 'setHumanProofMode');
    render(<><HumanProofSetting /><DialogHost /></>);
    const select = await screen.findByRole('combobox', { name: HUMAN_PROOF_LABEL });
    fireEvent.change(select, { target: { value: 'require' } });
    await screen.findByRole('dialog');
    act(() => setBackend('win'));
    fireEvent.click(screen.getByRole('button', { name: 'Confirm' }));
    await waitFor(() => expect(select).toBeEnabled());
    expect(select).toHaveValue('report');
    expect(set).not.toHaveBeenCalled();
  });

  it('discards a device list fetched for a backend that is no longer selected', async () => {
    status.current = { ...base, canBootstrap: false, approvedDevices: 1, device: 'approved' };
    let resolve!: (value: Awaited<ReturnType<typeof api.listRemoteDevices>>) => void;
    vi.spyOn(api, 'listRemoteDevices').mockImplementation(() => new Promise((done) => { resolve = done; }));
    const set = vi.spyOn(api, 'setHumanProofMode');
    render(<><HumanProofSetting /><DialogHost /></>);
    const select = await screen.findByRole('combobox', { name: HUMAN_PROOF_LABEL });
    fireEvent.change(select, { target: { value: 'require' } });
    await act(async () => {
      setBackend('win');
      resolve({ policy: 'any', devices: [{ id: 'dev_a', name: 'Original computer browser', status: 'approved', createdAt: '' }] });
    });
    await waitFor(() => expect(select).toBeEnabled());
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(set).not.toHaveBeenCalled();
  });

  it('never lists an alerts-only device as one that can act for you', async () => {
    status.current = { ...base, canBootstrap: false, approvedDevices: 1, device: 'approved' };
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'any', devices: [
      { id: 'dev_a', name: 'Edge on Windows', status: 'approved', createdAt: '', approvedVia: 'first-device', surface: 'local' },
      { id: 'dev_w', name: 'Desktop window', status: 'approved', scope: 'alerts', createdAt: '', approvedVia: 'device:dev_a', surface: 'local' },
    ] });
    const set = vi.spyOn(api, 'setHumanProofMode').mockResolvedValue({ ...base, mode: 'require', effective: 'require' });
    render(<><HumanProofSetting /><DialogHost /></>);
    fireEvent.change(await screen.findByRole('combobox', { name: HUMAN_PROOF_LABEL }), { target: { value: 'require' } });
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('Edge on Windows');
    expect(dialog).not.toHaveTextContent('Desktop window');
    fireEvent.click(screen.getByRole('button', { name: 'Confirm' }));
    await waitFor(() => expect(set).toHaveBeenCalledWith('require'));
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
