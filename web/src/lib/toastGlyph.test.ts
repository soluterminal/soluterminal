import { describe, expect, it } from 'vitest';
import { splitDaemonToastGlyph } from './toastGlyph';

describe('splitDaemonToastGlyph', () => {
  it.each([
    ['🤖 Fix agent: apply exit 0 in 2m14s', { message: 'Fix agent: apply exit 0 in 2m14s', icon: '🤖' }],
    ['◇ Docs agent: ok in 40s', { message: 'Docs agent: ok in 40s', icon: '◇' }],
    ['⏰ Morning digest → Build', { message: 'Morning digest → Build', icon: '⏰' }],
    ['🦉 Agent: commit done', { message: 'Agent: commit done', icon: '🦉' }],
    ['🧹 Go build cache trimmed — 1.2 GB reclaimed', { message: 'Go build cache trimmed — 1.2 GB reclaimed' }],
  ])('strips and classifies %s', (text, expected) => {
    expect(splitDaemonToastGlyph(text)).toEqual(expected);
  });

  it.each(['⚠️ Fix agent: apply exit 1', '⚠ Fix agent: apply exit 1'])(
    'maps warning prefix variants in %s to warn kind',
    (text) => {
      expect(splitDaemonToastGlyph(text)).toEqual({
        message: 'Fix agent: apply exit 1',
        kind: 'warn',
      });
    },
  );

  it('passes glyph-free text through unchanged', () => {
    expect(splitDaemonToastGlyph('Fix agent: all clear')).toEqual({ message: 'Fix agent: all clear' });
  });

  it('never strips a known glyph in the middle of text', () => {
    const text = 'Agent said 🤖 hello';
    expect(splitDaemonToastGlyph(text)).toEqual({ message: text });
  });
});
