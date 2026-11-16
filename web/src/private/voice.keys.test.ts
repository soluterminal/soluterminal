import { describe, expect, it } from 'vitest';
import { blocksVoiceShortcut } from './voice.keys';

describe('blocksVoiceShortcut', () => {
  it('allows the pane-owned terminal typing surface to invoke voice', () => {
    const xterm = document.createElement('textarea');
    xterm.className = 'xterm-helper-textarea';

    expect(blocksVoiceShortcut(xterm)).toBe(false);
  });

  it('still blocks dialog/rename inputs and unrelated composers', () => {
    const rename = document.createElement('input');
    const dialog = document.createElement('textarea');
    const unrelatedComposer = document.createElement('textarea');
    unrelatedComposer.className = 'composer-input';

    expect(blocksVoiceShortcut(rename)).toBe(true);
    expect(blocksVoiceShortcut(dialog)).toBe(true);
    expect(blocksVoiceShortcut(unrelatedComposer)).toBe(true);
    expect(blocksVoiceShortcut(document.createElement('div'))).toBe(false);
  });
});
