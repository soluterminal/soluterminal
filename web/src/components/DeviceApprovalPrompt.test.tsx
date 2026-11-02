import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../lib/deviceIdentity', async (orig) => {
  const real = await orig<typeof import('../lib/deviceIdentity')>();
  return {
    ...real,
    deviceIdentity: vi.fn(async () => ({ id: 'dev_0000000000000000000000', spki: 'MEKEY', code: 'V1CODE', key: {} })),
    deviceCodeFor: vi.fn(async (_spki: string, computer?: string) => (computer ? `V2-${computer}` : 'V1CODE')),
    pinComputerIdentity: vi.fn(),
    markDeviceEnrolled: vi.fn(),
    forgetComputer: vi.fn(),
    pinnedComputerIdentity: vi.fn(() => null),
  };
});

import { api, DEVICE_APPROVAL_EVENT, requestDeviceApproval, type RemoteDevice } from '../api';
import * as identity from '../lib/deviceIdentity';
import { CODE_DIFFERS, CODE_MATCHES, DeviceApprovalPrompt, FORGET_AND_COMPARE } from './DeviceApprovalPrompt';
import v3 from '../lib/testdata/device-code-v3.json';

const device = (status: 'pending' | 'approved', daemonIdentity: string): RemoteDevice =>
  ({ id: 'dev_0000000000000000000000', name: 'b', status, code: 'x', createdAt: '', daemonIdentity }) as RemoteDevice;

const ask = async () => {
  render(<DeviceApprovalPrompt />);
  act(() => { window.dispatchEvent(new CustomEvent(DEVICE_APPROVAL_EVENT, { detail: { target: 'mach_x/wsl' } })); });
  fireEvent.click(await screen.findByRole('button', { name: 'Ask for approval' }));
};

const polls = async (n = 1) => {
  for (let i = 0; i < n; i++) await act(async () => { vi.advanceTimersByTime(3100); });
};

describe('DeviceApprovalPrompt', () => {
  const reload = vi.fn();
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    Object.defineProperty(window, 'location', { value: { ...window.location, reload }, writable: true });
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
    reload.mockClear();
    vi.mocked(identity.pinComputerIdentity).mockClear();
    vi.mocked(identity.markDeviceEnrolled).mockClear();
    vi.mocked(identity.forgetComputer).mockClear();
    vi.mocked(identity.pinnedComputerIdentity).mockReturnValue(null);
  });

  const setupV3 = () => {
    vi.mocked(identity.deviceIdentity).mockResolvedValueOnce({
      id: v3.deviceId, spki: v3.deviceSPKI, code: '', key: {} as CryptoKeyPair,
    });
    const committed: RemoteDevice = {
      id: v3.deviceId, name: 'browser', status: 'committed', createdAt: '',
      daemonIdentity: v3.daemonSPKI, daemonNonce: v3.daemonNonce,
      enrolmentId: v3.enrolmentId, codeVersion: 3,
    };
    const pending: RemoteDevice = { ...committed, status: 'pending', code: 'SERVER-CODE' };
    const enroll = vi.spyOn(api, 'enrollRemoteDevice').mockResolvedValue(committed);
    const reveal = vi.spyOn(api, 'revealRemoteDevice').mockResolvedValue(pending);
    const status = vi.spyOn(api, 'remoteDeviceStatus').mockResolvedValue({ ...pending, status: 'approved' });
    return { enroll, reveal, status, pending };
  };

  it('labels a local V3 comparison and pins only the confirmed approved attempt', async () => {
    const { reveal } = setupV3();
    await ask();
    expect(await screen.findByText('V3')).toBeInTheDocument();
    expect(screen.queryByText('SERVER-CODE')).toBeNull();
    expect(reveal).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole('button', { name: CODE_MATCHES }));
    await polls();
    await waitFor(() => expect(identity.pinComputerIdentity).toHaveBeenCalledWith('mach_x/wsl', v3.daemonSPKI));
  });

  it('keeps a V3 confirmation bound to the displayed enrolment', async () => {
    const { status, pending } = setupV3();
    status.mockResolvedValue({ ...pending, status: 'approved', enrolmentId: 'enr_other' });
    await ask();
    fireEvent.click(await screen.findByRole('button', { name: CODE_MATCHES }));
    await polls();
    expect(await screen.findByRole('alert')).toHaveTextContent(/nothing was changed/);
    expect(identity.pinComputerIdentity).not.toHaveBeenCalled();
    expect(identity.markDeviceEnrolled).not.toHaveBeenCalled();
  });

  it('reports a reveal refusal without a V2 retry', async () => {
    const { reveal, enroll } = setupV3();
    reveal.mockRejectedValue(new Error('409'));
    await ask();
    expect(await screen.findByText(/Couldn't ask for approval/)).toHaveTextContent('409');
    expect(enroll).toHaveBeenCalledTimes(1);
    expect(screen.queryByText('V2')).toBeNull();
    expect(identity.pinComputerIdentity).not.toHaveBeenCalled();
  });

  // After a reload the first refusal can come before the prompt has mounted.
  it('opens for a refusal that came before it mounted', async () => {
    requestDeviceApproval('mach_x/wsl');
    render(<DeviceApprovalPrompt />);
    expect(await screen.findByRole('button', { name: 'Ask for approval' })).toBeInTheDocument();
  });

  // The enrolment response and every status poll come through the relay. A
  // relay can hand over its own key and then report "approved"; without the
  // person comparing codes, that must never pin.
  it('never pins from a forged "approved" poll the person has not confirmed', async () => {
    vi.spyOn(api, 'enrollRemoteDevice').mockResolvedValue(device('pending', 'RELAYKEY'));
    const status = vi.spyOn(api, 'remoteDeviceStatus').mockResolvedValue(device('approved', 'RELAYKEY'));
    await ask();
    expect(await screen.findByText('V2-RELAYKEY')).toBeInTheDocument();
    expect(screen.getByText('V2')).toBeInTheDocument();
    await polls(4);
    await waitFor(() => expect(status).toHaveBeenCalled());
    expect(await screen.findByText('Approved on the computer — does it show this exact code?')).toBeInTheDocument();
    expect(identity.pinComputerIdentity).not.toHaveBeenCalled();
    expect(identity.markDeviceEnrolled).not.toHaveBeenCalled();
    expect(reload).not.toHaveBeenCalled();
  });

  it('pins when the person confirms first and the computer then approves the same key', async () => {
    vi.spyOn(api, 'enrollRemoteDevice').mockResolvedValue(device('pending', 'COMPUTER'));
    const status = vi.spyOn(api, 'remoteDeviceStatus').mockResolvedValue(device('pending', 'COMPUTER'));
    await ask();
    fireEvent.click(await screen.findByRole('button', { name: CODE_MATCHES }));
    expect(await screen.findByText('Thanks — waiting for approval on the computer…')).toBeInTheDocument();
    await polls(2);
    expect(identity.pinComputerIdentity).not.toHaveBeenCalled();
    status.mockResolvedValue(device('approved', 'COMPUTER'));
    await polls(1);
    await waitFor(() => expect(identity.pinComputerIdentity).toHaveBeenCalledWith('mach_x/wsl', 'COMPUTER'));
    expect(identity.markDeviceEnrolled).toHaveBeenCalledWith('mach_x/wsl');
    expect(reload).toHaveBeenCalled();
  });

  it('pins when the computer approves first and the person then confirms', async () => {
    vi.spyOn(api, 'enrollRemoteDevice').mockResolvedValue(device('pending', 'COMPUTER'));
    vi.spyOn(api, 'remoteDeviceStatus').mockResolvedValue(device('approved', 'COMPUTER'));
    await ask();
    await screen.findByText('V2-COMPUTER');
    await polls(1);
    expect(await screen.findByText('Approved on the computer — does it show this exact code?')).toBeInTheDocument();
    expect(identity.pinComputerIdentity).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: CODE_MATCHES }));
    await waitFor(() => expect(identity.pinComputerIdentity).toHaveBeenCalledWith('mach_x/wsl', 'COMPUTER'));
    expect(identity.markDeviceEnrolled).toHaveBeenCalledWith('mach_x/wsl');
  });

  it('saves nothing when the person sees a different code, and says why', async () => {
    vi.spyOn(api, 'enrollRemoteDevice').mockResolvedValue(device('pending', 'RELAYKEY'));
    vi.spyOn(api, 'remoteDeviceStatus').mockResolvedValue(device('approved', 'RELAYKEY'));
    await ask();
    fireEvent.click(await screen.findByRole('button', { name: CODE_DIFFERS }));
    expect(screen.getByRole('alert')).toHaveTextContent(/intercepting/);
    expect(screen.getByRole('alert')).toHaveTextContent(/remove this browser/);
    await polls(3);
    expect(identity.pinComputerIdentity).not.toHaveBeenCalled();
    expect(identity.markDeviceEnrolled).not.toHaveBeenCalled();
  });

  it('pins nothing when the approval reports another key than the code compared', async () => {
    vi.spyOn(api, 'enrollRemoteDevice').mockResolvedValue(device('pending', 'COMPUTER'));
    vi.spyOn(api, 'remoteDeviceStatus').mockResolvedValue(device('approved', 'SWAPPED'));
    await ask();
    fireEvent.click(await screen.findByRole('button', { name: CODE_MATCHES }));
    await polls(1);
    expect(await screen.findByRole('alert')).toHaveTextContent(/intercepting/);
    expect(identity.pinComputerIdentity).not.toHaveBeenCalled();
    expect(identity.markDeviceEnrolled).not.toHaveBeenCalled();
  });

  it('never pins from an immediately-approved response (no comparison happened)', async () => {
    vi.spyOn(api, 'enrollRemoteDevice').mockResolvedValue(device('approved', 'FORGED'));
    await ask();
    await waitFor(() => expect(identity.markDeviceEnrolled).toHaveBeenCalled());
    expect(identity.pinComputerIdentity).not.toHaveBeenCalled();
  });

  it('refuses a computer key that differs from the pinned one, and replaces it only after a confirmed comparison', async () => {
    vi.mocked(identity.pinnedComputerIdentity).mockReturnValue('PINNED');
    const enroll = vi.spyOn(api, 'enrollRemoteDevice').mockResolvedValue(device('pending', 'OTHER'));
    const status = vi.spyOn(api, 'remoteDeviceStatus').mockResolvedValue(device('pending', 'OTHER'));
    await ask();
    expect(await screen.findByText(/identity changed/)).toBeInTheDocument();
    expect(identity.pinComputerIdentity).not.toHaveBeenCalled();

    // Comparing again keeps the old pin and enrolment: a relay that merely
    // claims the identity changed can't push this browser off the channel.
    fireEvent.click(screen.getByRole('button', { name: FORGET_AND_COMPARE }));
    expect(await screen.findByText('V2-OTHER')).toBeInTheDocument();
    expect(enroll).toHaveBeenCalledTimes(2);
    expect(identity.forgetComputer).not.toHaveBeenCalled();
    status.mockResolvedValue(device('approved', 'OTHER'));
    await polls(2);
    expect(identity.pinComputerIdentity).not.toHaveBeenCalled();
    fireEvent.click(await screen.findByRole('button', { name: CODE_MATCHES }));
    await waitFor(() => expect(identity.pinComputerIdentity).toHaveBeenCalledWith('mach_x/wsl', 'OTHER'));
  });

  it('keeps the old key when the replacement code differs', async () => {
    vi.mocked(identity.pinnedComputerIdentity).mockReturnValue('PINNED');
    vi.spyOn(api, 'enrollRemoteDevice').mockResolvedValue(device('pending', 'RELAYKEY'));
    vi.spyOn(api, 'remoteDeviceStatus').mockResolvedValue(device('approved', 'RELAYKEY'));
    await ask();
    fireEvent.click(await screen.findByRole('button', { name: FORGET_AND_COMPARE }));
    fireEvent.click(await screen.findByRole('button', { name: CODE_DIFFERS }));
    expect(screen.getByRole('alert')).toHaveTextContent(/nothing was changed/);
    await polls(2);
    expect(identity.pinComputerIdentity).not.toHaveBeenCalled();
    expect(identity.forgetComputer).not.toHaveBeenCalled();
    expect(identity.markDeviceEnrolled).not.toHaveBeenCalled();
  });

  it('asks to remove the browser on the computer first when it is still approved there', async () => {
    vi.mocked(identity.pinnedComputerIdentity).mockReturnValue('PINNED');
    const enroll = vi.spyOn(api, 'enrollRemoteDevice').mockResolvedValue(device('pending', 'OTHER'));
    await ask();
    await screen.findByText(/identity changed/);
    enroll.mockResolvedValue(device('approved', 'OTHER'));
    fireEvent.click(screen.getByRole('button', { name: FORGET_AND_COMPARE }));
    expect(await screen.findByText(/still approved on the computer/)).toBeInTheDocument();
    expect(identity.pinComputerIdentity).not.toHaveBeenCalled();
    expect(identity.markDeviceEnrolled).not.toHaveBeenCalled();
  });

  it('lets the person take a confirmation back until the computer approves', async () => {
    vi.spyOn(api, 'enrollRemoteDevice').mockResolvedValue(device('pending', 'COMPUTER'));
    vi.spyOn(api, 'remoteDeviceStatus').mockResolvedValue(device('pending', 'COMPUTER'));
    await ask();
    fireEvent.click(await screen.findByRole('button', { name: CODE_MATCHES }));
    fireEvent.click(screen.getByRole('button', { name: CODE_DIFFERS }));
    expect(screen.getByRole('alert')).toHaveTextContent(/intercepting/);
    expect(identity.pinComputerIdentity).not.toHaveBeenCalled();
  });
});
