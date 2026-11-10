// Device-local counts of the manual display fixes (ST-82): Alt+R, the command
// palette's "Refresh view" and the pane menu's "Fix display". Panes now heal
// themselves after output settles and on display-scale changes; these counts
// show whether people still reach for the manual fallback, so its removal can
// be decided on data. Stored in this browser's localStorage only — nothing is
// sent anywhere. Shown in Settings › Devices & updates › Diagnostics.

const STORAGE_KEY = 'solu.displayFixes.v1';
const MAX_COUNT = 1_000_000;

export type DisplayFixSource = 'altR' | 'palette' | 'fixDisplay';

export interface DisplayFixTelemetry {
  version: 1;
  altR: number;
  palette: number;
  fixDisplay: number;
  firstAt: string;
  lastAt: string;
}

let cache: DisplayFixTelemetry | null = null;
let pendingWrite: number | null = null;

function freshAggregate(now = new Date().toISOString()): DisplayFixTelemetry {
  return { version: 1, altR: 0, palette: 0, fixDisplay: 0, firstAt: now, lastAt: now };
}

function clampCount(value: unknown): number {
  if (typeof value !== 'number' || !Number.isFinite(value)) return 0;
  return Math.min(MAX_COUNT, Math.max(0, Math.floor(value)));
}

export function readDisplayFixTelemetry(): DisplayFixTelemetry {
  if (cache === null) {
    try {
      const raw = localStorage.getItem(STORAGE_KEY);
      if (raw) {
        const stored = JSON.parse(raw) as Partial<DisplayFixTelemetry>;
        if (stored.version === 1) {
          const now = new Date().toISOString();
          cache = {
            version: 1,
            altR: clampCount(stored.altR),
            palette: clampCount(stored.palette),
            fixDisplay: clampCount(stored.fixDisplay),
            firstAt: typeof stored.firstAt === 'string' ? stored.firstAt : now,
            lastAt: typeof stored.lastAt === 'string' ? stored.lastAt : now,
          };
        }
      }
    } catch {
      // Corrupt or blocked storage starts a fresh aggregate.
    }
    cache ??= freshAggregate();
  }
  return { ...cache };
}

function scheduleWrite(): void {
  if (pendingWrite !== null) return;
  pendingWrite = window.setTimeout(() => {
    pendingWrite = null;
    try {
      if (cache !== null) localStorage.setItem(STORAGE_KEY, JSON.stringify(cache));
    } catch {
      // Device-local telemetry is best effort in private/quota-limited storage.
    }
  }, 1000);
}

export function recordDisplayFix(source: DisplayFixSource): void {
  const aggregate = readDisplayFixTelemetry();
  aggregate[source] = Math.min(MAX_COUNT, aggregate[source] + 1);
  aggregate.lastAt = new Date().toISOString();
  cache = aggregate;
  scheduleWrite();
}

export function resetDisplayFixTelemetry(): void {
  if (pendingWrite !== null) {
    window.clearTimeout(pendingWrite);
    pendingWrite = null;
  }
  cache = null;
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    // A blocked store is already effectively reset for this module.
  }
}

// Debug view: `__SOLU_DISPLAY_FIXES__.read()` in the devtools console.
if (typeof window !== 'undefined') {
  (window as unknown as { __SOLU_DISPLAY_FIXES__?: unknown }).__SOLU_DISPLAY_FIXES__ = {
    read: readDisplayFixTelemetry,
    reset: resetDisplayFixTelemetry,
  };
}
