import { useEffect, useId, useRef, useState } from 'react';
import { api, ApiError, DEVICE_APPROVAL_EVENT, takePendingDeviceApproval } from '../api';
import {
  deviceIdentity,
  markDeviceEnrolled,
  pinComputerIdentity,
  pinnedComputerIdentity,
} from '../lib/deviceIdentity';
import { beginDeviceEnrolmentV3 } from '../lib/deviceEnrolment';

// Shown on a remote browser when the machine it is using only accepts devices
// a person approved on that computer (daemon trusted_devices.go). It asks the
// machine to add this browser, shows the code to compare, and waits.
//
// Everything this dialog hears from the computer (the enrolment response and
// the status polls) comes through the relay, so none of it is trusted alone:
// a relay could hand over its own key and then report "approved". The
// computer key is pinned only when the person also says, here, that the
// computer shows this exact code — the one step the relay can't fake — and
// the status poll reports approved with the same key, in either order.

const POLL_MS = 3000;

export const DEVICE_APPROVAL_TITLE = 'Approve this browser on your computer';
export const CODE_MATCHES = 'The computer shows this code';
export const CODE_DIFFERS = 'It shows a different code';
export const FORGET_AND_COMPARE = 'Forget the old computer key and compare codes again';
// (The old key is replaced only once the new code is confirmed and approved.)

const IDENTITY_CHANGED =
  "This computer's identity changed. If you reinstalled SoluTerminal there, remove this browser in Settings → " +
  'Privacy & security → Remote devices on the computer, then compare codes again here; otherwise someone may be ' +
  'intercepting the connection. Until a new code is confirmed, this browser keeps the old key and refuses ' +
  'the computer.';
const STILL_APPROVED =
  'This browser is still approved on the computer, so there is no code to compare. Remove it there in ' +
  'Settings → Privacy & security → Remote devices first, then compare codes again.';
const CODES_DIFFER =
  "The codes don't match, so nothing was changed: someone may be intercepting the connection. On the computer, " +
  'remove this browser in Settings → Privacy & security → Remote devices, and don\'t approve it there.';
// The computer keeps a request whose code was shown and refuses another for
// the same key (daemon errDevicePendingKept). The code covers this browser's
// one-time nonce, which a reload loses, so it can't be shown again; the
// computer's own copy of the code is never shown, since it comes through the
// relay and comparing it with itself would prove nothing.
export const REQUEST_WAITING =
  'A request from this browser is already waiting. Remove it in Settings → Privacy & security → Remote ' +
  "devices, then ask again. If you've already approved another device, remove it from that device.";

function requestWaiting(e: unknown): boolean {
  if (!(e instanceof ApiError) || e.status !== 409) return false;
  const body = e.body as { error?: unknown } | undefined;
  return body?.error === 'request_waiting';
}

// The server's sentence when it sent one, never the raw response body.
function errorText(e: unknown): string {
  const body = e instanceof ApiError ? (e.body as { message?: unknown } | undefined) : undefined;
  if (typeof body?.message === 'string') return body.message;
  return e instanceof Error ? e.message : String(e);
}

function browserName(): string {
  const ua = navigator.userAgent;
  const os = /Windows/.test(ua) ? 'Windows' : /Android/.test(ua) ? 'Android' : /iPhone|iPad/.test(ua) ? 'iOS'
    : /Mac OS X/.test(ua) ? 'macOS' : /Linux/.test(ua) ? 'Linux' : 'a device';
  const app = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome'
    : /Safari\//.test(ua) ? 'Safari' : 'Browser';
  return `${app} on ${os}`;
}

type Phase = 'ask' | 'waiting' | 'identity-changed' | 'still-approved' | 'codes-differ' | 'error';

export function DeviceApprovalPrompt() {
  const titleId = useId();
  const [target, setTarget] = useState<string | null>(null);
  const [code, setCode] = useState('');
  const [codeVersion, setCodeVersion] = useState<2 | 3>(2);
  const [enrolmentId, setEnrolmentId] = useState<string | undefined>();
  const generation = useRef(0);
  const [deviceId, setDeviceId] = useState<string | null>(null);
  // The computer key the code on screen covers.
  const [shownIdentity, setShownIdentity] = useState<string | null>(null);
  // The person said the computer shows this code.
  const [confirmed, setConfirmed] = useState(false);
  // What the status poll reported once it said approved (undefined: not yet).
  const [approvedIdentity, setApprovedIdentity] = useState<string | null | undefined>(undefined);
  const [phase, setPhase] = useState<Phase>('ask');
  const [error, setError] = useState('');

  useEffect(() => () => { generation.current++; }, []);

  useEffect(() => {
    const onNeeded = (e: Event) => {
      const detail = (e as CustomEvent<{ target: string }>).detail;
      takePendingDeviceApproval();
      setTarget((current) => current ?? detail.target);
    };
    // A refusal from before this mounted (the first load after a reload).
    const pending = takePendingDeviceApproval();
    if (pending) setTarget((current) => current ?? pending);
    window.addEventListener(DEVICE_APPROVAL_EVENT, onNeeded);
    return () => window.removeEventListener(DEVICE_APPROVAL_EVENT, onNeeded);
  }, []);

  // Poll until the computer reports approved.
  useEffect(() => {
    if (phase !== 'waiting' || !deviceId || !target || approvedIdentity !== undefined) return;
    let stopped = false;
    const timer = window.setInterval(() => {
      void api.remoteDeviceStatus(target, deviceId).then((d) => {
        if (stopped) return;
        if (codeVersion === 3 && (d.id !== deviceId || d.codeVersion !== 3 || d.enrolmentId !== enrolmentId
          || d.daemonIdentity !== shownIdentity)) {
          setPhase('codes-differ');
          return;
        }
        if (d.status !== 'approved') return;
        setApprovedIdentity(d.daemonIdentity ?? null);
      }).catch((e) => {
        if (!stopped && codeVersion === 3 && e instanceof ApiError && e.status === 404) {
          setCode('');
          setError('This comparison expired. Ask for approval again.');
          setPhase('error');
        }
      });
    }, POLL_MS);
    return () => {
      stopped = true;
      window.clearInterval(timer);
    };
  }, [phase, deviceId, target, approvedIdentity, codeVersion, enrolmentId, shownIdentity]);

  // Both the person's confirmation and the computer's approval: done.
  useEffect(() => {
    if (phase !== 'waiting' || !target || !confirmed || approvedIdentity === undefined) return;
    if ((approvedIdentity ?? null) !== shownIdentity) {
      // Approved, but for another key than the code the person compared.
      setPhase('codes-differ');
      return;
    }
    if (shownIdentity) pinComputerIdentity(target, shownIdentity);
    markDeviceEnrolled(target);
    window.location.reload();
  }, [phase, target, confirmed, approvedIdentity, shownIdentity]);

  if (!target) return null;

  const close = () => {
    generation.current++;
    setTarget(null);
    setCode('');
    setDeviceId(null);
    setPhase('ask');
    setConfirmed(false);
    setApprovedIdentity(undefined);
  };
  // replacing: the person chose to compare codes for a computer whose key
  // no longer matches the pinned one. The old pin (and enrolment) stay until
  // the new key is confirmed and approved, so a relay that only claims the
  // identity changed gains nothing: requests stay refused meanwhile.
  const request = async (replacing = false) => {
    const current = ++generation.current;
    setCode('');
    setError('');
    setConfirmed(false);
    setApprovedIdentity(undefined);
    try {
      const me = await deviceIdentity();
      const comparison = await beginDeviceEnrolmentV3(me,
        async (commit) => {
          if (current !== generation.current) throw new Error('Comparison closed');
          const reply = await api.enrollRemoteDevice(target, browserName(), me.spki, commit);
          if (current !== generation.current) throw new Error('Comparison closed');
          return reply;
        },
        async (id, enrolment, nonce) => {
          if (current !== generation.current) throw new Error('Comparison closed');
          return api.revealRemoteDevice(target, id, enrolment, nonce);
        });
      if (current !== generation.current) return;
      const d = comparison.device;
      // A computer key already pinned for this target must match; a new key is
      // pinned only after the person confirms the code below.
      const pinned = pinnedComputerIdentity(target);
      if (pinned && d.daemonIdentity !== pinned && !replacing) {
        setPhase('identity-changed');
        return;
      }
      if (replacing && d.status === 'approved') {
        setPhase('still-approved');
        return;
      }
      // The code covers this browser's key and the computer key the response
      // carried; the computer computes its own from the keys it actually holds.
      setCode(comparison.code);
      setCodeVersion(comparison.codeVersion);
      setEnrolmentId(d.enrolmentId);
      setShownIdentity(d.daemonIdentity ?? null);
      setDeviceId(d.id);
      if (d.status === 'approved') {
        // Already approved: no comparison happened now, so nothing new is
        // pinned (the private channel stays off for this target).
        markDeviceEnrolled(target);
        window.location.reload();
        return;
      }
      setPhase('waiting');
    } catch (e) {
      if (current !== generation.current) return;
      setPhase('error');
      setError(requestWaiting(e) ? REQUEST_WAITING : `Couldn't ask for approval: ${errorText(e)}`);
    }
  };
  const compareAgain = () => void request(true);

  const waiting = phase === 'waiting';
  const approvedFirst = waiting && approvedIdentity !== undefined && !confirmed;
  return (
    <div className="dialog-backdrop" onMouseDown={close}>
      <div
        className="dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        onKeyDown={(e) => {
          e.stopPropagation();
          if (e.key === 'Escape') { e.preventDefault(); close(); }
        }}
        onMouseDown={(e) => e.stopPropagation()}
        style={{ width: 460, maxWidth: '92vw' }}
      >
        <h2 id={titleId} style={{ margin: 0, fontSize: 16 }}>{DEVICE_APPROVAL_TITLE}</h2>
        <div style={{ marginTop: 6, fontSize: 12, color: 'var(--fg-dim)', lineHeight: 1.5 }}>
          That computer only accepts devices you have approved on it. Ask for approval, then on the
          computer open SoluTerminal → Settings → Privacy & security → Remote devices and approve the device
          showing the same code:
        </div>
        <div style={{ marginTop: 12, fontFamily: 'var(--font-mono, monospace)', fontSize: 18, letterSpacing: 1 }}>
          {code || (phase === 'ask' ? 'Shown after you ask' : phase === 'error' ? '' : '…')}
          {code && <span style={{ marginLeft: 8, fontSize: 12 }}>V{codeVersion}</span>}
        </div>
        {waiting && (
          <div style={{ marginTop: 10, fontSize: 12, lineHeight: 1.5 }}>
            {approvedFirst
              ? 'Approved on the computer — does it show this exact code?'
              : confirmed
                ? 'Thanks — waiting for approval on the computer…'
                : 'Waiting for approval on the computer… Does it show this exact code?'}
          </div>
        )}
        {phase === 'identity-changed' && (
          <div className="settings-error" role="alert" style={{ marginTop: 10, fontSize: 12, lineHeight: 1.5 }}>{IDENTITY_CHANGED}</div>
        )}
        {phase === 'still-approved' && (
          <div className="settings-error" role="alert" style={{ marginTop: 10, fontSize: 12, lineHeight: 1.5 }}>{STILL_APPROVED}</div>
        )}
        {phase === 'codes-differ' && (
          <div className="settings-error" role="alert" style={{ marginTop: 10, fontSize: 12, lineHeight: 1.5 }}>{CODES_DIFFER}</div>
        )}
        {phase === 'error' && (
          <div className="settings-error" role="alert" style={{ marginTop: 10, fontSize: 12, lineHeight: 1.5 }}>{error}</div>
        )}
        <div style={{ display: 'flex', justifyContent: 'flex-end', flexWrap: 'wrap', gap: 8, marginTop: 14 }}>
          <button type="button" onClick={close} style={{ fontSize: 12, padding: '4px 12px' }}>
            {phase === 'codes-differ' ? 'Close' : 'Not now'}
          </button>
          {waiting && (
            // Still offered after confirming, until the computer approves:
            // a person who clicked too soon can take it back.
            <button type="button" onClick={() => setPhase('codes-differ')} style={{ fontSize: 12, padding: '4px 12px' }}>
              {CODE_DIFFERS}
            </button>
          )}
          {waiting && !confirmed && (
            <button type="button" className="primary" onClick={() => setConfirmed(true)} style={{ fontSize: 12, padding: '4px 14px' }}>
              {CODE_MATCHES}
            </button>
          )}
          {(phase === 'identity-changed' || phase === 'still-approved') && (
            <button type="button" className="primary" onClick={compareAgain} style={{ fontSize: 12, padding: '4px 14px' }}>
              {FORGET_AND_COMPARE}
            </button>
          )}
          {(phase === 'ask' || phase === 'error') && (
            <button type="button" className="primary" onClick={() => void request()} style={{ fontSize: 12, padding: '4px 14px' }}>
              Ask for approval
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
