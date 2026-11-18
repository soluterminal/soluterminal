import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SettingsModal } from '../SettingsModal';
import { useSettings } from '../../settings';
import { CORE_SETTINGS_TABS } from './settingsTabs';
import { landOnSetting, settingsSearchResults } from './SettingsSearch';

// ST-281: search in Settings.

const desktopTabs = CORE_SETTINGS_TABS;

describe('settingsSearchResults', () => {
  it('ranks the row named in the query first, with its Tab › Section', () => {
    const font = settingsSearchResults('font', { tabs: desktopTabs, mobile: false });
    expect(font[0]).toMatchObject({ title: 'Font size', breadcrumb: 'Appearance › Text size', landOn: 'Font size' });

    const update = settingsSearchResults('update', { tabs: desktopTabs, mobile: false });
    expect(update[0]).toMatchObject({ title: 'Automatic updates', breadcrumb: 'Devices & updates › Updates' });

    const notif = settingsSearchResults('notif', { tabs: desktopTabs, mobile: false });
    expect(notif.length).toBeGreaterThan(0);
    expect(notif[0].breadcrumb.startsWith('Notifications & voice')).toBe(true);
  });

  it('matches aliases and words in any order', () => {
    // The Theme section ranks first on its alias, and lands on its first row.
    expect(settingsSearchResults('dark mode', { tabs: desktopTabs, mobile: false })[0]).toMatchObject({ title: 'Theme', landOn: 'App theme' });
    expect(settingsSearchResults('restart after', { tabs: desktopTabs, mobile: false })[0].title).toBe('After a restart');
    // The old Claude sessions tab lives in Terminal & panes.
    expect(settingsSearchResults('restart after', { tabs: desktopTabs, mobile: false })[0].breadcrumb).toBe('Terminal & panes › Session behaviour');
  });

  it('leaves out tabs the dialog does not show', () => {
    const withoutVoice = desktopTabs.filter((tab) => tab.key !== 'notifications');
    expect(settingsSearchResults('dictation', { tabs: withoutVoice, mobile: false })).toEqual([]);
    expect(settingsSearchResults('dictation', { tabs: desktopTabs, mobile: false }).length).toBeGreaterThan(0);
  });

  it('on a phone offers only what the phone shows', () => {
    const phoneTabs = desktopTabs.filter((tab) => ['appearance', 'panes', 'sidebar', 'privacy'].includes(tab.key));
    expect(settingsSearchResults('font', { tabs: phoneTabs, mobile: true })[0].title).toBe('Font size');
    expect(settingsSearchResults('update', { tabs: phoneTabs, mobile: true }).map((r) => r.title)).not.toContain('Automatic updates');
    const privacy = settingsSearchResults('encrypt', { tabs: phoneTabs, mobile: true });
    expect(privacy.map((r) => r.title)).toEqual(["Encrypt this browser's connection end to end"]);
    // Named once when the section is named like its tab.
    expect(privacy[0].breadcrumb).toBe('Privacy & security');
    expect(settingsSearchResults('check-in', { tabs: phoneTabs, mobile: true })).toEqual([]);
  });

  it('hides what Devices & updates hides while desktop restarts are paused', () => {
    const titles = (paused: boolean) => settingsSearchResults('desktop restarts', { tabs: desktopTabs, mobile: false, shellAutoApplyPaused: paused }).map((r) => r.title);
    expect(titles(false)).toContain('Desktop app restarts');
    expect(titles(true)).not.toContain('Desktop app restarts');
  });

  it('caps results after the tab filter, so hidden tabs never crowd out visible rows', () => {
    // 'a' matches far more than 30 rows; on a phone the visible ones still fill the list.
    const phoneTabs = desktopTabs.filter((tab) => ['appearance', 'panes', 'sidebar', 'privacy'].includes(tab.key));
    const phone = settingsSearchResults('a', { tabs: phoneTabs, mobile: true });
    expect(phone.length).toBeGreaterThan(5);
    expect(phone.every((r) => /^(Appearance|Terminal & panes|Sidebar|Privacy & security)/.test(r.breadcrumb))).toBe(true);
    expect(settingsSearchResults('a', { tabs: desktopTabs, mobile: false }).length).toBeLessThanOrEqual(30);
  });

  it('returns nothing for an empty or unmatched query', () => {
    expect(settingsSearchResults('   ', { tabs: desktopTabs, mobile: false })).toEqual([]);
    expect(settingsSearchResults('zzqx', { tabs: desktopTabs, mobile: false })).toEqual([]);
  });
});

describe('landOnSetting', () => {
  afterEach(() => vi.useRealTimers());

  it('opens a collapsed group, focuses the row control and marks the row', () => {
    const container = document.createElement('div');
    container.innerHTML = `
      <details><summary>Advanced</summary>
        <div class="settings-row" data-setting="Hidden row"><input id="hidden-input" /></div>
      </details>`;
    document.body.appendChild(container);
    landOnSetting(container, 'Hidden row');
    expect(container.querySelector('details')!.open).toBe(true);
    expect(document.activeElement?.id).toBe('hidden-input');
    expect(container.querySelector('[data-setting="Hidden row"]')!.classList.contains('settings-row-found')).toBe(true);
    container.remove();
  });

  it('waits for a row that renders a moment later', () => {
    vi.useFakeTimers();
    const container = document.createElement('div');
    document.body.appendChild(container);
    landOnSetting(container, 'Late row');
    const row = document.createElement('div');
    row.dataset.setting = 'Late row';
    row.innerHTML = '<button id="late-btn">Go</button>';
    container.appendChild(row);
    vi.advanceTimersByTime(60);
    expect(document.activeElement?.id).toBe('late-btn');
    container.remove();
  });
});

describe('Settings dialog search', () => {
  afterEach(() => {
    act(() => useSettings.getState().closeSettings());
  });

  function openDialog(tab = 'panes') {
    act(() => useSettings.getState().openSettings(tab));
    return render(<SettingsModal />);
  }

  it('finds a row by typing, says how many, and Enter lands focus on it', async () => {
    openDialog();
    const box = screen.getByRole('combobox', { name: 'Search settings' });
    fireEvent.change(box, { target: { value: 'font' } });
    expect(screen.getByRole('status')).toHaveTextContent(/settings? found/);
    const first = within(screen.getByRole('listbox', { name: 'Matching settings' })).getAllByRole('option')[0];
    expect(first).toHaveTextContent('Font size');
    expect(first).toHaveTextContent('Appearance › Text size');
    await act(async () => {
      fireEvent.keyDown(box, { key: 'Enter' });
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(screen.getByRole('tab', { selected: true })).toHaveTextContent('Appearance');
    const row = document.querySelector('[data-setting="Font size"]')!;
    expect(row.contains(document.activeElement)).toBe(true);
    expect((box as HTMLInputElement).value).toBe('');
  });

  it('says when nothing matches', () => {
    openDialog();
    fireEvent.change(screen.getByRole('combobox', { name: 'Search settings' }), { target: { value: 'zzqx' } });
    expect(screen.getByRole('status')).toHaveTextContent('No settings match');
    expect(screen.queryByRole('listbox', { name: 'Matching settings' })).toBeNull();
  });

  it('"/" and Ctrl+F focus the search; Esc clears it before closing Settings', () => {
    openDialog();
    const box = screen.getByRole('combobox', { name: 'Search settings' });
    (document.activeElement as HTMLElement | null)?.blur();
    fireEvent.keyDown(window, { key: '/' });
    expect(document.activeElement).toBe(box);
    (box as HTMLInputElement).blur();
    fireEvent.keyDown(window, { key: 'f', ctrlKey: true });
    expect(document.activeElement).toBe(box);
    fireEvent.change(box, { target: { value: 'font' } });
    fireEvent.keyDown(box, { key: 'Escape' });
    expect((box as HTMLInputElement).value).toBe('');
    expect(useSettings.getState().open).toBe(true);
  });
});
