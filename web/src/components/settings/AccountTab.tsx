import { useTeamFeatures } from '../../entitlements';
import { PersonalViewSetting } from './PersonalViewSetting';
import { PresentationModeSetting } from './PresentationModeSetting';
import { usePresentationMode } from '../../lib/hideIdentities';
import { confirmSignOut } from '../../lib/confirmSignOut';
import { useSettings } from '../../settings';
import { useCloud } from '../../cloud';
import { SettingsGroup, SettingsRow } from './SettingsRow';
import { MarketingConsentSetting } from './MarketingConsentSetting';

// Settings › Account: sign-in state, product emails, the personal-view
// switch and Presentation mode. These rows lived at the top of Settings ›
// Devices until the tab was split (2026-10-31); their behaviour is unchanged.

export function AccountTab() {
  const withoutAccount = useCloud((s) => s.withoutAccount && !s.session);
  const requestSignIn = useCloud((s) => s.requestSignIn);
  const signedInEmail = useCloud((s) => (s.session ? s.session.user?.email ?? '' : null));
  const signOut = useCloud((s) => s.signOut);
  const team = useTeamFeatures();
  const presentation = usePresentationMode();

  return (
    <SettingsGroup title="Account">
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
        <SettingsRow label="Account" hint={team ? 'Team plan.' : 'Free beta account.'}>
          <span className="settings-status">{signedInEmail && !presentation ? `Signed in as ${signedInEmail}` : 'Signed in'}</span>
          <button
            type="button"
            className="settings-btn"
            onClick={async () => {
              if (await confirmSignOut()) void signOut();
            }}
          >Sign out</button>
        </SettingsRow>
      )}
      {signedInEmail !== null && <MarketingConsentSetting />}
      <PersonalViewSetting />
      <PresentationModeSetting />
    </SettingsGroup>
  );
}
