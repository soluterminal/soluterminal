import { describe, expect, it } from 'vitest';
import {
  REBINDABLE_ACTIONS,
  actionById,
  comboFromEvent,
  resolveCombo,
} from './keybindings';

describe('REBINDABLE_ACTIONS registry', () => {
  it('has no two actions sharing a default combo', () => {
    // Collision guard: two defaults on the same combo means one action
    // could never fire (resolveCombo returns the first match). Catches a
    // new binding accidentally stepping on an existing one.
    const byCombo = new Map<string, string>();
    for (const a of REBINDABLE_ACTIONS) {
      const clash = byCombo.get(a.defaultCombo);
      expect(clash, `${a.id} and ${clash} both default to ${a.defaultCombo}`).toBeUndefined();
      byCombo.set(a.defaultCombo, a.id);
    }
  });

  it('has unique ids', () => {
    const ids = REBINDABLE_ACTIONS.map((a) => a.id);
    expect(new Set(ids).size).toBe(ids.length);
  });
});

describe('edit.commandLine action', () => {
  it('is registered with Alt+G as its default', () => {
    const action = actionById('edit.commandLine');
    expect(action).toBeDefined();
    expect(action?.defaultCombo).toBe('alt+g');
  });

  it('resolves Alt+G to edit.commandLine with no overrides', () => {
    expect(resolveCombo('alt+g', {})).toBe('edit.commandLine');
  });

  it('serializes an Alt+G KeyboardEvent to the alt+g combo', () => {
    const e = {
      key: 'g',
      ctrlKey: false,
      altKey: true,
      shiftKey: false,
      metaKey: false,
    } as KeyboardEvent;
    expect(comboFromEvent(e)).toBe('alt+g');
  });
});

describe('pane.zoom action', () => {
  it('is registered with Alt+Z as its default', () => {
    const action = actionById('pane.zoom');
    expect(action).toBeDefined();
    expect(action?.defaultCombo).toBe('alt+z');
  });

  it('resolves Alt+Z to pane.zoom with no overrides', () => {
    expect(resolveCombo('alt+z', {})).toBe('pane.zoom');
  });

  it('frees Alt+Z when the user rebinds zoom elsewhere', () => {
    // The point of making zoom rebindable: a GPU overlay owns Alt+Z, the
    // user moves zoom to Alt+M — the old chord must not still resolve.
    const overrides = { 'pane.zoom': 'alt+m' };
    expect(resolveCombo('alt+m', overrides)).toBe('pane.zoom');
    expect(resolveCombo('alt+z', overrides)).toBeNull();
  });
});

describe('resolveCombo overrides', () => {
  it('lets a user override win over a default', () => {
    // User moved edit.commandLine to ctrl+e; alt+g should now resolve to
    // nothing (no other action owns it) and ctrl+e to edit.commandLine.
    const overrides = { 'edit.commandLine': 'ctrl+e' };
    expect(resolveCombo('ctrl+e', overrides)).toBe('edit.commandLine');
    expect(resolveCombo('alt+g', overrides)).toBeNull();
  });
});

describe('removed actions', () => {
  it('drops Quick todo capture and ignores a saved override for it', () => {
    expect(actionById('todo.quickAdd')).toBeUndefined();
    expect(REBINDABLE_ACTIONS.some((a) => /todo/i.test(`${a.id} ${a.label} ${a.hint}`))).toBe(false);
    expect(resolveCombo('alt+t', {})).toBeNull();
    expect(resolveCombo('alt+t', { 'todo.quickAdd': 'alt+t' })).toBeNull();
  });
});
