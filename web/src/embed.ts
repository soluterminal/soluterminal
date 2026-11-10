// Shinoque's small, origin-bound host bridge. Keep this module independent of
// the store so it can be loaded before React and its capture listeners mount.
type Theme = 'light' | 'dark';
type HostMessage =
  | { kind: 'init'; theme: Theme; hostKeys?: string[]; capabilities?: string[] }
  | { kind: 'theme'; theme: Theme }
  | { kind: 'visibility'; visible: boolean };

type Activation = { origin: string | null; theme: Theme | null; windowId: string };
type ActivationInput = {
  framed: boolean;
  search: string;
  saved: string | null;
  savedWindow?: string | null;
  ancestorOrigin: string;
  referrer: string;
};

function embedAncestorOrigin(raw: string): string | null {
  try {
    const url = new URL(raw);
    if ((url.protocol === 'http:' || url.protocol === 'https:') &&
      ['127.0.0.1', 'localhost', '[::1]'].includes(url.hostname)) return url.origin;
    // Browsers resolve *.localhost to loopback, so only a local process can
    // serve this host: the same trust class as the existing loopback parents.
    // Accept any port (including normalized :80), but only this exact HTTP host.
    if (url.protocol === 'http:' && url.hostname === 'window.shinoque.localhost' &&
      !url.username && !url.password) return url.origin;
  } catch { /* missing or malformed origin */ }
  return null;
}

function activation(input?: ActivationInput): Activation {
  const off: Activation = { origin: null, theme: null, windowId: 'shinoque' };
  if (typeof window === 'undefined') return off;
  const framed = input?.framed ?? window.self !== window.top;
  if (!framed) return off;
  const params = new URLSearchParams(input?.search ?? window.location.search);
  let requested = params.get('embed') === 'shinoque';
  try { requested ||= (input ? input.saved : sessionStorage.getItem('solu.embed')) === 'shinoque'; } catch { /* blocked storage */ }
  if (!requested) return off;
  const ancestors = (window.location as Location & { ancestorOrigins?: DOMStringList }).ancestorOrigins;
  const origin = embedAncestorOrigin((input ? input.ancestorOrigin : ancestors?.[0]) || (input ? input.referrer : document.referrer));
  if (!origin) return off;
  let id = params.get('embedWindow');
  if (!params.has('embedWindow')) {
    if (input) id = input.savedWindow ?? null;
    else {
      try { id = sessionStorage.getItem('solu.embedWindow'); } catch { /* blocked storage */ }
    }
  }
  const windowId = id && /^[A-Za-z0-9_-]{1,96}$/.test(id) && id !== 'main'
    ? `shinoque.${id}` : 'shinoque';
  if (!input) {
    try {
      if (windowId === 'shinoque') sessionStorage.removeItem('solu.embedWindow');
      else sessionStorage.setItem('solu.embedWindow', id!);
    } catch { /* blocked storage */ }
  }
  if (!input && params.get('embed') === 'shinoque') {
    try { sessionStorage.setItem('solu.embed', 'shinoque'); } catch { /* blocked storage */ }
  }
  const requestedTheme = params.get('theme');
  return { origin, theme: requestedTheme === 'light' || requestedTheme === 'dark' ? requestedTheme : null, windowId };
}

let active = activation();
let hostVisible = !active.origin;
export function embedSurfaceVisible(): boolean {
  return !isEmbedded() || hostVisible;
}
let hostTheme: Theme | null = active.theme;
let hostKeys: string[] = [];
let readySent = false;
let hostOpensFiles = false;
export function isEmbedded(): boolean { return active.origin !== null; }
export function embedWindowIdentity(): string { return active.windowId; }
export function embedParentOrigin(): string | null { return active.origin; }
export function getEmbedTheme(): Theme | null { return hostTheme; }

// Test seam: recompute after tests replace location, referrer or frame globals.
export function __recomputeEmbedForTests(input?: ActivationInput): void {
  active = activation(input);
  hostVisible = !active.origin;
  hostTheme = active.theme;
  hostKeys = [];
  readySent = false;
  hostOpensFiles = false;
}

function plainObject(value: unknown): value is Record<string, unknown> {
  try {
    return value !== null && typeof value === 'object' && !Array.isArray(value) &&
      (Object.getPrototypeOf(value) === Object.prototype || Object.getPrototypeOf(value) === null);
  } catch { return false; }
}

function validKeys(value: unknown): value is string[] {
  return Array.isArray(value) && value.length <= 16 &&
    value.every((key) => typeof key === 'string' && /^mod\+(?:shift\+)?[a-z0-9]$/.test(key));
}

export function parseHostMessage(event: MessageEvent): HostMessage | null {
  if (!active.origin || event.source !== window.parent || event.origin !== active.origin) return null;
  const value: unknown = event.data;
  if (!plainObject(value) || value.source !== 'shinoque' || value.v !== 1) return null;
  if (value.kind === 'init' && (value.theme === 'light' || value.theme === 'dark') &&
      (value.hostKeys === undefined || validKeys(value.hostKeys)) &&
      (value.capabilities === undefined || (Array.isArray(value.capabilities) &&
        value.capabilities.length <= 16 && value.capabilities.every((item) =>
          typeof item === 'string' && item.length <= 64)))) {
    return {
      kind: 'init', theme: value.theme,
      ...(value.hostKeys === undefined ? {} : { hostKeys: value.hostKeys }),
      ...(value.capabilities === undefined ? {} : { capabilities: value.capabilities as string[] }),
    };
  }
  if (value.kind === 'theme' && (value.theme === 'light' || value.theme === 'dark')) {
    return { kind: 'theme', theme: value.theme };
  }
  if (value.kind === 'visibility' && typeof value.visible === 'boolean') {
    return { kind: 'visibility', visible: value.visible };
  }
  return null;
}

function send(kind: string, fields: Record<string, unknown> = {}): void {
  if (!active.origin) return;
  window.parent.postMessage({ source: 'soluterminal', v: 1, kind, ...fields }, active.origin);
}

export function sendEmbedReady(version?: string): void {
  if (!isEmbedded() || readySent) return;
  readySent = true;
  // Keep the v1 envelope: older hosts still initialize theme and shortcuts.
  send('ready', { protocolVersion: 3, windowIdentity: active.windowId, ...(version ? { version } : {}) });
}

// The host opens open-url in the person's signed-in browser as their own
// click, so only a person's gesture may send one. A caller that has the event
// passes it and must have a trusted click (xterm activates links on mouseup),
// or Enter or Space on the control. Callers without an event fall back to
// transient user activation, which any recent keypress in the frame also
// grants, so pass the event wherever there is one.
function personGesture(event?: Event): boolean {
  if (event) {
    if (!event.isTrusted) return false;
    if (event.type === 'click') return true;
    if (event.type === 'mouseup') return (event as MouseEvent).button === 0;
    return event.type === 'keydown' && ['Enter', ' '].includes((event as KeyboardEvent).key);
  }
  return navigator.userActivation?.isActive === true;
}

export const EMBED_OPEN_REFUSED = 'To open this in your browser, click Open in browser or the link itself.';

// The store imports this module, so load it only when a refusal needs a toast.
function refuseOpen(): void {
  void import('./store').then(({ useStore }) => {
    useStore.getState().toast(EMBED_OPEN_REFUSED, { kind: 'warn' });
  }).catch(() => { /* no toast surface */ });
}

// Send browser URLs as clicked; userinfo is never needed for an external link.
export function openEmbedUrl(raw: string, triggerEvent?: Event): boolean {
  if (!isEmbedded()) return false;
  try {
    const url = new URL(raw, window.location.href);
    if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password) return false;
    if (!personGesture(triggerEvent)) {
      refuseOpen();
      return false;
    }
    send('open-url', { url: url.href });
    return true;
  } catch { return false; }
}



export function openEmbedFile(path: string, line?: number, col?: number): boolean {
  if (!isEmbedded() || !hostOpensFiles || !path || path.length > 32768 ||
      /[\x00-\x1f\x7f]/.test(path) ||
      !(/^(?:\/|~\/|[a-zA-Z]:[\\/])/.test(path))) return false;
  if (line !== undefined && (!Number.isSafeInteger(line) || line < 1)) return false;
  if (col !== undefined && (line === undefined || !Number.isSafeInteger(col) || col < 1)) return false;
  send('open-file', {
    path,
    ...(line === undefined ? {} : { line }),
    ...(col === undefined ? {} : { col }),
  });
  return true;
}

export function sendEmbedNotification(title: string, body: string, paneId?: string): void {
  if (!isEmbedded()) return;
  send('notify', { title, body, ...(paneId ? { paneId } : {}) });
}

export function matchesHostKey(event: KeyboardEvent, keys: readonly string[] = hostKeys): string | null {
  if (!isEmbedded() || event.altKey || event.isComposing || !(navigator.platform.startsWith('Mac') ? event.metaKey : event.ctrlKey)) return null;
  if (navigator.platform.startsWith('Mac') ? event.ctrlKey : event.metaKey) return null;
  const key = `mod+${event.shiftKey ? 'shift+' : ''}${event.key.toLowerCase()}`;
  return keys.includes(key) ? key : null;
}

export function installEmbedBridge(onTheme: (theme: Theme) => void): () => void {
  if (!isEmbedded()) return () => {};
  if (hostTheme) onTheme(hostTheme);
  const onKeydown = (event: KeyboardEvent) => {
    const key = matchesHostKey(event);
    if (!key) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    send('host-key', { key });
  };
  const onMessage = (event: MessageEvent) => {
    const message = parseHostMessage(event);
    if (!message) return;
    if (message.kind === 'init') {
      hostKeys = message.hostKeys ?? [];
      hostOpensFiles = message.capabilities?.includes('open-file') === true;
      hostTheme = message.theme;
      onTheme(message.theme);
    } else if (message.kind === 'theme') {
      hostTheme = message.theme;
      onTheme(message.theme);
    } else if (message.kind === 'visibility') {
      hostVisible = message.visible;
      // Consumers that own transient interaction state (for example the
      // voice-first composer) need the hidden edge too. Keep the existing
      // visible-only event for terminal refits and add a symmetric detail
      // event so an embedded pane can cancel a gesture before its host hides
      // the iframe and drops the matching pointer-up.
      window.dispatchEvent(new CustomEvent('solu:embed-visibility', {
        detail: { visible: message.visible },
      }));
      if (message.visible) window.dispatchEvent(new Event('solu:embed-visible'));

    }
  };
  const onClick = (event: MouseEvent) => {
    if (!isEmbedded()) return;
    const target = event.target;
    const anchor = target instanceof Element ? target.closest('a[target="_blank"]') : null;
    if (!(anchor instanceof HTMLAnchorElement) || !/^https?:$/.test(anchor.protocol)) return;
    event.preventDefault();
    openEmbedUrl(anchor.href, event);
  };
  window.addEventListener('keydown', onKeydown, true);
  window.addEventListener('message', onMessage);
  document.addEventListener('click', onClick, true);
  return () => {
    hostOpensFiles = false;
    window.removeEventListener('keydown', onKeydown, true);
    window.removeEventListener('message', onMessage);
    document.removeEventListener('click', onClick, true);
  };
}
