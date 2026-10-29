import { describe, expect, it } from 'vitest';
import {
  DEFAULT_HIDDEN,
  DEFAULT_MIDDLE_ORDER,
  MIDDLE_SECTION_LABELS,
  effectiveMiddleOrder,
  isMiddleKey,
  isUnclaimedSidebarSpace,
  reorderMiddle,
  resolvedMiddleOrder,
  withoutTeamOnly,
} from './sidebarSections';

describe('resolvedMiddleOrder', () => {
  it('returns the default order when nothing is saved', () => {
    expect(resolvedMiddleOrder(undefined)).toEqual(DEFAULT_MIDDLE_ORDER);
  });

  it('honors the saved order and appends never-ordered (new) sections in default position', () => {
    // User only ever ordered these two; the others are "new" to them.
    const out = resolvedMiddleOrder(['files', 'ports']);
    expect(out.slice(0, 2)).toEqual(['files', 'ports']);
    // Every known section still appears exactly once.
    expect([...out].sort()).toEqual([...DEFAULT_MIDDLE_ORDER].sort());
    // The remainder keep their default relative order.
    expect(out.slice(2)).toEqual(
      DEFAULT_MIDDLE_ORDER.filter((k) => k !== 'files' && k !== 'ports'),
    );
  });

  it('drops unknown keys (incl. the retired usage section) and de-duplicates', () => {
    const out = resolvedMiddleOrder(['bogus', 'files', 'files', 'usage', 'ports']);
    expect(out.slice(0, 2)).toEqual(['files', 'ports']);
    expect(out).toHaveLength(DEFAULT_MIDDLE_ORDER.length);
  });
  it('drops the retired todos section from saved order and hidden lists', () => {
    expect(resolvedMiddleOrder(['todos', 'files'])).not.toContain('todos');
    expect(DEFAULT_MIDDLE_ORDER).not.toContain('todos');
    expect(isMiddleKey('todos')).toBe(false);
  });
});

// docs/DESIGN-agents-surface.md §1.3: the Agents section sits beside Schedules
// and must NOT default hidden — a surface whose whole purpose is "you can see
// what runs unattended" is useless if the user has to find it first.
describe('the Agents section', () => {
  it('sits immediately after schedules in the default order', () => {
    const schedules = DEFAULT_MIDDLE_ORDER.indexOf('schedules');
    expect(schedules).toBeGreaterThanOrEqual(0);
    expect(DEFAULT_MIDDLE_ORDER[schedules + 1]).toBe('agents');
  });

  it('is labeled for the customizer and the right-click menu', () => {
    expect(MIDDLE_SECTION_LABELS.agents).toBe('Agents');
  });

  it('is not hidden by default', () => {
    expect(DEFAULT_HIDDEN).not.toContain('agents');
    expect(effectiveMiddleOrder(undefined, undefined)).toContain('agents');
  });

  it('appears for a user whose saved order predates it, with no migration', () => {
    const saved = DEFAULT_MIDDLE_ORDER.filter((k) => k !== 'agents');
    const out = resolvedMiddleOrder(saved);
    expect(out).toContain('agents');
    expect(out).toHaveLength(DEFAULT_MIDDLE_ORDER.length);
    // Their explicit order is preserved; the new section lands after it.
    expect(out.slice(0, saved.length)).toEqual(saved);
  });
});

describe('effectiveMiddleOrder', () => {
  it('filters out hidden sections', () => {
    const out = effectiveMiddleOrder(undefined, ['credentials', 'mcp']);
    expect(out).not.toContain('credentials');
    expect(out).not.toContain('mcp');
    expect(out).toEqual(DEFAULT_MIDDLE_ORDER.filter((k) => k !== 'credentials' && k !== 'mcp'));
  });

  it('respects both custom order and hidden together', () => {
    const out = effectiveMiddleOrder(['schedules', 'files'], ['files']);
    expect(out[0]).toBe('schedules');
    expect(out).not.toContain('files');
  });
});

describe('isUnclaimedSidebarSpace', () => {
  it('opens the fallback on plain sidebar background', () => {
    const div = document.createElement('div');
    expect(isUnclaimedSidebarSpace(false, div)).toBe(true);
  });

  it('defers to a more specific handler that already preventDefaulted (section slot / workspace row)', () => {
    const div = document.createElement('div');
    expect(isUnclaimedSidebarSpace(true, div)).toBe(false);
  });

  it('does not steal a right-click inside a text field (native copy/paste survives)', () => {
    const input = document.createElement('input');
    const wrap = document.createElement('div');
    wrap.appendChild(input);
    expect(isUnclaimedSidebarSpace(false, input)).toBe(false);
    // also true for a descendant of the input's field wrapper
    const span = document.createElement('span');
    const textarea = document.createElement('textarea');
    textarea.appendChild(span);
    expect(isUnclaimedSidebarSpace(false, span)).toBe(false);
  });

  it('opens the fallback for a null target', () => {
    expect(isUnclaimedSidebarSpace(false, null)).toBe(true);
  });
});

describe('reorderMiddle', () => {
  it('moves a section before another', () => {
    const out = reorderMiddle(undefined, 'mcp', 'files', 'before');
    expect(out.indexOf('mcp')).toBe(out.indexOf('files') - 1);
    expect(out.indexOf('mcp')).toBeLessThan(out.indexOf('files'));
    expect([...out].sort()).toEqual([...DEFAULT_MIDDLE_ORDER].sort());
  });

  it('moves a section after another', () => {
    const out = reorderMiddle(undefined, 'ports', 'files', 'after');
    expect(out.indexOf('ports')).toBe(out.indexOf('files') + 1);
  });

  it('is a no-op when moving onto itself', () => {
    expect(reorderMiddle(undefined, 'ports', 'ports', 'before')).toEqual(DEFAULT_MIDDLE_ORDER);
  });
});

describe('withoutTeamOnly', () => {
  it('keeps every section for a team install', () => {
    expect(withoutTeamOnly(DEFAULT_MIDDLE_ORDER, true)).toEqual(DEFAULT_MIDDLE_ORDER);
  });

  it('drops Task Owners and Agents for a public install', () => {
    const out = withoutTeamOnly(DEFAULT_MIDDLE_ORDER, false);
    expect(out).not.toContain('owners');
    expect(out).not.toContain('agents');
    expect(out).toContain('files');
    expect(out).not.toContain('credentials');
  });

  it('keeps Credentials for a team plan without operator tooling', () => {
    const out = withoutTeamOnly(DEFAULT_MIDDLE_ORDER, false, true);
    expect(out).toContain('credentials');
    expect(out).not.toContain('owners');
    expect(out).not.toContain('agents');
  });

  it('drops Credentials for a public install even when saved as shown', () => {
    expect(withoutTeamOnly(['credentials', 'schedules'], false, false)).toEqual(['schedules']);
  });
});

describe('retired Tasks sidebar preferences',()=>{it('drops saved owners while preserving the surviving section order',()=>{expect(resolvedMiddleOrder(['files','owners','ports']).slice(0,2)).toEqual(['files','ports']);expect(resolvedMiddleOrder(['owners'])).not.toContain('owners')})});
