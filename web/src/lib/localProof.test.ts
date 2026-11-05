import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('./deviceIdentity', async (orig) => {
  const real = await orig<typeof import('./deviceIdentity')>();
  return {
    ...real,
    deviceIdentity: vi.fn(async () => ({ id: 'dev_me', spki: 'SPKI', code: '', key: {} as CryptoKeyPair })),
    signDeviceRequest: vi.fn(async (target: string) => ({ id: 'dev_me', time: '1', nonce: 'n', target, sig: 'sig' })),
  };
});

import { signDeviceRequest } from './deviceIdentity';
import v from './testdata/device-code-v3.json';
import {
  approveThisBrowserFirst, askToApproveThisBrowser, ensureLocalClock, isPaneSocketUrl, localClockOffset, localProofTarget, localSocketStanding,
  localSocketStandingChanged, refreshLocalProof,
} from './localProof';

const statusReply = (over: Record<string, unknown>) => new Response(JSON.stringify({
  mode: 'report', effective: 'report', target: 'local/dmn_a', serverTime: Date.now() + 90_000, envOff: false,
  approvedDevices: 1, canBootstrap: false, ...over,
}), { status: 200 });

describe('localProof', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    localStorage.clear();
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.mocked(signDeviceRequest).mockClear();
  });

  it('signs for a daemon only once it reports this browser approved, and stops when removed', async () => {
    fetchMock.mockResolvedValueOnce(statusReply({ device: 'pending' }));
    await refreshLocalProof('');
    expect(fetchMock.mock.calls[0][0]).toBe('/api/human-proof?device=dev_me');
    expect(localProofTarget('')).toBeNull();

    fetchMock.mockResolvedValueOnce(statusReply({ device: 'approved' }));
    await refreshLocalProof('');
    expect(localProofTarget('')).toBe('local/dmn_a');
    expect(localProofTarget('/b/win')).toBeNull();
    // The daemon's clock is 90 s ahead.
    expect(localClockOffset('local/dmn_a')).toBeGreaterThan(80_000);

    fetchMock.mockResolvedValueOnce(statusReply({ device: '' }));
    await refreshLocalProof('');
    expect(localProofTarget('')).toBeNull();
  });

  it('keeps an older daemon without the endpoint unsigned', async () => {
    fetchMock.mockResolvedValueOnce(new Response('not found', { status: 404 }));
    expect(await refreshLocalProof('')).toBeNull();
    expect(localProofTarget('')).toBeNull();
  });

  it('signs the first-device request with the key it approves, for the daemon’s target', async () => {
    fetchMock
      .mockResolvedValueOnce(statusReply({ canBootstrap: true, approvedDevices: 0 }))
      .mockResolvedValueOnce(new Response('{}', { status: 200 }))
      .mockResolvedValueOnce(statusReply({ device: 'approved' }));
    await approveThisBrowserFirst('/b/wsl');
    const [url, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(url).toBe('/b/wsl/api/human-proof/bootstrap');
    expect(JSON.parse(init.body as string)).toMatchObject({ publicKey: 'SPKI' });
    expect(new Headers(init.headers).get('X-Solu-Device-Target')).toBe('local/dmn_a');
    expect(vi.mocked(signDeviceRequest).mock.calls[0].slice(0, 4)).toEqual(['local/dmn_a', 'POST', '/api/human-proof/bootstrap', '']);
    expect(localProofTarget('/b/wsl')).toBe('local/dmn_a');
  });

  it('reports the daemon’s refusal of a first device', async () => {
    fetchMock
      .mockResolvedValueOnce(statusReply({}))
      .mockResolvedValueOnce(new Response('{"error":"already_bootstrapped","message":"A device is already approved here."}', { status: 409 }));
    await expect(approveThisBrowserFirst('')).rejects.toThrow('A device is already approved here.');
  });

  it('recognises pane terminal sockets', () => {
    expect(isPaneSocketUrl('ws://127.0.0.1:8787/api/panes/p_1/io?cols=80')).toBe(true);
    expect(isPaneSocketUrl('/b/wsl/api/panes/p_1/io')).toBe(true);
    expect(isPaneSocketUrl('ws://127.0.0.1:8787/api/ui/events')).toBe(false);
    expect(isPaneSocketUrl('ws://127.0.0.1:8787/api/panes/p_1/io/extra')).toBe(false);
  });

  it('measures the daemon clock before the first signature and keeps it across reloads', async () => {
    fetchMock.mockResolvedValueOnce(statusReply({ device: 'approved', target: 'local/dmn_clock' }));
    await ensureLocalClock('', 'local/dmn_clock');
    expect(fetchMock).toHaveBeenCalledTimes(1);
    await ensureLocalClock('', 'local/dmn_clock');
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(JSON.parse(localStorage.getItem('soluterminal.localProofClock') ?? '{}')['local/dmn_clock']).toBeGreaterThan(80_000);
    vi.resetModules();
    const fresh = await import('./localProof');
    expect(fresh.localClockOffset('local/dmn_clock')).toBeGreaterThan(80_000);
  });

  it('reopens a socket when this browser is approved or removed, or its clock was far off', async () => {
    const unsigned = { target: null, offset: 0 };
    const signed = { target: 'local/dmn_a', offset: 1_000 };
    expect(localSocketStandingChanged(unsigned, signed)).toBe(true);
    expect(localSocketStandingChanged(signed, unsigned)).toBe(true);
    expect(localSocketStandingChanged(signed, { ...signed, offset: 5_000 })).toBe(false);
    expect(localSocketStandingChanged(signed, { ...signed, offset: 90_000 })).toBe(true);
    expect(localSocketStandingChanged(unsigned, unsigned)).toBe(false);
    fetchMock.mockResolvedValueOnce(statusReply({ device: 'approved' }));
    await refreshLocalProof('');
    expect(localSocketStanding('')).toMatchObject({ target: 'local/dmn_a' });
  });
  it('asks with V3 commit/reveal and shows the code it computed, never the one the daemon sent', async () => {
    const me = { id: v.deviceId, spki: v.deviceSPKI };
    const { deviceIdentity } = await import('./deviceIdentity');
    vi.mocked(deviceIdentity).mockResolvedValueOnce({ ...me, code: '', key: {} as CryptoKeyPair });
    const committed = { id: me.id, status: 'committed', enrolmentId: v.enrolmentId, daemonIdentity: v.daemonSPKI, daemonNonce: v.daemonNonce, codeVersion: 3 };
    fetchMock
      .mockResolvedValueOnce(new Response(JSON.stringify(committed), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ ...committed, status: 'pending', daemonNonce: undefined, code: 'SERVER-SENT-CODE' }), { status: 200 }))
      .mockResolvedValueOnce(statusReply({ device: 'pending' }));
    const code = await askToApproveThisBrowser('');
    expect(code).not.toBe('SERVER-SENT-CODE');
    expect(code).toMatch(/\S/);
    const [enrolUrl, enrolInit] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(enrolUrl).toBe('/api/remote-devices/enroll');
    const sent = JSON.parse(String(enrolInit.body));
    expect(sent).toMatchObject({ publicKey: me.spki, codeVersion: 3 });
    expect(sent.commit).toMatch(/\S/);
    expect(fetchMock.mock.calls[1][0]).toBe('/api/remote-devices/enroll/reveal');
  });

  it('refuses a local request that offers no V3 code', async () => {
    fetchMock
      .mockResolvedValueOnce(new Response(JSON.stringify({ id: 'dev_me', status: 'pending', daemonIdentity: 'MFkw', codeVersion: 2 }), { status: 200 }))
      .mockResolvedValueOnce(statusReply({ device: 'pending' }));
    await expect(askToApproveThisBrowser('')).rejects.toThrow();
  });
});
