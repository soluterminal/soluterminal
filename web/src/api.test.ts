import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, configureApi, setBackend } from './api';

afterEach(() => {
  vi.unstubAllGlobals();
  configureApi({ prefix: '', token: null });
  setBackend(null);
});

describe('workflow settings revisions', () => {
  it('carries the reviewed GET revision in the PUT without adding settings fields', async () => {
    const settings = { resources: {maxAgentRuns:2} };
    const fetchMock = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => new Response(JSON.stringify(settings), {
      status: 200, headers: { 'content-type': 'application/json', ETag: init?.method === 'PUT' ? '"saved"' : '"reviewed"' },
    }));
    vi.stubGlobal('fetch', fetchMock);
    const current = await api.getWorkflowSettings();
    await api.setWorkflowSettings({ ...current, resources: {maxAgentRuns:3} }, current);
    const request = fetchMock.mock.calls[1][1];
    expect(new Headers(request?.headers).get('If-Match')).toBe('"reviewed"');
    expect(JSON.parse(String(request?.body))).toEqual({ resources: {maxAgentRuns:3} });
  });
  it('reports a concurrent write rejection instead of presenting the draft as saved', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ error: 'workflow settings changed' }), {
      status: 412, headers: { 'content-type': 'application/json' },
    })));
    await expect(api.setWorkflowSettings({ resources: {maxAgentRuns:2} })).rejects.toMatchObject({ status: 412 });
  });
});

describe('api.pasteFile', () => {
  it('posts the raw file with its URL-encoded name in X-Filename', async () => {
    const fetchMock = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response(
      JSON.stringify({ path: '/home/u/.soluterminal/pastes/solu-paste-1.pdf', bytes: 9 }),
      { status: 200, headers: { 'content-type': 'application/json' } },
    ));
    vi.stubGlobal('fetch', fetchMock);
    const file = new File(['hello pdf'], 'test report.pdf', { type: 'application/pdf' });
    await expect(api.pasteFile('pane-42', file)).resolves.toEqual({ path: '/home/u/.soluterminal/pastes/solu-paste-1.pdf', bytes: 9 });
    expect(fetchMock).toHaveBeenCalledOnce();
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/panes/pane-42/paste-file');
    expect(init?.method).toBe('POST');
    const headers = new Headers(init?.headers);
    expect(headers.get('content-type')).toBe('application/pdf');
    expect(headers.get('x-filename')).toBe('test%20report.pdf');
    expect(init?.body).toBe(file);
  });

  it('surfaces the daemon refusal for a file type it does not take', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('unsupported file type\n', { status: 415 })));
    await expect(api.pasteFile('pane-42', new File(['x'], 'a.zip'))).rejects.toMatchObject({ status: 415, message: 'unsupported file type' });
  });
});
