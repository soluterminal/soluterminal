import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { configureApi, setBackend } from '../api';
import { agentPreviewPort, browserAccessUrl, browserTileSandbox, desktopShellBlocksPreviews, framesInThisWindow, isPlainWebUrl, loadDaemonPreviewOrigin, loadRouterPreviewOrigin, loadShellPreviewGate, serviceTarget, setDaemonPreviewOrigin, setRouterPreviewOrigin, setShellPreviewGate, shellVersionFramesPreviews, systemBrowserUrl } from './portBrowser';
import { reconnectServiceBrowsers } from './ports';
import type { BrowserServiceTarget, Port } from '../types';
const target: BrowserServiceTarget = {
  url: 'https://[::1]:5173',
  previewPath: '/api/proxy/5173~YWJj/',
  backend: 'win',
  machinePrefix: '/m/host',
};
beforeEach(() => {
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () => new Response(JSON.stringify({ ticket: 'spp1.fixture.pass' }), { status: 200 }),
    ),
  );
});

describe('saved service restart recovery', () => {
  const original: Port = { port: 5173, addr: '127.0.0.1', proto: 'tcp', pid: 1, firstAt: '', process: 'node', cmdline: '', cwd: '', paneId: 'app-pane', backend: 'win', scheme: 'http', webStatus: 'ready', previewPath: '/api/proxy/5173~b2xk/' };
  const replacement: Port = { ...original, pid: 2, previewPath: '/api/proxy/5173~bmV3/' };
  const browsers = () => ({ saved: { id: 'saved', url: 'http://127.0.0.1:5173/app?view=1#top', createdAt: '', service: serviceTarget(original) } });

  it.each([
    ['another pane', { paneId: 'different-pane' }],
    ['another backend', { backend: 'wsl' }],
    ['another address', { addr: '0.0.0.0' }],
    ['another protocol', { proto: 'tcp6' }],
    ['another port', { port: 7000 }],
    ['an unowned listener', { paneId: undefined }],
    ['an unchecked listener', { webStatus: 'unverified' }],
    ['an unbound listener', { previewPath: undefined }],
  ] as const)('refuses %s on a reused port', (_label, patch) => {
    const saved = browsers();
    expect(reconnectServiceBrowsers(saved, [{ ...replacement, ...patch }], '')).toBe(saved);
  });

  it('does not rebind from another machine or an ambiguous response', () => {
    const saved = browsers();
    expect(reconnectServiceBrowsers(saved, [replacement], '/m/other')).toBe(saved);
    expect(reconnectServiceBrowsers(saved, [replacement, replacement], '')).toBe(saved);
    expect(reconnectServiceBrowsers(saved, [], '')).toBe(saved);
  });

  it('preserves page navigation while refreshing a scheme and listener', () => {
    const saved = browsers();
    const recovered = reconnectServiceBrowsers(saved, [{ ...replacement, scheme: 'https' }], '');
    expect(recovered.saved.url).toBe('https://127.0.0.1:5173/app?view=1#top');
    expect(recovered.saved.service?.previewPath).toBe(replacement.previewPath);
    expect(saved.saved.service?.previewPath).toBe(original.previewPath);
  });

  it('leaves external navigation and legacy targets without pane provenance alone', () => {
    const saved = browsers();
    saved.saved.url = 'https://example.com/app';
    expect(reconnectServiceBrowsers(saved, [replacement], '')).toBe(saved);
    saved.saved.url = original.scheme + '://127.0.0.1:5173';
    saved.saved.service.paneId = undefined;
    expect(reconnectServiceBrowsers(saved, [replacement], '')).toBe(saved);
  });
});
afterEach(() => {
  vi.unstubAllGlobals();
  configureApi({ prefix: '', token: null });
  setBackend(null);
});
describe('service browser routing', () => {
  it('keeps query and fragment separate from the scoped pass', async () => {
    configureApi({ prefix: '/m/host', token: 'a +/token' });
    const result = await browserAccessUrl('http://localhost:5173/app?view=1#section');
    const url = new URL(result!, 'https://cloud.test');
    expect(url.pathname).toBe('/m/host/api/proxy/5173/app');
    expect(url.searchParams.get('view')).toBe('1');
    expect(url.searchParams.get('access_token')).toBe('spp1.fixture.pass');
    expect(url.hash).toBe('#section');
  });
  it('uses the selected listener backend instead of the current backend', async () => {
    configureApi({ prefix: '/m/host', token: 'secret' });
    setBackend('wsl');
    expect(await browserAccessUrl('https://[::1]:5173/path?q=1#top', target)).toBe(
      '/m/host/b/win/api/proxy/5173~YWJj/path?q=1&access_token=spp1.fixture.pass#top',
    );
  });
  it('uses the backend route on desktop without adding a cloud credential', async () => {
    configureApi({ prefix: '', token: 'local-secret' });
    expect(await browserAccessUrl(target.url, { ...target, machinePrefix: '' })).toBe(
      '/b/win/api/proxy/5173~YWJj/',
    );
  });
  // On a dual-backend desktop the router serves previews on an origin of its
  // own: they keep scripts and data, and cannot act as the app.
  it('opens desktop router previews on the router preview origin', async () => {
    configureApi({ prefix: '', token: 'local-secret' });
    setRouterPreviewOrigin('http://127.0.0.1:41000');
    try {
      const url = await browserAccessUrl(target.url, { ...target, machinePrefix: '' });
      expect(url).toBe('http://127.0.0.1:41000/b/win/api/proxy/5173~YWJj/');
      expect(browserTileSandbox(url!)).toContain('allow-same-origin');
    } finally {
      setRouterPreviewOrigin(null);
    }
  });
  it('refuses a saved service from another machine', async () => {
    configureApi({ prefix: '/m/other', token: 'other-secret' });
    expect(await browserAccessUrl(target.url, target)).toBeNull();
  });
  it('never adds cloud credentials to external sites', async () => {
    configureApi({ prefix: '/m/host', token: 'secret' });
    expect(await browserAccessUrl('https://example.com/app', target)).toBe(
      'https://example.com/app',
    );
  });
  it('routes generic cloud localhost URLs through the current backend', async () => {
    configureApi({ prefix: '/m/host', token: null });
    setBackend('wsl');
    expect(await browserAccessUrl('http://localhost:3000/')).toBe('/m/host/b/wsl/api/proxy/3000/');
  });
});

describe('browserTileSandbox', () => {
  it('never lets a page on the app\'s own origin keep that origin', () => {
    for (const src of ['/api/proxy/8981/', '/m/mach_1/api/proxy/3000~abc/', `${location.origin}/api/preview-serve/p1/`, '/m/mach_1/b/wsl/api/proxy/5173/']) {
      expect(browserTileSandbox(src), src).not.toContain('allow-same-origin');
      expect(browserTileSandbox(src)).toContain('allow-scripts');
    }
  });

  it('keeps a localhost dev server on its own origin', () => {
    expect(browserTileSandbox('http://localhost:59173/')).toContain('allow-same-origin');
  });

  it('leaves desktop router previews as they were until they get their own origin', () => {
    expect(browserTileSandbox('/b/wsl/api/proxy/5173~abc/')).toContain('allow-same-origin');
  });
});

// After an in-app update the desktop keeps its old router until the app
// restarts. The new web app tells that apart from a router that is down.
describe('loadRouterPreviewOrigin', () => {
  const inRouterMode = async (answer: () => Promise<Response>) => {
    const before = window.location.href;
    window.history.replaceState(null, '', '/?solu_router=1');
    vi.stubGlobal('fetch', vi.fn(answer));
    try {
      return await loadRouterPreviewOrigin();
    } finally {
      window.history.replaceState(null, '', before);
      setRouterPreviewOrigin(null);
    }
  };

  it('is ready when the router publishes its preview origin', async () => {
    expect(await inRouterMode(async () => new Response(JSON.stringify({ origin: 'http://127.0.0.1:41000' }), { headers: { 'Content-Type': 'application/json' } }))).toBe('ready');
  });

  it('spots a router from before the update: its shell answers with the page', async () => {
    expect(await inRouterMode(async () => new Response('<!doctype html>', { headers: { 'Content-Type': 'text/html; charset=utf-8' } }))).toBe('old-router');
  });

  it('says nothing for a router that is down', async () => {
    expect(await inRouterMode(async () => { throw new TypeError('Failed to fetch'); })).toBe('unknown');
    expect(await inRouterMode(async () => new Response('down', { status: 502 }))).toBe('unknown');
  });

  it('does nothing outside router mode', async () => {
    expect(await loadRouterPreviewOrigin()).toBe('unknown');
  });
});

describe('agent preview requests', () => {
  const svc: Port = { port: 5173, addr: '127.0.0.1', proto: 'tcp', pid: 1, firstAt: '', process: 'node', cmdline: '', cwd: '', paneId: 'app', scheme: 'http', webStatus: 'ready', previewPath: '/api/proxy/5173~b2xk/' };

  it('uses the port the daemon matched, on the event’s backend', () => {
    expect(agentPreviewPort('http://localhost:5173/', svc, [], 'win')).toEqual({ ...svc, backend: 'win' });
  });

  it('matches an older daemon’s bare URL only to a pane’s web service on that backend', () => {
    const ports = [svc, { ...svc, port: 6000, paneId: undefined }, { ...svc, port: 7000, webStatus: 'unverified' as const }];
    expect(agentPreviewPort('http://127.0.0.1:5173/docs', undefined, ports, undefined)).toBe(svc);
    expect(agentPreviewPort('http://127.0.0.1:5173/', undefined, ports, 'wsl')).toBeNull();
    expect(agentPreviewPort('http://127.0.0.1:6000/', undefined, ports, undefined)).toBeNull();
    expect(agentPreviewPort('http://127.0.0.1:7000/', undefined, ports, undefined)).toBeNull();
    expect(agentPreviewPort('https://example.com/', undefined, ports, undefined)).toBeNull();
    expect(agentPreviewPort('not a url', undefined, ports, undefined)).toBeNull();
  });
});

describe('system browser addresses', () => {
  afterEach(() => configureApi({ prefix: '', token: null }));

  it.each(['file:///etc/passwd', 'javascript:alert(1)', 'http://user:pw@localhost:5173/', 'not a url'])
    ('refuses %s', async (raw) => {
      expect(isPlainWebUrl(raw)).toBe(false);
      expect(await systemBrowserUrl(raw)).toBeNull();
    });

  it('opens the address itself on this computer', async () => {
    configureApi({ prefix: '', token: null });
    expect(await systemBrowserUrl('http://127.0.0.1:5173/app')).toBe('http://127.0.0.1:5173/app');
  });

  it('on a dual desktop, refuses a WSL service whose port a Windows app also uses, and says why', async () => {
    configureApi({ prefix: '', token: null });
    const svc = (backend: 'wsl' | 'win', over: Partial<Port> = {}): Port => ({ port: 5173, addr: '127.0.0.1', proto: 'tcp', pid: 1, firstAt: '', process: 'node', cmdline: '', cwd: '', paneId: 'app', scheme: 'http', webStatus: 'ready', previewPath: '/api/proxy/5173~b2xk/', backend, ...over });
    const wsl = svc('wsl');
    const win = svc('win', { paneId: undefined });
    await expect(systemBrowserUrl('http://127.0.0.1:5173/', serviceTarget(wsl), [wsl, win])).rejects.toThrow(
      'A Windows app also uses port 5173, so your browser would open that one instead.',
    );
    // The Windows one is what the browser reaches: it opens.
    expect(await systemBrowserUrl('http://127.0.0.1:5173/', serviceTarget(win), [wsl, win])).toBe('http://127.0.0.1:5173/');
    // Any Windows listener on the port takes it, confirmed web service or not.
    for (const webStatus of ['unverified', 'internal'] as const) {
      await expect(
        systemBrowserUrl('http://127.0.0.1:5173/', serviceTarget(wsl), [wsl, svc('win', { webStatus, scheme: undefined, paneId: undefined })]),
      ).rejects.toThrow('A Windows app also uses port 5173');
    }
    // No Windows listener on that port: the address opens.
    expect(await systemBrowserUrl('http://127.0.0.1:5173/', serviceTarget(wsl), [wsl])).toBe('http://127.0.0.1:5173/');
    expect(await systemBrowserUrl('http://127.0.0.1:5173/', serviceTarget(wsl), [wsl, svc('win', { port: 5174 })])).toBe('http://127.0.0.1:5173/');
  });

  it('opens the proxy with a scoped pass through the relay', async () => {
    configureApi({ prefix: '/m/host', token: 'secret' });
    const service = serviceTarget({ port: 5173, addr: '127.0.0.1', proto: 'tcp', pid: 1, firstAt: '', process: 'node', cmdline: '', cwd: '', paneId: 'app', scheme: 'http', webStatus: 'ready', previewPath: '/api/proxy/5173~b2xk/' });
    const url = await systemBrowserUrl('http://127.0.0.1:5173/app', service);
    expect(url).toBe(new URL('/m/host/api/proxy/5173~b2xk/app?access_token=spp1.fixture.pass', location.href).href);
    expect(url).not.toContain('secret');
  });
});

describe('desktop app preview origins', () => {
  const w = window as unknown as { __SOLU_TAURI__?: boolean; __TAURI_INTERNALS__?: { invoke?: unknown } };
  const local: Port = { port: 5173, addr: '127.0.0.1', proto: 'tcp', pid: 1, firstAt: '', process: 'node', cmdline: '', cwd: '', paneId: 'app', scheme: 'http', webStatus: 'ready', previewPath: '/api/proxy/5173~b2xk/' };
  beforeEach(() => {
    w.__SOLU_TAURI__ = true;
    w.__TAURI_INTERNALS__ = { invoke: vi.fn() };
    configureApi({ prefix: '', token: null });
    setShellPreviewGate(true); // a 0.0.1241+ shell
  });
  afterEach(() => {
    delete w.__SOLU_TAURI__;
    delete w.__TAURI_INTERNALS__;
    setDaemonPreviewOrigin(null);
    setRouterPreviewOrigin(null);
    setShellPreviewGate(null);
  });

  it('frames on neither preview origin in a shell older than 0.0.1241 or of unknown version', () => {
    setRouterPreviewOrigin('http://127.0.0.1:41000');
    setDaemonPreviewOrigin({ origin: 'http://127.0.0.1:42000', prefix: '/b/host' });
    for (const gate of [false, null]) {
      setShellPreviewGate(gate);
      expect(desktopShellBlocksPreviews()).toBe(true);
      expect(framesInThisWindow('http://127.0.0.1:41000/b/wsl/api/proxy/5173~b2xk/')).toBe(false);
      expect(framesInThisWindow('http://127.0.0.1:42000/b/host/api/proxy/5173~b2xk/')).toBe(false);
    }
    setShellPreviewGate(true);
    expect(desktopShellBlocksPreviews()).toBe(false);
    expect(framesInThisWindow('http://127.0.0.1:42000/b/host/api/proxy/5173~b2xk/')).toBe(true);
  });

  it('reads the shell version: 0.0.1240 blocks previews', async () => {
    w.__TAURI_INTERNALS__ = { invoke: vi.fn(async (cmd: string) => (cmd === 'browser_pane_ping' ? '0.0.1240' : null)) };
    setShellPreviewGate(true);
    await loadShellPreviewGate();
    expect(desktopShellBlocksPreviews()).toBe(true);
  });

  it('frames only on a preview origin of its own, never the app or a dev server', () => {
    setRouterPreviewOrigin('http://127.0.0.1:41000');
    setDaemonPreviewOrigin({ origin: 'http://127.0.0.1:42000', prefix: '/b/host' });
    expect(framesInThisWindow('http://127.0.0.1:41000/b/wsl/api/proxy/5173~b2xk/')).toBe(true);
    expect(framesInThisWindow('http://127.0.0.1:42000/b/host/api/proxy/5173~b2xk/')).toBe(true);
    for (const src of ['http://127.0.0.1:5173/', 'http://localhost:5173/', '/api/proxy/5173/', `${location.origin}/b/wsl/api/proxy/5173/`, 'https://example.com/', 'http://127.0.0.1:42001/']) {
      expect(framesInThisWindow(src), src).toBe(false);
    }
  });

  it('frames as before in a browser tab', () => {
    delete w.__SOLU_TAURI__;
    expect(framesInThisWindow('http://127.0.0.1:5173/')).toBe(true);
  });

  it('routes a pane service to the daemon preview origin with its own origin kept', async () => {
    setDaemonPreviewOrigin({ origin: 'http://127.0.0.1:42000', prefix: '/b/host' });
    const url = await browserAccessUrl('http://127.0.0.1:5173/app?x=1&access_token=old#top', serviceTarget(local));
    expect(url).toBe('http://127.0.0.1:42000/b/host/api/proxy/5173~b2xk/app?x=1#top');
    expect(browserTileSandbox(url!)).toContain('allow-same-origin');
    expect(framesInThisWindow(url!)).toBe(true);
  });

  it('leaves a service the raw address without a preview origin, which the window then refuses to frame', async () => {
    const url = await browserAccessUrl('http://127.0.0.1:5173/', serviceTarget(local));
    expect(url).toBe('http://127.0.0.1:5173/');
    expect(framesInThisWindow(url!)).toBe(false);
  });

  it('accepts only a loopback preview origin and a known /b/ route from the daemon', async () => {
    for (const [answer, ok] of [
      [{ origin: 'http://127.0.0.1:42000', prefix: '/b/host' }, true],
      [{ origin: 'http://127.0.0.1:42000', prefix: '/b/wsl' }, true],
      [{ origin: 'https://evil.example', prefix: '/b/host' }, false],
      [{ origin: location.origin, prefix: '/b/host' }, false],
      [{ origin: 'http://127.0.0.1:42000', prefix: '/api' }, false],
      [{ origin: 'http://127.0.0.1:42000/x', prefix: '/b/host' }, false],
    ] as const) {
      setDaemonPreviewOrigin(null);
      vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(answer), { status: 200 })));
      expect(await loadDaemonPreviewOrigin(), JSON.stringify(answer)).toBe(ok);
      expect(framesInThisWindow('http://127.0.0.1:42000/b/host/api/proxy/1/')).toBe(ok && answer.origin === 'http://127.0.0.1:42000');
    }
  });

  it('does not ask a relayed machine or a router for the daemon origin', async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    configureApi({ prefix: '/m/host', token: 'secret' });
    expect(await loadDaemonPreviewOrigin()).toBe(false);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe('shell version gate for previews', () => {
  it.each([
    ['0.0.1241', true],
    ['0.0.1300', true],
    ['0.1.0', true],
    ['1.0.0', true],
    ['0.0.1241-rc.1', true],
    ['0.0.1240', false],
    ['0.0.284', false],
    ['', false],
    ['dev', false],
    [null, false],
    [undefined, false],
  ] as const)('%s → %s', (version, want) => {
    expect(shellVersionFramesPreviews(version)).toBe(want);
  });
});
