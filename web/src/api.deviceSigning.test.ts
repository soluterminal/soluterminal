import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('./lib/deviceIdentity', async (orig) => {
  const real = await orig<typeof import('./lib/deviceIdentity')>();
  return {
    ...real,
    signDeviceRequest: vi.fn(async (target: string) => ({ id: 'dev_t', time: '1', nonce: 'n', target, sig: 'sig' })),
  };
});

vi.mock('./lib/localProof', async (orig) => {
  const real = await orig<typeof import('./lib/localProof')>();
  return { ...real, refreshLocalProof: vi.fn(async () => null) };
});

import { api, apiFetch, configureApi, signStreamUrl, streamSigningNeeded } from './api';
import { markDeviceEnrolled, signDeviceRequest } from './lib/deviceIdentity';
import { refreshLocalProof } from './lib/localProof';

// This browser approved by the local daemon (lib/localProof.ts stores the
// target per backend once the daemon reports it approved).
function approveLocally(backendPrefix = '', target = 'local/dmn_test'): void {
  localStorage.setItem('soluterminal.localProofTargets', JSON.stringify({ [backendPrefix]: target }));
  markDeviceEnrolled(target);
}

describe('approved-device request signing', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    localStorage.clear();
    fetchMock = vi.fn(async () => new Response('{"cmd":[]}', { status: 200, headers: { 'content-type': 'application/json' } }));
    vi.stubGlobal('fetch', fetchMock);
  });
  afterEach(() => {
    configureApi({ prefix: '', token: null });
    vi.unstubAllGlobals();
    vi.mocked(signDeviceRequest).mockClear();
  });

  const sentHeaders = () => new Headers((fetchMock.mock.calls[0][1] as RequestInit).headers);

  it('leaves unenrolled machines unsigned', async () => {
    configureApi({ prefix: '/m/mach_x', token: 'jwt' });
    await api.listRemoteDevices().catch(() => null);
    expect(sentHeaders().get('X-Solu-Device')).toBeNull();
    expect(signDeviceRequest).not.toHaveBeenCalled();
  });

  it('signs relayed requests to an enrolled machine', async () => {
    markDeviceEnrolled('mach_x');
    configureApi({ prefix: '/m/mach_x', token: 'jwt' });
    await api.listRemoteDevices().catch(() => null);
    expect(sentHeaders().get('X-Solu-Device')).toBe('dev_t');
    expect(sentHeaders().get('X-Solu-Device-Target')).toBe('mach_x');
    expect(signDeviceRequest).toHaveBeenCalledWith('mach_x', 'GET', '/api/remote-devices', '', expect.any(Uint8Array));
  });

  it('never signs local requests', async () => {
    markDeviceEnrolled('mach_x');
    await api.listRemoteDevices().catch(() => null);
    expect(sentHeaders().get('X-Solu-Device')).toBeNull();
  });

  it('signs the decoded path the daemon checks', async () => {
    markDeviceEnrolled('mach_x');
    configureApi({ prefix: '/m/mach_x', token: 'jwt' });
    await signStreamUrl('wss://cloud.example/m/mach_x/api/workspaces/My%20Work%40home/events');
    expect(signDeviceRequest).toHaveBeenCalledWith('mach_x', 'GET', '/api/workspaces/My Work@home/events', '', expect.any(Uint8Array));
  });

  it('leaves out the query parameters the cloud strips', async () => {
    markDeviceEnrolled('mach_x');
    configureApi({ prefix: '/m/mach_x', token: 'jwt' });
    await signStreamUrl('wss://cloud.example/m/mach_x/api/ui/events?ticket=t1&access_token=a&x=1');
    expect(signDeviceRequest).toHaveBeenCalledWith('mach_x', 'GET', '/api/ui/events', 'x=1', expect.any(Uint8Array));
  });

  it('signs stream URLs for an enrolled machine with the daemon-side path', async () => {
    markDeviceEnrolled('mach_x');
    configureApi({ prefix: '/m/mach_x', token: 'jwt' });
    const url = await signStreamUrl('wss://cloud.example/m/mach_x/api/panes/p1/io?cols=80');
    expect(url).toContain('solu_dev=dev_t');
    expect(signDeviceRequest).toHaveBeenCalledWith('mach_x', 'GET', '/api/panes/p1/io', 'cols=80', expect.any(Uint8Array));
  });

  it('signs local requests that can carry authority once this computer approved the browser', async () => {
    approveLocally();
    await apiFetch('/api/pane-confirmations/c1', { method: 'POST', body: '{"allow":true}' });
    expect(sentHeaders().get('X-Solu-Device-Target')).toBe('local/dmn_test');
    expect(signDeviceRequest).toHaveBeenCalledWith('local/dmn_test', 'POST', '/api/pane-confirmations/c1', '', expect.anything(), 0);
  });

  it('keeps local reads, uploads and unapproved browsers unsigned', async () => {
    await apiFetch('/api/x', { method: 'POST', body: '{}' });
    approveLocally();
    await apiFetch('/api/panes');
    await apiFetch('/api/upload', { method: 'POST', body: new FormData() });
    for (const call of fetchMock.mock.calls) {
      expect(new Headers((call[1] as RequestInit).headers).get('X-Solu-Device')).toBeNull();
    }
    expect(signDeviceRequest).not.toHaveBeenCalled();
  });

  it('signs a local DELETE without a body', async () => {
    approveLocally();
    await apiFetch('/api/remote-devices/dev_a', { method: 'DELETE' });
    expect(signDeviceRequest).toHaveBeenCalledWith('local/dmn_test', 'DELETE', '/api/remote-devices/dev_a', '', expect.any(Uint8Array), 0);
  });

  it('signs for the backend in view in the desktop app', async () => {
    approveLocally('/b/win', 'local/dmn_win');
    await apiFetch('/api/x', { method: 'POST', body: '{}' });
    expect(signDeviceRequest).not.toHaveBeenCalled();
    configureApi({ prefix: '', token: null });
    const { setBackend } = await import('./api');
    setBackend('win');
    try {
      await apiFetch('/api/x', { method: 'POST', body: '{}' });
      expect(signDeviceRequest).toHaveBeenCalledWith('local/dmn_win', 'POST', '/api/x', '', expect.anything(), 0);
    } finally {
      setBackend(null);
    }
  });

  it('re-measures the clock and resends once when the daemon finds the signature stale', async () => {
    approveLocally();
    fetchMock
      .mockResolvedValueOnce(new Response('{"error":"device_required"}', { status: 403, headers: { 'x-solu-human-proof': 'stale' } }))
      .mockResolvedValueOnce(new Response('{}', { status: 200 }));
    const res = await apiFetch('/api/x', { method: 'POST', body: '{}' });
    expect(res.status).toBe(200);
    expect(refreshLocalProof).toHaveBeenCalledWith('');
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(signDeviceRequest).toHaveBeenCalledTimes(2);
  });

  it('signs only the pane terminal socket among local streams', async () => {
    approveLocally();
    expect(streamSigningNeeded(undefined, 'ws://127.0.0.1:8787/api/panes/p1/io?cols=80')).toBe(true);
    expect(streamSigningNeeded(undefined, 'ws://127.0.0.1:8787/api/ui/events')).toBe(false);
    const url = await signStreamUrl('ws://127.0.0.1:8787/api/panes/p1/io?cols=80');
    expect(url).toContain('solu_dev_to=local%2Fdmn_test');
    expect(signDeviceRequest).toHaveBeenCalledWith('local/dmn_test', 'GET', '/api/panes/p1/io', 'cols=80', expect.any(Uint8Array), 0);
    expect(await signStreamUrl('ws://127.0.0.1:8787/api/ui/events')).toBe('ws://127.0.0.1:8787/api/ui/events');
  });
});
