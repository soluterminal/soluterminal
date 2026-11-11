import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, type FeedbackEvent } from '../api';
import { __resetFeedbackInboxForTests, noteFeedbackSent, useFeedbackInbox } from './useFeedbackInbox';

function received(over: Partial<FeedbackEvent> = {}): FeedbackEvent {
  return { number: 812, title: 'Tabs vanish', event: 'received', message: 'Report received', at: '2026-11-10T08:00:00Z', ...over };
}

afterEach(() => {
  __resetFeedbackInboxForTests([]);
  vi.restoreAllMocks();
});

describe('noteFeedbackSent', () => {
  it('stamps the new report read, then fetches the list', async () => {
    const list = vi.spyOn(api, 'feedbackInbox')
      .mockResolvedValueOnce({ events: [] })
      .mockResolvedValue({ events: [received({ readAt: '2026-11-10T08:00:01Z' })] });
    const read = vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    const { result } = renderHook(() => useFeedbackInbox());
    await waitFor(() => expect(list).toHaveBeenCalledTimes(1));

    act(() => noteFeedbackSent(812));

    await waitFor(() => expect(result.current[0]?.readAt).toBe('2026-11-10T08:00:01Z'));
    expect(read).toHaveBeenCalledWith(812);
    expect(list.mock.invocationCallOrder[1]).toBeGreaterThan(read.mock.invocationCallOrder[0]);
  });

  // A poll already in flight when the stamp lands answers with the list as it
  // was before the stamp. It must not stand until the next poll.
  it('fetches once more when a poll was in flight as the stamp landed', async () => {
    let answerPoll: (v: { events: FeedbackEvent[] }) => void = () => {};
    const list = vi.spyOn(api, 'feedbackInbox')
      .mockImplementationOnce(() => new Promise((resolve) => { answerPoll = resolve; }))
      .mockResolvedValue({ events: [received({ readAt: '2026-11-10T08:00:01Z' })] });
    vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    const { result } = renderHook(() => useFeedbackInbox());

    act(() => noteFeedbackSent(812));
    await act(async () => { await Promise.resolve(); });
    await act(async () => { answerPoll({ events: [received()] }); });

    await waitFor(() => expect(result.current[0]?.readAt).toBe('2026-11-10T08:00:01Z'));
    expect(list).toHaveBeenCalledTimes(2);
  });

  it('still fetches when the stamp could not be stored', async () => {
    const list = vi.spyOn(api, 'feedbackInbox').mockResolvedValue({ events: [received()] });
    vi.spyOn(api, 'feedbackInboxRead').mockRejectedValue(new Error('offline'));
    renderHook(() => useFeedbackInbox());
    await waitFor(() => expect(list).toHaveBeenCalledTimes(1));

    act(() => noteFeedbackSent(812));

    await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
  });
});
