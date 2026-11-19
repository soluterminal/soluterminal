import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Terminal } from '@xterm/xterm';
import { anchorImeTextarea, attachImeAnchor, isImeKeydown } from './imeAnchor';

afterEach(() => document.body.replaceChildren());

type Fake = {
  term: Terminal;
  ta: HTMLTextAreaElement;
  buffer: { cursorX: number; cursorY: number; baseY: number; viewportY: number };
  cell: { width: number; height: number };
  fireResize: () => void;
};

function fakeTerm(): Fake {
  const element = document.createElement('div');
  const ta = document.createElement('textarea');
  ta.className = 'xterm-helper-textarea';
  element.append(ta);
  document.body.append(element);
  const buffer = { cursorX: 4, cursorY: 2, baseY: 0, viewportY: 0 };
  const cell = { width: 8, height: 17 };
  const resizeListeners: Array<() => void> = [];
  const term = {
    element,
    textarea: ta,
    cols: 80,
    rows: 24,
    buffer: { active: buffer },
    onResize: (cb: () => void) => {
      resizeListeners.push(cb);
      return { dispose: () => resizeListeners.splice(resizeListeners.indexOf(cb), 1) };
    },
    _core: { _renderService: { dimensions: { css: { cell } } } },
  } as unknown as Terminal;
  return { term, ta, buffer, cell, fireResize: () => resizeListeners.forEach((l) => l()) };
}

describe('anchorImeTextarea', () => {
  it('puts the textarea on the cursor cell, as xterm does on a cursor move', () => {
    const { term, ta } = fakeTerm();
    expect(anchorImeTextarea(term)).toBe(true);
    expect(ta.style.left).toBe('32px');
    expect(ta.style.top).toBe('34px');
    expect(ta.style.width).toBe('8px');
    expect(ta.style.height).toBe('17px');
  });

  it('keeps the bar inside the pane when the view is scrolled into history', () => {
    const { term, ta, buffer } = fakeTerm();
    Object.assign(buffer, { baseY: 500, cursorY: 20, viewportY: 100 });
    anchorImeTextarea(term);
    expect(ta.style.top).toBe(`${23 * 17}px`);
  });

  it('clamps a pending-wrap cursor to the last column', () => {
    const { term, ta, buffer } = fakeTerm();
    buffer.cursorX = 80;
    anchorImeTextarea(term);
    expect(ta.style.left).toBe(`${79 * 8}px`);
  });

  it('leaves the textarea alone until the renderer has a cell size', () => {
    const { term, ta, cell } = fakeTerm();
    cell.width = 0;
    ta.style.left = '-9999em';
    expect(anchorImeTextarea(term)).toBe(false);
    expect(ta.style.left).toBe('-9999em');
  });
});

describe('isImeKeydown', () => {
  it('matches the Process/229 keydown before a composition, not ones inside it', () => {
    expect(isImeKeydown(new KeyboardEvent('keydown', { key: 'Process' }))).toBe(true);
    expect(isImeKeydown(new KeyboardEvent('keydown', { keyCode: 229 } as KeyboardEventInit))).toBe(true);
    expect(isImeKeydown(new KeyboardEvent('keydown', { key: 'Process', isComposing: true }))).toBe(false);
    expect(isImeKeydown(new KeyboardEvent('keydown', { key: 'a' }))).toBe(false);
  });
});

describe('attachImeAnchor', () => {
  it('anchors an idle pane (never moved since open) on focus and at compositionstart', () => {
    const { term, ta, buffer } = fakeTerm();
    // xterm's stylesheet resting place: off the window's left edge.
    ta.style.left = '-9999em';
    ta.style.top = '0px';
    const onStart = vi.fn();
    attachImeAnchor(term, { enabled: () => true, onCompositionStart: onStart });
    ta.focus();
    expect(ta.style.left).toBe('32px');
    buffer.cursorX = 10;
    ta.dispatchEvent(new CompositionEvent('compositionstart'));
    expect(ta.style.left).toBe('80px');
    expect(onStart).toHaveBeenCalledWith(ta);
  });

  it('anchors on the IME keydown that precedes compositionstart', () => {
    const { term, ta } = fakeTerm();
    attachImeAnchor(term, { enabled: () => true });
    ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Process' }));
    expect(ta.style.left).toBe('32px');
  });

  it('leaves placement to xterm while composing', () => {
    const { term, ta, buffer } = fakeTerm();
    attachImeAnchor(term, { enabled: () => true });
    ta.dispatchEvent(new CompositionEvent('compositionstart'));
    ta.style.width = '48px'; // xterm widens it to the composition text
    buffer.cursorX = 20;
    ta.focus();
    ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Process' }));
    expect(ta.style.width).toBe('48px');
    ta.dispatchEvent(new CompositionEvent('compositionend'));
    ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Process' }));
    expect(ta.style.left).toBe('160px');
  });

  it('re-anchors after a resize only while focused', () => {
    const { term, ta, buffer, fireResize } = fakeTerm();
    attachImeAnchor(term, { enabled: () => true });
    fireResize();
    expect(ta.style.left).toBe('');
    ta.focus();
    buffer.cursorX = 1;
    fireResize();
    expect(ta.style.left).toBe('8px');
  });

  it('does nothing when disabled (phones pin the textarea instead) and detaches cleanly', () => {
    const { term, ta } = fakeTerm();
    let enabled = false;
    const detach = attachImeAnchor(term, { enabled: () => enabled });
    ta.focus();
    expect(ta.style.left).toBe('');
    enabled = true;
    detach();
    ta.blur();
    ta.focus();
    ta.dispatchEvent(new CompositionEvent('compositionstart'));
    expect(ta.style.left).toBe('');
  });
});
