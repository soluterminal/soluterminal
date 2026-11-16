import { render, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { useStore } from '../store';
import type { VoiceLocalSettings } from './voice.api';
import { VoiceCleanupNotice } from './voice.register';
import { VOICE_CLEANUP_NOTICE } from '../components/settings/VoiceCleanupSetting';

// ST-181: someone who was getting plan clean-ups by default is told once.
const unset: VoiceLocalSettings = {
  enabled: false, available: false, reason: 'off', model: 'm', using: 'cloud',
  planChoiceSet: false, noticeSeen: false,
};
const usedPlan = [{ id: 1, text: 'Run the tests.', paneTitle: 'claude', delivered: true, cleanedBy: 'local' as const }];

afterEach(() => {
  vi.restoreAllMocks();
  useStore.setState({ voiceHistory: [] });
});

function setup(settings: VoiceLocalSettings, history: typeof usedPlan | [] = usedPlan) {
  useStore.setState({ voiceHistory: history });
  const toast = vi.fn();
  useStore.setState({ toast });
  vi.spyOn(api, 'getVoiceLocal').mockResolvedValue(settings);
  const seen = vi.spyOn(api, 'markVoiceCleanupNoticeSeen').mockResolvedValue({ ...settings, noticeSeen: true });
  render(<VoiceCleanupNotice />);
  return { toast, seen };
}

describe('VoiceCleanupNotice', () => {
  it('tells a past plan user once and records it', async () => {
    const { toast, seen } = setup(unset);
    await waitFor(() => expect(toast).toHaveBeenCalledWith(VOICE_CLEANUP_NOTICE, expect.objectContaining({ kind: 'info' })));
    expect(seen).toHaveBeenCalledTimes(1);
  });

  it('stays quiet when already shown or chosen', async () => {
    for (const settings of [{ ...unset, noticeSeen: true }, { ...unset, planChoiceSet: true }]) {
      const { toast, seen } = setup(settings);
      await waitFor(() => expect(api.getVoiceLocal).toHaveBeenCalled());
      await Promise.resolve();
      expect(toast).not.toHaveBeenCalled();
      expect(seen).not.toHaveBeenCalled();
      vi.restoreAllMocks();
    }
  });

  // Nobody else's app asks the daemon at all.
  it('does not ask the daemon when this device never cleaned on the plan', async () => {
    const { toast } = setup(unset, []);
    await Promise.resolve();
    expect(api.getVoiceLocal).not.toHaveBeenCalled();
    expect(toast).not.toHaveBeenCalled();
  });
});
