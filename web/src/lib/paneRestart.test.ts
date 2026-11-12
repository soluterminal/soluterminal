import { beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, api } from '../api';
import { useStore } from '../store';
import type { PaneInfo } from '../types';
import { confirmDialog } from './dialog';
import { restartPaneFlow, restartTerminalFlow } from './paneRestart';

vi.mock('./dialog', () => ({ confirmDialog: vi.fn() }));

const paneInfo = (id: string): PaneInfo =>
  ({ id, cwd: '/x', cmd: ['claude'], status: 'running', state: 'idle' } as PaneInfo);

describe('restartPaneFlow', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.mocked(confirmDialog).mockReset();
    useStore.setState({ panes: {}, layout: null, parkedPaneIds: [] });
  });

  it('restarts in place without a prompt on success', async () => {
    const p = paneInfo('p');
    useStore.setState({ panes: { p } });
    const restart = vi.spyOn(api, 'restartPane').mockResolvedValue({ ...p, pid: 7 } as PaneInfo);
    await restartPaneFlow('p');
    expect(restart).toHaveBeenCalledWith('p', false, false);
    expect(confirmDialog).not.toHaveBeenCalled();
  });

  it('confirms and retries with fresh when the daemon has nothing to resume', async () => {
    const p = paneInfo('p');
    useStore.setState({ panes: { p } });
    const restart = vi.spyOn(api, 'restartPane')
      .mockRejectedValueOnce(new ApiError(409, 'no durable conversation to resume', { needsFresh: true }))
      .mockResolvedValueOnce({ ...p, pid: 9 } as PaneInfo);
    vi.mocked(confirmDialog).mockResolvedValue(true);
    await restartPaneFlow('p');
    expect(restart).toHaveBeenNthCalledWith(1, 'p', false, false);
    expect(confirmDialog).toHaveBeenCalledTimes(1);
    expect(restart).toHaveBeenNthCalledWith(2, 'p', true, true);
  });

  it('leaves the pane alone when the fresh confirm is declined', async () => {
    const p = paneInfo('p');
    useStore.setState({ panes: { p } });
    const restart = vi.spyOn(api, 'restartPane').mockRejectedValueOnce(
      new ApiError(409, 'no durable conversation to resume', { needsFresh: true }),
    );
    vi.mocked(confirmDialog).mockResolvedValue(false);
    await restartPaneFlow('p');
    expect(restart).toHaveBeenCalledTimes(1);
  });

  it('offers a confirmed forced restart when the pane is busy', async () => {
    const p = paneInfo('p');
    useStore.setState({ panes: { p } });
    const restart = vi.spyOn(api, 'restartPane')
      .mockRejectedValueOnce(new ApiError(409, 'agent is busy'))
      .mockResolvedValueOnce({ ...p, pid: 11 } as PaneInfo);
    vi.mocked(confirmDialog).mockResolvedValue(true);
    await restartPaneFlow('p');
    expect(confirmDialog).toHaveBeenCalledTimes(1);
    expect(restart).toHaveBeenNthCalledWith(2, 'p', false, true);
  });

  it('does not force a busy pane when the confirm is declined', async () => {
    const p = paneInfo('p');
    useStore.setState({ panes: { p } });
    const restart = vi.spyOn(api, 'restartPane').mockRejectedValueOnce(new ApiError(409, 'agent is busy'));
    vi.mocked(confirmDialog).mockResolvedValue(false);
    await restartPaneFlow('p');
    expect(restart).toHaveBeenCalledTimes(1);
  });
});

describe('restartTerminalFlow', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.mocked(confirmDialog).mockReset();
    useStore.setState({ panes: {}, layout: null, parkedPaneIds: [] });
  });

  it('reopens the shell fresh, never resuming an adopted conversation', async () => {
    const p = paneInfo('p');
    useStore.setState({ panes: { p } });
    const restart = vi.spyOn(api, 'restartPane').mockResolvedValue({ ...p, pid: 7 } as PaneInfo);
    await restartTerminalFlow('p');
    expect(restart).toHaveBeenCalledTimes(1);
    expect(restart).toHaveBeenCalledWith('p', true, false);
    expect(confirmDialog).not.toHaveBeenCalled();
  });

  it('asks before ending a busy terminal, and stays fresh when forced', async () => {
    const p = paneInfo('p');
    useStore.setState({ panes: { p } });
    const restart = vi.spyOn(api, 'restartPane')
      .mockRejectedValueOnce(new ApiError(409, 'agent is busy'))
      .mockResolvedValueOnce({ ...p, pid: 11 } as PaneInfo);
    vi.mocked(confirmDialog).mockResolvedValue(true);
    await restartTerminalFlow('p');
    expect(confirmDialog).toHaveBeenCalledTimes(1);
    expect(restart).toHaveBeenNthCalledWith(2, 'p', true, true);
  });

  it('leaves a busy terminal alone when the confirm is declined', async () => {
    const p = paneInfo('p');
    useStore.setState({ panes: { p } });
    const restart = vi.spyOn(api, 'restartPane').mockRejectedValueOnce(new ApiError(409, 'agent is busy'));
    vi.mocked(confirmDialog).mockResolvedValue(false);
    await restartTerminalFlow('p');
    expect(restart).toHaveBeenCalledTimes(1);
  });
});
