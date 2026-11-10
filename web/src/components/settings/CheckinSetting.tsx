import { useEffect, useRef, useState } from 'react';
import { api } from '../../api';
import type { PrivacySettings } from '../../api';
import { SITE_PRIVACY_URL } from '../../lib/siteLinks';
import { openInSystemBrowser } from '../../tauri-browser';
import { SettingsCheck, SettingsGroup } from './SettingsRow';

export const CHECKIN_LABEL = 'Send an anonymous daily check-in';
export const CHECKIN_HINT =
  'Sends an install ID, app version, OS, whether you’re signed in, and two yes/no answers about ' +
  'the last day: did you start a Claude Code or Codex pane, and did one agent work with another ' +
  'pane. Helps us count active installs.';
export const USAGE_LABEL = 'Share usage statistics';
export const USAGE_HINT =
  'For each Claude Code or Codex turn: model, tokens, project folder, git remote and branch. ' +
  'Sent while this computer is connected to your account, even after you sign out. ' +
  'Turning this on or connecting can also send earlier turns still on this computer.';
export const CRASH_LABEL = 'Send crash reports';
export const CRASH_HINT =
  "When SoluTerminal's background service hits an error: the error and where in our code it " +
  'happened. Never terminal text, keys or your own folder names.';

// The daemon owns these preferences (it does the sending), so the toggles
// read and write them there. A daemon without an endpoint answers 404 and
// that row stays hidden rather than offering a switch that does nothing.
export function CheckinSetting() {
  const [checkin, setCheckin] = useState<boolean | null>(null);
  const [privacy, setPrivacy] = useState<PrivacySettings | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const pendingRef = useRef(false);

  useEffect(() => {
    let mounted = true;
    void api
      .getCheckinSettings()
      .then((s) => {
        if (mounted && s) setCheckin(s.enabled);
      })
      .catch(() => {
        /* unreachable daemon: keep the row hidden */
      });
    void api
      .getPrivacySettings()
      .then((s) => {
        if (mounted && s) setPrivacy(s);
      })
      .catch(() => {
        /* older or unreachable daemon: keep the rows hidden */
      });
    return () => {
      mounted = false;
    };
  }, []);

  if (checkin === null && privacy === null) return null;

  // One save at a time; the row flips immediately and rolls back on failure.
  const save = async (
    apply: () => void,
    rollback: () => void,
    send: () => Promise<void>,
    what: string,
  ) => {
    if (pendingRef.current) return;
    pendingRef.current = true;
    setPending(true);
    setError(null);
    apply();
    try {
      await send();
    } catch (e) {
      rollback();
      setError(`Couldn't save the ${what} setting: ${String(e)}`);
    } finally {
      pendingRef.current = false;
      setPending(false);
    }
  };

  const changeCheckin = (next: boolean) => {
    const previous = checkin;
    void save(
      () => setCheckin(next),
      () => setCheckin(previous),
      async () => setCheckin((await api.setCheckinSettings(next)).enabled),
      'check-in',
    );
  };

  const changePrivacy = (key: 'usageUpload' | 'crashReports', next: boolean, what: string) => {
    const previous = privacy;
    if (!previous) return;
    void save(
      () => setPrivacy({ ...previous, [key]: next }),
      () => setPrivacy(previous),
      async () => setPrivacy(await api.setPrivacySettings({ [key]: next })),
      what,
    );
  };

  return (
    <SettingsGroup title="Privacy">
      {checkin !== null && (
        <SettingsCheck
          label={CHECKIN_LABEL}
          checked={checkin}
          disabled={pending}
          onChange={changeCheckin}
          hint={CHECKIN_HINT}
        />
      )}
      {privacy && (
        <SettingsCheck
          label={USAGE_LABEL}
          checked={privacy.usageUpload}
          disabled={pending || privacy.usageUploadEnvDisabled}
          onChange={(next) => changePrivacy('usageUpload', next, 'usage statistics')}
          hint={USAGE_HINT}
        />
      )}
      {privacy && (
        <SettingsCheck
          label={CRASH_LABEL}
          checked={privacy.crashReports}
          disabled={pending || privacy.crashReportsEnvDisabled}
          onChange={(next) => changePrivacy('crashReports', next, 'crash report')}
          hint={CRASH_HINT}
        />
      )}
      <p className="settings-row-hint">
        These can only be changed on this computer.{' '}
        {/* A plain target=_blank anchor does nothing in the desktop app. */}
        <a
          href={SITE_PRIVACY_URL}
          onClick={(e) => {
            e.preventDefault();
            void openInSystemBrowser(SITE_PRIVACY_URL, e.nativeEvent);
          }}
        >
          Privacy policy
        </a>
      </p>
      {error && <p className="settings-row-hint settings-error">{error}</p>}
    </SettingsGroup>
  );
}
