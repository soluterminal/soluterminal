// Arrival parameters for the hosted app. The website's two ways in open the
// app with ?intent=start&ref=<tag> ("Get started") or ?intent=signin ("Sign
// in"); the older ?intent=download links count as start. Marketing links may
// carry utm_* tags too. They are read once on load, kept in this tab's
// sessionStorage (so they survive a reload while the visitor waits for the
// emailed code) and removed from the address bar, so they are not copied,
// bookmarked or sent on as a referrer.
//
// The tags travel only in the sign-up call's user metadata, which Supabase
// applies when it creates the account and ignores for existing accounts: a
// first-touch record for free. It is self-reported attribution, never trusted
// for anything else.

const STORAGE_KEY = 'solu.arrival';

/**
 * The website's counters' switches (website/src/js/site-count.js and
 * download-count.js), honoured here too: with Do Not Track or Global Privacy
 * Control the app keeps no link tag, sends none with a sign-up and counts no
 * download; an automated browser (our tests and CI) isn't a visitor either.
 * The privacy note says so.
 */
export function trackingRefused(win: Window | undefined = typeof window === 'undefined' ? undefined : window): boolean {
  if (!win) return false;
  const nav = win.navigator as Navigator & { globalPrivacyControl?: boolean };
  const legacy = win as unknown as { doNotTrack?: string };
  return nav.doNotTrack === '1' || legacy.doNotTrack === '1' || nav.globalPrivacyControl === true || nav.webdriver === true;
}
// The website's own rule (website/src/js/download-count.js): a short tag of
// plain characters, so nothing odd reaches the account's metadata.
const MAX_TAG_LENGTH = 40;
const TAG_KEYS = ['ref', 'utm_source', 'utm_medium', 'utm_campaign', 'utm_term', 'utm_content'] as const;

export type ArrivalIntent = 'start' | 'signin';
export type FirstTouch = Partial<Record<(typeof TAG_KEYS)[number], string>>;

export interface Arrival {
  intent: ArrivalIntent | null;
  firstTouch: FirstTouch | null;
}

/** "start" and "signin" as named; the retired "download" means start. */
export function parseIntent(value: unknown): ArrivalIntent | null {
  if (value === 'start' || value === 'download') return 'start';
  if (value === 'signin') return 'signin';
  return null;
}

export function cleanTag(value: unknown): string | null {
  if (typeof value !== 'string') return null;
  const tag = value.replace(/[^A-Za-z0-9 _.:+/-]/g, '').trim().slice(0, MAX_TAG_LENGTH).trim();
  return tag || null;
}

function sessionStore(): Storage | null {
  try {
    return typeof window === 'undefined' ? null : window.sessionStorage;
  } catch {
    return null;
  }
}

function parseStored(raw: string | null): Arrival {
  const empty: Arrival = { intent: null, firstTouch: null };
  if (!raw) return empty;
  try {
    const parsed = JSON.parse(raw) as { intent?: unknown; firstTouch?: unknown };
    const intent = parseIntent(parsed.intent);
    let firstTouch: FirstTouch | null = null;
    if (parsed.firstTouch && typeof parsed.firstTouch === 'object') {
      const source = parsed.firstTouch as Record<string, unknown>;
      firstTouch = {};
      for (const key of TAG_KEYS) {
        const tag = cleanTag(source[key]);
        if (tag) firstTouch[key] = tag;
      }
      if (Object.keys(firstTouch).length === 0) firstTouch = null;
    }
    return { intent, firstTouch };
  } catch {
    return empty;
  }
}

// The page-load copy, used only when sessionStorage can't be written.
let memory: Arrival = { intent: null, firstTouch: null };
let storageFailed = false;

function writeStored(arrival: Arrival): void {
  memory = arrival;
  const store = sessionStore();
  if (!store) {
    storageFailed = true;
    return;
  }
  try {
    if (!arrival.intent && !arrival.firstTouch) store.removeItem(STORAGE_KEY);
    else store.setItem(STORAGE_KEY, JSON.stringify(arrival));
    storageFailed = false;
  } catch {
    // Private mode or blocked storage: the tags only last this page load.
    storageFailed = true;
  }
}

/**
 * Reads intent, ref and utm_* from the URL, stores them for this tab and
 * strips them from the address bar. The first tags seen in a tab win; a
 * later arrival may still set the intent.
 */
export function captureArrival(win: Window = window): Arrival {
  let url: URL;
  try {
    url = new URL(win.location.href);
  } catch {
    return readArrival();
  }
  const params = url.searchParams;
  const present = ['intent', ...TAG_KEYS].filter((key) => params.has(key));
  const stored = readArrival();
  if (present.length === 0) return stored;

  const intent = parseIntent(params.get('intent'));
  // The intent is how the page works (sign up or sign in) and is always kept;
  // the tags are kept only when the browser allows it. Either way they leave
  // the address bar below.
  const refused = trackingRefused(win);
  const landing: FirstTouch = {};
  for (const key of TAG_KEYS) {
    const tag = refused ? null : cleanTag(params.get(key));
    if (tag) landing[key] = tag;
  }
  const next: Arrival = {
    intent: intent ?? stored.intent,
    firstTouch: refused ? null : (stored.firstTouch ?? (Object.keys(landing).length ? landing : null)),
  };
  writeStored(next);

  for (const key of present) params.delete(key);
  try {
    const search = params.toString();
    win.history.replaceState(win.history.state, '', `${url.pathname}${search ? `?${search}` : ''}${url.hash}`);
  } catch {
    // A sandboxed frame can refuse replaceState; the tags are stored anyway.
  }
  return next;
}

export function readArrival(): Arrival {
  const store = sessionStore();
  if (!store || storageFailed) return memory;
  try {
    return parseStored(store.getItem(STORAGE_KEY));
  } catch {
    return memory;
  }
}

/**
 * The desktop app's own window. In its cloud launch mode it loads the hosted
 * app's bundle from the cloud, so isLocal alone doesn't tell the website's
 * visitors apart from people already running the app.
 */
export function inDesktopApp(): boolean {
  if (typeof window === 'undefined') return false;
  const w = window as unknown as { __SOLU_TAURI__?: unknown; __TAURI__?: unknown; __TAURI_INTERNALS__?: unknown };
  return Boolean(w.__SOLU_TAURI__ || w.__TAURI__ || w.__TAURI_INTERNALS__);
}

/** How the visitor came from the website: "Get started", "Sign in", or not at all. */
export function arrivalIntent(): ArrivalIntent | null {
  return readArrival().intent;
}

/**
 * Switch this tab's intent, keeping the first-touch tags: "New here? Get
 * started" on a "Welcome back" screen turns a sign-in into a sign-up.
 */
export function setArrivalIntent(intent: ArrivalIntent | null): void {
  writeStored({ ...readArrival(), intent });
}

/**
 * Extra sign-up metadata for the hosted app: the first-touch tags and which
 * surface the account was created on. Supabase stores it only when it creates
 * the account.
 */
export function signupMetadata(): Record<string, unknown> {
  const { intent, firstTouch } = readArrival();
  const data: Record<string, unknown> = {
    signup_surface: intent ? `web-${intent}` : 'web',
  };
  if (firstTouch && !trackingRefused()) data.first_touch = { ...firstTouch };
  return data;
}

/** The single tag the website's download counter records. */
export function downloadCountTag(): string | null {
  if (trackingRefused()) return null;
  const touch = readArrival().firstTouch;
  return touch?.utm_source ?? touch?.ref ?? null;
}
