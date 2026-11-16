// Curated starting points for the New Schedule dialog. A blank prompt
// textarea is the single biggest reason a scheduler goes unused — nobody
// wakes up knowing what to automate. Each template prefills kind + prompt +
// cron + name in one click; the user edits from there.
//
// Data, not UI: adding a recipe is a one-line change here, no component work.

import type { ScheduleInput, SchedulePermission } from '../types';

// NOTE: report STRUCTURE ("## Needs you" first, short sections) is not a
// recipe concern — the daemon appends the format contract to every task
// prompt at run time (schedules_task.go reportStyleHint), so recipes stay
// clean prompts a user can read and edit without boilerplate.

export interface ScheduleTemplate {
  id: string;
  title: string;
  /** One line under the title in the picker — what it does, in plain words. */
  blurb: string;
  emoji: string;
  /** Applied over the dialog's current state. `cwd` is left to the picker. */
  preset: {
    name: string;
    kind: 'task' | 'prompt';
    prompt: string;
    cron?: string;
    /** The least the recipe needs. None edits files (scheduleTemplates.test.ts). */
    permission?: SchedulePermission;
    notifyOnRun?: boolean;
    /** Appended on operator installs only, for MCP tools that public
     *  installs don't have (daemon/mcp_public.go). */
    operatorPromptSuffix?: string;
  };
}

// The prompt a recipe fills in. Public installs never get instructions for
// tools their agents can't call.
export function templatePrompt(t: ScheduleTemplate, operator: boolean): string {
  const suffix = operator ? t.preset.operatorPromptSuffix : undefined;
  return suffix ? `${t.preset.prompt} ${suffix}` : t.preset.prompt;
}

export const SCHEDULE_TEMPLATES: ScheduleTemplate[] = [
  {
    id: 'morning-digest',
    title: 'Morning digest',
    blurb: 'Summarize overnight PRs, issues, and CI into a short brief',
    emoji: '☕',
    preset: {
      name: 'Morning digest',
      kind: 'task',
      cron: '0 9 * * 1-5', // weekdays 9am
      // Read-only git in plan mode. (A read-only GitHub preset comes back
      // with a daemon-owned gh wrapper, ST-277.)
      permission: 'plan',
      notifyOnRun: true,
      prompt:
        'Summarize what changed in this repo overnight from its git history: '
        + 'merged branches and new commits, and anything that looks like it needs my '
        + 'review. Use read-only git commands. Keep it under 15 lines and '
        + 'lead with whatever needs my decision today.',
    },
  },
  {
    id: 'nightly-tests',
    title: 'Nightly test run',
    blurb: 'Run the test suite and report only what failed',
    emoji: '🌙',
    preset: {
      name: 'Nightly test run',
      kind: 'task',
      cron: '0 0 * * *', // daily midnight
      // Running the suite executes the project's code: the check preset.
      permission: 'check',
      prompt:
        'Run this project\'s test suite with exactly one of these commands, with no '
        + 'extra arguments: npm test, pnpm test, yarn test, go test ./..., cargo test, '
        + 'pytest, make test or make check. Do not change any files. '
        + 'If everything passes, reply with one line. '
        + 'If anything fails, list each failing test with the assertion and the most '
        + 'likely cause, ordered by how easy it looks to fix.',
    },
  },
  {
    id: 'dependency-audit',
    title: 'Dependency audit',
    blurb: 'Check for outdated and vulnerable dependencies',
    emoji: '🔒',
    preset: {
      name: 'Dependency audit',
      kind: 'task',
      cron: '0 9 * * 1', // Mondays 9am
      // Advisories and versions come from npm audit / npm outdated & co.,
      // which obey the repo's own package-manager config: the check preset.
      permission: 'check',
      prompt:
        'Audit this project\'s dependencies with exactly these commands, with no other '
        + 'arguments: npm/pnpm/yarn audit and outdated (optionally --json), pip list --outdated, '
        + 'pip-audit, go list -m -u all, govulncheck ./..., cargo outdated or cargo audit. '
        + 'Report: (1) any with known security '
        + 'advisories, (2) any more than one major version behind, (3) unused ones. '
        + 'For each, say what upgrading would break. Do not change any files.',
    },
  },
  {
    id: 'stale-branches',
    title: 'Stale branch report',
    blurb: 'Find branches that have gone quiet and say what to do with them',
    emoji: '🌿',
    preset: {
      name: 'Stale branch report',
      kind: 'task',
      cron: '0 16 * * 5', // Fridays 4pm
      // Read-only git (branch, log) runs in plan mode.
      permission: 'plan',
      prompt:
        'List git branches with no commits in the last 30 days. For each, say whether '
        + 'it was merged, abandoned, or still has unique work worth rescuing, and '
        + 'recommend delete / rebase / leave.',
    },
  },
  {
    id: 'standup',
    title: 'Standup summary',
    blurb: 'Turn yesterday\'s commits into standup notes',
    emoji: '📋',
    preset: {
      name: 'Standup summary',
      kind: 'task',
      cron: '15 9 * * 1-5', // weekdays 9:15am
      // git log is a read-only command plan mode runs.
      permission: 'plan',
      notifyOnRun: true,
      prompt:
        'Read the git log for the last 24 hours and write my standup: what I shipped, '
        + 'what is in progress, and anything blocked. Plain sentences, no bullet '
        + 'padding, max 6 lines.',
    },
  },
  {
    id: 'doc-drift',
    title: 'Doc drift check',
    blurb: 'Spot docs that no longer match the code',
    emoji: '📄',
    preset: {
      name: 'Doc drift check',
      kind: 'task',
      cron: '0 9 1 * *', // monthly, 1st at 9am
      // Reading files is all it needs.
      permission: 'plan',
      prompt:
        'Compare this project\'s README and docs against the actual code. List any '
        + 'instructions, flags, or file paths that are now wrong, with the correct '
        + 'value for each. Do not edit files.',
    },
  },
  {
    id: 'remind-me',
    title: 'Remind me…',
    blurb: 'A one-time nudge into a pane at a time you pick',
    emoji: '⏰',
    preset: {
      name: '',
      kind: 'prompt',
      notifyOnRun: true,
      prompt: '',
    },
  },
];

/** Shape the dialog consumes: the preset minus fields it owns (cwd, paneId). */
export type TemplatePatch = Partial<ScheduleInput> & { cron?: string };
