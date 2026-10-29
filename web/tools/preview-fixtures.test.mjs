import { afterEach, describe, expect, it, vi } from 'vitest';
import { buildPreviewFixtures } from './preview-fixtures.mjs';

afterEach(() => { vi.unstubAllGlobals(); vi.resetModules(); });

describe('isolated preview read contract', () => {

  it('answers the reads the app shell makes at boot as an empty install', () => {
    const fixtures = buildPreviewFixtures('0.0.1');
    for (const read of ['/api/agents', '/api/agents/reports', '/api/schedules']) expect(fixtures[read]).toEqual([]);
    expect(fixtures).not.toHaveProperty('/api/todos');
    expect(fixtures['/api/orchestrator-hosts']).toEqual({ policyVersion: '', hosts: [] });
  });

  it('reports exact missing read paths without queries, and does not confuse denied writes with missing fixtures', async () => {
    const { createPreviewFetch } = await import('../src/previewBoot');
    const missing = vi.fn();
    const network = vi.fn();
    const fetch = createPreviewFetch(network, 'http://192.0.2.1/preview/test/', { offline: true, onMissingRead: missing });
    expect((await fetch('/api/new-settings?private=value')).status).toBe(404);
    expect(missing).toHaveBeenCalledExactlyOnceWith('/api/new-settings');
    expect((await fetch('/api/new-settings', { method: 'POST' })).status).toBe(403);
    expect(missing).toHaveBeenCalledTimes(1);
    expect(network).not.toHaveBeenCalled();
  });
});
