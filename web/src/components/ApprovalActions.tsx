import { useStore } from '../store';

// Inline approval strip rendered above the terminal body when a pane is
// in `choice-prompt` (Claude has a numbered menu up). One-tap sends the
// corresponding key without the user having to focus the pane and type.
//
// Scope: desktop. Phones answer from MobileApprovalBar (inside the mobile
// quick-reply bar); while it is up, CSS hides this strip so the same keys
// are not offered twice.
//
// The strip deliberately stays key-literal (1/2/3/Y/N/Enter/Esc) rather
// than trying to parse Claude's menu text out of scrollback. Menu
// formats vary (numbered, y/N, (Enter to confirm)), xterm's buffer is
// canvas-rendered with no reliable text API, and a best-effort parser
// would be a failure mode. Showing raw keys matches what the user
// would type anyway.
//
// The keyboard path is typing the corresponding key in the terminal itself.
// Pane.tsx deliberately forwards Tab to the PTY, so this row is a pointer
// affordance rather than a second keyboard-focus path. Clicking it must not
// steal the caret from xterm's hidden textarea.

interface Action {
  label: string;
  bytes: string;
  title: string;
  kind?: 'default' | 'danger';
}

const ACTIONS: Action[] = [
  { label: '1', bytes: '1', title: '1 — first choice', kind: 'default' },
  { label: '2', bytes: '2', title: '2 — second choice', kind: 'default' },
  { label: '3', bytes: '3', title: '3 — third choice', kind: 'default' },
  { label: 'Y', bytes: 'y', title: 'y — yes', kind: 'default' },
  { label: 'N', bytes: 'n', title: 'n — no', kind: 'danger' },
  { label: '⏎', bytes: '\r', title: 'Enter — confirm default', kind: 'default' },
  { label: 'Esc', bytes: '\x1b', title: 'Escape — cancel', kind: 'danger' },
];

export function ApprovalActions({ paneId, sessionKey, actionGuard = ()=>true }: { paneId: string; sessionKey?:string; actionGuard?:()=>boolean }) {
  const sender = useStore((s) => s.senders[sessionKey ?? paneId]);
  if (!sender) return null;
  return (
    <div className="approval-actions" role="group" aria-label="Approval actions">
      <span className="approval-actions-label">Reply:</span>
      {ACTIONS.map((a) => (
        <button
          key={a.label}
          type="button"
          className={`approval-action ${a.kind === 'danger' ? 'danger' : ''}`}
          title={a.title}
          aria-label={a.title}
          onMouseDown={(e) => e.preventDefault()}
          onClick={(e) => {
            e.stopPropagation();
            if(!actionGuard())return;
            sender(a.bytes);
          }}
        >
          {a.label}
        </button>
      ))}
    </div>
  );
}
