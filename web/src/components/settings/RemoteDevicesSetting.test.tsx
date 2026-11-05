import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, configureApi, j } from '../../api';
import { markDeviceEnrolled, pinComputerIdentity } from '../../lib/deviceIdentity';
import { privateRelayEnabled } from '../../lib/privateRelay';
import { isEmbedded } from '../../embed';
import { useLocalProof } from '../../lib/useLocalProof';
import { ALERTS_SCOPE_TEXT, CANNOT_APPROVE_HERE, FULL_SCOPE_TEXT, PRIVATE_RELAY_LABEL, REMOTE_POLICY_LABEL, RemoteDevicesSetting, remoteDevicePendingText } from './RemoteDevicesSetting';
import { CODES_MATCH_APPROVE, NO_CODE_CONFIRM, RECOVERY_BANNER, REQUEST_CHANGED } from './ApproveDeviceDialog';

vi.mock('../../embed', async (orig) => ({ ...(await orig<typeof import('../../embed')>()), isEmbedded: vi.fn(() => false) }));
vi.mock('../../lib/useLocalProof', () => ({
  useLocalProof: vi.fn(() => ({ status: null, backendPrefix: '', reload: async () => {} })),
}));

afterEach(() => {
  vi.restoreAllMocks();
  vi.mocked(isEmbedded).mockReturnValue(false);
});

const COMPARE = 'Compare codes…';

const pendingPhone = {
  id: 'dev_1', name: 'Chrome on Android', status: 'pending' as const, code: 'GJ23-QFPG-G9PE-ASX5', createdAt: '2026-10-24T00:00:00Z',
};

describe('RemoteDevicesSetting', () => {
  it.each(['Approve', 'Remove'] as const)('shows enrolment steps when %s returns device_required', async (button) => {
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [pendingPhone], canManageDevices: true });
    const message = "This screen isn't an approved device yet, so it can't manage devices. Open Settings → Privacy & security → Ask to approve this browser, then approve it from a browser or app you've already approved. If that button isn't shown here, use an already-approved device instead. If you're locked out, recovery steps are in Settings → Privacy & security on the computer.";
    const refusal = () => j<never>(new Response(JSON.stringify({ error: 'device_required', message }), { status: 403 }));
    if (button === 'Approve') vi.spyOn(api, 'approveRemoteDevice').mockImplementation(refusal);
    else vi.spyOn(api, 'removeRemoteDevice').mockImplementation(refusal);
    render(<RemoteDevicesSetting />);
    if (button === 'Approve') {
      fireEvent.click(await screen.findByRole('button', { name: COMPARE }));
      fireEvent.click(screen.getByRole('checkbox', { name: NO_CODE_CONFIRM }));
      fireEvent.click(screen.getByRole('button', { name: CODES_MATCH_APPROVE }));
    } else {
      fireEvent.click(await screen.findByRole('button', { name: button }));
    }
    expect(await screen.findByText(/Open Settings → Privacy & security → Ask to approve this browser/)).toHaveTextContent(message);
    if (button === 'Approve') expect(api.approveRemoteDevice).toHaveBeenCalledTimes(1);
  });

  it.each([
    ['says it cannot', false],
    ['predates the hint', undefined],
  ])('offers no Approve when the daemon %s manage devices from this screen', async (_, canManageDevices) => {
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [
      { ...pendingPhone, codeVersion: 3, enrolmentId: 'enr_AAECAwQFBgcICQoLDA0ODw' },
    ], ...(canManageDevices === undefined ? {} : { canManageDevices }) });
    const approve = vi.spyOn(api, 'approveRemoteDevice');
    render(<RemoteDevicesSetting />);
    expect(await screen.findByText(CANNOT_APPROVE_HERE)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: COMPARE })).toBeNull();
    expect(screen.queryByRole('button', { name: /approve/i })).toBeNull();
    expect(approve).not.toHaveBeenCalled();
  });

  it('offers Approve in Shinoque Code only on its private channel', async () => {
    vi.mocked(isEmbedded).mockReturnValue(true);
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [pendingPhone], canManageDevices: true });
    const { unmount } = render(<RemoteDevicesSetting />);
    expect(await screen.findByText(CANNOT_APPROVE_HERE)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: COMPARE })).toBeNull();
    unmount();
    configureApi({ prefix: '/m/mach_e2e' });
    markDeviceEnrolled('mach_e2e');
    pinComputerIdentity('mach_e2e', 'MFkwPINNED');
    try {
      render(<RemoteDevicesSetting />);
      expect(await screen.findByRole('button', { name: COMPARE })).toBeInTheDocument();
    } finally {
      configureApi({ prefix: '' });
      localStorage.clear();
    }
  });

  it('shows the recovery banner in the dialog while SOLU_HUMAN_PROOF=off', async () => {
    vi.mocked(useLocalProof).mockReturnValue({ status: { envOff: true } as never, backendPrefix: '', reload: async () => {} });
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [pendingPhone], canManageDevices: true });
    render(<RemoteDevicesSetting />);
    fireEvent.click(await screen.findByRole('button', { name: COMPARE }));
    expect(screen.getByText(RECOVERY_BANNER)).toBeInTheDocument();
    vi.mocked(useLocalProof).mockReturnValue({ status: null, backendPrefix: '', reload: async () => {} });
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
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [phone], canManageDevices: true });
    const approve = vi.spyOn(api, 'approveRemoteDevice').mockResolvedValue({ ...phone, status: 'approved' });
    render(<RemoteDevicesSetting />);
    expect(await screen.findByText('V3')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: COMPARE }));
    expect(approve).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: CODES_MATCH_APPROVE }));
    await waitFor(() => expect(approve).toHaveBeenCalledWith(phone.id, phone.enrolmentId));
  });

  it('shows an alerts-only device as such next to its V3 code', async () => {
    const alertsWindow = { ...pendingPhone, name: 'Desktop window', codeVersion: 3, enrolmentId: 'enr_AAECAwQFBgcICQoLDA0ODw', scope: 'alerts' as const };
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [alertsWindow, { ...pendingPhone, id: 'dev_2', codeVersion: 3, enrolmentId: 'enr_AQECAwQFBgcICQoLDA0ODw' }] });
    render(<RemoteDevicesSetting />);
    expect(await screen.findByText(ALERTS_SCOPE_TEXT)).toBeInTheDocument();
    expect(screen.getAllByText(ALERTS_SCOPE_TEXT)).toHaveLength(1);
    expect(screen.getAllByText(FULL_SCOPE_TEXT)).toHaveLength(1);
    expect(screen.getAllByText('V3')).toHaveLength(2);
  });

  it('shows no code or approval button before reveal', async () => {
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [
      { ...pendingPhone, status: 'committed', codeVersion: 3 },
    ], canManageDevices: true });
    render(<RemoteDevicesSetting />);
    expect(await screen.findByText('Waiting for the device to confirm its request.')).toBeInTheDocument();
    expect(screen.queryByText(pendingPhone.code)).toBeNull();
    expect(screen.queryByRole('button', { name: COMPARE })).toBeNull();
  });

  it('re-reads a comparison when approval reports it changed, and does not retry', async () => {
    const list = vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'approved', devices: [
      { ...pendingPhone, codeVersion: 3, enrolmentId: 'enr_AAECAwQFBgcICQoLDA0ODw' },
    ], canManageDevices: true });
    const approve = vi.spyOn(api, 'approveRemoteDevice').mockImplementation(() =>
      j<never>(new Response('device enrolment changed', { status: 409 })));
    render(<RemoteDevicesSetting />);
    fireEvent.click(await screen.findByRole('button', { name: COMPARE }));
    fireEvent.click(screen.getByRole('button', { name: CODES_MATCH_APPROVE }));
    expect(await screen.findByText(REQUEST_CHANGED)).toBeInTheDocument();
    expect(list).toHaveBeenCalledTimes(2);
    expect(approve).toHaveBeenCalledTimes(1);
  });

  it('stays hidden on a daemon without the endpoint', async () => {
    const list = vi.spyOn(api, 'listRemoteDevices').mockResolvedValue(null);
    render(<RemoteDevicesSetting />);
    await waitFor(() => expect(list).toHaveBeenCalled());
    expect(screen.queryByText(REMOTE_POLICY_LABEL)).toBeNull();
  });

  it('approves an older request with no V3 code only after the second confirmation', async () => {
    vi.spyOn(api, 'listRemoteDevices').mockResolvedValue({ policy: 'any', devices: [pendingPhone], canManageDevices: true });
    const approve = vi.spyOn(api, 'approveRemoteDevice').mockResolvedValue({ ...pendingPhone, status: 'approved' });
    render(<RemoteDevicesSetting />);
    expect(await screen.findByText('GJ23-QFPG-G9PE-ASX5')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: COMPARE }));
    expect(screen.getByRole('button', { name: CODES_MATCH_APPROVE })).toBeDisabled();
    fireEvent.click(screen.getByRole('checkbox', { name: NO_CODE_CONFIRM }));
    fireEvent.click(screen.getByRole('button', { name: CODES_MATCH_APPROVE }));
    await waitFor(() => expect(approve).toHaveBeenCalledWith('dev_1', undefined));
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
  it('shows the claimed name as capped plain text', () => {
    const text = remoteDevicePendingText('\u202Eevil' + 'x'.repeat(300));
    expect(text).not.toContain('\u202E');
    expect(text.length).toBeLessThan(260);
  });

  it('names the device and asks to compare codes before approving', () => {
    const text = remoteDevicePendingText('Pixel 8a');
    expect(text).toContain('calling itself “Pixel 8a”');
    expect(text).toContain('same code');
    expect(text).toContain('Settings → Privacy & security');
  });

  it('says when the device asks only to take over push alerts', () => {
    const text = remoteDevicePendingText('Desktop window', 'alerts');
    expect(text).toContain('“Desktop window”');
    expect(text).toContain('push alerts');
    expect(text).toContain('nothing else');
    expect(text).toContain('same code');
    expect(remoteDevicePendingText('Pixel 8a', '')).not.toContain('push alerts');
  });
});
