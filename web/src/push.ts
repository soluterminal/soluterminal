// Web Push helpers. Only meaningful in cloud mode — local mode uses
// in-page Notification() (see App.tsx's state-transition scheduler).
// In cloud mode we register a real Push subscription with the browser,
// hand the subscription object to the cloud, and let the cloud fan out
// via VAPID when the daemon reports a Claude state transition.
//
// Storage: the browser keeps the subscription in its Push Service. Our
// cloud keeps it in memory (see cloud/push.go). We re-POST on every
// page load as a cheap liveness ping — no persistence layer needed.

import { debugLog } from './debug';
import { isEmbedded } from './embed';

const PUSH_SUB_CACHE_KEY = 'solu.pushSubEndpoint';
const PUSH_SUB_ROTATED_AT_KEY = 'solu.pushSubRotatedAt';

// How long a browser push subscription is trusted before we proactively
// rotate it (unsubscribe + resubscribe → fresh FCM endpoint). Android
// and FCM can silently throttle a specific endpoint — Doze bucket quotas,
// per-endpoint rate caps, Pixel Notification Cooldown — at which point
// webpush-go's SendNotification keeps returning 201 Created but the
// phone never sees a banner. There is no server-observable signal for
// this; the only known workaround is to generate a new endpoint, which
// resets whatever per-endpoint state the OS is tracking. A user who
// notices the outage can toggle the bell off/on to trigger the same
// rotation manually; this timer automates it so they don't have to.
// 1h chosen empirically: Benjamin observed throttle onset within ~60min
// of heavy use, and app-opens triggered by "I haven't seen a notification
// in a while" need to rotate to be useful. Going lower (< 30min) risks
// FCM per-origin subscribe rate-limit errors and widens the race window
// between sub.unsubscribe() and the new endpoint landing at the cloud —
// a push arriving during that gap silently drops.
const ROTATION_MAX_AGE_MS = 60 * 60 * 1000;

function readRotatedAt(): number {
  try {
    const v = localStorage.getItem(PUSH_SUB_ROTATED_AT_KEY);
    const n = v ? parseInt(v, 10) : 0;
    return Number.isFinite(n) ? n : 0;
  } catch { return 0; }
}

function writeRotatedAt(ts: number): void {
  try { localStorage.setItem(PUSH_SUB_ROTATED_AT_KEY, String(ts)); } catch { /* ignore */ }
}

export function isPushSubStale(): boolean {
  const ts = readRotatedAt();
  if (!ts) return true; // never rotated — treat as stale so first liveness ping rotates.
  return Date.now() - ts > ROTATION_MAX_AGE_MS;
}

export interface PushSupport {
  ok: boolean;
  reason?: string; // human-readable why-not for the button tooltip
}

export function detectPushSupport(): PushSupport {
  if (typeof window === 'undefined') return { ok: false, reason: 'no window' };
  if (!('serviceWorker' in navigator)) return { ok: false, reason: 'service workers unsupported' };
  if (!('PushManager' in window)) return { ok: false, reason: 'push API unsupported' };
  if (typeof Notification === 'undefined') return { ok: false, reason: 'Notifications unsupported' };
  return { ok: true };
}

// subscribePushToCloud:
//  1. Requests notification permission (if not already granted).
//  2. Subscribes with the browser's Push Service using the cloud's VAPID key.
//  3. POSTs the subscription to the cloud so it can push to us later.
//
// Returns true if the user is subscribed at the end; false on denial or error.
export async function subscribePushToCloud(
  vapidPublicKey: string,
  bearerToken: string,
  // false on the one retry after an "endpoint in use" answer, so a browser
  // that fails to drop its old subscription can't rotate forever.
  rotateIfEndpointInUse = true,
): Promise<boolean> {
  if (isEmbedded()) return false;
  const s = detectPushSupport();
  if (!s.ok) return false;
  if (!vapidPublicKey) return false; // cloud isn't configured for push

  if (Notification.permission === 'default') {
    const perm = await Notification.requestPermission();
    if (perm !== 'granted') return false;
  } else if (Notification.permission === 'denied') {
    return false;
  }

  const reg = await navigator.serviceWorker.ready;

  // Reuse an existing subscription if it matches our VAPID key; otherwise
  // drop it and make a new one. Browsers don't let you change VAPID on a
  // live subscription.
  let sub = await reg.pushManager.getSubscription();
  if (sub) {
    const appServerKey = sub.options.applicationServerKey;
    if (!appServerKey || !sameKey(appServerKey, vapidPublicKey)) {
      await sub.unsubscribe();
      sub = null;
    }
  }
  let createdFresh = false;
  if (!sub) {
    sub = await reg.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlBase64ToArrayBuffer(vapidPublicKey),
    });
    createdFresh = true;
  }

  const resp = await fetch('/api/push/subscribe', {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${bearerToken}`,
    },
    body: JSON.stringify(sub.toJSON()),
  });
  if (!resp.ok) {
    // This browser's endpoint is registered to another account (someone
    // else signed in here before). The cloud never moves it, so mint a
    // fresh one for this account, once.
    if (resp.status === 409 && !createdFresh && rotateIfEndpointInUse) {
      try {
        const body = await resp.clone().json();
        if (body && body.error === 'endpoint_in_use') {
          debugLog('[solu] push endpoint belongs to another account - rotating');
          return rotatePushSubscription(vapidPublicKey, bearerToken, false);
        }
      } catch { /* not JSON — fall through to the warning */ }
    }
    // eslint-disable-next-line no-console
    console.warn('push subscribe POST failed:', resp.status);
    return false;
  }
  try { localStorage.setItem(PUSH_SUB_CACHE_KEY, sub.endpoint); } catch { /* ignore */ }
  // Only stamp the rotation clock when we actually minted a new endpoint.
  // Reusing an existing subscription (common path on resubscribePush)
  // must NOT reset the clock, or the app open that was supposed to
  // trigger rotation would instead postpone it by another 1h.
  if (createdFresh) writeRotatedAt(Date.now());

  // Cloud-flagged staleness: if the cloud has been pushing to this
  // endpoint but our SW never acked (Android/FCM silently throttled),
  // the subscribe response tells us `{stale: true}`. Honour it by
  // rotating immediately — mints a fresh FCM endpoint, bypassing the
  // per-endpoint throttle, and sidesteps the 1h rotation timer which
  // empirically is too slow to catch ~25-min Pixel throttle onset.
  //
  // Skip this on `createdFresh` paths: a brand-new endpoint has no
  // send history and can't be stale; the server returns stale=false
  // anyway, but bailing early saves a second POST we'd discard.
  if (!createdFresh) {
    try {
      const body = await resp.clone().json();
      if (body && body.stale === true) {
        debugLog('[solu] cloud flagged push sub as stale - rotating');
        return rotatePushSubscription(vapidPublicKey, bearerToken);
      }
    } catch { /* legacy cloud returning 204/no-body — ignore */ }
  }
  return true;
}

// rotatePushSubscription: force a brand-new FCM endpoint.
//
// Unsubscribes the current browser push subscription (revoking its
// FCM endpoint), then calls subscribePushToCloud to create a fresh
// one and register it with the cloud. The old endpoint is explicitly
// POSTed to /api/push/unsubscribe so the cloud drops it immediately
// instead of waiting for the next sendPush to return 410 Gone.
//
// Motivating case: Android silently throttled an endpoint for ~1h;
// manual bell off+on fixed it instantly. This automates that workaround
// so a user who leaves the PWA closed overnight doesn't miss pushes
// in the morning — re-opening the app forces a fresh endpoint that
// bypasses whatever per-endpoint throttle Android has accumulated.
export async function rotatePushSubscription(
  vapidPublicKey: string,
  bearerToken: string,
  rotateIfEndpointInUse = true,
): Promise<boolean> {
  const s = detectPushSupport();
  if (!s.ok || !vapidPublicKey) return false;
  if (Notification.permission !== 'granted') return false;

  let oldEndpoint: string | null = null;
  try {
    const reg = await navigator.serviceWorker.ready;
    const sub = await reg.pushManager.getSubscription();
    if (sub) {
      oldEndpoint = sub.endpoint;
      await sub.unsubscribe();
    }
  } catch (e) {
    // eslint-disable-next-line no-console
    console.warn('push rotate: unsubscribe failed', e);
    // Continue anyway — subscribePushToCloud will mint a new sub
    // regardless of whether the old one was cleanly revoked.
  }

  if (oldEndpoint) {
    // Best-effort: tell the cloud to drop the dead endpoint. Without
    // this the cloud would keep it until the next sendPush → 410 Gone,
    // wasting a push cycle.
    try {
      await fetch('/api/push/unsubscribe', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${bearerToken}`,
        },
        body: JSON.stringify({ endpoint: oldEndpoint }),
      });
    } catch { /* ignore */ }
  }

  return subscribePushToCloud(vapidPublicKey, bearerToken, rotateIfEndpointInUse);
}

// unsubscribePushFromCloud: best-effort — tells the cloud to drop our
// subscription, then unsubscribes the browser locally. Idempotent.
export async function unsubscribePushFromCloud(bearerToken: string): Promise<void> {
  let endpoint: string | null = null;
  try { endpoint = localStorage.getItem(PUSH_SUB_CACHE_KEY); } catch { /* ignore */ }

  try {
    const reg = await navigator.serviceWorker.ready;
    const sub = await reg.pushManager.getSubscription();
    if (sub) {
      endpoint = endpoint || sub.endpoint;
      await sub.unsubscribe();
    }
  } catch { /* ignore */ }

  if (endpoint) {
    try {
      await fetch('/api/push/unsubscribe', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${bearerToken}`,
        },
        body: JSON.stringify({ endpoint }),
      });
    } catch { /* best effort */ }
  }
  try { localStorage.removeItem(PUSH_SUB_CACHE_KEY); } catch { /* ignore */ }
  try { localStorage.removeItem(PUSH_SUB_ROTATED_AT_KEY); } catch { /* ignore */ }
}

// ---------- helpers ----------

function urlBase64ToUint8Array(b64: string): Uint8Array {
  const pad = '='.repeat((4 - (b64.length % 4)) % 4);
  const base64 = (b64 + pad).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(base64);
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

// pushManager.subscribe's applicationServerKey wants a concrete
// ArrayBuffer (not a Uint8Array whose buffer might be shared). Build
// a freshly-allocated ArrayBuffer from the decoded bytes.
function urlBase64ToArrayBuffer(b64: string): ArrayBuffer {
  const u = urlBase64ToUint8Array(b64);
  const buf = new ArrayBuffer(u.byteLength);
  new Uint8Array(buf).set(u);
  return buf;
}

function sameKey(existing: ArrayBuffer, candidateB64: string): boolean {
  const candidate = urlBase64ToUint8Array(candidateB64);
  if (candidate.byteLength !== existing.byteLength) return false;
  const a = new Uint8Array(existing);
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== candidate[i]) return false;
  }
  return true;
}
