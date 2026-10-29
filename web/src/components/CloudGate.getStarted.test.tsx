import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { Session } from '@supabase/supabase-js';
import { useCloud, type CloudMachine } from '../cloud';
import { captureArrival } from '../lib/arrival';
import { CloudGate } from './CloudGate';

const WINDOWS_UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130 Safari/537.36';
const MAC_UA = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 Safari/605.1.15';
const WINDOWS_ARM_UA = 'Mozilla/5.0 (Windows NT 10.0; ARM64) AppleWebKit/537.36 Chrome/130 Safari/537.36 Edg/130';
const LINUX_UA = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/130 Safari/537.36';
const IPHONE_UA = 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148 Safari/604.1';
const ANDROID_UA = 'Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 Chrome/130 Mobile Safari/537.36';
const INSTALLER = 'https://soluterminal.com/download/SoluTerminal-Setup.exe';
const HANDHELD_COPY =
  "SoluTerminal is a Windows app. There's nothing to install on your phone. Open this page on your Windows PC to download it.";
// Written with \s so the source-wide check in windowsOnlyCopy.test.ts
// doesn't flag this file.
const NO_OTHER_PLATFORMS = /coming\ssoon/i;
const DESKTOP_COPY = 'SoluTerminal is a Windows app. Open this page on your Windows PC to download it.';

const machine: CloudMachine = { id: 'mach_1', name: 'desk', online: true, backends: ['host'] };

function visitor(ua: string, platform: string) {
  vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(ua);
  vi.spyOn(navigator, 'platform', 'get').mockReturnValue(platform);
}

// navigator.share and navigator.clipboard, removed again after each test.
function stub(name: 'share' | 'clipboard', value: unknown) {
  Object.defineProperty(navigator, name, { configurable: true, value });
}

function seed(machines: CloudMachine[] = []) {
  useCloud.setState({
    mode: 'cloud',
    isLocal: false,
    session: { access_token: 'token', user: { email: 'new@example.com' } } as Session,
    authorised: true,
    machines,
    machinesLoaded: true,
    selectedMachineId: null,
    error: null,
    bootstrap: vi.fn(async () => {}),
    signOut: vi.fn(async () => {}),
    refreshMachines: vi.fn(async () => {}),
  });
}

function entitlements(team: boolean) {
  return vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
    if (String(input).includes('/api/entitlements')) {
      return new Response(JSON.stringify({ tier: team ? 'team' : 'account', team }), { status: 200 });
    }
    return new Response('{}', { status: 404 });
  });
}

const renderGate = () => render(<CloudGate><div>app</div></CloudGate>);

describe('hosted app get-started card', () => {
  beforeEach(() => {
    useCloud.setState(useCloud.getInitialState(), true);
    sessionStorage.clear();
    window.history.replaceState(null, '', '/');
    Object.defineProperty(navigator, 'maxTouchPoints', { configurable: true, value: 0 });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    delete (navigator as { share?: unknown }).share;
    delete (navigator as { clipboard?: unknown }).clipboard;
    sessionStorage.clear();
    useCloud.setState(useCloud.getInitialState(), true);
  });

  it('greets a new account on Windows with the installer and the next steps', async () => {
    visitor(WINDOWS_UA, 'Win32');
    entitlements(false);
    seed();
    renderGate();

    expect(screen.getByRole('heading', { name: "You're in. Three steps to your first team." })).toBeInTheDocument();
    const download = screen.getByRole('link', { name: 'Download for Windows' });
    // The installer file on the website: never the release host, and never
    // /download/ itself, which would lead back here through the account step.
    expect(download).toHaveAttribute('href', INSTALLER);
    expect(screen.getByText(/74 MB, from soluterminal\.com\./)).toBeInTheDocument();
    expect(screen.getByText('Open it and sign in with the same email.')).toBeInTheDocument();
    expect(screen.getByText('new@example.com')).toBeInTheDocument();
    expect(screen.getByText(/your pc appears here\./i)).toBeInTheDocument();
    expect(screen.getByText(/connect this pc to your account\?/i)).toBeInTheDocument();
    // The developer actions move under "Other ways to connect".
    expect(screen.getByText('Other ways to connect')).toBeInTheDocument();
    expect(document.body.innerHTML).not.toMatch(/github|install\.sh|curl/i);
    // The website's look: its mark and lime primary, same as the sign-in.
    const shell = document.querySelector('.cloud-gate-shell');
    expect(shell).toHaveClass('cloud-brand');
    expect(shell?.querySelector('.cloud-brand-bar')).toHaveTextContent('SoluTerminalbeta');
  });

  it('keeps the install command out of "Other ways to connect" for public users', () => {
    visitor(WINDOWS_UA, 'Win32');
    entitlements(false);
    seed();
    renderGate();
    fireEvent.click(screen.getByText('Other ways to connect'));
    fireEvent.click(screen.getByRole('button', { name: /run soluterminal on this computer/i }));
    fireEvent.click(screen.getByRole('button', { name: /claim a new machine/i }));
    expect(document.body.textContent).not.toMatch(/install\.sh|curl|github/i);
  });

  it('uses the installer the cloud names', () => {
    visitor(WINDOWS_UA, 'Win32');
    entitlements(false);
    seed();
    useCloud.setState({ siteInstaller: { url: 'https://www.soluterminal.com/download/SoluTerminal-Setup.exe', size: '80 MB' } });
    renderGate();
    expect(screen.getByRole('link', { name: 'Download for Windows' })).toHaveAttribute(
      'href',
      'https://www.soluterminal.com/download/SoluTerminal-Setup.exe',
    );
    expect(screen.getByText(/80 MB, from soluterminal\.com\./)).toBeInTheDocument();
  });

  it('tells Mac visitors it is a Windows app and offers the link to copy, not an install command', () => {
    visitor(MAC_UA, 'MacIntel');
    entitlements(false);
    seed();
    renderGate();

    expect(screen.getByText(DESKTOP_COPY)).toBeInTheDocument();
    expect(screen.queryByText(HANDHELD_COPY)).toBeNull();
    expect(screen.getByText('SoluTerminal runs on Windows 10 & 11.')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Download for Windows' })).toBeNull();
    // Desktops copy; only phones and tablets get the share sheet.
    expect(screen.getByRole('button', { name: 'Copy link' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Share link' })).toBeNull();
    expect(screen.queryByText('Email me the link')).toBeNull();
    expect(document.querySelector('a[href^="mailto:"]')).toBeNull();
    expect(document.body.textContent).not.toMatch(NO_OTHER_PLATFORMS);
    expect(document.body.innerHTML).not.toMatch(/github|install\.sh|curl/i);
  });

  it('gives Linux desktops the same desktop step', () => {
    visitor(LINUX_UA, 'Linux x86_64');
    entitlements(false);
    seed();
    renderGate();
    expect(screen.getByText(DESKTOP_COPY)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Copy link' })).toBeInTheDocument();
    expect(document.querySelector('a[href^="mailto:"]')).toBeNull();
  });

  it('copies the download page link on a Mac even when a share sheet exists', async () => {
    visitor(MAC_UA, 'MacIntel');
    entitlements(false);
    const share = vi.fn(async () => {});
    const writeText = vi.fn(async () => {});
    stub('share', share);
    stub('clipboard', { writeText });
    seed();
    renderGate();
    fireEvent.click(screen.getByRole('button', { name: 'Copy link' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('https://soluterminal.com/download/'));
    expect(await screen.findByRole('button', { name: 'Copied' })).toBeInTheDocument();
    expect(share).not.toHaveBeenCalled();
  });

  it('shows the address to select when a Mac cannot copy', async () => {
    visitor(MAC_UA, 'MacIntel');
    entitlements(false);
    stub('clipboard', { writeText: vi.fn(async () => { throw new Error('denied'); }) });
    seed();
    renderGate();
    expect(screen.queryByText('https://soluterminal.com/download/')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Copy link' }));
    expect(await screen.findByText('https://soluterminal.com/download/')).toHaveClass('cloud-gate-start-address');
  });

  it.each([
    ['an iPhone', IPHONE_UA, 'iPhone', 5],
    ['an Android phone', ANDROID_UA, 'Linux armv81', 5],
    ['an iPad that reports itself as a Mac', MAC_UA, 'MacIntel', 5],
  ])('tells %s there is nothing to install on it', (_name, ua, platform, touchPoints) => {
    visitor(ua, platform);
    Object.defineProperty(navigator, 'maxTouchPoints', { configurable: true, value: touchPoints });
    entitlements(false);
    seed();
    renderGate();

    expect(screen.getByText(HANDHELD_COPY)).toBeInTheDocument();
    expect(screen.queryByText(DESKTOP_COPY)).toBeNull();
    expect(screen.getByRole('button', { name: 'Share link' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Copy link' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Download for Windows' })).toBeNull();
    expect(document.querySelector('a[href^="mailto:"]')).toBeNull();
    expect(document.body.textContent).not.toMatch(NO_OTHER_PLATFORMS);
    // Steps 2 and 3 are unchanged.
    expect(screen.getByText('Open it and sign in with the same email.')).toBeInTheDocument();
    expect(screen.getByText(/your pc appears here\./i)).toBeInTheDocument();
  });

  it('opens the share sheet with the download page on a phone', async () => {
    visitor(IPHONE_UA, 'iPhone');
    entitlements(false);
    const share = vi.fn(async () => {});
    const writeText = vi.fn(async () => {});
    stub('share', share);
    stub('clipboard', { writeText });
    seed();
    renderGate();
    fireEvent.click(screen.getByRole('button', { name: 'Share link' }));
    await waitFor(() => expect(share).toHaveBeenCalledWith({ url: 'https://soluterminal.com/download/' }));
    expect(writeText).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Share link' })).toBeInTheDocument();
  });

  it('does nothing more when the visitor closes the share sheet', async () => {
    visitor(IPHONE_UA, 'iPhone');
    entitlements(false);
    const share = vi.fn(async () => { throw new DOMException('cancelled', 'AbortError'); });
    const writeText = vi.fn(async () => {});
    stub('share', share);
    stub('clipboard', { writeText });
    seed();
    renderGate();
    fireEvent.click(screen.getByRole('button', { name: 'Share link' }));
    await waitFor(() => expect(share).toHaveBeenCalled());
    expect(writeText).not.toHaveBeenCalled();
    expect(screen.queryByText('https://soluterminal.com/download/')).toBeNull();
  });

  it('copies the link on a phone without a share sheet', async () => {
    visitor(ANDROID_UA, 'Linux armv81');
    entitlements(false);
    const writeText = vi.fn(async () => {});
    stub('clipboard', { writeText });
    seed();
    renderGate();
    expect('share' in navigator).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: 'Share link' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('https://soluterminal.com/download/'));
    expect(await screen.findByRole('button', { name: 'Copied' })).toBeInTheDocument();
  });

  it('shows the address to select on a phone that can neither share nor copy', async () => {
    visitor(IPHONE_UA, 'iPhone');
    entitlements(false);
    seed();
    renderGate();
    expect('clipboard' in navigator).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: 'Share link' }));
    expect(await screen.findByText('https://soluterminal.com/download/')).toHaveClass('cloud-gate-start-address');
    expect(screen.getByRole('button', { name: 'Share link' })).toBeInTheDocument();
  });

  it('falls back to copying when the share sheet fails', async () => {
    visitor(IPHONE_UA, 'iPhone');
    entitlements(false);
    const writeText = vi.fn(async () => {});
    stub('share', vi.fn(async () => { throw new DOMException('blocked', 'NotAllowedError'); }));
    stub('clipboard', { writeText });
    seed();
    renderGate();
    fireEvent.click(screen.getByRole('button', { name: 'Share link' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('https://soluterminal.com/download/'));
    expect(await screen.findByRole('button', { name: 'Copied' })).toBeInTheDocument();
  });

  it.each([
    ['Windows on ARM', WINDOWS_ARM_UA, 'Win32', 0],
    ['a Windows touch PC', WINDOWS_UA, 'Win32', 10],
  ])('keeps the Windows download for %s', (_name, ua, platform, touchPoints) => {
    visitor(ua, platform);
    Object.defineProperty(navigator, 'maxTouchPoints', { configurable: true, value: touchPoints });
    entitlements(false);
    seed();
    renderGate();
    expect(screen.getByRole('link', { name: 'Download for Windows' })).toHaveAttribute('href', INSTALLER);
    expect(screen.queryByText(HANDHELD_COPY)).toBeNull();
    expect(screen.queryByText(DESKTOP_COPY)).toBeNull();
  });

  it.each(['start', 'signin', 'download'])(
    'greets a new account from the website (intent=%s) with the first-run steps',
    (intent) => {
      visitor(WINDOWS_UA, 'Win32');
      entitlements(false);
      window.history.replaceState(null, '', `/?intent=${intent}&ref=hn`);
      captureArrival();
      seed();
      renderGate();

      expect(screen.getByRole('heading', { name: "You're in. Three steps to your first team." })).toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'Download for Windows' })).toHaveAttribute('href', INSTALLER);
      expect(screen.queryByRole('heading', { name: 'Your machines' })).toBeNull();
      expect(screen.queryByRole('button', { name: 'Hide' })).toBeNull();
    },
  );

  it.each(['start', 'signin', 'download'])(
    'takes a returning user with machines straight to them (intent=%s)',
    (intent) => {
      visitor(WINDOWS_UA, 'Win32');
      entitlements(false);
      window.history.replaceState(null, '', `/?intent=${intent}&ref=hn`);
      captureArrival();
      seed([machine]);
      renderGate();

      expect(screen.getByRole('heading', { name: 'Your machines' })).toBeInTheDocument();
      expect(screen.getByText('desk')).toBeInTheDocument();
      expect(screen.queryByText("You're in. Three steps to your first team.")).toBeNull();
      expect(screen.queryByText('Open it and sign in with the same email.')).toBeNull();
      // Only the small link under the list.
      const link = screen.getByRole('link', { name: 'Download for Windows' });
      expect(link).toHaveAttribute('href', INSTALLER);
      expect(link).not.toHaveClass('cloud-gate-start-download');
      expect(link.closest('.cloud-gate-footnote')).toHaveTextContent('Setting up another computer? Download for Windows');
    },
  );

  it('waits for the machine list instead of flashing the new-account steps', async () => {
    visitor(WINDOWS_UA, 'Win32');
    entitlements(false);
    window.history.replaceState(null, '', '/?intent=signin');
    captureArrival();
    seed();
    useCloud.setState({ machinesLoaded: false });
    renderGate();

    expect(screen.getByRole('status')).toHaveTextContent('Loading your machines…');
    expect(screen.queryByText("You're in. Three steps to your first team.")).toBeNull();
    act(() => useCloud.setState({ machines: [machine], machinesLoaded: true }));
    expect(screen.getByRole('heading', { name: 'Your machines' })).toBeInTheDocument();
    expect(screen.queryByText("You're in. Three steps to your first team.")).toBeNull();
  });

  it('offers a retry when the first machine list fails', () => {
    visitor(WINDOWS_UA, 'Win32');
    entitlements(false);
    seed();
    const refreshMachines = vi.fn(async () => {});
    useCloud.setState({ machinesLoaded: false, error: 'machines fetch: 503', refreshMachines });
    renderGate();
    expect(screen.getByRole('status')).toHaveTextContent("Couldn't load your machines.");
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(refreshMachines).toHaveBeenCalled();
  });

  it('marks the list loaded once the first fetch answers, even when it is empty', async () => {
    useCloud.setState({ session: { access_token: 'token' } as Session });
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response('[]', { status: 200 }));
    expect(useCloud.getState().machinesLoaded).toBe(false);
    await useCloud.getState().refreshMachines();
    expect(useCloud.getState().machinesLoaded).toBe(true);
    expect(useCloud.getState().machines).toEqual([]);
  });

  it('counts the hosted download as the account path, and falls back before the migration', async () => {
    visitor(WINDOWS_UA, 'Win32');
    entitlements(false);
    window.history.replaceState(null, '', '/?intent=start&utm_source=newsletter');
    captureArrival();
    const rpc = vi
      .fn()
      .mockResolvedValueOnce({ error: { code: 'PGRST202' } })
      .mockResolvedValue({ error: null });
    seed();
    useCloud.setState({ supabase: { rpc } as never });
    renderGate();
    const link = screen.getByRole('link', { name: 'Download for Windows' });
    link.addEventListener('click', (e) => e.preventDefault());
    fireEvent.click(link);
    await waitFor(() => expect(rpc).toHaveBeenCalledTimes(2));
    expect(rpc).toHaveBeenNthCalledWith(1, 'record_soluterminal_download', {
      p_source: 'newsletter',
      p_path: 'account',
    });
    expect(rpc).toHaveBeenNthCalledWith(2, 'record_soluterminal_download', { p_source: 'newsletter' });
  });

  it('keeps the plain machine list for a returning user without the intent', () => {
    visitor(WINDOWS_UA, 'Win32');
    entitlements(false);
    seed([machine]);
    renderGate();
    expect(screen.queryByText("You're in. Three steps to your first team.")).toBeNull();
    expect(screen.getByText('desk')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Download for Windows' })).toHaveAttribute('href', INSTALLER);
  });

  it('shows the headless install command to the team only', async () => {
    visitor(MAC_UA, 'MacIntel');
    entitlements(false);
    seed();
    const { unmount } = renderGate();
    fireEvent.click(screen.getByText('Other ways to connect'));
    fireEvent.click(screen.getByRole('button', { name: /claim a new machine/i }));
    expect(screen.getByLabelText('Code')).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/install\.sh/);
    unmount();

    vi.restoreAllMocks();
    visitor(MAC_UA, 'MacIntel');
    entitlements(true);
    seed();
    renderGate();
    fireEvent.click(screen.getByText('Other ways to connect'));
    fireEvent.click(screen.getByRole('button', { name: /claim a new machine/i }));
    await waitFor(() => expect(document.body.textContent).toMatch(/install\.sh/));
  });
});
