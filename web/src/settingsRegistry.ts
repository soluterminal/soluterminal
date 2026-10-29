/**
 * Canonical, UI-independent Settings information architecture. Keep routing,
 * search and command-palette callers on this registry so a label move never
 * breaks a saved URL.
 */
export type SettingsCategory =
  | 'appearance'
  | 'workspace'
  | 'sessions'
  | 'workflows'
  | 'agents'
  | 'credentials'
  | 'team'
  | 'devices';

export type LegacySettingsTab =
  | 'appearance'
  | 'panes'
  | 'claude'
  | 'sidebar'
  | 'team'
  | 'devices'
  | 'warden'
  | 'tasks'
  | 'agent'
  | 'credentials';

export interface SettingsTarget {
  category: SettingsCategory;
  section?: string;
  control?: string;
  modelRole?: string;
  search?: boolean;
}

export interface SettingsCategoryDef {
  id: SettingsCategory;
  label: string;
  description: string;
  mobile?: boolean;
}

export interface SettingsSectionDef {
  id: string;
  category: SettingsCategory;
  title: string;
  description: string;
  domId: string;
  aliases?: string[];
  advanced?: boolean;
}

export interface SettingsControlDef {
  id: string;
  section: string;
  label: string;
  description: string;
  aliases: string[];
  domId: string;
  applies: 'live' | 'daemon-restart' | 'app-restart';
  sensitive?: boolean;
  dangerous?: boolean;
}

export const SETTINGS_CATEGORIES: SettingsCategoryDef[] = [
  { id: 'appearance', label: 'Appearance', description: 'Theme, terminal colours and text sizing.' },
  { id: 'workspace', label: 'Panes & sidebar', description: 'Pane layout, mouse behaviour and sidebar sections.' },
  { id: 'sessions', label: 'Claude sessions', description: 'New-session, recovery, prompt and Focus behaviour.' },
  { id: 'workflows', label: 'Services', description: 'Model gateway, release train and resource limits.' },
  { id: 'agents', label: 'Agents', description: 'Warden controls.' },
  { id: 'credentials', label: 'Credentials', description: 'Protected provider accounts, runtime keys and repair status.' },
  { id: 'team', label: 'Team pool', description: 'Team membership and shared credential pool.' },
  { id: 'devices', label: 'Devices & updates', description: 'Pairing, desktop behaviour, updates and diagnostics.', mobile: false },
];

function defineSection(
  id: string,
  category: SettingsCategory,
  title: string,
  description: string,
  options: Partial<Pick<SettingsSectionDef, 'domId' | 'aliases' | 'advanced'>> = {},
): SettingsSectionDef {
  return {
    id,
    category,
    title,
    description,
    domId: options.domId ?? `settings-section-${id.replaceAll('.', '-')}`,
    aliases: options.aliases ?? [],
    advanced: options.advanced,
  };
}

export const SETTINGS_SECTIONS: SettingsSectionDef[] = [
  defineSection('appearance.theme', 'appearance', 'Theme', 'App and terminal colour themes.', { aliases: ['dark mode', 'light mode', 'theme'] }),
  defineSection('appearance.text', 'appearance', 'Text size', 'Terminal, Focus and sidebar sizing.', { aliases: ['font size', 'zoom', 'scale'] }),
  defineSection('workspace.panes', 'workspace', 'Panes', 'Pane layout, arrangement and mouse behaviour.'),
  defineSection('workspace.sidebar', 'workspace', 'Sidebar', 'Choose, arrange and reset sidebar sections.'),
  defineSection('sessions.behavior', 'sessions', 'Session behaviour', 'Claude session recovery, prompts and Focus vitals.'),
  // The workflow task engine and its settings were retired (2026-10-25); what
  // remains under Services is the model gateway, the opt-in release train and
  // the resource limits that still gate agent runs and release work.
  defineSection('workflows.modelsFallbacks', 'workflows', 'Model gateway', 'A local OpenAI-compatible endpoint for managed agents.', { domId: 'settings-section-models-fallbacks', aliases: ['model', 'local model', 'base url'] }),
  defineSection('workflows.automation', 'workflows', 'Release train', 'Opt-in automatic publishing on a cadence.', { advanced: true, aliases: ['release', 'publish'] }),
  defineSection('workflows.resources', 'workflows', 'Resource limits', 'Agent run slots and memory and disk floors.', { advanced: true, aliases: ['limit', 'disk', 'memory'] }),
  defineSection('agents.warden', 'agents', 'Warden', 'Fleet caretaker behaviour and briefs.'),
  defineSection('credentials.accounts', 'credentials', 'Provider accounts', 'Sign in, re-authenticate and review provider accounts.', { aliases: ['sign in', 'login', 'account', 're-auth'] }),
  defineSection('credentials.keys', 'credentials', 'Runtime API keys', 'Local runtime API keys and tokens.', { aliases: ['api key', 'token'] }),
  defineSection('credentials.autoSwitch', 'credentials', 'Auto-switch', 'Account exhaustion and switching policy.', { aliases: ['exhausted', 'switch account'] }),
  defineSection('credentials.team', 'credentials', 'Team pool health', 'Redacted team credential repair status.', { aliases: ['team repair', 're-auth'] }),
  defineSection('credentials.privacy', 'credentials', 'Screen-share safety', 'Mask identities across operator surfaces.', { aliases: ['screen share', 'privacy', 'mask'] }),
  defineSection('team.pool', 'team', 'Team pool', 'Membership and shared provider credentials.'),
  defineSection('devices.pairing', 'devices', 'Pairing & desktop', 'Desktop installer, phone pairing and startup.', { aliases: ['phone', 'pair', 'qr'] }),
  defineSection('devices.updates', 'devices', 'Updates', 'Automatic daemon and desktop updates.', { aliases: ['update', 'auto-update'] }),
  defineSection('devices.diagnostics', 'devices', 'Diagnostics', 'Compatibility flags and privacy-safe local migration counters.'),
];

type ControlSeed = [
  section: string,
  key: string,
  label: string,
  aliases?: string[],
  applies?: SettingsControlDef['applies'],
  dangerous?: boolean,
];

const CONTROL_SEEDS: ControlSeed[] = [
  ['appearance.theme', 'appTheme', 'App theme', ['dark mode', 'light mode', 'theme']],
  ['appearance.theme', 'colorTheme', 'Color theme', ['terminal theme']],
  ['appearance.text', 'fontSize', 'Font size', ['zoom', 'scale']],
  ['appearance.text', 'lineHeight', 'Line height'],
  ['appearance.text', 'focusTextOffset', 'Focus text offset'],
  ['appearance.text', 'sidebarSize', 'Sidebar text size', ['scale']],
  ['workspace.panes', 'paneLayout', 'Pane layout'],
  ['workspace.panes', 'arrange', 'Arrange'],
  ['workspace.panes', 'arrangeCols', 'Cols', ['columns']],
  ['workspace.panes', 'arrangeRows', 'Rows'],
  ['workspace.panes', 'arrangeGrid', 'Grid'],
  ['workspace.panes', 'arrangeLeftStack', 'Left+stack', ['left stack']],
  ['workspace.panes', 'copyOnSelect', 'Copy on select'],
  ['workspace.panes', 'middleClickPaste', 'Middle-click paste'],
  ['workspace.panes', 'autoNamePanes', 'Name new agent panes with AI', ['title', 'rename', 'ai name', 'haiku']],
  ['workspace.sidebar', 'sidebarSections', 'Sidebar sections'],
  ['workspace.sidebar', 'files', 'Files'],
  ['workspace.sidebar', 'ports', 'Localhost ports'],
  ['workspace.sidebar', 'background', 'Detached sessions'],
  ['workspace.sidebar', 'agentPanes', 'Started by agents'],
  ['workspace.sidebar', 'credentials', 'Credentials'],
  ['workspace.sidebar', 'schedules', 'Automations'],
  ['workspace.sidebar', 'agents', 'Agents'],
  ['workspace.sidebar', 'mcp', 'MCP servers'],
  ['workspace.sidebar', 'resetSidebar', 'Reset to default'],
  ['sessions.behavior', 'newClaudeSessions', 'New Claude sessions'],
  ['sessions.behavior', 'afterRestart', 'After a restart'],
  ['sessions.behavior', 'suggestPrompt', 'Suggest next prompt (Focus)'],
  ['sessions.behavior', 'promptHistory', 'Show prompt history strip'],
  ['sessions.behavior', 'focusVitals', 'Focus vitals'],
  ['sessions.behavior', 'focusModel', 'Model'],
  ['sessions.behavior', 'focusContext', 'Context %'],
  ['sessions.behavior', 'focusTokens', 'Tokens'],
  ['sessions.behavior', 'focusCost', 'Cost'],
  ['sessions.behavior', 'focusCache', 'Cache'],
  ['sessions.behavior', 'focusResets', 'Resets'],
  ['sessions.behavior', 'focusCwd', 'Working dir'],
  ['workflows.modelsFallbacks', 'localOpenAI', 'Local OpenAI-compatible', ['local model', 'base url']],
  ['workflows.automation', 'releaseTrain', 'Auto-publish on cadence (release train)'],
  ['workflows.automation', 'trainLandings', 'Functional landings before a train'],
  ['workflows.automation', 'trainAge', 'Max unreleased age (hours)'],
  ['workflows.automation', 'trainVeto', 'Veto window (minutes)'],
  ['workflows.resources', 'maxAgentRuns', 'Max agent runs'],
  ['workflows.resources', 'memoryFloor', 'Memory floor'],
  ['workflows.resources', 'diskFloor', 'Disk floor', ['disk']],
  ['workflows.resources', 'hostDiskFloor', 'Host disk floor (WSL)'],
  ['workflows.resources', 'hostDiskPercent', 'Host disk floor %'],
  ['workflows.resources', 'adaptiveHostDisk', 'Adaptive host disk floor'],
  ['workflows.resources', 'rotateCrashDumps', 'Rotate WSL crash dumps'],
  ['agents.warden', 'enabled', 'Enable the Warden'],
  ['agents.warden', 'autoFlags', 'Automatic pane flags'],
  ['agents.warden', 'model', 'Model'],
  ['agents.warden', 'triageAfter', 'Triage after'],
  ['agents.warden', 'maxRuns', 'Max runs / hour'],
  ['agents.warden', 'runBrief', 'Run a fleet brief now'],
  ['credentials.accounts', 'providerAccounts', 'Provider accounts', ['sign in', 'login', 'account', 're-auth']],
  ['credentials.keys', 'runtimeKeys', 'Runtime API keys', ['api key', 'token']],
  ['credentials.autoSwitch', 'autoSwitch', 'Auto-switch'],
  ['credentials.team', 'teamHealth', 'Team pool health'],
  ['credentials.privacy', 'hideIdentities', 'Hide identities (screen share)', ['screen share', 'privacy', 'mask']],
  ['team.pool', 'teamPool', 'Team pool'],
  ['devices.pairing', 'desktopApp', 'Desktop app'],
  ['devices.pairing', 'phoneAccess', 'Phone access', ['phone', 'pair', 'qr']],
  ['devices.pairing', 'launchStartup', 'Launch on startup'],
  ['devices.updates', 'automaticUpdates', 'Automatic updates', ['update', 'auto-update']],
  ['devices.updates', 'desktopRestarts', 'Desktop app restarts', [], 'app-restart'],
  ['devices.diagnostics', 'anonymousCheckin', 'Send an anonymous daily check-in', ['check-in', 'telemetry', 'privacy', 'analytics']],
  ['devices.diagnostics', 'agentIntegration', 'Let Claude Code and Codex use SoluTerminal', ['mcp', 'claude', 'codex', 'hooks', 'integration', 'uninstall']],
];

export const SETTINGS_CONTROLS: SettingsControlDef[] = CONTROL_SEEDS.map(([
  parent,
  key,
  label,
  aliases = [],
  applies = 'live',
  dangerous,
]) => ({
  id: `${parent}.${key}`,
  section: parent,
  label,
  description: label,
  aliases,
  domId: `settings-row-${parent.replaceAll('.', '-')}-${key}`,
  applies,
  dangerous,
  sensitive: parent.startsWith('credentials.'),
}));

export const LEGACY_TAB_TARGETS: Record<LegacySettingsTab, SettingsTarget> = {
  appearance: { category: 'appearance' },
  panes: { category: 'workspace', section: 'workspace.panes' },
  claude: { category: 'sessions' },
  sidebar: { category: 'workspace', section: 'workspace.sidebar' },
  team: { category: 'team' },
  devices: { category: 'devices' },
  warden: { category: 'agents', section: 'agents.warden' },
  tasks: { category: 'workflows' },
  agent: { category: 'agents' },
  credentials: { category: 'credentials' },
};

export const LEGACY_SECTION_TARGETS: Record<string, SettingsTarget> = {
  // Max heavy operations went with the task engine; old links open the section.
  'tasks.resourceLimits.maxHeavyOps': { category: 'workflows', section: 'workflows.resources' },
  'tasks.modelsFallbacks': { category: 'workflows', section: 'workflows.modelsFallbacks' },
};

const categoryIds = new Set(SETTINGS_CATEGORIES.map((item) => item.id));
const sectionsById = new Map(SETTINGS_SECTIONS.map((item) => [item.id, item]));
const controlsById = new Map(SETTINGS_CONTROLS.map((item) => [item.id, item]));

export function resolveSettingsTarget(input: string | SettingsTarget): { target: SettingsTarget; legacy: boolean } {
  if (typeof input !== 'string') {
    return categoryIds.has(input.category)
      ? { target: { ...input }, legacy: false }
      : { target: { category: 'appearance' }, legacy: false };
  }

  if (Object.prototype.hasOwnProperty.call(LEGACY_SECTION_TARGETS, input)) {
    return { target: { ...LEGACY_SECTION_TARGETS[input] }, legacy: true };
  }
  if (categoryIds.has(input as SettingsCategory)) {
    return { target: { category: input as SettingsCategory }, legacy: false };
  }
  if (Object.prototype.hasOwnProperty.call(LEGACY_TAB_TARGETS, input)) {
    return { target: { ...LEGACY_TAB_TARGETS[input as LegacySettingsTab] }, legacy: true };
  }

  const section = sectionsById.get(input);
  if (section) return { target: { category: section.category, section: section.id }, legacy: false };
  const control = controlsById.get(input);
  if (control) {
    const parent = sectionsById.get(control.section)!;
    return {
      target: { category: parent.category, section: parent.id, control: control.id },
      legacy: false,
    };
  }

  if (import.meta.env.DEV) console.warn(`Unknown settings destination: ${input}`);
  return { target: { category: 'appearance' }, legacy: false };
}

export interface SettingsSearchHit {
  control?: SettingsControlDef;
  section: SettingsSectionDef;
  category: SettingsCategoryDef;
  breadcrumb: string;
  advanced?: boolean;
  score: number;
}

// Controls Settings › Devices leaves out while the desktop app's unattended
// update restart is paused (updateStatus.shellAutoApplyPaused === true, as on
// the public channel by default), so search does not land on a missing row.
const HIDDEN_WHILE_SHELL_AUTO_APPLY_PAUSED = new Set(['devices.updates.desktopRestarts']);

export interface SettingsSearchOptions {
  mobile: boolean;
  /** Pass `updateStatus?.shellAutoApplyPaused === true`, as DevicesTab does. */
  shellAutoApplyPaused?: boolean;
}

export function searchSettings(query: string, options: SettingsSearchOptions): SettingsSearchHit[] {
  const tokens = query.trim().toLocaleLowerCase().split(/\s+/).filter(Boolean);
  if (tokens.length === 0) return [];

  const categoryOrder = new Map(SETTINGS_CATEGORIES.map((item, index) => [item.id, index]));
  const results: SettingsSearchHit[] = [];
  for (const section of SETTINGS_SECTIONS) {
    const category = SETTINGS_CATEGORIES.find((item) => item.id === section.category)!;
    if (options.mobile && category.mobile === false) continue;
    const candidates: Array<SettingsControlDef | undefined> = [
      undefined,
      ...SETTINGS_CONTROLS.filter((control) => control.section === section.id
        && !(options.shellAutoApplyPaused && HIDDEN_WHILE_SHELL_AUTO_APPLY_PAUSED.has(control.id))),
    ];
    for (const control of candidates) {
      const label = (control?.label ?? section.title).toLocaleLowerCase();
      const controlAliases = (control?.aliases ?? []).map((alias) => alias.toLocaleLowerCase());
      const sectionAliases = (section.aliases ?? []).map((alias) => alias.toLocaleLowerCase());
      const searchableDescription = [
        control?.description,
        section.description,
        section.title,
        category.label,
        category.description,
      ].filter(Boolean).join(' ').toLocaleLowerCase();
      let score = 0;
      let matched = true;
      for (const token of tokens) {
        if (controlAliases.includes(token)) score += 5;
        else if (sectionAliases.includes(token)) score += 4;
        else if (label.includes(token)) score += 3;
        else if ([...controlAliases, ...sectionAliases].some((alias) => alias.includes(token))) score += 2;
        else if (searchableDescription.includes(token)) score += 1;
        else {
          matched = false;
          break;
        }
      }
      if (matched) {
        results.push({
          control,
          section,
          category,
          breadcrumb: `${category.label} › ${section.title}`,
          advanced: section.advanced,
          score,
        });
      }
    }
  }

  return results.sort((left, right) => (
    right.score - left.score
    || categoryOrder.get(left.category.id)! - categoryOrder.get(right.category.id)!
    || SETTINGS_SECTIONS.indexOf(left.section) - SETTINGS_SECTIONS.indexOf(right.section)
    || Number(Boolean(left.control)) - Number(Boolean(right.control))
    || (left.control?.label ?? '').localeCompare(right.control?.label ?? '')
  )).slice(0, 30);
}

export function formatSettingsHash(target: SettingsTarget): string {
  return `settings=${encodeURIComponent(target.control ?? target.section ?? target.category)}`;
}

export function parseSettingsHash(raw: string): SettingsTarget | null {
  const body = raw.startsWith('#') ? raw.slice(1) : raw;
  const value = new URLSearchParams(body).get('settings');
  return value ? resolveSettingsTarget(value).target : null;
}

export function categoryById(id: SettingsCategory): SettingsCategoryDef {
  return SETTINGS_CATEGORIES.find((item) => item.id === id)!;
}

export function sectionById(id: string | undefined): SettingsSectionDef | undefined {
  return id ? sectionsById.get(id) : undefined;
}

export function controlById(id: string | undefined): SettingsControlDef | undefined {
  return id ? controlsById.get(id) : undefined;
}
