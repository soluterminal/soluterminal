import { useState } from 'react';
import { api, currentRoutingScope } from '../../api';
import { confirmDialog } from '../../lib/dialog';
import type { RemoteDevice } from '../../api';
import { approveThisBrowserFirst, askToApproveThisBrowser, thisDeviceName, type LocalProofMode } from '../../lib/localProof';
import { useLocalProof } from '../../lib/useLocalProof';
import { isFullApprovedDevice } from './deviceScope';
import { SettingsGroup, SettingsRow } from './SettingsRow';

export const HUMAN_PROOF_LABEL = 'Require approved devices for actions that need you';
export const HUMAN_PROOF_HINT =
  'Some connections can’t be traced to a program: macOS, a Windows browser or the desktop app reaching ' +
  'Linux (WSL), and the desktop app’s combined mode. On those, approving work, answering permission prompts ' +
  'and changing what agents may do need a browser or app you approved here. “Log only” keeps today’s ' +
  'behaviour and records each such action. If you lose every approved device, add SOLU_HUMAN_PROOF=off ' +
  "to ~/.solu/env (WSL, Linux or macOS) or %USERPROFILE%\\.solu\\env (Windows-native), then restart SoluTerminal's background service. " +
  "If your desktop tray has Restart background service, choose it (on Windows it restarts only the WSL service; if it refuses, use the steps below). Otherwise: with WSL, quit SoluTerminal from the tray icon, run wsl --shutdown in Windows PowerShell (this stops everything running in WSL, including your agents) and reopen SoluTerminal; on Linux run systemctl --user restart soluterminal.service, or if you start it yourself, stop and start it again; on macOS run launchctl kickstart -k gui/$(id -u)/com.soluterminal.daemon, or if you start it yourself, stop and start it again; on Windows without WSL, quit SoluTerminal from the tray icon and reopen it. " +
  "Then approve a device in Settings › Privacy & security, remove the SOLU_HUMAN_PROOF=off line, and restart the same way again. While that line is there, any browser or program on this computer can approve or remove devices.";

const MODE_LABELS: Record<LocalProofMode, string> = {
  off: 'Off',
  report: 'Log only',
  require: 'Require',
};

/** How a device was approved, for its row in the device list. */
export function approvalProvenance(d: Pick<RemoteDevice, 'approvedVia' | 'surface'>, names: Record<string, string> = {}): string {
  const where = d.surface === 'local' ? 'at this computer' : d.surface === 'relay' ? 'through the relay' : '';
  const via = d.approvedVia ?? '';
  const named = (id: string) => names[id] ? `“${names[id]}”` : 'another approved device';
  let how = '';
  if (via === 'first-device') how = 'approved as the first device';
  else if (via.startsWith('relay-device:')) how = `approved from ${named(via.slice('relay-device:'.length))} through the relay`;
  else if (via.startsWith('device:')) how = `approved from ${named(via.slice('device:'.length))}`;
  else if (via === 'traced') how = 'approved in a browser at this computer';
  else if (via === 'headers') how = 'approved before approved devices were required';
  else if (via === 'env-recovery') how = 'approved while the device check was off';
  return [where && `asked ${where}`, how].filter(Boolean).join(', ');
}

// Settings › Privacy & security › This computer. The daemon decides
// (daemon/human_proof.go); this browser signs once it is approved
// (lib/localProof.ts). Shown only for the computer this page runs on.
export const REQUIRE_NEEDS_FULL_DEVICE =
  'Approve a browser or app with full access first: an alerts-only device can’t act for you, so requiring approved devices now would lock you out.';

export function HumanProofSetting() {
  const { status, backendPrefix, reload } = useLocalProof();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [code, setCode] = useState<string | null>(null);
  if (!status) return null;

  const run = async (act: () => Promise<unknown>) => {
    if (pending) return;
    setPending(true);
    setError(null);
    try {
      await act();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setPending(false);
      await reload();
    }
  };

  const approved = status.device === 'approved';
  const changeMode = (mode: LocalProofMode) => {
    const scope = currentRoutingScope();
    void run(async () => {
      if (mode === 'require') {
        const list = await api.listRemoteDevices();
        if (scope.key !== currentRoutingScope().key) return;
        // Only the person's own devices act for them: an alerts-only device
        // can't approve anything, so it is never listed as a way back in.
        const devices = (list?.devices ?? []).filter(isFullApprovedDevice);
        if (devices.length === 0) {
          throw new Error(REQUIRE_NEEDS_FULL_DEVICE);
        }
        const ok = await confirmDialog({ message:
          'Only these can then act for you on connections that can’t be traced:\n\n' +
          devices.map((d) => `• ${d.name}${d.approvedVia ? ` (${approvalProvenance(d)})` : ''}`).join('\n') +
          '\n\nRemove any you don’t recognise first. Continue?' });
        if (!ok) return;
      }
      // The person reviewed devices on this computer; switching computers
      // while the dialog waits cancels that consent.
      if (scope.key !== currentRoutingScope().key) return;
      await api.setHumanProofMode(mode);
    });
  };

  let browserHint: string;
  if (approved) browserHint = `Approved as “${thisDeviceName()}”. Actions that need you are signed by this browser.`;
  else if (status.device === 'pending') browserHint = code
    ? `Waiting. In a browser or app that’s already approved, open Settings → Privacy & security and approve “${thisDeviceName()}” if it shows V3 code ${code}.`
    : 'Waiting for approval from a browser or app that’s already approved here. If you no longer have the code, remove this request there, then ask again.';
  else if (status.canBootstrap) browserHint = 'No browser or app is approved on this computer yet. Approve this one to start.';
  else browserHint = "This screen isn't an approved device yet. Choose Ask to approve this browser, then approve it from a browser or app you've already approved. Recovery steps are below if you're locked out.";

  return (
    <SettingsGroup title="This computer">
      <SettingsRow label="This browser" hint={browserHint}>
        {!approved && status.canBootstrap && (
          <button type="button" className="settings-btn primary" disabled={pending}
            onClick={() => void run(() => approveThisBrowserFirst(backendPrefix))}>
            Approve this browser
          </button>
        )}
        {/* The computer keeps a waiting request, so asking again is refused. */}
        {!approved && !status.canBootstrap && status.device !== 'pending' && (
          <button type="button" className="settings-btn" disabled={pending}
            onClick={() => void run(async () => setCode(await askToApproveThisBrowser(backendPrefix)))}>
            Ask to approve this browser
          </button>
        )}
      </SettingsRow>
      <SettingsRow label={HUMAN_PROOF_LABEL} hint={HUMAN_PROOF_HINT}>
        <select
          className="settings-select"
          aria-label={HUMAN_PROOF_LABEL}
          value={status.mode}
          disabled={pending || status.envOff}
          onChange={(e) => changeMode(e.target.value as LocalProofMode)}
        >
          {(['off', 'report', 'require'] as LocalProofMode[]).map((m) => (
            <option key={m} value={m} disabled={m === 'require' && !approved}>{MODE_LABELS[m]}</option>
          ))}
        </select>
      </SettingsRow>
      {status.mode === 'require' && status.effective !== 'require' && !status.envOff && (
        <p className="settings-row-hint">Not in force yet: no browser or app is approved.</p>
      )}
      {status.envOff && (
        <p className="settings-row-hint">Turned off at this computer (SOLU_HUMAN_PROOF=off). Remove the line from {'~/.solu/env (WSL, Linux or macOS) or %USERPROFILE%\\.solu\\env (Windows-native)'} and restart the background service the same way to use this setting.</p>
      )}
      {status.unproven && status.unproven.count > 0 && status.effective !== 'require' && (
        <p className="settings-row-hint">
          Since SoluTerminal started, {status.unproven.count} action{status.unproven.count === 1 ? ' was' : 's were'} accepted
          without an approved device (last: {status.unproven.action}, {new Date(status.unproven.at).toLocaleString()}).
        </p>
      )}
      {error && <p className="settings-row-hint settings-error" role="alert">{error}</p>}
    </SettingsGroup>
  );
}
