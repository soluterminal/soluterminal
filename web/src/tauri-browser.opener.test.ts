import { afterEach, describe, expect, it, vi } from 'vitest';
import * as embed from './embed';
import { openInSystemBrowser, openTerminalLink, systemBrowserSchemeAllowed } from './tauri-browser';

// ST-200 / ST-197: a link clicked in a terminal pane must reach the system
// browser through the opener plugin in the desktop app, where a bare
// window.open() is swallowed, and still open a tab in a plain browser.
afterEach(() => {
  delete (window as unknown as { __SOLU_TAURI__?: unknown }).__SOLU_TAURI__;
  delete (window as unknown as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
  vi.restoreAllMocks();
});

describe('openTerminalLink', () => {
  it('hands the link to the opener plugin in the desktop app', async () => {
    const invoke = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(window, '__SOLU_TAURI__', { value: true, configurable: true });
    Object.defineProperty(window, '__TAURI_INTERNALS__', { value: { invoke }, configurable: true });
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    openTerminalLink(new MouseEvent('click'), 'https://example.com/docs');
    await Promise.resolve();
    expect(invoke).toHaveBeenCalledWith('plugin:opener|open_url', { url: 'https://example.com/docs' });
    expect(open).not.toHaveBeenCalled();
  });

  it('opens a new tab in a plain browser, within the click', () => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    openTerminalLink(new MouseEvent('click'), 'https://example.com/docs');
    expect(open).toHaveBeenCalledWith('https://example.com/docs', '_blank', 'noopener,noreferrer');
  });
});

// @secure follow-up: the shared opener hands Windows only http, https and
// mailto links, from any caller, in the app and in a browser.
describe('openInSystemBrowser scheme allowlist', () => {
  const desktop = () => {
    const invoke = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(window, '__SOLU_TAURI__', { value: true, configurable: true });
    Object.defineProperty(window, '__TAURI_INTERNALS__', { value: { invoke }, configurable: true });
    return invoke;
  };

  it.each([
    'http://localhost:3000/',
    'https://example.com/docs?q=1#top',
    'mailto:support@example.com?subject=Hi',
  ])('passes %s to the opener plugin', async (url) => {
    const invoke = desktop();
    await expect(openInSystemBrowser(url)).resolves.toBe(true);
    expect(invoke).toHaveBeenCalledWith('plugin:opener|open_url', { url });
  });

  it.each([
    ['file:', 'file:///C:/Windows/System32/calc.exe'],
    ['ms-settings:', 'ms-settings:privacy'],
    ['ms-msdt:', 'ms-msdt:/id PCWDiagnostic'],
    ['search-ms:', 'search-ms:query=x'],
    ['custom protocol', 'vscode://file/C:/x'],
    ['javascript:', 'javascript:alert(1)'],
    ['data:', 'data:text/html,<b>x</b>'],
    ['smb UNC', '\\\\server\\share\\x.exe'],
    ['relative path', '/download/'],
    ['empty', ''],
  ])('refuses %s in the desktop app', async (_name, url) => {
    const invoke = desktop();
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    await expect(openInSystemBrowser(url)).resolves.toBe(false);
    expect(invoke).not.toHaveBeenCalled();
    expect(open).not.toHaveBeenCalled();
  });

  it.each(['file:///etc/passwd', 'ms-settings:privacy', 'vscode://file/x'])('refuses %s in a browser too', async (url) => {
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    await expect(openInSystemBrowser(url)).resolves.toBe(false);
    expect(open).not.toHaveBeenCalled();
  });

  it('hands on the URL as checked, not the raw string', async () => {
    const invoke = desktop();
    await expect(openInSystemBrowser(' https://exam\tple.com/a\nb ')).resolves.toBe(true);
    expect(invoke).toHaveBeenCalledWith('plugin:opener|open_url', { url: 'https://example.com/ab' });
  });

  it('refuses a scheme hidden by characters the parser drops', async () => {
    const invoke = desktop();
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    await expect(openInSystemBrowser('fi\tle:///C:/x.exe')).resolves.toBe(false);
    await expect(openInSystemBrowser('\u0001ms-settings:privacy')).resolves.toBe(false);
    expect(invoke).not.toHaveBeenCalled();
  });

  it.each([
    'https://trusted.example@evil.example/',
    'https://user:pass@example.com/',
    'http://:secret@example.com/',
  ])('refuses %s: a user name or password disguises the host', async (url) => {
    const invoke = desktop();
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    await expect(openInSystemBrowser(url)).resolves.toBe(false);
    expect(invoke).not.toHaveBeenCalled();
    expect(open).not.toHaveBeenCalled();
  });

  it('keeps mailto addresses, whose @ is not a user name', async () => {
    const invoke = desktop();
    await expect(openInSystemBrowser('mailto:support@example.com')).resolves.toBe(true);
    expect(invoke).toHaveBeenCalledWith('plugin:opener|open_url', { url: 'mailto:support@example.com' });
  });

  it('refuses before the embed bridge too', async () => {
    const embedded = vi.spyOn(embed, 'isEmbedded').mockReturnValue(true);
    const bridge = vi.spyOn(embed, 'openEmbedUrl').mockReturnValue(true);
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    await expect(openInSystemBrowser('file:///etc/passwd', new MouseEvent('click'))).resolves.toBe(false);
    expect(bridge).not.toHaveBeenCalled();
    await expect(openInSystemBrowser('https://example.com/', new MouseEvent('click'))).resolves.toBe(true);
    expect(bridge).toHaveBeenCalledWith('https://example.com/', expect.any(MouseEvent));
    embedded.mockRestore();
  });

  it('accepts the schemes case-insensitively, as URL parsing does', () => {
    expect(systemBrowserSchemeAllowed('HTTPS://EXAMPLE.COM/')).toBe(true);
    expect(systemBrowserSchemeAllowed('MAILTO:a@example.com')).toBe(true);
    expect(systemBrowserSchemeAllowed('FILE:///x')).toBe(false);
  });
});
