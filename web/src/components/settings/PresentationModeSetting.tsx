import { useHideIdentities } from '../../lib/hideIdentities';
import { SettingsCheck } from './SettingsRow';

export const PRESENTATION_MODE_LABEL = 'Presentation mode';
export const PRESENTATION_MODE_HINT =
  'For screenshots and videos: hides your email in the sidebar, Account settings and the command palette, ' +
  'hides the status-bar badge, and masks account names on panes. ' +
  'Other pages and text inside terminals aren\'t changed. Only changes this app on this device.';

// The switch for the shared hide-identities preference (lib/hideIdentities.ts).
// The hint names exactly the surfaces that honour it; a surface added to it
// needs its own test that no full email renders, and a line here.
export function PresentationModeSetting() {
  const [on, setOn] = useHideIdentities();
  return (
    <SettingsCheck
      label={PRESENTATION_MODE_LABEL}
      checked={on}
      onChange={setOn}
      hint={PRESENTATION_MODE_HINT}
    />
  );
}
