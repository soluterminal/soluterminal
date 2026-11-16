// Conversation-discontinuity notice (PaneInfo.switchNotice), shown above a
// terminal pane. Reason picks the copy: '' / absent = a renderer switch
// interrupted the turn; 'resume-repair' = a restart cut off the previous
// response (the resume synthetically answered a dangling tool_use);
// 'fresh-start' = a requested --resume was dropped (no transcript on disk /
// session live elsewhere) and the pane started a NEW conversation —
// informational, nothing to continue.
export interface SwitchNotice {
  interrupted: boolean;
  fromKind: 'native' | 'terminal';
  at: number;
  reason?: 'resume-repair' | 'fresh-start';
}

// Label for the kind switched AWAY from, as carried in switchNotice.fromKind
// ('terminal' | 'native'; 'native' was the retired Focus view).
export function fromKindLabel(fromKind: 'native' | 'terminal'): 'Terminal' | 'Focus' {
  return fromKind === 'native' ? 'Focus' : 'Terminal';
}

// Stable identity for a switchNotice, used to key the per-pane "dismissed" set
// so a dismissed marker doesn't reappear for the SAME notice (but a fresh
// one — a new `at` — shows again). Null/absent notice → null key.
export function switchNoticeKey(notice: SwitchNotice | null | undefined): number | null {
  return notice ? notice.at : null;
}

// Whether the notice banner should render: a live notice whose key the user
// hasn't dismissed in THIS session. Pure so the gate is testable.
export function shouldShowSwitchNotice(
  notice: SwitchNotice | null | undefined,
  dismissed: ReadonlySet<number>,
): boolean {
  const key = switchNoticeKey(notice);
  return key != null && !dismissed.has(key);
}

//   • Continue → sends a short "continue where you left off" turn (which clears
//     the daemon's notice on the next input). The caller wires the send.
//   • ✕ → dismisses locally (the caller keys the dismissed-set by switchNotice.at
//     so it doesn't reappear for the SAME notice).
export function SwitchInterruptedBanner({
  fromKind,
  reason,
  onContinue,
  onDismiss,
}: {
  fromKind: 'native' | 'terminal';
  reason?: 'resume-repair' | 'fresh-start';
  onContinue: () => void;
  onDismiss: () => void;
}) {
  const fromLabel = fromKindLabel(fromKind);
  const text =
    reason === 'resume-repair' ? (
      <>A restart interrupted the previous response.</>
    ) : reason === 'fresh-start' ? (
      <>Couldn&rsquo;t resume the previous conversation — started a fresh session.</>
    ) : (
      <>
        Switched from <strong>{fromLabel}</strong> mid-turn — the previous response was interrupted.
      </>
    );
  return (
    <div className="native-interrupted-banner" role="status">
      <span className="native-interrupted-glyph" aria-hidden>
        ↩
      </span>
      <span className="native-interrupted-text">{text}</span>
      {reason !== 'fresh-start' && (
        <button
          type="button"
          className="native-interrupted-continue"
          title="Send “Continue where you left off.” to resume"
          onClick={(e) => {
            e.stopPropagation();
            onContinue();
          }}
        >
          Continue
        </button>
      )}
      <button
        type="button"
        className="native-interrupted-dismiss"
        aria-label="Dismiss interrupted notice"
        title="Dismiss"
        onClick={(e) => {
          e.stopPropagation();
          onDismiss();
        }}
      >
        ✕
      </button>
    </div>
  );
}
