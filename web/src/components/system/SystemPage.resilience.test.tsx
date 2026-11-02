import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { SystemStatus } from '../../api';
import { SystemPage } from './SystemPage';

const mocks = vi.hoisted(() => ({
  now: Date.parse('2026-01-01T00:01:00Z'),
  snapshot: { status: null as SystemStatus | null, error: null as null | string, polling: false },
  refresh: vi.fn(),
  usage: vi.fn().mockResolvedValue(null),
}));

vi.mock('../../lib/useSystemStatus', () => ({
  SYSTEM_POLL_MS: 10_000,
  useSystemStatus: () => ({ ...mocks.snapshot, lastOkMs: 0 }),
  refreshSystemStatus: mocks.refresh,
}));
vi.mock('../../lib/useDaemonHealth', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/useDaemonHealth')>();
  return { ...actual, useDaemonHealth: () => ({ health: { version: '1' } }) };
});
vi.mock('../../lib/useNow', () => ({ useNow: () => mocks.now }));
vi.mock('../../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api')>();
  return { ...actual, api: { ...actual.api, ccUsageSummaryCached: mocks.usage } };
});

function fixture(): SystemStatus {
  return {
    sampledAt: '2026-01-01T00:00:00Z', treeSampledAt: '2026-01-01T00:00:00Z', generatedAt: '2026-01-01T00:00:00Z', sampleIntervalSec: 15,
    platform: { os: 'linux', procfs: true }, level: { overall: 'warn', reason: 'disk: 4.0 GB free', reasons: [], areas: { daemonMemory: 'ok', hostMemory: 'ok', disk: 'warn', engine: 'ok' } },
    daemon: { version: '1', webVersion: '1', backendID: 'wsl', shellVersion: '1', pid: 1, uptimeSec: 10, startedAt: '', rssMB: 512, goHeapMB: 100, goroutines: 10, cpuPct: 2, memGuardMB: 6144 },
    host: { cores: 8, cpuPct: 4, loadAvg1: 1, memTotalMB: 8192, memAvailableMB: 4096, memFloorMB: 2048 },
    footprint: { totalRssMB: 1024, daemonRssMB: 512, panesRssMB: 512, workersRssMB: 0, paneCount: 1, workerCount: 0, procs: 3, cpuPct: 2, complete: true }, panes: [], workers: [],
    disk: { layer: 'local', blockingLayer: 'none', anyLow: false, host: null, path: '~/.solu', freeMB: 4096, totalMB: 10000, floorMB: 2048, low: false, measuredAt: null, breakdown: null, breakdownAt: null, measuring: false, fresh: false, nextMeasureAfter: null, goCacheBytes: null, worktreesBytes: null },
    engine: { available: true, queue: { cap: 4, running: 1, runnable: 0, capacityQueued: 0, dependencyBlocked: 0, overlapSerialized: 0, awaitingApproval: 0, quotaPaused: 0, landingWait: 0, resourceWait: 0, heavyCap: 2, heavyRunning: 0, resourceDeferrals: 0, memFloorMB: 2048, diskFloorMB: 2048, diskFreeMB: 4096, diskLow: false, utilization: .25 }, runningInvocations: 0, decide: 2, gcLastSweepAt: null },
    runtimes: null, automation: { agents: { total: 1, paused: 0, running: 0, blocked: 0, errors24h: 0, nextRunAt: null }, schedules: { enabled: 1, disabled: 0, running: 0, errors24h: 0, nextFireAt: null }, builtins: [], autonomy: { globalPaused: false, pausedCount: 0 } },
    surface: { addr: '127.0.0.1:8787', ports: { total: 1, paneOwned: 1 }, previews: { active: 0 }, cloud: { configured: false, hasCreds: false, credsInvalid: false, relayState: 'unconfigured' } },
    release: { running: '1', latest: '1', available: false, applying: false, lastSuccessMs: 0, lastError: '', shellCurrent: '1', shellLatest: '1', shellAvailable: false, readiness: null },
  };
}

afterEach(() => {
  mocks.snapshot = { status: null, error: null, polling: false };
  vi.clearAllMocks();
});

describe('SystemPage stale and offline resilience', () => {
  it('marks a sample stale after three polling intervals', () => {
    mocks.snapshot = { status: fixture(), error: null, polling: false };
    render(<SystemPage onClose={() => {}} />);

    const checked = screen.getByText('stale · checked 1m ago');
    expect(checked).toHaveClass('system-stale');
  });

  it('keeps the last KPI sample visible during a network failure', () => {
    mocks.snapshot = { status: fixture(), error: 'network', polling: false };
    render(<SystemPage onClose={() => {}} />);

    expect(screen.getByText('Updating failed — showing the last successful sample.')).toBeInTheDocument();
    // A public install: the plain KPI labels.
    expect([...document.querySelectorAll('.system-kpi-label')].map((el) => el.textContent))
      .toEqual(['SoluTerminal', 'Memory', 'Disk']);
  });
});
