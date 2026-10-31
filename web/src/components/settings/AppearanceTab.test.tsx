import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { useStore } from '../../store';
import { AppearanceTab } from './AppearanceTab';

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
