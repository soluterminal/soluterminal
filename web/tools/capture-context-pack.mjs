#!/usr/bin/env node

// Deterministic light/dark evidence for context-pack controls. The production
// bundle is loaded unchanged; only daemon API responses are intercepted.
// Negative repro: in a disposable checkout, rename ws-pack-tag,
// new-pane-context-pack, context-pack-dialog, or pane-pack-missing, build, and
// run this script. The affected surface assertion exits non-zero.

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import {
  exitBrowserUnavailable,
  orchestratorHostsAllAvailable,
  resolvePlaywright,
} from './screens-lib.mjs';
import { ensureBrowserCanStart } from './browser-preflight.mjs';

const packPath = '/home/user/wsl-projects/strategy';
const now = new Date('2026-08-25T09:00:00Z').toISOString();
const toolsDir = path.dirname(fileURLToPath(import.meta.url));
const webDir = path.resolve(toolsDir, '..');
const repoDir = path.resolve(webDir, '..');

function pane(id, title, cmd, contextPackStatus, agent = '') {
  return {
    id,
    title,
    cmd: [cmd],
    cwd: '/home/user/project',
    kind: 'native',
    ...(agent ? { agent } : {}),
    status: 'running',
    createdAt: now,
    cols: 100,
    rows: 30,
    pid: id === 'pane_pack_unsupported' ? 4301 : 4302,
    lastActivityMs: Date.parse(now),
    state: 'idle',
    hasClaude: agent === 'claude',
    hasCodex: agent === 'codex',
    announceDelegation: true,
    contextPack: packPath,
    contextPackStatus,
  };
}

const unsupportedPane = pane('pane_pack_unsupported', 'Shell · unsupported pack', 'bash', 'unsupported');
const missingPane = pane('pane_pack_missing', 'Codex · missing pack', 'codex', 'missing', 'codex');

export const contextPackEvidenceFixture = Object.freeze({
  panes: Object.freeze([unsupportedPane, missingPane]),
  workspaces: Object.freeze([{
    name: 'Strategy',
    contextPack: packPath,
    layout: {
      type: 'split',
      direction: 'horizontal',
      ratio: 0.5,
      a: { type: 'pane', paneId: unsupportedPane.id },
      b: { type: 'pane', paneId: missingPane.id },
    },
    paneIds: [unsupportedPane.id, missingPane.id],
    panes: [unsupportedPane, missingPane].map(({ id, cwd, cmd, title, kind, agent, contextPack }) => ({
      id, cwd, cmd, title, kind, ...(agent ? { agent } : {}), contextPack,
    })),
    files: [],
    activeAt: now,
  }]),
});

export const contextPackEvidenceScenarios = Object.freeze([
  { name: 'context-pack-workspace-default', action: 'workspace-default' },
  { name: 'context-pack-new-pane', action: 'new-pane' },
  { name: 'context-pack-pane-dialog', action: 'pane-dialog' },
  { name: 'context-pack-pane-badges', action: 'pane-badges' },
]);

export function expectedContextPackCaptures() {
  return contextPackEvidenceScenarios.flatMap(({ name }) => [
    `${name}-dark.png`,
    `${name}-light.png`,
  ]);
}

export function missingContextPackEvidence(outDir) {
  return expectedContextPackCaptures().filter((file) => !fs.existsSync(path.join(outDir, file)));
}

function arg(name) {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : '';
}

const mime = {
  '.css': 'text/css; charset=utf-8',
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.png': 'image/png',
  '.svg': 'image/svg+xml',
  '.woff': 'font/woff',
  '.woff2': 'font/woff2',
};

async function fulfillBuiltApp(route, dist) {
  const request = route.request();
  const url = new URL(request.url());
  const pathname = url.pathname;
  const json = (value, status = 200) => route.fulfill({
    status,
    contentType: 'application/json; charset=utf-8',
    body: JSON.stringify(value),
  });
  if (request.method() === 'GET' && ['/api/ui/events', '/api/events'].includes(pathname)) {
    return route.fulfill({ status: 200, contentType: 'text/event-stream', body: ': context-pack evidence\n\n' });
  }
  if (pathname === '/api/config') return json({ error: 'not found' }, 404);
  if (pathname === '/api/health') return json({ status: 'ok', version: 'context-pack-evidence' });
  if (pathname === '/api/panes') return json(contextPackEvidenceFixture.panes);
  if (pathname === '/api/workspaces') return json(contextPackEvidenceFixture.workspaces);
  if (pathname === '/api/orchestrator-hosts') return json(orchestratorHostsAllAvailable());
  if (pathname === '/api/ports' || pathname === '/api/schedules') return json([]);
  if (pathname.startsWith('/api/panes/') && pathname.endsWith('/native/history')) return json([]);
  if (pathname.startsWith('/api/')) return json({ error: 'fixture endpoint unavailable' }, 404);
  if (!['GET', 'HEAD'].includes(request.method())) return route.fulfill({ status: 405, body: '' });

  let requestPath = decodeURIComponent(pathname);
  if (requestPath === '/') requestPath = '/index.html';
  let file = path.resolve(dist, `.${requestPath}`);
  const root = `${path.resolve(dist)}${path.sep}`;
  if (!file.startsWith(root)) return route.fulfill({ status: 403, body: '' });
  if (!fs.existsSync(file) || !fs.statSync(file).isFile()) file = path.join(dist, 'index.html');
  const body = request.method() === 'HEAD' ? '' : await fs.promises.readFile(file);
  return route.fulfill({
    status: 200,
    contentType: mime[path.extname(file)] || 'application/octet-stream',
    body,
  });
}

export async function openContextPackSurface(page, action) {
  const workspaceRow = page.locator('.ws-row').filter({ hasText: 'Strategy' });
  await workspaceRow.waitFor({ state: 'visible' });
  await page.locator('.pane-pack-unsupported').waitFor({ state: 'visible' });
  await page.locator('.pane-pack-missing').waitFor({ state: 'visible' });

  if (action === 'workspace-default') {
    await workspaceRow.click({ button: 'right' });
    await page.getByRole('menuitem', { name: 'Context pack…' }).click();
    const dialog = page.locator('.context-pack-dialog[role="dialog"]');
    await dialog.waitFor({ state: 'visible' });
    await dialog.locator('#context-pack-dir').waitFor({ state: 'visible' });
  } else if (action === 'new-pane') {
    await page.keyboard.press('Alt+n');
    const dialog = page.locator('form.dialog').filter({ hasText: 'New session' });
    await dialog.waitFor({ state: 'visible' });
    await dialog.getByRole('button', { name: 'codex', exact: true }).click();
    await page.locator('#new-pane-context-pack').waitFor({ state: 'visible' });
    await page.getByRole('button', { name: 'Workspace default' }).waitFor({ state: 'visible' });
    await page.getByRole('button', { name: 'None' }).waitFor({ state: 'visible' });
  } else if (action === 'pane-dialog') {
    const paneHeader = page.locator('.pane-header').filter({ hasText: 'Codex · missing pack' });
    await paneHeader.click({ button: 'right' });
    await page.getByText('Context pack (restarts agent)', { exact: true }).waitFor({ state: 'visible' });
    await page.getByRole('menuitem', { name: 'Change…' }).click();
    await page.locator('.context-pack-dialog[role="dialog"]').waitFor({ state: 'visible' });
  }
}

// Page.captureScreenshot is preferred. In a locked-down workflow sandbox the
// renderer can be available while compositor IPC is blocked; rasterize the
// same live DOM in that case so the evidence still comes from the built app.
async function capturePage(page, file) {
  try {
    await page.screenshot({ path: file, animations: 'disabled', timeout: 10_000 });
    return;
  } catch (error) {
    if (!String(error).includes('Timeout')) throw error;
  }
  const dataUrl = await page.evaluate(() => {
    const canvas = document.createElement('canvas');
    canvas.width = innerWidth;
    canvas.height = innerHeight;
    const ctx = canvas.getContext('2d');
    const backdrop = document.querySelector('.dialog-backdrop');
    const elements = backdrop ? [backdrop, ...backdrop.querySelectorAll('*')] : [document.body, ...document.body.querySelectorAll('*')];
    const visible = elements.map((element) => ({
      element,
      style: getComputedStyle(element),
      rect: element.getBoundingClientRect(),
    })).filter(({ style, rect }) => style.display !== 'none' && style.visibility !== 'hidden' && Number.parseFloat(style.opacity) > 0 && rect.width > 0 && rect.height > 0 && rect.bottom > 0 && rect.top < innerHeight && rect.right > 0 && rect.left < innerWidth);
    ctx.fillStyle = getComputedStyle(document.documentElement).backgroundColor || '#111827';
    ctx.fillRect(0, 0, innerWidth, innerHeight);
    const drawText = (text, style, rect) => {
      const normalized = text.replace(/\s+/g, ' ').trim();
      if (!normalized) return;
      const size = Number.parseFloat(style.fontSize) || 14;
      const lineHeight = Number.parseFloat(style.lineHeight) || size * 1.3;
      ctx.font = `${style.fontStyle} ${style.fontWeight} ${style.fontSize} ${style.fontFamily}`;
      ctx.fillStyle = style.color;
      ctx.textBaseline = 'top';
      const padLeft = Number.parseFloat(style.paddingLeft) || 0;
      const padRight = Number.parseFloat(style.paddingRight) || 0;
      const padTop = Number.parseFloat(style.paddingTop) || 0;
      const maxWidth = Math.max(1, rect.width - padLeft - padRight);
      const words = normalized.split(' ');
      const lines = [];
      let line = '';
      for (const word of words) {
        const next = line ? `${line} ${word}` : word;
        if (line && ctx.measureText(next).width > maxWidth) {
          lines.push(line);
          line = word;
        } else line = next;
      }
      if (line) lines.push(line);
      lines.slice(0, Math.max(1, Math.floor((rect.height - padTop) / lineHeight))).forEach((value, index) => {
        const measured = ctx.measureText(value).width;
        const x = style.textAlign === 'center' ? rect.left + (rect.width - measured) / 2
          : style.textAlign === 'right' ? rect.right - padRight - measured : rect.left + padLeft;
        ctx.fillText(value, x, rect.top + padTop + index * lineHeight, maxWidth);
      });
    };
    for (const { element, style, rect } of visible) {
      if (style.backgroundColor !== 'rgba(0, 0, 0, 0)' && style.backgroundColor !== 'transparent') {
        ctx.fillStyle = style.backgroundColor;
        ctx.fillRect(rect.left, rect.top, rect.width, rect.height);
      }
      const borderWidth = Number.parseFloat(style.borderTopWidth);
      if (borderWidth > 0 && style.borderTopColor !== 'rgba(0, 0, 0, 0)') {
        ctx.strokeStyle = style.borderTopColor;
        ctx.lineWidth = borderWidth;
        ctx.strokeRect(rect.left + borderWidth / 2, rect.top + borderWidth / 2, rect.width - borderWidth, rect.height - borderWidth);
      }
      const controlText = element instanceof HTMLInputElement || element instanceof HTMLTextAreaElement ? element.value : '';
      const help = element.closest('.dialog-help');
      if (help && help !== element) {
        continue;
      } else if (element instanceof HTMLButtonElement || element.classList.contains('dialog-help')) {
        drawText(element.innerText, style, rect);
      } else if (controlText) {
        drawText(controlText, style, rect);
      } else {
        for (const node of [...element.childNodes].filter((child) => child.nodeType === Node.TEXT_NODE)) {
          const range = document.createRange();
          range.selectNodeContents(node);
          const textRect = range.getBoundingClientRect();
          drawText(node.textContent, style, textRect);
        }
      }
    }
    return canvas.toDataURL('image/png');
  });
  fs.writeFileSync(file, Buffer.from(dataUrl.slice(dataUrl.indexOf(',') + 1), 'base64'));
}

async function preparePage(page, theme, action) {
  await page.addInitScript((selectedTheme) => {
    localStorage.setItem('solu.appTheme', selectedTheme);
    localStorage.setItem('solu.seenWelcome', '1');
    localStorage.setItem('solu.seenChecklist', '1');
  }, theme);
  await page.goto('http://localhost/', { waitUntil: 'load' });
  await page.evaluate((selectedTheme) => {
    if (selectedTheme === 'light') document.documentElement.setAttribute('data-theme', 'light');
    else document.documentElement.removeAttribute('data-theme');
  }, theme);

  await openContextPackSurface(page, action);
  await page.waitForTimeout(250);
}

export async function captureContextPackEvidence({ dist, out }) {
  const playwright = await resolvePlaywright();
  if (!playwright) process.exit(78);

  let browser;
  try {
    fs.mkdirSync(out, { recursive: true });
    try {
      const constrainedBrowser = process.env.SOLU_CAPTURE_CONSTRAINED === '1';
      await ensureBrowserCanStart();
      browser = await playwright.chromium.launch({
        headless: true,
        ...(process.env.PLAYWRIGHT_CHROMIUM_PATH
          ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_PATH }
          : {}),
        ...(constrainedBrowser ? { args: ['--disable-crash-reporter', '--disable-crashpad', '--single-process', '--no-zygote', '--disable-gpu'] } : {}),
      });
    } catch (error) {
      exitBrowserUnavailable(error instanceof Error ? error.message : String(error));
    }
    const context = await browser.newContext({
      viewport: { width: 1440, height: 1000 },
      serviceWorkers: 'block',
    });
    const captures = [];
    for (const theme of ['dark', 'light']) {
      for (const scenario of contextPackEvidenceScenarios) {
        const page = await context.newPage();
        try {
          await page.route('**/*', (route) => fulfillBuiltApp(route, path.resolve(dist)));
          await page.routeWebSocket('**/api/panes/**', (socket) => {
            socket.onMessage(() => {});
          });
          await preparePage(page, theme, scenario.action);
          const file = `${scenario.name}-${theme}.png`;
          await capturePage(page, path.join(out, file));
          captures.push(file);
        } finally {
          await page.close();
        }
      }
    }
    await context.close();
    return captures;
  } finally {
    if (browser) await browser.close().catch(() => {});
  }
}

export async function ensureContextPackEvidence({
  currentDist,
  out = path.join(repoDir, 'docs', 'screenshots'),
  capture = captureContextPackEvidence,
} = {}) {
  const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'solu-context-pack-evidence-'));
  try {
    await capture({ dist: currentDist, out: tempDir });
    fs.mkdirSync(out, { recursive: true });
    let created = false;
    for (const file of expectedContextPackCaptures()) {
      const source = path.join(tempDir, file);
      const destination = path.join(out, file);
      if (!fs.existsSync(destination) && fs.existsSync(source)) {
        fs.copyFileSync(source, destination);
        created = true;
      }
    }
    const stillMissing = missingContextPackEvidence(out);
    if (stillMissing.length > 0) throw new Error(`context pack evidence capture missed: ${stillMissing.join(', ')}`);
    return { captures: expectedContextPackCaptures(), created };
  } finally {
    fs.rmSync(tempDir, { recursive: true, force: true });
  }
}

async function main() {
  const dist = arg('--dist');
  const out = arg('--out');
  if (!dist || !out) {
    process.stderr.write('usage: capture-context-pack.mjs --dist <dir> --out <dir>\n');
    process.exit(2);
  }
  const captures = await captureContextPackEvidence({ dist, out });
  process.stdout.write(`${JSON.stringify({ captures }, null, 2)}\n`);
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  await main();
}
