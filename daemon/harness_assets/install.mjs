#!/usr/bin/env node
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';
import { runtimeRoot, nodeSupportsDsh, DSH_NODE_FLOOR, verifyDshPrivacy, prepareDshLock, dshInstallVerified, writeDshVerified, clearDshVerified, harnessTreeMatchesLock, ensureDshSpawnHelper, nodeSupportsPi, verifyDeepseekConfig, verifyPiConfig, readDshProvenance, readPiProvenance, writeDshProvenance, writePiProvenance, writeManagedNpmrc, preparePiLock, piTreeMatchesLock, piInstallVerified, writePiVerified, clearPiVerified, shortcutDisposition, shortcutDataDir, renderManagedShortcut } from './launch.mjs';
import { PINS } from './pins.mjs';

const [provider, ...args] = process.argv.slice(2);
if (!['glm', 'deepseek', 'pi', 'pi-glm'].includes(provider)) throw new Error('Choose glm, deepseek, pi, or pi-glm.');
const dataDir = args[0] === '--data-dir' && args[1] ? args[1] : path.join(os.homedir(), '.solu');
const versionMatches = async (file, wanted, flag = '--version') => {
  try { const result = await promisify(execFile)(process.execPath, [file, flag], { timeout: 15000 }); return result.stdout.trim() === wanted; }
  catch { return false; }
};
// Windows ships npm as npm.cmd, which spawn cannot start without a shell. Run
// the npm-cli.js bundled beside this Node instead, so no shell quoting is needed.
const npm = argv => {
  if (process.platform !== 'win32') return ['npm', argv];
  const cli = path.join(path.dirname(process.execPath), 'node_modules', 'npm', 'bin', 'npm-cli.js');
  return [process.execPath, [cli, ...argv]];
};
const run = (command, argv, cwd) => new Promise((resolve, reject) => {
  const child = spawn(command, argv, { cwd, stdio: 'inherit', shell: false });
  child.on('error', reject); child.on('exit', code => code === 0 ? resolve() : reject(new Error(`${command} exited ${code}`)));
});
const capture = (command, argv, cwd, timeoutMs = 120000) => new Promise(resolve => {
  const child = spawn(command, argv, { cwd, stdio: ['ignore', 'pipe', 'pipe'], shell: false });
  let out = '';
  child.stdout.on('data', data => { out += data; });
  child.stderr.on('data', data => { out += data; });
  const timer = setTimeout(() => { child.kill('SIGKILL'); resolve({ code: null, out: 'timed out' }); }, timeoutMs); timer.unref?.();
  child.on('error', error => { clearTimeout(timer); resolve({ code: 1, out: String(error?.message ?? error) }); });
  child.on('exit', code => { clearTimeout(timer); resolve({ code: code ?? 1, out }); });
});
// `npm audit signatures` checks every package installed in the prefix, not
// only the top-level one. Any failure refuses the install, including an
// unreachable audit endpoint (retry later).
const verifySignatures = async (prefix, label) => {
  const result = await capture(...npm(['audit', 'signatures']), prefix);
  const lines = String(result.out).split('\n').map(line => line.trim()).filter(Boolean);
  const tail = lines.slice(-1)[0] || '';
  if (result.code !== 0) throw new Error(`${label} install refused: npm registry signature verification failed: ${(tail || 'npm audit signatures failed').slice(0, 200)}`);
  const summary = lines.find(line => /verified registry signatures/.test(line)) || tail;
  console.log(`${label} registry signatures verified${summary ? ` (${summary})` : ''}.`);
};
// withInstallLock runs fn holding <dir>/.install.lock. The lock is created by
// hard-linking a file that already holds this process's pid, so it never
// exists empty: another installer always reads a real pid. A lock whose pid is
// gone is stale and broken (only while it still holds that pid); a live one is
// waited for, up to 15 minutes.
const withInstallLock = async (dir, fn) => {
  await fs.mkdir(dir, { recursive: true, mode: 0o700 });
  const lock = path.join(dir, '.install.lock');
  const mine = String(process.pid);
  const temp = `${lock}.${mine}.tmp`;
  await fs.writeFile(temp, mine, { mode: 0o600 });
  const holder = async () => String(await fs.readFile(lock, 'utf8').catch(() => '')).trim();
  const deadline = Date.now() + 15 * 60 * 1000;
  try {
    for (;;) {
      try { await fs.link(temp, lock); break; } catch (error) { if (error.code !== 'EEXIST') throw error; }
      const seen = await holder();
      const pid = Number(seen);
      let alive = !(pid > 0); // unreadable or unexpected content: treat as held
      if (pid > 0) { try { process.kill(pid, 0); alive = true; } catch (err) { alive = err.code === 'EPERM'; } }
      if (!alive && (await holder()) === seen) { await fs.rm(lock, { force: true }); continue; }
      if (Date.now() > deadline) throw new Error('Another install is still running in this folder. Try again when it finishes.');
      await new Promise(resolve => setTimeout(resolve, 1000));
    }
  } finally { await fs.rm(temp, { force: true }); }
  try { return await fn(); } finally { if ((await holder()) === mine) await fs.rm(lock, { force: true }); }
};
await fs.mkdir(runtimeRoot, { recursive: true, mode: 0o700 });
if (provider === 'deepseek') {
  // dsh 0.2.2's engines floor; its CLI is also a no-op without
  // `import.meta.main`. Fail the install rather than leave a launcher that
  // hangs on a handshake that never comes.
  if (!nodeSupportsDsh()) throw new Error(`DeepSeek Harness needs Node ${DSH_NODE_FLOOR} or newer. Update Node and rerun the installer.`);
  const prefix = path.join(runtimeRoot, 'deepseek');
  const runtime = path.join(prefix, 'node_modules/@deepseek-ai/dsh/lib/bin.js');
  const PIN = PINS.deepseek;
  await writeManagedNpmrc(prefix);
  // dsh 0.2.2 resolves 724 packages, five with install scripts. Like Pi, they
  // come only from SoluTerminal's own lockfile (dsh-lock.json, hash-pinned in
  // pins.mjs), installed by `npm ci --ignore-scripts`: npm checks every
  // tarball's integrity hash, and no package script runs. node-pty and koffi
  // load their shipped prebuilds; the one script effect dsh needs (the macOS
  // spawn-helper's mode) is applied below, against a pinned hash.
  await withInstallLock(prefix, async () => {
    const lock = await prepareDshLock(prefix, await fs.readFile(new URL('./dsh-lock.json', import.meta.url)));
    let provenance = await readDshProvenance(prefix, PIN);
    // Marker and tree first: an unverified bin.js is never executed.
    const reuse = (await dshInstallVerified(prefix)) && (await harnessTreeMatchesLock(prefix, lock)) && !!provenance;
    if (!reuse) {
      // A launch refuses DeepSeek Harness until every check below has passed.
      await clearDshVerified(prefix);
      await run(...npm(['ci', '--prefix', prefix, '--ignore-scripts', '--no-audit', '--no-fund']));
      provenance = await readDshProvenance(prefix, PIN);
    }
    try {
      if (!provenance) throw new Error('DeepSeek Harness install could not be verified against npm provenance (package-lock.json is missing or does not resolve the pinned version from registry.npmjs.org).');
      if (!(await harnessTreeMatchesLock(prefix, lock))) throw new Error('DeepSeek Harness install refused: the installed packages do not match SoluTerminal\'s lockfile.');
      console.log(`DeepSeek Harness ${provenance.version} provenance: ${provenance.resolved} ${provenance.integrity.slice(0, 24)}…`);
      await ensureDshSpawnHelper(prefix, lock);
      // Every package in the tree must carry a valid npm registry signature.
      await verifySignatures(prefix, 'DeepSeek Harness');
      // A version string does not prove the harness is usable: compose the ACP
      // profile once, offline, so a broken install fails here, not at launch.
      await verifyDeepseekConfig(runtime, path.join(dataDir, 'harness-state', 'deepseek'), process.execPath);
      // Our privacy rows must exist and be off in this version's pane and
      // workflow profiles; a renamed row would leave the patch unapplied. A
      // row finding is deterministic, so it clears the marker even on a reused
      // tree; a timeout or spawn error does not.
      try { await verifyDshPrivacy(runtime, path.join(dataDir, 'harness-state', 'deepseek'), process.execPath); }
      catch (error) { if (error.privacyRows) await clearDshVerified(prefix); throw error; }
    } catch (error) {
      // A tree that failed a check must not run. A previously verified tree we
      // reused stays (a registry outage does not uninstall a working harness).
      if (!reuse) {
        await clearDshVerified(prefix);
        await fs.rm(path.join(prefix, 'node_modules'), { recursive: true, force: true });
      }
      throw error;
    }
    await writeDshProvenance(dataDir, provenance);
    await writeDshVerified(prefix);
  });
  console.log('DeepSeek Harness config round-trip verified (official ACP profile); session log and plugin list off in the pane and workflow profiles.');
// The pi-glm row shares the Pi runtime binary with the pi row: both install
// (and verify) the same pinned package into the same prefix. Their managed
// homes and credentials stay separate at launch time.
} else if (provider === 'pi' || provider === 'pi-glm') {
  if (process.platform === 'win32') throw new Error('Install this managed harness on your SoluTerminal WSL backend.');
  // Pi's published engines floor. It does not need install scripts, so the
  // managed install passes --ignore-scripts like Pi's own documented install.
  if (!nodeSupportsPi()) throw new Error('Pi needs Node 22.19 or newer. Update Node and rerun the installer.');
  const prefix = path.join(runtimeRoot, 'pi');
  const runtime = path.join(prefix, 'node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js');
  const PIN = PINS.pi;
  await writeManagedNpmrc(prefix);
  // Pi 1.5 ships no shrinkwrap, so its dependencies come from SoluTerminal's
  // own lockfile (pi-lock.json, hash-pinned in pins.mjs): `npm ci` installs
  // exactly that tree, verifying every tarball's integrity hash, or fails.
  // pi and pi-glm install into this one prefix; one install at a time, so
  // neither's cleanup or npm ci can touch a tree the other just verified.
  await withInstallLock(prefix, async () => {
    const lock = await preparePiLock(prefix, await fs.readFile(new URL('./pi-lock.json', import.meta.url)));
    let provenance = await readPiProvenance(prefix, PIN);
    // Marker and tree first: an unverified cli.js is never executed.
    const reuse = (await piInstallVerified(prefix)) && (await piTreeMatchesLock(prefix, lock)) && !!provenance && (await versionMatches(runtime, PIN));
    if (!reuse) {
      // A launch refuses Pi until every check below has passed for this tree.
      await clearPiVerified(prefix);
      await run(...npm(['ci', '--prefix', prefix, '--ignore-scripts', '--no-audit', '--no-fund']));
      provenance = await readPiProvenance(prefix, PIN);
    }
    try {
      if (!provenance) throw new Error('Pi install could not be verified against npm provenance (package-lock.json is missing or does not resolve the pinned version from registry.npmjs.org).');
      if (!(await piTreeMatchesLock(prefix, lock))) throw new Error('Pi install refused: the installed packages do not match SoluTerminal\'s lockfile.');
      console.log(`Pi ${provenance.version} provenance: ${provenance.resolved} ${provenance.integrity.slice(0, 24)}…`);
      // Every package in the tree, not only Pi itself, must carry a valid npm
      // registry signature.
      await verifySignatures(prefix, 'Pi');
      // A version string does not prove the bundle loads: boot it once, offline.
      await verifyPiConfig(runtime, path.join(dataDir, 'harness-state', 'pi'), process.execPath);
    } catch (error) {
      // A tree that failed a check must not run. A previously verified tree we
      // reused stays (a registry outage does not uninstall a working Pi).
      if (!reuse) {
        await clearPiVerified(prefix);
        await fs.rm(path.join(prefix, 'node_modules'), { recursive: true, force: true });
      }
      throw error;
    }
    await writePiProvenance(dataDir, provenance);
    await writePiVerified(prefix);
  });
  console.log('Pi config round-trip verified (offline boot).');
} else {
  if (process.platform !== 'linux' || process.arch !== 'x64') throw new Error('The managed ZCode installer currently supports Linux x64, including WSL.');
  // Z.ai's desktop runtime omits the terminal UI and its bridge. Use the
  // matched, pinned community distribution, including its documented patches.
  const adapterRoot = path.join(runtimeRoot, 'zcode-terminal');
  await writeManagedNpmrc(adapterRoot);
  const adapterPackage = path.join(adapterRoot, 'node_modules/zcode-app-cli');
  let adapterVersion;
  try { adapterVersion = JSON.parse(await fs.readFile(path.join(adapterPackage, 'package.json'), 'utf8')).version; } catch {}
  if (adapterVersion !== '3.11.2-22') {
    await run(...npm(['install', '--prefix', adapterRoot, '--save-exact', '--no-audit', '--no-fund', 'zcode-app-cli@3.11.2-22']));
  }
  console.log('ZCode installed from the community zcode-app-cli distribution, including its ZCode runtime patches and terminal UI.');
}
// A shortcut is shared by every data dir on the machine. It may be replaced
// only when it is ours and either points at this data dir or this is the
// default data dir; a side-by-side install never repoints another dir's command.
const binDir = path.join(os.homedir(), '.local/bin');
await fs.mkdir(binDir, { recursive: true });
const shortcut = path.join(binDir, { glm: 'zcode', deepseek: 'deepseek', pi: 'pi', 'pi-glm': 'pi-glm' }[provider]);
const launcher = fileURLToPath(new URL('./launch.mjs', import.meta.url));
const resolver = fileURLToPath(new URL('./harness-shortcut.sh', import.meta.url));
const script = renderManagedShortcut({ resolver, node: process.execPath, launcher, provider, dataDir });
let existing = null;
try { existing = await fs.readFile(shortcut, 'utf8'); } catch (error) { if (error.code !== 'ENOENT') throw error; }
const disposition = shortcutDisposition(existing, dataDir);
if (disposition === 'create' || disposition === 'replace') { await fs.writeFile(shortcut, script, { mode: 0o700 }); await fs.chmod(shortcut, 0o700); }
else if (disposition === 'retain') console.log(`Existing ${shortcut} retained. Use the launch button in Credentials.`);
else console.log(`Existing ${shortcut} left as-is (owned by ${shortcutDataDir(existing) || 'another data dir'}). This install keeps its launcher at ${launcher}.`);
console.log(`${({ glm: 'ZCode', deepseek: 'DeepSeek Harness', pi: 'Pi', 'pi-glm': 'Pi (GLM)' })[provider]} installed. Add/select an API account in SoluTerminal → Credentials, then launch it.`);
