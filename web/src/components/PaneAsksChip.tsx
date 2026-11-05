import { useEffect, useId, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import type { PaneAsk, PaneInfo } from '../types';
import { PaneAskList, useDismissAsk } from './PaneAskList';
import { useAnchoredCard } from './useAnchoredCard';

/** The pane's open asks to the user (daemon asks.go), in the header: the ask
 *  itself when there is one, else how many. Click opens every ask in full with
 *  the bell's actions (Dismiss, open the pane); Dismiss is the person's (the
 *  agent resolves its own). Plain text only. */
export function PaneAsksChip({ pane }: { pane: PaneInfo }) {
  // The open card lives in the inner component, so it goes with the badge when
  // the last ask clears instead of reopening for the next one.
  return (pane.asks?.length ?? 0) > 0 ? <AsksChip pane={pane} asks={pane.asks ?? []} /> : null;
}

function AsksChip({ pane, asks }: { pane: PaneInfo; asks: PaneAsk[] }) {
  const [open, setOpen] = useState(false);
  const { pendingId, dismiss } = useDismissAsk();
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

  const one = asks.length === 1 ? asks[0] : null;
  const label = one ? `Waiting on you: ${one.text}` : `${asks.length} asks waiting on you`;
  const close = () => {
    setOpen(false);
    buttonRef.current?.focus();
  };
  return <span className="pane-unsent-message pane-asks-chip" role="status"
    title={asks.map((a) => `• ${a.text}`).join('\n')}
    onClick={(e) => e.stopPropagation()} onDoubleClick={(e) => e.stopPropagation()}>
    <button ref={buttonRef} type="button" className="pane-asks-chip-toggle pane-unsent-message-label"
      aria-haspopup="dialog" aria-expanded={open}
      onClick={() => setOpen((v) => !v)} onMouseDown={(e) => e.stopPropagation()}>{label}</button>
    {one && <>{' — '}<button type="button" disabled={!!pendingId} title="Dismiss this ask" onClick={() => void dismiss(one.id)}>Dismiss</button></>}
    {open && pos && createPortal(
      <div ref={cardRef} className="pane-origin-card pane-asks-card" style={{ top: pos.top, left: pos.left }}
        role="dialog" aria-labelledby={titleId}
        onClick={(e) => e.stopPropagation()} onMouseDown={(e) => e.stopPropagation()}
        onKeyDown={(e) => {
          e.stopPropagation();
          if (e.key === 'Escape') {
            e.preventDefault();
            close();
          }
        }}>
        <div id={titleId} className="pane-origin-card-title">{one ? 'Waiting on you' : `${asks.length} asks waiting on you`}</div>
        <PaneAskList pane={pane} asks={asks} />
        <div className="pane-origin-card-actions"><button type="button" onClick={close} autoFocus>Close</button></div>
      </div>,
      document.body,
    )}
  </span>;
}
