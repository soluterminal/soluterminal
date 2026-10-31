import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { VoiceConfigMenu } from './VoiceConfigMenu';
import { useSettings } from '../settings';
import { VOICE_SETTINGS_TAB } from './settings/VoiceCleanupSetting';

const initialSettings = useSettings.getState();

afterEach(() => {
  vi.restoreAllMocks();
  useSettings.setState(initialSettings, true);
});

// The mic menu keeps its quick toggles and history, and links to the one
// Settings › Voice group that holds every voice preference.
describe('VoiceConfigMenu', () => {
  it('keeps the quick toggles and opens Settings › Voice', () => {
    const openSettings = vi.spyOn(useSettings.getState(), 'openSettings');
    render(<VoiceConfigMenu />);
    fireEvent.click(screen.getByRole('button', { name: 'Voice settings' }));
    expect(screen.getByText('Auto-send')).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Voice language' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Voice settings…' }));
    expect(openSettings).toHaveBeenCalledWith(VOICE_SETTINGS_TAB);
    expect(screen.queryByRole('button', { name: 'Voice settings…' })).toBeNull();
  });
});
