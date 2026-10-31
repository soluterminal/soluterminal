import { useCallback, useEffect, useState } from 'react';
import { api, currentRoutingScope } from '../../api';
import type { RemoteDevicesState } from '../../api';
import { privateRelayCandidate, privateRelayEnabled, setPrivateRelayEnabled } from '../../lib/privateRelay';
import { SettingsGroup, SettingsRow } from './SettingsRow';

export const REMOTE_POLICY_LABEL = 'Who can use this computer remotely';
export const REMOTE_POLICY_HINT =
  'Any signed-in device: anyone signed in to your account can reach this computer through our relay. ' +
  'Only devices I approve: this computer also checks each phone or browser itself, so even someone who ' +
  'controls our servers or steals your login cannot reach it from an unapproved device.';

// The prompt on this computer when a phone or browser asks to use it. The
// name is the device's own claim, so the person is told to compare codes.
export function remoteDevicePendingText(name: string): string {
  return `“${name}” is asking to use this computer. Approve it only if it shows the same code as Settings → Privacy & security.`;
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
  return <SettingsGroup title="Encryption"><PrivateRelayRow /></SettingsGroup>;
}

// The daemon keeps the list and checks the signatures (daemon
// trusted_devices.go); approving and the policy are local, person-only acts,
// so on a remote viewer the buttons answer with the daemon's refusal.
export function RemoteDevicesSetting() {
  const [state, setState] = useState<RemoteDevicesState | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(() => {
    void api.listRemoteDevices().then(setState).catch(() => {
      /* older or unreachable daemon: keep the group hidden */
    });
  }, []);

  useEffect(() => {
    load();
    const timer = window.setInterval(load, 5000);
    return () => window.clearInterval(timer);
  }, [load]);

  if (!state) {
    // The list can be unavailable (an older daemon); this browser's own
    // setting still applies.
    return privateRelayCandidateInView() ? <SettingsGroup title="Remote devices"><PrivateRelayRow /></SettingsGroup> : null;
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
    } finally {
      setPending(false);
    }
  };

  const approved = state.devices.filter((d) => d.status === 'approved').length;
  const changePolicy = (policy: 'any' | 'approved') => {
    if (policy === 'approved' && approved === 0 &&
      !window.confirm('No device is approved yet, so your phone and other browsers will lose access until you approve them here. Continue?')) {
      return;
    }
    void run('change remote access', () => api.setRemoteAccessPolicy(policy).then(setState));
  };

  return (
    <SettingsGroup title="Remote devices">
      <PrivateRelayRow />
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
            label={d.name}
            hint={
              <>
                Code <code>{d.code}</code>{' '}
                {d.status === 'pending'
                  ? '— waiting for approval. Approve only if the device shows the same code.'
                  : d.lastSeenAt ? `— approved, last used ${new Date(d.lastSeenAt).toLocaleString()}` : '— approved'}
                {d.status === 'pending' && !d.verifiesComputer && (
                  <> (this code doesn't verify this computer to the device: expected only from an older app; otherwise someone may be altering the request)</>
                )}
                {d.status === 'approved' && !d.verifiesComputer && (
                  <> (older approval: it didn't verify this computer to the device; remove it and approve again from an updated app)</>
                )}
              </>
            }
          >
            {d.status === 'pending' && (
              <button type="button" className="settings-btn primary" disabled={pending}
                onClick={() => void run('approve the device', () => api.approveRemoteDevice(d.id))}>
                Approve
              </button>
            )}
            <button type="button" className="settings-btn" disabled={pending}
              onClick={() => void run('remove the device', () => api.removeRemoteDevice(d.id))}>
              Remove
            </button>
          </SettingsRow>
        ))
      )}
      <p className="settings-row-hint">Approving devices and this setting can only be changed on this computer.</p>
      {error && <p className="settings-row-hint settings-error">{error}</p>}
    </SettingsGroup>
  );
}
