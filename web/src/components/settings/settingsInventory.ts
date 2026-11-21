import { slotList } from '../../extensions';
import type { SettingsCategory } from '../../settingsRegistry';

/**
 * Hand-written snapshot of visible labels in the historical nine-tab dialog.
 * This must not be derived from settingsRegistry: registry refactors must not
 * be able to make the zero-setting-loss check pass by construction.
 */
const SETTINGS_INVENTORY: Record<SettingsCategory, string[]> = {
  appearance: [
    'App theme', 'Color theme', 'Font size', 'Line height', 'Sidebar text size',
  ],
  workspace: [
    'Pane layout', 'Arrange', 'Cols', 'Rows', 'Grid', 'Left+stack', 'Copy on select',
    'Middle-click paste', 'Always show agent memory', 'Name new agent panes with AI', 'Sidebar sections', 'Files', 'Localhost ports',
    'Detached sessions', 'Credentials', 'Automations', 'Agents', 'MCP servers', 'Reset to default',
  ],
  sessions: [
    'After a restart', 'Show prompt history strip', 'Worker model',
  ],
  workflows: [
    'Local OpenAI-compatible', 'Base URL', 'Model',
    'Auto-publish on cadence (release train)', 'Functional landings before a train',
    'Max unreleased age (hours)', 'Veto window (minutes)',
    'Max agent runs', 'Memory floor', 'Disk floor', 'Host disk floor (WSL)',
    'Host disk floor %', 'Adaptive host disk floor', 'Rotate WSL crash dumps',
  ],
  agents: [],
  credentials: [
    'Provider accounts', 'Runtime API keys', 'Allow Shinoque to use these credentials',
  ],
  team: [],
  devices: [
    'Presentation mode', 'Desktop app', 'Phone access', 'Launch on startup', 'Automatic updates',
    'Desktop app restarts', 'Send an anonymous daily check-in',
    'Let Claude Code and Codex use SoluTerminal',
    // ST-281: today's Account, Notifications & voice, Privacy & security and
    // Devices & updates rows.
    'Account', 'View as personal', 'Email me product updates (you can unsubscribe anytime)',
    'Send dictation automatically', 'Language',
    'Keep the microphone ready for 2 minutes after dictating (faster start; your system’s mic light stays on)',
    'Tick when the mic is listening', 'Clean up dictation with my Claude plan',
    'Use my Anthropic API key (ANTHROPIC_API_KEY). Clean-up then goes to Anthropic under your key and is billed to it.',
    'Words to recognise', 'Let agents in panes approve for me',
    'Require approved devices for actions that need you', 'This browser', 'Who can use this computer remotely',
    "Encrypt this browser's connection end to end", 'Verify this browser', 'Alerts handled by another app',
    'Share usage statistics', 'Send crash reports', 'Delete pane recordings after', 'Space for pane recordings', 'Developer overrides', 'Paused updates',
    'Restart background service', 'Display fixes used',
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
