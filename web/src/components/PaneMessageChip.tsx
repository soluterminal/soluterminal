import { useEffect, useState } from 'react';
import type { PaneInfo } from '../types';
import { useStore } from '../store';
import { formatAgo } from '../lib/turnTime';

// "Another agent just talked to this pane": the last message an agent pane
// delivered here (daemon pane_message_receipt.go), shown for a few minutes.
// It never changes who the pane belongs to; clicking goes to the sender.

export const MESSAGE_CHIP_MS = 10 * 60_000;

export function PaneMessageChip({ pane, now = Date.now }: { pane: PaneInfo; now?: () => number }) {
  const m = pane.lastAgentMessage;
  const revealPane = useStore((s) => s.revealPane);
  const senderOpen = useStore((s) => !!(m && s.panes[m.fromPaneId]));
  const [, tick] = useState(0);
  const left = m ? m.at + MESSAGE_CHIP_MS - now() : 0;
  useEffect(() => {
    if (left <= 0) return;
    const t = setTimeout(() => tick((n) => n + 1), left + 50);
    return () => clearTimeout(t);
  }, [left]);
  if (!m || left <= 0) return null;
  const note = m.outcome === 'sent_unconfirmed' ? ' (delivery not confirmed)' : m.outcome === 'queued' ? ' (queued until its turn ends)' : '';
  const detail = `Message from ${m.fromLabel} ${formatAgo(m.at)}${note}`;
  return (
    <button
      type="button"
      className="pane-message-chip"
      title={senderOpen ? `${detail} — go to ${m.fromLabel}` : detail}
      aria-label={detail}
      disabled={!senderOpen}
      onClick={(e) => {
        e.stopPropagation();
        void revealPane(m.fromPaneId);
      }}
      onMouseDown={(e) => e.stopPropagation()}
      onDoubleClick={(e) => e.stopPropagation()}
    >
      ✉ {m.fromLabel}
    </button>
  );
}
