
import { useEffect, useState } from 'react';
import { createPortal } from 'react-dom';
import { api } from '../../api';
import type { CCUsageSummary } from '../../api';
import { useDaemonHealth } from '../../lib/useDaemonHealth';
import { useNow } from '../../lib/useNow';
import { SYSTEM_POLL_MS, refreshSystemStatus, useSystemStatus } from '../../lib/useSystemStatus';
import { useSettings } from '../../settings';
import { useStore } from '../../store';
import { useOperatorFeatures, useTeamFeatures } from '../../entitlements';
import { ErrorBoundary } from '../ErrorBoundary';
import { RunStatusGlyph } from '../shared/runStatus';

import { IconGauge, IconX } from '../ui/icons';
import { ageLabel, automationLine, blockingLayerLine, cleanupScopeLabel, deltaLabel, publicKpiTiles, publicSystemLevel, fmtBytes, fmtMB, growthStateLabel, guestOnlyExplanation, hostOffenderLine, kpiTiles, levelVisual, releaseLine, storageProducerRows, topProcesses, untilLabel } from './systemModel';
import { plainBlockingLine, plainKpiTiles, plainProcessKind, plainRelayState, plainStateText, plainSystemLevel } from './systemPlain';
import { UPDATE_CHECK_FAILED } from '../../lib/updateErrors';
import './system.css';

interface Props { onClose: () => void }

function revealSidebar(key: 'schedules') {
  window.dispatchEvent(new CustomEvent('solu:reveal-sidebar-section', { detail: { key } }));
}

function LevelGlyph({ level }: { level: 'ok' | 'warn' | 'critical' | 'unknown' }) {
  return <RunStatusGlyph visual={levelVisual(level)} size={13} />;
}

function SystemPageState({ title, detail, onClose }: { title: string; detail: string; onClose: () => void }) {
  return (
    <div className="system-state">
      <button type="button" className="system-close" aria-label="Close System" onClick={onClose}><IconX size={16} /></button>
      <h3>{title}</h3>
      <p>{detail}</p>
    </div>
  );
}

function SystemPageBody({ onClose }: Props) {
  const { status, error, polling } = useSystemStatus();
  const { health } = useDaemonHealth();
  const now = useNow(1_000);
  const ports = useStore((s) => s.ports);
  // The workflow engine, crew, managed agents, release train and their
  // runbooks are team tooling; everyone else sees resources and updates only.
  const team = useOperatorFeatures();

  // Engineering detail (versions, Go memory, storage producers, host storage
  // internals, the listener) stays with team installs; the public page speaks
  // plainly about memory, disk, sessions and updates.
  const detail = useTeamFeatures() || team;
  const [usage, setUsage] = useState<CCUsageSummary | null>(null);

  useEffect(() => {
    void refreshSystemStatus({ measure: true });
    const controller = new AbortController();
    void api.ccUsageSummaryCached('7d', { signal: controller.signal }).then(setUsage, () => undefined);
    return () => controller.abort();
  }, []);
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') { event.preventDefault(); onClose(); }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);

  if (!detail && (error === 'old-daemon' || error === 'unavailable' || !status)) {
    const plain = plainStateText(error, Boolean(health));
    return <SystemPageState title={plain.title} detail={plain.detail} onClose={onClose} />;
  }
  if (error === 'old-daemon') return <SystemPageState title="System status unavailable" detail="This daemon predates the System page — update solud." onClose={onClose} />;
  if (error === 'unavailable') return <SystemPageState title="System sampler disabled" detail="Remove the SOLU_SYSTEM_SAMPLER kill-switch and restart solud." onClose={onClose} />;
  if (!status) return <SystemPageState title={health ? 'Loading system status…' : 'Daemon offline'} detail={error === 'network' ? 'The last update could not reach solud.' : 'Collecting the first bounded resource sample.'} onClose={onClose} />;

  const level = team ? status.level : detail ? publicSystemLevel(status.level) : plainSystemLevel(publicSystemLevel(status.level));
  const tiles = team ? kpiTiles(status) : detail ? publicKpiTiles(kpiTiles(status)) : plainKpiTiles(status);
  const ageMs = Math.max(0, now - Date.parse(status.sampledAt));
  const stale = !Number.isFinite(ageMs) || ageMs > 3 * SYSTEM_POLL_MS;
  const processes = topProcesses(status);
  const producers = storageProducerRows(status);
  const growthDay = status.disk.growth?.day;
  const growthWeek = status.disk.growth?.week;
  const cleanup = status.disk.cleanup;
  const guestOnly = guestOnlyExplanation(status);
  const usageTop = usage?.by_model?.[0]?.model ?? '—';
  const builtinsPresent = status.automation.builtins.filter((row) => row.present).length;
  const builtinsRunning = status.automation.builtins.filter((row) => row.running).length;
  const blocking = detail ? blockingLayerLine(status) : plainBlockingLine(status);
  // Free installs don't upload usage, so an empty Usage strip has nothing to
  // promise them; it shows only once there is something to show.
  const showUsage = detail || Boolean(usage?.hero.turns);

  return (
    <>
      <header className="system-header">
        <div className="system-title"><IconGauge size={18} /><h2>System</h2></div>
        <div className={`system-verdict is-${level.overall}`} aria-live="polite">
          <LevelGlyph level={level.overall} />
          <span>{level.reason || (level.overall === 'ok' ? 'All monitored systems healthy' : 'Partial system data')}</span>
        </div>
        <div className="system-header-meta">
          <span className={stale ? 'system-stale' : ''}>{stale ? 'stale · ' : ''}checked {ageLabel(status.sampledAt, now)}</span>
          {detail
            ? <span>{status.daemon.backendID || status.platform.os} · solud v{status.daemon.version} · web v{status.daemon.webVersion || '—'}</span>
            : <span>SoluTerminal v{status.daemon.version}</span>}
          {polling && <span>Updating…</span>}
        </div>
        <button type="button" className="system-close" aria-label="Close System" onClick={onClose}><IconX size={16} /></button>
      </header>

      {error === 'network' && <div className="system-inline-warn">Updating failed — showing the last successful sample.</div>}
      {blocking && <p className="system-inline-warning">{blocking}</p>}

      <section className="system-kpis" aria-label="System overview">
        {tiles.map((tile) => (
          <article key={tile.key} className={`system-kpi-tile is-${tile.level}`}>
            <div className="system-kpi-label">{tile.label}</div>
            <div className="system-kpi-value">{tile.value}</div>
            <div className="system-kpi-sub">{tile.sub}</div>
            {tile.meter !== null && <div className="system-meter"><span className="system-meter-fill" style={{ width: `${tile.meter * 100}%` }} />{tile.floor !== null && <span className="system-meter-floor" style={{ left: `${tile.floor * 100}%` }} />}</div>}
          </article>
        ))}
      </section>

      <section className={`system-resources${detail ? '' : ' is-plain'}`}>
        <article className="system-card system-processes">
          {detail ? <>
            <div className="system-card-heading"><h3>Processes</h3><span>{status.platform.procfs ? '60s process-tree sample' : 'process accounting partial on this platform'}</span></div>
            <p className="system-muted">Daemon RSS {fmtMB(status.daemon.rssMB)} · allocated Go heap {fmtMB(status.daemon.goHeapMB)} · Go committed {fmtMB(status.daemon.goRuntimeMB)} · guard accounting {fmtMB(status.daemon.guardUsedMB)} / {fmtMB(status.daemon.memGuardMB)}</p>
          </> : <div className="system-card-heading"><h3>Sessions</h3><span>updated every minute</span></div>}
          <div className="system-table-wrap"><table className="system-table"><thead><tr>{detail ? <><th>Name</th><th>Kind</th><th>State</th><th>Procs</th><th>RSS</th><th>CPU</th></> : <><th>Name</th><th>Type</th><th>State</th><th>Processes</th><th>Memory</th><th>CPU</th></>}</tr></thead><tbody>
            {processes.map((row) => <tr key={row.id}><td>{row.paneId ? <button type="button" className="system-link" onClick={() => { useStore.getState().setFocus(row.paneId!); onClose(); }}>{row.name}</button> : row.name}</td><td>{detail ? row.kind : plainProcessKind(row.kind)}</td><td>{row.state || '—'}</td><td>{row.procs}</td><td>{fmtMB(row.rssMB)}</td><td>{status.platform.procfs && row.cpuPct != null ? `${row.cpuPct.toFixed(1)}%` : '—'}</td></tr>)}
            {processes.length === 0 && <tr><td colSpan={6}>{detail ? 'No running pane or worker trees.' : 'No sessions running.'}</td></tr>}
          </tbody></table></div>
        </article>
        {detail && <article className="system-card system-producers">
          <div className="system-card-heading"><h3>Storage producers</h3><button type="button" className="system-link" onClick={() => void refreshSystemStatus({ measure: true })}>Measure now</button></div>
          <p className="system-muted">24h {growthStateLabel(growthDay)} · 7d {growthStateLabel(growthWeek)}. Category growth is measured correlation, not proof of causation.</p>
          <div className="system-table-wrap"><table className="system-table"><thead><tr><th>Producer</th><th>Policy</th><th>Apparent</th><th>Allocated</th><th>Scan</th><th>24h</th><th>7d</th></tr></thead><tbody>
            {producers.map((row) => (
              <tr key={row.category}>
                <td>{row.label}</td>
                <td><span className={`system-policy is-${row.protection}`}>{row.protection}</span>{row.cleanup !== 'none' ? ` · ${row.cleanup}` : ''}</td>
                <td>{row.measured ? `${row.complete ? '' : '≥'}${fmtBytes(row.apparentBytes)}` : '—'}</td>
                <td>{row.allocatedBytes != null ? fmtBytes(row.allocatedBytes) : '—'}</td>
                <td>{row.scannedAt ? ageLabel(row.scannedAt, now) : row.complete ? 'pending' : 'incomplete'}</td>
                <td>{deltaLabel(row.dayDelta)}</td>
                <td>{deltaLabel(row.weekDelta)}</td>
              </tr>
            ))}
            {producers.length === 0 && <tr><td colSpan={7}>No producer inventory yet.</td></tr>}
          </tbody></table></div>
          {status.disk.breakdownAt && <p className="system-muted">measured {ageLabel(status.disk.breakdownAt, now)}{status.disk.fresh ? ' · fresh cache' : ''}</p>}
          {status.disk.goCacheBytes != null && <p className="system-muted">Go build cache: {fmtBytes(status.disk.goCacheBytes)}</p>}

        </article>}
        {team && <article className="system-card system-cleanup">
          <div className="system-card-heading"><h3>Cleanup &amp; reclamation</h3><span>{cleanupScopeLabel(status)}</span></div>
          {guestOnly && <p className="system-cleanup-banner">{guestOnly}</p>}
          {cleanup ? <>
            <div className="system-rows">
              <div><span>Workflow reclaimable</span><span>{fmtBytes(cleanup.reclaimableBytes)}</span></div>
              <div><span>Reclaimed (guest)</span><span>{fmtBytes(cleanup.reclaimedBytes)}</span></div>
              <div><span>Records</span><span>{cleanup.previewOnly ? 'preview only' : 'executed operations recorded'}{cleanup.scannedAt ? ` · scanned ${ageLabel(cleanup.scannedAt, now)}` : ''}</span></div>
            </div>
            {cleanup.operations.length > 0 && <div className="system-table-wrap"><table className="system-table"><thead><tr><th>When</th><th>Kind</th><th>Layer</th><th>Outcome</th><th>Guest Δ</th></tr></thead><tbody>
              {cleanup.operations.map((op, index) => <tr key={`${op.at}:${op.kind}:${index}`}><td>{ageLabel(op.at, now)}</td><td>{op.kind}</td><td>{op.layer}</td><td>{op.executed ? (op.verified ? 'verified' : 'executed') : 'preview'}</td><td>{op.guestFreeDeltaBytes != null ? deltaLabel({ category: '', label: '', baselineBytes: 0, latestBytes: 0, deltaBytes: op.guestFreeDeltaBytes, noise: false }) : '—'}</td></tr>)}
            </tbody></table></div>}
            {(cleanup.skips?.length ?? 0) > 0 && <div className="system-rows">{(cleanup.skips ?? []).slice(0, 6).map((skip) => <div key={skip.reason}><span>{skip.reason}</span><span>{skip.count}</span></div>)}</div>}
          </> : <p>No cleanup operations recorded.</p>}
          <p className="system-muted">External and protected producers are read-only; nothing is deleted automatically. docs/RUNBOOK-wsl-host-disk.md</p>
        </article>}
        {detail && status.disk.host && <article className="system-card system-host-storage">
          <div className="system-card-heading"><h3>Host storage</h3><button type="button" className="system-link" onClick={() => void api.hostStorageRefresh().then(() => refreshSystemStatus()).catch(() => {})}>Refresh probe</button></div>
          <div className="system-table-wrap"><table className="system-table"><thead><tr><th>Layer</th><th>Free</th><th>Total</th><th>Floor</th><th>Status</th></tr></thead><tbody>
            <tr><td>Windows host ({status.disk.host.volume || 'unknown'})</td><td>{fmtMB(status.disk.host.freeMB)}</td><td>{fmtMB(status.disk.host.totalMB)}</td><td>{fmtMB(status.disk.host.floorMB)}</td><td>{status.disk.host.available ? status.disk.host.low ? 'blocked' : 'healthy' : 'unavailable'}</td></tr>
            <tr><td>WSL guest (~/.solu)</td><td>{fmtMB(status.disk.freeMB)}</td><td>{fmtMB(status.disk.totalMB)}</td><td>{fmtMB(status.disk.floorMB)}</td><td>{status.disk.low ? 'blocked' : 'healthy'}</td></tr>
          </tbody></table></div>
          {status.disk.host.available
            ? <p>ext4.vhdx {fmtBytes(status.disk.host.vhdxAllocatedBytes ?? (status.disk.host.vhdxPhysicalMB != null ? status.disk.host.vhdxPhysicalMB * 1024 * 1024 : null))} allocated / {fmtBytes(status.disk.host.vhdxApparentBytes ?? (status.disk.host.vhdxLogicalMB != null ? status.disk.host.vhdxLogicalMB * 1024 * 1024 : null))} apparent · sparse: {status.disk.host.sparse == null ? 'unknown' : status.disk.host.sparse ? 'yes' : 'no'} · sampled {ageLabel(status.disk.host.sampledAt, now)}{status.disk.host.dumpBytes != null ? ` · WSL dumps ${fmtBytes(status.disk.host.dumpBytes)}` : ''}</p>
            : <p className="system-error-text">Host probe unavailable: {status.disk.host.state} — {status.disk.host.detail || 'no detail'}</p>}
          {hostOffenderLine(status.disk.host) && <p className="system-error-text">{hostOffenderLine(status.disk.host)} — nothing is deleted automatically{team ? '; see runbook' : ''}</p>}
          {team && <p className="system-muted">docs/RUNBOOK-wsl-host-disk.md · solud host-storage status</p>}
        </article>}
      </section>

      <section className="system-grid" aria-label="Moving parts">

        {team && <article className="system-card"><h3>Crew & runtimes</h3><div className="system-rows">{(status.runtimes ?? []).map((runtime) => <div key={runtime.id}><span><RunStatusGlyph visual={runtime.status === 'healthy' ? 'ok' : runtime.status === 'unknown' ? 'neutral' : 'attention'} /> {runtime.id}</span><span>{runtime.status}{runtime.cooldownUntilEpoch ? ` · cooldown ${untilLabel(runtime.cooldownUntilEpoch * 1000, now)}` : ''}</span></div>)}</div>{!status.runtimes?.length && <p>Runtime health unavailable.</p>}</article>}
        {team && <article className="system-card"><h3>Automation</h3>{status.automation.autonomy.globalPaused && <div className="system-neutral-banner">Autonomy paused</div>}<p>{automationLine(status)}</p><div className="system-stat-line"><span>Errors 24h <b>{status.automation.schedules.errors24h}</b></span><span>Built-ins <b>{builtinsPresent}/{status.automation.builtins.length} present · {builtinsRunning} running</b></span><span>Warden <b>{status.automation.warden.enabled ? 'on' : 'off'}</b></span></div><div className="system-rows"><div><span>Next schedule</span><span>{untilLabel(status.automation.schedules.nextFireAt, now)}</span></div><div><span>Warden last run</span><span>{status.automation.warden.lastRunAt ? `${status.automation.warden.lastRunStatus || 'finished'} · ${ageLabel(status.automation.warden.lastRunAt, now)}` : '—'}</span></div></div><div className="system-card-actions"><button type="button" className="system-deep-link" onClick={() => { useStore.getState().setAgentsPanelOpen(true); onClose(); }}>Agents status</button><button type="button" className="system-deep-link" onClick={() => { revealSidebar('schedules'); onClose(); }}>Schedules</button></div></article>}
        {detail ? <article className="system-card"><h3>Surface</h3><div className="system-rows"><div><span>Listener</span><span>{status.surface.addr || '—'}</span></div><div><span>Ports</span><span>{status.surface.ports.paneOwned} pane-owned / {status.surface.ports.total} total</span></div><div><span>Previews</span><span>{status.surface.previews.active} active</span></div><div><span>Cloud relay</span><span><RunStatusGlyph visual={status.surface.cloud.relayState === 'online' ? 'ok' : status.surface.cloud.configured ? 'attention' : 'neutral'} /> {status.surface.cloud.relayState}</span></div></div>{ports.slice(0, 4).map((port) => <p className="system-muted" key={`${port.proto}:${port.port}`}>:{port.port} · {port.process}{port.paneId ? ' · pane-owned' : ''}</p>)}<button type="button" className="system-deep-link" onClick={() => { useSettings.getState().openSettings('devices'); onClose(); }}>Devices & cloud</button></article> : <article className="system-card"><h3>Connections</h3><div className="system-rows"><div><span>Local servers</span><span>{status.surface.ports.total}{status.surface.ports.paneOwned ? ` · ${status.surface.ports.paneOwned} from your sessions` : ''}</span></div><div><span>Phone &amp; browser access</span><span><RunStatusGlyph visual={status.surface.cloud.relayState === 'online' ? 'ok' : status.surface.cloud.configured ? 'attention' : 'neutral'} /> {plainRelayState(status.surface.cloud.relayState)}</span></div></div>{ports.slice(0, 4).map((port) => <p className="system-muted" key={`${port.proto}:${port.port}`}>:{port.port} · {port.process}</p>)}<button type="button" className="system-deep-link" onClick={() => { useSettings.getState().openSettings('devices'); onClose(); }}>Devices &amp; cloud</button></article>}
        {team ? <article className="system-card"><h3>Release & update</h3><p>{releaseLine(status)}</p><div className="system-rows"><div><span>Daemon</span><span>{status.release.running} → {status.release.latest || status.release.running}</span></div><div><span>Shell</span><span>{status.release.shellCurrent || '—'} → {status.release.shellLatest || status.release.shellCurrent || '—'}</span></div>{status.release.workflowDeferState && <div><span>Workflow update</span><span>{status.release.workflowDeferState === 'backoff' ? `deferred · retry ${untilLabel(status.release.workflowNextRetryMs, now)}` : `draining · ${status.release.workflowInflight ?? 0} in flight`}{status.release.workflowDeferAttempts ? ` · attempt ${status.release.workflowDeferAttempts}` : ''}</span></div>}<div><span>Last successful check</span><span>{status.release.lastSuccessMs ? ageLabel(status.release.lastSuccessMs, now) : '—'}</span></div><div><span>Release train</span><span>{status.release.train ? `${status.release.train.enabled ? 'on' : 'off'} · ${status.release.train.phase}` : '—'}</span></div></div>{status.release.lastError && <p className="system-error-text">{status.release.lastError}</p>}<button type="button" className="system-deep-link" onClick={() => { useStore.getState().setVersionPopoverOpen(true); onClose(); }}>Open update status</button></article> : <article className="system-card"><h3>Updates</h3><div className="system-rows"><div><span>SoluTerminal</span><span>{status.release.running} → {status.release.latest || status.release.running}</span></div><div><span>Last successful check</span><span>{status.release.lastSuccessMs ? ageLabel(status.release.lastSuccessMs, now) : '—'}</span></div></div>{status.release.lastError && <p className="system-error-text">{UPDATE_CHECK_FAILED}</p>}<button type="button" className="system-deep-link" onClick={() => { useStore.getState().setVersionPopoverOpen(true); onClose(); }}>Open update status</button></article>}
      </section>

      {showUsage && <section className="system-usage">
        <div><h3>Usage · 7 days</h3>{usage?.hero.turns ? <p><b>{usage.hero.turns.toLocaleString()}</b> turns · <b>${usage.hero.cost_usd.toFixed(2)}</b> API-equivalent estimate · top model <b>{usageTop}</b></p> : <p>Usage capture needs cloud pairing.</p>}</div>
        <button type="button" className="system-deep-link" onClick={() => { window.dispatchEvent(new CustomEvent('solu:open-usage')); onClose(); }}>Open Usage</button>
      </section>}
    </>
  );
}

export function SystemPage({ onClose }: Props) {
  return createPortal(<div className="system-page-backdrop" onClick={(event) => { if (event.target === event.currentTarget) onClose(); }}><div className="system-page" role="dialog" aria-label="System" onClick={(event) => event.stopPropagation()}><ErrorBoundary label="system"><SystemPageBody onClose={onClose} /></ErrorBoundary></div></div>, document.body);
}
