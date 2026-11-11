import { useEffect, useSyncExternalStore } from 'react';
import { api, type FeedbackReport } from '../api';

// "My reports" (ST-162): the person's own reports with their live status, from
// GET /api/feedback/mine. That route reads GitHub once per report, so it is
// fetched when the Inbox opens and after a send, never on the ambient poll.

let reports: FeedbackReport[] = [];
const listeners = new Set<() => void>();
let inflight: Promise<void> | null = null;

/** Fetch the list now; a call while one is in flight shares it. */
export function refreshMyReports(): Promise<void> {
  if (inflight) return inflight;
  inflight = api.feedbackMine()
    .then((res) => {
      const next = res.reports ?? [];
      if (JSON.stringify(next) !== JSON.stringify(reports)) {
        reports = next;
        listeners.forEach((l) => l());
      }
    })
    // Older daemon, signed out or offline: keep what was shown.
    .catch(() => {})
    .finally(() => { inflight = null; });
  return inflight;
}

function subscribe(l: () => void): () => void {
  listeners.add(l);
  return () => { listeners.delete(l); };
}

/** The list, fetched once each time a consumer mounts (the Inbox opening). */
export function useMyReports(): FeedbackReport[] {
  const snap = useSyncExternalStore(subscribe, () => reports);
  useEffect(() => { void refreshMyReports(); }, []);
  return snap;
}

/** The status word a row and its thread show (wording item 19), or null when
 *  the status can't be read. "Fixed in 0.0.N" only once 0.0.N is public: the
 *  cloud sends fixedIn only then, and a merged fix still reads "Looking into
 *  it". Both closed reasons read "Closed"; the message says which. */
export function reportStatusWord(r: Pick<FeedbackReport, 'status' | 'fixedIn'>): string | null {
  switch (r.status) {
    case 'received': return 'Received';
    case 'triaged':
    case 'fix-merged': return 'Looking into it';
    case 'needs-info': return 'Needs info';
    case 'known': return 'Known problem';
    case 'answered': return 'Answered';
    case 'fixed': return r.fixedIn ? `Fixed in ${r.fixedIn}` : 'Looking into it';
    case 'not-planned':
    case 'cant-reproduce':
    case 'closed': return 'Closed';
    default: return null;
  }
}

// Test hook.
export function __resetMyReportsForTests(next: FeedbackReport[] = []): void {
  reports = next;
  inflight = null;
  listeners.forEach((l) => l());
}
