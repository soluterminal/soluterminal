import type { BrowserInfo, CreatePaneReq, FileTileInfo, Layout, PaneDef, PaneInfo, Workspace } from '../types';
import { collectBrowserIds, collectFileIds, collectPaneIds } from './layout';

// Extract inline pane defs for the given layout. Used by both Save As and
// the _current autosave so workspaces are restorable after pane deaths.
export function collectPaneDefs(layout: Layout | null, panes: Record<string, PaneInfo>): PaneDef[] {
  const ids = collectPaneIds(layout);
  const defs: PaneDef[] = [];
  for (const id of ids) {
    const p = panes[id];
    if (!p) continue;
    defs.push({
      id: p.id,
      cwd: p.cwd,
      cmd: paneRecoveryCommand(p),
      identityBlocked: p.identityBlocked,
      title: p.title,
      cols: p.cols,
      rows: p.rows,
      flagged: p.flagged,
      flagMemo: p.flagMemo,
      claudeSessionId: p.claudeSessionId,
      kind: p.kind === 'native' ? 'native' : undefined,
      agent: p.agent,
      codexThreadId: p.codexThreadId,
      museSessionId: p.museSessionId,
      provider: p.provider,
      effort: p.effort,
      model: p.model,
      role: p.role,
      contextPack: p.contextPack,
      neverAutoAnswer: p.neverAutoAnswer,
      titleOverride: p.titleOverride,
      origin: p.origin,
      identity: p.identity,
    });
  }
  return defs;
}

export function collectBrowserInfos(layout: Layout | null, browsers: Record<string, BrowserInfo>): BrowserInfo[] {
  const ids = collectBrowserIds(layout);
  const out: BrowserInfo[] = [];
  for (const id of ids) {
    const b = browsers[id];
    if (b) out.push(b);
  }
  return out;
}

export function collectFileInfos(layout: Layout | null, files: Record<string, FileTileInfo>): FileTileInfo[] {
  const ids = collectFileIds(layout);
  const out: FileTileInfo[] = [];
  for (const id of ids) {
    const f = files[id];
    if (f) out.push(f);
  }
  return out;
}

export function pickWorkspaceByName(workspaces: Workspace[], name: string, backend: 'wsl' | 'win' | null): Workspace | null {
  const matches = workspaces.filter((w) => w.name === name);
  if (matches.length === 0) return null;
  if (backend) {
    const sameBackend = matches.find((w) => w.backend === backend);
    if (sameBackend) return sameBackend;
  }
  return matches[0];
}

export function isRunningPane(p: PaneInfo | undefined): p is PaneInfo {
  return p?.status === 'running';
}

// A pane a layout should KEEP: running, or suspended-resumable. After a full
// teardown (Windows reboot / wsl --shutdown) the daemon's cold restore parks
// every dead-agent Claude pane as exited+resumable (SuspendExisting) so the
// user resumes IN PLACE via the pane's "Resume session" affordance. Layout
// alive-checks must treat those as present: judging them by isRunningPane made
// bootstrap/loadWorkspace prune them and respawn a DUPLICATE pane from the
// saved def's stale argv (a fresh claude that "lost" the conversation, or a
// second `claude --resume` of a session another pane owns), stranding the
// suspended pane — with its accurate session id — outside every workspace.
export function isPresentPane(p: PaneInfo | undefined): p is PaneInfo {
  if (!p) return false;
  return !!p.recoveryError || p.status === 'running' || (p.status === 'exited' && !!p.resumable);
}

// Synthesize an optimistic, "running" PaneInfo from a saved PaneDef. Used by
// the cached-first workspace switch to seed the destination's panes when the
// carried-over global `panes` cache hasn't yet globalized them (the 3s
// refreshPanes poll lag — e.g. a workspace's slow /mnt/c claude-resume panes).
// Seeding prevents resolveWorkspaceInitialLayout from pruning the layout to
// empty (→ null focus → new-pane cwd detection failing) WITHOUT a blocking
// listPanes round-trip on the switch. The placeholder fields (epoch createdAt,
// pid 0, idle state) are overwritten by Phase 2's fresh listPanes and the next
// 3s poll; the cwd/cmd/title come straight from the saved def, so the pane is
// functionally correct (and its cwd is right) during the brief seed window.
// `kind` is carried from the def so a native pane seeds AS native (no
// terminal-renders-JSONL flash); it's undefined for terminals.
export function optimisticPaneFromDef(def: PaneDef): PaneInfo {
  return {
    id: def.id,
    cwd: def.cwd,
    cmd: def.cmd,
    title: def.title,
    status: 'running',
    createdAt: new Date(0).toISOString(),
    cols: def.cols ?? 80,
    rows: def.rows ?? 24,
    pid: 0,
    lastActivityMs: 0,
    state: 'idle',
    hasClaude: false,
    flagged: def.flagged,
    flagMemo: def.flagMemo,
    claudeSessionId: def.claudeSessionId,
    kind: def.kind,
    agent: def.agent,
    codexThreadId: def.codexThreadId,
    museSessionId: def.museSessionId,
    identityBlocked: def.identityBlocked,
    provider: def.provider,
    effort: def.effort,
    model: def.model,
    role: def.role,
    contextPack: def.contextPack,
    neverAutoAnswer: def.neverAutoAnswer,
    titleOverride: def.titleOverride,
    origin: def.origin,
    identity: def.identity,
  };
}

// Remove from `aliveById` any optimistic seed (id in `seededIds`) that the fresh
// listPanes did NOT confirm — it's genuinely gone, so the caller's respawn loop
// recreates it or retains a recovery tile. Confirmed seeds were already replaced
// with real PaneInfo by the caller before this runs. A failed fetch leaves
// seeds untouched. Only an authoritative single-backend empty list confirms
// absence; an empty aggregate can mean that a router backend is offline.
export function dropUnconfirmedSeeds(
  aliveById: Map<string, PaneInfo>,
  seededIds: Set<string>,
  freshPanes: PaneInfo[],
  authoritativeEmpty = false,
): void {
  if (seededIds.size === 0 || (freshPanes.length === 0 && !authoritativeEmpty)) return;
  const freshIds = new Set(freshPanes.map((p) => p.id));
  for (const id of seededIds) {
    if (!freshIds.has(id)) aliveById.delete(id);
  }
}

// Save a provider resume recipe for a conversation opened inside a shell.
// Keep ambiguous identities blocked; never infer a thread from directory recency.
export function paneRecoveryCommand(p: PaneInfo): string[] {
  if (p.kind === 'native' || p.identityBlocked) return p.cmd;
  const executable = p.cmd?.[0]?.split(/[\\/]/).pop()?.toLowerCase().replace(/\.exe$/, '');
  if (p.agent === 'codex' && p.codexThreadId && executable !== 'codex') return ['codex', 'resume', p.codexThreadId];
  if (p.agent === 'muse' && p.museSessionId && executable !== 'muse') return ['muse', 'resume', p.museSessionId];
  if (p.agent !== 'codex' && p.agent !== 'muse' && p.claudeSessionId && executable !== 'claude') return ['claude', '--resume', p.claudeSessionId];
  return p.cmd;
}

export function paneRestoreRequest(def: PaneDef): CreatePaneReq {
  const { id: _id, flagged: _flagged, flagMemo: _memo, ...recipe } = def;
  return { ...recipe, restore: true };
}

export function failedPaneFromDef(def: PaneDef, error: unknown): PaneInfo {
  return {
    ...optimisticPaneFromDef(def), status: 'exited', resumable: true,
    restorePending: true,
    recoveryError: error instanceof Error ? error.message : String(error),
  };
}

// Only an explicit click may open the provider's conversation picker.
export function paneRetryRequest(p: PaneInfo): CreatePaneReq {
  const def = collectPaneDefs({ type: 'pane', paneId: p.id }, { [p.id]: p })[0];
  const req = paneRestoreRequest(def);
  const conversationId = p.agent === 'codex' ? p.codexThreadId : p.agent === 'muse' ? p.museSessionId : p.claudeSessionId;
  if (p.kind !== 'native' && (p.identityBlocked || !conversationId)) {
    req.codexThreadId = req.claudeSessionId = req.museSessionId = undefined;
    req.identityBlocked = undefined;
    if (p.agent === 'codex') {
      req.cmd = ['codex', '--no-alt-screen', 'resume', '--all']; req.restore = false;
    } else if (p.agent === 'claude' || p.cmd.some((part) => /(^|[\\/])claude(?:\.exe)?$/.test(part))) {
      req.cmd = ['claude', '--resume']; req.restore = false;
    }
  }
  return req;
}
