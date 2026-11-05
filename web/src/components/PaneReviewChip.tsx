import { useEffect, useId, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import type { PaneInfo, PaneReview } from '../types';
import { formatAgo } from '../lib/turnTime';
import { api } from '../api';
import { useStore } from '../store';
import { useAnchoredCard } from './useAnchoredCard';
import { PaneAskList } from './PaneAskList';

// "Was this work reviewed, by which model, and what is still open?" — the
// latest solu-review result for a pane (daemon pane_review.go), as a compact
// header chip with a details card. Panes whose work was never reviewed show
// nothing.

const AGENT_NAMES: Record<string, string> = { claude: 'Claude', codex: 'Codex' };
const agentName = (k?: string) => (k ? AGENT_NAMES[k] ?? k : '');

/** Short chip text and tone for a review. */
export function reviewChip(r: PaneReview): { label: string; tone: 'ok' | 'warn' | 'muted' } {
  // Fixed after the last round: no longer waiting on anyone, but not
  // re-reviewed either, so not the approved green.
  if (r.followup && (r.verdict === 'request-changes' || r.verdict === 'comment')) return { label: 'Fixed after review', tone: 'muted' };
  switch (r.verdict) {
    case 'approve': return { label: '✓ Reviewed', tone: 'ok' };
    case 'request-changes': return { label: `${r.mustFix || 1} to fix`, tone: 'warn' };
    case 'comment': return { label: 'Needs your decision', tone: 'warn' };
    default: return { label: 'Review failed', tone: 'muted' };
  }
}

/** One line: "Codex reviewed Claude's work: 2 to fix". */
export function reviewSummary(r: PaneReview): string {
  const who = agentName(r.reviewer) || 'A reviewer';
  const whose = r.author ? ` ${agentName(r.author)}'s work` : '';
  switch (r.verdict) {
    case 'approve': return `${who} reviewed${whose}: approved`;
    case 'request-changes': return `${who} reviewed${whose}: ${r.mustFix} to fix`;
    case 'comment': return `${who} reviewed${whose}: needs your decision`;
    default: return 'The review could not run';
  }
}

export function PaneReviewChip({ pane }: { pane: PaneInfo }) {
  const r = pane.review;
  const [open, setOpen] = useState(false);
  const [clearError, setClearError] = useState('');
  const titleId = useId();
  const buttonRef = useRef<HTMLButtonElement>(null);
  const cardRef = useRef<HTMLDivElement>(null);
  const pos = useAnchoredCard(open, buttonRef, 360);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!cardRef.current?.contains(e.target as Node) && !buttonRef.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('click', onDown);
    return () => document.removeEventListener('click', onDown);
  }, [open]);

  if (!r) return null;
  const chip = reviewChip(r);
  const summary = reviewSummary(r);
  const crossModel = !!r.author && !!r.reviewer && r.author !== r.reviewer;
  const mustFix = (r.findings ?? []).filter((f) => f.blocking);
  const notes = (r.findings ?? []).filter((f) => !f.blocking);
  const close = () => {
    setOpen(false);
    buttonRef.current?.focus();
  };
  const clear = async () => {
    setClearError('');
    try {
      await api.clearPaneReview(pane.id);
      useStore.setState((s) => (s.panes[pane.id] ? { panes: { ...s.panes, [pane.id]: { ...s.panes[pane.id], review: undefined } } } : {}));
      setOpen(false);
    } catch (e) {
      setClearError((e as Error).message || 'Could not clear the result.');
    }
  };

  return (
    <span className="pane-review">
      <button
        ref={buttonRef}
        type="button"
        className={`pane-review-chip is-${chip.tone}`}
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-label={`${summary}. Show review`}
        title={summary}
        onClick={(e) => {
          e.stopPropagation();
          setOpen((v) => !v);
        }}
        onMouseDown={(e) => e.stopPropagation()}
        onDoubleClick={(e) => e.stopPropagation()}
      >
        {chip.label}
      </button>
      {open && pos && createPortal(
        <div
          ref={cardRef}
          className="pane-origin-card pane-review-card"
          style={{ top: pos.top, left: pos.left }}
          role="dialog"
          aria-labelledby={titleId}
          onClick={(e) => e.stopPropagation()}
          onMouseDown={(e) => e.stopPropagation()}
          onKeyDown={(e) => {
            e.stopPropagation();
            if (e.key === 'Escape') {
              e.preventDefault();
              close();
            }
          }}
        >
          <div id={titleId} className="pane-origin-card-title">{summary}</div>
          <dl className="pane-origin-card-rows">
            {r.title && (<><dt>Change</dt><dd>{r.title}</dd></>)}
            {r.branch && (<><dt>Branch</dt><dd>{r.branch}</dd></>)}
            {r.head && (<><dt>Reviewed commit</dt><dd><code>{r.head.slice(0, 12)}</code></dd></>)}
            <dt>Reviewer</dt>
            <dd>
              {agentName(r.reviewer) || 'unknown'}{r.model && r.model !== 'codex default' ? ` (${r.model})` : ''}
              {crossModel ? ' · a different model from the author' : r.author ? ' · same model as the author' : ''}
            </dd>
            {r.fallback && (<><dt>Note</dt><dd>{r.fallback}</dd></>)}
            <dt>Round</dt><dd>{r.round} of 2</dd>
            {r.at > 0 && (<><dt>When</dt><dd>{formatAgo(r.at)}</dd></>)}
          </dl>
          <p className="pane-review-summary">This is a local review of the recorded change. Before merging, check the pull request's current commit and required reviews.</p>
          {r.summary && <p className="pane-review-summary">{r.summary}</p>}
          {r.error && <p className="pane-review-summary">{r.error}</p>}
          {mustFix.length > 0 && (
            <>
              <div className="pane-review-heading">Must fix</div>
              <ul className="pane-review-list">
                {mustFix.map((f, i) => (
                  <li key={i}><span className={`pane-review-sev sev-${f.severity}`}>{f.severity}</span> {f.what}{f.where ? <span className="pane-review-where"> — {f.where}</span> : null}</li>
                ))}
              </ul>
            </>
          )}
          {(r.questions ?? []).length > 0 && (
            <>
              <div className="pane-review-heading">Questions for you</div>
              <ul className="pane-review-list">
                {(r.questions ?? []).map((q, i) => <li key={i}>{q}</li>)}
              </ul>
            </>
          )}
          {r.verdict === 'comment' && (pane.asks ?? []).length > 0 && (
            <>
              <div className="pane-review-heading">Asks waiting on you</div>
              <PaneAskList pane={pane} asks={pane.asks ?? []} />
            </>
          )}
          {notes.length > 0 && (
            <div className="pane-review-notes">{notes.length} optional note{notes.length === 1 ? '' : 's'} — the agent has the full list.</div>
          )}
          {r.followup && (
            <>
              <div className="pane-review-heading">Fixed after the review</div>
              <p className="pane-review-summary">
                {r.followup.note}
                <span className="pane-review-where"> — {formatAgo(r.followup.at)}; not re-reviewed (the protocol stops at two rounds)</span>
              </p>
            </>
          )}
          {clearError && <p className="pane-review-summary" role="alert">{clearError}</p>}
          <div className="pane-origin-card-actions">
            <button
              type="button"
              onClick={() => { void clear(); }}
              title="Remove this result from the pane. The review record and GitHub's checks are not affected."
            >
              Clear
            </button>
            <button type="button" onClick={close} autoFocus>Close</button>
          </div>
        </div>,
        document.body,
      )}
    </span>
  );
}
