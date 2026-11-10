import { describe, expect, it } from 'vitest';
import { MAX_REPLAY_COLS, replayPinTarget } from './replayPin';

describe('replayPinTarget', () => {
  it('pins a full replay to the announced size after the pane shrank', () => {
    expect(replayPinTarget(false, { cols: 130, rows: 39 }, { cols: 130, rows: 35 })).toEqual({
      cols: 130,
      rows: 39,
    });
  });

  it('does not pin when the size is unchanged', () => {
    expect(replayPinTarget(false, { cols: 130, rows: 39 }, { cols: 130, rows: 39 })).toBeNull();
  });

  it('never pins an exact resume', () => {
    expect(replayPinTarget(true, { cols: 130, rows: 39 }, { cols: 130, rows: 35 })).toBeNull();
  });

  it('ignores an unknown announced size', () => {
    expect(replayPinTarget(false, { cols: 0, rows: 0 }, { cols: 130, rows: 35 })).toBeNull();
  });

  it('writes the replay at the width the daemon produced it at', () => {
    // A workspace switch measured the host mid-layout and floored it to 40
    // columns; the PTY (and so the replayed bytes) is 220 wide.
    expect(replayPinTarget(false, { cols: 40, rows: 30 }, { cols: 40, rows: 30 }, 220)).toEqual({
      cols: 220,
      rows: 30,
    });
  });

  it('does not pin when the terminal already has the replay width', () => {
    expect(replayPinTarget(false, { cols: 40, rows: 30 }, { cols: 220, rows: 30 }, 220)).toBeNull();
  });

  it('falls back to the announced width when the daemon sends none', () => {
    expect(replayPinTarget(false, { cols: 54, rows: 25 }, { cols: 40, rows: 25 }, 0)).toEqual({
      cols: 54,
      rows: 25,
    });
  });

  it('ignores a replay width the daemon could never run', () => {
    // A tampered replay-start must not make xterm allocate a huge terminal.
    for (const bad of [MAX_REPLAY_COLS + 1, 100000, 220.5, Number.NaN, Number.POSITIVE_INFINITY]) {
      expect(replayPinTarget(false, { cols: 54, rows: 25 }, { cols: 40, rows: 25 }, bad)).toEqual({
        cols: 54,
        rows: 25,
      });
    }
  });

  it('accepts the widest terminal the daemon runs', () => {
    expect(replayPinTarget(false, { cols: 54, rows: 25 }, { cols: 40, rows: 25 }, MAX_REPLAY_COLS)).toEqual({
      cols: MAX_REPLAY_COLS,
      rows: 25,
    });
  });

  it('never pins an exact resume, even with a replay width', () => {
    expect(replayPinTarget(true, { cols: 40, rows: 30 }, { cols: 40, rows: 30 }, 220)).toBeNull();
  });
});
