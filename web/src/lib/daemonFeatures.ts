import type { Health } from '../types';

// Capabilities a daemon advertises in /api/health `features`
// (shared/daemonfeatures). A desktop setup can update the UI while the
// running daemon stays a version behind, so a new UI action is offered only
// when the daemon it talks to names it (ST-315).

/** The daemon serves /api/panes/{id}/recording(s). */
export const PANE_RECORDING = 'pane-recording.v1';

/** The daemon takes a time limit and per-pane overrides at switch-on, reports
 * the rule check on the status route, and serves the workspace switch and the
 * recording settings (ST-317). */
export const PANE_RECORDING_V2 = 'pane-recording.v2';

/** While anything records the daemon keeps the session layout timeline and
 * takes view records on /api/recording/view (ST-317). */
export const PANE_RECORDING_TIMELINE = 'pane-recording-timeline.v1';

/** Whether the daemon behind this health report serves feature. Unknown (no health yet, or an older daemon) is no. */
export function daemonSupports(health: Health | null | undefined, feature: string): boolean {
  return !!health?.features?.includes(feature);
}
