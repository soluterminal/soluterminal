// Shared Playwright screenshot/smoke infrastructure.
// Exit 78 means the Playwright module is missing or incompatible; exit 79
// means the module resolved but its Chromium browser executable is absent.

import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { ensureBrowserCanStart } from './browser-preflight.mjs';

export async function launchChromium(playwright, options) {
  await ensureBrowserCanStart();
  return playwright.chromium.launch(options);
}

// web/src/components/DaemonHealthBanner.tsx renders one `.daemon-banner`
// variant at a time. Every bundle-state variant carries
// `.daemon-banner-dismiss` (lines 102/132/163), keyed by `dismissedFor` so it
// stays dismissed across later health polls; the unreachable-only role="alert"
// variant (line 173) intentionally has none so a capture against an unreachable
// daemon fixture fails loudly. web/src/styles.css:462-466 fixes the banner at
// --z-banner-daemon (9500), above the Topbar, where it intercepts pointer events
// aimed at `.chrome-btn` controls.
export const CAPTURE_ENVIRONMENT_PREFIX = 'capture-environment:';

export function captureEnvironmentError(detail, options) {
  return new Error(`${CAPTURE_ENVIRONMENT_PREFIX} ${detail}`, options);
}

export async function dismissDaemonBanners(page, { timeout = 5_000 } = {}) {
  const dismissed = [];
  const banner = page.locator('.daemon-banner').first();
  let bannerCount = await banner.count();
  for (let round = 0; round < 3; round += 1) {
    if (bannerCount === 0) return dismissed;
    const text = (await banner.innerText().catch(() => '')).replace(/\s+/g, ' ').trim();
    const dismiss = banner.locator('.daemon-banner-dismiss');
    if ((await dismiss.count()) === 0) {
      throw captureEnvironmentError(`daemon banner has no dismiss control and overlays the chrome: ${text}`);
    }
    await dismiss.click({ timeout });
    await banner.waitFor({ state: 'hidden', timeout });
    dismissed.push(text);
    bannerCount = await banner.count();
  }

  if (bannerCount === 0) return dismissed;
  const text = (await banner.innerText().catch(() => '')).replace(/\s+/g, ' ').trim();
  throw captureEnvironmentError(`daemon banner kept reappearing after 3 dismissals: ${text}`);
}

export async function clickChromeControl(page, target, { attempts = 3, timeout = 10_000, label = 'chrome control' } = {}) {
  for (let attempt = 1; attempt <= attempts; attempt += 1) {
    for (const text of await dismissDaemonBanners(page)) {
      fs.writeSync(process.stderr.fd, `dismissed daemon banner before clicking ${label}: ${text}\n`);
    }
    try {
      await target.click({ timeout });
      return;
    } catch (error) {
      const detail = error instanceof Error ? error.message : String(error);
      const intercepted = detail.includes('intercepts pointer events');
      if (!intercepted) {
        throw new Error(`${detail} (failed after ${attempt} attempt${attempt === 1 ? '' : 's'})`, { cause: error });
      }
      if (attempt < attempts) continue;
      if (detail.includes('daemon-banner')) {
        const banner = page.locator('.daemon-banner').first();
        const bannerText = (await banner.count()) > 0
          ? (await banner.innerText().catch(() => '')).replace(/\s+/g, ' ').trim()
          : '';
        const namedBanner = bannerText ? `${bannerText}; ${detail}` : detail;
        throw captureEnvironmentError(
          `daemon banner blocked ${label}: ${namedBanner} (failed after ${attempts} attempts)`,
          { cause: error },
        );
      }
      throw new Error(`${detail} (failed after ${attempts} attempts)`, { cause: error });
    }
  }
}

export async function openSettingsDialog(page, { attempts = 3, timeout = 10_000 } = {}) {
  const settings = page.getByRole('button', { name: 'Settings', exact: true }).first();
  const dialog = page.getByRole('dialog', { name: 'Settings', exact: true });
  await clickChromeControl(page, settings, { attempts, timeout, label: 'Settings' });
  await dialog.waitFor({ state: 'visible', timeout });
}

export function captureHarnessUrl(base) {
  try {
    const url = new URL(base);
    url.searchParams.set('solu_capture', '1');
    return url.href;
  } catch (error) {
    // Playwright accepts paths relative to its configured baseURL. Keep that
    // form intact instead of resolving it against an unrelated placeholder
    // origin merely to edit the query string.
    if (!(error instanceof TypeError) || typeof base !== 'string') throw error;
    const hashIndex = base.indexOf('#');
    const beforeHash = hashIndex >= 0 ? base.slice(0, hashIndex) : base;
    const hash = hashIndex >= 0 ? base.slice(hashIndex) : '';
    const queryIndex = beforeHash.indexOf('?');
    const path = queryIndex >= 0 ? beforeHash.slice(0, queryIndex) : beforeHash;
    const params = new URLSearchParams(queryIndex >= 0 ? beforeHash.slice(queryIndex + 1) : '');
    params.set('solu_capture', '1');
    return `${path}?${params.toString()}${hash}`;
  }
}

export async function resolvePlaywright(candidates = ['playwright', 'playwright-core', process.env.PLAYWRIGHT_MODULE_PATH]) {
  for (const candidate of candidates.filter(Boolean)) {
    // ESM import() cannot load a bare directory (the common shape of
    // PLAYWRIGHT_MODULE_PATH: .../node_modules/playwright), so resolve a
    // directory to its package entry file first.
    const specifiers = [candidate];
    try {
      if (path.isAbsolute(candidate) && fs.statSync(candidate).isDirectory()) {
        const require = createRequire(import.meta.url);
        specifiers.unshift(pathToFileURL(require.resolve(candidate)).href);
      }
    } catch { /* fall through to the raw specifier */ }
    for (const specifier of specifiers) {
      try {
        const mod = await import(specifier);
        if (mod.chromium) return mod;
        if (mod.default?.chromium) return mod.default;
      } catch { /* try the next optional source */ }
    }
  }
  return null;
}

const mime = {
  '.css': 'text/css; charset=utf-8',
  '.html': 'text/html; charset=utf-8',
  '.ico': 'image/x-icon',
  '.js': 'text/javascript; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.png': 'image/png',
  '.svg': 'image/svg+xml',
  '.woff': 'font/woff',
  '.woff2': 'font/woff2',
};

function json(res, value, status = 200) {
  res.writeHead(status, { 'content-type': 'application/json; charset=utf-8' });
  res.end(JSON.stringify(value));
}

export function systemStatusFixture(opts = {}) {
  const now = typeof opts === 'number' ? opts : (opts.now ?? Date.now());
  let scenario = arguments.length > 1 ? arguments[1] : 'default';
  if (typeof opts !== 'number' && typeof opts.scenario === 'string') scenario = opts.scenario;
  const sampledAt = new Date(now).toISOString();
  const status = {
    generatedAt: sampledAt, sampledAt, treeSampledAt: sampledAt, sampleIntervalSec: 15,
    platform: { os: 'linux', procfs: true },
    level: { overall: 'ok', reasons: [], areas: { disk: 'ok', hostMemory: 'ok', daemonMemory: 'ok', daemonCpu: 'ok', engine: 'ok', runtimes: 'ok', automation: 'ok', cloud: 'ok' } },
    daemon: { version: '0.0.1020', pid: 100, uptimeSec: 3600, startedAt: new Date(now - 3_600_000).toISOString(), rssMB: 420, goHeapMB: 180, goroutines: 92, cpuPct: 3.2, memGuardMB: 6144, webVersion: '0.0.1020', backendID: 'wsl', shellVersion: '0.0.1020' },
    host: { cores: 12, cpuPct: 18, loadAvg1: 1.4, memTotalMB: 32768, memAvailableMB: 18432, memFloorMB: 2048 },
    footprint: { totalRssMB: 1736, daemonRssMB: 420, panesRssMB: 1100, workersRssMB: 216, paneCount: 2, workerCount: 1, procs: 9, cpuPct: 12, complete: true },
    panes: [
      { id: 'pane_ops', title: 'Operations', agent: 'codex', kind: 'native', role: 'execute', status: 'running', state: 'busy', pid: 101, procs: 4, rssMB: 740, cpuPct: 6.5, agentRssMB: 620 },
      { id: 'pane_shell', title: 'Release shell', agent: '', kind: '', role: '', status: 'running', state: 'idle', pid: 102, procs: 2, rssMB: 360, cpuPct: .8, agentRssMB: null },
    ],
    workers: [{ taskId: 'wft_run00003', taskTitle: 'Repair Codex MCP permission prompt detection', stage: 'verify', role: 'verify', runtime: 'codex', pid: 103, procs: 2, rssMB: 216, cpuPct: 1.9, startedAt: new Date(now - 600_000).toISOString() }],
    disk: {
      layer: 'local', blockingLayer: 'none', anyLow: false, host: null, path: '/home/user/.solu',
      freeMB: 65536, totalMB: 262144, freeBytes: 65536 * 1024 ** 2, usedBytes: 196608 * 1024 ** 2, totalBytes: 262144 * 1024 ** 2,
      floorMB: 8192, low: false, measuredAt: sampledAt,
      breakdown: [
        { name: 'workflow_worktrees', category: 'workflow_worktrees', label: 'Workflow worktrees', bytes: 45 * 1024 ** 3, apparentBytes: 45 * 1024 ** 3, allocatedBytes: 44 * 1024 ** 3, measured: true, complete: true, protection: 'managed', cleanup: 'workflow_gc', targetCount: 3, scannedAt: sampledAt },
        { name: 'retained_task_history', category: 'retained_task_history', label: 'Retained task history', bytes: 2 * 1024 ** 3, apparentBytes: 2 * 1024 ** 3, measured: true, complete: true, protection: 'managed', cleanup: 'retention', targetCount: 12, scannedAt: sampledAt },
        { name: 'shared_go_build', category: 'shared_go_build', label: 'Legacy or external Go build cache', bytes: 256 * 1024 ** 2, apparentBytes: 256 * 1024 ** 2, measured: true, complete: true, protection: 'read-only', cleanup: 'none', targetCount: 1, scannedAt: sampledAt },
        { name: 'managed_go_build', category: 'managed_go_build', label: 'Owned Go build cache', bytes: 256 * 1024 ** 2, apparentBytes: 256 * 1024 ** 2, measured: true, complete: true, protection: 'managed', cleanup: 'generated_storage_janitor', targetCount: 1, scannedAt: sampledAt },
        { name: 'external_user_cache', category: 'external_user_cache', label: 'External user cache', bytes: 112 * 1024 ** 3, apparentBytes: 112 * 1024 ** 3, measured: true, complete: false, protection: 'read-only', cleanup: 'none', targetCount: 20, reasons: ['entry_limit'], scannedAt: sampledAt },
        { name: 'protected_recovery_data', category: 'protected_recovery_data', label: 'Protected recovery data', bytes: 59.5 * 1024 ** 3, apparentBytes: 59.5 * 1024 ** 3, measured: true, complete: true, protection: 'protected', cleanup: 'none', targetCount: 1, scannedAt: sampledAt },
      ],
      growth: {
        day: { window: '24h', available: true, continuity: 'ok', baselineAt: new Date(now - 86_400_000).toISOString(), latestAt: sampledAt, deltas: [{ category: 'workflow_worktrees', label: 'Workflow worktrees', baselineBytes: 40 * 1024 ** 3, latestBytes: 45 * 1024 ** 3, deltaBytes: 5 * 1024 ** 3, noise: false }] },
        week: { window: '168h', available: false, continuity: 'reboot', deltas: [] },
      },
      cleanup: {
        scannedAt: sampledAt, complete: true,
        reclaimableBytes: 3 * 1024 ** 3, reclaimableTmpBytes: 1 * 1024 ** 3, reclaimableWorktreeBytes: 2 * 1024 ** 3, reclaimedBytes: 1 * 1024 ** 3,
        previewOnly: true, scope: '0 eligible under current workflow GC policy',
        operations: [
          { at: new Date(now - 3_600_000).toISOString(), kind: 'worktree_gc', source: 'manual', layer: 'guest', executed: true, dryRun: false, complete: true, verified: false, guestFreeDeltaBytes: 2 * 1024 ** 3 },
          { at: new Date(now - 600_000).toISOString(), kind: 'worktree_gc', source: 'manual', layer: 'guest', executed: false, dryRun: true, complete: true, verified: false, estimatedBytes: 3 * 1024 ** 3 },
        ],
        skips: [{ reason: 'within retention window', count: 4 }],
        state: 'guest_only_pending_host_reclaim', message: 'Linux space freed; Windows image still allocated; maintenance needed',
      },
      breakdownAt: sampledAt, measuring: false, fresh: true, nextMeasureAfter: new Date(now + 600_000).toISOString(), goCacheBytes: 512 * 1024 ** 2, worktreesBytes: 45 * 1024 ** 3,
    },
    engine: { available: true, queue: { cap: 4, running: 1, runnable: 1, capacityQueued: 0, dependencyBlocked: 0, overlapSerialized: 0, awaitingApproval: 1, quotaPaused: 0, landingWait: 0, resourceWait: 0, heavyCap: 2, heavyRunning: 1, resourceDeferrals: 0, memFloorMB: 2048, diskFloorMB: 8192, diskFreeMB: 65536, diskLow: false, utilization: .25 }, runningInvocations: 1, decide: 1, gcLastSweepAt: sampledAt },
    runtimes: [{ id: 'codex', provider: 'openai', status: 'healthy', installed: true, lastInvocation: null }],
    automation: { agents: { total: 3, paused: 1, running: 1, blocked: 0, errors24h: 0, nextRunAt: new Date(now + 900_000).toISOString() }, schedules: { enabled: 2, disabled: 1, running: 0, errors24h: 0, nextFireAt: new Date(now + 1_800_000).toISOString() }, builtins: [], autonomy: { globalPaused: false, pausedCount: 0 }, warden: { enabled: true, lastRunAt: sampledAt, lastRunStatus: 'ok' } },
    surface: { addr: '127.0.0.1:8787', ports: { total: 2, paneOwned: 1 }, previews: { active: 0 }, cloud: { configured: true, hasCreds: true, credsInvalid: false, relayState: 'online' } },
    release: { running: '0.0.1020', latest: '0.0.1020', available: false, applying: false, lastSuccessMs: now, lastError: '', shellCurrent: '0.0.1020', shellLatest: '0.0.1020', shellAvailable: false, readiness: null },
  };
  if (opts?.hostDisk) {
    const offender = { label: '%TEMP%\\wsl-crashes', path: '~\\AppData\\Local\\Temp\\wsl-crashes', mb: 51_200, files: 10, dumps: 10, complete: true, text: '51.0 GB of WSL crash dumps (10 .dmp files) in %TEMP%\\wsl-crashes' };
    const hostReason = `host disk: 40.0 GB free on C: (2.0%) · VHDX 553 GB physical, not sparse · ${offender.text} · unexplained host loss 49.9 GB`;
    status.level = { overall: 'critical', reason: hostReason, reasons: [{ key: 'hostDisk', level: 'critical', text: hostReason }], areas: { ...status.level.areas, hostDisk: 'critical', engine: 'warn' } };
    status.disk = { ...status.disk, layer: 'guest', freeMB: 682_000, totalMB: 1_048_576, low: false, blockingLayer: 'host', anyLow: true, host: { available: true, state: 'ok', detail: '', volume: 'C:\\', distro: 'Ubuntu', freeMB: 41_000, totalMB: 2_050_000, floorMB: 102_500, floorPct: 5, hysteresisMB: 4096, low: true, vhdxPath: '~\\AppData\\Local\\wsl\\{…}\\ext4.vhdx', vhdxPhysicalMB: 566_000, vhdxLogicalMB: 1_048_576, sparse: false, sampledAt, runbook: 'docs/RUNBOOK-wsl-host-disk.md', unexplainedLossMB: 51_095, vhdxGrowthMB: 0, lossWindowMin: 43, offender } };
  }
  return status;
}

export function hostStorageFixture(now = Date.now()) {
  const status = systemStatusFixture({ hostDisk: true, now });
  return { available: true, state: 'ok', sample: status.disk.host, policy: { floorMB: { value: 102400, source: 'adaptive' }, floorPct: { value: 5, source: 'adaptive' }, hysteresisMB: { value: 4096, source: 'adaptive' }, browserReserveMB: { value: 8192, source: 'adaptive' } }, pressure: { guestFreeMB: 682_000, guestFloorMB: 8192, guestLow: false, hostLow: true, blockingLayer: 'host', unexplainedLossMB: 51_095, offender: status.disk.host.offender.text },
    capacitySamples: [
      { at: new Date(now-300000).toISOString(), kind: 'boot' },
      { at: new Date(now-240000).toISOString(), kind: 'sample', hostFreeMB: 41000, vhdxPhysicalMB: 566000 },
      { at: new Date(now-60000).toISOString(), kind: 'sample', hostFreeMB: 40000, vhdxPhysicalMB: 567000 },
    ],
    operations: [
      { at: new Date(now-120000).toISOString(), kind: 'worktree_gc', source: 'manual', layer: 'guest', executed: true, dryRun: false, complete: true, verified: false, guestFreeDeltaBytes: 2 * 1024 ** 3 },
      { at: new Date(now-60000).toISOString(), kind: 'windows_maintenance', source: 'cli', layer: 'host', executed: false, dryRun: true, complete: false, verified: false, estimatedBytes: 3 * 1024 ** 3 },
    ],
    events: [
    { at: new Date(now-360000).toISOString(), kind: 'preflight', source: 'launcher' },
    { at: new Date(now-300000).toISOString(), kind: 'boot' },
    { at: new Date(now-240000).toISOString(), kind: 'sample' },
    { at: new Date(now-180000).toISOString(), kind: 'crossing' },
    { at: new Date(now-120000).toISOString(), kind: 'host_loss', active: true, unexplainedLossMB: 51_095, vhdxGrowthMB: 0, lossWindowMin: 43, offender: '%TEMP%\\wsl-crashes', offenderMB: 51_200, offenderDumps: 10, offenderComplete: true, suggestedPath: '~\\AppData\\Local\\Temp\\wsl-crashes' },
    { at: new Date(now-90000).toISOString(), kind: 'admission_blocked', cmd: 'capture-screens', class: 'browser', unexplainedLossMB: 51_095, offender: '%TEMP%\\wsl-crashes', offenderMB: 51_200, offenderDumps: 10 },
    { at: new Date(now-60000).toISOString(), kind: 'sample' },
  ] };
}

export function orchestratorHostsAllAvailable() {
  return {
    policyVersion: '3',
    hosts: [
      { agent: 'claude', label: 'Claude', supported: true, installed: true, available: true },
      { agent: 'codex', label: 'Codex', supported: true, installed: true, available: true },
      { agent: 'muse', label: 'Muse', supported: false, installed: true, available: false, reason: 'Muse Code has no MCP client' },
    ],
  };
}

export function orchestratorHostsCodexUnavailable(reason) {
  const info = orchestratorHostsAllAvailable();
  info.hosts[1] = { ...info.hosts[1], installed: false, available: false, reason };
  return info;
}

export function fixtureHealthVersion(dist, fallback = 'dev') {
  try {
    const manifest = JSON.parse(fs.readFileSync(path.join(dist, 'webdist.json'), 'utf8'));
    if (typeof manifest.webVersion === 'string' && manifest.webVersion.trim()) {
      return manifest.webVersion.trim();
    }
  } catch {
    // Capture fixtures without provenance use a dev-like daemon version.
  }
  return fallback;
}

// A confirmed team install. Team-only surfaces (Tasks, Orchestrator, Task
// Owners, Agents) stay hidden without it; dictation and phone access need at
// least a free account.
export const TEAM_ENTITLEMENTS = {
  tier: 'team',
  team: true,
  account: true,
  accountSwitching: true,
  teamCredentials: true,
  remoteAccess: true,
  push: true,
  voice: true,
  smartApprovals: true,
  source: 'machine',
};

// These are the ambient reads issued whenever the real app shell mounts. They
// are explicit so createServer can fail closed for an unknown API without
// turning every expected background poll into a browser console error.
export function buildAmbientAppObjectResponses(healthVersion, now = Date.parse('2026-08-10T10:00:00Z')) {
  const at = new Date(now).toISOString();

  return {
    // An empty config is the fixture server's explicit local-mode sentinel.
    '/api/config': {},
    // Desktop shell mounts the phone-access status poller during ordinary smoke.
    '/api/cloud/status': { configured: false, hasCreds: false, credsInvalid: false, claimCorrupt: false, relayState: 'unconfigured', pairing: { active: false } },
    // api.ts reads this at module evaluation, before any React surface mounts.
    '/api/enter-gap': { claudeMs: 400, defaultMs: 100 },
    // Team install: the smoke covers the full credentials surface.
    '/api/entitlements': TEAM_ENTITLEMENTS,
    // Settings → Devices reads the anonymous check-in preference.
    '/api/settings/checkin': { enabled: true },
    // The Claude Code / Codex opt-in: an answered install, so the first-run
    // question never covers the screens under test.
    '/api/settings/agent-integration': {
      state: 'on',
      source: 'user',
      effective: 'on',
      needsAnswer: false,
      noticePending: false,
      operatorDefault: true,
      changes: [],
    },
    // The pane context menu checks for a local Jev key (smart approvals).
    '/api/workflow/runtimes/typesafe-api/secrets': {
      runtime: 'typesafe-api',
      secrets: [{ env: 'TYPESAFE_API_KEY', required: true, configured: true }],
    },
    '/api/update/status': {
      current: healthVersion,
      latest: healthVersion,
      available: false,
      applying: false,
    },
    '/api/recovery/settings': { resumeAfterRestart: 'manual' },
    '/api/native/settings': { promptSuggestions: false, defaultClaudeMode: '' },
    '/api/update/settings': { autoApply: 'off', shellAutoApply: 'off' },
    // Preview channel: fixtures model a machine without a source checkout.
    '/api/update/preview': { available: false, main: {}, newChanges: 0, ahead: 0 },
    '/api/warden/settings': { enabled: false },
    '/api/aria/status': { paused: false, auto_remaining_hr: 6, auto_max_per_hr: 6 },
    '/api/aria/auto-pending': { pending: [] },
    '/api/claude/accounts': {
      active: '',
      machineWide: false,
      currentUnsaved: false,
      accounts: [],
    },
    '/api/claude/autoswitch': {
      enabled: false,
      pinned: '',
      switchThresholdPct: 80,
      softWarnPct: 60,
      pool: {},
      state: 'paused',
      statusLine: 'Off',
    },
    '/api/claude/team': { available: false },
    '/api/codex/accounts': { accounts: [], active: '', liveAuthPresent: false },
    '/api/codex/team': { available: false },
    '/api/muse/accounts': {
      accounts: [],
      active: '',
      liveAuthPresent: false,
      livePersonal: false,
      authPath: '',
    },
    '/api/muse/team': { available: false },
    '/api/feedback/inbox': { events: [] },
    '/api/warden/status': { enabled: false, runs: [] },
    '/api/agents/status': {
      fixAgent: null,
      prAgent: null,
      cloud: null,
      generatedAt: null,
      fetchedAt: at,
    },
    // The status bar refreshes the digest with the task list, and the Tasks
    // first-run crew explainer reads runtime health. Both are object APIs, so
    // they need explicit shapes rather than either the list or 501 fallback.

    '/api/workflow/runtimes/health': {runtimes:[],resolution:[]},
  };
}

const ambientAppListResponses = new Set([
  '/api/agents',
  '/api/configuration/history',
  '/api/agents/reports',
  '/api/workflow/activation',
  '/api/workflow/suggestions',
  '/api/workflow/workstreams',
]);

// This fixture never runs a daemon, so /api/health must report a dev-like
// version or one equal to the served /webdist.json. The old 0.0.990 literal
// caused a packaged-mismatch banner against every real capture build.
export function createServer(dist, state = {}) {
  const workflowObjectResponses = buildServiceObjectResponses();
  const ambientAppObjectResponses = buildAmbientAppObjectResponses(state.healthVersion ?? 'dev');
  return http.createServer((req, res) => {
    const holdState = state.holdState ?? null;
    const bundleState = state.bundleState ?? null;
    const healthVersion = state.healthVersion ?? 'dev';
    const url = new URL(req.url || '/', 'http://127.0.0.1');
    // ClaudeInstallBanner rechecks on desktop mount. This is a specific
    // fixture response, never a real CLI invocation or blanket POST success.
    if (req.method === 'POST' && url.pathname === '/api/setup/claude/recheck') {
      return json(res, state.claudeBootstrapStatus ?? {
        claudeChecked: true, claudeInstalled: true,
        claudeVersion: 'fixture', claudeAuthed: true,
      });
    }
    const configuredResponses = state.apiResponses;
    if (configuredResponses) {
      const methodKey = `${req.method} ${url.pathname}`;
      const hasMethod = Object.prototype.hasOwnProperty.call(configuredResponses, methodKey);
      const hasLegacyGet = req.method === 'GET' && Object.prototype.hasOwnProperty.call(configuredResponses, url.pathname);
      if (hasMethod || hasLegacyGet) {
        let configured = configuredResponses[hasMethod ? methodKey : url.pathname];
        if (typeof configured === 'function') configured = configured(req, url);
        if (configured && configured.__fixtureResponse === true) {
          return json(res, configured.body, configured.status ?? 200);
        }
        return json(res, configured);
      }
    }
    if (bundleState && req.method === 'GET' && /^\/b\/(?:wsl|win)\/api\/health$/.test(url.pathname)) {
      const backend = url.pathname.includes('/b/win/') ? 'win' : 'wsl';
      const packaged = bundleState === 'packaged';
      return json(res, backend === 'win'
        ? {
            status: 'ok', version: packaged ? '0.0.993' : '0.0.992',
            webVersion: '0.0.992', webSourceSha: '0123456789abcdef0123456789abcdef01234567',
            backendID: 'win', panes: 0, uptimeSec: 60,
          }
        : {
            status: 'ok', version: '0.0.993', webVersion: '0.0.993',
            webSourceSha: '0123456789abcdef0123456789abcdef01234567',
            backendID: 'wsl', panes: 0, uptimeSec: 60,
          });
    }
    if (bundleState && req.method === 'GET' && /^\/b\/(?:wsl|win)\/api\/update\/status$/.test(url.pathname)) {
      return json(res, { current: url.pathname.includes('/b/wsl/') ? '0.0.993' : '0.0.992', latest: '0.0.993', available: false, applying: false });
    }
    if (req.method === 'GET' && ['/api/ui/events', '/api/events'].includes(url.pathname)) {
      res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache', connection: 'keep-alive' });
      res.write(': visual verification\n\n');
      if (state.sseEvents?.length) {
        setTimeout(() => {
          for (const event of state.sseEvents) {
            res.write(`data: ${JSON.stringify(event)}\n\n`);
          }
        }, 500);
      }
      return;
    }
    if (req.method === 'GET' && url.pathname === '/api/panes') return json(res, state.panes ?? []);
    if (req.method === 'GET' && url.pathname === '/api/orchestrator-hosts') {
      return json(res, state.orchestratorHosts ?? orchestratorHostsAllAvailable());
    }
    if (req.method === 'GET' && url.pathname === '/api/workspaces') return json(res, state.workspaces ?? []);
    if (req.method === 'GET' && url.pathname === '/api/ports') return json(res, []);
    if (req.method === 'GET' && url.pathname === '/api/schedules') return json(res, state.schedules ?? []);
    if (req.method === 'GET' && url.pathname === '/api/file') {
      const fixture = state.fileFixtures?.[url.searchParams.get('path')];
      if (fixture) return json(res, fixture);
      return json(res, { error: 'file fixture not found' }, 404);
    }
    if (req.method === 'GET' && Object.prototype.hasOwnProperty.call(workflowObjectResponses, url.pathname)) {
      return json(res, workflowObjectResponses[url.pathname]);
    }
    if (req.method === 'GET' && url.pathname === '/api/workflow/campaigns') return json(res, []);
    if (holdState && req.method === 'GET' && url.pathname === '/api/update/settings') {
      return json(res, { autoApply: 'off' });
    }
    if (holdState && req.method === 'GET' && url.pathname === '/api/update/status') {
      const workflow = holdState.cause === 'workflow' || holdState.cause === 'backoff';
      const backoff = holdState.cause === 'backoff';
      return json(res, {
        current: '0.0.990',
        latest: '0.0.991',
        available: true,
        // Reproduce the v0.0.1048 legacy conflict exactly: its internal
        // pre-admission reservation leaked applying=true while the level state
        // simultaneously reported a workflow drain.
        applying: workflow,
        lastCheckedMs: Date.now() - 12_000,
        notes: '- Typed update holds\n- Stable queued update surfaces',
        workflowDeferred: workflow,
        workflowDeferReason: workflow
          ? 'waiting for 2 in-flight workflow invocations'
          : undefined,
        workflowDeferSinceMs: workflow ? holdState.deadlineMs - 10 * 60_000 : undefined,
        workflowDeferDeadlineMs: workflow ? holdState.deadlineMs : undefined,
        workflowInflight: workflow ? 2 : undefined,
        workflowDeferAttempts: backoff ? 2 : undefined,
        workflowNextRetryMs: backoff ? holdState.nextRetryMs : undefined,
        workflowDeferState: workflow ? (backoff ? 'backoff' : 'draining') : undefined,
      });
    }
    if (holdState && req.method === 'POST' && url.pathname === '/api/update/apply') {
      if (holdState.cause === 'focus') {
        return json(res, {
          busy: true,
          cause: 'focus',
          reason: `a Focus pane is mid-turn (pane ${holdState.paneId})`,
          paneId: holdState.paneId,
        }, 409);
      }
      return json(res, {
        busy: true,
        cause: 'workflow',
        reason: 'waiting for 2 in-flight workflow invocations',
        workflowInflight: 2,
        workflowDeferDeadlineMs: holdState.deadlineMs,
      }, 409);
    }
    if (req.method === 'GET' && url.pathname === '/api/health') return json(res, {
      status: 'ok',
      version: healthVersion,
      system: { level: 'ok', sampledAt: new Date().toISOString() },
      releaseReadiness: {
        state: 'ok',
        lastReleaseVersion: '0.0.989',
        lastReleaseAnchor: 'bef8f4074e78d4180edf275160669cf88d96d1d9',
        anchorSource: 'published-manifest',
        anchorProvenance: 'exact',
        unreleasedTotal: 6,
        functionalCount: 4,
        docsOnlyCount: 2,
        functionalPaths: ['daemon', 'web'],
        suggestedNextVersion: healthVersion,
        oldestUnreleasedAt: '2026-08-01T00:00:00Z',
        functionalCommitCount: 3,
        oldestFunctionalUnreleasedAt: '2026-08-09T22:00:00Z',
        runningVersion: healthVersion,
        dirty: false,
        computedAt: '2026-08-10T00:00:00Z',
      },
    });
    if (req.method === 'GET' && url.pathname === '/api/system/status') return json(res, systemStatusFixture({ hostDisk: !!state.hostDisk }));
    if (req.method === 'GET' && url.pathname === '/api/system/host-storage') return json(res, state.hostDisk ? hostStorageFixture() : { available: false, state: 'not_wsl' });
    if (req.method === 'POST' && url.pathname === '/api/system/host-storage/refresh') return json(res, state.hostDisk ? hostStorageFixture() : { available: false, state: 'not_wsl' });
    if (req.method === 'GET' && url.pathname === '/api/cc-usage/summary') return json(res, {
      hero: { turns: 72, cost_usd: 12.34 }, by_model: [{ model: 'gpt-5.6', turns: 72 }],
    });
    if (req.method === 'GET' && url.pathname === '/api/workflow/release-check') return json(res, {
      allowed: false,
      cut: {
        id: 'rc_f25d8cc5', version: '0.0.990', repoKey: '/home/user/project/.git',
        sourceSha: 'ba629081b01fc92beabaa848c69ec5186aeb6a7e', declaredBy: 'api', status: 'open',
        createdAt: '2026-08-10T07:49:00Z', updatedAt: '2026-08-10T08:19:00Z',
      },
      blockers: [{
        taskId: 'wft_354752a3', title: 'Reconcile Muse review prepass with adaptive timeouts',
        status: 'awaiting_approval', queueState: 'awaiting_approval', tier: 'critical', why: 'in-scope task awaits human approval',
      }],
      waived: [{ taskId: 'wft_73d91c20', reason: 'Unrelated documentation experiment continues in parallel', by: 'tasks-ui', at: '2026-08-10T08:10:00Z' }],
      included: [{ taskId: 'wft_08c71e3f', title: 'Release prerequisite verification', status: 'completed', landOutcome: 'merged', ancestor: 'verified' }],
      warnings: [],
    });
    if (req.method === 'GET' && url.pathname === '/api/workflow/release-train') return json(res, {
      policy: {
        enabled: false,
        minFunctionalLandings: 6,
        maxUnreleasedAgeHours: 12,
        quietRequired: true,
        notifyVetoMinutes: 10,
      },
      state: {
        phase: 'disabled',
        train: null,
        parked: null,
        restartPending: null,
      },
      preview: null,
      events: [],
    });
    // The Autonomy overview is an OBJECT, not a list — it must be answered
    // before the array catch-all below, which would otherwise hand the section
    // a `[]` whose `.policies` is undefined. Representative rather than empty:
    // a screenshot of an empty inventory reviews nothing.
    if (req.method === 'GET' && url.pathname === '/api/autonomy') return json(res, autonomyFixture());

    if (req.method === 'GET' && Object.prototype.hasOwnProperty.call(ambientAppObjectResponses, url.pathname)) {
      return json(res, ambientAppObjectResponses[url.pathname]);
    }
    if (req.method === 'GET' && ambientAppListResponses.has(url.pathname)) return json(res, []);
    if (req.method === 'GET' && url.pathname.startsWith('/api/')) {
      return json(res, {
        error: `unhandled API fixture: ${url.pathname}`,
        path: url.pathname,
      }, 501);
    }
    if (req.method !== 'GET' && req.method !== 'HEAD') {
      res.writeHead(405).end();
      return;
    }

    let requestPath = decodeURIComponent(url.pathname);
    if (requestPath === '/') requestPath = '/index.html';
    let file = path.resolve(dist, `.${requestPath}`);
    const root = path.resolve(dist) + path.sep;
    if (!file.startsWith(root)) {
      res.writeHead(403).end();
      return;
    }
    if (!fs.existsSync(file) || !fs.statSync(file).isFile()) file = path.join(dist, 'index.html');
    fs.readFile(file, (error, body) => {
      if (error) {
        res.writeHead(404).end();
        return;
      }
      res.writeHead(200, { 'content-type': mime[path.extname(file)] || 'application/octet-stream' });
      if (req.method === 'HEAD') res.end();
      else res.end(body);
    });
  });
}

// Exit 79 tells the engine the module resolved but its browser build is
// absent — distinct from exit 78 (no module at all) so the approval warning
// can name the actual fix.
export function exitBrowserUnavailable(detail) {
  const lines = [detail];
  if (process.env.PLAYWRIGHT_BROWSERS_PATH) {
    lines.push(`PLAYWRIGHT_BROWSERS_PATH=${process.env.PLAYWRIGHT_BROWSERS_PATH}`);
  }
  lines.push("run 'npx playwright install chromium' in web/");
  // Flush diagnostics before the distinguished immediate exit. A piped
  // process.stderr is asynchronous on some Node versions, so writeSync keeps
  // the approval warning actionable instead of losing the final line.
  fs.writeSync(process.stderr.fd, `${lines.join('; ')}\n`);
  process.exit(79);
}

// Small deterministic fixture server used by the behavioural Playwright lane.
// It intentionally requires an explicit non-Vite port so CI and local runs do
// not race a developer's :5173 process or silently choose a different port.
if (import.meta.url === pathToFileURL(process.argv[1] || '').href && process.argv.includes('--serve')) {
  const valueAfter = (flag) => {
    const index = process.argv.indexOf(flag);
    return index >= 0 ? process.argv[index + 1] : undefined;
  };
  const dist = path.resolve(valueAfter('--dist') || 'dist');
  const port = Number.parseInt(valueAfter('--port') || '', 10);
  if (!Number.isInteger(port) || port <= 0 || port === 5173) {
    fs.writeSync(process.stderr.fd, 'screens-lib --serve requires --port <non-5173 port>\n');
    process.exit(2);
  }
  const server = createServer(dist, { healthVersion: fixtureHealthVersion(dist) });
  server.once('error', (error) => {
    fs.writeSync(process.stderr.fd, `fixture server failed: ${error.message}\n`);
    process.exit(1);
  });
  server.listen(port, '127.0.0.1', () => {
    fs.writeSync(process.stdout.fd, `fixture server listening on http://127.0.0.1:${port}\n`);
  });
}

export function autonomyFixture(){return {agents:[],policies:[],controllers:[],builtins:[],globalPaused:false};}
export function buildServiceObjectResponses(){return {
 '/api/workflow/settings':{resources:{maxAgentRuns:4,memFloorMB:8192,diskFloorMB:8192}},
 '/api/workflow/runtimes/health':{runtimes:[],resolution:[]},
 '/api/workflow/runtimes':[], '/api/workflow/providers':[],
 '/api/workflow/release-ledger':[],
 '/api/workflow/release-train':{policy:{enabled:false},state:{phase:'disabled',train:null,parked:null,restartPending:null},preview:null,events:[]}
};}
export function buildServiceFixtures(){return buildServiceObjectResponses();}
export async function routeServiceFixtures(context,fixtures=buildServiceFixtures()){
 for(const [endpoint,value] of Object.entries(fixtures)){
  await context.route('**'+endpoint+'*',route=>route.fulfill({status:200,contentType:'application/json',body:JSON.stringify(value)}));
 }
}
