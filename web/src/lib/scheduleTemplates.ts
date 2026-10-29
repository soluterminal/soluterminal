// Curated starting points for the New Schedule dialog. A blank prompt
// textarea is the single biggest reason a scheduler goes unused — nobody
// wakes up knowing what to automate. Each template prefills kind + prompt +
// cron + name in one click; the user edits from there.
//
// Data, not UI: adding a recipe is a one-line change here, no component work.

import type { ScheduleInput } from '../types';

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
    permission?: 'plan' | 'edit';
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
      permission: 'plan',
      notifyOnRun: true,
      prompt:
        'Summarize what changed in this repo overnight: merged PRs, new issues, '
        + 'failing CI, and anything awaiting my review. Keep it under 15 lines and '
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
      permission: 'plan',
      prompt:
        'Run this project\'s test suite. If everything passes, reply with one line. '
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
      permission: 'plan',
      prompt:
        'Audit this project\'s dependencies. Report: (1) any with known security '
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
