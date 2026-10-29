import { useEffect, useRef } from 'react';
import { useStore } from '../store';
import { paneScreen, sendMenuKey } from '../lib/menuKeys';

// Phone answer row for a terminal pane at `choice-prompt`. It renders inside
// MobileQuickReply's bar, above the reply field, so the bar's measured
// height (--quick-reply-h) already reserves room for it under the terminal
// and lifts the key bar above it.
//
// Like the desktop ApprovalActions strip it stays key-literal: menus differ
// (Claude's option 1 is not always "Yes"), so buttons name the key and only
// the meaning every menu shares. Keys go through the same per-session sender
// with the caller's gesture guard, as typed keys would.

interface Key {
  label: string;
  // Dropped on very narrow phones (CSS), so the row still fits at 320px.
  meaning?: string;
  bytes: string;
  name: string;
  kind?: 'danger';
}

const KEYS: Key[] = [
  { label: '1', bytes: '1', name: 'Press 1 — menu option 1' },
  { label: '2', bytes: '2', name: 'Press 2 — menu option 2' },
  { label: '3', bytes: '3', name: 'Press 3 — menu option 3' },
  { label: '⏎', meaning: ' Enter', bytes: '\r', name: 'Press Enter — confirm the highlighted option' },
  { label: 'Esc', meaning: ' · Cancel', bytes: '\x1b', name: 'Press Esc — cancel', kind: 'danger' },
];

// A second tap this soon after the first is treated as a bounce: the menu
// usually has not repainted yet, so it would answer whatever comes next.
export const MOBILE_APPROVAL_DEBOUNCE_MS = 500;

export function MobileApprovalBar({ sessionKey, capture }: {
  sessionKey: string;
  // Returns a validity check bound to this tap's target, process and view.
  capture: () => () => boolean;
}) {
  const sender = useStore((s) => s.senders[sessionKey]);
  const lastTap = useRef(0);
  useEffect(() => {
    if (!sender) return;
    document.body.classList.add('mobile-approval-visible');
    return () => document.body.classList.remove('mobile-approval-visible');
  }, [sender]);
  if (!sender) return null;
  const press = (key: Key) => {
    const now = Date.now();
    if (now - lastTap.current < MOBILE_APPROVAL_DEBOUNCE_MS) return;
    const valid = capture();
    if (!valid()) return;
    lastTap.current = now;
    const send = (bytes: string) => valid() && useStore.getState().senders[sessionKey] === sender && sender(bytes);
    sendMenuKey(send, key.bytes, () => paneScreen(useStore.getState().paneSnapshots[sessionKey]));
  };
  return (
    <div className="mobile-approval-bar" role="group" aria-label="Answer terminal prompt">
      {KEYS.map((k) => (
        <button
          key={k.label}
          type="button"
          className={`mobile-approval-key${k.kind ? ` ${k.kind}` : ''}`}
          aria-label={k.name}
          title={k.name}
          onMouseDown={(e) => e.preventDefault()}
          onClick={(e) => { e.stopPropagation(); press(k); }}
        >
          {k.label}
          {k.meaning && <span className="mobile-approval-meaning">{k.meaning}</span>}
        </button>
      ))}
    </div>
  );
}
