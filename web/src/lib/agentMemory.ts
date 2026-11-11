import type { PaneInfo } from '../types';

type PaneMemoryFields = Pick<PaneInfo, 'agentRssMB' | 'claudeRssMB'>;

// New daemons report provider-neutral agentRssMB. Fall back to the historical
// Claude-named field so a freshly loaded UI remains compatible during daemon
// updates and with older remote backends.
export function paneAgentRssMB(pane: PaneMemoryFields): number {
  return pane.agentRssMB ?? pane.claudeRssMB ?? 0;
}

// The header's memory pill stays hidden until memory is unusual: at least
// max(1.5 GB, 10% of the machine's RAM), or fast growth within the session.
export const UNUSUAL_FLOOR_MB = 1536;
// Fast growth: 500 MB more than at any sample in the last ten minutes, once
// the agent is past 1 GB. It stays flagged until memory falls below 1 GB
// again (a restart), so the pill does not blink off while the agent is big.
export const FAST_GROWTH_MB = 500;
export const FAST_GROWTH_WINDOW_MS = 10 * 60_000;
export const FAST_GROWTH_MIN_MB = 1024;
const MAX_TRACKED_PANES = 200;

export function unusualMemoryThresholdMB(hostMemTotalMB: number | null | undefined): number {
  return Math.max(UNUSUAL_FLOOR_MB, hostMemTotalMB && hostMemTotalMB > 0 ? Math.round(hostMemTotalMB / 10) : 0);
}

export function agentMemoryLabel(mb: number): string {
  return mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb} MB`;
}

interface GrowthHistory { samples: { at: number; mb: number }[]; fast: boolean }
// Per pane, outside React, so the Terminal and Focus headers share one history
// and switching between them does not start it over.
const growth = new Map<string, GrowthHistory>();

// Records a sample (the daemon samples about once a minute) and says whether
// the pane's agent memory is growing fast.
export function recordAgentMemory(paneId: string, mb: number, now = Date.now()): boolean {
  let h = growth.get(paneId);
  if (!h) {
    h = { samples: [], fast: false };
    growth.set(paneId, h);
    if (growth.size > MAX_TRACKED_PANES) growth.delete(growth.keys().next().value!);
  }
  if (!(mb > 0)) return h.fast;
  if (mb < FAST_GROWTH_MIN_MB) h.fast = false;
  // A sample stands until the next one, so the newest one older than the
  // window is what memory was when the window opened.
  const cutoff = now - FAST_GROWTH_WINDOW_MS;
  const opening = h.samples.filter(s => s.at < cutoff).at(-1);
  h.samples = [...(opening ? [opening] : []), ...h.samples.filter(s => s.at >= cutoff), { at: now, mb }];
  const low = Math.min(...h.samples.map(s => s.mb));
  if (mb >= FAST_GROWTH_MIN_MB && mb - low >= FAST_GROWTH_MB) h.fast = true;
  return h.fast;
}

export function agentMemoryGrowingFast(paneId: string): boolean {
  return growth.get(paneId)?.fast ?? false;
}

export function resetAgentMemoryGrowthForTests(): void {
  growth.clear();
}
