import type { Workspace } from '../types';

// Serialize this view's saves and advance bases only through revisions that
// this view itself committed. A revision from another window still conflicts.
export function createWorkspaceSaveQueue() {
  const tails = new Map<string, Promise<unknown>>();
  const revisions = new Map<string, Map<number, number>>();
  return (key: string, workspace: Workspace, send: (w: Workspace) => Promise<Workspace>): Promise<Workspace> => {
    const operation = (tails.get(key) ?? Promise.resolve()).catch(() => {}).then(async () => {
      const own = revisions.get(key) ?? new Map<number, number>();
      let base = workspace.layoutVersion ?? 0;
      while (own.has(base) && own.get(base)! > base) base = own.get(base)!;
      const saved = await send({ ...workspace, layoutVersion: base });
      if (typeof saved.layoutVersion === 'number' && saved.layoutVersion > base) {
        const original = workspace.layoutVersion ?? 0;
        own.delete(original); own.set(original, saved.layoutVersion);
        own.delete(base); own.set(base, saved.layoutVersion);
        while (own.size > 256) own.delete(own.keys().next().value!);
        revisions.set(key, own);
      }
      return saved;
    });
    tails.set(key, operation);
    void operation.finally(() => { if (tails.get(key) === operation) tails.delete(key); }).catch(() => {});
    return operation;
  };
}
