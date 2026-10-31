import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, configureApi } from '../../api';
import { markDeviceEnrolled, pinComputerIdentity } from '../../lib/deviceIdentity';
import { privateRelayEnabled } from '../../lib/privateRelay';
import { PRIVATE_RELAY_LABEL, REMOTE_POLICY_LABEL, RemoteDevicesSetting, remoteDevicePendingText } from './RemoteDevicesSetting';

afterEach(() => vi.restoreAllMocks());

const pendingPhone = {
  id: 'dev_1', name: 'Chrome on Android', status: 'pending' as const, code: 'GJ23-QFPG-G9PE-ASX5', createdAt: '2026-10-24T00:00:00Z',
};

describe('RemoteDevicesSetting', () => {
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
