import { slotList } from '../../extensions';
import type { SettingsCategory } from '../../settingsRegistry';

/**
 * Hand-written snapshot of visible labels in the historical nine-tab dialog.
 * This must not be derived from settingsRegistry: registry refactors must not
 * be able to make the zero-setting-loss check pass by construction.
 */
const SETTINGS_INVENTORY: Record<SettingsCategory, string[]> = {
  appearance: [
    'App theme', 'Color theme', 'Font size', 'Line height', 'Focus text offset', 'Sidebar text size',
  ],
  workspace: [
    'Pane layout', 'Arrange', 'Cols', 'Rows', 'Grid', 'Left+stack', 'Copy on select',
    'Middle-click paste', 'Always show agent memory', 'Name new agent panes with AI', 'Sidebar sections', 'Files', 'Localhost ports',
    'Detached sessions', 'Credentials', 'Automations', 'Agents', 'MCP servers', 'Reset to default',
  ],
  sessions: [
    'New Claude sessions', 'After a restart', 'Suggest next prompt (Focus)',
    'Show prompt history strip', 'Focus vitals', 'Model', 'Context %', 'Tokens', 'Cost',
    'Cache', 'Resets', 'Working dir',
  ],
  workflows: [
    'Local OpenAI-compatible',
    'Auto-publish on cadence (release train)', 'Functional landings before a train',
    'Max unreleased age (hours)', 'Veto window (minutes)',
    'Max agent runs', 'Memory floor', 'Disk floor', 'Host disk floor (WSL)',
    'Host disk floor %', 'Adaptive host disk floor', 'Rotate WSL crash dumps',
  ],
  agents: [],
  credentials: [
    'Provider accounts', 'Runtime API keys',
  ],
  team: [],
  devices: [
    'Presentation mode', 'Desktop app', 'Phone access', 'Launch on startup', 'Automatic updates',
    'Desktop app restarts', 'Send an anonymous daily check-in',
    'Let Claude Code and Codex use SoluTerminal',
  ],
};

export function settingsInventory(): Record<SettingsCategory, string[]> {
  const result = { ...SETTINGS_INVENTORY };
  for (const entry of slotList('settingsRegistryEntries')) {
    for (const [category, labels] of Object.entries(entry.inventory ?? {})) {
      const key = category as SettingsCategory;
      result[key] = [...result[key], ...labels];
    }
  }
  return result;
}
