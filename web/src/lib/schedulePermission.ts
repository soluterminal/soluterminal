// Words for a task automation's permission preset, shared by the dialog's
// radio list and the sidebar row. What each preset lets Claude run is fixed by
// the daemon (schedules_task.go scheduleTaskPresets); this file only names it.

import type { SchedulePermission } from '../types';

export interface SchedulePermissionChoice {
  value: SchedulePermission;
  label: string;
  /** Dim text after the label in the dialog. */
  detail: string;
  /** Sidebar row tooltip: "Claude task in ~/x · <summary>". */
  summary: string;
  /** Sidebar row suffix after the folder name; '' for none. */
  rowTag: string;
}

export const SCHEDULE_PERMISSION_CHOICES: SchedulePermissionChoice[] = [
  {
    value: 'plan',
    label: 'Read & plan only',
    detail: '— reads files and runs read-only git; changes nothing',
    summary: 'read & plan only',
    rowTag: ' (plan)',
  },
  {
    value: 'check',
    label: 'Read & run checks',
    detail: '— also runs your project\'s tests and dependency checks, which execute code from the repository',
    summary: 'may run tests and dependency reports',
    rowTag: ' (checks)',
  },
  {
    value: 'edit',
    label: 'Allow edits',
    detail: '— Claude may change files in this directory',
    summary: 'may edit files',
    rowTag: '',
  },
];

/** The choice for a stored preset; anything unknown reads as plan. */
export function schedulePermissionChoice(p: string | undefined): SchedulePermissionChoice {
  return SCHEDULE_PERMISSION_CHOICES.find((c) => c.value === p) ?? SCHEDULE_PERMISSION_CHOICES[0];
}
