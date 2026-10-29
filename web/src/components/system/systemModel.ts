import type { SystemDiskBreakdownRow, SystemDiskGrowth, SystemGrowthDelta, SystemGrowthWindow, SystemHostDisk, SystemLevel, SystemStatus } from '../../api';
import type { RunVisual } from '../shared/runStatus';

export interface KpiTile {
  key: string;
  label: string;
  value: string;
  sub: string;
  level: SystemLevel;
  meter: number | null;
  floor: number | null;
}

export interface TopProcess {
  id: string;
  name: string;
  kind: 'term' | 'Focus' | 'worker' | 'rest';
  state: string;
  procs: number;
  rssMB: number | null;
  cpuPct: number | null;
  paneId?: string;
  taskId?: string;
}

export function levelVisual(level: SystemLevel): RunVisual {
  if (level === 'ok') return 'ok';
  if (level === 'warn') return 'attention';
  if (level === 'critical') return 'error';
  return 'neutral';
}

export function fmtMB(mb: number | null | undefined): string {
  if (mb == null) return '—';
  return mb >= 1024
    ? `${(mb / 1024).toFixed(mb >= 10240 ? 1 : 2)} GB`
    : `${Math.round(mb)} MB`;
}

export function fmtBytes(bytes: number | null | undefined): string {
  if (bytes == null) return '—';
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(value >= 10 ? 1 : 2)} ${units[unit]}`;
}

export function fmtPctOf(part: number | null | undefined, total: number | null | undefined): string {
  return part == null || total == null || total <= 0 ? '—' : `${Math.round(100 * part / total)}%`;
}

export function ageLabel(value: string | number | null | undefined, now = Date.now()): string {
  if (value == null) return '—';
  const at = typeof value === 'number' ? value : Date.parse(value);
  if (!Number.isFinite(at)) return '—';
  const sec = Math.max(0, Math.floor((now - at) / 1000));
  if (sec < 60) return `${sec}s ago`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 48) return `${hr}h ago`;
  return `${Math.floor(hr / 24)}d ago`;
}

export function untilLabel(value: string | number | null | undefined, now = Date.now()): string {
  if (value == null) return '—';
  const at = typeof value === 'number' ? value : Date.parse(value);
  if (!Number.isFinite(at)) return '—';
  const sec = Math.max(0, Math.ceil((at - now) / 1000));
  if (sec === 0) return 'now';
  if (sec < 60) return `in ${sec}s`;
  const min = Math.ceil(sec / 60);
  if (min < 60) return `in ${min}m`;
  const hr = Math.ceil(min / 60);
  if (hr < 48) return `in ${hr}h`;
  return `in ${Math.ceil(hr / 24)}d`;
}

export function hostMemoryLabel(status: SystemStatus): string {
  return status.host.provenance === 'wsl' ? 'WSL VM memory' : 'Host memory';
}

export function windowsHostLine(status: SystemStatus, now = Date.now()): string {
  const host = status.host.windows;
  if (!host) return '';
  const sampledAt = host.sampledAt ? Date.parse(host.sampledAt) : NaN;
  const ageSec = host.ageSec ?? (Number.isFinite(sampledAt) ? Math.max(0, Math.floor((now - sampledAt) / 1000)) : undefined);
  const age = ageSec != null ? ` · ${ageSec}s old` : '';
  const free = host.freeMB == null ? '—' : fmtMB(host.freeMB);
  return `Windows host: ${free} free (${host.status}${age})`;
}

export function pressureRows(status: SystemStatus): Array<[string, string, string]> {
  const p = status.host.pressure;
  const value = (v: number | null | undefined, suffix = '') => v == null ? '— · unknown' : `${v.toFixed(v < 10 ? 2 : 1)}${suffix}`;
  const provenance = p?.provenance === 'wsl' ? 'WSL VM' : p?.provenance || status.host.provenance || status.platform.os;
  return [
    ['Memory PSI some10 / full10', `${value(p?.memPSISome10, '%')} / ${value(p?.memPSIFull10, '%')}`, provenance],
    ['CPU PSI some60', value(p?.cpuPSISome60, '%'), provenance],
    ['I/O PSI full60', value(p?.ioPSIFull60, '%'), provenance],
    ['Swap-out', value(p?.swapOutPerSec, '/s'), provenance],
    ['Load / CPUs', `${value(p?.loadAvg1)} / ${p?.nproc ?? status.host.cores}`, provenance],
  ];
}

export function controllerLine(queue: SystemStatus['engine']['queue']): string {
  const c = queue.heavyController;
  if (!c) return '';
  const floor = c.floorActive ? ` · configured minimum ${c.floorCap ?? c.capEff}` : '';
  const advisory = c.floorActive && c.wouldCap != null ? ` (advisory ${c.wouldCap})` : '';
  return `Adaptive cap: ${c.mode} · effective ${c.capEff} · ceiling ${c.heavyMax} · ${c.reason}${floor}${advisory}`;
}

export function recentRollback(queue: SystemStatus['engine']['queue'], now = Date.now()) {
  const rows = queue.heavyController?.rollbacks ?? [];
  const row = rows[rows.length - 1];
  if (!row) return null;
  const age = now - Date.parse(row.at);
  return Number.isFinite(age) && age >= 0 && age <= 24 * 60 * 60_000 ? row : null;
}

const clamp = (value: number) => Math.max(0, Math.min(1, value));

export function kpiTiles(status: SystemStatus): KpiTile[] {
  const areas = status.level.areas ?? {};
  const queue = status.engine.queue;
  const topLevelBytes = status.disk.breakdown
    ?.filter(isSoluTerminalData)
    .reduce((total, row) => total + row.bytes, 0) ?? null;
  const host = status.disk.host ?? null;
  const guestFreeBytes = status.disk.freeBytes ?? (status.disk.freeMB != null ? status.disk.freeMB * 1024 * 1024 : null);
  const guestTotalBytes = status.disk.totalBytes ?? (status.disk.totalMB != null ? status.disk.totalMB * 1024 * 1024 : null);
  const guestUsedBytes = status.disk.usedBytes
    ?? (guestTotalBytes != null && guestFreeBytes != null ? Math.max(0, guestTotalBytes - guestFreeBytes) : null);
  const hostAllocatedBytes = host?.vhdxAllocatedBytes ?? (host?.vhdxPhysicalMB != null ? host.vhdxPhysicalMB * 1024 * 1024 : null);
  const hostApparentBytes = host?.vhdxApparentBytes ?? (host?.vhdxLogicalMB != null ? host.vhdxLogicalMB * 1024 * 1024 : null);
  const tiles: KpiTile[] = [
    {
      key: 'footprint',
      label: 'Footprint',
      value: `${status.footprint.totalRssMB != null && !status.footprint.complete ? '≥' : ''}${fmtMB(status.footprint.totalRssMB)}`,
      sub: `daemon ${fmtMB(status.footprint.daemonRssMB)} · ${status.footprint.paneCount} panes ${fmtMB(status.footprint.panesRssMB)} · ${status.footprint.workerCount} workers ${fmtMB(status.footprint.workersRssMB)} · ${status.footprint.procs} procs`,
      level: areas.hostMemory ?? 'unknown',
      meter: status.host.memTotalMB && status.footprint.totalRssMB != null
        ? clamp(status.footprint.totalRssMB / status.host.memTotalMB)
        : null,
      floor: null,
    },
    {
      key: 'memory',
      label: hostMemoryLabel(status),
      value: `${fmtMB(status.host.memAvailableMB)} available`,
      sub: `of ${fmtMB(status.host.memTotalMB)} · engine floor ${fmtMB(status.host.memFloorMB)}${windowsHostLine(status) ? ` · ${windowsHostLine(status)}` : ''}`,
      level: areas.hostMemory ?? 'unknown',
      meter: status.host.memAvailableMB != null && status.host.memTotalMB
        ? clamp(status.host.memAvailableMB / status.host.memTotalMB)
        : null,
      floor: status.host.memFloorMB && status.host.memTotalMB
        ? clamp(status.host.memFloorMB / status.host.memTotalMB)
        : null,
    },
    {
      key: 'disk',
      label: host ? 'Disk (WSL guest)' : 'Disk',
      value: `${fmtBytes(guestFreeBytes)} free`,
      sub: `of ${fmtBytes(guestTotalBytes)} · used ${fmtBytes(guestUsedBytes)} · ${status.disk.measuring ? 'measuring…' : `~/.solu ${fmtBytes(topLevelBytes)}`}`,
      level: areas.disk ?? 'unknown',
      meter: guestFreeBytes != null && guestTotalBytes
        ? clamp(guestFreeBytes / guestTotalBytes)
        : null,
      floor: status.disk.floorMB && guestTotalBytes
        ? clamp((status.disk.floorMB * 1024 * 1024) / guestTotalBytes)
        : null,
    },
    {
      key: 'engine',
      label: 'Engine',
      value: status.engine.available ? `${queue.running}/${queue.cap} running` : 'Unavailable',
      sub: status.engine.available
        ? `heavy ${queue.heavyRunning}/${queue.heavyCapEff ?? queue.heavyCap}${queue.heavyMax ? ` · ceiling ${queue.heavyMax}` : ''}${queue.heavyController ? ` · ${queue.heavyController.mode}` : ''} · ${status.engine.decide} waiting on you`
        : 'Workflow engine disabled',
      level: areas.engine ?? (status.engine.available ? 'ok' : 'unknown'),
      meter: status.engine.available && queue.cap ? clamp(queue.running / queue.cap) : null,
      floor: null,
    },
  ];
  if (host) {
    const pct = host.freeMB != null && host.totalMB ? Math.round(100 * host.freeMB / host.totalMB) : null;
    tiles.splice(3, 0, {
      key: 'hostDisk',
      label: 'Windows host disk',
      value: host.available ? `${fmtBytes(host.freeBytes ?? (host.freeMB != null ? host.freeMB * 1024 * 1024 : null))} free on ${host.volume}` : 'Unavailable',
      sub: host.available
        ? `of ${fmtBytes(host.totalBytes ?? (host.totalMB != null ? host.totalMB * 1024 * 1024 : null))} · ${pct ?? '—'}% · VHDX ${fmtBytes(hostAllocatedBytes)} allocated / ${fmtBytes(hostApparentBytes)} apparent${host.sparse === false ? ', not sparse' : host.sparse ? ', sparse' : ''} · blocks heavy work below ${fmtMB(host.floorMB)}${host.offender?.text ? ` · ${host.offender.text}` : ''}`
        : `probe ${host.state}`,
      level: areas.hostDisk ?? 'unknown',
      meter: host.freeMB != null && host.totalMB ? clamp(host.freeMB / host.totalMB) : null,
      floor: host.floorMB != null && host.totalMB ? clamp(host.floorMB / host.totalMB) : null,
    });
  }
  return tiles;
}

export function hostOffenderLine(host: SystemHostDisk | null): string | null {
  if (!host?.offender?.text) return null;
  const loss = host.unexplainedLossMB != null
    ? ` · unexplained host loss ${fmtMB(host.unexplainedLossMB)} in ${host.lossWindowMin ?? 0} min`
    : '';
  return `${host.offender.text}${loss}`;
}

export function blockingLayerLine(status: SystemStatus): string | null {
  const layer = status.disk.blockingLayer ?? 'none';
  const host = status.disk.host ?? null;
  const offender = hostOffenderLine(host);
  const guest = `Low disk on ~/.solu volume (${fmtMB(status.disk.freeMB)} free, floor ${fmtMB(status.disk.floorMB)})`;
  if (layer === 'guest') return `${guest} — heavy work is waiting.`;
  if (layer === 'host' && host) return offender
    ? `Heavy work is blocked by the Windows host disk (${host.volume}: ${fmtMB(host.freeMB)} free, floor ${fmtMB(host.floorMB)}) — ${offender}; delete it manually (see docs/RUNBOOK-wsl-host-disk.md) or run solud host-storage compact --dry-run`
    : `Heavy work is blocked by the Windows host disk (${host.volume}: ${fmtMB(host.freeMB)} free, floor ${fmtMB(host.floorMB)}) — the WSL guest still has ${fmtMB(status.disk.freeMB)} free; free host space or run solud host-storage compact --dry-run`;
  if (layer === 'both' && host) return offender
    ? `${guest}; the Windows host disk is also low (${host.volume}: ${fmtMB(host.freeMB)} free, floor ${fmtMB(host.floorMB)}). Both layers must recover — ${offender}; delete it manually (see docs/RUNBOOK-wsl-host-disk.md) or run solud host-storage compact --dry-run`
    : `${guest}; the Windows host disk is also low (${host.volume}: ${fmtMB(host.freeMB)} free, floor ${fmtMB(host.floorMB)}). Both layers must recover.`;
  return null;
}

export function topProcesses(status: SystemStatus, count = 12): TopProcess[] {
  const all: TopProcess[] = [
    ...status.panes.map((pane) => ({
      id: `pane:${pane.id}`,
      name: pane.title || pane.id,
      kind: pane.kind === 'native' ? 'Focus' as const : 'term' as const,
      state: pane.state || pane.status,
      procs: pane.procs,
      rssMB: pane.rssMB,
      cpuPct: pane.cpuPct,
      paneId: pane.id,
    })),
    ...status.workers.map((worker) => ({
      id: `worker:${worker.taskId}:${worker.pid}`,
      name: worker.taskTitle || worker.taskId,
      kind: 'worker' as const,
      state: worker.stage,
      procs: worker.procs,
      rssMB: worker.rssMB,
      cpuPct: worker.cpuPct,
      taskId: worker.taskId,
    })),
  ].sort((a, b) => (b.rssMB ?? -1) - (a.rssMB ?? -1));
  if (all.length <= count) return all;
  const rest = all.slice(count);
  const rssMB = rest.every((row) => row.rssMB != null)
    ? rest.reduce((total, row) => total + (row.rssMB ?? 0), 0)
    : null;
  const cpuPct = rest.every((row) => row.cpuPct != null)
    ? rest.reduce((total, row) => total + (row.cpuPct ?? 0), 0)
    : null;
  return [
    ...all.slice(0, count),
    {
      id: 'rest',
      name: `${rest.length} other processes`,
      kind: 'rest',
      state: '',
      procs: rest.reduce((total, row) => total + row.procs, 0),
      rssMB,
      cpuPct,
    },
  ];
}

export function diskRows(rows: SystemDiskBreakdownRow[] | null | undefined) {
  // The ~/.solu denominator is only the SoluTerminal-owned production tree.
  // External/protected producer rows and capacity-only categories are shown but
  // never inflate the local-volume total.
  const total = (rows ?? [])
    .filter(isSoluTerminalData)
    .reduce((sum, row) => sum + row.bytes, 0);
  return [...(rows ?? [])]
    .sort((a, b) => b.bytes - a.bytes)
    .map((row) => ({
      ...row,
      depth: 0,
      share: isSoluTerminalData(row) && total > 0 ? row.bytes / total : 0,
    }));
}

export function isSoluTerminalData(row: SystemDiskBreakdownRow): boolean {
  const category = row.category ?? row.name;
  return category === 'workflow_worktrees' || category === 'retained_task_history'
    || category === 'task_temp' || category === 'other_soluterminal_data';
}

export interface StorageProducerRow {
  category: string;
  label: string;
  protection: string;
  cleanup: string;
  managed: boolean;
  apparentBytes: number;
  allocatedBytes: number | null;
  complete: boolean;
  measured: boolean;
  scannedAt: string | null;
  targetCount: number;
  dayDelta: SystemGrowthDelta | null;
  weekDelta: SystemGrowthDelta | null;
}

export function growthDelta(window: SystemGrowthWindow | null | undefined, category: string): SystemGrowthDelta | null {
  if (!window?.available) return null;
  return window.deltas?.find((delta) => delta.category === category) ?? null;
}

export function storageProducerRows(status: SystemStatus): StorageProducerRow[] {
  const growth: SystemDiskGrowth | null | undefined = status.disk.growth;
  return (status.disk.breakdown ?? [])
    .filter((row) => row.measured || (row.bytes ?? 0) > 0)
    .map((row) => {
      const category = row.category ?? row.name;
      return {
        category,
        label: row.label || row.name,
        protection: row.protection ?? 'read-only',
        cleanup: row.cleanup ?? 'none',
        managed: (row.protection ?? 'read-only') === 'managed',
        apparentBytes: row.apparentBytes ?? row.bytes,
        allocatedBytes: row.allocatedBytes ?? null,
        complete: row.complete !== false,
        measured: row.measured !== false,
        scannedAt: row.scannedAt ?? null,
        targetCount: row.targetCount ?? 0,
        dayDelta: growthDelta(growth?.day, category),
        weekDelta: growthDelta(growth?.week, category),
      };
    })
    .sort((a, b) => b.apparentBytes - a.apparentBytes);
}

export function growthStateLabel(window: SystemGrowthWindow | null | undefined): string {
  if (!window) return 'unavailable';
  if (window.available) return 'measured';
  switch (window.continuity) {
    case 'no_baseline': return 'no baseline';
    case 'stale': return 'stale';
    case 'gap': return 'history gap';
    case 'reboot': return 'reboot boundary';
    case 'incomplete': return 'incomplete';
    default: return 'unavailable';
  }
}

export function deltaLabel(delta: SystemGrowthDelta | null): string {
  if (!delta) return '—';
  if (delta.noise) return 'noise (<64 MiB / 1%)';
  const sign = delta.deltaBytes > 0 ? '+' : delta.deltaBytes < 0 ? '−' : '';
  return `${sign}${fmtBytes(Math.abs(delta.deltaBytes))}`;
}

export const GUEST_ONLY_MESSAGE = 'Linux space freed; Windows image still allocated; maintenance needed';

// guestOnlyExplanation is the only place the guest-cleanup versus host-VHDX
// distinction is phrased for the UI. It never promises compaction savings.
export function guestOnlyExplanation(status: SystemStatus): string | null {
  const cleanup = status.disk.cleanup;
  if (!cleanup?.message && cleanup?.state !== 'guest_only_pending_host_reclaim') return null;
  const message = cleanup.message || (cleanup.state === 'guest_only_pending_host_reclaim' ? GUEST_ONLY_MESSAGE : '');
  if (!message) return null;
  return `${message}. Linux deletions do not shrink the Windows VHDX by themselves; run the Windows maintenance step (see docs/RUNBOOK-wsl-host-disk.md) to reclaim host space. No compaction savings are guaranteed.`;
}

export function cleanupScopeLabel(status: SystemStatus): string {
  const cleanup = status.disk.cleanup;
  if (!cleanup) return 'workflow GC eligibility unavailable';
  if (cleanup.previewOnly) return `${cleanup.scope} · preview only`;
  return cleanup.scope || 'workflow GC policy scope';
}

export function automationLine(status: SystemStatus): string {
  // Managed agents were removed; the built-in agents are schedules.
  return `${status.automation.schedules.enabled} schedules enabled · ${status.automation.schedules.running} running`;
}

export function releaseLine(status: SystemStatus): string {
  if (status.release.available) return `${status.release.latest || 'Update'} available`;
  if (status.release.readiness?.functionalCommitCount) {
    return `${status.release.readiness.functionalCommitCount} functional commits unreleased`;
  }
  return `${status.release.running} is current`;
}

// Areas that only describe the team's workflow engine and its runtimes. A
// public install does not run the engine, so they are left out of its
// verdict and KPI tiles.
const WORKFLOW_AREAS = new Set(['engine', 'runtimes']);

export function publicSystemLevel(level: SystemStatus['level']): SystemStatus['level'] {
  const rank = { ok: 0, unknown: 1, warn: 2, critical: 3 } as const;
  const reasons = (level.reasons ?? []).filter((r) => !WORKFLOW_AREAS.has(r.key));
  const areas = Object.fromEntries(Object.entries(level.areas ?? {}).filter(([key]) => !WORKFLOW_AREAS.has(key)));
  const worst = reasons.reduce<SystemStatus['level']['overall']>((acc, r) => (rank[r.level] > rank[acc] ? r.level : acc), 'ok');
  return { overall: worst, reason: reasons.find((r) => r.level === worst)?.text, reasons, areas };
}

export function publicKpiTiles(tiles: KpiTile[]): KpiTile[] {
  return tiles.filter((tile) => tile.key !== 'engine');
}
