import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from './api';
import { AUTO_UPDATE_ASK_SNOOZE_MS, resetAutoUpdateAskForTests, useStore } from './store';
import type { UpdateSettings } from './types';

const dialog = vi.hoisted(() => ({
  answer: null as boolean | null,
  asked: vi.fn(),
}));

vi.mock('./lib/dialog', async (orig) => ({
  ...(await orig<typeof import('./lib/dialog')>()),
  confirmOrDismissDialog: (opts: unknown) => {
    dialog.asked(opts);
    return Promise.resolve(dialog.answer);
  },
}));

const asking: UpdateSettings = { autoApply: 'off', shellAutoApply: 'off', autoApplyAsk: true };
const NOW = 1_800_000_000_000;

let put: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  localStorage.clear();
  resetAutoUpdateAskForTests();
  dialog.asked.mockClear();
  dialog.answer = null;
  useStore.setState({ updateSettings: asking });
  put = vi.spyOn(api, 'putUpdateSettings').mockImplementation(async (body) => ({
    autoApply: body.autoApply ?? 'off',
    shellAutoApply: body.shellAutoApply,
    autoApplyAsk: false,
  }));
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('the one-time automatic-updates question', () => {
  it('asks in plain words when the daemon says to', async () => {
    await useStore.getState().maybeAskAutoUpdate(NOW);
    expect(dialog.asked).toHaveBeenCalledTimes(1);
    expect(dialog.asked.mock.calls[0][0]).toMatchObject({
      message: expect.stringContaining('Install updates by themselves when nothing is running?'),
      confirmLabel: 'Turn on',
      cancelLabel: 'Keep notify me',
    });
  });

  it.each([
    ['a fresh install (nothing to ask)', { autoApply: 'when-idle', autoApplyAsk: false }],
    ['an older daemon that omits the field', { autoApply: 'off' }],
    ['SOLU_AUTO_UPDATE=0 (daemon reports no question)', { autoApply: 'off', autoApplyAsk: false }],
  ] as const)('does not ask for %s', async (_name, settings) => {
    useStore.setState({ updateSettings: settings as UpdateSettings });
    await useStore.getState().maybeAskAutoUpdate(NOW);
    expect(dialog.asked).not.toHaveBeenCalled();
    expect(put).not.toHaveBeenCalled();
  });

  it('Turn on saves when-idle as the answer', async () => {
    dialog.answer = true;
    await useStore.getState().maybeAskAutoUpdate(NOW);
    expect(put).toHaveBeenCalledWith(expect.objectContaining({ autoApply: 'when-idle', autoApplyAnswered: true }));
    expect(useStore.getState().updateSettings).toMatchObject({ autoApply: 'when-idle', autoApplyAsk: false });
  });

  it('Keep notify me records off as the answer and changes nothing else', async () => {
    dialog.answer = false;
    await useStore.getState().maybeAskAutoUpdate(NOW);
    expect(put).toHaveBeenCalledWith(expect.objectContaining({ autoApply: 'off', autoApplyAnswered: true }));
    expect(useStore.getState().updateSettings).toMatchObject({ autoApply: 'off', autoApplyAsk: false });
  });

  it('dismissing without an answer saves nothing and asks again only after the snooze', async () => {
    dialog.answer = null;
    await useStore.getState().maybeAskAutoUpdate(NOW);
    expect(put).not.toHaveBeenCalled();
    expect(useStore.getState().updateSettings).toMatchObject({ autoApply: 'off', autoApplyAsk: true });

    // Reloads and polls over the next hours and days do not ask again.
    for (const later of [NOW + 5 * 60_000, NOW + 24 * 3_600_000, NOW + AUTO_UPDATE_ASK_SNOOZE_MS - 1]) {
      await useStore.getState().maybeAskAutoUpdate(later);
    }
    expect(dialog.asked).toHaveBeenCalledTimes(1);

    await useStore.getState().maybeAskAutoUpdate(NOW + AUTO_UPDATE_ASK_SNOOZE_MS + 1);
    expect(dialog.asked).toHaveBeenCalledTimes(2);
    expect(put).not.toHaveBeenCalled();
  });

  it('a failed save records nothing and is not retried at once', async () => {
    dialog.answer = true;
    put.mockRejectedValueOnce(new Error('offline'));
    await useStore.getState().maybeAskAutoUpdate(NOW);
    await useStore.getState().maybeAskAutoUpdate(NOW + 60_000);
    expect(dialog.asked).toHaveBeenCalledTimes(1);
    expect(useStore.getState().updateSettings).toMatchObject({ autoApply: 'off', autoApplyAsk: true });
  });

  it('saving the desktop-restart setting is not an answer and keeps the question', async () => {
    await useStore.getState().setUpdateShellAutoApply('when-idle');
    const body = put.mock.calls[0][0] as { autoApplyAnswered?: boolean; autoApply?: string };
    expect(body.autoApplyAnswered).toBeUndefined();
    expect(body.autoApply).toBe('off');
  });

  it('choosing in Settings is an answer', async () => {
    await useStore.getState().setUpdateAutoApply('off');
    expect(put).toHaveBeenCalledWith(expect.objectContaining({ autoApply: 'off', autoApplyAnswered: true }));
  });
});
