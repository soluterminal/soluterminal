import { describe, expect, it } from 'vitest';
import { SCHEDULE_TEMPLATES, templatePrompt } from './scheduleTemplates';
import { describeCron, parseCron } from './cron';

describe('SCHEDULE_TEMPLATES', () => {
  it('have unique ids', () => {
    const ids = SCHEDULE_TEMPLATES.map((t) => t.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it('every cron is valid and human-describable', () => {
    for (const t of SCHEDULE_TEMPLATES) {
      if (!t.preset.cron) continue;
      expect(parseCron(t.preset.cron), `${t.id}: ${t.preset.cron}`).not.toBeNull();
      expect(describeCron(t.preset.cron), `${t.id}`).toBeTruthy();
    }
  });

  it('task templates carry a permission and never edit files', () => {
    for (const t of SCHEDULE_TEMPLATES.filter((x) => x.preset.kind === 'task')) {
      expect(t.preset.permission, t.id).toBeDefined();
      // No shipped recipe may silently write to the user's working tree.
      expect(t.preset.permission, t.id).not.toBe('edit');
    }
  });

  it('each task recipe gets the least permission its job needs', () => {
    const perms = Object.fromEntries(
      SCHEDULE_TEMPLATES.filter((t) => t.preset.kind === 'task').map((t) => [t.id, t.preset.permission]),
    );
    expect(perms).toEqual({
      'morning-digest': 'plan', // read-only git (GitHub preset: ST-277)
      'nightly-tests': 'check', // the test command runs project code
      'dependency-audit': 'check', // npm audit / npm outdated obey repo config
      'stale-branches': 'plan', // read-only git
      standup: 'plan', // git log
      'doc-drift': 'plan', // file reads
    });
  });

  it('every task template has a non-empty prompt', () => {
    for (const t of SCHEDULE_TEMPLATES.filter((x) => x.preset.kind === 'task')) {
      expect(t.preset.prompt.trim().length, t.id).toBeGreaterThan(20);
    }
  });

  it('never asks for the removed todo tools, on any install', () => {
    for (const t of SCHEDULE_TEMPLATES) {
      for (const operator of [true, false]) {
        expect(templatePrompt(t, operator), t.id).not.toMatch(/list_todos|add_todo|update_todo|todo/i);
      }
    }
    const digest = SCHEDULE_TEMPLATES.find((t) => t.id === 'morning-digest')!;
    expect(templatePrompt(digest, true)).toBe(templatePrompt(digest, false));
    expect(templatePrompt(digest, false)).toContain('lead with whatever needs my decision today.');
  });

  it('never asks a public install for tools it lacks', () => {
    for (const t of SCHEDULE_TEMPLATES) {
      expect(templatePrompt(t, false)).not.toMatch(/MCP|list_todos|add_todo|workflow_task/);
    }
  });

  it('the one-time reminder recipe has no cron and notifies', () => {
    const remind = SCHEDULE_TEMPLATES.find((t) => t.id === 'remind-me');
    expect(remind).toBeDefined();
    expect(remind!.preset.cron).toBeUndefined();
    expect(remind!.preset.notifyOnRun).toBe(true);
  });
});
