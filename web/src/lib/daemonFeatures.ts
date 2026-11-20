import type { Health } from '../types';

// Capabilities a daemon advertises in /api/health `features`
// (shared/daemonfeatures). A desktop setup can update the UI while the
// running daemon stays a version behind, so a new UI action is offered only
// when the daemon it talks to names it (ST-315).

/** The daemon serves /api/panes/{id}/recording(s). */
export const PANE_RECORDING = 'pane-recording.v1';

/** Whether the daemon behind this health report serves feature. Unknown (no health yet, or an older daemon) is no. */
export function daemonSupports(health: Health | null | undefined, feature: string): boolean {
  return !!health?.features?.includes(feature);
}
