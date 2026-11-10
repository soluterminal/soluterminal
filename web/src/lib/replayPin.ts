export interface TermSize {
  cols: number;
  rows: number;
}

// The widest terminal the daemon runs (maxTerminalCols in daemon/pane.go).
// A reported replay width past it can only come from a tampered or broken
// replay-start, and resizing to it would make xterm allocate a huge buffer.
export const MAX_REPLAY_COLS = 512;

// The daemon aligns a full reattach repaint to the terminal size the client
// announced at connect. When the layout has moved since (a notice or banner
// appeared), the replay must be written at the announced size and the pane
// refitted afterwards. Exact resumes carry no repaint, so they never pin.
//
// The width is the one the daemon says the replay was produced at, when it
// says (replayCols). The replayed bytes are hard-wrapped and cursor-addressed
// for that width; written at any other width they garble. A width that is not
// a whole number of columns the daemon could run is ignored.
export function replayPinTarget(
  isResume: boolean,
  announced: TermSize,
  current: TermSize,
  replayCols = 0,
): TermSize | null {
  if (isResume) return null;
  const validReplayCols = Number.isInteger(replayCols) && replayCols > 1 && replayCols <= MAX_REPLAY_COLS;
  const cols = validReplayCols ? replayCols : announced.cols;
  if (cols <= 1 || announced.rows <= 0) return null;
  if (current.cols === cols && current.rows === announced.rows) return null;
  return { cols, rows: announced.rows };
}
