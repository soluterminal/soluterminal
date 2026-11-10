import { SITE_PRIVACY_URL } from '../lib/siteLinks';
import { openInSystemBrowser } from '../tauri-browser';

// The welcome tour's last slide says plainly what leaves this computer, so
// nobody learns it from the privacy page after the fact. Keep it in step with
// the privacy policy and the Privacy settings group (CheckinSetting.tsx).
export function WelcomePrivacyNote({ account }: { account: boolean }) {
  return (
    <div className="welcome-privacy-note" style={{ marginTop: 12, fontSize: 12, color: 'var(--fg-dim)' }}>
      <strong style={{ color: 'var(--fg)' }}>What leaves this computer</strong>
      <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
        <li>
          An anonymous daily check-in: app version, OS, a random install id, and yes or no for whether a Claude
          Code or Codex pane was started and whether two agents worked together. You can turn it off in Settings.
        </li>
        {account ? (
          <>
            <li>Phone access goes through our relay while you use it.</li>
            <li>Voice: your recording, plus the last few lines of terminal text so names and commands come out right.</li>
          </>
        ) : (
          <li>
            If you sign in to a free account, remote access and voice also go through our servers. The privacy
            policy lists exactly what.
          </li>
        )}
        <li>Smart approvals, if you turn them on, send the text of each approval prompt to our approval service.</li>
      </ul>
      <a
        href={SITE_PRIVACY_URL}
        onClick={(e) => {
          e.preventDefault();
          void openInSystemBrowser(SITE_PRIVACY_URL, e.nativeEvent);
        }}
      >
        Privacy policy
      </a>
    </div>
  );
}
