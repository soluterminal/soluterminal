import { cleanup, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __recomputeEmbedForTests, embedParentOrigin, getEmbedTheme, installEmbedBridge,
  isEmbedded, matchesHostKey, openEmbedUrl, parseHostMessage, sendEmbedNotification, sendEmbedReady,
} from './embed';
import { DesktopInstallerLink } from './components/DesktopInstallerLink';
import { useStore } from './store';
import { isTauri } from './voice';
import { getWindowInfo, windowScopedKey } from './lib/windowIdentity';
import { openInSystemBrowser } from './tauri-browser';
import { dispatchTrusted, setUserActivation, trusted } from './test/trustedEvent';

const base = {
  framed: true,
  search: '?embed=shinoque',
  saved: null,
  ancestorOrigin: 'http://127.0.0.1:4700/path',
  referrer: '',
};
const origin = 'http://127.0.0.1:4700';
const click = () => trusted(new MouseEvent('click'));
const host = (data: unknown, from = origin, source: MessageEventSource | null = window.parent) =>
  new MessageEvent('message', { data, origin: from, source });

let stop = () => {};
beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  __recomputeEmbedForTests(base);
});
afterEach(() => {
  stop();
  stop = () => {};
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  setUserActivation(null);
  __recomputeEmbedForTests();
});

describe('embed activation', () => {
  it.each([
    [{ framed: false }, false, null],
    [{ search: '' }, false, null],
    [{ search: '', saved: 'shinoque' }, true, origin],
    [{ ancestorOrigin: '', referrer: 'https://localhost:14700/a' }, true, 'https://localhost:14700'],
    [{ ancestorOrigin: 'http://[::1]:4700/' }, true, 'http://[::1]:4700'],
    [{ ancestorOrigin: 'http://window.shinoque.localhost:14700' }, true, 'http://window.shinoque.localhost:14700'],
    [{ ancestorOrigin: 'http://window.shinoque.localhost:14800/path' }, true, 'http://window.shinoque.localhost:14800'],
    // Browsers normalize explicit :80 to a portless origin; both qualify.
    [{ ancestorOrigin: 'http://window.shinoque.localhost:80/' }, true, 'http://window.shinoque.localhost'],
    [{ ancestorOrigin: 'http://window.shinoque.localhost' }, true, 'http://window.shinoque.localhost'],
    [{ ancestorOrigin: 'http://window.shinoque.localhost:1' }, true, 'http://window.shinoque.localhost:1'],
    [{ ancestorOrigin: 'http://window.shinoque.localhost:65535' }, true, 'http://window.shinoque.localhost:65535'],
    [{ ancestorOrigin: '', referrer: 'http://window.shinoque.localhost:14700/app' }, true, 'http://window.shinoque.localhost:14700'],
    ...[
      'http://window.shinoque.localhost.evil.com:14700',
      'http://evil-window.shinoque.localhost:14700',
      'http://evil.shinoque.localhost:14700',
      'http://evil.shinoque.localhost',
      'http://window.shinoque.localhost.evil.com',
      'http://window-shinoque.localhost:14700',
      'http://window-shinoque.localhost',
      'https://window.shinoque.localhost.attacker',
      'http://other.shinoque.localhost:14700',
      'http://other.localhost:14700',
      'http://window.shinoque.localhost.:14700',
      'http://window.shinoque.localhost:65536',
      'http://window.shinoque.localhost:bad',
      'https://window.shinoque.localhost:14700',
      'https://window.shinoque.localhost:443',
      'https://window.shinoque.localhost',
      'http://user@window.shinoque.localhost:14700',
      'http://window.shinoque.localhost:14700@evil.com',
    ].map((ancestorOrigin) => [{ ancestorOrigin }, false, null] as const),
    [{ ancestorOrigin: 'https://evil.example/', referrer: `http://127.0.0.1:4700/` }, false, null],
    [{ ancestorOrigin: '', referrer: '' }, false, null],
    [{ ancestorOrigin: 'file:///tmp/host.html' }, false, null],
  ] as const)('checks frame, request and allowed parent %#', (change, expected, parent) => {
    __recomputeEmbedForTests({ ...base, ...change });
    expect(isEmbedded()).toBe(expected);
    expect(embedParentOrigin()).toBe(parent);
  });

  it.each([
    ['http://window.shinoque.localhost:14700/code', 'http://window.shinoque.localhost:14700'],
    ['http://window.shinoque.localhost/code', 'http://window.shinoque.localhost'],
    ['http://evil.shinoque.localhost:14700/code', null],
    ['http://window.shinoque.localhost.evil.com:14700/code', null],
    ['http://window-shinoque.localhost:14700/code', null],
    ['https://window.shinoque.localhost.attacker/code', null],
    ['https://window.shinoque.localhost:14700/code', null],
  ])('keeps the exact HTTP host rule in the Firefox referrer fallback: %s', (referrer, expected) => {
    // Firefox has no ancestorOrigins; an absent/empty ancestor uses referrer
    // through the same URL parser and exact-host check, never a looser rule.
    __recomputeEmbedForTests({ ...base, ancestorOrigin: '', referrer });
    expect(embedParentOrigin()).toBe(expected);
    expect(isEmbedded()).toBe(expected !== null);
  });

  it('isolates workspace identity from the standalone main window', () => {
    expect(getWindowInfo()).toEqual({ label: 'shinoque', role: 'secondary' });
    expect(windowScopedKey('solu.activeWorkspace')).toBe('solu.activeWorkspace.shinoque');
    __recomputeEmbedForTests({ ...base, framed: false });
    expect(getWindowInfo()).toEqual({ label: 'main', role: 'primary' });
  });

  it('gives trusted host windows distinct storage and ownership identities', () => {
    __recomputeEmbedForTests({ ...base, search: '?embed=shinoque&embedWindow=main-8' });
    expect(getWindowInfo()).toEqual({ label: 'shinoque.main-8', role: 'secondary' });
    expect(windowScopedKey('solu.activeWorkspace')).toBe('solu.activeWorkspace.shinoque.main-8');
    __recomputeEmbedForTests({ ...base, search: '?embed=shinoque&embedWindow=main-9' });
    expect(windowScopedKey('solu.activeWorkspace')).toBe('solu.activeWorkspace.shinoque.main-9');
    for (const id of ['main', '../main', 'main.evil', 'a'.repeat(97)]) {
      __recomputeEmbedForTests({ ...base, search: `?embed=shinoque&embedWindow=${encodeURIComponent(id)}` });
      expect(getWindowInfo().label).toBe('shinoque');
    }
    __recomputeEmbedForTests({ ...base, search: '?embed=shinoque&embedWindow=main-8', framed: false });
    expect(getWindowInfo().label).toBe('main');
  });

  it('ignores injected Tauri globals in the embed', () => {
    vi.stubGlobal('__TAURI_INTERNALS__', { invoke: vi.fn() });
    expect(isTauri()).toBe(false);
  });

  it('keeps a peer identity on a query-free reload only under a trusted parent', () => {
    const reload = { ...base, search: '', saved: 'shinoque', savedWindow: 'main-8' };
    __recomputeEmbedForTests(reload);
    expect(getWindowInfo().label).toBe('shinoque.main-8');
    __recomputeEmbedForTests({ ...reload, framed: false });
    expect(getWindowInfo().label).toBe('main');
    __recomputeEmbedForTests({ ...reload, ancestorOrigin: 'https://evil.test' });
    expect(getWindowInfo().label).toBe('main');
    __recomputeEmbedForTests({ ...reload, search: '?embed=shinoque&embedWindow=main' });
    expect(getWindowInfo().label).toBe('shinoque');
  });
});

describe('host protocol', () => {
  it('binds Shinoque window messages to the exact parent origin and port', () => {
    const parent = 'http://window.shinoque.localhost:14700';
    __recomputeEmbedForTests({ ...base, ancestorOrigin: parent });
    const message = { source: 'shinoque', v: 1, kind: 'theme', theme: 'light' };
    expect(parseHostMessage(host(message, parent))).toEqual({ kind: 'theme', theme: 'light' });
    for (const other of [
      'http://window.shinoque.localhost:14800', 'http://window.shinoque.localhost',
      'https://window.shinoque.localhost:14700', 'http://127.0.0.1:14700',
      'http://evil-window.shinoque.localhost:14700',
      'http://window.shinoque.localhost.evil.com:14700',
    ]) expect(parseHostMessage(host(message, other))).toBeNull();
    expect(parseHostMessage(host(message, parent, null))).toBeNull();
    const post = vi.spyOn(window.parent, 'postMessage').mockImplementation(() => {});
    sendEmbedReady();
    expect(post).toHaveBeenCalledWith(expect.objectContaining({ kind: 'ready' }), parent);
  });

  it('accepts only the exact parent, source, version and valid fields', () => {
    const init = { source: 'shinoque', v: 1, kind: 'init', theme: 'dark', hostKeys: ['mod+j', 'mod+shift+k'] };
    expect(parseHostMessage(host(init))).toEqual({ kind: 'init', theme: 'dark', hostKeys: init.hostKeys });
    expect(parseHostMessage(host(init, 'http://localhost:4700'))).toBeNull();
    expect(parseHostMessage(host(init, origin, null))).toBeNull();
    for (const bad of [null, [], 'init', { ...init, source: 'other' }, { ...init, v: 2 },
      { ...init, kind: 'unknown' }, { ...init, theme: 'auto' }, { ...init, hostKeys: ['alt+j'] },
      { ...init, hostKeys: Array(17).fill('mod+j') }, { ...init, hostKeys: ['mod+long'] },
      { source: 'shinoque', v: 1, kind: 'visibility', visible: 'true' }]) {
      expect(parseHostMessage(host(bad))).toBeNull();
    }
    expect(parseHostMessage(host({ source: 'shinoque', v: 1, kind: 'theme', theme: 'light', extra: 'ignored' })))
      .toEqual({ kind: 'theme', theme: 'light' });
  });

  it('sends once, always using the exact parent origin, and drops unsupported URLs', () => {
    const post = vi.spyOn(window.parent, 'postMessage').mockImplementation(() => {});
    sendEmbedReady('1.2.3');
    sendEmbedReady('1.2.3');
    expect(post).toHaveBeenCalledExactlyOnceWith({ source: 'soluterminal', v: 1, kind: 'ready', protocolVersion: 3, windowIdentity: 'shinoque', version: '1.2.3' }, origin);
    expect(openEmbedUrl('https://example.com/docs', click())).toBe(true);
    expect(post).toHaveBeenLastCalledWith({ source: 'soluterminal', v: 1, kind: 'open-url', url: 'https://example.com/docs' }, origin);
    for (const url of ['javascript:alert(1)', 'file:///tmp/data', 'https://user:pass@example.com/']) {
      expect(openEmbedUrl(url)).toBe(false);
    }
    expect(openEmbedUrl('https://example.com/?access_token=opaque#id_token=opaque', click())).toBe(true);
    expect(post).toHaveBeenCalledTimes(3);
    expect(post.mock.calls.every(([, target]) => String(target) === origin && String(target) !== '*')).toBe(true);
  });

  it('routes OAuth target-blank links with their query intact only in embed mode', () => {
    const post = vi.spyOn(window.parent, 'postMessage').mockImplementation(() => {});
    stop = installEmbedBridge(() => {});
    const a = document.createElement('a');
    a.href = 'https://claude.com/cai/oauth/authorize?code=true&response_type=code&code_challenge=abc&state=opaque';
    a.target = '_blank';
    document.body.append(a);
    expect(dispatchTrusted(a, new MouseEvent('click', { bubbles: true, cancelable: true }))).toBe(false);
    expect(post).toHaveBeenCalledWith({ source: 'soluterminal', v: 1, kind: 'open-url', url: a.href }, origin);
    __recomputeEmbedForTests({ ...base, framed: false });
    expect(a.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))).toBe(true);
    expect(openEmbedUrl(a.href)).toBe(false);
    a.remove();
  });

  describe('open-url needs a person to have clicked', () => {
    const url = 'http://127.0.0.1:5173/admin';
    const opened = { source: 'soluterminal', v: 1, kind: 'open-url', url };
    const anchor = () => {
      const a = document.createElement('a');
      a.href = url;
      a.target = '_blank';
      document.body.append(a);
      return a;
    };
    const refused = (toast: unknown) => vi.waitFor(() =>
      expect(toast).toHaveBeenCalledWith(expect.stringContaining('click Open in browser'), { kind: 'warn' }));

    it('posts nothing for a programmatic call and tells the person to click instead', async () => {
      const post = vi.spyOn(window.parent, 'postMessage').mockImplementation(() => {});
      const toast = vi.spyOn(useStore.getState(), 'toast').mockImplementation(() => {});
      expect(openEmbedUrl(url)).toBe(false);
      setUserActivation(false);
      expect(openEmbedUrl(url)).toBe(false);
      expect(await openInSystemBrowser(url)).toBe(false);
      expect(post).not.toHaveBeenCalled();
      await refused(toast);
      expect(toast).toHaveBeenCalledTimes(3);
    });

    it('posts once for a real click, xterm link mouseup, or Enter or Space on the control', () => {
      const post = vi.spyOn(window.parent, 'postMessage').mockImplementation(() => {});
      setUserActivation(false);
      const gestures = [
        click(),
        trusted(new MouseEvent('mouseup', { button: 0 })),
        trusted(new KeyboardEvent('keydown', { key: 'Enter' })),
        trusted(new KeyboardEvent('keydown', { key: ' ' })),
      ];
      for (const gesture of gestures) {
        post.mockClear();
        expect(openEmbedUrl(url, gesture)).toBe(true);
        expect(post).toHaveBeenCalledExactlyOnceWith(opened, origin);
      }
      stop = installEmbedBridge(() => {});
      const a = anchor();
      post.mockClear();
      expect(dispatchTrusted(a, new MouseEvent('click', { bubbles: true, cancelable: true }))).toBe(false);
      expect(post).toHaveBeenCalledExactlyOnceWith(opened, origin);
      a.remove();
    });

    it('posts nothing for an untrusted dispatched link click, and still keeps the frame from navigating', async () => {
      const post = vi.spyOn(window.parent, 'postMessage').mockImplementation(() => {});
      const toast = vi.spyOn(useStore.getState(), 'toast').mockImplementation(() => {});
      stop = installEmbedBridge(() => {});
      const a = anchor();
      expect(a.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))).toBe(false);
      a.click();
      expect(post).not.toHaveBeenCalled();
      await refused(toast);
      a.remove();
    });

    it('posts nothing on any event-carrying path after a keypress elsewhere, then a programmatic trigger', async () => {
      const post = vi.spyOn(window.parent, 'postMessage').mockImplementation(() => {});
      const toast = vi.spyOn(useStore.getState(), 'toast').mockImplementation(() => {});
      stop = installEmbedBridge(() => {});
      // Typing in a terminal leaves the frame with transient user activation.
      setUserActivation(true);
      const a = anchor();
      a.click();
      a.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
      for (const event of [
        new MouseEvent('click'),
        new MouseEvent('mouseup', { button: 0 }),
        new KeyboardEvent('keydown', { key: 'Enter' }),
        trusted(new KeyboardEvent('keydown', { key: 'a' })),
        trusted(new MouseEvent('mouseup', { button: 2 })),
        trusted(new MouseEvent('mousedown')),
      ]) {
        expect(openEmbedUrl(url, event)).toBe(false);
        expect(await openInSystemBrowser(url, event)).toBe(false);
      }
      expect(post).not.toHaveBeenCalled();
      await refused(toast);
      a.remove();
    });

    it('lets a caller with no event use transient activation as the backstop', () => {
      const post = vi.spyOn(window.parent, 'postMessage').mockImplementation(() => {});
      setUserActivation(true);
      expect(openEmbedUrl(url)).toBe(true);
      expect(post).toHaveBeenCalledExactlyOnceWith(opened, origin);
    });
  });

  it('passes notification title and body through unchanged', () => {
    const post = vi.spyOn(window.parent, 'postMessage').mockImplementation(() => {});
    sendEmbedNotification('Approval needed · workspace', 'Claude wants to run a command', 'pane-1');
    expect(post).toHaveBeenCalledExactlyOnceWith({
      source: 'soluterminal', v: 1, kind: 'notify',
      title: 'Approval needed · workspace', body: 'Claude wants to run a command', paneId: 'pane-1',
    }, origin);
  });

  it('routes browser helpers through the host only in embed mode', async () => {
    const post = vi.spyOn(window.parent, 'postMessage').mockImplementation(() => {});
    const popup = vi.spyOn(window, 'open').mockImplementation(() => null);
    expect(await openInSystemBrowser('https://example.com/login', click())).toBe(true);
    expect(post).toHaveBeenCalledTimes(1);
    expect(popup).not.toHaveBeenCalled();
    __recomputeEmbedForTests({ ...base, framed: false });
    expect(await openInSystemBrowser('https://example.com/login')).toBe(false);
    expect(popup).toHaveBeenCalledTimes(1);
  });

  it('consumes registered host keys before other capture listeners', () => {
    const post = vi.spyOn(window.parent, 'postMessage').mockImplementation(() => {});
    stop = installEmbedBridge(() => {});
    window.dispatchEvent(host({ source: 'shinoque', v: 1, kind: 'init', theme: 'dark', hostKeys: ['mod+j'] }));
    const later = vi.fn();
    window.addEventListener('keydown', later, true);
    const mac = navigator.platform.startsWith('Mac');
    const event = new KeyboardEvent('keydown', { key: 'j', bubbles: true, cancelable: true, [mac ? 'metaKey' : 'ctrlKey']: true });
    window.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
    expect(later).not.toHaveBeenCalled();
    expect(post).toHaveBeenCalledWith({ source: 'soluterminal', v: 1, kind: 'host-key', key: 'mod+j' }, origin);
    expect(matchesHostKey(new KeyboardEvent('keydown', { key: 'j', altKey: true, [mac ? 'metaKey' : 'ctrlKey']: true }))).toBeNull();
    window.removeEventListener('keydown', later, true);
  });

  it('applies host themes in memory without writing the standalone preference', () => {
    localStorage.setItem('solu.appTheme', 'dark');
    const setItem = vi.spyOn(Storage.prototype, 'setItem');
    stop = installEmbedBridge((theme) => useStore.getState().setAppThemeLight(theme === 'light'));
    window.dispatchEvent(host({ source: 'shinoque', v: 1, kind: 'theme', theme: 'light' }));
    expect(getEmbedTheme()).toBe('light');
    expect(useStore.getState().appThemeLight).toBe(true);
    expect(localStorage.getItem('solu.appTheme')).toBe('dark');
    expect(setItem).not.toHaveBeenCalled();
    useStore.getState().setAppTheme('dark');
    expect(setItem).not.toHaveBeenCalled();
  });

  it('refits panes on a trusted visible message and never requests frame notification permission', async () => {
    const visible = vi.fn();
    const visibility = vi.fn();
    window.addEventListener('solu:embed-visible', visible);
    window.addEventListener('solu:embed-visibility', visibility);
    stop = installEmbedBridge(() => {});
    window.dispatchEvent(host({ source: 'shinoque', v: 1, kind: 'visibility', visible: false }));
    window.dispatchEvent(host({ source: 'shinoque', v: 1, kind: 'visibility', visible: true }));
    expect(visible).toHaveBeenCalledTimes(1);
    expect(visibility.mock.calls.map(([event]) => (event as CustomEvent).detail.visible)).toEqual([false, true]);
    const requestPermission = vi.fn();
    vi.stubGlobal('Notification', { permission: 'default', requestPermission });
    expect(await useStore.getState().setNotifyEnabled(true)).toBe(true);
    expect(localStorage.getItem('solu.notifyEnabled')).toBe('1');
    expect(await useStore.getState().setNotifyEnabled(false)).toBe(false);
    expect(localStorage.getItem('solu.notifyEnabled')).toBe('0');
    expect(requestPermission).not.toHaveBeenCalled();
    window.removeEventListener('solu:embed-visible', visible);
    window.removeEventListener('solu:embed-visibility', visibility);
  });
});

it('hides installer chrome only in embed mode', () => {
  // jsdom reports Linux, so outside embed mode the link renders as the
  // Windows-only note; either way it renders nothing while embedded.
  const { container, rerender } = render(<DesktopInstallerLink>Download desktop app</DesktopInstallerLink>);
  expect(container).toBeEmptyDOMElement();
  __recomputeEmbedForTests({ ...base, framed: false });
  rerender(<DesktopInstallerLink>Download desktop app</DesktopInstallerLink>);
  expect(container).not.toBeEmptyDOMElement();
});
