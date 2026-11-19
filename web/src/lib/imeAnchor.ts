// Keep xterm's helper textarea on the cursor cell so a desktop IME (Windows
// pinyin, Japanese, Korean...) opens its candidate bar at the caret.
//
// xterm 5.5 moves the textarea only from onCursorMove (Terminal._syncTextArea)
// and, once composing, from onRender. Until the cursor moves after open, the
// textarea keeps its stylesheet resting place (left:-9999em; top:0), which is
// outside the window: Windows clamps the candidate bar to the window's
// top-left corner. An idle Claude Code pane can sit like that indefinitely
// (no output, so no cursor move), and a move made while the renderer had no
// cell size yet leaves it at the pane's 0,0. Feedback #73 / ST-306.
//
// anchorImeTextarea repeats xterm's own placement from the public buffer API
// (the same cell as xterm's when the view is at the bottom; scrolled back,
// xterm leaves the textarea alone and this keeps it on the nearest visible
// row), and is called on focus, on an IME keydown and at compositionstart,
// before the IME asks for the caret.

import type { Terminal } from '@xterm/xterm';

interface CellSize { width: number; height: number }

function cellSize(term: Terminal): CellSize | null {
  // Private name verified for xterm 5.5 (underscore-prefixed renderService).
  const dims = (term as unknown as {
    _core?: { _renderService?: { dimensions?: { css?: { cell?: CellSize } } } };
  })._core?._renderService?.dimensions?.css?.cell;
  if (dims && dims.width > 0 && dims.height > 0) return { width: dims.width, height: dims.height };
  // Fallback: the screen element is cols x rows cells in layout pixels.
  const screen = term.element?.querySelector<HTMLElement>('.xterm-screen');
  if (!screen || term.cols <= 0 || term.rows <= 0) return null;
  const width = screen.offsetWidth / term.cols;
  const height = screen.offsetHeight / term.rows;
  return width > 0 && height > 0 ? { width, height } : null;
}

/** Place term.textarea on the cursor cell. Returns false (and leaves the
 *  textarea alone) when the terminal has no usable cell size yet. */
export function anchorImeTextarea(term: Terminal): boolean {
  const ta = term.textarea;
  if (!ta) return false;
  const cell = cellSize(term);
  if (!cell) return false;
  const buf = term.buffer.active;
  const col = Math.max(0, Math.min(buf.cursorX, term.cols - 1));
  // xterm draws the cursor row relative to the bottom of the buffer. When the
  // view is scrolled into history the cursor is off-screen: keep the bar
  // inside the pane on the nearest visible row instead of off the window.
  const rowInView = buf.baseY + buf.cursorY - buf.viewportY;
  const row = Math.max(0, Math.min(rowInView, term.rows - 1));
  const left = `${col * cell.width}px`;
  const top = `${row * cell.height}px`;
  if (ta.style.left !== left) ta.style.left = left;
  if (ta.style.top !== top) ta.style.top = top;
  // Never 0x0: some IMEs ignore a zero-size caret rect (xterm uses >= 1px).
  ta.style.width = `${Math.max(cell.width, 1)}px`;
  ta.style.height = `${Math.max(cell.height, 1)}px`;
  ta.style.lineHeight = `${cell.height}px`;
  return true;
}

/** True for the keydown a Windows/Chromium IME sends before compositionstart
 *  (keydowns inside a composition are left to xterm, which sizes the textarea
 *  to the composition text). */
export function isImeKeydown(e: KeyboardEvent): boolean {
  return !e.isComposing && (e.keyCode === 229 || e.key === 'Process');
}

/** Wire the anchor to term's textarea. Returns the teardown. */
export function attachImeAnchor(
  term: Terminal,
  opts: { enabled: () => boolean; onCompositionStart?: (ta: HTMLTextAreaElement) => void },
): () => void {
  const ta = term.textarea;
  if (!ta) return () => {};
  // While composing xterm owns the placement (it widens the textarea to the
  // composition text); re-anchoring then would shrink it back to one cell.
  let composing = false;
  const anchor = () => { if (!composing && opts.enabled()) anchorImeTextarea(term); };
  const onKeydown = (e: KeyboardEvent) => { if (isImeKeydown(e)) anchor(); };
  const onCompositionStart = () => {
    composing = false;
    anchor();
    composing = true;
    opts.onCompositionStart?.(ta);
  };
  const onCompositionEnd = () => { composing = false; };
  // Capture on the target runs before xterm's own (bubble) listeners.
  ta.addEventListener('focus', anchor);
  ta.addEventListener('keydown', onKeydown, { capture: true });
  ta.addEventListener('compositionstart', onCompositionStart, { capture: true });
  ta.addEventListener('compositionend', onCompositionEnd);
  ta.addEventListener('blur', onCompositionEnd);
  // A layout change (split, sidebar, fit) while focused moves the cursor cell
  // but not the textarea until the next cursor move.
  const resize = term.onResize(() => { if (document.activeElement === ta) anchor(); });
  return () => {
    ta.removeEventListener('focus', anchor);
    ta.removeEventListener('keydown', onKeydown, { capture: true });
    ta.removeEventListener('compositionstart', onCompositionStart, { capture: true });
    ta.removeEventListener('compositionend', onCompositionEnd);
    ta.removeEventListener('blur', onCompositionEnd);
    resize.dispose();
  };
}
