// Plain-language System page for public installs. The daemon's own wording
// ("~/.solu volume", "VHDX … sparse", "engine floor", "guard accounting") and
// the repo runbooks are for the team; a stranger sees what each number means
// for their computer and nothing that points into our repository.

import type { SystemStatus } from '../../api';
import { fmtBytes, fmtMB, kpiTiles, type KpiTile, type TopProcess } from './systemModel';

// Plain words for each system area. Shared with the status-bar chip.
export const PLAIN_SYSTEM_AREA: Record<string, string> = {
  disk: 'Disk almost full',
  hostDisk: 'Disk almost full',
  hostMemory: 'Memory running low',
  windowsHost: 'Memory running low',
  hostPressure: 'Computer under memory pressure',
  daemonMemory: 'SoluTerminal is using a lot of memory',
  daemonCpu: 'SoluTerminal is using a lot of CPU',
  automation: 'An automation failed',
  cloud: 'Cloud connection problem',
};

// The verdict in plain words: a warning or a problem names the area; partial
// data (an area this platform can't measure) is not worth a stranger's worry.
export function plainSystemLevel(level: SystemStatus['level']): SystemStatus['level'] {
  const reasons = (level.reasons ?? []).map((r) => ({
    ...r,
    text: r.level === 'unknown' ? 'Some details aren’t available yet' : PLAIN_SYSTEM_AREA[r.key] ?? 'Something needs attention',
  }));
  const top = reasons.find((r) => r.level === level.overall);
  return { ...level, reason: top?.text, reasons };
}

const bytesOf = (bytes: number | null | undefined, mb: number | null | undefined) =>
  bytes ?? (mb != null ? mb * 1024 * 1024 : null);

// The KPI tiles without engineering detail: how much memory SoluTerminal and
// the computer have, and how much disk is free.
export function plainKpiTiles(status: SystemStatus): KpiTile[] {
  const host = status.disk.host ?? null;
  const out: KpiTile[] = [];
  for (const tile of kpiTiles(status)) {
    if (tile.key === 'engine') continue;
    if (tile.key === 'footprint') {
      const sessions = status.footprint.paneCount;
      out.push({
        ...tile,
        label: 'SoluTerminal',
        sub: `memory in use · ${sessions} ${sessions === 1 ? 'session' : 'sessions'}`,
      });
    } else if (tile.key === 'memory') {
      out.push({
        ...tile,
        label: 'Memory',
        sub: `of ${fmtMB(status.host.memTotalMB)}`,
        floor: null,
      });
    } else if (tile.key === 'disk') {
      const total = bytesOf(status.disk.totalBytes, status.disk.totalMB);
      out.push({
        ...tile,
        label: host ? 'Linux (WSL) disk' : 'Disk',
        sub: `of ${fmtBytes(total)}`,
        floor: null,
      });
    } else if (tile.key === 'hostDisk' && host) {
      out.push({
        ...tile,
        label: 'Windows disk',
        value: host.available ? `${fmtBytes(bytesOf(host.freeBytes, host.freeMB))} free` : 'Unavailable',
        sub: host.available
          ? `of ${fmtBytes(bytesOf(host.totalBytes, host.totalMB))} on ${host.volume}`
          : 'Couldn’t read the Windows disk',
        floor: null,
      });
    }
  }
  return out;
}

// The low-disk warning in plain words, without runbook paths or commands.
export function plainBlockingLine(status: SystemStatus): string | null {
  const layer = status.disk.blockingLayer ?? 'none';
  const host = status.disk.host ?? null;
  const advice = 'Free some space so your sessions keep running smoothly.';
  // The host layer blocks heavy work below a generous floor (100 GB by
  // default). That is not "almost full": say so only when the Windows-disk
  // alarm itself is critical (under ~20 GB and ~5% free).
  const hostFull = status.level.areas?.hostDisk === 'critical';
  const hostWaits = host ? `Some background work waits until your Windows drive ${host.volume} has more space (${fmtMB(host.freeMB)} free).` : '';
  if (layer === 'guest') {
    return `Disk space is low (${fmtMB(status.disk.freeMB)} free). ${advice}`;
  }
  if (layer === 'host' && host) {
    return hostFull ? `Your Windows drive ${host.volume} is almost full (${fmtMB(host.freeMB)} free). ${advice}` : hostWaits;
  }
  if (layer === 'both' && host) {
    return hostFull
      ? `Your Windows drive ${host.volume} (${fmtMB(host.freeMB)} free) and the Linux (WSL) disk (${fmtMB(status.disk.freeMB)} free) are both almost full. ${advice}`
      : `Disk space is low (${fmtMB(status.disk.freeMB)} free). ${advice} ${hostWaits}`;
  }
  return null;
}

export function plainProcessKind(kind: TopProcess['kind']): string {
  if (kind === 'term') return 'Terminal';
  if (kind === 'rest') return 'Other';
  return kind;
}

export function plainRelayState(state: string): string {
  if (state === 'online') return 'Connected';
  if (state === 'connecting') return 'Connecting…';
  if (state === 'offline') return 'Offline';
  return 'Not set up';
}

// Error states before the first sample, without daemon or kill-switch names.
export function plainStateText(error: string | null, online: boolean): { title: string; detail: string } {
  if (error === 'old-daemon') {
    return { title: 'System status unavailable', detail: 'Update SoluTerminal to see this page.' };
  }
  if (error === 'unavailable') {
    return { title: 'System status turned off', detail: 'Resource monitoring is turned off on this computer.' };
  }
  return {
    title: online ? 'Loading system status…' : 'SoluTerminal isn’t running',
    detail: error === 'network'
      ? 'Couldn’t reach SoluTerminal’s background service. It will retry.'
      : 'Taking the first measurement.',
  };
}
