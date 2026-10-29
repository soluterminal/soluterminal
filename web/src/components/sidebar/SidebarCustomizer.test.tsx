import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../../cloud', () => ({
  useCloud: { subscribe: vi.fn(), getState: () => ({ session: null }) },
}));
vi.mock('../../api', () => ({
  api: {
    getUserPrefs: vi.fn().mockResolvedValue({ prefs: {}, updated_at: null }),
    putUserPrefs: vi.fn().mockResolvedValue({ prefs: {}, updated_at: '2026-07-04T00:00:00.000Z' }),
  },
}));

import { usePrefs } from '../../prefs';
import { PERSONAL_VIEW_SIDEBAR_NOTE, SidebarCustomizer } from './SidebarCustomizer';
import { useEntitlements } from '../../entitlements';
import { DEFAULT_HIDDEN, DEFAULT_MIDDLE_ORDER, MIDDLE_SECTION_LABELS, sectionOffered } from './sidebarSections';
import { setExtensionsForTests } from '../../extensions';

describe('SidebarCustomizer', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    localStorage.clear();
    usePrefs.setState({ prefs: {}, updatedAt: null, hydrated: false });
    useEntitlements.setState({ loaded: true, team: true, operator: true });
  });

  afterEach(() => {
    setExtensionsForTests(null);
    vi.useRealTimers();
  });

  it('lists every offered section; shipped default-hidden ones start unchecked', () => {
    render(<SidebarCustomizer />);
    const defaultHidden = new Set<string>(DEFAULT_HIDDEN);
    for (const key of DEFAULT_MIDDLE_ORDER.filter(sectionOffered)) {
      const cb = screen.getByRole('checkbox', { name: MIDDLE_SECTION_LABELS[key] });
      if (defaultHidden.has(key)) expect(cb).not.toBeChecked();
      else expect(cb).toBeChecked();
    }
  });

  it('offers a feature-rendered section only when a module registers it', () => {
    setExtensionsForTests([]);
    const { unmount } = render(<SidebarCustomizer />);
    expect(screen.queryByRole('checkbox', { name: MIDDLE_SECTION_LABELS.agents })).toBeNull();
    unmount();
    setExtensionsForTests([{ sidebarSections: { agents: () => null } }]);
    render(<SidebarCustomizer />);
    expect(screen.getByRole('checkbox', { name: MIDDLE_SECTION_LABELS.agents })).toBeInTheDocument();
  });

  it('leaves Task Owners and Agents out for a public install', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false });
    render(<SidebarCustomizer />);
    expect(screen.queryByRole('checkbox', { name: MIDDLE_SECTION_LABELS.owners })).toBeNull();
    expect(screen.queryByRole('checkbox', { name: MIDDLE_SECTION_LABELS.agents })).toBeNull();
    // Files and Localhost ports start hidden for a public install, and can be
    // switched back on here. Todos was removed and is not offered at all.
    expect(screen.getByRole('checkbox', { name: MIDDLE_SECTION_LABELS.files })).not.toBeChecked();
    expect(screen.queryByRole('checkbox', { name: /todo/i })).toBeNull();
    expect(screen.getByRole('checkbox', { name: MIDDLE_SECTION_LABELS.ports })).not.toBeChecked();
    expect(screen.getByRole('checkbox', { name: MIDDLE_SECTION_LABELS.schedules })).toBeChecked();
    fireEvent.click(screen.getByRole('checkbox', { name: MIDDLE_SECTION_LABELS.ports }));
    expect(usePrefs.getState().prefs.sidebar?.hidden).not.toContain('ports');
    expect(usePrefs.getState().prefs.sidebar?.hidden).toEqual(expect.arrayContaining(['files']));
  });

  it('unchecking a visible section records it as hidden in prefs', () => {
    render(<SidebarCustomizer />);
    fireEvent.click(screen.getByRole('checkbox', { name: MIDDLE_SECTION_LABELS.files }));
    expect(usePrefs.getState().prefs.sidebar?.hidden).toContain('files');
  });

  it('re-checking a default-hidden section removes it from hidden', () => {
    render(<SidebarCustomizer />);
    // mcp ships hidden; checking it should un-hide (and materialize the
    // rest of the default-hidden set as an explicit list).
    fireEvent.click(screen.getByRole('checkbox', { name: MIDDLE_SECTION_LABELS.mcp }));
    const hidden = usePrefs.getState().prefs.sidebar?.hidden ?? [];
    expect(hidden).not.toContain('mcp');
    for (const key of DEFAULT_HIDDEN) {
      if (key !== 'mcp') expect(hidden).toContain(key);
    }
  });

  it('reset is disabled at defaults, enabled after a change, and clears prefs', () => {
    render(<SidebarCustomizer />);
    expect(screen.getByRole('button', { name: /Reset to default/ })).toBeDisabled();

    fireEvent.click(screen.getByRole('checkbox', { name: MIDDLE_SECTION_LABELS.files }));
    expect(usePrefs.getState().prefs.sidebar?.hidden).toContain('files');
    expect(screen.getByRole('button', { name: /Reset to default/ })).not.toBeDisabled();

    fireEvent.click(screen.getByRole('button', { name: /Reset to default/ }));
    expect(usePrefs.getState().prefs.sidebar).toEqual({});
  });
});

describe('SidebarCustomizer in personal view', () => {
  beforeEach(() => {
    localStorage.clear();
    usePrefs.setState({ prefs: { sidebar: { hidden: ['files'] } }, updatedAt: null, hydrated: false });
    useEntitlements.setState({ loaded: true, team: false, operator: false, viewPersonal: true });
  });

  afterEach(() => {
    useEntitlements.setState({ viewPersonal: false });
  });

  it('is read-only and says why, leaving the real arrangement untouched', () => {
    render(<SidebarCustomizer />);
    expect(screen.getByText(PERSONAL_VIEW_SIDEBAR_NOTE)).toBeInTheDocument();
    const box = screen.getByRole('checkbox', { name: MIDDLE_SECTION_LABELS.schedules });
    expect(box).toBeDisabled();
    fireEvent.click(box);
    expect(screen.getByRole('button', { name: 'Reset to default' })).toBeDisabled();
    for (const row of document.querySelectorAll('.sidebar-customizer-row')) {
      expect(row.getAttribute('draggable')).toBe('false');
    }
    expect(usePrefs.getState().prefs.sidebar).toEqual({ hidden: ['files'] });
  });
});
