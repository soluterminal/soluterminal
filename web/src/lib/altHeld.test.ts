import { afterEach, describe, expect, it } from 'vitest';
import { installAltHeld, paneTitleHint } from './altHeld';

const held = () => 'altHeld' in document.documentElement.dataset;
const key = (type: 'keydown' | 'keyup', init: KeyboardEventInit) => window.dispatchEvent(new KeyboardEvent(type, init));

describe('installAltHeld', () => {
  let uninstall = () => {};
  afterEach(() => uninstall());

  it('marks the page while Alt is held and clears it on release', () => {
    uninstall = installAltHeld();
    expect(held()).toBe(false);
    key('keydown', { key: 'Alt', altKey: true });
    expect(held()).toBe(true);
    key('keydown', { key: '3', altKey: true });
    expect(held()).toBe(true);
    key('keyup', { key: 'Alt', altKey: false });
    expect(held()).toBe(false);
  });

  it('clears when the window loses focus, since Alt+Tab never sends the key-up', () => {
    uninstall = installAltHeld();
    key('keydown', { key: 'Alt', altKey: true });
    window.dispatchEvent(new Event('blur'));
    expect(held()).toBe(false);
  });

  it('ignores AltGr, which reports Ctrl+Alt', () => {
    uninstall = installAltHeld();
    key('keydown', { key: 'AltGraph', altKey: true, ctrlKey: true });
    expect(held()).toBe(false);
  });

  it('removes its listeners and its mark on uninstall', () => {
    uninstall = installAltHeld();
    key('keydown', { key: 'Alt', altKey: true });
    uninstall();
    expect(held()).toBe(false);
    key('keydown', { key: 'Alt', altKey: true });
    expect(held()).toBe(false);
  });
});

describe('paneTitleHint', () => {
  it('names the pane number and its shortcut', () => {
    expect(paneTitleHint(3)).toBe('#3 · Alt+3 focuses this pane · Double-click to rename');
  });
  it('says only how to rename when the pane has no number', () => {
    expect(paneTitleHint(0)).toBe('Double-click to rename');
  });
});
