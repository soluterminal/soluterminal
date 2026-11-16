import { useEffect, useState } from 'react';
import { useDeveloperOverrides } from '../../lib/developerOverrides';
import { SettingsGroup, SettingsRow } from './SettingsRow';

export const DEVELOPER_OVERRIDES_LABEL = 'Developer overrides';
export const DEVELOPER_OVERRIDES_HINT =
  'Lets local builds be installed on this computer (the Preview channel and follow-main) and lets a dev-web folder ' +
  'replace the app\'s built-in web files. Leave it off unless you develop SoluTerminal. Switching the update channel ' +
  'turns it on for you.';

// The daemon enforces the gate (daemon/developer_overrides.go); this row shows
// and changes it. Hidden on a daemon without the endpoint.
export function DeveloperOverridesSetting() {
  const { loaded, on, required, refresh, set } = useDeveloperOverrides();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  if (!loaded) return null;

  const change = async (next: boolean) => {
    if (pending) return;
    setPending(true);
    setError(null);
    try {
      await set(next);
    } catch (e) {
      setError(`Couldn't save developer overrides: ${String(e)}`);
    } finally {
      setPending(false);
    }
  };

  return (
    <SettingsGroup title="Developer overrides">
      <SettingsRow
        label={DEVELOPER_OVERRIDES_LABEL}
        hint={
          <>
            {DEVELOPER_OVERRIDES_HINT}
            {!required && <> This is a development build, where they are always allowed.</>}
          </>
        }
      >
        <select
          className="settings-select"
          value={on ? 'on' : 'off'}
          disabled={pending}
          onChange={(e) => void change(e.target.value === 'on')}
        >
          <option value="off">Off</option>
          <option value="on">On</option>
        </select>
      </SettingsRow>
      {on && (
        <p className="settings-row-hint">
          <strong>Developer overrides are on.</strong>
        </p>
      )}
      <p className="settings-row-hint">This can only be turned on at this computer.</p>
      {error && <p className="settings-row-hint settings-error">{error}</p>}
    </SettingsGroup>
  );
}
