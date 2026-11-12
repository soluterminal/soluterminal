import { afterEach, describe, expect, it, vi } from 'vitest';
import { cloudPairApi } from './cloudPair';

// The daemon's real answers (daemon/api.go): pair/start returns JSON;
// pair/cancel and disconnect return 200 with an empty body.
function answer(body: string, init?: ResponseInit) {
  const fetchMock = vi.fn().mockResolvedValue(new Response(body, init));
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

describe('cloudPairApi', () => {
  afterEach(() => { vi.unstubAllGlobals(); });

  it('treats the empty 200 from disconnect as success', async () => {
    const fetchMock = answer('', { status: 200 });
    await expect(cloudPairApi.disconnect()).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledWith('/api/cloud/disconnect', { method: 'POST' });
  });

  it('treats the empty 200 from pair/cancel as success', async () => {
    answer('', { status: 200 });
    await expect(cloudPairApi.pairCancel()).resolves.toBeUndefined();
  });

  it('still parses the pairing code from pair/start', async () => {
    answer(JSON.stringify({ code: 'ABC123', expiresAt: '2026-11-12T05:00:00Z' }), { status: 200 });
    await expect(cloudPairApi.pairStart()).resolves.toEqual({ code: 'ABC123', expiresAt: '2026-11-12T05:00:00Z' });
  });

  it('reports a refusal with its status, so the panel can tell a relayed request apart', async () => {
    answer('disconnect is local-only\n', { status: 403 });
    await expect(cloudPairApi.disconnect()).rejects.toMatchObject({ message: 'disconnect is local-only', status: 403 });
  });
});
