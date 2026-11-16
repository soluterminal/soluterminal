import type { Schedule, ScheduleChange } from '../types';

// Who made a recorded change, in words that finish "... by <who>". The daemon
// records the person as "person" from the app, an agent as its pane's @handle
// or label through its tools, and itself as "SoluTerminal" with a reason.
export function changeActor(c: ScheduleChange): string {
  if (c.source === 'ui' && c.by === 'person') return 'you, in the app';
  if (c.source === 'mcp') return `the agent in ${c.by}`;
  return c.reason ? `${c.by} (${c.reason})` : c.by;
}

const ACTION_WORDS: Record<ScheduleChange['action'], string> = {
  enabled: 'Turned on',
  disabled: 'Paused',
  deleted: 'Deleted',
};

function changeTime(at: string): string {
  const d = new Date(at);
  return isNaN(d.getTime())
    ? 'at an unknown time'
    : d.toLocaleString(undefined, { weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });
}

// One line for the automation's details: "Paused Sat 14 Nov, 18:20 by the
// agent in @cmo".
export function describeChange(c: ScheduleChange): string {
  return `${ACTION_WORDS[c.action] ?? c.action} ${changeTime(c.at)} by ${changeActor(c)}`;
}

// The record of the pause a switched-off automation is in now. Null when it
// is on, or when it was paused before changes were recorded.
export function pausedChange(s: Schedule): ScheduleChange | null {
  const c = s.changes?.[0];
  return !s.enabled && c && c.action === 'disabled' ? c : null;
}

// "since Sat 14 Nov, 18:20 by the agent in @cmo", or "" without a record.
export function pausedSince(s: Schedule): string {
  const c = pausedChange(s);
  return c ? `since ${changeTime(c.at)} by ${changeActor(c)}` : '';
}
