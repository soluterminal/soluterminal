import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, configureApi, currentRoutingScope, type RemoteDevice } from '../../api';
import * as identity from '../../lib/deviceIdentity';
import { setPrivateRelayEnabled } from '../../lib/privateRelay';
import v from '../../lib/testdata/device-code-v3.json';
import { DeviceVerification } from './DeviceVerification';

afterEach(() => {
  vi.restoreAllMocks();
  configureApi({ prefix: '' });
  localStorage.clear();
});

describe('DeviceVerification', () => {
  const prepare = () => {
    configureApi({ prefix: '/m/mach_verify' });
    identity.markDeviceEnrolled('mach_verify');
    identity.pinComputerIdentity('mach_verify', v.daemonSPKI);
    setPrivateRelayEnabled(true);
    vi.spyOn(identity, 'deviceIdentity').mockResolvedValue({ id: v.deviceId, spki: v.deviceSPKI, code: '', key: {} as CryptoKeyPair });
    const committed = {
      id: v.deviceId, status: 'committed', enrolmentId: v.enrolmentId, codeVersion: 3,
      daemonIdentity: v.daemonSPKI, daemonNonce: v.daemonNonce,
    } as RemoteDevice;
    const commit = vi.spyOn(api, 'verifyRemoteDevice').mockResolvedValue(committed);
    const reveal = vi.spyOn(api, 'revealDeviceVerification').mockResolvedValue({
      ...committed, status: 'approved', verify: { state: 'revealed', enrolmentId: v.enrolmentId, code: 'SERVER-CODE' },
    });
    return { commit, reveal, committed };
  };

  it('offers verification on a private connection and shows a locally computed V3 code', async () => {
    const { commit, reveal } = prepare();
    const scope = currentRoutingScope();
    render(<DeviceVerification />);
    fireEvent.click(screen.getByRole('button', { name: 'Compare V3 codes' }));
    expect(await screen.findByText(/Verify V3/)).toBeInTheDocument();
    expect(commit).toHaveBeenCalledWith(scope, v.deviceId, expect.stringMatching(/^[0-9a-f]{64}$/));
    expect(reveal).toHaveBeenCalledWith(scope, v.deviceId, v.enrolmentId, expect.stringMatching(/^[0-9a-f]{64}$/));
    expect(screen.queryByText('SERVER-CODE')).toBeNull();
    expect(screen.getByText(/[0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4}/)).toBeInTheDocument();
  });

  it('does not offer verification with the private channel disabled', () => {
    prepare();
    setPrivateRelayEnabled(false);
    render(<DeviceVerification />);
    expect(screen.queryByRole('button', { name: 'Compare V3 codes' })).toBeNull();
  });

  it('checks the commitment identity against the private connection pin before computing a code', async () => {
    const { commit, reveal, committed } = prepare();
    commit.mockResolvedValue({ ...committed, daemonIdentity: v.deviceSPKI });
    const compute = vi.spyOn(identity, 'deviceCodeV3For');
    render(<DeviceVerification />);
    fireEvent.click(screen.getByRole('button', { name: 'Compare V3 codes' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Computer identity does not match the private connection');
    expect(compute).not.toHaveBeenCalled();
    expect(reveal).not.toHaveBeenCalled();
    expect(screen.queryByText(/Verify V3/)).toBeNull();
  });

  it('does not accept a V2 fallback as verification', async () => {
    const { commit, reveal } = prepare();
    commit.mockResolvedValue({ id: v.deviceId, status: 'approved', daemonIdentity: v.daemonSPKI, codeVersion: 2 } as RemoteDevice);
    render(<DeviceVerification />);
    fireEvent.click(screen.getByRole('button', { name: 'Compare V3 codes' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Unsupported comparison response');
    expect(reveal).not.toHaveBeenCalled();
  });

  it('does not show a completion from another routing scope', async () => {
    const { reveal, committed } = prepare();
    reveal.mockImplementation(async () => {
      configureApi({ prefix: '/m/mach_other' });
      return { ...committed, status: 'approved', verify: { state: 'revealed', enrolmentId: v.enrolmentId } };
    });
    render(<DeviceVerification />);
    fireEvent.click(screen.getByRole('button', { name: 'Compare V3 codes' }));
    await waitFor(() => expect(reveal).toHaveBeenCalled());
    expect(screen.queryByText(/Verify V3/)).toBeNull();
  });
});
