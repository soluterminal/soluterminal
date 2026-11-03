import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, configureApi, j } from '../../api';
import { markDeviceEnrolled, pinComputerIdentity } from '../../lib/deviceIdentity';
import { privateRelayEnabled } from '../../lib/privateRelay';
import { PRIVATE_RELAY_LABEL, REMOTE_POLICY_LABEL, RemoteDevicesSetting, remoteDevicePendingText } from './RemoteDevicesSetting';

afterEach(() => vi.restoreAllMocks());

const pendingPhone = {
  id: 'dev_1', name: 'Chrome on Android', status: 'pending' as const, code: 'GJ23-QFPG-G9PE-ASX5', createdAt: '2026-10-24T00:00:00Z',
};

describe('RemoteDevicesSetting', () => {
  it.each(['Approve', 'Remove'] as const)('shows enrolment steps when %s returns device_required', async (button) => {
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [pendingPhone] });
    const message = "This screen isn't an approved device yet, so it can't manage devices. Open Settings → Privacy & security → Ask to approve this browser, then approve it from a browser or app you've already approved. If that button isn't shown here, use an already-approved device instead. If you're locked out, recovery steps are in Settings → Privacy & security on the computer.";
    const refusal = () => j<never>(new Response(JSON.stringify({ error: 'device_required', message }), { status: 403 }));
    if (button === 'Approve') vi.spyOn(api, 'approveRemoteDevice').mockImplementation(refusal);
    else vi.spyOn(api, 'removeRemoteDevice').mockImplementation(refusal);
    render(<RemoteDevicesSetting />);
    fireEvent.click(await screen.findByRole('button', { name: button }));
    expect(await screen.findByText(/Open Settings → Privacy & security → Ask to approve this browser/)).toHaveTextContent(message);
  });

  it('confirms verification separately from approval', async () => {
    const phone = { ...pendingPhone, status: 'approved' as const, verify: {
      state: 'revealed' as const, enrolmentId: 'enr_AAECAwQFBgcICQoLDA0ODw', code: 'V3-LOCAL-CODE',
    } };
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [phone] });
    const confirm = vi.spyOn(api, 'confirmDeviceVerification').mockResolvedValue(phone);
    const approve = vi.spyOn(api, 'approveRemoteDevice');
    render(<RemoteDevicesSetting />);
    expect(await screen.findByText('V3-LOCAL-CODE')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Verify codes match' }));
    await waitFor(() => expect(confirm).toHaveBeenCalledWith(phone.id, phone.verify.enrolmentId));
    expect(approve).not.toHaveBeenCalled();
  });

  it('rejects verification and offers removal without revoking automatically', async () => {
    const phone = { ...pendingPhone, status: 'approved' as const, verify: {
      state: 'revealed' as const, enrolmentId: 'enr_AAECAwQFBgcICQoLDA0ODw', code: 'V3-LOCAL-CODE',
    } };
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [phone] });
    const reject = vi.spyOn(api, 'rejectDeviceVerification').mockResolvedValue(phone);
    const remove = vi.spyOn(api, 'removeRemoteDevice');
    render(<RemoteDevicesSetting />);
    fireEvent.click(await screen.findByRole('button', { name: 'Codes differ' }));
    await waitFor(() => expect(reject).toHaveBeenCalledWith(phone.id, phone.verify.enrolmentId));
    expect(await screen.findByRole('alert')).toHaveTextContent('You can remove this device');
    expect(screen.getByRole('button', { name: 'Remove' })).toBeInTheDocument();
    expect(remove).not.toHaveBeenCalled();
  });

  it('shows V3 next to a revealed code and approves the displayed enrolment', async () => {
    const phone = { ...pendingPhone, codeVersion: 3, enrolmentId: 'enr_AAECAwQFBgcICQoLDA0ODw' };
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [phone] });
    const approve = vi.spyOn(api, 'approveRemoteDevice').mockResolvedValue({ ...phone, status: 'approved' });
    render(<RemoteDevicesSetting />);
    expect(await screen.findByText('V3')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Approve' }));
    await waitFor(() => expect(approve).toHaveBeenCalledWith(phone.id, phone.enrolmentId));
  });

  it('shows no code or approval button before reveal', async () => {
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [
      { ...pendingPhone, status: 'committed', codeVersion: 3 },
    ] });
    render(<RemoteDevicesSetting />);
    expect(await screen.findByText('Waiting for the device to confirm its request.')).toBeInTheDocument();
    expect(screen.queryByText(pendingPhone.code)).toBeNull();
    expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull();
  });

  it('re-reads a comparison when approval reports it changed', async () => {
    const list = vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [
      { ...pendingPhone, codeVersion: 3, enrolmentId: 'enr_AAECAwQFBgcICQoLDA0ODw' },
    ] });
    vi.spyOn(api, 'approveRemoteDevice').mockRejectedValue(new Error('409'));
    render(<RemoteDevicesSetting />);
    fireEvent.click(await screen.findByRole('button', { name: 'Approve' }));
    await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
  });

  it('stays hidden on a daemon without the endpoint', async () => {
    const list = vi.spyOn(api, 'listRemoteDevices').mockResolvedValue(null);
    render(<RemoteDevicesSetting />);
    await waitFor(() => expect(list).toHaveBeenCalled());
    expect(screen.queryByText(REMOTE_POLICY_LABEL)).toBeNull();
  });

  it('shows a pending device with its code and approves it', async () => {
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'any', devices: [pendingPhone] });
    const approve = vi.spyOn(api, 'approveRemoteDevice').mockResolvedValue({ ...pendingPhone, status: 'approved' });
    render(<RemoteDevicesSetting />);
    expect(await screen.findByText('GJ23-QFPG-G9PE-ASX5')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Approve' }));
    await waitFor(() => expect(approve).toHaveBeenCalledWith('dev_1'));
  });

  it('warns before requiring approval with no approved device', async () => {
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'any', devices: [pendingPhone] });
    const set = vi.spyOn(api, 'setRemoteAccessPolicy').mockResolvedValue({ policy: 'approved', devices: [] });
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    render(<RemoteDevicesSetting />);
    fireEvent.change(await screen.findByRole('combobox'), { target: { value: 'approved' } });
    expect(confirm).toHaveBeenCalled();
    expect(set).not.toHaveBeenCalled();
    confirm.mockReturnValue(true);
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'approved' } });
    await waitFor(() => expect(set).toHaveBeenCalledWith('approved'));
  });

  describe('end-to-end encryption opt-in', () => {
    afterEach(() => {
      configureApi({ prefix: '' });
      localStorage.clear();
    });

    it('is not offered locally or for a computer this browser has not pinned', async () => {
      const list = vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'any', devices: [] });
      render(<RemoteDevicesSetting />);
      await waitFor(() => expect(list).toHaveBeenCalled());
      expect(await screen.findByText(REMOTE_POLICY_LABEL)).toBeInTheDocument();
      expect(screen.queryByText(PRIVATE_RELAY_LABEL)).toBeNull();
    });

    it('is offered, on by default, for an enrolled and pinned computer, and can be turned off and on', async () => {
      configureApi({ prefix: '/m/mach_e2e' });
      markDeviceEnrolled('mach_e2e');
      pinComputerIdentity('mach_e2e', 'MFkwPINNED');
      vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [] });
      const reload = vi.fn();
      vi.spyOn(window, 'location', 'get').mockReturnValue({ ...window.location, reload } as Location);
      render(<RemoteDevicesSetting />);
      await screen.findByText(REMOTE_POLICY_LABEL);
      const box = screen.getByRole('checkbox', { name: PRIVATE_RELAY_LABEL });
      expect(box).toBeChecked();
      fireEvent.click(box);
      expect(privateRelayEnabled()).toBe(false);
      expect(box).not.toBeChecked();
      expect(reload).not.toHaveBeenCalled();
      // Turning it back on ends every connection already open on the
      // plaintext relay (they come back through the channel only).
      fireEvent.click(box);
      expect(privateRelayEnabled()).toBe(true);
      expect(reload).toHaveBeenCalledTimes(1);
    });

    it('stays available when the device list is not', async () => {
      configureApi({ prefix: '/m/mach_e2e' });
      markDeviceEnrolled('mach_e2e');
      pinComputerIdentity('mach_e2e', 'MFkwPINNED');
      vi.spyOn(api, 'listRemoteDevices').mockResolvedValue(null);
      render(<RemoteDevicesSetting />);
      expect(await screen.findByRole('checkbox', { name: PRIVATE_RELAY_LABEL })).toBeInTheDocument();
      expect(screen.queryByText(REMOTE_POLICY_LABEL)).toBeNull();
    });
  });
});

describe('remoteDevicePendingText', () => {
  it('names the device and asks to compare codes before approving', () => {
    const text = remoteDevicePendingText('Pixel 8a');
    expect(text).toContain('“Pixel 8a”');
    expect(text).toContain('same code');
    expect(text).toContain('Settings → Privacy & security');
  });
});
