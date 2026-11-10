import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { api, configureApi, setBackend } from './api';
import { useStore } from './store';
import type { Port } from './types';
import { serviceTarget } from './lib/portBrowser';
const ports = [
  {
    port: 5173,
    addr: '127.0.0.1',
    pid: 1,
    process: 'node',
    cmdline: 'vite',
    cwd: '',
    proto: 'tcp',
    firstAt: '',
  },
] as Port[];
beforeEach(() => {
  configureApi({ prefix: '', token: null });
  setBackend(null);
  vi.useFakeTimers();
  useStore.setState({ ports: [], portsError: null, portsUpdatedAt: null, browsers: {} });
});
afterEach(() => {
  vi.restoreAllMocks();
  configureApi({ prefix: '', token: null });
  setBackend(null);
  vi.clearAllTimers();
  vi.useRealTimers();
});
it('discards a refresh from the previous machine', async () => {
  let resolve!: (p: Port[]) => void;
  vi.spyOn(api, 'listPorts').mockReturnValue(
    new Promise((r) => {
      resolve = r;
    }),
  );
  const pending = useStore.getState().refreshPorts();
  configureApi({ prefix: '/m/new', token: null });
  resolve(ports);
  await pending;
  expect(useStore.getState().ports).toEqual([]);
  expect(useStore.getState().portsUpdatedAt).toBeNull();
});
it('keeps the newest refresh when responses finish out of order', async () => {
  let resolve!: (p: Port[]) => void;
  vi.spyOn(api, 'listPorts')
    .mockReturnValueOnce(
      new Promise((r) => {
        resolve = r;
      }),
    )
    .mockResolvedValueOnce([]);
  const old = useStore.getState().refreshPorts();
  await useStore.getState().refreshPorts();
  resolve(ports);
  await old;
  expect(useStore.getState().ports).toEqual([]);
});
it('accepts a slow response while a later poll is still pending', async () => {
  let first!: (p: Port[]) => void;
  let second!: (p: Port[]) => void;
  vi.spyOn(api, 'listPorts')
    .mockReturnValueOnce(
      new Promise((r) => {
        first = r;
      }),
    )
    .mockReturnValueOnce(
      new Promise((r) => {
        second = r;
      }),
    );
  const a = useStore.getState().refreshPorts();
  const b = useStore.getState().refreshPorts();
  first(ports);
  await a;
  expect(useStore.getState().ports).toEqual(ports);
  second([]);
  await b;
  expect(useStore.getState().ports).toEqual([]);
});
it('preserves last-known services and reports a failure, then clears it on recovery', async () => {
  useStore.setState({ ports });
  const list = vi
    .spyOn(api, 'listPorts')
    .mockRejectedValueOnce(new Error('offline'))
    .mockResolvedValueOnce([]);
  await useStore.getState().refreshPorts();
  expect(useStore.getState().ports).toEqual(ports);
  expect(useStore.getState().portsError).toContain('Could not refresh');
  await useStore.getState().refreshPorts();
  expect(useStore.getState().portsError).toBeNull();
  expect(useStore.getState().ports).toEqual([]);
  expect(list).toHaveBeenCalledTimes(2);
});
it('displays partial-backend warnings returned by discovery', async () => {
  vi.spyOn(api, 'listPorts').mockResolvedValue(
    Object.assign(ports, { warning: 'Windows services unavailable.' }),
  );
  await useStore.getState().refreshPorts();
  expect(useStore.getState().portsError).toBe('Windows services unavailable.');
});

it('reconnects a saved browser to the replacement listener in the same pane', async () => {
  configureApi({ prefix: '/m/restart', token: null });
  const original: Port = { ...ports[0], paneId: 'app', backend: 'win', scheme: 'http', webStatus: 'ready', previewPath: '/api/proxy/5173~b2xk/' };
  const browser = { id: 'browser_restart', url: 'http://127.0.0.1:5173/app?mode=1#top', createdAt: '', service: serviceTarget(original) };
  useStore.setState({ browsers: { [browser.id]: browser } });
  const replacement: Port = { ...original, pid: 2, previewPath: '/api/proxy/5173~bmV3/' };
  vi.spyOn(api, 'listPorts').mockResolvedValue([replacement]);
  await useStore.getState().refreshPorts();
  const recovered = useStore.getState().browsers[browser.id];
  expect(recovered.url).toBe(browser.url);
  expect(recovered.service).toEqual({ ...browser.service, previewPath: replacement.previewPath });
  const stable = useStore.getState().browsers;
  await useStore.getState().refreshPorts();
  expect(useStore.getState().browsers).toBe(stable);
});

const paneService = { ...ports[0], paneId: 'app', scheme: 'http', webStatus: 'ready', previewPath: '/api/proxy/5173~b2xk/' } as Port;

it('Alt+B previews a pane service, then closes the tile', () => {
  useStore.setState({ layout: null, ports: [{ ...ports[0], port: 9000 }, paneService] });
  useStore.getState().toggleBrowser();
  const [tile] = Object.values(useStore.getState().browsers);
  expect(tile.url).toBe('http://127.0.0.1:5173');
  expect(tile.service).toEqual(expect.objectContaining({ paneId: 'app', previewPath: '/api/proxy/5173~b2xk/' }));
  useStore.getState().toggleBrowser();
  expect(useStore.getState().browsers).toEqual({});
});

it('Alt+B says there is nothing to preview instead of opening an empty tile', () => {
  const toast = vi.spyOn(useStore.getState(), 'toast');
  useStore.setState({ layout: null, ports: [{ ...ports[0] }] });
  useStore.getState().toggleBrowser();
  expect(useStore.getState().browsers).toEqual({});
  expect(toast).toHaveBeenCalledWith(expect.stringContaining('No previews yet'), expect.anything());
});

it('opens a page of the service, never another origin, in the one tile', () => {
  useStore.setState({ layout: null });
  const service = serviceTarget(paneService);
  useStore.getState().openBrowserHere(service, 'http://127.0.0.1:5173/admin?tab=2');
  useStore.getState().openBrowserHere(service, 'https://example.com/phish');
  const tiles = Object.values(useStore.getState().browsers);
  expect(tiles).toHaveLength(1);
  expect(tiles[0].url).toBe('http://127.0.0.1:5173');
  useStore.getState().openBrowserHere(service, 'http://127.0.0.1:5173/admin?tab=2');
  expect(Object.values(useStore.getState().browsers)[0].url).toBe('http://127.0.0.1:5173/admin?tab=2');
});
