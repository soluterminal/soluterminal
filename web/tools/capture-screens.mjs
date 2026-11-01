#!/usr/bin/env node

// Set SOLU_CAPTURE_CPU_THROTTLE=<n> to reproduce capture under CPU contention.
import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { pathToFileURL } from 'node:url';

const screens = await import(new URL('./screens-lib.mjs', import.meta.url));

const {buildServiceFixtures,captureHarnessUrl,createServer,exitBrowserUnavailable,launchChromium,resolvePlaywright,systemStatusFixture}=screens;
export {resolvePlaywright};

function notePageCrash(page, scenario, theme) {
  page.on('crash', () => fs.writeSync(process.stderr.fd, `browser-crash: page crashed (${scenario}/${theme})\n`));
}

const NEW_TASK_PROMPT_LABEL = 'What should be built or changed?';

export function updateDeferredResponses(buildVersion) {
  const now = Date.parse('2026-08-25T10:00:00+08:00');
  const fixtures = buildServiceFixtures(now, 'governed');
  return {
    '/api/system/status': systemStatusFixture(now, 'governed'),
    '/api/workflow/stats': fixtures.stats,
    '/api/workflow/tasks': fixtures.summaries,
    '/api/workflow/stage-stats': fixtures.stageStats,
    '/api/workflow/inbox': fixtures.inbox,
    '/api/workflow/release-ledger': fixtures.releaseLedger,
    '/api/workflow/documents': { documents: fixtures.documents },
    '/api/workflow/settings': { defaultRoleBindings: {}, resources: { maxConcurrentTasks: 10, memFloorMB: 8192, diskFloorMB: 8192 } },
    '/api/workflow/runtimes/health': { runtimes: [], resolution: [], diskLow: false },
    '/api/workflow/crew-readiness': { roles: [], summary: { ready: 0, fallback: 0, unavailable: 0, advisory: 0 }, machineDisclaimer: '' },
    '/api/workflow/release-train': { policy: { enabled: false }, state: { phase: 'disabled', train: null }, preview: null, events: [] },
    '/api/workflow/release-check': { allowed: false, blockers: [], waived: [], included: [], warnings: [] },
    '/api/workflow/role-scorecard': { generatedAt: new Date(now).toISOString(), scope: { tasks: 0 }, minimumSamples: 3, cells: [] },
    '/api/workflow/campaigns': [],
    '/api/workflow/worktrees': { worktrees: [], totals: { count: 0, totalBytes: 0, sizeComplete: true, reclaimableCount: 0, reclaimableBytes: 0, orphanCount: 0, orphanBytes: 0 }, lastSweep: null, settings: { enabled: true, graceHours: 48 } },
    '/api/workflow/retention': { at: new Date(now).toISOString(), dryRun: true, retentionDays: 10, tasks: [], totals: { tasks: 0, reclaimable: 0, pruned: 0, logsBytes: 0, artifactsBytes: 0, eventsBytes: 0, totalBytes: 0 }, errors: [] },
    '/api/workflow/autopilot/status': { approvalsLastHour: 0, maxPerHour: 6, pendingCountdowns: 0, gateGranted: false, gatePolicyCount: 0, machineDisclaimer: '' },
    '/api/cc-usage/summary': { hero: { turns: 72, cost_usd: 12.34 }, by_model: [{ model: 'gpt-5.6', turns: 72 }] },
    '/api/orchestrator-hosts': { policyVersion: '3', hosts: [] },
    '/api/autonomy': { agents: [], policies: [], controllers: [], builtins: [], legacySchedules: [] },
    '/webdist.json': { webVersion: buildVersion },
  };
}

export function chromiumLaunchArgs({ socketless }) {
  if (!socketless) return undefined;
  // Engine captures run outside restricted sandboxes. Avoid single-process there:
  // its in-process renderer SIGTRAPs under the daemon spawn (int3 in Chrome_InProcRe).
  return ['--allow-file-access-from-files', '--disable-crash-reporter', '--disable-crashpad', '--in-process-gpu', '--single-process', '--no-zygote'];
}

export function updateDeferredDetailsSelector(variantName) {
  // The desktop status bar is intentionally hidden at the narrow breakpoint;
  // the existing update banner is the queued-state details entry point there.
  return variantName === 'narrow'
    ? '.update-banner-text'
    : 'button.statusbar-version-queued';
}

export function validateWorkflowDrainCapture(firstCopy, secondCopy, overlayCount) {
  const expected = /Update 0\.0\.991 ready[\s\S]*restarting in \d+:\d{2} after 2 in-flight workers finish/;
  if (!expected.test(firstCopy) || !expected.test(secondCopy) || firstCopy === secondCopy) {
    throw new Error(`workflow draining banner did not expose a live M:SS countdown: ${firstCopy} -> ${secondCopy}`);
  }
  if (overlayCount !== 0) {
    throw new Error('workflow draining evidence unexpectedly contains the Updating overlay');
  }
}

export async function waitForAppReady(page, { timeout = 60_000 } = {}) {
  await page.waitForSelector('.app', { state: 'attached', timeout });
  // The app keeps an SSE connection open, so networkidle is only a useful
  // best-effort settling signal. The statusbar is the durable proof that the
  // first application data/render cycle landed.
  await page.waitForLoadState('networkidle', { timeout: 10_000 }).catch(() => {});
  await page.waitForFunction(
    () => document.querySelector('.statusbar') != null,
    undefined,
    { timeout },
  );
}

export function screenshotManifest(shots, evidenceWarnings = []) {
  return { shots, evidenceWarnings };
}

async function newCapturePage(context) {
  const page = await context.newPage();
  const rawRate = process.env.SOLU_CAPTURE_CPU_THROTTLE;
  if (!rawRate) return page;
  const rate = Number(rawRate);
  if (!Number.isFinite(rate) || rate < 1) {
    throw new Error(`SOLU_CAPTURE_CPU_THROTTLE must be a number >= 1, got ${rawRate}`);
  }
  const cdp = await context.newCDPSession(page);
  await cdp.send('Emulation.setCPUThrottlingRate', { rate });
  return page;
}

export function reportCaptureFailure(error, {
  writeStderr = (line) => fs.writeSync(process.stderr.fd, line),
  setExitCode = (code) => { process.exitCode = code; },
} = {}) {
  const message = error instanceof Error ? error.message : String(error);
  if (message.includes("Executable doesn't exist")) {
    exitBrowserUnavailable(message);
  }
  writeStderr(`${message}\n`);
  setExitCode(1);
}

function arg(name) {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : '';
}

export async function main() {
  const dist = arg('--dist');
  const out = arg('--out');
  const driver = arg('--driver');
  const scenario = arg('--scenario') || 'default';
  const constrained = process.env.SOLU_CAPTURE_CONSTRAINED === '1';
  const socketless = process.argv.includes('--socketless') || (constrained && scenario === 'update-deferred');
  const staleBundleFilter = arg('--bundle-state');
  const themeFilter = arg('--theme');
  const updateDeferredVariant = arg('--variant');
  if (!dist || !out) {
    process.stderr.write('usage: capture-screens.mjs --dist <dir> --out <dir> [--scenario default|update-holds|update-deferred|stale-bundles] [--socketless] [--bundle-state activation|packaged] [--theme light|dark] [--driver <module>]\n');
    process.exit(2);
  }
  if (!['default', 'update-holds', 'update-deferred', 'stale-bundles'].includes(scenario)) {
    process.stderr.write(`unknown screenshot scenario: ${scenario}\n`);
    process.exit(2);
  }
  if (socketless && !['stale-bundles', 'update-deferred'].includes(scenario)) {
    process.stderr.write('--socketless is supported only for the stale-bundles and update-deferred evidence scenarios\n');
    process.exit(2);
  }
  if (staleBundleFilter && !['activation', 'packaged'].includes(staleBundleFilter)) {
    process.stderr.write(`unknown stale bundle state: ${staleBundleFilter}\n`);
    process.exit(2);
  }
  if (themeFilter && !['light', 'dark'].includes(themeFilter)) {
    process.stderr.write(`unknown capture theme: ${themeFilter}\n`);
    process.exit(2);
  }
  if (updateDeferredVariant && !['light', 'dark', 'narrow'].includes(updateDeferredVariant)) {
    process.stderr.write(`unknown update-deferred variant: ${updateDeferredVariant}\n`);
    process.exit(2);
  }

  const playwright = await resolvePlaywright();
  if (!playwright) {
    fs.writeSync(process.stderr.fd, 'Playwright module not found (tried playwright, playwright-core, and PLAYWRIGHT_MODULE_PATH)\n');
    process.exit(78);
  }

  try {
    const executable = playwright.chromium.executablePath();
    if (!executable || !fs.existsSync(executable)) {
      exitBrowserUnavailable(`chromium executable not found at ${executable || '(unresolved)'}`);
    }
  } catch (error) {
    exitBrowserUnavailable(`chromium executablePath failed: ${error instanceof Error ? error.message : String(error)}`);
  }

  let server;
  let browser;
  try {
    fs.mkdirSync(out, { recursive: true });
    // Launch before opening the local server so browser availability is
    // classified deterministically even in runtimes that restrict listeners.
    browser = await launchChromium(playwright, {
      headless: true,
      executablePath: process.env.PLAYWRIGHT_CHROMIUM_PATH || undefined,
      args: chromiumLaunchArgs({ socketless }),
    });
    if (driver) {
      const { default: capture } = await import(pathToFileURL(path.resolve(driver)).href);
      await capture({ browser, dist, out, waitForAppReady, themes: themeFilter ? [themeFilter] : ['light', 'dark'] });
      return;
    }
    browser.on('disconnected', () => fs.writeSync(process.stderr.fd, 'browser-crash: browser disconnected during capture\n'));
    const holdState = scenario === 'update-holds'
      ? {
          cause: 'focus',
          paneId: 'pane_0123456789abcdef',
          deadlineMs: Date.now() + 9 * 60_000,
        }
      : scenario === 'update-deferred'
        ? {
            cause: 'backoff',
            deadlineMs: Date.now() - 1_000,
            nextRetryMs: Date.now() + 12 * 60_000,
          }
        : null;
    let updateDeferredBuildVersion = '';
    let updateDeferredAPIResponses = null;
    if (scenario === 'update-deferred') {
      updateDeferredBuildVersion = JSON.parse(fs.readFileSync(path.join(path.resolve(dist), 'webdist.json'), 'utf8')).webVersion;
      updateDeferredAPIResponses = updateDeferredResponses(updateDeferredBuildVersion);
    }
    const captureState = {
      holdState,
      bundleState: scenario === 'stale-bundles' ? 'activation' : null,
      apiResponses: updateDeferredAPIResponses,
    };
    let address = null;
    if (!socketless) {
      server = createServer(path.resolve(dist), captureState);
      await new Promise((resolve, reject) => {
        server.once('error', reject);
        server.listen(0, '127.0.0.1', resolve);
      });
      address = server.address();
      if (!address || typeof address === 'string') throw new Error('static server did not allocate a TCP port');
    }
    const shots = [];
    const evidenceWarnings = [];
    const requiredEvidenceFailures = [];
    if (captureState.bundleState) {
      const bundleScenarios = staleBundleFilter ? [staleBundleFilter] : ['activation', 'packaged'];
      const themes = themeFilter ? [themeFilter] : ['light', 'dark'];
      for (const bundleScenario of bundleScenarios) {
        captureState.bundleState = bundleScenario;
        for (const theme of themes) {
          const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
          try {
            if (socketless) {
              const distRoot = path.resolve(dist);
              await context.route('file://**', (route) => {
                const requestPath = new URL(route.request().url()).pathname;
                if (requestPath.startsWith('/assets/') || requestPath === '/manifest.webmanifest' || requestPath.startsWith('/icon-')) {
                  const candidate = path.join(distRoot, requestPath);
                  if (fs.existsSync(candidate) && fs.statSync(candidate).isFile()) return route.fulfill({ path: candidate });
                }
                return route.continue();
              });
            }
            const page = await newCapturePage(context);
            notePageCrash(page, bundleScenario, theme);
            await page.addInitScript(({ selectedTheme, selectedBundleScenario, useSocketless }) => {
              localStorage.setItem('solu.appTheme', selectedTheme);
              localStorage.setItem('solu.seenWelcome', '1');
              localStorage.setItem('solu.seenChecklist', '1');
              if (!useSocketless) return;
              const nativeFetch = window.fetch.bind(window);
              window.fetch = async (input, init) => {
                const raw = typeof input === 'string' || input instanceof URL ? String(input) : input.url;
                const pathname = new URL(raw, window.location.href).pathname;
                const response = (value) => new Response(JSON.stringify(value), {
                  status: 200,
                  headers: { 'content-type': 'application/json; charset=utf-8' },
                });
                if (pathname === '/webdist.json') return response({ webVersion: '0.0.992' });
                if (/^\/b\/(?:wsl|win)\/api\/health$/.test(pathname)) {
                  const win = pathname.includes('/b/win/');
                  const packaged = selectedBundleScenario === 'packaged';
                  return response(win
                    ? { status: 'ok', version: packaged ? '0.0.993' : '0.0.992', webVersion: '0.0.992', backendID: 'win', panes: 0, uptimeSec: 60 }
                    : { status: 'ok', version: '0.0.993', webVersion: '0.0.993', backendID: 'wsl', panes: 0, uptimeSec: 60 });
                }
                if (/^\/b\/(?:wsl|win)\/api\/update\/status$/.test(pathname)) {
                  return response({ current: pathname.includes('/b/wsl/') ? '0.0.993' : '0.0.992', latest: '0.0.993', available: false, applying: false });
                }
                if (pathname.includes('/api/')) return response([]);
                return nativeFetch(input, init);
              };
              window.EventSource = class {
                addEventListener() {}
                close() {}
              };
            }, { selectedTheme: theme, selectedBundleScenario: bundleScenario, useSocketless: socketless });
            const captureUrl = socketless
              ? `${pathToFileURL(path.join(path.resolve(dist), 'index.html')).href}?solu_router=1&b=wsl`
              : `http://127.0.0.1:${address.port}/?solu_router=1&b=wsl`;
            await page.goto(captureUrl, { waitUntil: 'load' });
            await page.waitForSelector('.daemon-banner', { timeout: 15_000 });
            await page.waitForTimeout(250);
            const bannerCopy = await page.locator('.daemon-banner').innerText();
            const expectedCopy = bundleScenario === 'activation'
              ? 'finishing the update on the shell backend'
              : 'package contains an older interface';
            if (!bannerCopy.toLowerCase().includes(expectedCopy)) {
              throw new Error(`${bundleScenario} fixture rendered unexpected banner: ${bannerCopy}`);
            }
            if (await page.getByRole('dialog', { name: 'Release readiness' }).isVisible().catch(() => false)) {
              throw new Error('stale-bundle evidence unexpectedly contains the release-readiness dialog');
            }
            const file = `stale-bundle-${bundleScenario}-${theme}.png`;
            const outputPath = path.join(out, file);
            if (socketless) {
              // Restricted workflow sandboxes deny Chromium's screenshot GPU
              // path and all TCP listeners. Print the already-rendered page
              // through Playwright, then rasterize that one-page artifact at
              // CSS pixel density. Normal host evidence still uses screenshot.
              const pdfPath = path.join(out, `.${file}.pdf`);
              await page.pdf({ path: pdfPath, width: '1440px', height: '1000px', printBackground: true, timeout: 120_000 });
              try {
                execFileSync('python3', ['-c', [
                  'import fitz,sys',
                  'doc=fitz.open(sys.argv[1])',
                  'pix=doc[0].get_pixmap(dpi=96, alpha=False)',
                  'pix.save(sys.argv[2])',
                ].join(';'), pdfPath, outputPath]);
              } finally {
                fs.rmSync(pdfPath, { force: true });
              }
            } else {
              await page.screenshot({ fullPage: true, path: outputPath, timeout: 120_000 });
            }
            shots.push({ label: `${bundleScenario}-${theme}`, file });
          } finally {
            if (!socketless) await context.close();
          }
        }
      }
      process.stdout.write(`${JSON.stringify({ shots, transport: socketless ? 'file' : 'http' })}\n`);
      return;
    }
    if (scenario === 'update-deferred') {
      const captureVersion = updateDeferredBuildVersion;
      const variants = [
        { name: 'light', theme: 'light', viewport: { width: 1440, height: 1000 } },
        { name: 'dark', theme: 'dark', viewport: { width: 1440, height: 1000 } },
        { name: 'narrow', theme: 'dark', viewport: { width: 420, height: 900 } },
      ].filter((variant) => !updateDeferredVariant || variant.name === updateDeferredVariant);
      const constrainedContext = constrained
        ? await browser.newContext({ viewport: variants[0].viewport })
        : null;
      let fileRouteInstalled = false;
      for (const variant of variants) {
        const context = constrainedContext ?? await browser.newContext({ viewport: variant.viewport });
        try {
          if (socketless && !fileRouteInstalled) {
            const distRoot = path.resolve(dist);
            await context.route('file://**', (route) => {
              const requestPath = decodeURIComponent(new URL(route.request().url()).pathname);
              if (requestPath.startsWith('/assets/') || requestPath === '/manifest.webmanifest' || requestPath.startsWith('/icon-')) {
                const candidate = path.join(distRoot, requestPath);
                if (fs.existsSync(candidate) && fs.statSync(candidate).isFile()) return route.fulfill({ path: candidate });
              }
              return route.continue();
            });
            fileRouteInstalled = true;
          }
          const page = await context.newPage();
          notePageCrash(page, scenario, variant.name);
          if (constrained) await page.setViewportSize(variant.viewport);
          page.on('pageerror', (error) => process.stderr.write(`update-deferred page error: ${error.stack || error.message}\n`));
          page.on('console', (message) => {
            if (message.type() === 'error') process.stderr.write(`update-deferred console error: ${message.text()}\n`);
          });
          await page.addInitScript(({ selectedTheme, useSocketless, deferred, cannedResponses, builtVersion }) => {
            localStorage.setItem('solu.appTheme', selectedTheme);
            localStorage.setItem('solu.seenWelcome', '1');
            localStorage.setItem('solu.seenChecklist', '1');
            if (!useSocketless) return;
            const nativeFetch = window.fetch.bind(window);
            const response = (value, status = 200) => new Response(JSON.stringify(value), {
              status,
              headers: { 'content-type': 'application/json; charset=utf-8' },
            });
            window.fetch = async (input, init) => {
              const raw = typeof input === 'string' || input instanceof URL ? String(input) : input.url;
              const pathname = new URL(raw, window.location.href).pathname.replace(/^\/b\/(?:wsl|win)/, '');
              if (pathname === '/webdist.json') return response({ webVersion: builtVersion });
              if (pathname === '/api/health') return response({ status: 'ok', version: builtVersion, webVersion: builtVersion, backendID: 'wsl', panes: 0, uptimeSec: 3600 });
              if (pathname === '/api/update/settings') return response({ autoApply: 'off' });
              if (pathname === '/api/update/status') return response({
                current: '0.0.990', latest: '0.0.991', available: true, applying: false,
                lastCheckedMs: Date.now() - 12_000,
                notes: '- Lossless automatic updates\n- Explicit forced-update consequences',
                workflowDeferred: true,
                workflowDeferReason: 'drain deadline reached with 2 invocations in flight — deferring update',
                workflowDeferSinceMs: deferred.deadlineMs - 10 * 60_000,
                workflowDeferDeadlineMs: deferred.deadlineMs,
                workflowInflight: 2,
                workflowDeferAttempts: 2,
                workflowNextRetryMs: deferred.nextRetryMs,
                workflowDeferState: 'backoff',
              });
              if (pathname === '/api/update/apply' && init?.method === 'POST') return response({ ok: true });
              if (Object.prototype.hasOwnProperty.call(cannedResponses, pathname)) return response(cannedResponses[pathname]);
              if (pathname.startsWith('/api/')) return response([]);
              return nativeFetch(input, init);
            };
            window.EventSource = class { addEventListener() {} close() {} };
          }, {
            selectedTheme: variant.theme,
            useSocketless: socketless,
            deferred: holdState,
            cannedResponses: updateDeferredResponses(captureVersion),
            builtVersion: captureVersion,
          });
          const captureURL = captureHarnessUrl(socketless
            ? `${pathToFileURL(path.join(path.resolve(dist), 'index.html')).href}?solu_router=1&b=wsl`
            : `http://127.0.0.1:${address.port}/`);
          await page.goto(captureURL, { waitUntil: 'load' });
          const detailsTrigger = page.locator(updateDeferredDetailsSelector(variant.name));
          await detailsTrigger.waitFor({ state: 'visible' });
          await detailsTrigger.evaluate((element) => element.click());
          await page.waitForSelector('.version-popover .vp-apply');
          await page.locator('.version-popover .vp-apply').evaluate((element) => element.click());
          await page.getByText('Their work will be lost.', { exact: true }).waitFor();
          await page.waitForTimeout(250);
          const file = `update-deferred-${variant.name}.png`;
          const outputPath = path.join(out, file);
          await page.emulateMedia({ media: 'screen' });
          if (constrained) {
            // Managed workflow sandboxes can deny Chromium's compositor
            // screenshot syscall after the real bundle has rendered. Print
            // that rendered page and rasterize at CSS-pixel density, matching
            // the established adaptive-heavy capture fallback.
            const height = Math.max(variant.viewport.height, await page.evaluate(() => Math.ceil(Math.max(document.body.scrollHeight, document.documentElement.scrollHeight))));
            const pdfPath = `${outputPath}.pdf`;
            await page.pdf({
              path: pdfPath,
              width: `${variant.viewport.width}px`,
              height: `${height}px`,
              printBackground: true,
              timeout: 120_000,
            });
            try {
              execFileSync('python3', ['-c', [
                'import fitz,sys',
                'doc=fitz.open(sys.argv[1])',
                'pix=doc[0].get_pixmap(dpi=96, alpha=False)',
                'pix.save(sys.argv[2])',
              ].join(';'), pdfPath, outputPath]);
            } finally {
              fs.rmSync(pdfPath, { force: true });
            }
          } else {
            await page.screenshot({ fullPage: true, path: outputPath });
          }
          shots.push({ label: `update-deferred-${variant.name}`, file });
        } finally {
          if (!constrained) await context.close();
        }
      }
      process.stdout.write(`${JSON.stringify({ shots })}\n`);
      if (constrained) process.exit(0);
      return;
    }
    if (holdState) {
      for (const cause of ['focus', 'workflow']) {
        holdState.cause = cause;
        holdState.deadlineMs = Date.now() + 9 * 60_000;
        for (const theme of ['light', 'dark']) {
          const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
          try {
            const page = await newCapturePage(context);
            notePageCrash(page, scenario, theme);
            await page.addInitScript((selectedTheme) => {
              localStorage.setItem('solu.appTheme', selectedTheme);
              localStorage.setItem('solu.seenWelcome', '1');
              localStorage.setItem('solu.seenChecklist', '1');
            }, theme);
            await page.goto(captureHarnessUrl(`http://127.0.0.1:${address.port}/`), { waitUntil: 'load' });
            let queuedCopy;
            if (cause === 'workflow') {
              const banner = page.locator('.update-banner-draining');
              await banner.waitFor({ state: 'visible' });
              await page.getByText('Update 0.0.991 ready', { exact: true }).waitFor();
              const firstCopy = await banner.innerText();
              await page.waitForTimeout(1100);
              queuedCopy = await banner.innerText();
              validateWorkflowDrainCapture(
                firstCopy,
                queuedCopy,
                await page.locator('.update-overlay').count(),
              );
            } else {
              await page.waitForSelector('button.statusbar-version-has-update');
              await page.locator('button.statusbar-version-has-update').click();
              await page.waitForSelector('.version-popover .vp-apply');
              await page.locator('.version-popover .vp-apply').click();
              await page.waitForSelector('button.statusbar-version-queued');
              await page.waitForTimeout(250);
              queuedCopy = (await Promise.all([
                page.locator('button.statusbar-version-queued').innerText(),
                page.locator('.version-popover .vp-apply-sub').innerText(),
              ])).join(' ');
            }
            const file = `${cause}-queued-${theme}.png`;
            await page.screenshot({ fullPage: true, path: path.join(out, file) });
            shots.push({ label: `${cause}-${theme}`, file, queuedCopy });
          } finally {
            await context.close();
          }
        }
      }
      process.stdout.write(`${JSON.stringify({ shots })}\n`);
      return;
    }
    for(const theme of (themeFilter?[themeFilter]:['light','dark'])) {
      const context=await browser.newContext({viewport:{width:1440,height:1000}});
      try {
        const page=await context.newPage();
        await page.addInitScript(theme=>{localStorage.setItem('solu.appTheme',theme);localStorage.setItem('solu.seenWelcome','1');localStorage.setItem('solu.seenChecklist','1')},theme);
        await page.goto(captureHarnessUrl(`http://127.0.0.1:${address.port}/`));await waitForAppReady(page);
        await page.screenshot({fullPage:true,path:path.join(out,`workspace-${theme}.png`)});
        await page.goto(`http://127.0.0.1:${address.port}/#view=system`);await page.locator('.system-kpis').waitFor();
        await page.screenshot({fullPage:true,path:path.join(out,`system-${theme}.png`)});
      } finally {await context.close()}
    }
  } catch (error) {
    reportCaptureFailure(error);
  } finally {
    if (browser) await browser.close().catch(() => {});
    if (server) await new Promise((resolve) => server.close(resolve));
  }
}

// Guard so tests can import resolvePlaywright without running a capture.
if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  await main();
}
