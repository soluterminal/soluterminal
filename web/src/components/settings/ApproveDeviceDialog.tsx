import { useEffect, useId, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { api, ApiError } from '../../api';
import type { RemoteDevice, RemoteDevicesState } from '../../api';
import { deviceClaimedName } from '../../lib/deviceClaimedName';
import { deviceScopeText } from './deviceScope';

// The one place a pending device is approved from (Settings → Privacy &
// security → Remote devices). The daemon decides (POST /approve,
// trusted_devices.go); this dialog is the person's proof: it shows the V3
// code the device computed and sends the enrolment that was on screen.
//
// Never automatic: it opens only from a click in the device list, its
// default focus is not on Approve and Enter doesn't approve. It is drawn on
// the page body, never inside a pane, where auto-approve could answer it.

export const CODES_MATCH_APPROVE = 'Codes match: approve';
export const CODES_DIFFER_REMOVE = 'Codes differ: remove';
export const NO_CODE_WARNING =
  'No code to compare. This request has no V3 code, so nothing shows it came from the device you mean. ' +
  'Approve it only if you are sure.';
export const NO_CODE_CONFIRM = 'I am sure: approve without comparing codes';
export const RECOVERY_BANNER = 'Recovery mode is on';
export const REQUEST_CHANGED = 'This request changed since it was shown. Compare the new code before approving.';

function surfaceText(surface: RemoteDevice['surface']): string {
  if (surface === 'relay') return 'through the relay';
  if (surface === 'local') return 'at this computer';
  return 'not recorded';
}

/** The V3 code to compare, or null when there is none to compare (V1/V2, not revealed). */
function comparableCode(d: RemoteDevice): string | null {
  return d.status === 'pending' && d.codeVersion === 3 && d.code && d.enrolmentId ? d.code : null;
}

function refusalText(e: unknown): string {
  if (e instanceof ApiError) {
    const body = e.body as { message?: unknown } | undefined;
    if (body && typeof body.message === 'string') return body.message;
  }
  return e instanceof Error ? e.message : String(e);
}

export function ApproveDeviceDialog({ device, recovery, reload, onClose }: {
  device: RemoteDevice;
  /** SOLU_HUMAN_PROOF=off at this computer. */
  recovery: boolean;
  /** Re-read the device list. */
  reload: () => Promise<RemoteDevicesState | null>;
  onClose: () => void;
}) {
  const titleId = useId();
  const dialogRef = useRef<HTMLDivElement>(null);
  // What the person is looking at; the approval names its enrolment.
  const [shown, setShown] = useState<RemoteDevice | null>(device);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [noCodeConfirmed, setNoCodeConfirmed] = useState(false);

  useEffect(() => { dialogRef.current?.focus(); }, []);

  const reread = async (): Promise<RemoteDevice | null> => {
    const list = await reload().catch(() => null);
    return list?.devices.find((d) => d.id === device.id) ?? null;
  };

  const approve = async (d: RemoteDevice) => {
    if (busy) return;
    setBusy(true);
    setError('');
    try {
      await api.approveRemoteDevice(d.id, d.codeVersion === 3 ? d.enrolmentId : undefined);
      await reload().catch(() => null);
      onClose();
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        // The request on screen is no longer the one waiting: show what is
        // there now. Approving it again is the person's choice.
        const fresh = await reread();
        setShown(fresh);
        setNoCodeConfirmed(false);
        setNotice(fresh ? REQUEST_CHANGED : 'This request is gone.');
      } else {
        setError(refusalText(e));
      }
    } finally {
      setBusy(false);
    }
  };

  const remove = async (d: RemoteDevice) => {
    if (busy) return;
    setBusy(true);
    setError('');
    try {
      await api.removeRemoteDevice(d.id);
      await reload().catch(() => null);
      onClose();
    } catch (e) {
      setError(refusalText(e));
    } finally {
      setBusy(false);
    }
  };

  const code = shown ? comparableCode(shown) : null;
  const pending = shown?.status === 'pending';
  const canApprove = !!shown && pending && !busy && (code !== null || noCodeConfirmed);

  return createPortal(
    <div className="dialog-backdrop" onMouseDown={onClose}>
      <div
        ref={dialogRef}
        className="dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        onKeyDown={(e) => {
          e.stopPropagation();
          if (e.key === 'Escape') { e.preventDefault(); onClose(); }
        }}
        onMouseDown={(e) => e.stopPropagation()}
        style={{ width: 480, maxWidth: '92vw', outline: 'none' }}
      >
        <h2 id={titleId} style={{ margin: 0, fontSize: 16 }}>Approve a device?</h2>
        {recovery && (
          <div className="settings-error" role="status" style={{ marginTop: 8, fontSize: 12 }}>{RECOVERY_BANNER}</div>
        )}
        {!shown ? (
          <div style={{ marginTop: 10, fontSize: 12 }}>{notice || 'This request is gone.'}</div>
        ) : (
          <>
            <dl style={{ margin: '10px 0 0', fontSize: 12, lineHeight: 1.6, display: 'grid', gridTemplateColumns: 'auto 1fr', columnGap: 10 }}>
              <dt style={{ color: 'var(--fg-dim)' }}>Name the device gave</dt>
              <dd style={{ margin: 0, overflowWrap: 'anywhere' }} data-testid="approve-device-name"><bdi>{deviceClaimedName(shown.name)}</bdi></dd>
              <dt style={{ color: 'var(--fg-dim)' }}>Asked</dt>
              <dd style={{ margin: 0 }}>{new Date(shown.createdAt).toLocaleString()}</dd>
              <dt style={{ color: 'var(--fg-dim)' }}>From</dt>
              <dd style={{ margin: 0 }}>{surfaceText(shown.surface)}</dd>
            </dl>
            {notice && <div role="status" style={{ marginTop: 10, fontSize: 12 }}>{notice}</div>}
            {!pending ? (
              <div style={{ marginTop: 10, fontSize: 12 }}>
                {shown.status === 'approved' ? 'This device is approved now.' : 'This request isn\'t waiting for approval now.'}
              </div>
            ) : code ? (
              <div style={{ marginTop: 14 }}>
                <div style={{ fontSize: 12, color: 'var(--fg-dim)' }}>Compare with the code on the device</div>
                <div style={{ marginTop: 4, display: 'flex', alignItems: 'baseline', gap: 10 }}>
                  <span data-testid="approve-device-code" style={{ fontFamily: 'var(--font-mono, monospace)', fontSize: 26, letterSpacing: 2 }}>{code}</span>
                  <span style={{ fontSize: 13, fontWeight: 600 }}>V3</span>
                  <span data-testid="approve-device-scope" style={{ fontSize: 13, fontWeight: shown.scope === 'alerts' ? 600 : 400 }}>{deviceScopeText(shown.scope)}</span>
                </div>
              </div>
            ) : (
              <div style={{ marginTop: 14 }}>
                <div className="settings-error" role="alert" style={{ fontSize: 12, lineHeight: 1.5 }}>{NO_CODE_WARNING}</div>
                <label style={{ display: 'flex', gap: 6, marginTop: 8, fontSize: 12 }}>
                  <input type="checkbox" checked={noCodeConfirmed} onChange={(e) => setNoCodeConfirmed(e.target.checked)} />
                  {NO_CODE_CONFIRM}
                </label>
              </div>
            )}
          </>
        )}
        {error && <div className="settings-error" role="alert" style={{ marginTop: 10, fontSize: 12, lineHeight: 1.5 }}>{error}</div>}
        <div style={{ display: 'flex', justifyContent: 'flex-end', flexWrap: 'wrap', gap: 8, marginTop: 16 }}>
          <button type="button" className="settings-btn" onClick={onClose}>Cancel</button>
          {shown && pending && <>
            <button type="button" className="settings-btn" disabled={busy} onClick={() => void remove(shown)}>
              {CODES_DIFFER_REMOVE}
            </button>
            <button
              type="button"
              className="settings-btn primary"
              disabled={!canApprove}
              // Enter never approves: only a deliberate click (or Space).
              onKeyDown={(e) => { if (e.key === 'Enter') e.preventDefault(); }}
              onClick={() => void approve(shown)}
            >
              {CODES_MATCH_APPROVE}
            </button>
          </>}
        </div>
      </div>
    </div>,
    document.body,
  );
}
