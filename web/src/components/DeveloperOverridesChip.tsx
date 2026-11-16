import { useEffect } from 'react';
import { useDeveloperOverrides } from '../lib/developerOverrides';
import { useSettings } from '../settings';
import { IconAlertTriangle } from './ui/icons';

// Status-bar indicator for Developer overrides (daemon/developer_overrides.go):
// shown for as long as the setting is on, so the person always knows local
// builds and a dev-web folder can change what this computer runs. Click opens
// Settings › Privacy & security. Renders its own leading separator, and
// nothing while the setting is off.
export function DeveloperOverridesChip() {
  const on = useDeveloperOverrides((s) => s.on);
  const refresh = useDeveloperOverrides((s) => s.refresh);
  useEffect(() => {
    void refresh();
  }, [refresh]);
  if (!on) return null;
  const title = 'Developer overrides are on: local builds and a dev-web folder can change this app. Click to review.';
  return (
    <>
      <span className="statusbar-sep">·</span>
      <button
        type="button"
        className="statusbar-dev-overrides"
        onClick={() => useSettings.getState().openSettings('privacy')}
        title={title}
        aria-label={title}
        data-tauri-drag-region={false}
      >
        <IconAlertTriangle size={13} />
        <span>Developer overrides on</span>
      </button>
    </>
  );
}
