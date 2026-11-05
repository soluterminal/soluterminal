import { useEffect, useRef, useState } from 'react';
import { create } from 'zustand';
import { currentRoutingScope } from '../api';
import { askToApproveThisBrowser, localProofStatus, thisDeviceName, type LocalProofStatus } from '../lib/localProof';
import { useLocalProof } from '../lib/useLocalProof';
import { SITE_HELP_LOST_DEVICES_URL } from '../lib/siteLinks';
import { openInSystemBrowser } from '../tauri-browser';

// First run on a screen at this computer that isn't approved while others
// are (daemon human_proof.go): it can view, but acting for the person needs
// an approved device. The prompt only enrols this browser (V3, with its own
// key, lib/localProof.ts) and shows the code it computed itself; approving
// happens on an approved device. It never blocks the screen, can be put
// away, and comes back in this session only when the standing changes. With
// no approved device the first-device path in Settings is unchanged.

export const APPROVE_SCREEN_TITLE = "This screen isn't approved yet";
export const APPROVE_SCREEN_ASK = 'Ask to approve this browser';
const DISMISSED_KEY = 'soluterminal.approveScreenPromptDismissed';

/** The standing the prompt was put away for: shown again when it changes. */
function standingKey(s: LocalProofStatus): string {
  return `${s.target}|${s.device ?? ''}`;
}

function dismissedFor(): string | null {
  try { return sessionStorage.getItem(DISMISSED_KEY); } catch { return null; }
}

// Opened on purpose (the claim chip's "Approve this screen", ST-54): shown
// even when put away earlier or while no device is approved yet. It still only
// asks; approving stays on an approved device or at the computer.
const useOpenedOnPurpose = create<{ open: boolean }>(() => ({ open: false }));

/**
 * Show the prompt now. False when it has nothing to offer here: the view
 * isn't this computer's own daemon, its standing isn't known yet, or this
 * browser is already approved.
 */
export function openApproveThisScreenPrompt(): boolean {
  const scope = currentRoutingScope();
  const status = scope.prefix === '' ? localProofStatus(scope.backendPrefix) : null;
  if (!status || status.device === 'approved') return false;
  useOpenedOnPurpose.setState({ open: true });
  return true;
}

/** Where this screen's request can be approved, by the daemon's rule. */
export function whereToApprove(s: Pick<LocalProofStatus, 'effective'>): string {
  // Outside require, a browser traced at the computer approves too
  // (deviceManagementProven); under require only approved devices do.
  return s.effective === 'require'
    ? 'Approve it in Settings → Privacy & security → Remote devices on a browser or app you’ve already approved.'
    : 'Approve it in Settings → Privacy & security → Remote devices on a browser or app you’ve already approved, ' +
      'or on the computer itself in a browser SoluTerminal can tell apart from other programs.';
}

export function ApproveThisScreenPrompt() {
  const { status, backendPrefix, reload } = useLocalProof();
  const [dismissed, setDismissed] = useState(dismissedFor);
  const openedOnPurpose = useOpenedOnPurpose((s) => s.open);
  // The code and the computer it was asked of. Shown only while that
  // request is the one waiting: removed, approved or another backend in
  // view, and it is gone.
  const [asked, setAsked] = useState<{ target: string; code: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const currentTarget = useRef<string | null>(null);
  currentTarget.current = status?.target ?? null;
  const waitingHere = !!status && !!asked && asked.target === status.target && status.device === 'pending';
  useEffect(() => {
    if (asked && !waitingHere) setAsked(null);
  }, [asked, waitingHere]);

  if (!status || status.device === 'approved') return null;
  if (status.approvedDevices === 0 && !openedOnPurpose) return null;
  const key = standingKey(status);
  if (dismissed === key && !openedOnPurpose) return null;

  const dismiss = () => {
    try { sessionStorage.setItem(DISMISSED_KEY, key); } catch { /* blocked storage: this mount only */ }
    setDismissed(key);
    useOpenedOnPurpose.setState({ open: false });
  };
  const ask = async () => {
    if (busy) return;
    const target = status.target;
    setBusy(true);
    setError('');
    try {
      const code = await askToApproveThisBrowser(backendPrefix);
      // A late answer for a computer no longer in view is dropped.
      if (code && currentTarget.current === target) setAsked({ target, code });
    } catch (e) {
      if (currentTarget.current === target) setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
      await reload();
    }
  };

  const waiting = status.device === 'pending';
  const code = waitingHere ? asked!.code : '';
  return (
    <section
      role="region"
      aria-label={APPROVE_SCREEN_TITLE}
      style={{
        position: 'fixed', left: 16, bottom: 16, zIndex: 'var(--z-toast)', width: 380, maxWidth: 'calc(100vw - 32px)',
        background: 'var(--bg-2)', border: '1px solid var(--border)', borderRadius: 6, padding: 14, fontSize: 12, lineHeight: 1.5,
      }}
    >
      <h2 style={{ margin: 0, fontSize: 14 }}>{APPROVE_SCREEN_TITLE}</h2>
      <p style={{ margin: '6px 0 0' }}>
        You can look around. Approving work, answering permission prompts and managing devices need a browser or app this
        computer has approved.
      </p>
      {code ? (
        <div style={{ marginTop: 10 }}>
          <div style={{ display: 'flex', alignItems: 'baseline', gap: 10 }}>
            <span data-testid="approve-screen-code" style={{ fontFamily: 'var(--font-mono, monospace)', fontSize: 22, letterSpacing: 2 }}>{code}</span>
            <span style={{ fontWeight: 600 }}>V3</span>
          </div>
          <p style={{ margin: '6px 0 0' }}>
            {whereToApprove(status)} Approve “{thisDeviceName()}” only if it shows this code.
          </p>
        </div>
      ) : waiting ? (
        <p style={{ margin: '8px 0 0' }}>
          This browser is waiting for approval. {whereToApprove(status)} If you no longer have the code, remove this
          request there, then ask again.
        </p>
      ) : (
        <p style={{ margin: '8px 0 0' }}>Ask, then compare the code shown here. {whereToApprove(status)}</p>
      )}
      {error && <p className="settings-error" role="alert" style={{ margin: '8px 0 0' }}>{error}</p>}
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'flex-end', flexWrap: 'wrap', gap: 8, marginTop: 10 }}>
        <a
          href={SITE_HELP_LOST_DEVICES_URL}
          style={{ marginRight: 'auto' }}
          onClick={(e) => {
            e.preventDefault();
            void openInSystemBrowser(SITE_HELP_LOST_DEVICES_URL);
          }}
        >
          Locked out?
        </a>
        <button type="button" className="settings-btn" onClick={dismiss}>Not now</button>
        {!waiting && !code && (
          <button type="button" className="settings-btn primary" disabled={busy} onClick={() => void ask()}>
            {APPROVE_SCREEN_ASK}
          </button>
        )}
      </div>
    </section>
  );
}
