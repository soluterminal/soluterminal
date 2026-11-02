import { beforeEach, describe, expect, it } from 'vitest';
import { readSettingsTelemetry, resetSettingsTelemetry } from './lib/settingsTelemetry';
import { useSettings } from './settings';

function resetStore(): void {
  useSettings.setState({
    open: false,
    category: 'appearance',
    tab: 'appearance',
    currentTarget: { category: 'appearance' },
    pendingTarget: null,
    section: null,
    modelRole: null,
    query: '',
    history: [],
  });
}

describe('settings store compatibility adapter', () => {
  beforeEach(() => {
    localStorage.clear();
    resetSettingsTelemetry();
    resetStore();
  });

  it('preserves legacy tab, delayed-section and model-role state', () => {
    useSettings.getState().openSettings('tasks', 'tasks.modelsFallbacks', 'executor');
    expect(useSettings.getState()).toMatchObject({
      open: true,
      tab: 'tasks',
      section: 'tasks.modelsFallbacks',
      modelRole: 'executor',
      category: 'workflows',
      pendingTarget: {
        category: 'workflows',
        section: 'workflows.modelsFallbacks',
        modelRole: 'executor',
      },
    });
    expect(useSettings.getState().consumeSection()).toBe('tasks.modelsFallbacks');
    expect(useSettings.getState().consumeSection()).toBeNull();
    expect(useSettings.getState().consumeModelRole()).toBe('executor');
    expect(useSettings.getState().consumeModelRole()).toBeNull();

    useSettings.getState().openSettings('tasks', 'tasks.resourceLimits.maxHeavyOps');
    expect(useSettings.getState().section).toBe('tasks.resourceLimits.maxHeavyOps');
    // The control went with the task engine: the old link opens its section.
    expect(useSettings.getState().pendingTarget).toMatchObject({ category: 'workflows', section: 'workflows.resources' });
    expect(useSettings.getState().pendingTarget?.control).toBeUndefined();

    useSettings.getState().openSettings('agent');
    expect(useSettings.getState()).toMatchObject({ tab: 'agent', category: 'agents' });
    useSettings.getState().openSettings('sidebar');
    expect(useSettings.getState()).toMatchObject({ tab: 'sidebar', category: 'workspace' });

    useSettings.getState().setCategory('credentials');
    useSettings.getState().closeSettings();
    useSettings.getState().openSettings();
    expect(useSettings.getState()).toMatchObject({ open: true, category: 'credentials' });
  });

  it('maps canonical targets back to legacy observable fields', () => {
    useSettings.getState().openSettings({
      category: 'workflows',
      section: 'workflows.resources',
      control: 'workflows.resources.maxAgentRuns',
    });
    expect(useSettings.getState()).toMatchObject({ tab: 'tasks' });

    useSettings.getState().openSettings({ category: 'agents', section: 'agents.aria' });
    expect(useSettings.getState().tab).toBe('agent');

    useSettings.getState().openSettings({
      category: 'workflows',
      section: 'workflows.modelsFallbacks',
      modelRole: 'reviewer:spec_compliance',
    });
    expect(useSettings.getState()).toMatchObject({
      tab: 'tasks',
      section: 'tasks.modelsFallbacks',
      modelRole: 'reviewer:spec_compliance',
    });
  });

  it('records each legacy open exactly once and never counts canonical opens', () => {
    useSettings.getState().openSettings('sidebar');
    expect(readSettingsTelemetry().legacyTab).toEqual({ sidebar: 1 });

    useSettings.getState().openSettings('tasks', 'tasks.modelsFallbacks');
    expect(readSettingsTelemetry()).toMatchObject({
      legacyTab: { sidebar: 1 },
      legacySection: { 'tasks.modelsFallbacks': 1 },
    });

    useSettings.getState().openSettings({ category: 'credentials' });
    useSettings.getState().openSettings('appearance');
    expect(readSettingsTelemetry()).toMatchObject({
      legacyTab: { sidebar: 1 },
      legacySection: { 'tasks.modelsFallbacks': 1 },
    });
  });

  it('can retire legacy redirects without affecting canonical targets', () => {
    localStorage.setItem('solu.settings.legacyRedirects', '0');
    useSettings.getState().openSettings('tasks');
    expect(useSettings.getState().category).toBe('appearance');
    useSettings.getState().openSettings({ category: 'workflows' });
    expect(useSettings.getState().category).toBe('workflows');
  });

  it('keeps setTab compatible while storing the canonical target', () => {
    useSettings.getState().setTab('panes');
    expect(useSettings.getState()).toMatchObject({
      category: 'workspace',
      tab: 'panes',
      pendingTarget: { category: 'workspace', section: 'workspace.panes' },
      section: null,
    });
  });

  it('restores exact history, persists categories and clears ephemeral state', () => {
    const first = { category: 'workflows' as const, section: 'workflows.resources' };
    useSettings.getState().openSettings(first);
    useSettings.getState().openSettings({ category: 'credentials', section: 'credentials.accounts' });
    useSettings.getState().setQuery('heavy');
    useSettings.getState().goBack();
    expect(useSettings.getState()).toMatchObject({
      category: 'workflows',
      currentTarget: first,
      pendingTarget: first,
      query: 'heavy',
      history: [],
    });
    expect(localStorage.getItem('solu.settings.lastCategory')).toBe('workflows');

    useSettings.getState().closeSettings();
    expect(useSettings.getState()).toMatchObject({
      open: false,
      pendingTarget: null,
      section: null,
      modelRole: null,
      query: '',
      history: [],
    });
  });
});
