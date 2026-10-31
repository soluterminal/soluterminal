import { useEffect, useRef, useState } from 'react';
import { usePhoneAccessAllowed } from '../entitlements';
import { IconSmartphone } from './ui/icons';
import { useStore } from '../store';
import { useFocusTrap } from './useFocusTrap';
import { APP_SITE_URL } from '../lib/siteLinks';
import {
  cloudPairApi,
  usePhoneAccessStatus,
  secondsUntil,
  isDesktopShell,
  type CloudStatus,
} from '../cloudPair';

// The in-window PAIR PANEL — replaces the old native OS dialog + system-browser
// bounce. Lives in the embedded web UI so it survives dismissal and is always
// re-reachable (header chip, product banner, Preferences entry).
//
// Flow:
//   1. On open → POST /api/cloud/pair/start, render the returned 6-char CODE.
//   2. Poll /api/cloud/status (via usePhoneAccessStatus): while pairing.active
//      show "Waiting…"; the instant relayState flips "online", show success.
//   3. Handle code expiry ("Get a new code" → start again) + credsInvalid.
//   4. Cancel → POST /api/cloud/pair/cancel.
//
// Desktop-shell only: the gate (PhoneAccessPairPanelGate) refuses to mount
// outside Tauri, so this never renders on the phone/relay web.

type Phase = 'starting' | 'waiting' | 'connected' | 'expired' | 'signin' | 'error';

function PairPanel({ onClose }: { onClose: () => void }) {
  const cardRef = useRef<HTMLDivElement>(null);
  useFocusTrap(cardRef, true);
  useEffect(() => { cardRef.current?.focus(); }, []);
  // Local pairing lifecycle, separate from the polled status so we can show a
  // "starting" spinner before the first /pair/start resolves.
  const [code, setCode] = useState<string | null>(null);
  const [expiresAt, setExpiresAt] = useState<string | undefined>(undefined);
  const [startErr, setStartErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);
  // Bumped after each start/cancel so the poller fetches fresh status promptly.
  const [pokeKey, setPokeKey] = useState(0);
  const { status } = usePhoneAccessStatus({ pokeKey });
  // Tick once a second so the expiry countdown re-renders without re-polling.
  const [, forceTick] = useState(0);
  const startedRef = useRef(false);

  // Kick off a pairing on mount. start() is idempotent server-side, so if a
  // code is already pending the daemon hands back the same one.
  const start = async () => {
    setBusy(true);
    setStartErr(null);
    try {
      const res = await cloudPairApi.pairStart();
      setCode(res.code);
      setExpiresAt(res.expiresAt);
      setPokeKey((k) => k + 1);
    } catch (e) {
      setStartErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  useEffect(() => {
    if (startedRef.current) return;
    startedRef.current = true;
    void start();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    const t = setInterval(() => forceTick((n) => n + 1), 1000);
    return () => clearInterval(t);
  }, []);

  const cancel = async () => {
    try {
      await cloudPairApi.pairCancel();
    } catch {
      /* best-effort — closing anyway */
    } finally {
      setPokeKey((k) => k + 1);
      onClose();
    }
  };

  // Forget the cloud connection entirely (deletes the daemon's claim.json). The
  // way out of a stuck "sign-in needed" (machine deleted server-side → token
  // permanently rejected) and the off-switch for a user who no longer wants
  // cloud. Status reverts to "not set up" on the next poll; we close after.
  const [disconnecting, setDisconnecting] = useState(false);
  const disconnect = async () => {
    setDisconnecting(true);
    try {
      await cloudPairApi.disconnect();
      useStore.getState().toast('Cloud disconnected — this computer no longer connects to the cloud.', {
        kind: 'info',
        ttlMs: 7000,
      });
      onClose(); // success: dismiss; the chip/banner poll reverts to "not set up"
    } catch (e) {
      const status = (e as { status?: number } | null)?.status;
      useStore.getState().toast(
        status === 403
          ? 'Disconnect must run on the machine itself, not over your phone/cloud link.'
          : `Couldn’t disconnect: ${(e as Error).message}`,
        { kind: 'info', icon: '!', ttlMs: 8000 },
      );
      // Failure: keep the panel open so the user can retry; re-poll status.
      setPokeKey((k) => k + 1);
    } finally {
      setDisconnecting(false);
    }
  };

  const remaining = secondsUntil(expiresAt);
  const phase: Phase = derivePhase(status, { code, startErr, remaining });

  const copyCode = async () => {
    if (!code) return;
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard may be unavailable; the code is on-screen regardless */
    }
  };

  return (
    <div
      className="phone-access-scrim"
      onClick={() => {
        // Click-outside closes, but cancel a still-pending pairing so we don't
        // leave a dangling code server-side.
        if (phase === 'connected') onClose();
        else void cancel();
      }}
    >
      <div
        ref={cardRef}
        tabIndex={-1}
        className="phone-access-card"
        role="dialog"
        aria-modal="true"
        aria-labelledby="phone-access-title"
        onKeyDown={(e) => {
          e.stopPropagation();
          if (e.key === 'Escape') {
            e.preventDefault();
            if (phase === 'connected') onClose();
            else void cancel();
          }
        }}
        onClick={(e) => e.stopPropagation()}
      >
        {phase === 'connected' ? (
          <ConnectedBody
            ownerEmail={status?.ownerEmail}
            onClose={onClose}
            onDisconnect={() => void disconnect()}
            disconnecting={disconnecting}
          />
        ) : phase === 'signin' ? (
          <SigninBody
            onClose={onClose}
            onDisconnect={() => void disconnect()}
            disconnecting={disconnecting}
          />
        ) : (
          <PairBody
            phase={phase}
            code={code}
            busy={busy}
            startErr={startErr}
            remaining={remaining}
            copied={copied}
            onCopy={copyCode}
            onRegenerate={() => void start()}
            onCancel={() => void cancel()}
          />
        )}
      </div>
    </div>
  );
}

function derivePhase(
  status: CloudStatus | null,
  ctx: { code: string | null; startErr: string | null; remaining: number | null },
): Phase {
  // Success wins the instant the relay comes online.
  if (status?.relayState === 'online') return 'connected';
  if (status?.credsInvalid) return 'signin';
  if (ctx.startErr) return 'error';
  if (!ctx.code) return 'starting';
  if (ctx.remaining !== null && ctx.remaining <= 0) return 'expired';
  return 'waiting';
}

function PairBody(props: {
  phase: Phase;
  code: string | null;
  busy: boolean;
  startErr: string | null;
  remaining: number | null;
  copied: boolean;
  onCopy: () => void;
  onRegenerate: () => void;
  onCancel: () => void;
}) {
  const { phase, code, busy, startErr, remaining, copied, onCopy, onRegenerate, onCancel } = props;
  const expired = phase === 'expired';

  return (
    <>
      <h2 id="phone-access-title"><IconSmartphone size={16} style={{ verticalAlign: '-2px' }} /> Set up phone access</h2>
      <p className="phone-access-intro">
        Control this computer from your phone — run Claude Code here, drive it
        from anywhere.
      </p>

      {phase === 'error' ? (
        <div className="phone-access-error" role="alert">
          Couldn&rsquo;t start pairing: {startErr}
          <div className="phone-access-actions">
            <button type="button" className="phone-access-btn primary" onClick={onRegenerate}>
              Try again
            </button>
            <button type="button" className="phone-access-btn" onClick={onCancel}>
              Close
            </button>
          </div>
        </div>
      ) : phase === 'starting' || !code ? (
        <div className="phone-access-codebox">
          <div className="phone-access-code is-loading" aria-busy="true">
            ••••••
          </div>
          <div className="phone-access-status">Getting a code…</div>
        </div>
      ) : (
        <>
          <div className={`phone-access-codebox ${expired ? 'is-expired' : ''}`}>
            <button
              type="button"
              className="phone-access-code"
              onClick={onCopy}
              title="Click to copy"
              aria-label={`Pairing code ${code.split('').join(' ')}. Click to copy.`}
            >
              {code}
            </button>
            <div className="phone-access-code-meta">
              <button type="button" className="phone-access-copy" onClick={onCopy}>
                {copied ? '✓ Copied' : 'Copy code'}
              </button>
              {!expired && remaining !== null && (
                <span className="phone-access-expiry">Expires in {fmtMMSS(remaining)}</span>
              )}
            </div>
          </div>

          {expired ? (
            <div className="phone-access-status warn" role="status">
              That code expired (the server may have been waking up).
              <div className="phone-access-actions">
                <button
                  type="button"
                  className="phone-access-btn primary"
                  disabled={busy}
                  onClick={onRegenerate}
                >
                  {busy ? 'Getting a new code…' : 'Get a new code'}
                </button>
              </div>
            </div>
          ) : (
            <>
              <ol className="phone-access-steps">
                <li>
                  <b>On your phone:</b> open soluterminal and sign in with the{' '}
                  <b>same account</b>.
                </li>
                <li>
                  {/* The phone's real button (CloudGate.tsx); on a first visit it sits
                      under the folded "Other ways to connect". */}
                  Tap <b>&ldquo;+ Claim a new machine&rdquo;</b> (under &ldquo;Other ways to
                  connect&rdquo; if you don&rsquo;t see it) and enter this code.
                </li>
              </ol>
              <p className="phone-access-secondary">
                Or approve in your browser at <b>{APP_SITE_URL}</b>.
              </p>
              <div className="phone-access-status" role="status">
                <span className="phone-access-spinner" aria-hidden />
                Waiting for you to enter the code…
              </div>
            </>
          )}
        </>
      )}

      {phase !== 'error' && (
        <div className="phone-access-actions end">
          <button type="button" className="phone-access-btn ghost" onClick={onCancel}>
            Cancel
          </button>
        </div>
      )}
    </>
  );
}

function ConnectedBody({
  ownerEmail,
  onClose,
  onDisconnect,
  disconnecting,
}: {
  ownerEmail?: string;
  onClose: () => void;
  onDisconnect: () => void;
  disconnecting: boolean;
}) {
  return (
    <>
      <h2 id="phone-access-title" className="ok">
        ✓ Connected
      </h2>
      <p className="phone-access-intro">
        You can now control this computer from your phone.
        {ownerEmail ? <> Paired to <b>{ownerEmail}</b>.</> : null}
      </p>
      <div className="phone-access-actions end">
        <button
          type="button"
          className="phone-access-btn ghost"
          onClick={onDisconnect}
          disabled={disconnecting}
          title="Forget this cloud connection (you can pair again anytime)"
        >
          {disconnecting ? 'Disconnecting…' : 'Disconnect'}
        </button>
        <button type="button" className="phone-access-btn primary" onClick={onClose}>
          Close
        </button>
      </div>
    </>
  );
}

function SigninBody({
  onClose,
  onDisconnect,
  disconnecting,
}: {
  onClose: () => void;
  onDisconnect: () => void;
  disconnecting: boolean;
}) {
  return (
    <>
      <h2 id="phone-access-title" className="warn">
        Sign-in needed
      </h2>
      <p className="phone-access-intro">
        This computer&rsquo;s cloud connection is no longer valid (the sign-in expired, or
        this machine was removed from your account). Re-pair it: choose
        &ldquo;Set up phone access&rdquo; again and enter a fresh code on your phone, signed in
        with the same account.
      </p>
      <p className="phone-access-secondary">
        Or, if you don&rsquo;t use cloud on this computer, <b>Disconnect</b> to stop the
        reminders. You can pair again anytime.
      </p>
      <div className="phone-access-actions end">
        <button
          type="button"
          className="phone-access-btn"
          onClick={onDisconnect}
          disabled={disconnecting}
          title="Forget this cloud connection and clear the sign-in reminder"
        >
          {disconnecting ? 'Disconnecting…' : 'Disconnect'}
        </button>
        <button type="button" className="phone-access-btn ghost" onClick={onClose}>
          Close
        </button>
      </div>
    </>
  );
}

function fmtMMSS(secs: number): string {
  const m = Math.floor(secs / 60);
  const s = secs % 60;
  return `${m}:${String(s).padStart(2, '0')}`;
}

// Pulls visibility off the store. Separate gate so App doesn't re-render every
// time the panel flips, and so the panel (which starts a pairing on mount) only
// mounts when actually opened. Desktop-shell only — never on the phone/relay.
export function PhoneAccessPairPanelGate() {
  const open = useStore((s) => s.phoneAccessOpen);
  const setOpen = useStore((s) => s.setPhoneAccessOpen);
  const allowed = usePhoneAccessAllowed();
  // The desktop tray's "Pair with cloud…" item dispatches this window event
  // (lib.rs) so it can surface the panel directly instead of only focusing the
  // window. Listener must run before the early return to respect hook rules.
  useEffect(() => {
    if (!isDesktopShell()) return;
    const onOpen = () => setOpen(true);
    window.addEventListener('solu:open-pair-panel', onOpen);
    return () => window.removeEventListener('solu:open-pair-panel', onOpen);
  }, [setOpen]);
  if (!open || !isDesktopShell() || !allowed) return null;
  return <PairPanel onClose={() => setOpen(false)} />;
}
