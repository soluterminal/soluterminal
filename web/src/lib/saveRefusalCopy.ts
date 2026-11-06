import { ApiError, currentRoutingScope } from '../api';
import { localProofStatus } from './localProof';
import { isEmbedded } from '../embed';

// The daemon refuses an edit from a screen it can't tie to a person with a 403
// human_required / device_required whose message speaks to agents (use the MCP
// tool). A person at a Shinoque Code window is already in the app, so they get
// the one step that helps: approve this screen in Settings.
const NOT_APPROVED_TO_SAVE =
  "This screen isn't approved to save files yet. Open Settings → Privacy & security → Ask to approve this browser.";
// Nothing is approved on this computer yet, and the frame can't approve its
// own first browser: that is done in a browser at the computer.
const NO_DEVICE_APPROVED_YET =
  "This screen isn't approved to save files yet, and no browser is approved on this computer. " +
  'Open SoluTerminal in a web browser on this computer, choose Settings → Privacy & security → Approve this browser, then come back and choose Ask to approve this browser.';

// Ask isn't offered while this browser's request is already waiting, nor when
// another machine is in view (Settings shows nothing there): approval then
// happens on a device that is already approved.
const APPROVE_FROM_APPROVED_DEVICE =
  "This screen isn't approved to save files yet. Approve it from a browser or app you've already approved, in Settings → Privacy & security.";

function approvalAdvice(): string {
  const scope = currentRoutingScope();
  if (scope.prefix !== '') return APPROVE_FROM_APPROVED_DEVICE;
  // Unknown standing (not loaded yet) gets the Ask line, the common case.
  const status = localProofStatus(scope.backendPrefix);
  // No approved device means nobody to approve a waiting request either.
  if (status && status.approvedDevices === 0) return NO_DEVICE_APPROVED_YET;
  return status?.device === 'pending' ? APPROVE_FROM_APPROVED_DEVICE : NOT_APPROVED_TO_SAVE;
}

function isApprovalRefusal(error: unknown): boolean {
  if (!(error instanceof ApiError) || error.status !== 403 || !isEmbedded()) return false;
  if (typeof error.body !== 'object' || error.body === null) return false;
  const { error: code, caller } = error.body as { error?: unknown; caller?: unknown };
  // A caller the daemon traced to a pane or worker can't be helped by
  // approving a screen: its own message stands.
  if (caller !== undefined && caller !== 'unverified') return false;
  return code === 'human_required' || code === 'device_required';
}

/** True for the refusal that needs a next step the person must read: its toast stays until dismissed or the person edits. */
export const isSaveApprovalRefusal = isApprovalRefusal;

/** The sentence to show for a failed line save; never raw JSON. */
export function saveFailureText(error: unknown): string {
  if (error instanceof ApiError) {
    const body = error.body;
    const parsed = typeof body === 'object' && body !== null
      ? (body as { error?: unknown; message?: unknown }) : {};
    if (isApprovalRefusal(error)) return approvalAdvice();
    if (typeof parsed.message === 'string' && parsed.message) return parsed.message;
  }
  return error instanceof Error ? error.message : String(error);
}
