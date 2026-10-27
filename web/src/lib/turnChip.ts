import type { PaneInfo, TurnRecord } from '../types';

// How long a finished turn keeps its header chip. Past this the "done 5h ago"
// readout is noise; the turn stays reachable from the pane menu.
export const turnChipMaxAgeMs = 3 * 60 * 60 * 1000;

// The chip shows the LAST FINISHED turn only. While a turn runs the status
// dot already pulses and the agent's own TUI shows its elapsed time, so a
// header timer would just repeat both — the chip hides until the turn ends.
export type TurnChipModel =
  | { kind: 'changed'; turn: TurnRecord }
  | { kind: 'done' | 'interrupted' | 'failed'; turn: TurnRecord };

export function turnChipModel(pane: PaneInfo, now: number, unseen = false): TurnChipModel | null {
  if (pane.kind === 'native' || (pane.status !== 'running' && !unseen)) return null;
  const t = pane.turn;
  if (!t || t.active || t.uncertain) return null;
  const last = t.last;
  if (!last || (!unseen && now - last.endedMs > turnChipMaxAgeMs)) return null;
  if (last.files === 0 && (last.outcome === 'interrupted' || last.outcome === 'failed')) return { kind: last.outcome, turn: last };
  return last.files > 0 ? { kind: 'changed', turn: last } : { kind: 'done', turn: last };
}

// Screen-reader label for the turn chip. The file count needs a singular form
// for exactly one changed file ("changed 1 file"), otherwise it reads
// "changed 1 files".
export function turnChipAriaLabel(files: number, unseen = false): string {
  const prefix = unseen ? 'New: ' : '';
  return `${prefix}last turn changed ${files} file${files === 1 ? '' : 's'}. Review changes`;
}

// paneFinishedUnseen: the agent finished a turn after the user last looked at
// the pane. Shown as the chip's "new" highlight.
export function paneFinishedUnseen(pane: PaneInfo, lastViewedMs: number): boolean {
  const last = pane.turn?.last;
  if (!last || last.historical || pane.turn?.active || pane.turn?.uncertain) return false;
  return last.endedMs > lastViewedMs;
}

// Output that lands this long after a turn's recorded end is no longer that
// turn's tail (prompt redraw, the agent exiting back to the shell).
export const turnOutputGraceMs = 5_000;

export type PaneUnread = 'turn' | 'output' | null;

// paneUnread decides what — if anything — a pane has that you haven't seen.
//   'turn'   → an agent turn finished since you looked (chip "new" tag)
//   'output' → plain terminal output since you looked (the small dot)
// While an agent runs (or is mid-turn) only finished turns count: spinners
// and status redraws are not news. Once the agent has exited, ordinary shell
// output after the last turn counts again, so a pane that ran Claude once
// still behaves like a shell afterwards. Panes the daemon never saw an agent
// turn in keep the classic any-output rule.
export function paneUnread(pane: PaneInfo, lastViewedMs: number, activityMs: number): PaneUnread {
  if (paneFinishedUnseen(pane, lastViewedMs)) return 'turn';
  const t = pane.turn;
  if (!t) return activityMs > lastViewedMs && activityMs > 0 ? 'output' : null;
  if (t.active || pane.hasClaude || pane.hasCodex || pane.hasMuse) return null;
  const after = Math.max(lastViewedMs, (t.last?.endedMs ?? 0) + turnOutputGraceMs);
  return activityMs > after ? 'output' : null;
}
