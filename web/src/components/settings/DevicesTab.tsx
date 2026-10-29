import { useEffect, useRef, useState } from 'react';
import { usePhoneAccessAllowed, useTeamFeatures } from '../../entitlements';
import { PersonalViewSetting } from './PersonalViewSetting';
import { confirmDialog } from '../../lib/dialog';
import { useStore } from '../../store';
import { useSettings } from '../../settings';
import { useCloud } from '../../cloud';
import { DesktopInstallerLink, detectVisitorOS } from '../DesktopInstallerLink';
import { chipStateFor, isDesktopShell, usePhoneAccessStatus } from '../../cloudPair';
import { getAutostartStatus, setAutostart, type AutostartStatus } from '../../tauri-browser';
import { SettingsCheck, SettingsGroup, SettingsRow } from './SettingsRow';
import { CheckinSetting } from './CheckinSetting';
import { AgentAuthoritySetting } from './AgentAuthoritySetting';
import { RemoteDevicesSetting } from './RemoteDevicesSetting';
import { AgentIntegrationSetting } from './AgentIntegrationSetting';
import { MarketingConsentSetting } from './MarketingConsentSetting';
import type { UpdateAutoApplyMode } from '../../types';
import { slotList } from '../../extensions';

// Other-device entry points. The installer download is always offered — in a
// browser it's how you get the app; inside the desktop shell it's how you grab
// the installer for another machine (or reinstall by hand), so only the hint
// changes. The shell additionally gets the phone-access pairing row. The tab
// itself is hidden on mobile (see SettingsModal) — no row applies there. Also
// home to the per-install automatic-updates preference (the update path is a
// desktop/daemon concern, so this desktop-only tab is its natural place).

export function DevicesTab() {
  const setPhoneAccessOpen = useStore((s) => s.setPhoneAccessOpen);
  const updateSettings = useStore((s) => s.updateSettings);
  const setUpdateAutoApply = useStore((s) => s.setUpdateAutoApply);
  const setUpdateShellAutoApply = useStore((s) => s.setUpdateShellAutoApply);
  const updateHoldVersion = useStore((s) => s.updateStatus?.holdVersion);
  const updateHoldExpiresMs = useStore((s) => s.updateStatus?.holdExpiresMs);
  // The channel's signed update-control default (daemon/updater_control.go:
  // off for public) can pause the unattended installer restart, and then
  // "Restart when idle" does nothing, so the row is hidden. Only an explicit
  // true hides it: a daemon older than the field shows the row as before. The
  // saved choice is left as it is and applies again if the channel allows it.
  const shellAutoApplyPaused = useStore((s) => s.updateStatus?.shellAutoApplyPaused === true);
  const releaseUpdateHold = useStore((s) => s.releaseUpdateHold);
  const desktop = isDesktopShell();
  const phoneAllowed = usePhoneAccessAllowed();
  // On macOS/Linux this row is a copyable install one-liner for another
  // computer, which is only useful with an account to reach it from.
  const os = detectVisitorOS();
  const showDesktopRow = phoneAllowed || (os !== 'mac' && os !== 'linux');
  const withoutAccount = useCloud((s) => s.withoutAccount && !s.session);
  const requestSignIn = useCloud((s) => s.requestSignIn);
  const signedInEmail = useCloud((s) => (s.session ? s.session.user?.email ?? '' : null));
  const signOut = useCloud((s) => s.signOut);
  const team = useTeamFeatures();
  const [autostartStatus, setAutostartStatus] = useState<AutostartStatus | null>(null);
  const [autostartError, setAutostartError] = useState<string | null>(null);
  const [autostartPending, setAutostartPending] = useState(false);
  const autostartPendingRef = useRef(false);

  useEffect(() => {
    let mounted = true;
    void getAutostartStatus().then((next) => {
      if (mounted) setAutostartStatus(next);
    });
    return () => {
      mounted = false;
    };
  }, []);

  // Live pairing status (same /api/cloud/status poll as the footer chip), so
  // an already-paired user sees "Connected" here — not a misleading "Set up".
  const { status } = usePhoneAccessStatus({ enabled: desktop });
  const chip = chipStateFor(status);

  const openPairPanel = () => {
    // The pair panel is its own overlay — close Settings first.
    useSettings.getState().closeSettings();
    setPhoneAccessOpen(true);
  };

  const changeAutostart = async (enabled: boolean) => {
    if (autostartPendingRef.current) return;
    const previous = autostartStatus;
    if (!previous) return;
    autostartPendingRef.current = true;
    setAutostartPending(true);
    setAutostartError(null);
    setAutostartStatus({ ...previous, enabled });
    try {
      setAutostartStatus(await setAutostart(enabled));
    } catch (error) {
      console.warn('desktop_set_autostart invoke failed:', error);
      setAutostartStatus(previous);
      setAutostartError(`Couldn't update launch-on-startup setting: ${String(error)}`);
    } finally {
      autostartPendingRef.current = false;
      setAutostartPending(false);
    }
  };

  return (
    <>
    {(withoutAccount || signedInEmail !== null || showDesktopRow || (desktop && phoneAllowed)) && (
    <SettingsGroup title="Devices">
      {withoutAccount && (
        <SettingsRow
          label="Account"
          hint="A free account adds phone and browser access, push alerts, voice and smart approvals."
        >
          <span className="settings-status">Not signed in</span>
          <button
            type="button"
            className="settings-btn"
            onClick={() => {
              useSettings.getState().closeSettings();
              requestSignIn();
            }}
          >Create free account or sign in</button>
        </SettingsRow>
      )}
      {signedInEmail !== null && (
        <SettingsRow label="Account" hint={team ? 'Team account.' : 'Free beta account.'}>
          <span className="settings-status">{signedInEmail ? `Signed in as ${signedInEmail}` : 'Signed in'}</span>
          <button
            type="button"
            className="settings-btn"
            onClick={async () => {
              const ok = await confirmDialog({
                message: 'Sign out? Your panes keep running. Phone access, push alerts, voice and smart approvals stop until you sign in again.',
                confirmLabel: 'Sign out',
              });
              if (ok) void signOut();
            }}
          >Sign out</button>
        </SettingsRow>
      )}
      {signedInEmail !== null && <MarketingConsentSetting />}
      <PersonalViewSetting />
      {showDesktopRow && (
        <SettingsRow
          label="Desktop app"
          hint={
            desktop
              ? 'Grab the installer for another computer, or reinstall by hand.'
              : 'Native window, tray, global hotkeys — and it keeps the daemon alive.'
          }
        >
          <DesktopInstallerLink className="settings-btn" style={{ textDecoration: 'none' }}>
            Download
          </DesktopInstallerLink>
        </SettingsRow>
      )}
      {desktop && phoneAllowed && (
        <SettingsRow
          label="Phone access"
          hint={
            chip.kind === 'online'
              ? 'Your phone can control this computer. Manage opens the pairing panel (re-pair, disconnect).'
              : chip.kind === 'signin'
                ? 'The cloud connection needs a fresh sign-in.'
                : 'Control this computer from your phone.'
          }
        >
          <span className={`settings-status ${chip.kind}`}>
            {chip.kind === 'online'
              ? `Connected${status?.ownerEmail ? ` — ${status.ownerEmail}` : ''}`
              : chip.kind === 'connecting'
                ? 'Connecting…'
                : chip.kind === 'signin'
                  ? 'Sign-in needed'
                  : 'Not set up'}
          </span>
          <button type="button" className="settings-btn" onClick={openPairPanel}>
            {chip.kind === 'online'
              ? 'Manage'
              : chip.kind === 'connecting'
                ? 'Open'
                : chip.kind === 'signin'
                  ? 'Sign in'
                  : 'Set up'}
          </button>
        </SettingsRow>
      )}
    </SettingsGroup>
    )}
    {autostartStatus?.supported && (
      <SettingsGroup title="Desktop">
        <SettingsCheck
          label="Launch on startup"
          checked={autostartStatus.enabled}
          disabled={autostartPending}
          onChange={(enabled) => void changeAutostart(enabled)}
          hint="Start SoluTerminal in the background when you sign in to your computer. Open it from the tray icon."
        />
        {autostartError && <p className="settings-row-hint settings-error">{autostartError}</p>}
      </SettingsGroup>
    )}
    <SettingsGroup title="Updates">
      <SettingsRow
        label="Automatic updates"
        hint="Install new versions in the background when nothing is mid-task. Your panes keep running; the window reconnects briefly. “Notify me” shows a banner so you update by hand."
      >
        <select
          className="settings-select"
          value={updateSettings?.autoApply ?? 'off'}
          onChange={(e) => void setUpdateAutoApply(e.target.value as UpdateAutoApplyMode).catch(() => {})}
        >
          <option value="off">Notify me</option>
          <option value="when-idle">Install when idle</option>
        </select>
      </SettingsRow>
      {!shellAutoApplyPaused && (
      <SettingsRow
        label="Desktop app restarts"
        hint="Update the desktop app and relaunch it once you're idle (no agent working, nothing waiting for you, no recent typing). A 60-second countdown comes first and can be postponed for an hour."
      >
        <select
          className="settings-select"
          value={updateSettings?.shellAutoApply ?? 'off'}
          onChange={(e) => void setUpdateShellAutoApply(e.target.value as UpdateAutoApplyMode).catch(() => {})}
        >
          <option value="off">Notify me</option>
          <option value="when-idle">Restart when idle</option>
        </select>
      </SettingsRow>
      )}
      {updateHoldVersion && (
        <SettingsRow
          label="Paused updates"
          hint={`Updates to v${updateHoldVersion} are paused${updateHoldExpiresMs ? ` until ${new Date(updateHoldExpiresMs).toLocaleString()}` : ''}.`}
        >
          <button type="button" className="settings-btn" onClick={() => void releaseUpdateHold()}>
            Resume updates
          </button>
        </SettingsRow>
      )}
    </SettingsGroup>
    {slotList('settingsSections').filter((section) => section.tab === 'devices').map((section, i) => <section.Component key={i} />)}
    <AgentIntegrationSetting />
    <AgentAuthoritySetting />
    <RemoteDevicesSetting />
    <CheckinSetting />
    </>
  );
}
