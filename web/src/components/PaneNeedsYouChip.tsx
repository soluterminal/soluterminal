import { useEffect, useId, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import type { PaneInfo } from '../types';
import { useStore } from '../store';
import { formatAgo } from '../lib/turnTime';
import { useAnchoredCard } from './useAnchoredCard';

const PROMPT_LINES = 14;

/** The last lines of a captured pane tail, blank edges trimmed. */
export function promptLines(text: string): string[] {
  const lines = text.split('\n').map((l) => l.replace(/\s+$/, ''));
  while (lines.length > 0 && lines[0] === '') lines.shift();
  while (lines.length > 0 && lines[lines.length - 1] === '') lines.pop();
  return lines.slice(-PROMPT_LINES);
}

/** The header's "needs you" badge when the daemon detected a prompt in the
 *  terminal (not a posted ask). Click says so and shows the prompt's lines as
 *  captured. Detection is the daemon's; this only displays it. */
export function PaneNeedsYouChip({ pane }: { pane: PaneInfo }) {
  // The open card lives in the inner component, so it goes with the badge when
  // attention clears instead of reopening for the next prompt.
  return pane.attention === 'needs-user' ? <NeedsYouChip pane={pane} /> : null;
}

function NeedsYouChip({ pane }: { pane: PaneInfo }) {
  const [open, setOpen] = useState(false);
  const tail = useStore((s) => s.paneTails[pane.id]);
  const titleId = useId();
  const buttonRef = useRef<HTMLButtonElement>(null);
  const cardRef = useRef<HTMLDivElement>(null);
  const pos = useAnchoredCard(open, buttonRef, 420);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!cardRef.current?.contains(e.target as Node) && !buttonRef.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('click', onDown);
    return () => document.removeEventListener('click', onDown);
  }, [open]);

  const close = () => {
    setOpen(false);
    buttonRef.current?.focus();
  };
  const choice = pane.state === 'choice-prompt';
  const lines = tail ? promptLines(tail.text) : [];
  const asks = pane.asks?.length ?? 0;
  return <span className="pane-needs-you-wrap" onClick={(e) => e.stopPropagation()} onDoubleClick={(e) => e.stopPropagation()}>
    <button ref={buttonRef} type="button" className="pane-needs-you"
      aria-haspopup="dialog" aria-expanded={open} title="Show what this pane is waiting for"
      onClick={() => setOpen((v) => !v)} onMouseDown={(e) => e.stopPropagation()}>needs you</button>
    {open && pos && createPortal(
      <div ref={cardRef} className="pane-origin-card pane-needs-you-card" style={{ top: pos.top, left: pos.left }}
        role="dialog" aria-labelledby={titleId}
        onClick={(e) => e.stopPropagation()} onMouseDown={(e) => e.stopPropagation()}
        onKeyDown={(e) => {
          e.stopPropagation();
          if (e.key === 'Escape') {
            e.preventDefault();
            close();
          }
        }}>
        <div id={titleId} className="pane-origin-card-title">{choice ? 'A choice prompt in the terminal' : 'The terminal is waiting for your input'}</div>
        <p className="pane-needs-you-source">Detected in the terminal output; no agent posted an ask for it.</p>
        <dl className="pane-origin-card-rows">
          {pane.attentionReason && (<><dt>Why you</dt><dd>{pane.attentionReason}</dd></>)}
          {pane.attentionSummary && (<><dt>Summary</dt><dd>{pane.attentionRisk ? `${pane.attentionRisk} risk: ` : ''}{pane.attentionSummary}</dd></>)}
          {pane.attentionAction && (<><dt>Suggested</dt><dd>{pane.attentionAction}</dd></>)}
        </dl>
        {lines.length > 0
          ? <>
            <pre className="pane-needs-you-lines" aria-label="Prompt lines">{lines.join('\n')}</pre>
            <div className="pane-needs-you-captured">Captured {formatAgo(tail!.ts)}</div>
          </>
          : <p className="pane-needs-you-source">The prompt's lines are not captured yet. Open the pane to read them.</p>}
        {asks > 0 && <p className="pane-needs-you-source">This pane also has {asks} posted {asks === 1 ? 'ask' : 'asks'}: use the asks badge.</p>}
        <div className="pane-origin-card-actions">
          <button type="button" onClick={() => void useStore.getState().revealPane(pane.id)}>Open pane</button>
          <button type="button" onClick={close} autoFocus>Close</button>
        </div>
      </div>,
      document.body,
    )}
  </span>;
}
