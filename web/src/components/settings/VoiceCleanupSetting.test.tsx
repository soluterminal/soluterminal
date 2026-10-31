import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../api';
import type { VoiceLocalSettings } from '../../private/voice.api';
import { useStore } from '../../store';
import { VOICE_AUTO_SEND_LABEL, VOICE_KEEP_MIC_LABEL,
  VOICE_READY_TICK_LABEL, VOICE_LOCAL_LABEL, VoiceCleanupSetting, voiceLocalHint } from './VoiceCleanupSetting';

const on: VoiceLocalSettings = { enabled: true, available: true, model: 'claude-haiku-4-5' };

afterEach(() => {
  vi.restoreAllMocks();
});

describe('VoiceCleanupSetting', () => {
  it('is on by default and explains where the terminal text goes', async () => {
    vi.spyOn(api, 'getVoiceLocal').mockResolvedValue(on);
    render(<VoiceCleanupSetting />);
    expect(await screen.findByLabelText(VOICE_LOCAL_LABEL)).toBeChecked();
    expect(screen.getByText(/goes only to Anthropic/)).toBeInTheDocument();
  });

  it('saves the choice to the daemon', async () => {
    vi.spyOn(api, 'getVoiceLocal').mockResolvedValue(on);
    const put = vi.spyOn(api, 'setVoiceLocal').mockResolvedValue({ ...on, enabled: false, available: false, reason: 'off' });
    render(<VoiceCleanupSetting />);
    const box = await screen.findByLabelText(VOICE_LOCAL_LABEL);
    fireEvent.click(box);
    await waitFor(() => expect(put).toHaveBeenCalledWith(false));
    await waitFor(() => expect(box).not.toBeChecked());
    expect(screen.getByText(/counts toward your daily voice allowance/)).toBeInTheDocument();
  });

  it('rolls back when saving fails', async () => {
    vi.spyOn(api, 'getVoiceLocal').mockResolvedValue(on);
    vi.spyOn(api, 'setVoiceLocal').mockRejectedValue(new Error('offline'));
    render(<VoiceCleanupSetting />);
    const box = await screen.findByLabelText(VOICE_LOCAL_LABEL);
    fireEvent.click(box);
    await waitFor(() => expect(screen.getByText(/Couldn't save the voice setting/)).toBeInTheDocument());
    expect(box).toBeChecked();
  });

  it('hides the Claude-plan row on a daemon without the setting', async () => {
    const get = vi.spyOn(api, 'getVoiceLocal').mockResolvedValue(null);
    render(<VoiceCleanupSetting />);
    await waitFor(() => expect(get).toHaveBeenCalled());
    expect(screen.queryByLabelText(VOICE_LOCAL_LABEL)).toBeNull();
    expect(screen.getByLabelText(VOICE_KEEP_MIC_LABEL)).toBeInTheDocument();
  });

  it('leaves the mic closed by default and saves the opt-in', async () => {
    vi.spyOn(api, 'getVoiceLocal').mockResolvedValue(null);
    useStore.getState().setVoiceKeepWarm(false);
    render(<VoiceCleanupSetting />);
    const box = screen.getByLabelText(VOICE_KEEP_MIC_LABEL);
    expect(box).not.toBeChecked();
    expect(screen.getByText(/split second from just before you press/)).toBeInTheDocument();
    fireEvent.click(box);
    expect(useStore.getState().voiceKeepWarmMs).toBe(120_000);
    expect(localStorage.getItem('solu.voiceKeepWarmMs')).toBe('120000');
    fireEvent.click(box);
    expect(useStore.getState().voiceKeepWarmMs).toBe(0);
    expect(localStorage.getItem('solu.voiceKeepWarmMs')).toBe('0');
  });

  it('names the 2-minute window and the mic light in plain words', () => {
    expect(VOICE_KEEP_MIC_LABEL).toMatch(/2 minutes after dictating/);
    expect(VOICE_KEEP_MIC_LABEL).toMatch(/mic light stays on/);
  });

  it('ticks when the mic is listening, by default, and saves the choice', async () => {
    vi.spyOn(api, 'getVoiceLocal').mockResolvedValue(null);
    useStore.getState().setVoiceReadyTick(true);
    render(<VoiceCleanupSetting />);
    const box = screen.getByLabelText(VOICE_READY_TICK_LABEL);
    expect(box).toBeChecked();
    fireEvent.click(box);
    expect(useStore.getState().voiceReadyTick).toBe(false);
    expect(localStorage.getItem('solu.voiceReadyTick')).toBe('0');
    useStore.getState().setVoiceReadyTick(true);
  });

  it('says why it is not in use and what happens instead', () => {
    expect(voiceLocalHint({ ...on, available: false, reason: 'not_signed_in' })).toMatch(/Sign in to Claude Code/);
    expect(voiceLocalHint({ ...on, available: false, reason: 'not_installed' })).toMatch(/Needs Claude Code/);
    expect(voiceLocalHint({ ...on, available: false, reason: 'paused' })).toMatch(/Paused/);
  });
});

// Auto-send and language used to be only in the mic menu; Settings › Voice
// now holds every voice preference, on the same store values.
describe('VoiceCleanupSetting auto-send and language', () => {
  afterEach(() => {
    useStore.setState({ voiceAutoSend: false, voiceLanguage: '' });
  });

  it('toggles auto-send on the same preference the mic menu uses', async () => {
    vi.spyOn(api, 'getVoiceLocal').mockResolvedValue(null);
    useStore.setState({ voiceAutoSend: false });
    render(<VoiceCleanupSetting />);
    const box = screen.getByLabelText(VOICE_AUTO_SEND_LABEL);
    expect(box).not.toBeChecked();
    fireEvent.click(box);
    expect(useStore.getState().voiceAutoSend).toBe(true);
    expect(box).toBeChecked();
  });

  it('names each language and saves the code', async () => {
    vi.spyOn(api, 'getVoiceLocal').mockResolvedValue(null);
    useStore.setState({ voiceLanguage: '' });
    render(<VoiceCleanupSetting />);
    const select = screen.getByRole('combobox', { name: 'Voice language' });
    expect([...select.querySelectorAll('option')].map((o) => o.textContent)).toEqual([
      'Auto-detect', 'English', 'Chinese (中文)', 'Malay (Bahasa Melayu)', 'Japanese (日本語)', 'Spanish (Español)',
    ]);
    fireEvent.change(select, { target: { value: 'ms' } });
    expect(useStore.getState().voiceLanguage).toBe('ms');
  });

  it('keeps a language set outside the list selectable', async () => {
    vi.spyOn(api, 'getVoiceLocal').mockResolvedValue(null);
    useStore.setState({ voiceLanguage: 'de' });
    render(<VoiceCleanupSetting />);
    expect(screen.getByRole('combobox', { name: 'Voice language' })).toHaveValue('de');
  });
});
