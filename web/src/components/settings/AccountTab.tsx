import { usePersonalViewAvailable, useTeamFeatures } from '../../entitlements';
import { PersonalViewSetting } from './PersonalViewSetting';
import { confirmDialog } from '../../lib/dialog';
import { useSettings } from '../../settings';
import { useCloud } from '../../cloud';
import { SettingsGroup, SettingsRow } from './SettingsRow';
import { MarketingConsentSetting } from './MarketingConsentSetting';

// Settings › Account: sign-in state, product emails and the personal-view
// switch. These rows lived at the top of Settings › Devices until the tab was
// split (2026-10-31); their behaviour is unchanged.

/** Whether any Account row applies. The modal hides the tab otherwise. */
export function useAccountTabVisible(): boolean {
  const withoutAccount = useCloud((s) => s.withoutAccount && !s.session);
  const signedIn = useCloud((s) => !!s.session);
  const personalView = usePersonalViewAvailable();
  return withoutAccount || signedIn || personalView;
}

export function AccountTab() {
  const withoutAccount = useCloud((s) => s.withoutAccount && !s.session);
  const requestSignIn = useCloud((s) => s.requestSignIn);
  const signedInEmail = useCloud((s) => (s.session ? s.session.user?.email ?? '' : null));
  const signOut = useCloud((s) => s.signOut);
  const team = useTeamFeatures();

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
    </SettingsGroup>
  );
}
