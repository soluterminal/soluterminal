import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, configureApi } from '../api';
import { serviceTarget, setDaemonPreviewOrigin, setRouterPreviewOrigin, setShellPreviewGate } from '../lib/portBrowser';
import { isDesktopApp } from '../tauri-browser';
import { useStore } from '../store';
import { __recomputeEmbedForTests } from '../embed';
import { dispatchTrusted, setUserActivation } from '../test/trustedEvent';
import type { BrowserInfo, Port } from '../types';
import { BrowserPane, PREVIEW_LOAD_TIMEOUT_MS } from './BrowserPane';

const tauri = vi.hoisted(() => ({ openInSystemBrowser: vi.fn() }));

vi.mock('../tauri-browser', async (original) => ({
  ...(await original<typeof import('../tauri-browser')>()),
  isDesktopApp: vi.fn(() => false),
  openInSystemBrowser: tauri.openInSystemBrowser,
}));

const port = (over: Partial<Port> = {}): Port => ({
  port: 5173, addr: '127.0.0.1', proto: 'tcp', pid: 1, firstAt: '', process: 'node', cmdline: '', cwd: '',
  paneId: 'app', scheme: 'http', webStatus: 'ready', previewPath: '/api/proxy/5173~b2xk/', ...over,
});

// A tile on a pane's service. Build it after configureApi: the target records
// the machine it was opened on.
const serviceTile = (url = 'http://127.0.0.1:5173/app?x=1', over: Partial<Port> = {}): BrowserInfo => ({
  id: 'browser_test',
  url,
  title: 'Dev server',
  createdAt: '2026-05-18T00:00:00.000Z',
  service: serviceTarget(port(over)),
});

const savedPublicTile = (url: string): BrowserInfo => ({
  id: 'browser_test',
  url,
  title: 'Dev server',
  createdAt: '2026-05-18T00:00:00.000Z',
});

type Answer = Response | Error;
let previewAnswer: () => Answer;
let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  previewAnswer = () => new Response('<h1>ok</h1>', { status: 200 });
  fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url === '/api/relay/ticket') return new Response(JSON.stringify({ ticket: 'spp1.123.mac', expiresInSec: 3600 }), { status: 200 });
    const answer = previewAnswer();
    if (answer instanceof Error) throw answer;
    return answer;
  });
  vi.stubGlobal('fetch', fetchMock);
  tauri.openInSystemBrowser.mockReset().mockResolvedValue(true);
});

afterEach(() => {
  configureApi({ prefix: '', token: null });
  __recomputeEmbedForTests();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const previewCalls = () => fetchMock.mock.calls.filter(([url]) => String(url) !== '/api/relay/ticket');

describe('BrowserPane routing', () => {
  const JWT = 'jwt token/with+symbols';

  it('loads a service through the cloud proxy with a scoped pass, never the sign-in token', async () => {
    configureApi({ prefix: '/m/machine-1', token: JWT });

    render(<BrowserPane info={serviceTile('http://127.0.0.1:5173/app?x=1')} />);

    const iframe = await screen.findByTitle('Dev server');
    expect(iframe).toHaveAttribute('src', '/m/machine-1/api/proxy/5173~b2xk/app?x=1&access_token=spp1.123.mac');
    expect(iframe.getAttribute('src')).not.toContain(encodeURIComponent(JWT));
    expect(iframe).toHaveAttribute('sandbox', 'allow-forms allow-modals allow-popups allow-scripts');
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/relay/ticket');
    expect(new Headers(init.headers).get('Authorization')).toBe(`Bearer ${JWT}`);
    expect(JSON.parse(String(init.body))).toEqual({ path: '/m/machine-1/api/proxy/5173~b2xk/', kind: 'proxy' });
    expect(screen.getByText('127.0.0.1:5173/app?x=1')).toBeInTheDocument();
    // No address bar: the tile shows only the service it was opened on.
    expect(screen.queryByRole('textbox')).toBeNull();
  });

  it('reloads a restarted service with its new listener selector and scoped pass', async () => {
    configureApi({ prefix: '/m/restart-machine', token: JWT });
    const info = serviceTile('http://127.0.0.1:5173/app?view=1#top');
    useStore.setState({ browsers: { [info.id]: info } });
    function LiveBrowser() {
      const saved = useStore((s) => s.browsers[info.id]);
      return <BrowserPane info={saved} />;
    }
    vi.spyOn(api, 'listPorts').mockResolvedValue([port({ pid: 2, previewPath: '/api/proxy/5173~bmV3/' })]);
    render(<LiveBrowser />);
    await vi.waitFor(() => expect(screen.getByTitle('Dev server').getAttribute('src')).toContain('/5173~b2xk/'));
    fireEvent.click(screen.getByTitle('Reload'));
    await vi.waitFor(() => expect(screen.getByTitle('Dev server')).toHaveAttribute('src', '/m/restart-machine/api/proxy/5173~bmV3/app?view=1&access_token=spp1.123.mac#top'));
    const ticketCalls = fetchMock.mock.calls.filter(([url]) => url === '/api/relay/ticket');
    const scopes = ticketCalls.map(([, init]) => JSON.parse(String((init as RequestInit).body)).path);
    expect(scopes).toContain('/m/restart-machine/api/proxy/5173~bmV3/');
  });
});

describe('BrowserPane shows only pane services', () => {
  it('never frames a saved public page; it offers the system browser instead', async () => {
    render(<BrowserPane info={savedPublicTile('https://example.com/dashboard')} />);

    expect(screen.queryByTitle('Dev server')).toBeNull();
    expect(screen.getByRole('status')).toHaveTextContent('This page opens in your browser');
    expect(fetchMock).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Open in browser' }));
    await vi.waitFor(() => expect(tauri.openInSystemBrowser).toHaveBeenCalledWith('https://example.com/dashboard'));
  });
});

describe('BrowserPane load failures', () => {
  it('replaces the frame with an Open in browser message when the proxy cannot reach the server', async () => {
    configureApi({ prefix: '/m/machine-1', token: 'jwt' });
    previewAnswer = () => new Response('preview of 127.0.0.1:5173 failed: connection refused — is the dev server actually listening?', { status: 502 });

    render(<BrowserPane info={serviceTile()} />);

    expect(await screen.findByText('Couldn’t load this preview')).toBeInTheDocument();
    expect(screen.getByText(/connection refused/)).toBeInTheDocument();
    expect(screen.queryByTitle('Dev server')).toBeNull();
    expect(screen.getAllByRole('button', { name: /Open in browser/ }).length).toBeGreaterThan(0);
  });

  it('still shows an app’s own error page', async () => {
    configureApi({ prefix: '/m/machine-1', token: 'jwt' });
    previewAnswer = () => new Response('<h1>TypeError in page.tsx</h1>', { status: 500 });

    render(<BrowserPane info={serviceTile()} />);

    const iframe = await screen.findByTitle('Dev server');
    await vi.waitFor(() => expect(previewCalls()).toHaveLength(1));
    expect(screen.queryByText('Couldn’t load this preview')).toBeNull();
    expect(iframe).toBeVisible();
  });

  it('reports a local dev server that is not answering', async () => {
    previewAnswer = () => new TypeError('Failed to fetch');

    render(<BrowserPane info={serviceTile()} />);

    expect(await screen.findByText('Couldn’t load this preview')).toBeInTheDocument();
    expect(screen.getByText('Port 5173 isn’t answering. Is the dev server still running?')).toBeInTheDocument();
    const [, init] = previewCalls()[0] as [string, RequestInit];
    expect(init.mode).toBe('no-cors');
  });

  it('offers the browser when a page never finishes loading (it may refuse framing)', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    render(<BrowserPane info={serviceTile()} />);
    const iframe = screen.getByTitle('Dev server');

    await act(async () => { vi.advanceTimersByTime(PREVIEW_LOAD_TIMEOUT_MS + 10); });

    expect(screen.getByText('This page is taking a while')).toBeInTheDocument();
    expect(iframe).not.toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: 'Keep waiting' }));
    expect(screen.queryByText('This page is taking a while')).toBeNull();
    expect(iframe).toBeVisible();
  });

  it('drops the slow-load message once the page loads', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    render(<BrowserPane info={serviceTile()} />);
    await act(async () => { vi.advanceTimersByTime(PREVIEW_LOAD_TIMEOUT_MS + 10); });
    expect(screen.getByText('This page is taking a while')).toBeInTheDocument();

    fireEvent.load(screen.getByTitle('Dev server'));

    expect(screen.queryByText('This page is taking a while')).toBeNull();
  });

  it('shows no message for a page that loads in time', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    render(<BrowserPane info={serviceTile()} />);
    fireEvent.load(screen.getByTitle('Dev server'));
    await act(async () => { vi.advanceTimersByTime(PREVIEW_LOAD_TIMEOUT_MS + 10); });
    expect(screen.queryByRole('status')).toBeNull();
  });
});

describe('BrowserPane toolbar', () => {
  it('enables Back only after the page navigated, and steps back through the session history', () => {
    const back = vi.spyOn(history, 'back').mockImplementation(() => {});
    render(<BrowserPane info={serviceTile()} />);
    const iframe = screen.getByTitle('Dev server');
    const backButton = screen.getByRole('button', { name: 'Back' });

    fireEvent.load(iframe);
    expect(backButton).toBeDisabled();
    history.pushState({}, '', location.href); // a link click inside the page adds an entry
    fireEvent.load(iframe);
    expect(backButton).toBeEnabled();

    fireEvent.click(backButton);
    expect(back).toHaveBeenCalledTimes(1);
    fireEvent.load(iframe); // the frame went back
    expect(backButton).toBeDisabled();
  });

  it('never enables Back for a load that added no history (a reload, HMR or location.replace)', () => {
    const back = vi.spyOn(history, 'back').mockImplementation(() => {});
    render(<BrowserPane info={serviceTile()} />);
    const iframe = screen.getByTitle('Dev server');
    const backButton = screen.getByRole('button', { name: 'Back' });

    fireEvent.load(iframe);
    fireEvent.load(iframe); // the page reloaded itself: no new entry
    fireEvent.load(iframe);
    expect(backButton).toBeDisabled();
    fireEvent.click(backButton);
    expect(back).not.toHaveBeenCalled();
  });

  it('opens a local service in the system browser at its own address', async () => {
    render(<BrowserPane info={serviceTile('http://127.0.0.1:5173/app')} />);
    fireEvent.click(screen.getByRole('button', { name: /Open in browser/ }));
    await vi.waitFor(() => expect(tauri.openInSystemBrowser).toHaveBeenCalledWith('http://127.0.0.1:5173/app'));
  });

  it('opens a relayed service in the system browser through the proxy with a scoped pass', async () => {
    configureApi({ prefix: '/m/machine-1', token: 'jwt-token' });
    render(<BrowserPane info={serviceTile('http://127.0.0.1:5173/')} />);
    fireEvent.click(screen.getByRole('button', { name: /Open in browser/ }));
    await vi.waitFor(() => expect(tauri.openInSystemBrowser).toHaveBeenCalledTimes(1));
    const [opened] = tauri.openInSystemBrowser.mock.calls[0] as [string];
    expect(opened).toBe(new URL('/m/machine-1/api/proxy/5173~b2xk/?access_token=spp1.123.mac', location.href).href);
    expect(opened).not.toContain('jwt-token');
  });

  it('keeps the pointer off the page during a pane drag so drop zones work', () => {
    render(<BrowserPane info={serviceTile()} />);
    act(() => useStore.getState().setDraggingPane(true));
    expect(screen.getByTitle('Dev server')).toHaveStyle({ pointerEvents: 'none' });
    act(() => useStore.getState().setDraggingPane(false));
  });
});

describe('BrowserPane inside the Shinoque embed', () => {
  const embedded = {
    framed: true,
    search: '?embed=shinoque',
    saved: null,
    ancestorOrigin: 'http://127.0.0.1:4700',
    referrer: '',
  };

  it('hands the page to the host with open-url when Open in browser is clicked', () => {
    __recomputeEmbedForTests(embedded);
    const post = vi.spyOn(window.parent, 'postMessage').mockImplementation(() => {});
    render(<BrowserPane info={serviceTile('http://127.0.0.1:5173/current?view=1#section')} />);

    expect(post).not.toHaveBeenCalled();
    dispatchTrusted(screen.getByRole('button', { name: /Open in browser/ }), new MouseEvent('click', { bubbles: true, cancelable: true }));

    expect(post).toHaveBeenCalledExactlyOnceWith(
      { source: 'soluterminal', v: 1, kind: 'open-url', url: 'http://127.0.0.1:5173/current?view=1#section' },
      'http://127.0.0.1:4700',
    );
    expect(tauri.openInSystemBrowser).not.toHaveBeenCalled();
  });

  it('posts nothing for a programmatic click on Open in browser, even after a keypress elsewhere', async () => {
    __recomputeEmbedForTests(embedded);
    const post = vi.spyOn(window.parent, 'postMessage').mockImplementation(() => {});
    const toast = vi.spyOn(useStore.getState(), 'toast').mockImplementation(() => {});
    setUserActivation(true);
    try {
      render(<BrowserPane info={serviceTile('http://127.0.0.1:5173/current')} />);
      const button = screen.getByRole('button', { name: /Open in browser/ });
      fireEvent.click(button);
      button.click();
      expect(post).not.toHaveBeenCalled();
      await vi.waitFor(() => expect(toast).toHaveBeenCalledWith(expect.stringContaining('click Open in browser'), { kind: 'warn' }));
    } finally {
      setUserActivation(null);
    }
  });
});

describe('BrowserPane in the desktop app', () => {
  beforeEach(() => {
    vi.mocked(isDesktopApp).mockReturnValue(true);
    setShellPreviewGate(true); // a 0.0.1241+ shell
  });
  afterEach(() => {
    vi.mocked(isDesktopApp).mockReturnValue(false);
    act(() => {
      setDaemonPreviewOrigin(null);
      setRouterPreviewOrigin(null);
      setShellPreviewGate(null);
    });
  });

  it.each([
    ['older than 0.0.1241', false],
    ['of unknown version', null],
  ] as const)('never frames a preview in a shell %s, on either preview origin, and asks for the update', (_label, gate) => {
    setShellPreviewGate(gate);
    setDaemonPreviewOrigin({ origin: 'http://127.0.0.1:42000', prefix: '/b/host' });
    setRouterPreviewOrigin('http://127.0.0.1:41000');
    const { unmount } = render(<BrowserPane info={serviceTile('http://127.0.0.1:5173/app')} />);
    expect(screen.queryByTitle('Dev server')).toBeNull();
    expect(screen.getByText(/Update the SoluTerminal desktop app to preview here/)).toBeInTheDocument();
    unmount();
    render(<BrowserPane info={serviceTile('http://127.0.0.1:5173/', { backend: 'wsl' })} />);
    expect(screen.queryByTitle('Dev server')).toBeNull();
    expect(screen.getByText(/Update the SoluTerminal desktop app to preview here/)).toBeInTheDocument();
    expect(previewCalls()).toHaveLength(0);
  });

  it('asks the daemon for its preview origin again on Reload (a restart moves it)', async () => {
    setDaemonPreviewOrigin({ origin: 'http://127.0.0.1:42000', prefix: '/b/host' });
    previewAnswer = () => new Response(JSON.stringify({ origin: 'http://127.0.0.1:43000', prefix: '/b/host' }), { status: 200 });
    render(<BrowserPane info={serviceTile('http://127.0.0.1:5173/app')} />);
    expect(screen.getByTitle('Dev server')).toHaveAttribute('src', 'http://127.0.0.1:42000/b/host/api/proxy/5173~b2xk/app');
    fireEvent.click(screen.getByTitle('Reload'));
    await vi.waitFor(() => expect(screen.getByTitle('Dev server')).toHaveAttribute('src', 'http://127.0.0.1:43000/b/host/api/proxy/5173~b2xk/app'));
    expect(previewCalls().some(([url]) => String(url).endsWith('/api/preview-origin'))).toBe(true);
  });

  it('frames a pane service on the daemon preview origin', async () => {
    setDaemonPreviewOrigin({ origin: 'http://127.0.0.1:42000', prefix: '/b/host' });
    render(<BrowserPane info={serviceTile('http://127.0.0.1:5173/app')} />);
    const iframe = screen.getByTitle('Dev server');
    expect(iframe).toHaveAttribute('src', 'http://127.0.0.1:42000/b/host/api/proxy/5173~b2xk/app');
    expect(iframe).toHaveAttribute('sandbox', 'allow-forms allow-modals allow-popups allow-scripts allow-same-origin');
  });

  it('never frames a dev server on its own port; it offers the browser until a preview origin is known', () => {
    render(<BrowserPane info={serviceTile('http://127.0.0.1:5173/app')} />);
    expect(screen.queryByTitle('Dev server')).toBeNull();
    expect(screen.getByText('This preview opens in your browser')).toBeInTheDocument();
    expect(previewCalls()).toHaveLength(0);

    act(() => setDaemonPreviewOrigin({ origin: 'http://127.0.0.1:42000', prefix: '/b/host' }));

    expect(screen.getByTitle('Dev server')).toHaveAttribute('src', 'http://127.0.0.1:42000/b/host/api/proxy/5173~b2xk/app');
    expect(screen.queryByText('This preview opens in your browser')).toBeNull();
  });

  it('frames a router backend service only on the router preview origin', () => {
    render(<BrowserPane info={serviceTile('http://127.0.0.1:5173/', { backend: 'wsl' })} />);
    expect(screen.queryByTitle('Dev server')).toBeNull();
    act(() => setRouterPreviewOrigin('http://127.0.0.1:41000'));
    expect(screen.getByTitle('Dev server')).toHaveAttribute('src', 'http://127.0.0.1:41000/b/wsl/api/proxy/5173~b2xk/');
  });

  it('does not frame a relayed preview on the app’s own origin', async () => {
    configureApi({ prefix: '/m/machine-1', token: 'jwt' });
    render(<BrowserPane info={serviceTile()} />);
    expect(await screen.findByText('This preview opens in your browser')).toBeInTheDocument();
    expect(screen.queryByTitle('Dev server')).toBeNull();
  });
});
