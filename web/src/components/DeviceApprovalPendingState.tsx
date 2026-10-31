// Behind the approval prompt (DeviceApprovalPrompt) while the computer only
// accepts devices a person approved there. The first load failed because this
// browser isn't approved yet, not because the daemon is down, so the
// daemon-troubleshooting screen (BootstrapErrorState) would send the person
// after the wrong cause.
export const APPROVAL_PENDING_TITLE = 'Waiting for this browser to be approved';
export const APPROVAL_PENDING_DETAIL =
  'This computer only accepts devices approved on it. On the computer, open Settings › Privacy & security, check that ' +
  'the code matches the one shown here, and approve this browser. The page reloads once it is approved.';

export function DeviceApprovalPendingState({ onRetry }: { onRetry: () => void | Promise<void> }) {
  return (
    <div className="bootstrap-error-state" role="status" aria-live="polite">
      <div className="bootstrap-error-card">
        <div className="bootstrap-error-kicker">Approval needed</div>
        <h2>{APPROVAL_PENDING_TITLE}</h2>
        <p>{APPROVAL_PENDING_DETAIL}</p>
        <div className="bootstrap-error-actions">
          <button type="button" className="primary" onClick={() => void onRetry()}>
            Try again
          </button>
        </div>
      </div>
    </div>
  );
}
