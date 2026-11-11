import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, type FeedbackEvent } from '../api';
import { __resetFeedbackInboxForTests, markFeedbackEventRead, useFeedbackInbox } from './useFeedbackInbox';

function event(over: Partial<FeedbackEvent> = {}): FeedbackEvent {
  return { number: 42, title: 'Paste fails', event: 'needs-info', message: 'Which browser?', at: '2026-11-10T08:00:00Z', ...over };
}

afterEach(() => {
  __resetFeedbackInboxForTests([]);
  vi.restoreAllMocks();
});

describe('markFeedbackEventRead', () => {
  it('flips a loaded row read at once and does not refetch the list', async () => {
    const list = vi.spyOn(api, 'feedbackInbox').mockResolvedValue({ events: [event()] });
    const read = vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    const { result } = renderHook(() => useFeedbackInbox());
    await waitFor(() => expect(result.current).toHaveLength(1));

    act(() => markFeedbackEventRead(42));

    expect(result.current[0].readAt).toBeTruthy();
    expect(read).toHaveBeenCalledWith(42);
    await act(async () => { await Promise.resolve(); });
    expect(list).toHaveBeenCalledTimes(1);
  });

  // A notification tap on a cold start: the first poll is still in flight and
  // answers with the row unread. The stamp must not wait for the next poll.
  it('refetches after the stamp is stored when the row has not loaded yet', async () => {
    let answerFirst: (v: { events: FeedbackEvent[] }) => void = () => {};
    const list = vi.spyOn(api, 'feedbackInbox')
      .mockImplementationOnce(() => new Promise((resolve) => { answerFirst = resolve; }))
      .mockResolvedValue({ events: [event({ readAt: '2026-11-10T08:05:00Z' })] });
    vi.spyOn(api, 'feedbackInboxRead').mockResolvedValue(undefined);
    const { result } = renderHook(() => useFeedbackInbox());

    act(() => markFeedbackEventRead(42));
    await act(async () => { await Promise.resolve(); });
    await act(async () => { answerFirst({ events: [event()] }); });

    await waitFor(() => expect(result.current[0]?.readAt).toBe('2026-11-10T08:05:00Z'));
    expect(list).toHaveBeenCalledTimes(2);
  });
});
