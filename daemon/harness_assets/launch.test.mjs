import { test } from 'node:test';
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { spawn, execFileSync } from 'node:child_process';
import { createServer } from 'node:http';
import { fileURLToPath } from 'node:url';
import { endpoint, selectedAccount, resolveAccountKey, loadedBrokerAccount, configurePiBroker, clearPiBroker, ensurePiCacheWarmingOff, cleanEnvironment, configureZcode, nodeSupportsDsh, nodeSupportsPi, deepseekPlan, piPlan, piGlmPlan, verifyPinnedRuntime, verifyDeepseekConfig, verifyPiConfig, readDshProvenance, readPiProvenance, writeDshProvenance, writePiProvenance, clientBuildVersion, managedNpmrc, writeManagedNpmrc, runtimeRoot, shortcutDataDir, shortcutDisposition, renderManagedShortcut, releaseBrokerCapability } from './launch.mjs';
import piApproval from './pi-approval.mjs';
import { PINS, DSH_LOCK_SHA256, DSH_SPAWN_HELPER } from './pins.mjs';
import { deepseekBaseURL, formatHarnessLock, prepareDshLock, dshRootPackageJSON, writeDshVerified, clearDshVerified, dshInstallVerified, ensureDshSpawnHelper } from './launch.mjs';
import { DSH_PRIVACY_ROWS, dshPatchEntries, rotateDshAnonymousId } from './deepseek-model.mjs';

test('keyless broker metadata launches with a local capability and Pi provider override', async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-broker-launch-'));
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, opts) => {
    assert.equal(String(url), 'http://127.0.0.1:8787/api/team-broker/capability');
    assert.deepEqual(JSON.parse(opts.body), { provider: 'pi-glm', label: 'shared', teamId: 'team-one' });
    return { ok: true, json: async () => ({ baseURL: 'http://127.0.0.1:8787/api/team-broker/pi-glm/shared', token: 'a'.repeat(64) }) };
  };
  try {
    await fs.mkdir(path.join(dir, 'harness-accounts'));
    await fs.mkdir(path.join(dir, 'harness-launchers'));
    await fs.writeFile(path.join(dir, 'harness-launchers', 'daemon-url'), 'http://127.0.0.1:8787\n');
    await fs.writeFile(path.join(dir, 'harness-accounts', 'pi-glm.json'), JSON.stringify({ version: 1, selected: 'team/shared', scope: 'pair', teamId: 'team-one', syncedAt: Date.now()/1000, personal: {}, team: { shared: { name: 'shared', apiKey: '', brokerOnly: true, billing: 'coding-plan', model: 'glm-5.3-flash' } } }));
    const account = await loadedBrokerAccount(dir, 'pi-glm', '');
    assert.equal(account.apiKey, 'a'.repeat(64));
    assert.equal(account.broker, true);
    const piHome = path.join(dir, 'pi-home');
    await configurePiBroker(account, piHome, 'pi-glm');
    const config = JSON.parse(await fs.readFile(path.join(piHome, 'models.json'), 'utf8'));
    assert.equal(config.providers.zai.baseUrl, `${account.baseURL}/v1`);
    assert.equal(config.providers.zai.apiKey, account.apiKey);
    await clearPiBroker(piHome);
    await assert.rejects(fs.readFile(path.join(piHome, 'models.json')));
    await fs.writeFile(path.join(piHome, 'models.json'), JSON.stringify({ providers: { myServer: { baseUrl: 'http://localhost:8080/v1' } } }));
    await assert.rejects(configurePiBroker(account, piHome, 'pi-glm'), /custom models/);
    await clearPiBroker(piHome);
    assert.ok((await fs.readFile(path.join(piHome, 'models.json'), 'utf8')).includes('myServer'));
  } finally {
    globalThis.fetch = originalFetch;
    await fs.rm(dir, { recursive: true, force: true });
  }
});

test('broker launch stays running while unchanged keyless metadata is polled', async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-broker-sustain-'));
  const root = path.join(dir, 'runtime');
  const runtime = path.join(root, 'zcode-terminal/node_modules/zcode-app-cli/vendor/zcode.cjs');
  const tui = path.join(path.dirname(runtime), 'node_modules/@zcode/tui/package.json');
  const server = createServer((_req, res) => {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ baseURL: `http://127.0.0.1:${server.address().port}/api/team-broker/glm/shared`, token: 'b'.repeat(64) }));
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  let child;
  try {
    await fs.mkdir(path.dirname(tui), { recursive: true });
    await fs.writeFile(runtime, 'setInterval(() => {}, 1000);\n');
    await fs.writeFile(tui, '{}');
    await fs.mkdir(path.join(dir, 'harness-accounts'));
    await fs.writeFile(path.join(dir, 'harness-accounts/glm.json'), JSON.stringify({ version: 1, selected: 'team/shared', scope: 'pair', teamId: 'team-one', syncedAt: Date.now()/1000, personal: {}, team: { shared: { name: 'shared', apiKey: '', brokerOnly: true, billing: 'api', model: 'glm-5.3-flash' } } }));
    child = spawn(process.execPath, [fileURLToPath(new URL('./launch.mjs', import.meta.url)), 'glm', '--data-dir', dir], {
      cwd: dir, env: { ...process.env, HOME: dir, SOLU_HARNESS_ROOT: root, SOLU_DAEMON_URL: `http://127.0.0.1:${server.address().port}` }, stdio: ['ignore', 'ignore', 'pipe'],
    });
    let stderr = '';
    child.stderr.on('data', chunk => { stderr += chunk; });
    await new Promise(resolve => setTimeout(resolve, 2200));
    assert.equal(child.exitCode, null, `broker launch exited during metadata poll: ${stderr}`);
  } finally {
    if (child && child.exitCode === null && child.signalCode === null) {
      child.kill('SIGTERM');
      await new Promise(resolve => child.once('exit', resolve));
    }
    server.close();
    await fs.rm(dir, { recursive: true, force: true });
  }
});

test('billing routes cannot silently spend the other GLM balance', () => {
  assert.equal(endpoint('glm', 'api'), 'https://api.z.ai/api/paas/v4');
  assert.equal(endpoint('glm', 'coding-plan'), 'https://api.z.ai/api/coding/paas/v4');
  assert.equal(endpoint('pi', 'api'), 'https://api.deepseek.com');
  assert.throws(() => endpoint('deepseek', 'coding-plan'));
  assert.throws(() => endpoint('pi', 'coding-plan'));
});
test('team selection requires a fresh assignment with no ambient or personal fallback', () => {
  const account = { name: 'main', apiKey: '', brokerOnly: true, billing: 'api', model: ' DEEPSEEK/DEEPSEEK-V4-PRO ' };
  const state = { version: 1, selected: 'team/main', scope: 'pair-a', teamId: 'team-a', syncedAt: 1000, team: { main: account }, personal: { main: { ...account, apiKey: 'personal-secret', brokerOnly: false } } };
  assert.equal(selectedAccount(state, 'deepseek', 1010000).source, 'team');
  assert.equal(selectedAccount(state, 'deepseek', 1010000).model, 'deepseek-flash');
  assert.equal(selectedAccount(state, 'deepseek', 1010000).teamId, 'team-a');
  assert.throws(() => selectedAccount(state, 'deepseek', 1121000));
  assert.throws(() => selectedAccount({ ...state, team: {} }, 'deepseek', 1010000));
  assert.throws(() => selectedAccount({ ...state, scope: '' }, 'deepseek', 1010000));
  assert.throws(() => selectedAccount({ ...state, team: { main: { ...account, apiKey: 'old-shared-key', brokerOnly: false } } }, 'deepseek', 1010000));
  assert.throws(() => selectedAccount({ ...state, team: { main: { ...account, model: 'deepseek-v4-pro-preview' } } }, 'deepseek', 1010000), /unsupported|malformed/i);
});
test('launch environment excludes competing identities and Node preload settings', () => {
  assert.deepEqual(cleanEnvironment({ PATH: '/bin', TERM: 'xterm', ANTHROPIC_API_KEY: 'a', OPENAI_API_KEY: 'b', DEEPSEEK_API_KEY: 'c', ZCODE_MODEL: 'other', PI_CODING_AGENT_DIR: '/home/u/.pi', PI_TELEMETRY: '1', NODE_OPTIONS: '--require injection' }), { PATH: '/bin', TERM: 'xterm' });
});
test('shortcutDataDir reads quoted and unquoted --data-dir values', () => {
  assert.equal(shortcutDataDir("exec node launch deepseek --data-dir '/home/u/.solu' \"$@\""), '/home/u/.solu');
  assert.equal(shortcutDataDir('exec node launch deepseek --data-dir "/home/u/.solu" "$@"'), '/home/u/.solu');
  assert.equal(shortcutDataDir('exec node launch deepseek --data-dir /home/u/.solu "$@"'), '/home/u/.solu');
  assert.equal(shortcutDataDir('exec node launch deepseek "$@"'), '');
  assert.equal(shortcutDataDir(undefined), '');
});
test('a side-by-side install cannot repoint another data dir\'s shortcut', () => {
  const prod = '/home/u/.solu';
  const side = '/home/u/.solu-collaboration-test';
  const managed = `#!/bin/sh\n# soluterminal-managed-harness\nexec '/usr/bin/node' '/home/u/.solu/harness-launchers/launch.mjs' 'deepseek' --data-dir '${prod}' "$@"\n`;
  const ownedBySide = `#!/bin/sh\n# soluterminal-managed-harness\nexec '/usr/bin/node' '${side}/harness-launchers/launch.mjs' 'deepseek' --data-dir '${side}' "$@"\n`;
  assert.equal(shortcutDisposition(null, side, prod), 'create');
  assert.equal(shortcutDisposition(managed, side, prod), 'skip');
  assert.equal(shortcutDisposition(managed, prod, prod), 'replace');
  assert.equal(shortcutDisposition(ownedBySide, side, prod), 'replace');
  assert.equal(shortcutDisposition('#!/bin/sh\necho mine\n', side, prod), 'retain');
});
test('every managed launch carries its data dir as the reaper ownership tag', () => {
  const deepseek = { name: 'main', apiKey: 'sk-test', billing: 'api', model: 'deepseek-flash' };
  const pi = { name: 'main', apiKey: 'sk-test', billing: 'api', model: 'deepseek-v4-flash' };
  const glm = { name: 'main', apiKey: 'zai-test', billing: 'coding-plan', model: 'glm-5.3-flash' };
  for (const plan of [deepseekPlan(deepseek, '/data/one'), piPlan(pi, '/data/one'), piGlmPlan(glm, '/data/one')]) {
    assert.equal(plan.env.SOLU_HARNESS_DATA_DIR, '/data/one');
  }
});

test('a dsh launch keeps the key in the environment, never in argv', () => {
  const account = { name: 'main', apiKey: 'key-sentinel', billing: 'api', model: 'deepseek-flash' };
  const plan = deepseekPlan(account, '/data', ['--help'], { PATH: '/bin', DEEPSEEK_API_KEY: 'ambient', NODE_OPTIONS: '--require inject' });
  assert.equal(plan.env.DEEPSEEK_API_KEY, 'key-sentinel');
  assert.equal(plan.env.DEEPSEEK_BASE_URL, 'https://api.deepseek.com/anthropic');
  assert.equal(plan.env.DSH_TELEMETRY_DISABLED, '1');
  assert.equal(plan.env.DSH_TELEMETRY_MODE, 'DISABLED');
  assert.equal(plan.env.DSH_HOME, '/data/harness-state/deepseek');
  assert.equal(plan.env.PATH, '/bin');
  assert.equal(plan.env.NODE_OPTIONS, undefined);
  assert.ok(plan.argv.includes('deepseek-flash'));
  assert.ok(plan.argv.includes('--help'));
  assert.ok(!plan.argv.some(part => part.includes('key-sentinel')));
  assert.ok(plan.runtime.endsWith('deepseek/node_modules/@deepseek-ai/dsh/lib/bin.js'));
});
test('a sealed account key unseals with the data-dir secret; re-sealing the same key still opens', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-seal-'));
  try {
    const dir = path.join(root, 'harness-accounts');
    await fs.mkdir(dir, { recursive: true });
    // Stores written before sealing existed keep working.
    assert.equal(await resolveAccountKey('sk-plaintext-sentinel', root), 'sk-plaintext-sentinel');
    const secret = crypto.randomBytes(32);
    await fs.writeFile(path.join(dir, '.secret-key'), secret.toString('hex'));
    const seal = (plain) => {
      const nonce = crypto.randomBytes(12);
      const cipher = crypto.createCipheriv('aes-256-gcm', secret, nonce);
      const ct = Buffer.concat([cipher.update(plain, 'utf8'), cipher.final()]);
      return `enc:v1:${Buffer.concat([nonce, ct, cipher.getAuthTag()]).toString('base64')}`;
    };
    const first = seal('sk-sealed-sentinel');
    const second = seal('sk-sealed-sentinel');
    assert.notEqual(first, second, 'a fresh nonce seals the same key to a different envelope');
    assert.equal(await resolveAccountKey(first, root), 'sk-sealed-sentinel');
    assert.equal(await resolveAccountKey(second, root), 'sk-sealed-sentinel');
    // Tampered envelopes and unknown versions fail closed with a recovery hint.
    await assert.rejects(resolveAccountKey(first.slice(0, -4) + 'AAAA', root), /Re-save/);
    await assert.rejects(resolveAccountKey('enc:v9:d3Jvbmd2ZXJzaW9u', root), /envelope/);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});
test('a sealed store without its local key fails closed', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-seal-missing-'));
  try {
    await assert.rejects(resolveAccountKey('enc:v1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==', root), /local key is missing/);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});
test('the supported-Node gate needs Node 24.9 and import.meta.main', () => {
  assert.equal(typeof nodeSupportsDsh(), 'boolean');
  // dsh 0.2.2 publishes engines node >=24.9.0.
  assert.equal(nodeSupportsDsh('24.9.0', true), true);
  assert.equal(nodeSupportsDsh('25.0.0', true), true);
  assert.equal(nodeSupportsDsh('24.8.1', true), false);
  assert.equal(nodeSupportsDsh('24.2.0', true), false);
  assert.equal(nodeSupportsDsh('22.19.0', true), false);
  assert.equal(nodeSupportsDsh('26.1.0', false), false);
});
test('a Pi launch keeps the key in the environment and pins the provider and model', () => {
  const account = { name: 'main', apiKey: 'pi-key-sentinel', billing: 'api', model: 'deepseek-v4-flash' };
  const plan = piPlan(account, '/data', ['--help'], { PATH: '/bin', DEEPSEEK_API_KEY: 'ambient', PI_CODING_AGENT_DIR: '/home/u/.pi', NODE_OPTIONS: '--require inject' });
  assert.equal(plan.env.DEEPSEEK_API_KEY, 'pi-key-sentinel');
  assert.equal(plan.env.PI_CODING_AGENT_DIR, '/data/harness-state/pi');
  assert.equal(plan.env.PI_CODING_AGENT_SESSION_DIR, '/data/harness-state/pi/sessions');
  assert.equal(plan.env.PI_OFFLINE, '1');
  assert.equal(plan.env.PI_SKIP_VERSION_CHECK, '1');
  assert.equal(plan.env.PI_TELEMETRY, '0');
  assert.equal(plan.env.PATH, '/bin');
  assert.equal(plan.env.NODE_OPTIONS, undefined);
  assert.deepEqual(plan.argv.slice(0, 4), [plan.runtime, '--provider', 'deepseek', '--model']);
  assert.equal(plan.argv[4], 'deepseek-flash');
  // Every managed Pi launch loads the SoluTerminal approval gate; Pi itself
  // ships no permission system.
  assert.deepEqual(plan.argv.slice(5, 7), ['--extension', plan.argv[6]]);
  assert.ok(plan.argv[6].endsWith('pi-approval.mjs'), plan.argv[6]);
  assert.ok(plan.argv.includes('--help'));
  assert.ok(!plan.argv.some(part => part.includes('pi-key-sentinel')));
  assert.ok(plan.runtime.endsWith('pi/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js'));
});
test('a Pi GLM launch binds the ZAI provider with an isolated home', () => {
  const account = { name: 'main', apiKey: 'zai-key-sentinel', billing: 'coding-plan', model: 'glm-5.3-flash' };
  const plan = piGlmPlan(account, '/data', ['--help'], { PATH: '/bin', ZAI_API_KEY: 'ambient', DEEPSEEK_API_KEY: 'ambient-deepseek', PI_CODING_AGENT_DIR: '/home/u/.pi', NODE_OPTIONS: '--require inject' });
  assert.equal(plan.env.ZAI_API_KEY, 'zai-key-sentinel');
  assert.equal(plan.env.PI_CODING_AGENT_DIR, '/data/harness-state/pi-glm');
  assert.equal(plan.env.PI_CODING_AGENT_SESSION_DIR, '/data/harness-state/pi-glm/sessions');
  assert.equal(plan.env.PI_OFFLINE, '1');
  assert.equal(plan.env.PI_SKIP_VERSION_CHECK, '1');
  assert.equal(plan.env.PI_TELEMETRY, '0');
  // Ambient keys of either row are stripped by cleanEnvironment; only the
  // managed ZAI key is set, and the managed DeepSeek home is not shared.
  assert.equal(plan.env.DEEPSEEK_API_KEY, undefined);
  assert.equal(plan.env.PATH, '/bin');
  assert.equal(plan.env.NODE_OPTIONS, undefined);
  assert.deepEqual(plan.argv.slice(0, 4), [plan.runtime, '--provider', 'zai', '--model']);
  assert.equal(plan.argv[4], 'glm-5.3-flash');
  assert.deepEqual(plan.argv.slice(5, 7), ['--extension', plan.argv[6]]);
  assert.ok(plan.argv[6].endsWith('pi-approval.mjs'), plan.argv[6]);
  assert.ok(plan.argv.includes('--help'));
  assert.ok(!plan.argv.some(part => part.includes('zai-key-sentinel')));
  assert.ok(plan.runtime.endsWith('pi/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js'));
});
test('a Pi launch resumes a specific session when the daemon asks for one', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-pi-resume-'));
  try {
    const sessions = path.join(root, 'harness-state', 'pi', 'sessions');
    await fs.mkdir(sessions, { recursive: true });
    await fs.writeFile(path.join(sessions, '2026-01-01T00-00-00-000Z_sess-abc.jsonl'), '{"type":"session","id":"sess-abc"}\n');
    const account = { name: 'main', apiKey: 'pi-key', billing: 'api', model: 'deepseek-v4-flash' };
    const plan = piPlan(account, root, [], { PI_CODING_AGENT_DIR: '/home/u/.pi', SOLU_HARNESS_RESUME_SESSION: 'sess-abc' });
    const at = plan.argv.indexOf('--session');
    assert.ok(at > 0, plan.argv.join(' '));
    assert.equal(plan.argv[at + 1], 'sess-abc');
    // A stale id (no session file) is dropped, not passed to Pi — `--session`
    // on a missing session makes Pi exit immediately.
    const stale = piPlan(account, root, [], { PI_CODING_AGENT_DIR: '/home/u/.pi', SOLU_HARNESS_RESUME_SESSION: 'missing-session' });
    assert.ok(!stale.argv.includes('--session'), stale.argv.join(' '));
    // Without the hint the argv stays exactly as before (no --session).
    const fresh = piPlan(account, root, [], { PI_CODING_AGENT_DIR: '/home/u/.pi' });
    assert.ok(!fresh.argv.includes('--session'), fresh.argv.join(' '));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('the managed Pi approval extension is loadable as an extension factory', () => {
  // Pi loads the file named on `--extension` and expects a default-exported
  // factory. A syntax/export regression would otherwise only surface in a live
  // managed pane.
  assert.equal(typeof piApproval, 'function');
});
test('the Pi Node gate matches the published engines floor', () => {
  const [major, minor] = String(process.versions.node).split('.').map(Number);
  assert.equal(nodeSupportsPi(), major > 22 || (major === 22 && minor >= 19));
});
test('a launch plan exposes the manifest used for pin verification', () => {
  const account = { name: 'main', apiKey: 'k', billing: 'api', model: 'deepseek-flash' };
  const dsh = deepseekPlan(account, '/data', []);
  assert.ok(dsh.packageJson.endsWith('deepseek/node_modules/@deepseek-ai/dsh/package.json'), dsh.packageJson);
  const pi = piPlan({ ...account, model: 'deepseek-v4-flash' }, '/data', []);
  assert.ok(pi.packageJson.endsWith('pi/node_modules/@earendil-works/pi-coding-agent/package.json'), pi.packageJson);
});
test('DeepSeek launch plans canonicalize safe legacy models and fail closed on unknown input', () => {
  const account = { name: 'main', apiKey: 'k', billing: 'api', model: '' };
  for (const raw of ['', 'deepseek-v4-pro', '  DEEPSEEK-V4-PRO  ', 'deepseek/deepseek-chat']) {
    const dsh = deepseekPlan({ ...account, model: raw }, '/data', []);
    assert.equal(dsh.argv[2], 'deepseek-flash', raw);
    assert.equal(dsh.env.SOLU_HARNESS_ACCOUNT_MODEL, 'deepseek-flash', raw);
    assert.equal(piPlan({ ...account, model: raw }, '/data', []).argv[4], 'deepseek-flash', raw);
  }
  const vision = 'deepseek-v4-flash-vision-exp';
  // DeepSeek Harness 0.2.2 dropped vision-exp too; its deepseek-flash takes images.
  assert.equal(deepseekPlan({ ...account, model: vision }, '/data', []).argv[2], 'deepseek-flash');
  // Pi 1.5's DeepSeek catalog has no vision-exp; its deepseek-flash takes images.
  assert.equal(piPlan({ ...account, model: vision }, '/data', []).argv[4], 'deepseek-flash');
  for (const raw of ['deepseek-v4-pro-preview', 'deepseek/future-model', 'deepseek//deepseek-v4-pro']) {
    assert.throws(() => deepseekPlan({ ...account, model: raw }, '/data', []), /unsupported|malformed/i, raw);
    assert.throws(() => piPlan({ ...account, model: raw }, '/data', []), /unsupported|malformed/i, raw);
  }
});
test('verifyPinnedRuntime compares the installed manifest to the pin', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-pin-verify-'));
  try {
    const pkg = path.join(root, 'package.json');
    await fs.writeFile(pkg, JSON.stringify({ name: '@deepseek-ai/dsh', version: '0.1.4' }));
    await assert.rejects(verifyPinnedRuntime(pkg, '0.1.5-rc.1', 'DeepSeek Harness'), /0\.1\.4.*0\.1\.5-rc\.1|Reinstall/);
    await fs.writeFile(pkg, JSON.stringify({ name: '@deepseek-ai/dsh', version: '0.1.5-rc.1' }));
    await verifyPinnedRuntime(pkg, '0.1.5-rc.1', 'DeepSeek Harness');
    // A missing or unversioned manifest is tolerated so fakes and pre-provenance
    // installs still run; published packages always ship a version.
    await verifyPinnedRuntime(path.join(root, 'missing.json'), '0.1.5-rc.1', 'DeepSeek Harness');
    await fs.writeFile(pkg, JSON.stringify({ name: '@deepseek-ai/dsh' }));
    await verifyPinnedRuntime(pkg, '0.1.5-rc.1', 'DeepSeek Harness');
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});
test('launch refuses a managed runtime that drifted from the pin', async t => {
  // launch() gates on the Node capability before it reaches the pin check.
  if (!nodeSupportsDsh()) return t.skip('this host cannot run dsh (Node < 24.9)');
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-launch-drift-'));
  const harnessRoot = path.join(root, 'harnesses');
  const dataDir = path.join(root, 'data');
  try {
    const pkgRoot = path.join(harnessRoot, 'deepseek/node_modules/@deepseek-ai/dsh');
    const runtime = path.join(pkgRoot, 'lib/bin.js');
    await fs.mkdir(path.dirname(runtime), { recursive: true });
    await fs.writeFile(path.join(pkgRoot, 'package.json'), JSON.stringify({ name: '@deepseek-ai/dsh', version: '0.1.4' }));
    await fs.writeFile(runtime, 'process.exit(0);');
    await fs.mkdir(path.join(dataDir, 'harness-accounts'), { recursive: true });
    await fs.writeFile(path.join(dataDir, 'harness-accounts', 'deepseek.json'), JSON.stringify({ version: 1, selected: 'personal/main', personal: { main: { name: 'main', apiKey: 'k', billing: 'api', model: 'deepseek-flash' } }, team: {} }));
    const child = spawn(process.execPath, [fileURLToPath(new URL('./launch.mjs', import.meta.url)), 'deepseek', '--data-dir', dataDir], { env: { ...process.env, SOLU_HARNESS_ROOT: harnessRoot }, stdio: ['pipe', 'pipe', 'pipe'] });
    let errors = '';
    child.stderr.on('data', data => { errors += data; });
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); });
    assert.equal(code, 1, `expected the launch to refuse a drifted runtime: ${errors}`);
    assert.ok(errors.includes(`0.1.4 is installed but ${PINS.deepseek} is required`), errors);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});
test('the pinned Pi runtime boots offline through the managed home', async t => {
  const runtime = path.join(runtimeRoot, 'pi/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js');
  if (!(await fs.stat(runtime).then(() => true, () => false))) return t.skip('Pi runtime is not installed on this host');
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-pi-home-'));
  try {
    assert.match(await verifyPiConfig(runtime, home), /^\d+\.\d+\.\d+/);
  } finally { await fs.rm(home, { recursive: true, force: true }); }
});
test('the pinned runtime composes its ACP profile offline', async t => {
  const runtime = path.join(os.homedir(), '.local/share/soluterminal/harnesses/deepseek/node_modules/@deepseek-ai/dsh/lib/bin.js');
  if (!(await fs.stat(runtime).catch(() => null))) return t.skip('dsh is not installed on this host');
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-dsh-config-'));
  try {
    const dump = await verifyDeepseekConfig(runtime, dir);
    assert.ok(dump.includes('@deepseek-ai/dsh'));
  } finally { await fs.rm(dir, { recursive: true, force: true }); }
});
test('a rotated account stops the managed process group', async t => {
  // The managed launcher refuses on a Node without import.meta.main, which is
  // the correct behavior — there is no process group to observe on such a host.
  if (!nodeSupportsDsh()) return t.skip('this host cannot run dsh (Node < 24.9)');
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-launch-life-'));
  const harnessRoot = path.join(root, 'harnesses');
  const dataDir = path.join(root, 'data');
  try {
    // A fake installed runtime that only waits. `.js` with no package.json is
    // CJS, so it uses require.
    const runtime = path.join(harnessRoot, 'deepseek/node_modules/@deepseek-ai/dsh/lib/bin.js');
    const started = path.join(root, 'started');
    await fs.mkdir(path.dirname(runtime), { recursive: true });
    await fs.writeFile(runtime, `const fs=require('node:fs');fs.writeFileSync(${JSON.stringify(started)},'1');process.stdin.resume();setInterval(()=>{},1000);`);
    await writeDshVerified(path.join(harnessRoot, 'deepseek'));
    await fs.mkdir(path.join(dataDir, 'harness-accounts'), { recursive: true });
    const accountFile = path.join(dataDir, 'harness-accounts', 'deepseek.json');
    const state = { version: 1, selected: 'personal/main', personal: { main: { name: 'main', apiKey: 'first-key', billing: 'api', model: 'deepseek-flash' } }, team: {} };
    await fs.writeFile(accountFile, JSON.stringify(state));
    const child = spawn(process.execPath, [fileURLToPath(new URL('./launch.mjs', import.meta.url)), 'deepseek', '--data-dir', dataDir], { env: { ...process.env, SOLU_HARNESS_ROOT: harnessRoot }, stdio: ['pipe', 'pipe', 'pipe'] });
    let errors = '';
    child.stderr.on('data', data => { errors += data; });
    const deadline = Date.now() + 8000;
    while (!(await fs.stat(started).catch(() => null)) && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 100));
    assert.ok(await fs.stat(started).catch(() => null), `harness never started: ${errors}`);
    state.personal.main.apiKey = 'second-key';
    await fs.writeFile(accountFile, JSON.stringify(state));
    const timer = setTimeout(() => child.kill('SIGKILL'), 12000);
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); }); clearTimeout(timer);
    assert.notEqual(code, null, `launcher was killed by the test timeout: ${errors}`);
    assert.ok(errors.includes('API account changed'), errors);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});
test('ZCode writes its required private live slot, preserves settings, and rejects project overrides', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'harness-config-test-'));
  try {
    const configFile = path.join(root, 'user/config.json');
    await fs.mkdir(path.dirname(configFile));
    await fs.writeFile(configFile, JSON.stringify({ ui: { locale: 'en-US' }, provider: { other: { kind: 'anthropic' } } }));
    const account = { name: 'main', apiKey: 'secret-sentinel', billing: 'api', model: 'glm-5.3' };
    await configureZcode(account, root, configFile);
    const raw = await fs.readFile(configFile, 'utf8');
    assert.equal((await fs.stat(configFile)).mode & 0o777, 0o600);
    const config = JSON.parse(raw);
    assert.equal(config.ui.locale, 'en-US');
    assert.equal(config.provider.other.kind, 'anthropic');
    assert.equal(config.model.main, 'zai/glm-5.3');
    assert.equal(config.provider.zai.options.apiKey, 'secret-sentinel');
    assert.equal(config.soluterminalManagedGLM.name, 'main');
    await fs.writeFile(path.join(root, 'zcode.json'), '{"model":"other/model"}');
    await assert.rejects(configureZcode(account, root, configFile), /overrides/);
    assert.equal(await fs.readFile(configFile, 'utf8'), raw);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});
test('provenance requires a registry-resolved sha512 entry for the pin', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-dsh-provenance-'));
  try {
    const prefix = path.join(root, 'deepseek');
    await fs.mkdir(prefix, { recursive: true });
    const lock = path.join(prefix, 'package-lock.json');
    const entry = over => ({
      version: '0.1.5-rc.1',
      resolved: 'https://registry.npmjs.org/@deepseek-ai/dsh/-/dsh-0.1.5-rc.1.tgz',
      integrity: 'sha512-abc',
      ...over,
    });
    const write = async value => fs.writeFile(lock, JSON.stringify({ packages: { 'node_modules/@deepseek-ai/dsh': value } }));
    await write(entry());
    assert.equal((await readDshProvenance(prefix, '0.1.5-rc.1')).integrity, 'sha512-abc');
    await write(entry({ version: '0.1.5-rc.2' }));
    assert.equal(await readDshProvenance(prefix, '0.1.5-rc.1'), null, 'wrong version accepted');
    await write(entry({ integrity: 'md5-nope' }));
    assert.equal(await readDshProvenance(prefix, '0.1.5-rc.1'), null, 'non-sha512 accepted');
    await write(entry({ resolved: 'https://evil.example/dsh.tgz' }));
    assert.equal(await readDshProvenance(prefix, '0.1.5-rc.1'), null, 'non-npm origin accepted');
    await fs.rm(lock);
    assert.equal(await readDshProvenance(prefix, '0.1.5-rc.1'), null, 'missing lockfile accepted');
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});
test('Pi provenance resolves the pinned package from the official registry', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-pi-prov-'));
  try {
    const prefix = path.join(root, 'pi');
    await fs.mkdir(prefix, { recursive: true });
    const lock = path.join(prefix, 'package-lock.json');
    const entry = over => ({
      version: '0.85.1',
      resolved: 'https://registry.npmjs.org/@earendil-works/pi-coding-agent/-/pi-coding-agent-0.85.1.tgz',
      integrity: 'sha512-abc',
      ...over,
    });
    const write = async value => fs.writeFile(lock, JSON.stringify({ packages: { 'node_modules/@earendil-works/pi-coding-agent': value } }));
    await write(entry());
    const proven = await readPiProvenance(prefix, '0.85.1');
    assert.equal(proven.package, '@earendil-works/pi-coding-agent');
    assert.equal(new URL(proven.resolved).host, 'registry.npmjs.org');
    await write(entry({ version: '0.86.0' }));
    assert.equal(await readPiProvenance(prefix, '0.85.1'), null, 'wrong version accepted');
    await write(entry({ resolved: 'https://evil.example/pi.tgz' }));
    assert.equal(await readPiProvenance(prefix, '0.85.1'), null, 'non-npm origin accepted');
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});
test('provenance is recorded privately beside the DSH home', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-dsh-provwrite-'));
  try {
    const file = await writeDshProvenance(root, { version: '0.1.5-rc.1', resolved: 'https://registry.npmjs.org/x.tgz', integrity: 'sha512-abc' });
    assert.equal((await fs.stat(file)).mode & 0o777, 0o600);
    const record = JSON.parse(await fs.readFile(file, 'utf8'));
    assert.equal(record.version, '0.1.5-rc.1');
    assert.match(record.node, /^v\d+/);
    assert.ok(record.verifiedAt);
    const piFile = await writePiProvenance(root, { version: '0.85.1', resolved: 'https://registry.npmjs.org/x.tgz', integrity: 'sha512-abc' });
    assert.equal((await fs.stat(piFile)).mode & 0o777, 0o600);
    assert.match(piFile, /harness-state[/\\]pi[/\\]install-provenance\.json$/);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});
test('the pinned runtime answers a one-shot headless job', async t => {
  const runtime = path.join(os.homedir(), '.local/share/soluterminal/harnesses/deepseek/node_modules/@deepseek-ai/dsh/lib/bin.js');
  if (!(await fs.stat(runtime).catch(() => null))) return t.skip('dsh is not installed on this host');
  const script = fileURLToPath(new URL('../../scripts/smoke-deepseek-headless.mjs', import.meta.url));
  // The smoke script is a maintainer tool, not part of every checkout.
  if (!(await fs.stat(script).catch(() => null))) return t.skip('the smoke script is not in this checkout');
  const child = spawn(process.execPath, [script], { stdio: ['ignore', 'pipe', 'pipe'] });
  let err = '';
  child.stderr.on('data', data => { err += data; });
  const timer = setTimeout(() => child.kill('SIGKILL'), 90000);
  const code = await new Promise(resolve => child.on('exit', resolve));
  clearTimeout(timer);
  assert.equal(code, 0, err);
});

test('the client build stamp prefers the extracted VERSION, then the repo Tauri version', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-build-stamp-'));
  try {
    // Extracted launcher: VERSION written beside it wins.
    const extracted = path.join(root, 'harness-launchers');
    await fs.mkdir(extracted, { recursive: true });
    await fs.writeFile(path.join(extracted, 'VERSION'), '9.9.9\n');
    assert.equal(await clientBuildVersion(path.join(extracted, 'launch.mjs')), '9.9.9');

    // Repo checkout: no VERSION, so read desktop/src-tauri/tauri.conf.json.
    const repo = path.join(root, 'repo');
    const assets = path.join(repo, 'daemon', 'harness_assets');
    await fs.mkdir(assets, { recursive: true });
    await fs.mkdir(path.join(repo, 'desktop', 'src-tauri'), { recursive: true });
    await fs.writeFile(path.join(repo, 'desktop', 'src-tauri', 'tauri.conf.json'), JSON.stringify({ version: '1.2.3' }));
    assert.equal(await clientBuildVersion(path.join(assets, 'launch.mjs')), '1.2.3');

    // Neither: the client falls back to dev rather than failing.
    assert.equal(await clientBuildVersion(path.join(root, 'nowhere', 'launch.mjs')), 'dev');
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('the launch plan carries the build stamp into the child environment', () => {
  const account = { name: 'main', apiKey: 'k', billing: 'api', model: 'deepseek-flash' };
  const plan = deepseekPlan(account, '/data', [], { PATH: '/bin', SOLU_CLIENT_BUILD: '1.2.3' });
  assert.equal(plan.env.SOLU_CLIENT_BUILD, '1.2.3');
  assert.equal(plan.env.PATH, '/bin');
});

test('the managed npm config pins exact versions against the official registry', async () => {
  const cfg = managedNpmrc();
  assert.match(cfg, /save-exact=true/);
  assert.match(cfg, /registry=https:\/\/registry\.npmjs\.org\//);
  // min-release-age would refuse a pin published in the last N days, breaking a
  // same-day bump; exact pins plus verified provenance are the guard instead.
  assert.ok(!/min-release-age/.test(cfg), 'min-release-age must stay unset');
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-npmrc-'));
  try {
    const file = await writeManagedNpmrc(root);
    assert.equal((await fs.stat(file)).mode & 0o777, 0o600);
    assert.equal(await fs.readFile(file, 'utf8'), cfg);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('the managed shortcut execs the shared resolver and keeps its data dir parseable', () => {
  const script = renderManagedShortcut({
    resolver: '/launchers/harness-shortcut.sh',
    node: "/opt/it's/node",
    launcher: '/launchers/launch.mjs',
    provider: 'deepseek',
    dataDir: '/home/u/.solu',
  });
  assert.match(script, /^#!\/bin\/sh\n/);
  assert.ok(script.includes('# soluterminal-managed-harness'));
  assert.ok(script.includes("exec /bin/sh '/launchers/harness-shortcut.sh'"), script);
  assert.ok(script.includes("'/opt/it'\\''s/node'"), script);
  assert.equal(shortcutDataDir(script), '/home/u/.solu');
});

test('the shared shortcut resolver re-resolves Node when the pinned interpreter is gone', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-shortcut-'));
  try {
    const resolver = fileURLToPath(new URL('./harness-shortcut.sh', import.meta.url));
    const launcher = path.join(root, 'launch.mjs');
    const out = path.join(root, 'out');
    const tools = path.join(root, 'tools');
    await fs.mkdir(tools);
    // Only the tools the resolver itself uses, so PATH holds no node and no bash.
    for (const tool of ['ls', 'awk', 'tail']) {
      const src = execFileSync('/bin/sh', ['-c', `command -v ${tool}`], { encoding: 'utf8' }).trim();
      await fs.symlink(src, path.join(tools, tool));
    }
    const run = (pinned, env) => new Promise(resolve => {
      const child = spawn('/bin/sh', [resolver, pinned, launcher, 'deepseek', '--data-dir', '/data', 'prompt'], { env, stdio: ['ignore', 'ignore', 'pipe'] });
      let stderr = '';
      child.stderr.on('data', data => { stderr += data; });
      child.on('exit', code => resolve({ code, stderr }));
    });

    // 1. Pinned interpreter gone, a Node is on PATH: forward the arguments.
    const bin = path.join(root, 'bin');
    await fs.mkdir(bin);
    await fs.writeFile(path.join(bin, 'node'), `#!/bin/sh\nprintf '%s\\n' "$@" > "${out}"\n`, { mode: 0o755 });
    let result = await run(path.join(root, 'missing'), { PATH: `${bin}:${tools}`, HOME: root });
    assert.equal(result.code, 0, result.stderr);
    assert.equal(await fs.readFile(out, 'utf8'), `${launcher}\ndeepseek\n--data-dir\n/data\nprompt\n`);

    // 2. Newest nvm install wins over an older one, with no login shell.
    const home = path.join(root, 'home');
    for (const version of ['v9.9.9', 'v10.0.0']) {
      const dir = path.join(home, '.nvm', 'versions', 'node', version, 'bin');
      await fs.mkdir(dir, { recursive: true });
      await fs.writeFile(path.join(dir, 'node'), `#!/bin/sh\nprintf 'NVM ${version}\\n' > "${out}"\n`, { mode: 0o755 });
    }
    result = await run(path.join(root, 'missing'), { PATH: tools, HOME: home });
    assert.equal(result.code, 0, result.stderr);
    assert.equal(await fs.readFile(out, 'utf8'), 'NVM v10.0.0\n');

    // 3. A pinned interpreter that exists but cannot run the provider (an old
    //    distro Node) is skipped for the newer nvm install.
    const stale = path.join(root, 'stale-node');
    await fs.writeFile(stale, `#!/bin/sh\ncase "$1" in -e) exit 1 ;; esac\nprintf 'STALE\\n' > "${out}"\n`, { mode: 0o755 });
    result = await run(stale, { PATH: tools, HOME: home });
    assert.equal(result.code, 0, result.stderr);
    assert.equal(await fs.readFile(out, 'utf8'), 'NVM v10.0.0\n');

    // 4. Only a below-floor interpreter: name the version, don't say "not found".
    result = await run(stale, { PATH: tools, HOME: path.join(root, 'empty-home') });
    assert.equal(result.code, 127);
    assert.match(result.stderr, /below the supported version/);
    assert.match(result.stderr, /pinned interpreter/);

    // 5. Nothing usable: an actionable message, not a bare shell "not found".
    result = await run(path.join(root, 'missing'), { PATH: tools, HOME: path.join(root, 'empty-home') });
    assert.equal(result.code, 127);
    assert.match(result.stderr, /Node\.js not found/);
    assert.match(result.stderr, /pinned interpreter/);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('releaseBrokerCapability hands the capability back to the local daemon', async () => {
  const calls = [];
  const fetchImpl = async (url, init) => { calls.push({ url: String(url), init }); return { body: null }; };
  const token = 'a'.repeat(64);
  await releaseBrokerCapability({ apiKey: token, baseURL: 'http://127.0.0.1:8787/api/team-broker/pi/shared' }, fetchImpl);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, 'http://127.0.0.1:8787/api/team-broker/capability/release');
  assert.equal(calls[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(calls[0].init.body), { token });
  // A personal key is never sent, and a gone daemon is not an error.
  await releaseBrokerCapability({ apiKey: 'sk-personal', baseURL: 'http://127.0.0.1:8787/x' }, fetchImpl);
  assert.equal(calls.length, 1);
  await releaseBrokerCapability({ apiKey: token, baseURL: 'http://127.0.0.1:8787/x' }, async () => { throw new Error('offline'); });
});

test('the managed Pi home pins prompt-cache warming off and keeps other settings', async () => {
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-pi-settings-'));
  const file = path.join(home, 'settings.json');
  try {
    await ensurePiCacheWarmingOff(home);
    assert.deepEqual(JSON.parse(await fs.readFile(file, 'utf8')), { cacheWarming: 'off' });
    if (process.platform !== 'win32') assert.equal((await fs.stat(file)).mode & 0o777, 0o600);
    await fs.writeFile(file, JSON.stringify({ cacheWarming: 'idle', theme: 'dark' }));
    await ensurePiCacheWarmingOff(home);
    assert.deepEqual(JSON.parse(await fs.readFile(file, 'utf8')), { cacheWarming: 'off', theme: 'dark' });
    const before = (await fs.stat(file)).mtimeMs;
    await ensurePiCacheWarmingOff(home); // already off: no rewrite
    assert.equal((await fs.stat(file)).mtimeMs, before);
    await fs.writeFile(file, '{not json');
    await assert.rejects(ensurePiCacheWarmingOff(home), /unreadable/);
    await fs.writeFile(file, '[]');
    await assert.rejects(ensurePiCacheWarmingOff(home), /unreadable/);
  } finally { await fs.rm(home, { recursive: true, force: true }); }
});

test('the Pi lockfile is hash-pinned: a tampered integrity line is refused before npm runs', async () => {
  const { preparePiLock, piRootPackageJSON } = await import('./launch.mjs');
  const { PI_LOCK_SHA256 } = await import('./pins.mjs');
  const lockBytes = await fs.readFile(new URL('./pi-lock.json', import.meta.url));
  assert.equal(crypto.createHash('sha256').update(lockBytes).digest('hex'), PI_LOCK_SHA256, 'pi-lock.json must match PI_LOCK_SHA256');
  const prefix = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-pi-lock-'));
  try {
    const lock = await preparePiLock(prefix, lockBytes);
    assert.equal(lock.packages['node_modules/@earendil-works/pi-coding-agent'].version, PINS.pi);
    for (const [key, entry] of Object.entries(lock.packages)) {
      if (!key) continue;
      assert.match(entry.integrity || '', /^sha512-/, `${key} has no sha512 integrity`);
      assert.match(entry.resolved || '', /^https:\/\/registry\.npmjs\.org\//, `${key} not from the npm registry`);
    }
    assert.equal(await fs.readFile(path.join(prefix, 'package.json'), 'utf8'), piRootPackageJSON());
    assert.deepEqual(await fs.readFile(path.join(prefix, 'package-lock.json')), lockBytes);
    assert.equal(lock.packages[''].dependencies['@earendil-works/pi-coding-agent'], PINS.pi, 'root dependency must match the generated package.json');

    // Change one character of one integrity hash.
    const text = String(lockBytes);
    const at = text.indexOf('"integrity": "sha512-') + '"integrity": "sha512-'.length;
    const tampered = Buffer.from(text.slice(0, at) + (text[at] === 'A' ? 'B' : 'A') + text.slice(at + 1));
    const other = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-pi-lock-'));
    try {
      await assert.rejects(preparePiLock(other, tampered), /does not match the pinned hash/);
      await assert.rejects(fs.stat(path.join(other, 'package-lock.json')), 'a refused lockfile must not be written');
    } finally { await fs.rm(other, { recursive: true, force: true }); }
    await assert.rejects(preparePiLock(prefix, lockBytes, { pin: '9.9.9' }), /does not resolve Pi 9\.9\.9/);
  } finally { await fs.rm(prefix, { recursive: true, force: true }); }
});

test('piTreeMatchesLock accepts only the locked tree', async () => {
  const { piTreeMatchesLock } = await import('./launch.mjs');
  const prefix = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-pi-tree-'));
  const lock = { packages: {
    '': { name: 'root' },
    'node_modules/a': { version: '1.0.0', integrity: 'sha512-a' },
    'node_modules/b': { version: '2.0.0', integrity: 'sha512-b', optional: true },
  } };
  const writeHidden = async packages => {
    await fs.mkdir(path.join(prefix, 'node_modules'), { recursive: true });
    await fs.writeFile(path.join(prefix, 'node_modules', '.package-lock.json'), JSON.stringify({ packages }));
  };
  try {
    assert.equal(await piTreeMatchesLock(prefix, lock), false, 'nothing installed');
    await writeHidden({ 'node_modules/a': { version: '1.0.0', integrity: 'sha512-a' } });
    assert.equal(await piTreeMatchesLock(prefix, lock), true, 'an optional package for another platform may be absent');
    await writeHidden({ 'node_modules/a': { version: '1.0.1', integrity: 'sha512-a' } });
    assert.equal(await piTreeMatchesLock(prefix, lock), false, 'wrong version');
    await writeHidden({ 'node_modules/a': { version: '1.0.0', integrity: 'sha512-x' } });
    assert.equal(await piTreeMatchesLock(prefix, lock), false, 'wrong integrity');
    await writeHidden({ 'node_modules/a': { version: '1.0.0', integrity: 'sha512-a' }, 'node_modules/extra': { version: '1.0.0', integrity: 'sha512-e' } });
    assert.equal(await piTreeMatchesLock(prefix, lock), false, 'a package the lockfile does not list');
    await writeHidden({});
    assert.equal(await piTreeMatchesLock(prefix, lock), false, 'a required package missing');
  } finally { await fs.rm(prefix, { recursive: true, force: true }); }
});

test('a Pi install counts as verified only with a marker for this pin and lockfile', async () => {
  const { writePiVerified, clearPiVerified, piInstallVerified } = await import('./launch.mjs');
  const prefix = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-pi-verified-'));
  try {
    assert.equal(await piInstallVerified(prefix), false, 'no marker');
    await writePiVerified(prefix);
    assert.equal(await piInstallVerified(prefix), true);
    assert.equal(await piInstallVerified(prefix, { pin: '9.9.9' }), false, 'another pin');
    assert.equal(await piInstallVerified(prefix, { sha256: 'f'.repeat(64) }), false, 'another lockfile');
    await clearPiVerified(prefix);
    assert.equal(await piInstallVerified(prefix), false, 'cleared before a reinstall');
  } finally { await fs.rm(prefix, { recursive: true, force: true }); }
});

test('launch refuses a Pi runtime whose install did not pass every check', async t => {
  if (!nodeSupportsPi()) return t.skip('this host cannot run Pi');
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-launch-pi-unverified-'));
  const harnessRoot = path.join(root, 'harnesses');
  const dataDir = path.join(root, 'data');
  try {
      const pkgRoot = path.join(harnessRoot, 'pi/node_modules/@earendil-works/pi-coding-agent');
    const runtime = path.join(pkgRoot, 'dist/bundle/cli.js');
    await fs.mkdir(path.dirname(runtime), { recursive: true });
    await fs.writeFile(path.join(pkgRoot, 'package.json'), JSON.stringify({ name: '@earendil-works/pi-coding-agent', version: PINS.pi }));
    await fs.writeFile(runtime, 'process.exit(0);');
    await fs.mkdir(path.join(dataDir, 'harness-accounts'), { recursive: true });
    await fs.writeFile(path.join(dataDir, 'harness-accounts', 'pi.json'), JSON.stringify({ version: 1, selected: 'personal/main', personal: { main: { name: 'main', apiKey: 'k', billing: 'api', model: 'deepseek-flash' } }, team: {} }));
    const child = spawn(process.execPath, [fileURLToPath(new URL('./launch.mjs', import.meta.url)), 'pi', '--data-dir', dataDir], { env: { ...process.env, SOLU_HARNESS_ROOT: harnessRoot }, stdio: ['pipe', 'pipe', 'pipe'] });
    let errors = '';
    child.stderr.on('data', data => { errors += data; });
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); });
    assert.equal(code, 1, `expected the launch to refuse an unverified Pi: ${errors}`);
    assert.match(errors, /Pi is not verified on this computer/);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('DeepSeek Harness talks Messages under /anthropic; saved official roots move there', () => {
  assert.equal(endpoint('deepseek', 'api'), 'https://api.deepseek.com/anthropic');
  assert.equal(endpoint('pi', 'api'), 'https://api.deepseek.com', 'Pi stays on Chat Completions');
  for (const saved of ['', 'https://api.deepseek.com', 'https://api.deepseek.com/', 'https://API.deepseek.com/v1', '  https://api.deepseek.com/v1/ ']) {
    assert.equal(deepseekBaseURL(saved), 'https://api.deepseek.com/anthropic', saved);
  }
  assert.equal(deepseekBaseURL('https://api.deepseek.com/anthropic'), 'https://api.deepseek.com/anthropic');
  assert.equal(deepseekBaseURL('https://gateway.example/deepseek'), 'https://gateway.example/deepseek');
  const broker = 'http://127.0.0.1:8787/api/team-broker/deepseek/main';
  assert.equal(deepseekPlan({ name: 'main', apiKey: 'cap', billing: 'api', model: '', baseURL: broker, broker: true }, '/data', []).env.DEEPSEEK_BASE_URL, broker);
});

test('the pane client patch turns off the session log and plugin inventory', async () => {
  assert.deepEqual(DSH_PRIVACY_ROWS, [
    { id: 'session-log-deepseek', config: { enabled: false } },
    { id: 'plugin-package-inventory-deepseek', config: { enabled: false } },
  ]);
  assert.deepEqual(dshPatchEntries('deepseek-flash'), [{ id: 'acp', config: { provider: 'deepseek-official', model: 'deepseek-flash' } }, ...DSH_PRIVACY_ROWS]);
  const home = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-dsh-anon-'));
  try {
    await rotateDshAnonymousId(home);
    const first = (await fs.readFile(path.join(home, '.anonymous-user-id'), 'utf8')).trim();
    await rotateDshAnonymousId(home);
    const second = (await fs.readFile(path.join(home, '.anonymous-user-id'), 'utf8')).trim();
    assert.match(first, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    assert.notEqual(first, second, 'each launch gets a fresh id');
  } finally { await fs.rm(home, { recursive: true, force: true }); }
});

test('the DeepSeek Harness lockfile is hash-pinned: a tampered integrity line is refused before npm runs', async () => {
  const lockBytes = await fs.readFile(new URL('./dsh-lock.json', import.meta.url));
  assert.equal(crypto.createHash('sha256').update(lockBytes).digest('hex'), DSH_LOCK_SHA256, 'dsh-lock.json must match DSH_LOCK_SHA256');
  const prefix = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-dsh-lock-'));
  try {
    const lock = await prepareDshLock(prefix, lockBytes);
    assert.equal(lock.packages['node_modules/@deepseek-ai/dsh'].version, PINS.deepseek);
    assert.equal(lock.packages['node_modules/node-pty'].version, DSH_SPAWN_HELPER.nodePty, 'DSH_SPAWN_HELPER is pinned for the locked node-pty');
    for (const [key, entry] of Object.entries(lock.packages)) {
      if (!key) continue;
      assert.match(entry.integrity || '', /^sha512-/, `${key} has no sha512 integrity`);
      assert.match(entry.resolved || '', /^https:\/\/registry\.npmjs\.org\//, `${key} not from the npm registry`);
    }
    assert.equal(await fs.readFile(path.join(prefix, 'package.json'), 'utf8'), dshRootPackageJSON());
    assert.deepEqual(await fs.readFile(path.join(prefix, 'package-lock.json')), lockBytes);
    assert.equal(lock.packages[''].dependencies['@deepseek-ai/dsh'], PINS.deepseek, 'root dependency must match the generated package.json');
    const text = String(lockBytes);
    const marker = '"integrity":"sha512-';
    assert.ok(text.includes(marker));
    const at = text.indexOf(marker) + marker.length;
    const tampered = Buffer.from(text.slice(0, at) + (text[at] === 'A' ? 'B' : 'A') + text.slice(at + 1));
    const other = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-dsh-lock-'));
    try {
      await assert.rejects(prepareDshLock(other, tampered), /DeepSeek Harness install refused: its lockfile does not match the pinned hash/);
      await assert.rejects(fs.stat(path.join(other, 'package-lock.json')), 'a refused lockfile must not be written');
    } finally { await fs.rm(other, { recursive: true, force: true }); }
    await assert.rejects(prepareDshLock(prefix, lockBytes, { pin: '9.9.9' }), /does not resolve DeepSeek Harness 9\.9\.9/);
  } finally { await fs.rm(prefix, { recursive: true, force: true }); }
});

test('a DeepSeek Harness install counts as verified only with a marker for this pin and lockfile', async () => {
  const prefix = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-dsh-verified-'));
  try {
    assert.equal(await dshInstallVerified(prefix), false, 'no marker');
    await writeDshVerified(prefix);
    assert.equal(await dshInstallVerified(prefix), true);
    assert.equal(await dshInstallVerified(prefix, { pin: '9.9.9' }), false, 'another pin');
    assert.equal(await dshInstallVerified(prefix, { sha256: 'f'.repeat(64) }), false, 'another lockfile');
    const { piInstallVerified } = await import('./launch.mjs');
    assert.equal(await piInstallVerified(prefix), false, 'a DeepSeek Harness marker is not a Pi marker');
    await clearDshVerified(prefix);
    assert.equal(await dshInstallVerified(prefix), false, 'cleared before a reinstall');
  } finally { await fs.rm(prefix, { recursive: true, force: true }); }
});

test('the installer sets the spawn-helper mode only for the pinned node-pty and a pinned hash', async () => {
  const prefix = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-dsh-helper-'));
  const body = Buffer.from('helper-bytes');
  const sha = crypto.createHash('sha256').update(body).digest('hex');
  const spec = { nodePty: '1.2.0-beta.15', mode: 0o755, sha256: { 'darwin-arm64': sha } };
  const lock = { packages: { 'node_modules/node-pty': { version: '1.2.0-beta.15' } } };
  const helper = (plat) => path.join(prefix, 'node_modules', 'node-pty', 'prebuilds', plat, 'spawn-helper');
  try {
    // Linux and Windows prebuilds ship no helper: nothing to do, and an
    // unexpected one is refused.
    assert.equal(await ensureDshSpawnHelper(prefix, lock, { platform: 'linux', arch: 'x64', spec }), null);
    await fs.mkdir(path.dirname(helper('linux-x64')), { recursive: true });
    await fs.writeFile(helper('linux-x64'), body, { mode: 0o644 });
    await assert.rejects(ensureDshSpawnHelper(prefix, lock, { platform: 'linux', arch: 'x64', spec }), /unexpected spawn-helper/);
    await assert.rejects(ensureDshSpawnHelper(prefix, { packages: { 'node_modules/node-pty': { version: '1.2.0' } } }, { platform: 'darwin', arch: 'arm64', spec }), /does not pin node-pty 1\.2\.0-beta\.15/);
    await assert.rejects(ensureDshSpawnHelper(prefix, lock, { platform: 'darwin', arch: 'arm64', spec }), /spawn-helper is missing/);
    await fs.mkdir(path.dirname(helper('darwin-arm64')), { recursive: true });
    await fs.writeFile(helper('darwin-arm64'), Buffer.from('tampered'), { mode: 0o644 });
    await assert.rejects(ensureDshSpawnHelper(prefix, lock, { platform: 'darwin', arch: 'arm64', spec }), /does not match its pinned hash/);
    assert.equal((await fs.stat(helper('darwin-arm64'))).mode & 0o777, 0o644, 'a mismatched helper is not made executable');
    await fs.writeFile(helper('darwin-arm64'), body, { mode: 0o644 });
    await fs.chmod(helper('darwin-arm64'), 0o644);
    assert.equal(await ensureDshSpawnHelper(prefix, lock, { platform: 'darwin', arch: 'arm64', spec }), helper('darwin-arm64'));
    if (process.platform !== 'win32') assert.equal((await fs.stat(helper('darwin-arm64'))).mode & 0o7777, 0o755);
  } finally { await fs.rm(prefix, { recursive: true, force: true }); }
});

test('the pinned macOS spawn-helper hashes cover both shipped prebuilds', () => {
  assert.deepEqual(Object.keys(DSH_SPAWN_HELPER.sha256).sort(), ['darwin-arm64', 'darwin-x64']);
  for (const sha of Object.values(DSH_SPAWN_HELPER.sha256)) assert.match(sha, /^[0-9a-f]{64}$/);
  assert.equal(DSH_SPAWN_HELPER.mode, 0o755, 'dsh-subprocess-local sets 0755');
});

test('launch refuses a DeepSeek Harness runtime whose install did not pass every check', async t => {
  if (!nodeSupportsDsh()) return t.skip('this host cannot run dsh (Node < 24.9)');
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-launch-dsh-unverified-'));
  const harnessRoot = path.join(root, 'harnesses');
  const dataDir = path.join(root, 'data');
  try {
    const pkgRoot = path.join(harnessRoot, 'deepseek/node_modules/@deepseek-ai/dsh');
    const runtime = path.join(pkgRoot, 'lib/bin.js');
    const ran = path.join(root, 'ran');
    await fs.mkdir(path.dirname(runtime), { recursive: true });
    await fs.writeFile(path.join(pkgRoot, 'package.json'), JSON.stringify({ name: '@deepseek-ai/dsh', version: PINS.deepseek }));
    await fs.writeFile(runtime, `require('node:fs').writeFileSync(${JSON.stringify(ran)}, '1');`);
    await fs.mkdir(path.join(dataDir, 'harness-accounts'), { recursive: true });
    await fs.writeFile(path.join(dataDir, 'harness-accounts', 'deepseek.json'), JSON.stringify({ version: 1, selected: 'personal/main', personal: { main: { name: 'main', apiKey: 'k', billing: 'api', model: 'deepseek-flash' } }, team: {} }));
    const child = spawn(process.execPath, [fileURLToPath(new URL('./launch.mjs', import.meta.url)), 'deepseek', '--data-dir', dataDir], { env: { ...process.env, SOLU_HARNESS_ROOT: harnessRoot }, stdio: ['pipe', 'pipe', 'pipe'] });
    let errors = '';
    child.stderr.on('data', data => { errors += data; });
    const code = await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', resolve); });
    assert.equal(code, 1, errors);
    assert.match(errors, /DeepSeek Harness is not verified on this computer/);
    await assert.rejects(fs.stat(ran), 'an unverified bin.js must never run');
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test('the DeepSeek Harness lockfile keeps the one-entry-per-line layout', async () => {
  const lockBytes = await fs.readFile(new URL('./dsh-lock.json', import.meta.url), 'utf8');
  assert.equal(formatHarnessLock(JSON.parse(lockBytes)), lockBytes, 'regenerate dsh-lock.json with formatHarnessLock (see launch.mjs)');
  assert.ok(lockBytes.split('\n').length < 1000, 'one line per package keeps a bump reviewable');
});

test('the install privacy check refuses a dump where a privacy row is missing or on', async () => {
  const { dshPrivacyRowsOff, verifyDshPrivacy } = await import('./launch.mjs');
  const off = ['- id: session', "  name: '@deepseek-ai/dsh-session'", '- id: session-log-deepseek', "  name: '@deepseek-ai/dsh-session-log-deepseek'", '  config:', '    enabled: false',
    '- id: plugin-package-inventory-deepseek', "  name: '@deepseek-ai/dsh-plugin-package-inventory-deepseek'", '  disabled: true', '- id: typert'].join('\n');
  assert.deepEqual(dshPrivacyRowsOff(off), []);
  assert.deepEqual(dshPrivacyRowsOff(off.replace('    enabled: false', '    enabled: true')), ['session-log-deepseek is not switched off']);
  assert.deepEqual(dshPrivacyRowsOff(off.replace('- id: session-log-deepseek', '- id: session-log-deepseek-v2')), ['session-log-deepseek is missing']);
  // A nested setting that is off does not vouch for a row that is on.
  assert.deepEqual(dshPrivacyRowsOff(off.replace('    enabled: false', '    enabled: true\n    nested:\n      enabled: false')), ['session-log-deepseek is not switched off']);
  // A later section's `enabled: false` does not vouch for the last row.
  assert.deepEqual(dshPrivacyRowsOff(['- id: session-log-deepseek', '  config:', '    enabled: false', '- id: plugin-package-inventory-deepseek', "  name: 'x'", 'other:', '  enabled: false'].join('\n')), ['plugin-package-inventory-deepseek is not switched off']);
  // A fake runtime whose dump no longer has the session-log row (as a future
  // rename would) fails the install check.
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'solu-dsh-privacy-check-'));
  try {
    const runtime = path.join(root, 'bin.mjs');
    await fs.writeFile(runtime, `process.stdout.write(${JSON.stringify(off.replace('- id: session-log-deepseek', '- id: renamed-session-log'))});`);
    await assert.rejects(verifyDshPrivacy(runtime, path.join(root, 'home')), error => /install refused: in its acp profile, session-log-deepseek is missing/.test(error.message) && error.privacyRows === true);
    // A runtime that fails to run is not a row finding.
    await fs.writeFile(runtime, 'process.exit(3);');
    await assert.rejects(verifyDshPrivacy(runtime, path.join(root, 'home')), error => /privacy check exited 3/.test(error.message) && !error.privacyRows);
    await fs.writeFile(runtime, `process.stdout.write(${JSON.stringify(off)});`);
    await verifyDshPrivacy(runtime, path.join(root, 'home'));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});
