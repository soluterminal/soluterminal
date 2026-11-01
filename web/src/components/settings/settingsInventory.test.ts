import { describe, expect, it } from 'vitest';
import { SETTINGS_SECTION_IDS } from '../../settings';
import {
  settingsCategories,
  settingsControls,
  settingsSections,
  controlById,
  sectionById,
} from '../../settingsRegistry';
import { settingsInventory } from './settingsInventory';

function sortedUnique(values: string[]): string[] {
  return [...new Set(values)].sort();
}

describe('settings inventory', () => {
  it('preserves every visible setting label in its new category', () => {
    for (const category of settingsCategories()) {
      const sectionIds = new Set(
        settingsSections().filter((section) => section.category === category.id).map(({ id }) => id),
      );
      const registryLabels = settingsControls()
        .filter((control) => sectionIds.has(control.section))
        .map(({ label }) => label);
      expect(sortedUnique(registryLabels)).toEqual(sortedUnique(settingsInventory()[category.id]));
    }

    const inventoryLabels = new Set(Object.values(settingsInventory()).flat());
    const registryLabels = new Set(settingsControls().map(({ label }) => label));
    expect([...inventoryLabels].filter((label) => !registryLabels.has(label))).toEqual([]);
    expect([...registryLabels].filter((label) => !inventoryLabels.has(label))).toEqual([]);
  });

  it('keeps the two existing delayed-focus DOM ids stable', () => {
    expect(SETTINGS_SECTION_IDS['tasks.resourceLimits.maxHeavyOps']).toBe(
      sectionById('workflows.resources')?.domId,
    );
    expect(controlById('workflows.resources.maxHeavyOps')).toBeUndefined();
    expect(SETTINGS_SECTION_IDS['tasks.modelsFallbacks']).toBe(
      sectionById('workflows.modelsFallbacks')?.domId,
    );
  });
});
