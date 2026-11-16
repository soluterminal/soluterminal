// relativeTime — "just now / 42s ago / 6m ago / 3h ago / 2d ago" from a
// timestamp in ms. `now` is injectable for deterministic tests; an undefined
// timestamp reads "earlier".
export function relativeTime(ts: number | undefined, now: number = Date.now()): string {
  if (ts == null) return 'earlier';
  const deltaMs = now - ts;
  if (deltaMs < 0) return 'just now';
  if (deltaMs < 5000) return 'just now';
  const s = Math.floor(deltaMs / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}
