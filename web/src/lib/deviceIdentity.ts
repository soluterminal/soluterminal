// This browser's device identity for "approved devices" (daemon
// trusted_devices.go; docs/DESIGN-trusted-devices-and-private-relay.md).
//
// The key pair is a WebCrypto ECDSA P-256 key whose private half is
// non-extractable: page scripts can ask it to sign, but cannot read or send
// it anywhere, so nothing that sees the traffic can sign on this device's
// behalf after the page is closed. The pair lives in IndexedDB.
//
// Requests are signed only for daemons this device has enrolled with
// (deviceEnrolledFor below), so nothing changes for anyone who never turns
// approved devices on. Each daemon (a machine's wsl and win backends are two)
// keeps its own list of approved devices, so enrolment is per target.

const DB_NAME = 'soluterminal-device';
const STORE = 'keys';
const KEY_ID = 'device-p256';
const ENROLLED_KEY = 'soluterminal.enrolledMachines';
const SIG_VERSION = 'SOLU-DEVICE-V1';
const CODE_VERSION = 'SOLU-DEVICE-CODE-V1\n';
const CODE_VERSION_2 = 'SOLU-DEVICE-CODE-V2\n';
const PINNED_KEY = 'soluterminal.pinnedComputers';
const CROCKFORD = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';

export interface DeviceIdentity {
  id: string;
  spki: string; // base64 SPKI DER, as the daemon stores it
  code: string; // XXXX-XXXX-XXXX-XXXX, compared by the person when approving
  key: CryptoKeyPair;
}

export interface DeviceSignature {
  id: string;
  time: string;
  nonce: string;
  target: string;
  sig: string;
}

const enc = new TextEncoder();

function b64(bytes: Uint8Array): string {
  let s = '';
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s);
}

function b64url(bytes: Uint8Array): string {
  return b64(bytes).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

async function sha256(data: Uint8Array | string): Promise<Uint8Array> {
  const bytes = typeof data === 'string' ? enc.encode(data) : data;
  return new Uint8Array(await crypto.subtle.digest('SHA-256', bytes as BufferSource));
}

function hex(bytes: Uint8Array): string {
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

/** Same derivation as daemon deviceID: the id is a function of the key. */
export async function deviceIdFor(spki: string): Promise<string> {
  return 'dev_' + b64url(await sha256(spki.trim())).slice(0, 22);
}

/**
 * Same derivation as daemon deviceCode: 80 bits of a hash over this device's
 * key and, when known, the computer's identity key (V2). A relay that swaps
 * either key during enrolment makes the two screens show different codes.
 */
export async function deviceCodeFor(spki: string, computerSpki?: string): Promise<string> {
  const sum = await sha256(computerSpki
    ? CODE_VERSION_2 + spki.trim() + '\n' + computerSpki.trim()
    : CODE_VERSION + spki.trim());
  return crockford80(sum);
}

function crockford80(sum: Uint8Array): string {
  let n = 0n;
  for (const b of sum.slice(0, 10)) n = (n << 8n) | BigInt(b);
  let out = '';
  for (let i = 0; i < 16; i++) {
    if (i > 0 && i % 4 === 0) out += '-';
    out += CROCKFORD[Number((n >> BigInt(75 - 5 * i)) & 31n)];
  }
  return out;
}

export function newDeviceNonceV3(): string {
  return hex(crypto.getRandomValues(new Uint8Array(32)));
}

export function validDeviceNonceV3(nonce: string): boolean {
  return /^[0-9a-f]{64}$/.test(nonce);
}

export function validEnrolmentIdV3(id: string): boolean {
  return /^enr_[A-Za-z0-9_-]{21}[AQgw]$/.test(id);
}

export async function deviceCommitV3For(id: string, nonce: string): Promise<string> {
  if (!/^dev_[A-Za-z0-9_-]{22}$/.test(id) || !validDeviceNonceV3(nonce)) throw new Error('Invalid V3 commitment fields');
  return hex(await sha256(['SOLU-DEVICE-COMMIT-V3', id, nonce].join('\n')));
}

function canonicalSPKIBytes(spki: string): Uint8Array {
  try {
    const decoded = atob(spki);
    if (!/^[A-Za-z0-9+/]+={0,2}$/.test(spki) || btoa(decoded) !== spki) throw new Error();
    return Uint8Array.from(decoded, (c) => c.charCodeAt(0));
  } catch {
    throw new Error('Invalid canonical SPKI');
  }
}

export async function deviceSPKIHashFor(spki: string): Promise<string> {
  const der = canonicalSPKIBytes(spki);
  await crypto.subtle.importKey('spki', der as BufferSource, { name: 'ECDSA', namedCurve: 'P-256' }, false, ['verify']);
  return hex(await sha256(der));
}

export async function deviceCodeV3For(
  spki: string, computerSpki: string, enrolmentId: string, deviceNonce: string, daemonNonce: string,
): Promise<string> {
  canonicalSPKIBytes(spki);
  canonicalSPKIBytes(computerSpki);
  if (!validEnrolmentIdV3(enrolmentId) || !validDeviceNonceV3(deviceNonce) || !validDeviceNonceV3(daemonNonce)) {
    throw new Error('Invalid V3 comparison fields');
  }
  return crockford80(await sha256(['SOLU-DEVICE-CODE-V3', spki, computerSpki, enrolmentId, deviceNonce, daemonNonce].join('\n')));
}

export interface DeviceEnrolmentReply {
  id: string;
  status: 'committed' | 'pending' | 'approved' | 'failed';
  code?: string;
  codeVersion?: number;
  enrolmentId?: string;
  daemonIdentity?: string;
  daemonNonce?: string;
  verify?: { enrolmentId?: string; state: 'committed' | 'revealed' | 'failed'; code?: string };
  v3Verified?: { at: string; deviceSPKIHash: string; daemonSPKIHash: string };
}

function canonicalEscape(s: string): string {
  let out = '';
  for (const b of enc.encode(s)) {
    const c = String.fromCharCode(b);
    out += /[A-Za-z0-9\-_.~]/.test(c) ? c : '%' + b.toString(16).toUpperCase().padStart(2, '0');
  }
  return out;
}

/**
 * The query form a device signs (daemon canonicalDeviceQuery): keys sorted,
 * each key's values in order, everything outside A-Z a-z 0-9 - _ . ~ as %XX.
 * The cloud re-encodes queries before forwarding, so raw bytes can't match.
 */
export function canonicalDeviceQuery(rawQuery: string): string {
  if (!rawQuery) return '';
  const byKey = new Map<string, string[]>();
  for (const [k, v] of new URLSearchParams(rawQuery)) {
    if (!byKey.has(k)) byKey.set(k, []);
    byKey.get(k)!.push(v);
  }
  const keys = [...byKey.keys()].sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
  return keys.flatMap((k) => byKey.get(k)!.map((v) => `${canonicalEscape(k)}=${canonicalEscape(v)}`)).join('&');
}

/** The exact bytes a device signs (daemon deviceSigMessage). */
export async function deviceSigMessage(
  target: string,
  method: string,
  path: string,
  rawQuery: string,
  unix: string,
  nonce: string,
  body: Uint8Array,
): Promise<string> {
  const query = canonicalDeviceQuery(rawQuery);
  const full = query ? `${path}?${query}` : path;
  return [SIG_VERSION, target, method.toUpperCase(), full, unix, nonce, hex(await sha256(body))].join('\n');
}

function openDb(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, 1);
    req.onupgradeneeded = () => req.result.createObjectStore(STORE);
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

async function idbGet<T>(key: string): Promise<T | undefined> {
  const db = await openDb();
  try {
    return await new Promise<T | undefined>((resolve, reject) => {
      const req = db.transaction(STORE, 'readonly').objectStore(STORE).get(key);
      req.onsuccess = () => resolve(req.result as T | undefined);
      req.onerror = () => reject(req.error);
    });
  } finally {
    db.close();
  }
}

async function idbPut(key: string, value: unknown): Promise<void> {
  const db = await openDb();
  try {
    await new Promise<void>((resolve, reject) => {
      const tx = db.transaction(STORE, 'readwrite');
      tx.objectStore(STORE).put(value, key);
      tx.oncomplete = () => resolve();
      tx.onerror = () => reject(tx.error);
    });
  } finally {
    db.close();
  }
}

let identity: Promise<DeviceIdentity> | null = null;

/** This browser's identity, created on first use. */
export function deviceIdentity(): Promise<DeviceIdentity> {
  identity ??= (async () => {
    let key = await idbGet<CryptoKeyPair>(KEY_ID).catch(() => undefined);
    if (!key) {
      key = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
      await idbPut(KEY_ID, key);
    }
    const spki = b64(new Uint8Array(await crypto.subtle.exportKey('spki', key.publicKey)));
    return { id: await deviceIdFor(spki), spki, code: await deviceCodeFor(spki), key };
  })();
  identity.catch(() => {
    identity = null;
  });
  return identity;
}

/**
 * Sign one request for target (`<machineId>` or `<machineId>/<backendId>`
 * over the relay, `local/<daemon key id>` at this computer). clockOffsetMs
 * corrects for a daemon clock that differs from this one (WSL after sleep).
 */
export async function signDeviceRequest(
  target: string,
  method: string,
  path: string,
  rawQuery: string,
  body: Uint8Array,
  clockOffsetMs = 0,
): Promise<DeviceSignature> {
  const me = await deviceIdentity();
  const time = String(Math.floor((Date.now() + clockOffsetMs) / 1000));
  const nonce = b64url(crypto.getRandomValues(new Uint8Array(16)));
  const msg = await deviceSigMessage(target, method, path, rawQuery, time, nonce, body);
  // WebCrypto ECDSA returns r||s (P1363), the form the daemon verifies.
  const sig = new Uint8Array(
    await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, me.key.privateKey, enc.encode(msg)),
  );
  return { id: me.id, time, nonce, target, sig: b64url(sig) };
}

export function deviceSignatureHeaders(s: DeviceSignature): Record<string, string> {
  return {
    'X-Solu-Device': s.id,
    'X-Solu-Device-Time': s.time,
    'X-Solu-Device-Nonce': s.nonce,
    'X-Solu-Device-Target': s.target,
    'X-Solu-Device-Sig': s.sig,
  };
}

/** Append the signature to a WebSocket/EventSource URL (they cannot set headers). */
export function withDeviceSignatureQuery(url: string, s: DeviceSignature): string {
  const params = `solu_dev=${encodeURIComponent(s.id)}&solu_dev_t=${s.time}&solu_dev_n=${encodeURIComponent(s.nonce)}` +
    `&solu_dev_to=${encodeURIComponent(s.target)}&solu_dev_sig=${encodeURIComponent(s.sig)}`;
  return url + (url.includes('?') ? '&' : '?') + params;
}

const machineBackends = new Map<string, string[]>();

/** Record a machine's live backends (cloud /api/machines) for default routing. */
export function noteMachineBackends(machineId: string, backends: string[]): void {
  machineBackends.set(machineId, backends);
}

/**
 * Relay routing prefix → daemon target. `/m/<machine>/b/<backend>` names the
 * backend; a bare `/m/<machine>` goes wherever the cloud routes it (the first
 * live of wsl, win, host — cloud Machine.getTunnel), and daemons with a backend
 * id only accept a signature that names it. null for local routing.
 */
export function relayTarget(prefix: string, backendPrefix: string): string | null {
  const m = /^\/m\/([^/]+)$/.exec(prefix);
  if (!m) return null;
  const machine = decodeURIComponent(m[1]);
  const b = /^\/b\/([^/]+)$/.exec(backendPrefix);
  if (b) return `${machine}/${decodeURIComponent(b[1])}`;
  const live = machineBackends.get(machine) ?? [];
  const routed = ['wsl', 'win', 'host'].find((id) => live.includes(id));
  return routed ? `${machine}/${routed}` : machine;
}

function readEnrolled(): Record<string, true> {
  try {
    const raw = localStorage.getItem(ENROLLED_KEY);
    return raw ? (JSON.parse(raw) as Record<string, true>) : {};
  } catch {
    return {};
  }
}

/** Whether this device has enrolled with the daemon behind target. */
export function deviceEnrolledFor(target: string): boolean {
  return !!readEnrolled()[target];
}

/** Remember that the daemon behind target approved this device. */
export function markDeviceEnrolled(target: string): void {
  try {
    localStorage.setItem(ENROLLED_KEY, JSON.stringify({ ...readEnrolled(), [target]: true }));
  } catch {
    // Private mode / blocked storage: the device keeps working unsigned.
  }
}

/** The relay path prefix that reaches target's daemon. */
export function relayPrefixFor(target: string): string {
  const [machine, backend] = target.split('/');
  return `/m/${encodeURIComponent(machine)}${backend ? `/b/${encodeURIComponent(backend)}` : ''}`;
}

/** Remember the computer identity behind target, pinned at enrolment. */
export function pinComputerIdentity(target: string, spki: string): void {
  try {
    const raw = localStorage.getItem(PINNED_KEY);
    const all = raw ? (JSON.parse(raw) as Record<string, string>) : {};
    localStorage.setItem(PINNED_KEY, JSON.stringify({ ...all, [target]: spki }));
  } catch {
    // Blocked storage: the private channel stays unavailable for this device.
  }
}

/**
 * Forget target's computer: its pinned identity and this device's enrolment.
 * The next approval compares codes afresh before anything is pinned again.
 */
export function forgetComputer(target: string): void {
  try {
    const pinned = JSON.parse(localStorage.getItem(PINNED_KEY) ?? '{}') as Record<string, string>;
    delete pinned[target];
    localStorage.setItem(PINNED_KEY, JSON.stringify(pinned));
    const enrolled = readEnrolled();
    delete enrolled[target];
    localStorage.setItem(ENROLLED_KEY, JSON.stringify(enrolled));
  } catch {
    // Blocked storage: nothing was kept to forget.
  }
}

/** The computer identity pinned for target, if any. */
export function pinnedComputerIdentity(target: string): string | null {
  try {
    const raw = localStorage.getItem(PINNED_KEY);
    return raw ? ((JSON.parse(raw) as Record<string, string>)[target] ?? null) : null;
  } catch {
    return null;
  }
}
