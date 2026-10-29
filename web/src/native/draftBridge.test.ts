// Tests for the per-pane draft bridge that powers the message-action "edit &
// resend": setDraft must notify subscribed composers so an external push lands in
// the mounted textarea. Covers persistence, notify, per-pane isolation, and
// unsubscribe.

import { describe, it, expect, vi } from 'vitest';
import { getDraft, hasAnyUnsentDraft, setDraft, subscribeDraft, unsentDraftPaneId } from './useNativePane';

describe('draft bridge (edit & resend plumbing)', () => {
  it('persists and clears a draft', () => {
    setDraft('pane-a', 'hello');
    expect(getDraft('pane-a')).toBe('hello');
    setDraft('pane-a', '');
    expect(getDraft('pane-a')).toBe('');
  });

  it('notifies subscribers of an external push and stops after unsubscribe', () => {
    const seen: string[] = [];
    const unsub = subscribeDraft('pane-b', (v) => seen.push(v));
    setDraft('pane-b', 'edit me');
    expect(seen).toEqual(['edit me']);
    unsub();
    setDraft('pane-b', 'ignored');
    expect(seen).toEqual(['edit me']);
  });

  it('isolates notifications per pane', () => {
    const a = vi.fn();
    const b = vi.fn();
    const ua = subscribeDraft('pane-c', a);
    const ub = subscribeDraft('pane-d', b);
    setDraft('pane-c', 'x');
    expect(a).toHaveBeenCalledWith('x');
    expect(b).not.toHaveBeenCalled();
    ua();
    ub();
  });
});

// Drafts must survive the page reloads the user doesn't ask for — the
// "install when idle" / required-update swap and the stale-bundle self-heal
// both reload the webview, and a module map alone dies with it.
describe('draft survives a page reload (sessionStorage write-through)', () => {
  it('writes through on set and removes on clear', () => {
    setDraft('pane-ss', 'half-typed prompt');
    expect(sessionStorage.getItem('solu.native.draft.pane-ss')).toBe('half-typed prompt');
    setDraft('pane-ss', ''); // the send path — sent text must not be re-restored
    expect(sessionStorage.getItem('solu.native.draft.pane-ss')).toBeNull();
  });

  it('hydrates a fresh module load from sessionStorage (the post-swap page)', async () => {
    sessionStorage.setItem('solu.native.draft.pane-reload', 'typed before the swap');
    vi.resetModules(); // simulate the reload: fresh module state, same tab storage
    const fresh = await import('./useNativePane');
    expect(fresh.getDraft('pane-reload')).toBe('typed before the swap');
    fresh.setDraft('pane-reload', '');
    expect(sessionStorage.getItem('solu.native.draft.pane-reload')).toBeNull();
  });
});

// #739: a whitespace-only draft, or one left behind by a pane that has since
// closed, must not hold the unattended updates for the life of the app.
describe('unsent drafts that hold an update', () => {
  it('counts only drafts with text, and only for panes that still exist', () => {
    setDraft('pane-ws', '  \n\t');
    setDraft('pane-gone', 'typed, then the pane closed');
    expect(unsentDraftPaneId({ 'pane-ws': {} })).toBeUndefined();
    expect(hasAnyUnsentDraft({ 'pane-ws': {} })).toBe(false);
    expect(unsentDraftPaneId({})).toBeUndefined();
    expect(unsentDraftPaneId({ 'pane-gone': {} })).toBe('pane-gone');
    expect(hasAnyUnsentDraft({ 'pane-gone': {}, other: {} })).toBe(true);
    setDraft('pane-ws', '');
    setDraft('pane-gone', '');
    expect(unsentDraftPaneId({ 'pane-gone': {} })).toBeUndefined();
  });
});
