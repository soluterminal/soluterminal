import { useEffect, useState } from 'react';
import { api } from '../api';
import { useCloud } from '../cloud';
import { useAccountFeatures, useEntitlements } from '../entitlements';
import { connectThisComputerAfterWarning } from '../accountConnect';

// "Approve with caution" asks Jev whether a prompt is low-risk. With a Jev
// key saved on this computer the daemon calls it directly; otherwise the
// call goes through the cloud on a free account. This one line tells the
// user which applies and how much of today's allowance is left, or what
// turns it on (signing in, or connecting this computer).

export const SMART_APPROVALS_SIGN_IN = 'Sign in (free) to use smart approvals';
export const SMART_APPROVALS_FREE = 'Smart approvals: via your free account (100/day)';
export const SMART_APPROVALS_TEAM = 'Smart approvals: via your account';
export const SMART_APPROVALS_CONNECT = 'Connect this computer to use smart approvals';
export const SMART_APPROVALS_USED_UP = "Smart approvals: today's are used up (they reset tomorrow)";
/** After Connect from the hint (wording item 21): connecting makes them
 *  available; it doesn't switch anything on. */
export const SMART_APPROVALS_CONNECTED =
  'This computer is connected to your account. You can use smart approvals in a few seconds.';

export function smartApprovalsLeft(remaining: number, limit?: number): string {
  return `Smart approvals: ${remaining} of ${limit ?? 100} left today`;
}

type SmartStatus = Awaited<ReturnType<typeof api.smartApprovalsStatus>>;

const LOCAL_KEY_TTL_MS = 60_000;
let localKeyCache: { at: number; value: Promise<boolean> } | null = null;

// Whether the daemon holds its own Jev key. Any failure (older daemon, the
// workflow engine down) reads as "no key": the hint then points at the
// account path, which is the safe advice.
export function hasLocalJevKey(): Promise<boolean> {
  const now = Date.now();
  if (localKeyCache && now - localKeyCache.at < LOCAL_KEY_TTL_MS) return localKeyCache.value;
  const value = api
    .workflowRuntimeSecrets('typesafe-api')
    .then((r) => r.secrets.some((s) => s.env === 'TYPESAFE_API_KEY' && s.configured))
    .catch(() => false);
  localKeyCache = { at: now, value };
  return value;
}

export function resetSmartApprovalsCacheForTests(): void {
  localKeyCache = null;
}

// Signing in swaps the whole app for the sign-in screen, so the menu that
// holds this line goes away with it.
export function SmartApprovalsHint() {
  const [localKey, setLocalKey] = useState<boolean | null>(null);
  // The daemon's own answer (source + allowance); null on older daemons,
  // which fall back to the key check alone.
  const [status, setStatus] = useState<SmartStatus | undefined>(undefined);
  const account = useAccountFeatures();
  const team = useEntitlements((s) => s.loaded && s.team);
  const cloudOff = useEntitlements((s) => s.smartApprovals === false);
  // Signing in is only possible where the sign-in screen exists (a daemon
  // that serves the account flow).
  const canSignIn = useCloud((s) => s.mode === 'cloud' && s.isLocal && s.accountOptional);
  const requestSignIn = useCloud((s) => s.requestSignIn);

  useEffect(() => {
    let mounted = true;
    void hasLocalJevKey().then((v) => {
      if (mounted) setLocalKey(v);
    });
    void api.smartApprovalsStatus().then((v) => {
      if (mounted) setStatus(v);
    });
    return () => {
      mounted = false;
    };
  }, []);

  if (localKey !== false || status === undefined || status?.source === 'local') return null;
  if (account) {
    if (cloudOff) return null;
    if (team) {
      return <div className="pane-ctx-menu-hint" role="note">{SMART_APPROVALS_TEAM}</div>;
    }
    // Signed in, but this computer isn't connected to the account yet: the
    // cloud only answers for a connected computer.
    if (status?.source === 'none') {
      return (
        <button
          type="button"
          role="menuitem"
          className="pane-ctx-menu-hint-action"
          onClick={() => { void connectThisComputerAfterWarning(SMART_APPROVALS_CONNECTED); }}
        >
          {SMART_APPROVALS_CONNECT}
        </button>
      );
    }
    let text = SMART_APPROVALS_FREE;
    if (status?.paused === 'cloud_quota' || status?.remaining === 0) text = SMART_APPROVALS_USED_UP;
    else if (typeof status?.remaining === 'number') text = smartApprovalsLeft(status.remaining, status.limit);
    return (
      <div className="pane-ctx-menu-hint" role="note">
        {text}
      </div>
    );
  }
  if (!canSignIn) return null;
  return (
    <button
      type="button"
      role="menuitem"
      className="pane-ctx-menu-hint-action"
      onClick={requestSignIn}
    >
      {SMART_APPROVALS_SIGN_IN}
    </button>
  );
}
