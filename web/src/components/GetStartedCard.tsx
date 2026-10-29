import { useEffect, useState } from 'react';
import { useCloud } from '../cloud';
import { countSiteDownload } from '../lib/countDownload';
import { SITE_DOWNLOAD_URL } from '../lib/siteLinks';
import { HANDHELD_WINDOWS_ONLY, DESKTOP_WINDOWS_ONLY, WINDOWS_VERSIONS_NOTE } from '../lib/windowsOnlyCopy';
import { detectVisitorOS, isHandheldVisitor } from './DesktopInstallerLink';
import { useIsMobileDevice } from './useIsMobileView';

// The hosted app's first screen for an account with no machines, however it
// signed in: download the Windows app from the website, sign in there with
// the same email, and the PC shows up here. Visitors who aren't on Windows
// are told it is a Windows app and get the link to take to their PC instead
// (shared from a phone or tablet, copied on a Mac or Linux desktop); never
// the headless install command.

export const GET_STARTED_TITLE = "You're in. Three steps to your first team.";

// Which first step a visitor gets. Any Windows browser (ARM and touch PCs
// included) downloads the app; phones and tablets, iPads among them, and
// Mac or Linux desktops get the link for their PC. A browser we can't place
// keeps the Windows download.
export type DownloadRoute = 'windows' | 'handheld' | 'desktop';

export function downloadRoute(isMobile: boolean): DownloadRoute {
  const os = detectVisitorOS();
  if (os === 'windows') return 'windows';
  if (isMobile || isHandheldVisitor()) return 'handheld';
  return os === 'other' ? 'windows' : 'desktop';
}

export function usesWindowsDownload(isMobile: boolean): boolean {
  return downloadRoute(isMobile) === 'windows';
}

/** A direct link to the website's installer file, counted as the account path. */
export function SiteInstallerLink({ className, children }: { className?: string; children: React.ReactNode }) {
  const installer = useCloud((s) => s.siteInstaller);
  return (
    <a className={className} href={installer.url} download onClick={countSiteDownload}>
      {children}
    </a>
  );
}

// The page heading already is GET_STARTED_TITLE: only an account with no
// machines sees these steps.
export function GetStartedCard({ email }: { email: string | null }) {
  const isMobile = useIsMobileDevice();
  const route = downloadRoute(isMobile);
  const size = useCloud((s) => s.siteInstaller.size);

  return (
    <section className="cloud-gate-start" aria-label={GET_STARTED_TITLE}>
      <ol className="cloud-gate-start-steps">
        <li>
          {route === 'windows' ? (
            <>
              <strong>Download SoluTerminal for Windows</strong>
              <SiteInstallerLink className="cloud-gate-start-download">Download for Windows</SiteInstallerLink>
              <span className="cloud-gate-footnote">{size}, from soluterminal.com.</span>
            </>
          ) : (
            <OtherDeviceStep handheld={route === 'handheld'} />
          )}
        </li>
        <li>
          <strong>Open it and sign in with the same email.</strong>{' '}
          {email && <span className="cloud-gate-start-email">{email}</span>}{' '}
          We&apos;ll send a new code.
        </li>
        <li>
          <strong>Your PC appears here.</strong> Then you can check on your agents from this page, on any
          device.
          <span className="cloud-gate-footnote">
            When the app asks &ldquo;Connect this PC to your account?&rdquo;, choose Connect.
          </span>
        </li>
      </ol>
    </section>
  );
}

function OtherDeviceStep({ handheld }: { handheld: boolean }) {
  const [shared, setShared] = useState<'idle' | 'copied' | 'failed'>('idle');
  useEffect(() => {
    if (shared !== 'copied') return;
    const t = window.setTimeout(() => setShared('idle'), 1800);
    return () => window.clearTimeout(t);
  }, [shared]);
  const share = async () => {
    // A phone's share sheet sends the link to the PC however the visitor
    // likes. Desktops copy it: their share sheets are rarely how a link
    // reaches another computer.
    if (handheld && typeof navigator.share === 'function') {
      try {
        await navigator.share({ url: SITE_DOWNLOAD_URL });
        return;
      } catch (e) {
        // Closing the sheet is a choice, not a failure.
        if (e instanceof DOMException && e.name === 'AbortError') return;
      }
    }
    try {
      await navigator.clipboard.writeText(SITE_DOWNLOAD_URL);
      setShared('copied');
    } catch {
      // No clipboard (insecure origin, old browser): show the address to select.
      setShared('failed');
    }
  };
  return (
    <>
      <strong>{handheld ? HANDHELD_WINDOWS_ONLY : DESKTOP_WINDOWS_ONLY}</strong>
      <span className="cloud-gate-start-actions">
        <button type="button" className="cloud-gate-link" onClick={() => void share()}>
          {shared === 'copied' ? 'Copied' : handheld ? 'Share link' : 'Copy link'}
        </button>
      </span>
      {shared === 'failed' && <span className="cloud-gate-start-address">{SITE_DOWNLOAD_URL}</span>}
      <span className="cloud-gate-footnote">{WINDOWS_VERSIONS_NOTE}</span>
    </>
  );
}
