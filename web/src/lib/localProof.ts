// Approved devices at this computer (daemon human_proof.go).
//
// When the daemon can't tell which program is calling (a Windows browser or
// the desktop app reaching a WSL daemon, macOS, the desktop app's router), it
// takes a request as the person's only if an approved device signed it. This
// browser signs with its device key (lib/deviceIdentity.ts) for the target
// the daemon names, `local/<daemon key id>`, once that daemon approved it.
// Each daemon keeps its own list, so in the desktop app's Dual mode the wsl
// and win backends are approved separately.
//
// The status is fetched directly (not through api.ts, which imports this
// file): local daemons need no token.

import {
  deviceIdentity,
  deviceEnrolledFor,
  deviceSignatureHeaders,
  forgetComputer,
  markDeviceEnrolled,
  signDeviceRequest,
  type DeviceEnrolmentReply,
} from './deviceIdentity';
import { beginDeviceEnrolmentV3 } from './deviceEnrolment';
import { isEmbedded } from '../embed';
import { isTauri } from '../voice';

export type LocalProofMode = 'off' | 'report' | 'require';

export interface LocalProofStatus {
  mode: LocalProofMode;
  effective: LocalProofMode;
  target: string;
  serverTime: number;
  envOff: boolean;
  lowered?: string;
  /**
   * Devices approved without an approved device's proof (the first one, or
   * any approved while SOLU_HUMAN_PROOF=off), and removals made by such a
   * device or one it approved ("removed"). Shown until acknowledged.
   */
  notices?: { id: string; name: string; at: string; via: 'first-device' | 'env-recovery' | 'removed'; removed?: string }[];
  approvedDevices: number;
  canBootstrap: boolean;
  device?: 'approved' | 'pending' | 'committed' | 'failed' | '';
  unproven?: { count: number; at: string; action: string };
}

const TARGETS_KEY = 'soluterminal.localProofTargets';
const OFFSETS_KEY = 'soluterminal.localProofClock';
// The daemon's clock minus this one, per target. Kept across reloads so the
// first signature after one (often the terminal socket) is already right.
const offsets = new Map<string, number>(Object.entries(readJSON<Record<string, number>>(OFFSETS_KEY)));
const statuses = new Map<string, LocalProofStatus>();
const listeners = new Set<() => void>();

function readJSON<T extends object>(key: string): T {
  try {
    return JSON.parse(localStorage.getItem(key) ?? '{}') as T;
  } catch {
    return {} as T;
  }
}

function readTargets(): Record<string, string> {
  return readJSON<Record<string, string>>(TARGETS_KEY);
}

function noteOffset(target: string, offset: number): void {
  offsets.set(target, offset);
  try {
    localStorage.setItem(OFFSETS_KEY, JSON.stringify(Object.fromEntries(offsets)));
  } catch {
    // Blocked storage: measured again after a reload.
  }
}

function writeTarget(backendPrefix: string, target: string | null): void {
  try {
    const all = readTargets();
    if (target) all[backendPrefix] = target;
    else delete all[backendPrefix];
    localStorage.setItem(TARGETS_KEY, JSON.stringify(all));
  } catch {
    // Blocked storage: this browser stays unsigned, as before.
  }
}

/** The local target this browser signs for on backendPrefix, once approved there. */
export function localProofTarget(backendPrefix: string): string | null {
  if (typeof crypto === 'undefined' || !crypto.subtle) return null;
  const target = readTargets()[backendPrefix];
  return target && deviceEnrolledFor(target) ? target : null;
}

export function localClockOffset(target: string): number {
  return offsets.get(target) ?? 0;
}

/**
 * Make sure the daemon's clock is known before signing for target: a
 * signature outside its 60 s window is no proof, and a socket keeps the
 * standing it opened with.
 */
export async function ensureLocalClock(backendPrefix: string, target: string): Promise<void> {
  if (offsets.has(target)) return;
  await refreshLocalProof(backendPrefix).catch(() => null);
}

/** What a local socket was signed with: a target (or none) and the clock. */
export interface LocalSocketStanding {
  target: string | null;
  offset: number;
}

export function localSocketStanding(backendPrefix: string): LocalSocketStanding {
  const target = localProofTarget(backendPrefix);
  return { target, offset: target ? localClockOffset(target) : 0 };
}

/**
 * Whether a socket opened with `then` should be reopened now: this browser
 * was approved or removed since, or the clock it signed with was off by more
 * than the daemon accepts.
 */
export function localSocketStandingChanged(then: LocalSocketStanding, now: LocalSocketStanding): boolean {
  if (then.target !== now.target) return true;
  return now.target !== null && Math.abs(now.offset - then.offset) > 30_000;
}

export function localProofStatus(backendPrefix: string): LocalProofStatus | null {
  return statuses.get(backendPrefix) ?? null;
}

export function onLocalProofChange(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/**
 * Fetch this browser's standing with the daemon behind backendPrefix. null
 * for an older daemon without the endpoint.
 */
export async function refreshLocalProof(backendPrefix: string): Promise<LocalProofStatus | null> {
  const me = await deviceIdentity();
  const res = await fetch(`${backendPrefix}/api/human-proof?device=${encodeURIComponent(me.id)}`, { cache: 'no-store' });
  if (!res.ok) return null;
  const status = (await res.json()) as LocalProofStatus;
  noteOffset(status.target, status.serverTime - Date.now());
  const known = readTargets()[backendPrefix];
  if (status.device === 'approved') {
    if (known && known !== status.target) forgetComputer(known);
    writeTarget(backendPrefix, status.target);
    markDeviceEnrolled(status.target);
  } else if (known) {
    // Removed, or the daemon's identity changed: stop signing for it.
    forgetComputer(known);
    writeTarget(backendPrefix, null);
  }
  statuses.set(backendPrefix, status);
  for (const l of listeners) l();
  return status;
}

/** What this browser is called in the device list. */
export function thisDeviceName(): string {
  const ua = typeof navigator === 'undefined' ? '' : navigator.userAgent;
  const os = /Windows/.test(ua) ? 'Windows' : /Android/.test(ua) ? 'Android' : /iPhone|iPad/.test(ua) ? 'iOS'
    : /Mac OS X/.test(ua) ? 'macOS' : /Linux/.test(ua) ? 'Linux' : 'a device';
  if (isEmbedded()) return `Shinoque Code on ${os}`;
  if (isTauri()) return `SoluTerminal app on ${os}`;
  const app = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome'
    : /Safari\//.test(ua) ? 'Safari' : 'Browser';
  return `${app} on ${os}`;
}

async function postJSON(url: string, body: string, headers: Record<string, string> = {}): Promise<Response> {
  return fetch(url, { method: 'POST', headers: { 'content-type': 'application/json', ...headers }, body });
}

async function errorText(res: Response): Promise<string> {
  try {
    const b = (await res.json()) as { message?: string; error?: string };
    return b.message ?? b.error ?? `HTTP ${res.status}`;
  } catch {
    return `HTTP ${res.status}`;
  }
}

/**
 * Approve this browser as the computer's first device. Only works while no
 * device is approved there; the request is signed with the key it approves.
 */
export async function approveThisBrowserFirst(backendPrefix: string): Promise<void> {
  const status = await refreshLocalProof(backendPrefix);
  if (!status) throw new Error('This version of SoluTerminal does not support approved devices here.');
  const me = await deviceIdentity();
  const path = '/api/human-proof/bootstrap';
  const body = JSON.stringify({ name: thisDeviceName(), publicKey: me.spki });
  const sig = await signDeviceRequest(status.target, 'POST', path, '', new TextEncoder().encode(body), localClockOffset(status.target));
  const res = await postJSON(backendPrefix + path, body, deviceSignatureHeaders(sig));
  if (!res.ok) throw new Error(await errorText(res));
  await refreshLocalProof(backendPrefix);
}

/**
 * Ask to approve this browser from a device that already is: enrol with this
 * browser's key, then V3 commit/reveal. Returns the V3 code this browser
 * computed itself (never one the daemon sent), or '' when it was already
 * approved.
 */
export async function askToApproveThisBrowser(backendPrefix: string): Promise<string> {
  const me = await deviceIdentity();
  const send = async (path: string, body: unknown): Promise<DeviceEnrolmentReply> => {
    const res = await postJSON(backendPrefix + path, JSON.stringify(body));
    if (!res.ok) throw new Error(await errorText(res));
    return (await res.json()) as DeviceEnrolmentReply;
  };
  const comparison = await beginDeviceEnrolmentV3(me,
    (commit) => send('/api/remote-devices/enroll', { name: thisDeviceName(), publicKey: me.spki, codeVersion: 3, commit }),
    (id, enrolmentId, nonce) => send('/api/remote-devices/enroll/reveal', { id, enrolmentId, nonce }));
  await refreshLocalProof(backendPrefix);
  if (comparison.device.status === 'approved') return '';
  if (comparison.codeVersion !== 3) throw new Error('This computer did not offer a V3 code to compare.');
  return comparison.code;
}

/** Whether a URL is a pane terminal socket: the one stream whose caller matters. */
export function isPaneSocketUrl(url: string): boolean {
  try {
    return /\/api\/panes\/[^/]+\/io$/.test(new URL(url, 'http://local.invalid/').pathname);
  } catch {
    return false;
  }
}
