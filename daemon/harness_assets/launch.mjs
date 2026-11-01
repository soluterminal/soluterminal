#!/usr/bin/env node
// Only credentials selected in SoluTerminal can reach a managed harness.
import fs from 'node:fs/promises';
import fsSync from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { createDecipheriv, createHash } from 'node:crypto';
import { PINS } from './pins.mjs';
import { canonicalDeepseekModel } from './deepseek-model.mjs';

// The root is overridable so tests can point at a fake runtime tree.
export const runtimeRoot = process.env.SOLU_HARNESS_ROOT || path.join(os.homedir(), '.local/share/soluterminal/harnesses');
// dsh's CLI only runs when `import.meta.main` exists (Node >= 24.2). On an
// older Node it boots nothing and the ACP client waits for a handshake that
// never comes, so fail with a precise message instead of a 30s stall.
export const nodeSupportsDsh = () => typeof import.meta.main === 'boolean';
// Pi's published engines floor. Unlike dsh it does not need import.meta.main,
// so the two gates are deliberately separate.
export const nodeSupportsPi = () => {
  const [major, minor] = String(process.versions.node).split('.').map(Number);
  return major > 22 || (major === 22 && minor >= 19);
};
export function endpoint(provider, billing) {
  if (provider === 'deepseek' && billing === 'api') return 'https://api.deepseek.com';
  if (provider === 'pi' && billing === 'api') return 'https://api.deepseek.com';
  if (provider === 'pi-glm' && billing === 'coding-plan') return 'https://api.z.ai/api/coding/paas/v4';
  if (provider === 'glm' && billing === 'api') return 'https://api.z.ai/api/paas/v4';
  if (provider === 'glm' && billing === 'coding-plan') return 'https://api.z.ai/api/coding/paas/v4';
  throw new Error('Unsupported provider or billing type.');
}
export function selectedAccount(state, provider, now = Date.now()) {
  const [source, name, extra] = String(state.selected ?? '').split('/');
  const account = ['personal', 'team'].includes(source) && !extra ? state[source]?.[name] : null;
  const keylessTeam = source === 'team' && account?.brokerOnly === true && account.apiKey === '' &&
    typeof state.teamId === 'string' && state.teamId !== '';
  if (state.version !== 1 || !account || account.name !== name || typeof account.apiKey !== 'string' ||
      (!keylessTeam && (!account.apiKey.trim() || /[\r\n\0]/.test(account.apiKey))) ||
      typeof account.model !== 'string' ||
      (!['deepseek', 'pi'].includes(provider) && !account.model.trim())) {
    throw new Error('Select an API account in SoluTerminal → Credentials first.');
  }
  endpoint(provider, account.billing);
  // Routed keys require the paired daemon to keep validating the assignment.
  if (source === 'team' && (!state.scope || !state.syncedAt || now / 1000 - state.syncedAt > 120)) {
    throw new Error('Account route unavailable. Open SoluTerminal and refresh the selected account.');
  }
  if (source === 'team' && !keylessTeam) {
    throw new Error('Account route unavailable. Ask the account owner to renew access.');
  }
  const model = ['deepseek', 'pi'].includes(provider) ? canonicalDeepseekModel(account.model) : account.model;
  return { ...account, model, source, scope: source === 'team' ? state.scope : '', teamId: keylessTeam ? state.teamId : '' };
}
export function cleanEnvironment(env) {
  return Object.fromEntries(Object.entries(env).filter(([key]) =>
    !/^(ZCODE_|DSH_|PI_|DEEPSEEK_|ZAI_|GLM_|ANTHROPIC_|OPENAI_|SOLUTERMINAL_GLM_)/i.test(key) &&
    !['NODE_OPTIONS', 'NODE_TLS_REJECT_UNAUTHORIZED'].includes(key)));
}
export const defaultDataDir = () => path.join(os.homedir(), '.solu');
// A managed shortcut records the data dir that owns it in its --data-dir value.
export function shortcutDataDir(script) {
  const match = String(script ?? '').match(/--data-dir\s+(?:'([^']*)'|"([^"]*)"|(\S+))/);
  return match ? (match[1] ?? match[2] ?? match[3] ?? '') : '';
}
// One shortcut name is shared by every data dir on the machine, so a
// side-by-side install (a test or dev data dir) must never silently repoint the
// command a user relies on to reach the real account. The default data dir
// always wins so it can self-heal after such a hijack; any other data dir may
// only rewrite a shortcut that already points at it. `existing` is the current
// file contents, or null when the file does not exist.
export function shortcutDisposition(existing, dataDir, defaultDir = defaultDataDir()) {
  if (existing == null) return 'create';
  if (!String(existing).includes('# soluterminal-managed-harness')) return 'retain';
  const current = shortcutDataDir(existing);
  if (current && path.resolve(current) === path.resolve(dataDir)) return 'replace';
  if (path.resolve(dataDir) === path.resolve(defaultDir)) return 'replace';
  return 'skip';
}
// The managed shortcut is deliberately tiny: it execs the shared resolver with
// the interpreter captured when the shortcut was written, and the resolver
// re-resolves Node at run time so a moved or removed nvm/asdf install does not
// strand the command. Keep this in one place — the daemon's Go writer
// (writeHarnessCommand in daemon/harness_api.go) mirrors it, and tests on both
// sides pin the format.
export function renderManagedShortcut({ resolver, node, launcher, provider, dataDir }) {
  const quote = text => "'" + String(text).replaceAll("'", "'\\''") + "'";
  return `#!/bin/sh\n# soluterminal-managed-harness\nexec /bin/sh ${quote(resolver)} ${quote(node)} ${quote(launcher)} ${quote(provider)} --data-dir ${quote(dataDir)} "$@"\n`;
}
async function readState(file) { return JSON.parse(await fs.readFile(file, 'utf8')); }
const fingerprint = (account) => createHash('sha256').update(JSON.stringify(account)).digest('hex');

// Stored API keys may be sealed at rest as `enc:v1:<base64 nonce|ciphertext|tag>`
// (AES-256-GCM with the 32-byte hex key in <dataDir>/harness-accounts/.secret-key).
// A harness child needs the cleartext key in its environment, so unseal here and
// keep the envelope off the wire. Plaintext keys pass through unchanged for
// stores written before sealing existed. Failures name the recovery (re-save in
// Credentials) and never echo key material.
export async function resolveAccountKey(apiKey, dataDir) {
  if (!apiKey.startsWith('enc:')) return apiKey;
  if (!apiKey.startsWith('enc:v1:')) throw new Error('Unsupported managed credential envelope. Re-save the API account in SoluTerminal → Credentials.');
  let secret = '';
  try {
    secret = (await fs.readFile(path.join(dataDir, 'harness-accounts', '.secret-key'), 'utf8')).trim();
  } catch {
    throw new Error('Managed credential store is sealed but its local key is missing. Re-save the API account in SoluTerminal → Credentials.');
  }
  if (!/^[0-9a-fA-F]{64}$/.test(secret)) throw new Error('Managed credential key is malformed. Re-save the API account in SoluTerminal → Credentials.');
  let raw = null;
  try {
    raw = Buffer.from(apiKey.slice('enc:v1:'.length), 'base64');
    if (raw.length < 12 + 16 + 1) throw new Error('short envelope');
  } catch {
    throw new Error('Managed API account is unreadable. Re-save it in SoluTerminal → Credentials.');
  }
  try {
    const decipher = createDecipheriv('aes-256-gcm', Buffer.from(secret, 'hex'), raw.subarray(0, 12));
    decipher.setAuthTag(raw.subarray(raw.length - 16));
    const plain = Buffer.concat([decipher.update(raw.subarray(12, raw.length - 16)), decipher.final()]).toString('utf8');
    if (!plain.trim() || /[\r\n\0]/.test(plain)) throw new Error('empty key');
    return plain;
  } catch {
    throw new Error('Managed API account failed to unlock. Re-save it in SoluTerminal → Credentials.');
  }
}

// The selected account with a launchable key. The fingerprint compares this
// resolved form so re-sealing the same key (fresh nonce, new ciphertext) does
// not look like an account change, while a real key/model/scope change still
// restarts the harness.
export async function loadedAccount(file, provider, dataDir) {
  const stored = selectedAccount(await readState(file), provider);
  return { ...stored, apiKey: await resolveAccountKey(stored.apiKey, dataDir) };
}

// Team assignments are keyless entries in the normal account store. Reading
// the selected entry here keeps shortcut launches and the in-app selector in
// sync, with no second sidecar that could point at a revoked assignment.
export async function readBrokerMetadata(dataDir, provider) {
  const state = await readState(path.join(dataDir, 'harness-accounts', `${provider}.json`));
  const meta = selectedAccount(state, provider);
  if (meta.source !== 'team' || !meta.brokerOnly || meta.apiKey !== '' ||
      !/^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(meta.name) || !meta.teamId) {
    throw new Error('Account route unavailable. Refresh the selected account in SoluTerminal.');
  }
  endpoint(provider, meta.billing);
  return meta;
}

export async function loadedBrokerAccount(dataDir, provider, daemonURL = process.env.SOLU_DAEMON_URL || '') {
  const meta = await readBrokerMetadata(dataDir, provider);
  if (!daemonURL) {
    try { daemonURL = (await fs.readFile(path.join(dataDir, 'harness-launchers', 'daemon-url'), 'utf8')).trim(); }
    catch { throw new Error('Account route unavailable. Open SoluTerminal and retry.'); }
  }
  const daemon = new URL(daemonURL);
  if (!['127.0.0.1', '::1', 'localhost'].includes(daemon.hostname) || daemon.protocol !== 'http:') {
    throw new Error('Account route unavailable. The daemon address must be local.');
  }
  const response = await fetch(new URL('/api/team-broker/capability', daemon), {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ provider, label: meta.name, teamId: meta.teamId }),
  });
  if (!response.ok) throw new Error('Account route unavailable. Open SoluTerminal and check the connection.');
  const cap = await response.json();
  if (typeof cap.token !== 'string' || !/^[0-9a-f]{64}$/.test(cap.token) || typeof cap.baseURL !== 'string' ||
      !cap.baseURL.startsWith(`${daemon.origin}/api/team-broker/${provider}/`)) {
    throw new Error('Account route unavailable. The session is invalid.');
  }
  return { name: meta.name, model: meta.model, billing: meta.billing, source: 'team', scope: meta.teamId,
    apiKey: cap.token, baseURL: cap.baseURL, broker: true };
}

// CLI 0.16.5 advertises --settings but rejects it. Its supported configuration
// location is ~/.zcode/cli/config.json. The interactive login checker requires
// a key in provider.zai.options, even for the prepaid OpenAI-compatible route.
// The daemon reconciles this managed live slot on selection/revocation changes.
export async function configureZcode(account, cwd, configFile = path.join(os.homedir(), '.zcode/cli/config.json'), storePath = '') {
  let cursor = path.resolve(cwd);
  for (;;) {
    for (const name of ['zcode.json', '.zcode/config.json']) {
      try {
        const project = JSON.parse(await fs.readFile(path.join(cursor, name), 'utf8'));
        if (project.model || project.provider) throw new Error('This workspace overrides ZCode model routing. Remove its model/provider override before using a managed API account.');
      } catch (error) { if (error.code !== 'ENOENT') throw error; }
    }
    if (await fs.stat(path.join(cursor, '.git')).then(() => true, () => false) || path.dirname(cursor) === cursor) break;
    cursor = path.dirname(cursor);
  }
  let config = {};
  try { config = JSON.parse(await fs.readFile(configFile, 'utf8')); }
  catch (error) { if (error.code !== 'ENOENT') throw new Error('ZCode config is unreadable or malformed; it was not overwritten.'); }
  if (!config || typeof config !== 'object' || Array.isArray(config)) throw new Error('Invalid ZCode config.');
  const ref = `zai/${account.model}`;
  if (!config.soluterminalManagedGLM && config.provider?.zai && !config.soluterminalPreviousZaiProvider) {
    config.soluterminalPreviousZaiProvider = config.provider.zai;
  }
  config.provider = { ...config.provider, zai: {
    kind: 'openai-compatible', options: { baseURL: account.baseURL ? `${account.baseURL}/v1` : endpoint('glm', account.billing), apiKey: account.apiKey }, models: { [account.model]: {} },
  } };
  config.soluterminalManagedGLM = { storePath, source: account.source, name: account.name, billing: account.billing, model: account.model, scope: account.scope, brokerOnly: account.broker === true, keyHash: createHash('sha256').update(account.apiKey).digest('hex') };
  const model = config.model && typeof config.model === 'object' ? config.model : {};
  config.model = { ...model, main: ref, lite: ref };
  await fs.mkdir(path.dirname(configFile), { recursive: true, mode: 0o700 });
  const temp = `${configFile}.${process.pid}.tmp`;
  await fs.writeFile(temp, JSON.stringify(config, null, 2) + '\n', { mode: 0o600 });
  await fs.rename(temp, configFile);
}

// Build the DeepSeek child's argv and environment. The key lives only in the
// environment: argv is visible in `ps` and is echoed to the browser.
export function deepseekPlan(account, dataDir, args = [], baseEnv = process.env) {
  account = { ...account, model: canonicalDeepseekModel(account.model) };
  const env = cleanEnvironment(baseEnv);
  env.DEEPSEEK_API_KEY = account.apiKey;
  env.DEEPSEEK_BASE_URL = account.baseURL || endpoint('deepseek', account.billing);
  env.DSH_HOME = path.join(dataDir, 'harness-state', 'deepseek');
  // Attribution for the per-key spend meter: the terminal client writes a
  // session→account sidecar so the daemon can price each session's turns
  // against the account (and therefore the user) that launched it.
  env.SOLU_HARNESS_ACCOUNT = account.name;
  env.SOLU_HARNESS_ACCOUNT_MODEL = account.model;
  env.SOLU_HARNESS_DATA_DIR = dataDir;
  const packageRoot = path.join(runtimeRoot, 'deepseek/node_modules/@deepseek-ai/dsh');
  const runtime = path.join(packageRoot, 'lib/bin.js');
  const client = fileURLToPath(new URL('./deepseek-terminal.mjs', import.meta.url));
  return { argv: [client, runtime, account.model, ...args], env, runtime, packageJson: path.join(packageRoot, 'package.json') };
}

// Build a Pi child's argv and environment for one Pi-provider binding. Pi is
// model-agnostic, but a managed account carries one key, so each harness row
// binds Pi to exactly one of Pi's first-party providers with that provider's
// key env var. Each row gets its own managed home, so concurrent Pi sessions
// on different vendors never share state. The key lives only in the
// environment, and offline/telemetry flags keep a managed launch from
// talking to pi.dev on its own.
//
// Pi has no built-in tool-permission system, so every managed launch loads
// pi-approval.mjs with an explicit `--extension`. It renders the approval menu
// shape SoluTerminal's per-pane AutoMode already recognizes, giving a managed
// Pi pane the same approval gate and always-allow memory as Claude, Codex,
// and the DeepSeek client. Explicit `-e` paths load before project trust, so
// the gate exists even in an untrusted checkout.
function piPlanFor(piProvider, keyEnv, stateDir, account, dataDir, args, baseEnv) {
  const env = cleanEnvironment(baseEnv);
  env[keyEnv] = account.apiKey;
  env.PI_CODING_AGENT_DIR = path.join(dataDir, 'harness-state', stateDir);
  env.PI_CODING_AGENT_SESSION_DIR = path.join(dataDir, 'harness-state', stateDir, 'sessions');
  env.PI_OFFLINE = '1';
  env.PI_SKIP_VERSION_CHECK = '1';
  env.PI_TELEMETRY = '0';
  // Ownership tag: the daemon's orphan reaper only touches clients whose data
  // dir matches its own (agent_reaper.go harnessClientBelongsTo).
  env.SOLU_HARNESS_DATA_DIR = dataDir;
  const packageRoot = path.join(runtimeRoot, 'pi/node_modules/@earendil-works/pi-coding-agent');
  const runtime = path.join(packageRoot, 'dist/bundle/cli.js');
  const approval = fileURLToPath(new URL('./pi-approval.mjs', import.meta.url));
  // Daemon-driven resume. The approval extension records this pane's Pi session
  // id in a per-pane sidecar; the daemon replays it here as `--session <id>`.
  // Pi's resolver matches an exact id (or unique prefix) inside this pane's
  // session dir, so the conversation continues across a pane-agent restart. A
  // stale id is dropped rather than passed: `--session` on a missing session
  // makes Pi exit immediately, so verify the .jsonl really is there first.
  const resumeSession = String(baseEnv?.SOLU_HARNESS_RESUME_SESSION || '').trim();
  const argv = [runtime, '--provider', piProvider, '--model', account.model];
  if (resumeSession && piSessionOnDisk(env.PI_CODING_AGENT_SESSION_DIR, resumeSession)) argv.push('--session', resumeSession);
  argv.push('--extension', approval, ...args);
  return { argv, env, runtime, packageJson: path.join(packageRoot, 'package.json') };
}

// piSessionOnDisk reports whether a Pi session file for `sessionId` exists under
// sessionDir (filename `<timestamp>_<id>.jsonl`). Recursive because Pi may nest
// per-cwd session directories. A miss is not fatal — the launcher starts fresh.
function piSessionOnDisk(sessionDir, sessionId) {
  const id = String(sessionId || '').trim();
  const root = String(sessionDir || '').trim();
  if (!id || !root) return false;
  const stack = [root];
  try {
    while (stack.length) {
      const dir = stack.pop();
      for (const entry of fsSync.readdirSync(dir, { withFileTypes: true })) {
        const full = path.join(dir, entry.name);
        if (entry.isDirectory()) stack.push(full);
        else if (entry.isFile() && entry.name.endsWith('.jsonl') && entry.name.includes(id)) return true;
      }
    }
  } catch { /* missing/unreadable session dir → treat as no reusable session */ }
  return false;
}

// Pi on its first-party DeepSeek provider.
export function piPlan(account, dataDir, args = [], baseEnv = process.env) {
  return piPlanFor('deepseek', 'DEEPSEEK_API_KEY', 'pi', { ...account, model: canonicalDeepseekModel(account.model) }, dataDir, args, baseEnv);
}

// Pi on its first-party ZAI provider. Pi's zai provider uses the Z.ai Coding
// Plan key (ZAI_API_KEY) against its fixed Coding Plan base URL, which has no
// override — so the pi-glm row declares only the coding-plan billing mode.
export function piGlmPlan(account, dataDir, args = [], baseEnv = process.env) {
  return piPlanFor('zai', 'ZAI_API_KEY', 'pi-glm', account, dataDir, args, baseEnv);
}

// Pi reads provider overrides from its managed home. Only a route launch
// writes this file, and an existing user-owned model configuration is never
// replaced. This preserves the built-in DeepSeek/ZAI model catalog and tool
// behavior while redirecting provider calls to the daemon.
export async function configurePiBroker(account, piHome, provider) {
  if (!account.broker || !account.baseURL || !['pi', 'pi-glm'].includes(provider)) return;
  const file = path.join(piHome, 'models.json');
  const marker = path.join(piHome, '.soluterminal-managed-broker');
  let existing = null;
  try { existing = JSON.parse(await fs.readFile(file, 'utf8')); }
  catch (error) { if (error.code !== 'ENOENT') throw new Error('Account route unavailable. Pi model configuration is unreadable.'); }
  const managed = await fs.access(marker).then(() => true, () => false);
  if (existing && !managed && !isManagedPiBrokerConfig(existing)) {
    throw new Error('Account route unavailable. Move custom models out of the managed Pi home and retry.');
  }
  const key = provider === 'pi' ? 'deepseek' : 'zai';
  const config = { providers: { [key]: {
    baseUrl: `${account.baseURL}/v1`, apiKey: account.apiKey,
  } } };
  await fs.mkdir(piHome, { recursive: true, mode: 0o700 });
  const temp = `${file}.${process.pid}.tmp`;
  const text = JSON.stringify(config) + '\n';
  await fs.writeFile(temp, text, { mode: 0o600 });
  await fs.rename(temp, file);
  if (!managed) {
    try { await fs.writeFile(marker, 'managed by SoluTerminal\n', { mode: 0o600 }); }
    catch (error) {
      // The file was created by this call; remove it only if nobody replaced
      // our exact bytes between the rename and marker write.
      if (await fs.readFile(file, 'utf8').catch(() => '') === text) await fs.unlink(file).catch(() => {});
      throw error;
    }
  }
}

function isManagedPiBrokerConfig(config) {
  if (!config || typeof config !== 'object' || Object.keys(config).join(',') !== 'providers') return false;
  const providers = config.providers;
  if (!providers || typeof providers !== 'object' || Object.keys(providers).length !== 1) return false;
  const [key] = Object.keys(providers);
  const entry = providers[key];
  return ['deepseek', 'zai'].includes(key) && entry && Object.keys(entry).sort().join(',') === 'apiKey,baseUrl' &&
    /^[0-9a-f]{64}$/.test(entry.apiKey) &&
    /^http:\/\/(?:127\.0\.0\.1|\[::1\]):\d+\/api\/team-broker\/(?:pi|pi-glm)\/[A-Za-z0-9_.%-]+\/v1$/.test(entry.baseUrl);
}

export async function clearPiBroker(piHome) {
  const file = path.join(piHome, 'models.json');
  const marker = path.join(piHome, '.soluterminal-managed-broker');
  const marked = await fs.access(marker).then(() => true, () => false);
  let config;
  try { config = JSON.parse(await fs.readFile(file, 'utf8')); }
  catch (error) { if (error.code !== 'ENOENT') throw new Error('Pi model configuration is unreadable.'); }
  if (isManagedPiBrokerConfig(config)) await fs.unlink(file);
  if (marked) await fs.unlink(marker);
}

// The installer verifies the tarball provenance and boots the runtime once. A
// managed launch additionally refuses a runtime whose on-disk version has
// drifted from the pin — a partial install, a manual swap, or a stale prefix —
// instead of executing it silently. A missing manifest is tolerated so fakes
// and pre-provenance installs still run; published packages always ship one.
export async function verifyPinnedRuntime(packageJson, pinnedVersion, label) {
  let version = '';
  try {
    version = String(JSON.parse(await fs.readFile(packageJson, 'utf8')).version || '');
  } catch { /* no manifest: nothing to compare */ }
  if (version && version !== pinnedVersion) {
    throw new Error(`${label} ${version} is installed but ${pinnedVersion} is required. Reinstall it from SoluTerminal → Credentials.`);
  }
}

// The client's build stamp. The daemon writes VERSION beside the extracted
// launcher under <dataDir>/harness-launchers; a plain repo checkout has none,
// so fall back to the product version in the Tauri config. Without this the
// shortcut-launched client reports a bare "dev" and "am I on the latest?" has
// no answer.
export async function clientBuildVersion(launchFile = fileURLToPath(import.meta.url)) {
  const dir = path.dirname(launchFile);
  try {
    const stamp = (await fs.readFile(path.join(dir, 'VERSION'), 'utf8')).trim();
    if (stamp) return stamp;
  } catch { /* no extracted stamp */ }
  try {
    const conf = JSON.parse(await fs.readFile(path.join(dir, '..', '..', 'desktop', 'src-tauri', 'tauri.conf.json'), 'utf8'));
    if (conf && conf.version) return String(conf.version);
  } catch { /* not a repo checkout */ }
  return 'dev';
}

// Compose the ACP profile once, offline. A version string alone does not prove
// the harness is usable; this is the config round-trip the installer reports.
export async function verifyDeepseekConfig(runtime, dshHome, execPath = process.execPath) {
  await fs.mkdir(dshHome, { recursive: true, mode: 0o700 });
  return await new Promise((resolve, reject) => {
    const child = spawn(execPath, [runtime, '--profile', 'acp', '--dump-config'], { env: { ...process.env, DSH_HOME: dshHome }, stdio: ['ignore', 'pipe', 'pipe'] });
    let out = '', err = '';
    const timer = setTimeout(() => { child.kill('SIGKILL'); reject(new Error('DeepSeek Harness config round-trip timed out')); }, 60000);
    child.stdout.on('data', data => { out += data; });
    child.stderr.on('data', data => { err += data; });
    child.on('error', error => { clearTimeout(timer); reject(error); });
    child.on('exit', code => {
      clearTimeout(timer);
      if (code !== 0) return reject(new Error(`DeepSeek Harness config round-trip exited ${code}: ${String(err).slice(-300)}`));
      if (!out.includes('@deepseek-ai/dsh')) return reject(new Error('DeepSeek Harness ACP profile did not compose a config'));
      resolve(out);
    });
  });
}

// A version string alone does not prove the bundle loads. Boot Pi once offline
// against the managed home so a broken install fails at install time.
export async function verifyPiConfig(runtime, piHome, execPath = process.execPath) {
  await fs.mkdir(piHome, { recursive: true, mode: 0o700 });
  return await new Promise((resolve, reject) => {
    const child = spawn(execPath, [runtime, '--version'], {
      env: { ...process.env, PI_CODING_AGENT_DIR: piHome, PI_OFFLINE: '1', PI_SKIP_VERSION_CHECK: '1', PI_TELEMETRY: '0' },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let out = '';
    const timer = setTimeout(() => { child.kill('SIGKILL'); reject(new Error('Pi config round-trip timed out')); }, 60000);
    child.stdout.on('data', data => { out += data; });
    child.stderr.on('data', data => { out += data; });
    child.on('error', error => { clearTimeout(timer); reject(error); });
    child.on('exit', code => {
      clearTimeout(timer);
      if (code !== 0 || !/\d+\.\d+\.\d+/.test(out)) return reject(new Error(`Pi config round-trip exited ${code}: ${String(out).slice(-300)}`));
      resolve(out.trim());
    });
  });
}

// Provenance: the npm lockfile records the exact tarball the pinned package was
// resolved from. A version string can be present on a package assembled another
// way, so the installer requires a registry-resolved sha512 entry for the pin.
export async function readHarnessProvenance(prefix, packageName, pinnedVersion) {
  let lock;
  try { lock = JSON.parse(await fs.readFile(path.join(prefix, 'package-lock.json'), 'utf8')); }
  catch { return null; }
  const entry = lock?.packages?.['node_modules/' + packageName];
  if (!entry || entry.version !== pinnedVersion) return null;
  const integrity = String(entry.integrity || '');
  const resolved = String(entry.resolved || '');
  if (!/^sha512-/.test(integrity)) return null;
  let host = '';
  try { host = new URL(resolved).host; } catch { return null; }
  if (host !== 'registry.npmjs.org') return null;
  return { package: packageName, version: entry.version, resolved, integrity };
}

export const readDshProvenance = (prefix, pinnedVersion) =>
  readHarnessProvenance(prefix, '@deepseek-ai/dsh', pinnedVersion);
export const readPiProvenance = (prefix, pinnedVersion) =>
  readHarnessProvenance(prefix, '@earendil-works/pi-coding-agent', pinnedVersion);

// Record the verified provenance beside the managed harness home so which
// tarball a machine actually installed is auditable after the fact.
export async function writeHarnessProvenance(dataDir, dirName, provenance) {
  const dir = path.join(dataDir, 'harness-state', dirName);
  await fs.mkdir(dir, { recursive: true, mode: 0o700 });
  const file = path.join(dir, 'install-provenance.json');
  const temp = `${file}.${process.pid}.tmp`;
  const record = { ...provenance, node: process.version, verifiedAt: new Date().toISOString() };
  await fs.writeFile(temp, JSON.stringify(record, null, 2) + '\n', { mode: 0o600 });
  await fs.rename(temp, file);
  return file;
}

export const writeDshProvenance = (dataDir, provenance) => writeHarnessProvenance(dataDir, 'deepseek', provenance);
export const writePiProvenance = (dataDir, provenance) => writeHarnessProvenance(dataDir, 'pi', provenance);

// The managed install's private npm config, in the spirit of Pi's supply-chain
// hardening: exact pins, the official registry, and no audit/fund noise.
// min-release-age is deliberately NOT set — we pin exact versions and verify the
// resolved tarball, so a same-day pin bump must still install.
export function managedNpmrc() {
  return [
    '# SoluTerminal managed harness install (generated)',
    'save-exact=true',
    'audit=false',
    'fund=false',
    'registry=https://registry.npmjs.org/',
    '',
  ].join('\n');
}

export async function writeManagedNpmrc(prefix) {
  await fs.mkdir(prefix, { recursive: true, mode: 0o700 });
  const file = path.join(prefix, '.npmrc');
  await fs.writeFile(file, managedNpmrc(), { mode: 0o600 });
  return file;
}

export async function launch(argv = process.argv.slice(2)) {
  const provider = argv.shift();
  if (!['glm', 'deepseek', 'pi', 'pi-glm'].includes(provider)) throw new Error('Usage: launch.mjs glm|deepseek|pi|pi-glm [--data-dir directory]');
  const index = argv.indexOf('--data-dir');
  const dataDir = index < 0 ? path.join(os.homedir(), '.solu') : argv.splice(index, 2)[1];
  if (!dataDir) throw new Error('--data-dir requires a directory.');
  if (argv.includes('--help') || argv.includes('-h')) {
    const label = { glm: 'ZCode', deepseek: 'DeepSeek Harness', pi: 'Pi', 'pi-glm': 'Pi (GLM)' }[provider];
    const detail = {
      glm: 'Community zcode-app-cli distribution of ZCode, with runtime patches and terminal UI. --prompt TEXT runs a single prompt; --mode chooses build, edit, plan, or yolo.',
      deepseek: 'The SoluTerminal terminal client runs the official DSH ACP profile. Use /help, /model, /effort, /sessions, /resume ID, /new, or /exit.',
      pi: 'The official Pi coding harness (github.com/earendil-works/pi). Managed launches use a private config directory and run Pi on its DeepSeek provider. Mutating tools ask for approval; choose option 3 to remember a tool, and /allow lists or clears that memory. Use /model, /resume, /tree, /export, or /quit.',
      'pi-glm': 'The official Pi coding harness (github.com/earendil-works/pi). Managed launches use a private config directory and run Pi on its ZAI provider with your Coding Plan key. Mutating tools ask for approval; choose option 3 to remember a tool, and /allow lists or clears that memory. Use /model, /resume, /tree, /export, or /quit.',
    }[provider];
    console.log(`SoluTerminal ${label}\nRun in your project directory after adding and selecting an API account in Credentials.\n--install installs the runtime and terminal shortcut.\n${detail}\nTeam assignments are managed in Settings → Team.`);
    return 0;
  }
  if (argv.includes('--install')) {
    const child = spawn(process.execPath, [fileURLToPath(new URL('./install.mjs', import.meta.url)), provider, '--data-dir', dataDir], { stdio: 'inherit' });
    return await new Promise(resolve => child.on('exit', code => resolve(code ?? 1)));
  }
  const file = path.join(dataDir, 'harness-accounts', `${provider}.json`);
  const selected = selectedAccount(await readState(file), provider);
  const broker = selected.source === 'team' && selected.brokerOnly === true;
  const account = broker ? await loadedBrokerAccount(dataDir, provider) : await loadedAccount(file, provider, dataDir);
  try {
    return await launchAccount(provider, argv, dataDir, file, broker, account);
  } finally {
    if (broker) await releaseBrokerCapability(account);
  }
}

// The daemon counts every route capability against a fixed store until it is
// released or expires, so a launch hands its capability back however it ends.
// Best effort: a lost release is reclaimed once the store fills.
export async function releaseBrokerCapability(account, fetchImpl = fetch) {
  if (!/^[0-9a-f]{64}$/.test(account?.apiKey || '')) return;
  try {
    const response = await fetchImpl(new URL('/api/team-broker/capability/release', new URL(account.baseURL).origin), {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: account.apiKey }), signal: AbortSignal.timeout(5000),
    });
    await response.body?.cancel();
  } catch { /* the daemon may be gone; the store reclaims the slot later */ }
}

async function launchAccount(provider, argv, dataDir, file, broker, account) {
  const initial = fingerprint(account);
  const brokerInitial = broker ? fingerprint(await readBrokerMetadata(dataDir, provider)) : '';
  let env = cleanEnvironment(process.env);
  let command;
  if (provider === 'glm') {
    const runtime = path.join(runtimeRoot, 'zcode-terminal/node_modules/zcode-app-cli/vendor/zcode.cjs');
    await fs.access(runtime).catch(() => { throw new Error('ZCode is not installed. Run zcode --install, or use Install in Credentials.'); });
    await fs.access(path.join(path.dirname(runtime), 'node_modules/@zcode/tui/package.json')).catch(() => { throw new Error('ZCode terminal UI is missing. Run zcode --install.'); });
    await configureZcode(account, process.cwd(), undefined, file);
    if (!broker && fingerprint(selectedAccount(await readState(file), provider)) !== initial) throw new Error('API account changed during launch. Select it and retry.');
    env.ZAI_API_KEY = account.apiKey;
    env.ZCODE_STORAGE_DIR = path.join(dataDir, 'harness-state/zcode');
    command = [runtime, '--mode', 'build', ...argv];
  } else if (provider === 'deepseek') {
    if (!nodeSupportsDsh()) throw new Error('DeepSeek Harness needs Node 24.2 or newer (its CLI relies on import.meta.main). Update Node, then install or launch again.');
    env.SOLU_CLIENT_BUILD = await clientBuildVersion();
    const plan = deepseekPlan(account, dataDir, argv, env);
    await fs.access(plan.runtime).catch(() => { throw new Error('DeepSeek Harness is not installed. Run deepseek --install, or use Install in Credentials.'); });
    await verifyPinnedRuntime(plan.packageJson, PINS.deepseek, 'DeepSeek Harness');
    await fs.mkdir(plan.env.DSH_HOME, { recursive: true, mode: 0o700 });
    env = plan.env; command = plan.argv;
  } else if (provider === 'pi') {
    if (!nodeSupportsPi()) throw new Error('Pi needs Node 22.19 or newer. Update Node, then install or launch again.');
    const plan = piPlan(account, dataDir, argv, env);
    await fs.access(plan.runtime).catch(() => { throw new Error('Pi is not installed. Run pi --install, or use Install in Credentials.'); });
    await verifyPinnedRuntime(plan.packageJson, PINS.pi, 'Pi');
    await fs.mkdir(plan.env.PI_CODING_AGENT_DIR, { recursive: true, mode: 0o700 });
    await fs.mkdir(plan.env.PI_CODING_AGENT_SESSION_DIR, { recursive: true, mode: 0o700 });
    if (broker) await configurePiBroker(account, plan.env.PI_CODING_AGENT_DIR, provider);
    else await clearPiBroker(plan.env.PI_CODING_AGENT_DIR);
    env = plan.env; command = plan.argv;
  } else {
    if (!nodeSupportsPi()) throw new Error('Pi needs Node 22.19 or newer. Update Node, then install or launch again.');
    const plan = piGlmPlan(account, dataDir, argv, env);
    await fs.access(plan.runtime).catch(() => { throw new Error('Pi is not installed. Run pi-glm --install, or use Install in Credentials.'); });
    await verifyPinnedRuntime(plan.packageJson, PINS.pi, 'Pi');
    await fs.mkdir(plan.env.PI_CODING_AGENT_DIR, { recursive: true, mode: 0o700 });
    await fs.mkdir(plan.env.PI_CODING_AGENT_SESSION_DIR, { recursive: true, mode: 0o700 });
    if (broker) await configurePiBroker(account, plan.env.PI_CODING_AGENT_DIR, provider);
    else await clearPiBroker(plan.env.PI_CODING_AGENT_DIR);
    env = plan.env; command = plan.argv;
  }
  const grouped = process.platform !== 'win32';
  const child = spawn(process.execPath, command, { env, stdio: 'inherit', detached: grouped });
  const signalChild = signal => {
    try { if (grouped && child.pid) process.kill(-child.pid, signal); else child.kill(signal); }
    catch (error) { if (error.code !== 'ESRCH') throw error; }
  };
  let stopped = false;
  let killTimer;
  const stop = (signal = 'SIGTERM') => {
    if (stopped) return;
    stopped = true;
    signalChild(signal);
    killTimer = setTimeout(() => signalChild('SIGKILL'), 5000);
    killTimer.unref();
  };
  const timer = setInterval(async () => {
    try {
      if (broker) {
        if (fingerprint(await readBrokerMetadata(dataDir, provider)) === brokerInitial) return;
      } else if (fingerprint(await loadedAccount(file, provider, dataDir)) === initial) return;
    } catch { /* Removed, expired, unsealable or malformed assignments stop the old process. */ }
    if (!stopped) process.stderr.write('\nAPI account changed or team access expired. Restart the harness after selecting an account.\n');
    stop();
  }, 1000);
  const onTerm = () => stop();
  process.on('SIGTERM', onTerm);
  // Interactive Ctrl+C belongs to the harness (cancel turn / quit UI).
  const onInt = () => signalChild('SIGINT');
  process.on('SIGINT', onInt);
  try {
    return await new Promise((resolve, reject) => { child.on('error', reject); child.on('exit', code => resolve(code ?? 1)); });
  } finally {
    if (stopped) signalChild('SIGKILL');
    clearInterval(timer); clearTimeout(killTimer);
    process.off('SIGTERM', onTerm); process.off('SIGINT', onInt);
  }
}
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  launch().then(code => { process.exitCode = code; }).catch(error => { console.error(error.code === 'ENOENT' ? 'No managed API account saved. Open SoluTerminal → Credentials.' : error.message); process.exitCode = 1; });
}
