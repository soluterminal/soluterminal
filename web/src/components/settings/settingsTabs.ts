import type { SettingsTab } from '../../settings';

// The Settings dialog's own tabs, in rail order (feature modules add theirs
// through extensions.ts). Shared by the dialog, its search breadcrumbs and
// the command palette's "Settings: <tab>" entries.
export const CORE_SETTINGS_TABS: { key: SettingsTab; label: string }[] = [
  { key: 'appearance', label: 'Appearance' },
  { key: 'panes', label: 'Terminal & panes' },
  { key: 'sidebar', label: 'Sidebar' },
  { key: 'account', label: 'Account' },
  { key: 'notifications', label: 'Notifications & voice' },
  { key: 'privacy', label: 'Privacy & security' },
  { key: 'devices', label: 'Devices & updates' },
  { key: 'tasks', label: 'Services' },
];
