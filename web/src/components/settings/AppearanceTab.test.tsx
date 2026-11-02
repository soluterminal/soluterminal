import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { __recomputeEmbedForTests, installEmbedBridge } from '../../embed';
import { useStore } from '../../store';
import { THEMES } from '../../themes';
import { AppearanceTab } from './AppearanceTab';

function setEmbed(framed: boolean, theme = '') {
  __recomputeEmbedForTests({
    framed, search: `?embed=shinoque${theme ? `&theme=${theme}` : ''}`,
    saved: null, ancestorOrigin: 'http://localhost:7777', referrer: '',
  });
}

describe('Appearance app theme', () => {
  beforeEach(() => {
    setEmbed(false);
    useStore.setState({ appTheme: 'system', terminalThemeExplicit: false });
  });
  afterEach(() => setEmbed(false));

  it('shows the host theme and prevents preference changes while keeping terminal themes editable', () => {
    setEmbed(true, 'light');
    render(<AppearanceTab />);
    const select = screen.getByRole('combobox', { name: 'App theme' });
    expect(select).toBeDisabled();
    expect(select).toHaveValue('light');
    expect(screen.getByText('Follows Shinoque’s theme. Change it in Shinoque Settings → Appearance.')).toBeVisible();
    fireEvent.change(select, { target: { value: 'dark' } });
    expect(useStore.getState().appTheme).toBe('system');

    const terminalSelect = screen.getAllByRole('combobox')[1];
    expect(terminalSelect).toBeEnabled();
    fireEvent.change(terminalSelect, { target: { value: THEMES[0].name } });
    expect(useStore.getState().terminalThemeName).toBe(THEMES[0].name);
    expect(useStore.getState().terminalThemeExplicit).toBe(true);
    fireEvent.change(terminalSelect, { target: { value: 'auto' } });
    expect(useStore.getState().terminalThemeExplicit).toBe(false);
  });

  it('keeps standalone preferences editable', () => {
    render(<AppearanceTab />);
    const select = screen.getByRole('combobox', { name: 'App theme' });
    expect(select).toBeEnabled();
    expect(select).toHaveValue('system');
    expect(screen.getByText(/System follows your computer's setting/)).toBeVisible();
    fireEvent.change(select, { target: { value: 'dark' } });
    expect(useStore.getState().appTheme).toBe('dark');
    expect(select).toHaveValue('dark');
  });

  it('stays editable until the host supplies a theme, then follows init and theme messages live', () => {
    setEmbed(true);
    const dispose = installEmbedBridge(() => {});
    try {
      render(<AppearanceTab />);
      const select = screen.getByRole('combobox', { name: 'App theme' });
      expect(select).toBeEnabled();
      for (const [kind, theme] of [['init', 'dark'], ['theme', 'light']]) {
        act(() => window.dispatchEvent(new MessageEvent('message', {
          source: window.parent, origin: 'http://localhost:7777',
          data: { source: 'shinoque', v: 1, kind, theme },
        })));
        expect(select).toBeDisabled();
        expect(select).toHaveValue(theme);
      }
      expect(useStore.getState().appTheme).toBe('system');
    } finally {
      dispose();
    }
  });
});

describe('AppearanceTab text size inputs', () => {
  const initial = {
    terminalFontSize: useStore.getState().terminalFontSize,
    terminalLineHeight: useStore.getState().terminalLineHeight,
    uiScale: useStore.getState().uiScale,
  };
  afterEach(() => useStore.setState(initial));

  it('lets a font size be typed over the current one instead of clamping each keystroke', () => {
    useStore.setState({ terminalFontSize: 13 });
    render(<AppearanceTab />);
    const input = screen.getByRole('spinbutton', { name: 'Font size' });
    fireEvent.change(input, { target: { value: '1' } });
    expect(input).toHaveValue(1);
    expect(useStore.getState().terminalFontSize).toBe(13);
    fireEvent.change(input, { target: { value: '18' } });
    expect(useStore.getState().terminalFontSize).toBe(18);
    fireEvent.change(input, { target: { value: '' } });
    expect(input).toHaveValue(null);
    fireEvent.blur(input);
    expect(input).toHaveValue(18);
  });

  it('lets a sidebar text size below 1 be typed', () => {
    useStore.setState({ uiScale: 1 });
    render(<AppearanceTab />);
    const input = screen.getByRole('spinbutton', { name: 'Sidebar text size' });
    fireEvent.change(input, { target: { value: '0' } });
    expect(input).toHaveValue(0);
    expect(useStore.getState().uiScale).toBe(1);
    fireEvent.change(input, { target: { value: '0.9' } });
    expect(useStore.getState().uiScale).toBe(0.9);
  });

  it('clamps an out-of-range font size when the field is left', () => {
    useStore.setState({ terminalFontSize: 13 });
    render(<AppearanceTab />);
    const input = screen.getByRole('spinbutton', { name: 'Font size' });
    fireEvent.change(input, { target: { value: '108' } });
    expect(useStore.getState().terminalFontSize).toBe(13);
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(useStore.getState().terminalFontSize).toBe(22);
    expect(input).toHaveValue(22);
  });
});
