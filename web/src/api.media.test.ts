import { afterEach, expect, it, vi } from 'vitest';
import { apiFetch, configureApi, ROUTER_RESTART_EVENT, routerRestartNeeded } from './api';

afterEach(() => { vi.unstubAllGlobals(); configureApi({ prefix: '', token: null }); });

it('labels JSON and bodyless writes while preserving upload formats and bytes', async () => {
  configureApi({ prefix: '', token: null });
  const fetchMock = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) => new Response(null, { status: 204 }));
  vi.stubGlobal('fetch', fetchMock);
  const json = '{"enabled":true}';
  const multipart = new FormData();
  multipart.append('audio', new Blob(['audio'], { type: 'audio/webm' }));
  await apiFetch('/api/update/check', { method: 'POST' });
  await apiFetch('/api/settings/example', { method: 'PUT', body: json });
  await apiFetch('/api/transcribe', { method: 'POST', body: multipart });
  await apiFetch('/api/panes/example/paste-file', { method: 'POST', headers: { 'Content-Type': 'application/octet-stream' }, body: new Blob(['example']) });
  await apiFetch('/api/health');
  const calls = fetchMock.mock.calls;
  expect(new Headers(calls[0][1]?.headers).get('Content-Type')).toBe('application/json');
  expect(new Headers(calls[1][1]?.headers).get('Content-Type')).toBe('application/json');
  expect(calls[1][1]?.body).toBe(json);
  expect(new Headers(calls[2][1]?.headers).has('Content-Type')).toBe(false);
  expect(calls[2][1]?.body).toBe(multipart);
  expect(new Headers(calls[3][1]?.headers).get('Content-Type')).toBe('application/octet-stream');
  expect(new Headers(calls[4][1]?.headers).has('Content-Type')).toBe(false);
});

it('reports a required desktop restart once when compatibility is used', async () => {
  const original = window.location.href;
  const notice = vi.fn();
  window.addEventListener(ROUTER_RESTART_EVENT, notice);
  try {
    window.history.replaceState(null, '', '/?solu_router=1');
    configureApi({ prefix: '', token: null });
    const fetchMock = vi.fn(async () => new Response('{}', { headers: { 'Content-Type': 'application/json' } }));
    vi.stubGlobal('fetch', fetchMock);
    await apiFetch('/api/health');
    expect(routerRestartNeeded()).toBe(false);
    fetchMock.mockImplementation(async () => new Response('{}', { headers: { 'X-Solu-Router-Restart': 'required' } }));
    await apiFetch('/api/health');
    await apiFetch('/api/health');
    expect(routerRestartNeeded()).toBe(true);
    expect(notice).toHaveBeenCalledTimes(1);
  } finally {
    window.removeEventListener(ROUTER_RESTART_EVENT, notice);
    window.history.replaceState(null, '', original);
  }
});
