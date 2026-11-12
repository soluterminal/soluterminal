// Desktop "phone access" (cloud-pairing) status + pairing API.
//
// This is the IN-APP onboarding surface that lives INSIDE the Tauri desktop
// shell (served by the local daemon at 127.0.0.1:8787). It is NOT the
// browser-over-relay claim flow (that's CloudGate.tsx, the phone/web path) —
// this module only drives the header chip, the product banner, and the
// in-window pair panel, all of which are gated on the desktop shell via
// isTauri() so they NEVER render on the phone/relay web.
//
// Daemon contract (same-origin GET/POST, no machine prefix — these are the
// daemon's OWN cloud-pairing endpoints, not relayed):
//   GET  /api/cloud/status      → CloudStatus
//   POST /api/cloud/pair/start  → { code, expiresAt } (idempotent)
//   POST /api/cloud/pair/cancel → 200, empty body
//   POST /api/cloud/disconnect  → 200, empty body (403 over the relay)

import { useEffect, useRef, useState } from 'react';
import { isTauri } from './voice';
import { noteRelayConfigured } from './lib/relayConfigured';

export type RelayState = 'unconfigured' | 'offline' | 'connecting' | 'online';

export interface CloudPairing {
  active: boolean;
  code?: string;
  expiresAt?: string; // RFC3339
}

export interface CloudStatus {
  configured: boolean;
  hasCreds: boolean;
  credsInvalid: boolean;
  claimCorrupt: boolean;
  relayState: RelayState;
  ownerEmail?: string;
  pairing?: CloudPairing;
}

export interface PairStartResponse {
  code: string;
  expiresAt?: string; // RFC3339
}

// Re-exported so callers gate on a single import. True only inside the Tauri
// desktop webview (window.__SOLU_TAURI__ set by lib.rs init_script); a browser
// tab / the phone PWA returns false and never mounts these surfaces.
export function isDesktopShell(): boolean {
  return isTauri();
}

async function jsonOrThrow<T>(r: Response): Promise<T> {
  if (!r.ok) {
    const body = (await r.text().catch(() => '')).trim();
    // Attach the HTTP status so callers can branch on it (e.g. 403 = the
    // request was relay-forwarded and the daemon refused a local-only action).
    const err = new Error(body || `${r.status} ${r.statusText}`) as Error & { status?: number };
    err.status = r.status;
    throw err;
  }
  if (r.status === 204) return undefined as T;
  // pair/cancel and disconnect answer 200 with no body: that is success, not
  // a response to parse. (r.json() rejects on an empty body, which used to
  // report a Disconnect that had worked as "Couldn't disconnect".)
  const text = await r.text();
  return (text.trim() ? JSON.parse(text) : undefined) as T;
}

export const cloudPairApi = {
  status: (signal?: AbortSignal): Promise<CloudStatus> =>
    fetch('/api/cloud/status', { signal }).then(jsonOrThrow<CloudStatus>),
  pairStart: (): Promise<PairStartResponse> =>
    fetch('/api/cloud/pair/start', { method: 'POST' }).then(jsonOrThrow<PairStartResponse>),
  pairCancel: (): Promise<void> =>
    fetch('/api/cloud/pair/cancel', { method: 'POST' }).then(jsonOrThrow<void>),
  // Forget this machine's cloud connection: deletes the daemon's claim.json and
  // reverts status to "not set up". The escape hatch when the cloud machine
  // record was deleted server-side (token permanently rejected → "sign-in
  // needed" forever) or the user no longer wants cloud. LOCAL-ONLY (403 over the
  // relay — you can't disconnect a machine through its own tunnel).
  disconnect: (): Promise<void> =>
    fetch('/api/cloud/disconnect', { method: 'POST' }).then(jsonOrThrow<void>),
};

// Derived, human-facing summary of the header chip's single truth.
export type ChipKind = 'online' | 'connecting' | 'signin' | 'not-set-up';

export interface ChipState {
  kind: ChipKind;
  label: string;
  // Whether tapping the chip should open the pair panel (true for the
  // actionable states; the connected state is calm/non-actionable but we
  // still let a click open the panel so the user can re-pair if they want).
  actionable: boolean;
}

// Map a live CloudStatus into the chip's one displayed truth. Order matters:
// online wins, then any active/connecting pairing, then a creds problem, then
// the not-set-up default.
export function chipStateFor(s: CloudStatus | null): ChipState {
  if (!s) {
    // No status yet — show the neutral "not set up" entry point rather than a
    // spinner, so the chip is always a usable affordance.
    return { kind: 'not-set-up', label: 'Phone access: not set up', actionable: true };
  }
  if (s.relayState === 'online') {
    return { kind: 'online', label: 'Phone access: Connected', actionable: true };
  }
  if (s.relayState === 'connecting' || s.pairing?.active) {
    return { kind: 'connecting', label: 'Connecting…', actionable: true };
  }
  if (s.credsInvalid) {
    return { kind: 'signin', label: 'Phone access: sign-in needed', actionable: true };
  }
  // !hasCreds / unconfigured / offline-with-no-creds → not set up.
  return { kind: 'not-set-up', label: 'Phone access: not set up', actionable: true };
}

// Shared poller. Polls /api/cloud/status every `intervalMs` while the desktop
// shell is active, plus an immediate fetch on mount and an extra poke whenever
// `pokeKey` changes (e.g. right after starting/cancelling a pairing so the UI
// reflects the new pairing.active without waiting a full interval). Returns the
// latest status (null until the first response), the last error, and a manual
// refresh function. No-ops (returns null) outside the desktop shell.
export function usePhoneAccessStatus(opts?: {
  intervalMs?: number;
  pokeKey?: unknown;
  enabled?: boolean;
}): { status: CloudStatus | null; error: string | null; refresh: () => void } {
  const intervalMs = opts?.intervalMs ?? 5000;
  const enabled = (opts?.enabled ?? true) && isDesktopShell();
  const [status, setStatus] = useState<CloudStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  // A bump counter the refresh() closure increments to force a re-fetch.
  const [tick, setTick] = useState(0);
  const aliveRef = useRef(true);

  const refresh = () => setTick((t) => t + 1);

  useEffect(() => {
    if (!enabled) return;
    aliveRef.current = true;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const ctrl = new AbortController();

    const run = async () => {
      try {
        const s = await cloudPairApi.status(ctrl.signal);
        noteRelayConfigured(s.configured);
        if (!aliveRef.current) return;
        setStatus(s);
        setError(null);
      } catch (e) {
        if (!aliveRef.current) return;
        // Keep the last good status on a transient error; only surface the
        // message. The chip falls back to its neutral entry point on null.
        if ((e as Error).name !== 'AbortError') setError((e as Error).message);
      } finally {
        if (aliveRef.current) timer = setTimeout(run, intervalMs);
      }
    };
    void run();

    return () => {
      aliveRef.current = false;
      if (timer) clearTimeout(timer);
      ctrl.abort();
    };
    // pokeKey + tick force a fresh poll cycle when pairing starts/stops or
    // refresh() is called.
  }, [enabled, intervalMs, tick, opts?.pokeKey]);

  return { status, error, refresh };
}

// Format the seconds-remaining until a code's expiry, or null if no/expired.
export function secondsUntil(expiresAt?: string): number | null {
  if (!expiresAt) return null;
  const ms = new Date(expiresAt).getTime() - Date.now();
  if (!Number.isFinite(ms)) return null;
  return Math.max(0, Math.floor(ms / 1000));
}
