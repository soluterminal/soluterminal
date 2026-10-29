import { useState } from 'react';
import type { CSSProperties, ReactNode } from 'react';
import { showEmbedChrome } from '../embedChrome';
import { SITE_DOWNLOAD_URL } from '../lib/siteLinks';
import { WINDOWS_VERSIONS_NOTE } from '../lib/windowsOnlyCopy';

export type VisitorOS = 'windows' | 'mac' | 'linux' | 'other';

// Best-effort visitor-OS sniff. We only need a coarse three-way split:
// Windows gets the desktop app; macOS/Linux are told it is a Windows app
// (there is no mac/Linux desktop bundle); anything we can't classify
// falls back to the Windows download. Exported so CloudGate's host/claim
// instructions and the get-started card can route visitors.
export function detectVisitorOS(): VisitorOS {
  if (typeof navigator === 'undefined') return 'other';
  const ua = navigator.userAgent || '';
  const platform = navigator.platform || '';
  if (/Win/i.test(platform) || /Windows/i.test(ua)) return 'windows';
  // Treat iPad/iPhone like macOS: no desktop app either way.
  if (/Mac/i.test(platform) || /Macintosh|Mac OS X|iPhone|iPad|iPod/i.test(ua)) {
    return 'mac';
  }
  if (/Linux|X11/i.test(platform) || /Linux/i.test(ua)) return 'linux';
  return 'other';
}

// A phone or tablet by its user agent, whatever its screen width: iOS and
// Android say so, and iPadOS reports itself as a Mac but has a touch screen,
// which Macs don't. detectVisitorOS files all of these under mac or linux.
export function isHandheldVisitor(): boolean {
  if (typeof navigator === 'undefined') return false;
  const ua = navigator.userAgent || '';
  if (/iPhone|iPad|iPod|Android/i.test(ua)) return true;
  const mac = /Mac/i.test(navigator.platform || '') || /Macintosh/i.test(ua);
  return mac && (navigator.maxTouchPoints ?? 0) > 1;
}

// The website's download page, not the cloud's installer redirect: that
// redirect lands on the release host's URL, and the download page carries
// the checksum and the install steps too.
export function desktopInstallerUrl(): string {
  return SITE_DOWNLOAD_URL;
}

export function DesktopInstallerLink({
  children = 'Download desktop app',
  className,
  style,
}: {
  children?: ReactNode;
  className?: string;
  style?: CSSProperties;
}) {
  if (!showEmbedChrome('desktopInstaller')) return null;
  const os = detectVisitorOS();

  // macOS/Linux: no desktop bundle exists, so the .exe is useless. Say so and
  // offer the link to take to a Windows PC. Never the headless install
  // command: it fetches from the release host, which stays private.
  if (os === 'mac' || os === 'linux') {
    return <WindowsOnlyNote className={className} style={style} />;
  }

  // Windows (and any OS we couldn't classify): unchanged — link to the .exe.
  return (
    <a
      href={desktopInstallerUrl()}
      className={className}
      style={style}
      target="_blank"
      rel="noreferrer"
    >
      {children}
    </a>
  );
}

function WindowsOnlyNote({ className, style }: { className?: string; style?: CSSProperties }) {
  const [copied, setCopied] = useState<'idle' | 'copied' | 'failed'>('idle');
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(SITE_DOWNLOAD_URL);
      setCopied('copied');
    } catch {
      // No clipboard (insecure origin): the link is shown inline to select.
      setCopied('failed');
    }
    window.setTimeout(() => setCopied('idle'), 1600);
  };

  return (
    <span
      style={{ display: 'inline-flex', flexDirection: 'column', gap: 4, minWidth: 0, maxWidth: '100%', ...style }}
    >
      <button
        type="button"
        className={className}
        onClick={() => void copy()}
        title="SoluTerminal is a Windows app. Copy the link to open on your Windows PC."
        style={{ cursor: 'pointer' }}
      >
        {copied === 'copied' ? 'Copied' : 'Copy link'}
      </button>
      <span style={{ fontSize: 11, opacity: 0.75, overflowWrap: 'anywhere' }}>
        {WINDOWS_VERSIONS_NOTE} Open {SITE_DOWNLOAD_URL} on your Windows PC
        {copied === 'failed' ? ' (select and copy the link)' : ''}.
      </span>
    </span>
  );
}
