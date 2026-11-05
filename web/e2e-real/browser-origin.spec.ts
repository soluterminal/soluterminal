import { test, expect } from 'playwright/test';
import { createServer, type Server } from 'node:http';
import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { homedir } from 'node:os';

test('browser requests and embedded first loads use their own authority', async ({ page, request, baseURL }) => {
  expect(process.env.SOLU_REAL_E2E_SCENARIO).toBe('embedded-client');
  const health = await (await request.get('/api/health?identity=1')).json();
  expect(health.pid).toBe(Number(process.env.SOLU_REAL_E2E_PID));
  const frameURL = `${baseURL}/?embed=shinoque&theme=dark`;
  const html = `<script>window.messages=[];addEventListener('message',e=>window.messages.push(e.data))</script><iframe title="Code" src="${frameURL}"></iframe>`;
  const servers: Server[] = [];
  async function serve(port: number): Promise<string> {
    const server = createServer((_req, res) => { res.setHeader('Content-Type', 'text/html'); res.end(html); });
    servers.push(server);
    await new Promise<void>((resolve, reject) => { server.once('error', reject); server.listen(port, '127.0.0.1', resolve); });
    const address = server.address();
    if (!address || typeof address === 'string' || address.port === 8787) throw new Error('Invalid isolated listener');
    return `http://127.0.0.1:${address.port}`;
  }
  try {
    const allowed = await serve(Number(process.env.SOLU_REAL_E2E_PARENT_PORT));
    const foreign = await serve(0);
    const headers = await request.get('/');
    expect(headers.headers()['content-security-policy']).toContain(allowed);
    await page.goto(allowed);
    await expect.poll(() => page.evaluate(() => (window as unknown as { messages: { source?: string; kind?: string }[] }).messages.some(m => m.source === 'soluterminal' && m.kind === 'ready'))).toBe(true);
    const frame = page.frames().find(f => f.url().startsWith(`${baseURL}/`));
    expect(frame).toBeTruthy();
    expect(new URL(frame!.url()).searchParams.get('embed')).toBe('shinoque');

    const refusal = page.waitForEvent('console', { predicate: message => message.text().includes('frame-ancestors') });
    await page.goto(foreign);
    await refusal;
    expect(await page.evaluate(() => (window as unknown as { messages: unknown[] }).messages.length)).toBe(0);

    const file = join(homedir(), 'editor-example.txt');
    writeFileSync(file, 'before\n', { mode: 0o600 });
    const response = page.waitForResponse(r => r.url() === `${baseURL}/api/file/line` && r.request().method() === 'POST');
    await page.evaluate(async ({ base, path }) => {
      await fetch(`${base}/api/file/line`, { method: 'POST', mode: 'no-cors', headers: { 'Content-Type': 'text/plain' }, body: JSON.stringify({ path, lineNo: 0, newText: 'after', expectedMtime: 0 }) });
    }, { base: baseURL, path: file });
    expect((await response).status()).toBe(403);
    expect(readFileSync(file, 'utf8')).toBe('before\n');
    for (const path of ['/api/panes/example/io', '/api/panes/example/native/ws']) {
      const socket = await request.get(path, { headers: { Origin: foreign, Upgrade: 'websocket', Connection: 'Upgrade' } });
      expect(socket.status()).toBe(403);
    }
  } finally {
    await Promise.all(servers.map(server => new Promise<void>(resolve => server.close(() => resolve()))));
  }
});
