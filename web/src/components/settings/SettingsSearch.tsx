import { forwardRef, useEffect, useId, useMemo, useState } from 'react';
import type { KeyboardEvent } from 'react';
import type { SettingsTab } from '../../settings';
import {
  searchSettings,
  settingsControls,
  settingsTabFor,
  type SettingsSearchHit,
  type SettingsTarget,
} from '../../settingsRegistry';

// Search in Settings (ST-281). Matches row labels, section titles,
// descriptions and aliases from the settings registry; a result shows
// "Tab › Section", and choosing one opens that tab and lands on the row.

export interface SettingsSearchResult {
  key: string;
  title: string;
  breadcrumb: string;
  target: SettingsTarget;
  /** The row's data-setting label to land on; undefined lands on the tab. */
  landOn?: string;
}

/** On a phone, Privacy & security holds only this browser's encryption switch. */
const PHONE_PRIVACY_CONTROL = 'devices.privacy.privateRelay';

/** The most results shown, after the tab filter. */
const MAX_RESULTS = 30;

export function settingsSearchResults(
  query: string,
  { tabs, mobile, shellAutoApplyPaused = false }: {
    tabs: { key: SettingsTab; label: string }[];
    mobile: boolean;
    /** As DevicesTab: Desktop app restarts is hidden while this is true. */
    shellAutoApplyPaused?: boolean;
  },
): SettingsSearchResult[] {
  const labels = new Map(tabs.map((tab) => [tab.key, tab.label]));
  const controls = settingsControls();
  const seen = new Set<string>();
  const results: SettingsSearchResult[] = [];
  // Which tabs show is the dialog's call (account, team and phone rules), so
  // search everything and keep only hits whose tab is on the rail.
  for (const hit of searchSettings(query, { mobile: false, shellAutoApplyPaused, limit: Infinity })) {
    if (results.length >= MAX_RESULTS) break;
    const tab = dialogTab(hit, mobile);
    const tabLabel = labels.get(tab);
    if (!tabLabel) continue;
    if (mobile && tab === 'privacy' && hit.control?.id !== PHONE_PRIVACY_CONTROL) continue;
    const key = hit.control?.id ?? hit.section.id;
    const title = hit.control?.label ?? hit.section.title;
    // A section named like its tab (Account, Privacy & security) shows once.
    const breadcrumb = hit.section.title === tabLabel ? tabLabel : `${tabLabel} › ${hit.section.title}`;
    // A section and its row with the same name read the same: keep one.
    const reads = `${title}\n${breadcrumb}`;
    if (seen.has(key) || seen.has(reads)) continue;
    seen.add(key);
    seen.add(reads);
    results.push({
      key,
      title,
      breadcrumb,
      target: {
        category: hit.category.id,
        section: hit.section.id,
        ...(hit.control ? { control: hit.control.id } : {}),
      },
      // A section result lands on its first row.
      landOn: hit.control?.label ?? controls.find((control) => control.section === hit.section.id)?.label,
    });
  }
  return results;
}

// The dialog shows the old Claude sessions tab inside Terminal & panes, and a
// phone's Devices rows inside Privacy & security.
function dialogTab(hit: SettingsSearchHit, mobile: boolean): SettingsTab {
  const tab = settingsTabFor({ category: hit.category.id, section: hit.section.id });
  if (tab === 'claude') return 'panes';
  if (tab === 'devices' && mobile) return 'privacy';
  return tab;
}

const FOCUSABLE = 'input:not([disabled]), select:not([disabled]), textarea:not([disabled]), button:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])';

function attrValue(value: string): string {
  return value.replace(/["\\]/g, '\\$&');
}

/**
 * Scrolls to and focuses the row whose data-setting is `label`, opening any
 * closed <details> it sits in. Rows that load their data appear a moment
 * after the tab, so it looks again for up to about two seconds.
 */
export function landOnSetting(container: HTMLElement | null, label: string | undefined, tries = 40): void {
  if (!container || !label) return;
  const row = container.querySelector<HTMLElement>(`[data-setting="${attrValue(label)}"]`);
  if (!row) {
    if (tries > 0) setTimeout(() => landOnSetting(container, label, tries - 1), 50);
    return;
  }
  for (let el = row.parentElement; el && el !== container; el = el.parentElement) {
    if (el instanceof HTMLDetailsElement && !el.open) el.open = true;
  }
  row.scrollIntoView?.({ block: 'center' });
  const control = row.querySelector<HTMLElement>(FOCUSABLE);
  if (control) control.focus({ preventScroll: true });
  else {
    row.tabIndex = -1;
    row.focus({ preventScroll: true });
  }
  row.classList.add('settings-row-found');
  setTimeout(() => row.classList.remove('settings-row-found'), 1600);
}

export const SettingsSearchBox = forwardRef<HTMLInputElement, {
  query: string;
  onQuery: (query: string) => void;
  results: SettingsSearchResult[];
  onChoose: (result: SettingsSearchResult) => void;
}>(function SettingsSearchBox({ query, onQuery, results, onChoose }, ref) {
  const listId = useId();
  const [active, setActive] = useState(0);
  useEffect(() => setActive(0), [query]);
  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown' && results.length) {
      e.preventDefault();
      setActive((i) => (i + 1) % results.length);
    } else if (e.key === 'ArrowUp' && results.length) {
      e.preventDefault();
      setActive((i) => (i - 1 + results.length) % results.length);
    } else if (e.key === 'Enter' && results.length) {
      e.preventDefault();
      onChoose(results[Math.min(active, results.length - 1)]);
    } else if (e.key === 'Escape' && query) {
      // Clear the search first; a second Esc closes Settings.
      e.preventDefault();
      e.nativeEvent.stopPropagation();
      onQuery('');
    }
  };
  const searching = query.trim().length > 0;
  return (
    <div className="settings-search">
      <input
        ref={ref}
        type="search"
        className="settings-search-input"
        placeholder="Search settings  ( / )"
        aria-label="Search settings"
        role="combobox"
        aria-expanded={searching && results.length > 0}
        aria-controls={listId}
        aria-autocomplete="list"
        aria-activedescendant={searching && results.length ? `${listId}-${active}` : undefined}
        value={query}
        onChange={(e) => onQuery(e.target.value)}
        onKeyDown={onKeyDown}
      />
      <p className="settings-search-count" role="status" aria-live="polite">
        {searching ? (results.length ? `${results.length} ${results.length === 1 ? 'setting' : 'settings'} found` : 'No settings match') : ''}
      </p>
      {searching && results.length > 0 && (
        <ul id={listId} className="settings-search-results" role="listbox" aria-label="Matching settings">
          {results.map((result, i) => (
            <li
              key={result.key}
              id={`${listId}-${i}`}
              role="option"
              aria-selected={i === active}
              className={`settings-search-result${i === active ? ' active' : ''}`}
              onMouseDown={(e) => e.preventDefault()}
              onMouseEnter={() => setActive(i)}
              onClick={() => onChoose(result)}
            >
              <span className="settings-search-title">{result.title}</span>
              <span className="settings-search-crumb">{result.breadcrumb}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
});

/** The visible results for a query, memoised on the inputs. */
export function useSettingsSearch(
  query: string,
  tabs: { key: SettingsTab; label: string }[],
  mobile: boolean,
  shellAutoApplyPaused: boolean,
): SettingsSearchResult[] {
  const tabKey = tabs.map((tab) => tab.key).join(',');
  return useMemo(
    () => settingsSearchResults(query, { tabs, mobile, shellAutoApplyPaused }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [query, tabKey, mobile, shellAutoApplyPaused],
  );
}
