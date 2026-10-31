import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, type PaneConfirmRequest } from '../api';
import { useStore } from '../store';
import { forgetPaneConfirm, notePaneConfirm, resetPaneConfirmsForTest, resyncPaneConfirms, reviewPaneConfirm, usePaneConfirms } from './paneConfirm';

const apiMock = vi.hoisted(() => ({
  paneConfirmations: vi.fn(),
  answerPaneConfirmation: vi.fn(),
}));

vi.mock('../api', async (importOriginal) => ({ ...(await importOriginal<typeof import('../api')>()), api: apiMock }));

const request: PaneConfirmRequest = {
  id: 'req-1',
  action: 'close',
  callerPaneId: 'pane-lead',
  caller: '@cmo',
  paneId: 'pane-social',
  pane: 'pane #4 “Social loop”',
  title: 'Close pane #4 “Social loop”?',
  message: '@cmo wants to close pane #4 “Social loop”. Closing stops what is running in it; files are not deleted.',
  createdAt: 1,
  expiresAt: 2,
};

const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

describe('pane confirmations', () => {
  beforeEach(() => {
    resetPaneConfirmsForTest();
    useStore.setState({ toasts: [], dialog: null });
    apiMock.paneConfirmations.mockResolvedValue({ pending: [request] });
    apiMock.answerPaneConfirmation.mockResolvedValue({ allowed: true, failed: false, result: 'Killed pane pane-social.' });
  });
  afterEach(() => vi.clearAllMocks());

  it('lists a request once, outside the toast queue, and never opens the dialog by itself', () => {
    notePaneConfirm(request.id, request.message);
    notePaneConfirm(request.id, request.message);
    // Other notifications cannot push it out of reach.
    for (let i = 0; i < 5; i++) useStore.getState().toast(`other ${i}`);
    expect(usePaneConfirms.getState().pending).toEqual([{ id: request.id, message: request.message }]);
    expect(useStore.getState().dialog).toBeNull();
  });

  it('allows the request from the dialog', async () => {
    notePaneConfirm(request.id, request.message);
    const review = reviewPaneConfirm(request.id);
    await flush();
    expect(useStore.getState().dialog).toMatchObject({
      kind: 'confirm',
      title: request.title,
      confirmLabel: 'Allow',
      cancelLabel: 'Deny',
      danger: true,
    });
    useStore.getState().closeDialog(true);
    await review;
    expect(apiMock.answerPaneConfirmation).toHaveBeenCalledWith('req-1', true);
    expect(useStore.getState().toasts.map((t) => t.message)).toEqual(['Closed pane #4 “Social loop”.']);
    expect(usePaneConfirms.getState().pending).toEqual([]);
  });

  it('reports an allowed rename', async () => {
    apiMock.paneConfirmations.mockResolvedValue({ pending: [{ ...request, action: 'rename', title: 'Rename pane #4 “Social loop”?' }] });
    const review = reviewPaneConfirm(request.id);
    await flush();
    expect(useStore.getState().dialog).toMatchObject({ title: 'Rename pane #4 “Social loop”?', danger: false });
    useStore.getState().closeDialog(true);
    await review;
    expect(useStore.getState().toasts.at(-1)?.message).toBe('Renamed pane #4 “Social loop”.');
  });

  it('dismissing the dialog denies', async () => {
    const review = reviewPaneConfirm(request.id);
    await flush();
    useStore.getState().closeDialog(false);
    await review;
    expect(apiMock.answerPaneConfirmation).toHaveBeenCalledWith('req-1', false);
  });

  it('keeps the request on screen when the answer is refused', async () => {
    const message = 'This browser window was started from inside a SoluTerminal pane, so it counts as an agent.';
    apiMock.answerPaneConfirmation.mockRejectedValue(new ApiError(403, '{}', { error: 'human_required', message }));
    notePaneConfirm(request.id, request.message);
    const review = reviewPaneConfirm(request.id);
    await flush();
    useStore.getState().closeDialog(true);
    await review;
    expect(useStore.getState().toasts.map((t) => t.message)).toEqual([`Couldn't send your answer: ${message}`]);
    expect(usePaneConfirms.getState().pending).toHaveLength(1);
  });

  it('says so when the request was already answered', async () => {
    apiMock.paneConfirmations.mockResolvedValue({ pending: [] });
    await reviewPaneConfirm(request.id);
    expect(useStore.getState().dialog).toBeNull();
    expect(apiMock.answerPaneConfirmation).not.toHaveBeenCalled();
    expect(useStore.getState().toasts.at(-1)?.message).toBe('That request was already answered or has expired.');
  });

  it('drops a request answered elsewhere, and resyncs to the open ones', async () => {
    notePaneConfirm('stale', 'answered while disconnected');
    await resyncPaneConfirms();
    expect(usePaneConfirms.getState().pending.map((p) => p.id)).toEqual([request.id]);
    forgetPaneConfirm(request.id);
    expect(usePaneConfirms.getState().pending).toEqual([]);
  });
});
