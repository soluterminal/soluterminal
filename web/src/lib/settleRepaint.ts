// Settle repaint — the self-heal for leftover, overlapping letters in xterm's
// default renderer (issue #45, worst on phones and scaled displays). Rows the
// renderer drew mid-burst can keep stale glyphs at fractional pixel ratios; one
// full viewport refresh once output, scroll or resize has gone quiet repaints
// them from the buffer.
//
// Trailing debounce only: every poke pushes the repaint back, so a burst of
// output never repaints until it stops. A stream that never pauses (a spinner)
// simply waits; the repaint is a clean-up, not a frame.

export const SETTLE_REPAINT_DELAY_MS = 250;

export interface SettleRepaint {
  /** Output, scroll or resize happened: (re)start the quiet timer. */
  poke: () => void;
  /** Cancel any pending repaint and ignore later pokes. */
  dispose: () => void;
}

export function createSettleRepaint(
  repaint: () => void,
  delayMs = SETTLE_REPAINT_DELAY_MS,
): SettleRepaint {
  let timer: number | null = null;
  let disposed = false;
  return {
    poke: () => {
      if (disposed) return;
      if (timer !== null) window.clearTimeout(timer);
      timer = window.setTimeout(() => {
        timer = null;
        if (disposed) return;
        try { repaint(); } catch { /* renderer may already be disposed */ }
      }, delayMs);
    },
    dispose: () => {
      disposed = true;
      if (timer !== null) window.clearTimeout(timer);
      timer = null;
    },
  };
}
