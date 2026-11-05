import { useCallback, useEffect, useState } from 'react';
import { api, currentRoutingScope } from '../../api';
import { confirmDialog } from '../../lib/dialog';
import type { RemoteDevice, RemoteDevicesState } from '../../api';
import { privateRelayCandidate, privateRelayEnabled, privateRelayTarget, setPrivateRelayEnabled } from '../../lib/privateRelay';
import { isEmbedded } from '../../embed';
import { useLocalProof } from '../../lib/useLocalProof';
import { deviceClaimedName } from '../../lib/deviceClaimedName';
import { SettingsGroup, SettingsRow } from './SettingsRow';
import { ALERTS_SCOPE_TEXT, FULL_SCOPE_TEXT, isFullApprovedDevice } from './deviceScope';
import { DeviceVerification } from './DeviceVerification';
import { approvalProvenance } from './HumanProofSetting';
import { ApproveDeviceDialog } from './ApproveDeviceDialog';

export const REMOTE_POLICY_LABEL = 'Who can use this computer remotely';
export const REMOTE_POLICY_HINT =
  'Any signed-in device: anyone signed in to your account can reach this computer through our relay. ' +
  'Only devices I approve: this computer also checks each phone or browser itself, so even someone who ' +
  'controls our servers or steals your login cannot reach it from an unapproved device.';

export { ALERTS_SCOPE_TEXT, FULL_SCOPE_TEXT, deviceScopeText } from './deviceScope';

// The prompt on this computer when a phone or browser asks to use it. The
// name is the device's own claim, so it is shown as such and the person is
// told to compare codes. The prompt only opens Settings: approving happens in
// ApproveDeviceDialog. An alerts-only device says so.
export function remoteDevicePendingText(name: string, scope?: string): string {
  if (scope === 'alerts') {
    return `A device calling itself “${deviceClaimedName(name)}” is asking to take over this computer's push alerts (${ALERTS_SCOPE_TEXT.toLowerCase()}). Approve it only if it shows the same code as Settings → Privacy & security.`;
  }
  return `A device calling itself “${deviceClaimedName(name)}” is asking to use this computer. Approve it only if it shows the same code as Settings → Privacy & security.`;
}

export const PRIVATE_RELAY_LABEL = "Encrypt this browser's connection end to end";
export const PRIVATE_RELAY_HINT =
  'On for a browser this computer approved with matching codes. Everything between them travels in a ' +
  'channel only the two of them can read; our relay sees only its size, and this browser never falls back ' +
  'to the ordinary relay for this computer (only asking to be approved again does). Turning it back on ' +
  'reloads the page so every open connection moves into the channel.';

export function privateRelayCandidateInView(): string | null {
  const scope = currentRoutingScope();
  return privateRelayCandidate(scope.prefix, scope.backendPrefix);
}

// This browser's opt-in to the private relay channel (lib/privateRelay).
// Offered only where it can work: the computer in view approved this browser
// and its identity is pinned.
function PrivateRelayRow() {
  const [on, setOn] = useState(privateRelayEnabled);
  if (!privateRelayCandidateInView()) return null;
  return (
    <SettingsRow label={PRIVATE_RELAY_LABEL} hint={PRIVATE_RELAY_HINT}>
      <input
        type="checkbox"
        aria-label={PRIVATE_RELAY_LABEL}
        checked={on}
        onChange={(e) => {
          setPrivateRelayEnabled(e.target.checked);
          setOn(privateRelayEnabled());
          // Connections already open on the plaintext relay (pane sockets,
          // event streams, requests in flight) end with the page and come
          // back through the channel only.
          if (privateRelayEnabled()) window.location.reload();
        }}
      />
    </SettingsRow>
  );
}

// A phone browser has no Devices tab (its installer and pairing rows don't
// apply there), but its end-to-end encryption, on by default, must still be
// switchable: this is that one row, for the phone Settings.
export function PhoneEncryptionSetting() {
  if (!privateRelayCandidateInView()) return null;
  return <SettingsGroup title="Encryption"><PrivateRelayRow /><DeviceVerification /></SettingsGroup>;
}

export const CANNOT_APPROVE_HERE =
  "This screen can't approve devices. Approve from a browser or app you've already approved.";

/**
 * Whether this screen offers Approve: only where the daemon says its
 * approval would go through (canManageDevices). Shinoque Code's screen also
 * needs its private channel to this computer (as ruled for the shared
 * library in #934): never on its local or plain relay connection.
 */
export function approveOfferedHere(state: RemoteDevicesState): boolean {
  if (state.canManageDevices !== true) return false;
  if (!isEmbedded()) return true;
  const scope = currentRoutingScope();
  return privateRelayTarget(scope.prefix, scope.backendPrefix) !== null;
}

// The daemon keeps the list and checks the signatures (daemon
// trusted_devices.go) and decides who may approve; the list only offers
// Approve where the daemon says it would go through, and approving happens in
// ApproveDeviceDialog after comparing codes.
export function RemoteDevicesSetting() {
  const [state, setState] = useState<RemoteDevicesState | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [rejected, setRejected] = useState<string | null>(null);
  const [reviewing, setReviewing] = useState<RemoteDevice | null>(null);
  const { status: localStatus } = useLocalProof();

  const reload = useCallback(async () => {
    const next = await api.listRemoteDevices();
    setState(next);
    return next;
  }, []);
  const load = useCallback(() => {
    void reload().catch(() => {
      /* older or unreachable daemon: keep the group hidden */
    });
  }, [reload]);

  useEffect(() => {
    load();
    const timer = window.setInterval(load, 5000);
    return () => window.clearInterval(timer);
  }, [load]);

  if (!state) {
    // The list can be unavailable (an older daemon); this browser's own
    // setting still applies.
    return privateRelayCandidateInView() ? <SettingsGroup title="Remote devices"><PrivateRelayRow /><DeviceVerification /></SettingsGroup> : null;
  }

  const run = async (what: string, act: () => Promise<unknown>) => {
    if (pending) return;
    setPending(true);
    setError(null);
    try {
      await act();
      load();
    } catch (e) {
      setError(`Couldn't ${what}: ${String(e)}`);
      load();
    } finally {
      setPending(false);
    }
  };

  // An alerts-only device can't reach this computer remotely, so it doesn't count.
  const approved = state.devices.filter(isFullApprovedDevice).length;
  const canApprove = approveOfferedHere(state);
  const anyPending = state.devices.some((d) => d.status === 'pending');
  const names = Object.fromEntries(state.devices.map((d) => [d.id, d.name]));
  const changePolicy = (policy: 'any' | 'approved') => {
    const scope = currentRoutingScope();
    void run('change remote access', async () => {
      if (policy === 'approved' && approved === 0 &&
        !await confirmDialog({ message: 'No device is approved yet, so your phone and other browsers will lose access until you approve them here. Continue?' })) {
        return;
      }
      if (scope.key !== currentRoutingScope().key) return;
      await api.setRemoteAccessPolicy(policy).then(setState);
    });
  };

  return (
    <SettingsGroup title="Remote devices">
      <PrivateRelayRow />
      <DeviceVerification />
      <SettingsRow label={REMOTE_POLICY_LABEL} hint={REMOTE_POLICY_HINT}>
        <select
          className="settings-select"
          value={state.policy}
          disabled={pending}
          onChange={(e) => changePolicy(e.target.value as 'any' | 'approved')}
        >
          <option value="any">Any signed-in device</option>
          <option value="approved">Only devices I approve</option>
        </select>
      </SettingsRow>
      {state.devices.length === 0 ? (
        <p className="settings-row-hint">
          No devices have asked yet. A phone or browser asks when it reaches this computer while only approved devices are allowed.
        </p>
      ) : (
        state.devices.map((d) => (
          <SettingsRow
            key={d.id}
            label={deviceClaimedName(d.name)}
            hint={
              <>
                {d.verify ? null : d.status === 'failed' ? 'Comparison ended. Ask again from the device.' : d.status === 'committed' ? 'Waiting for the device to confirm its request.' : <>
                  Code <code>{d.code}</code>{' '}
                  <span>{d.codeVersion === 3 ? 'V3' : d.codeVersion === 2 || d.verifiesComputer ? 'V2' : 'V1'}</span>{' '}
                  {d.scope === 'alerts' ? <strong>{ALERTS_SCOPE_TEXT}</strong> : <span>{FULL_SCOPE_TEXT}</span>}{' '}
                </>}
                {d.status === 'pending'
                  ? '— waiting for approval. Approve only if the device shows the same code.'
                  : d.status === 'approved' ? d.lastSeenAt ? `— approved, last used ${new Date(d.lastSeenAt).toLocaleString()}` : '— approved' : ''}
                {d.status === 'pending' && !d.verifiesComputer && (
                  <> (this code doesn't verify this computer to the device: expected only from an older app; otherwise someone may be altering the request)</>
                )}
                {d.status === 'approved' && !d.verifiesComputer && (
                  <> (older approval: it didn't verify this computer to the device; remove it and approve again from an updated app)</>
                )}
                {(d.surface || d.approvedVia) && <div>{approvalProvenance(d, names)}</div>}
                {d.verify?.state === 'committed' && <div>Verify V3 — waiting for the device.</div>}
                {d.verify?.state === 'revealed' && <div>Verify V3 <code>{d.verify.code}</code> — compare with the device.</div>}
                {d.verify?.state === 'failed' && <div>Verify V3 ended. Start again from the device.</div>}
                {rejected === d.id && <div role="alert">The codes differ. Verification was not recorded. You can remove this device.</div>}
              </>
            }
          >
            {d.status === 'approved' && d.verify?.state === 'revealed' && !!d.verify.enrolmentId && <>
              <button type="button" className="settings-btn primary" disabled={pending}
                onClick={() => void run('confirm the comparison', () => api.confirmDeviceVerification(d.id, d.verify!.enrolmentId!))}>
                Verify codes match
              </button>
              <button type="button" className="settings-btn" disabled={pending}
                onClick={() => void run('reject the comparison', async () => {
                  await api.rejectDeviceVerification(d.id, d.verify!.enrolmentId!);
                  setRejected(d.id);
                })}>
                Codes differ
              </button>
            </>}
            {d.status === 'pending' && canApprove && (
              <button type="button" className="settings-btn primary" disabled={pending}
                onClick={() => setReviewing(d)}>
                Compare codes…
              </button>
            )}
            <button type="button" className="settings-btn" disabled={pending}
              onClick={() => void run('remove the device', () => api.removeRemoteDevice(d.id))}>
              Remove
            </button>
          </SettingsRow>
        ))
      )}
      {anyPending && !canApprove && <p className="settings-row-hint">{CANNOT_APPROVE_HERE}</p>}
      <p className="settings-row-hint">Who can use this computer remotely can only be changed on this computer.</p>
      {error && <p className="settings-row-hint settings-error">{error}</p>}
      {reviewing && (
        <ApproveDeviceDialog
          device={reviewing}
          recovery={!!localStatus?.envOff}
          reload={reload}
          onClose={() => setReviewing(null)}
        />
      )}
    </SettingsGroup>
  );
}
