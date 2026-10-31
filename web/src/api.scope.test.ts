import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError, configureApi, currentRoutingScope, onRoutingScopeChange, setBackend } from './api';

afterEach(() => {
  vi.unstubAllGlobals();
  configureApi({ prefix: '', token: null });
  setBackend(null);
});

describe('pane snapshot completeness',()=>{
  it('distinguishes a failed backend from a successful empty snapshot',async()=>{
    vi.stubGlobal('fetch',vi.fn(async(input:RequestInfo|URL)=>{
      if(String(input).includes('/b/win/'))return new Response('offline',{status:503});
      return new Response('[]',{status:200,headers:{'content-type':'application/json'}});
    }));
    const partial=await api.listPanesAllBackends();
    expect(partial.length).toBe(0);expect(partial.completeBackends).toEqual(['wsl']);
    vi.stubGlobal('fetch',vi.fn(async()=>new Response('[]',{status:200,headers:{'content-type':'application/json'}})));
    const complete=await api.listPanesAllBackends();
    expect(complete.completeBackends).toEqual(['wsl','win']);
  });
});

describe('pinned-scope workflow mutations', () => {
  it('pins ordinary restoration to its captured backend and refuses stale restoration before fetch', async () => {
    const fetchMock = vi.fn(async (_input:RequestInfo|URL) => new Response(JSON.stringify({id:'pane-restored'}),{status:200}));
    vi.stubGlobal('fetch', fetchMock);
    configureApi({prefix:'/m/A'});setBackend('wsl');
    const scope=currentRoutingScope(),body={cwd:'/repo',cmd:['bash'],cols:80,rows:24};
    await expect(api.createPane(body,scope)).resolves.toMatchObject({id:'pane-restored'});
    expect(String(fetchMock.mock.calls[0][0])).toBe('/m/A/b/wsl/api/panes');
    configureApi({prefix:'/m/B'});
    await expect(api.createPane(body,scope)).rejects.toBeInstanceOf(ApiError);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});

describe('onRoutingScopeChange', () => {
  it('fires on prefix and backend changes, never on token-only changes, and unsubscribes', () => {
    const listener = vi.fn();
    const unsubscribe = onRoutingScopeChange(listener);

    configureApi({ prefix: '/m/A' });
    expect(listener).toHaveBeenCalledTimes(1);
    setBackend('wsl');
    expect(listener).toHaveBeenCalledTimes(2);

    // A Supabase session refresh on the same machine must not invalidate
    // task state — token-only changes and no-op assignments stay silent.
    configureApi({ token: 'refreshed-jwt' });
    configureApi({ prefix: '/m/A' });
    setBackend('wsl');
    expect(listener).toHaveBeenCalledTimes(2);

    unsubscribe();
    configureApi({ prefix: '/m/B' });
    expect(listener).toHaveBeenCalledTimes(2);
  });
});

describe('backend-scoped sidebar preferences', () => {
  it('uses the viewed Cloud machine and backend for GET/PUT and refuses stale saves', async () => {
    const fetchMock = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response('{"prefs":{}}', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    configureApi({ prefix: '/m/A' });
    setBackend('wsl');
    const scope = currentRoutingScope();
    await api.getUIPrefs(scope);
    await api.putUIPrefs({ sidebar: { hidden: [] } }, scope);
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toEqual([
      '/m/A/b/wsl/api/settings/ui', '/m/A/b/wsl/api/settings/ui',
    ]);
    expect(fetchMock.mock.calls[1][1]?.body).toBe('{"sidebar":{"hidden":[]}}');
    configureApi({ prefix: '/m/B' });
    await expect(api.putUIPrefs({ sidebar: { hidden: [] } }, scope)).rejects.toBeInstanceOf(ApiError);
    await expect(api.getUIPrefs(scope)).rejects.toBeInstanceOf(ApiError);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});
