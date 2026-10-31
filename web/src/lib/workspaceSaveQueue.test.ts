import { expect, it } from 'vitest';
import type { Workspace } from '../types';
import { createWorkspaceSaveQueue } from './workspaceSaveQueue';
const workspace: Workspace = { name: 'Desk', paneIds: [], layout: null, activeAt: '', layoutVersion: 0 };

it('serializes overlapping saves and advances only through its own committed revisions', async () => {
  const enqueue = createWorkspaceSaveQueue(), bases: number[] = [];
  let canonical = 0;
  const send = async (w: Workspace) => {
    bases.push(w.layoutVersion!);
    expect(w.layoutVersion).toBe(canonical);
    return { ...w, layoutVersion: ++canonical };
  };
  await Promise.all([enqueue('desk', workspace, send), enqueue('desk', workspace, send)]);
  expect(bases).toEqual([0, 1]);
});
it('does not silently advance through another client\'s revision', async () => {
  const enqueue = createWorkspaceSaveQueue();
  await enqueue('desk', workspace, async w => ({ ...w, layoutVersion: 1 }));
  await expect(enqueue('desk', workspace, async w => {
    expect(w.layoutVersion).toBe(1); // the other window has independently committed revision 2
    throw new Error('409 stale layout');
  })).rejects.toThrow('409');
});

it('compresses hot queued bases while bounding older revision history', async () => {
  const enqueue = createWorkspaceSaveQueue();
  let version = 0;
  const results = await Promise.all(Array.from({ length: 400 }, () => enqueue('long-lived', workspace, async w => {
    expect(w.layoutVersion).toBe(version);
    return { ...w, layoutVersion: ++version };
  })));
  expect(results.at(-1)?.layoutVersion).toBe(400);
});
