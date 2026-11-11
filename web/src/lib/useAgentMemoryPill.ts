import { useEffect, useState, useSyncExternalStore } from 'react';
import { api, onRoutingScopeChange } from '../api';
import { useStore } from '../store';
import type { PaneInfo } from '../types';
import { agentMemoryGrowingFast, agentMemoryLabel, paneAgentRssMB, recordAgentMemory, UNUSUAL_FLOOR_MB, unusualMemoryThresholdMB } from './agentMemory';
import { getSystemStatusSnapshot } from './useSystemStatus';

// The machine's RAM, read once and only when some agent passes the 1.5 GB
// floor: under it, the 10% rule cannot matter. undefined = not known yet;
// null = the backend did not say, so the floor alone applies.
let hostMemTotalMB: number | null | undefined;
let loading = false;
let generation = 0;
const listeners = new Set<() => void>();
const emit = () => listeners.forEach(listener => listener());

function loadHostMemTotal() {
  if (hostMemTotalMB !== undefined || loading) return;
  const known = getSystemStatusSnapshot().status?.host.memTotalMB;
  if (known) { hostMemTotalMB = known; emit(); return; }
  loading = true;
  const requestGeneration = generation;
  const done = (mb: number | null) => {
    if (requestGeneration !== generation) return;
    loading = false;
    hostMemTotalMB = mb;
    emit();
  };
  api.systemStatus().then(s => done(s.host.memTotalMB ?? null), () => done(null));
}

// Another backend is another machine.
onRoutingScopeChange(() => {
  generation += 1;
  loading = false;
  hostMemTotalMB = undefined;
  emit();
});

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

export function resetHostMemoryForTests(): void {
  generation += 1;
  loading = false;
  hostMemTotalMB = undefined;
}

export interface AgentMemoryPillState { mb: number; label: string; unusual: boolean; show: boolean }

// Whether a pane header shows its agent's memory. Unusual memory always shows
// (on a phone too) and offers a restart; "Always show agent memory" adds a
// plain readout on desktop.
export function useAgentMemoryPill(pane: PaneInfo, isMobileView: boolean): AgentMemoryPillState {
  const mb = paneAgentRssMB(pane);
  const alwaysShow = useStore(s => s.alwaysShowAgentMemory);
  const hostMB = useSyncExternalStore(subscribe, () => hostMemTotalMB);
  const pastFloor = mb >= UNUSUAL_FLOOR_MB;
  // hostMB returns to undefined on a machine switch, so a pane already past
  // the floor asks the new machine too.
  useEffect(() => { if (pastFloor && hostMB === undefined) loadHostMemTotal(); }, [pastFloor, hostMB]);
  const [fast, setFast] = useState(() => agentMemoryGrowingFast(pane.id));
  useEffect(() => { setFast(recordAgentMemory(pane.id, mb)); }, [pane.id, mb]);
  const unusual = mb > 0 && (fast || (pastFloor && hostMB !== undefined && mb >= unusualMemoryThresholdMB(hostMB)));
  return { mb, label: agentMemoryLabel(mb), unusual, show: mb > 0 && (unusual || (alwaysShow && !isMobileView)) };
}
