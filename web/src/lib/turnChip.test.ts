import { describe, expect, it } from 'vitest';
import type { PaneInfo, TurnRecord } from '../types';
import { paneFinishedUnseen, paneUnread, turnChipAriaLabel, turnChipModel, turnChipMaxAgeMs, turnOutputGraceMs } from './turnChip';
import { formatAgo, formatDuration } from './turnTime';

const now = 1_700_000_000_000;

function pane(p: Partial<PaneInfo> = {}): PaneInfo {
  return { id: 'pane_1', cwd: '/r', cmd: ['claude'], title: 't', status: 'running', ...p } as PaneInfo;
}

const last: TurnRecord = { n: 3, startedMs: now - 200_000, endedMs: now - 60_000, files: 2, added: 10, removed: 4 };

describe('turnChipModel', () => {
  it('hides while a turn runs (the status dot already shows work)', () => {
    expect(turnChipModel(pane({ turn: { active: true, startedMs: now - 192_000, count: 2, last } }), now)).toBeNull();
  });

  it('offers the diff when the last turn changed files', () => {
    const m = turnChipModel(pane({ turn: { active: false, count: 3, last } }), now);
    expect(m?.kind).toBe('changed');
  });

  it('says done when the last turn changed nothing', () => {
    const m = turnChipModel(pane({ turn: { active: false, count: 3, last: { ...last, files: 0, added: 0, removed: 0 } } }), now);
    expect(m?.kind).toBe('done');
  });

  it('hides stale turns, exited panes, Focus panes, and panes without turns', () => {
    const stale = { ...last, endedMs: now - turnChipMaxAgeMs - 1 };
    expect(turnChipModel(pane({ turn: { active: false, count: 3, last: stale } }), now)).toBeNull();
    expect(turnChipModel(pane({ status: 'exited', turn: { active: false, count: 3, last } }), now)).toBeNull();
    expect(turnChipModel(pane({ kind: 'native', turn: { active: false, count: 3, last } } as Partial<PaneInfo>), now)).toBeNull();
    expect(turnChipModel(pane(), now)).toBeNull();
  });
});

it('keeps an unseen expired or exited completion discoverable', () => {
  const stale = { ...last, endedMs: now - turnChipMaxAgeMs - 1 };
  const p = pane({ status: 'exited', turn: { active: false, count: 3, last: stale } });
  expect(turnChipModel(p, now)).toBeNull();
  expect(turnChipModel(p, now, true)?.turn.endedMs).toBe(stale.endedMs);
});

describe('finished-turn unread', () => {
  it('is unread only for a turn that ended after the last view', () => {
    const p = pane({ hasClaude: true, turn: { active: false, count: 3, last } });
    expect(paneFinishedUnseen(p, last.endedMs - 1)).toBe(true);
    expect(paneFinishedUnseen(p, last.endedMs + 1)).toBe(false);
  });

  it('is not unread while the next turn is still running', () => {
    const p = pane({ turn: { active: true, startedMs: now, count: 3, last } });
    expect(paneFinishedUnseen(p, 0)).toBe(false);
  });

  it('classifies unread as a finished turn, plain output, or nothing', () => {
    const turn = { active: false, count: 3, last };
    // Turn finished after the last view → 'turn', even though the agent has
    // already exited (a short-lived agent run in a shell).
    expect(paneUnread(pane({ cmd: ['bash'], turn }), last.endedMs - 1, last.endedMs)).toBe('turn');
    // Agent still running: its redraws are not news.
    expect(paneUnread(pane({ hasClaude: true, turn }), last.endedMs + 1, last.endedMs + 60_000)).toBeNull();
    // Agent gone: shell output well after the turn counts again…
    expect(paneUnread(pane({ cmd: ['bash'], turn }), last.endedMs + 1, last.endedMs + turnOutputGraceMs + 1)).toBe('output');
    // …but the turn's own tail (prompt redraw right at the end) does not.
    expect(paneUnread(pane({ cmd: ['bash'], turn }), last.endedMs + 1, last.endedMs + 1000)).toBeNull();
    // Never ran an agent turn: classic any-output rule.
    expect(paneUnread(pane({ cmd: ['bash'] }), 100, 200)).toBe('output');
    expect(paneUnread(pane({ cmd: ['bash'] }), 300, 200)).toBeNull();
  });
});

describe('turn time formatting', () => {
  it('formats durations compactly', () => {
    expect(formatDuration(42_000)).toBe('42s');
    expect(formatDuration(192_000)).toBe('3m12s');
    expect(formatDuration(3_900_000)).toBe('1h05m');
  });
  it('formats relative times', () => {
    expect(formatAgo(now - 10_000, now)).toBe('just now');
    expect(formatAgo(now - 120_000, now)).toBe('2m ago');
    expect(formatAgo(now - 3 * 3600_000, now)).toBe('3h ago');
  });
});

it('does not call interrupted or failed provider turns done', () => {
  for (const outcome of ['interrupted', 'failed'] as const) {
    expect(turnChipModel(pane({ turn: { active: false, count: 3, last: { ...last, files: 0, outcome } } }), now)?.kind).toBe(outcome);
  }
});

it('suppresses completion and unread while ownership is uncertain', () => {
  const p = pane({ hasCodex: true, turn: { active: false, uncertain: true, count: 3, last } });
  expect(turnChipModel(p, now, true)).toBeNull();
  expect(paneFinishedUnseen(p, 0)).toBe(false);
  expect(paneUnread(p, 0, now)).toBeNull();
});

it('shows recovered provider time without manufacturing new unread', () => {
  const p = pane({ hasCodex: true, turn: { active: false, count: 3, last: { ...last, historical: true, source: 'codex:session' } } });
  expect(turnChipModel(p, now)?.turn.endedMs).toBe(last.endedMs);
  expect(paneFinishedUnseen(p, 0)).toBe(false);
});

describe('turnChipAriaLabel', () => {
  it('uses the singular for exactly one changed file', () => {
    expect(turnChipAriaLabel(1)).toBe('last turn changed 1 file. Review changes');
  });

  it('uses the plural for more than one changed file', () => {
    expect(turnChipAriaLabel(2)).toBe('last turn changed 2 files. Review changes');
  });

  it('keeps the unseen prefix', () => {
    expect(turnChipAriaLabel(1, true)).toBe('New: last turn changed 1 file. Review changes');
  });
});
