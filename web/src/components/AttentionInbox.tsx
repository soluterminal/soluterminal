import { isPromptForUser } from '../lib/paneStatus';
import { useMemo } from 'react';
import { useStore, ariaObservationFingerprint, ARIA_DISMISSAL_TTL_MS, type AriaTail } from '../store';
import { isActionable } from '../lib/actionability';
import type { PaneInfo } from '../types';

// Cross-workspace attention surface — the SINGLE SOURCE OF TRUTH for
// "which panes need the user's attention right now". The topbar bell and
// the sidebar collapse button consume `useAttentionItems()`, so the count
// and choice-prompt state stay in lockstep across surfaces by construction.
//
// Inputs joined here:
//   - `panes` — live state from /api/panes polling, source of truth
//                 for "is this pane currently in attention".
//   - `paneTails` — latest tail captured by daemon SSE +
//                 mount-bootstrap; provides the question text for
//                 fingerprinting + ObservationRow rendering.
//   - `ariaDismissedFingerprints` — paneId+fingerprint → dismissal ts;
//                 pane is hidden until the question changes or TTL
//                 expires (10 min).
//
// Output: array of AttentionItem, one per pane in attention state.
// Includes a `dismissed` flag so consumers can choose to render
// dismissed items differently (e.g. greyed) — bell + panel both filter
// them out by default.
//
// Scope: active-backend panes only. Cross-backend aggregation would
// need additional fetch plumbing (the other backend's panes aren't in
// `s.panes`); deferred until the pattern proves out.

const CURRENT_WS = '_current';

function shortRelative(ms: number): string {
  if (!ms) return '';
  const s = Math.max(0, Math.floor((Date.now() - ms) / 1000));
  if (s < 5) return 'now';
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

export interface AttentionItem {
  pane: PaneInfo;
  /** Latest captured tail for this pane (from SSE or bootstrap).
   *  Undefined if no daemon observation has arrived yet — pane is in
   *  attention state per /api/panes but the watcher hasn't emitted /
   *  the bootstrap hasn't completed. Bell still surfaces these so the
   *  user isn't blind during the brief gap. */
  tail?: AriaTail;
  /** Fingerprint of `tail.text`. Undefined when tail is. */
  fingerprint?: string;
  /** True if the user dismissed the current question. Items with this
   *  flag are filtered out by default — pass `includeDismissed: true`
   *  to surface them (e.g. for an "undo" affordance). */
  dismissed: boolean;
  workspaceName: string | null;
  /** 'unsent': a message another pane sent is still sitting in this pane's
   *  input box (Send/Clear on the pane header). */
  kind: 'choice' | 'awaiting' | 'unsent';
  /** Daemon's explanation of WHY the human is needed, when it has one
   *  ("AskUserQuestion needs your answer…", "won't auto-approve —
   *  touches secrets", a cautious-mode decline). Undefined for plain
   *  no-auto-policy prompts. */
  reason?: string;
  /** LLM-graded one-liner for the current prompt, when the async
   *  summarizer has run ("wants to run a prod DB migration"). */
  summary?: string;
  /** low | medium | high blast-radius grade accompanying `summary`. */
  risk?: string;
  /** Suggested next step accompanying `summary`. */
  action?: string;
}

// Shared selector. Memoized on the inputs so Bell + Panel + Sidebar
// renders don't recompute on unrelated store changes.
export function useAttentionItems(opts: { includeDismissed?: boolean } = {}): AttentionItem[] {
  const panes = useStore((s) => s.panes);
  const workspaces = useStore((s) => s.workspaces);
  const tails = useStore((s) => s.paneTails);
  const dismissed = useStore((s) => s.ariaDismissedFingerprints);
  const includeDismissed = !!opts.includeDismissed;

  return useMemo(() => {
    const ownerByPane = new Map<string, string>();
    for (const w of workspaces) {
      if (w.name === CURRENT_WS) continue;
      for (const id of w.paneIds ?? []) {
        if (!ownerByPane.has(id)) ownerByPane.set(id, w.name);
      }
    }
    const now = Date.now();
    const items: AttentionItem[] = [];
    // A stuck message is listed whenever the pane has no prompt row of its
    // own (no prompt, or one that is dismissed or not actionable).
    const pushUnsent = (p: PaneInfo) => {
      if (!p.unsentMessage) return;
      items.push({
        pane: p, dismissed: false, workspaceName: ownerByPane.get(p.id) ?? null, kind: 'unsent',
        summary: `Message from ${p.unsentMessage.sender} not sent — Send or Clear it on the pane`,
      });
    };
    for (const p of Object.values(panes)) {
      if (p.status !== 'running') continue;
      if (!isPromptForUser(p)) {
        pushUnsent(p);
        continue;
      }
      const tail = tails[p.id];
      const fingerprint = tail ? ariaObservationFingerprint(tail.text) : undefined;
      let isDismissed = false;
      if (fingerprint) {
        const at = dismissed[`${p.id}::${fingerprint}`];
        if (at && now - at < ARIA_DISMISSAL_TTL_MS) isDismissed = true;
      }
      if (isDismissed && !includeDismissed) {
        pushUnsent(p);
        continue;
      }
      // Actionability gate: hide rows where the tail is just Claude
      // idle / chrome / prose with no real prompt. Keeps bell + panel
      // counts identical because both surfaces consume this filtered
      // list — without this the panel would hide non-actionable rows
      // inside ObservationRow but the bell would still count them.
      if (tail && !isActionable(tail.text)) {
        pushUnsent(p);
        continue;
      }
      items.push({
        pane: p,
        tail,
        fingerprint,
        dismissed: isDismissed,
        workspaceName: ownerByPane.get(p.id) ?? null,
        kind: p.state === 'choice-prompt' ? 'choice' : 'awaiting',
        reason: p.attentionReason,
        summary: p.attentionSummary,
        risk: p.attentionRisk,
        action: p.attentionAction,
      });
    }
    // Choice prompts are more urgent than passive awaiting-input; within a
    // kind, graded risk sorts high → low (a prod migration outranks a Read),
    // then newest-activity first.
    const riskWeight = (r?: string) => (r === 'high' ? 3 : r === 'medium' ? 2 : r === 'low' ? 1 : 0);
    const kindWeight = { choice: 0, awaiting: 1, unsent: 2 } as const;
    items.sort((a, b) => {
      if (a.kind !== b.kind) return kindWeight[a.kind] - kindWeight[b.kind];
      const rw = riskWeight(b.risk) - riskWeight(a.risk);
      if (rw !== 0) return rw;
      return (b.pane.lastActivityMs ?? 0) - (a.pane.lastActivityMs ?? 0);
    });
    return items;
  }, [panes, workspaces, tails, dismissed, includeDismissed]);
}

export function useAttentionBadge(): { count: number; hasChoice: boolean; firstPaneId: string | null } {
  const panes = useStore((s) => s.panes);
  const tails = useStore((s) => s.paneTails);
  const dismissed = useStore((s) => s.ariaDismissedFingerprints);

  return useMemo(() => {
    const now = Date.now();
    let count = 0;
    let hasChoice = false;
    let firstPaneId: string | null = null;
    const countUnsent = (p: PaneInfo) => {
      if (!p.unsentMessage) return;
      count++;
      if (firstPaneId === null) firstPaneId = p.id;
    };
    for (const p of Object.values(panes)) {
      if (p.status !== 'running') continue;
      // Same gates as useAttentionItems, so the bell count and the panel
      // rows stay mathematically identical.
      if (!isPromptForUser(p)) {
        countUnsent(p);
        continue;
      }
      const tail = tails[p.id];
      const fingerprint = tail ? ariaObservationFingerprint(tail.text) : undefined;
      if (fingerprint) {
        const at = dismissed[`${p.id}::${fingerprint}`];
        if (at && now - at < ARIA_DISMISSAL_TTL_MS) {
          countUnsent(p);
          continue;
        }
      }
      if (tail && !isActionable(tail.text)) {
        countUnsent(p);
        continue;
      }
      count++;
      if (firstPaneId === null) firstPaneId = p.id;
      if (p.state === 'choice-prompt') hasChoice = true;
    }
    return { count, hasChoice, firstPaneId };
  }, [panes, tails, dismissed]);
}

export function AttentionInbox({
  items,
  onSelect,
}: {
  items: AttentionItem[];
  // Called with the clicked pane's id so consumers can dismiss the
  // row locally (a user already looking at the pane doesn't want
  // continued nagging while they think about what to type).
  onSelect?: (paneId: string) => void;
}) {
  const revealPane = useStore((s) => s.revealPane);
  if (items.length === 0) return null;
  return (
    <div className="attention-inbox-list">
      {items.map((it) => {
        const title = it.pane.title || it.pane.cmd.join(' ') || it.pane.id.slice(-6);
        const wsLabel = it.workspaceName ?? 'background';
        const age = shortRelative(it.pane.lastActivityMs ?? 0);
        const kindLabel = it.kind === 'choice' ? 'approval needed' : it.kind === 'unsent' ? 'message not sent' : 'waiting for input';
        const hoverDetail = it.summary
          ? `${it.summary}${it.action ? `\n→ ${it.action}` : ''}`
          : kindLabel;
        return (
          <div
            key={it.pane.id}
            className={`attention-row attention-${it.kind}`}
            title={`${title} · ${wsLabel} · ${hoverDetail} · ${age}`}
            onClick={() => {
              void revealPane(it.pane.id);
              onSelect?.(it.pane.id);
            }}
          >
            <span className={`ws-badge ws-badge-${it.kind}`} aria-hidden />
            <span className="attention-main">
              <span className="attention-title-line">
                <span className="attention-title">{title}</span>
                <span className="attention-ws">{wsLabel}</span>
                <span className="attention-age">{age}</span>
              </span>
              {it.summary && (
                <span className="attention-summary-line">
                  {it.risk && <span className={`attention-risk attention-risk-${it.risk}`}>{it.risk}</span>}
                  <span className="attention-summary">{it.summary}</span>
                </span>
              )}
            </span>
          </div>
        );
      })}
    </div>
  );
}
