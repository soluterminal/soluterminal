import { useEffect, useSyncExternalStore } from 'react';
import { api, type FeedbackEvent } from '../api';

// Shared poll for the feedback-inbox events — module singleton so the footer
// chip and the inbox feed read ONE fetch, not one each (the useAgentsStatus
// pattern, minus the cloud fan-out). The endpoint is pure-DB on the cloud and
// returns a calm empty set on unpaired machines, so ambient polling is cheap
// and safe. Poll only while some consumer is mounted and the tab is visible.

const POLL_MS = 120_000;

let events: FeedbackEvent[] = [];
let listeners = new Set<() => void>();
let timer: ReturnType<typeof setInterval> | null = null;
let inflight = false;
// A refresh asked for while one is in flight: run once more when it lands, so
// a list fetched before a read-stamp reached the cloud doesn't stand for a
// whole poll interval.
let again = false;

async function refresh() {
  if (document.hidden) return;
  if (inflight) {
    again = true;
    return;
  }
  inflight = true;
  try {
    const res = await api.feedbackInbox();
    const next = res.events ?? [];
    // Referential stability: only notify when the payload actually changed,
    // so the always-mounted consumers don't re-render every poll.
    if (JSON.stringify(next) !== JSON.stringify(events)) {
      events = next;
      listeners.forEach((l) => l());
    }
  } catch {
    // Old daemon (no route) / cloud hiccup: keep the last-known list. An
    // inbox that briefly shows stale feedback rows beats one that flickers.
  } finally {
    inflight = false;
    if (again) {
      again = false;
      void refresh();
    }
  }
}

function subscribe(l: () => void): () => void {
  listeners.add(l);
  if (!timer) {
    void refresh();
    timer = setInterval(() => void refresh(), POLL_MS);
  }
  return () => {
    listeners.delete(l);
    if (listeners.size === 0 && timer) {
      clearInterval(timer);
      timer = null;
    }
  };
}

export function useFeedbackInbox(): FeedbackEvent[] {
  const snap = useSyncExternalStore(subscribe, () => events);
  // Catch up when the tab becomes visible again (the interval skips hidden).
  useEffect(() => {
    const onVis = () => { if (!document.hidden) void refresh(); };
    document.addEventListener('visibilitychange', onVis);
    return () => document.removeEventListener('visibilitychange', onVis);
  }, []);
  return snap;
}

// A report this app just sent: its "Received" is not news to the person who
// sent it. Stamp it read on the cloud, then fetch, so the row shows up in the
// feed now and never as unread.
export function noteFeedbackSent(number: number): void {
  void api.feedbackInboxRead(number).catch(() => {}).then(() => refresh());
}

// Optimistic local read-stamp: flips the row read immediately (and notifies
// subscribers) while the POST syncs the cloud; the next poll reconciles.
export function markFeedbackEventRead(number: number): void {
  const known = events.some((e) => e.number === number);
  let changed = false;
  events = events.map((e) => {
    if (e.number !== number || e.readAt) return e;
    changed = true;
    return { ...e, readAt: new Date().toISOString() };
  });
  if (changed) listeners.forEach((l) => l());
  const synced = api.feedbackInboxRead(number);
  // No local row to flip (a notification tap on a cold start, before the
  // first poll landed): pick the stamp up from the cloud once it is stored.
  if (!known && listeners.size > 0) void synced.then(() => refresh()).catch(() => {});
  else void synced.catch(() => {});
}

// Test hook: reset module state between tests.
export function __resetFeedbackInboxForTests(next: FeedbackEvent[] = []): void {
  events = next;
  again = false;
  listeners.forEach((l) => l());
}
