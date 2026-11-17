import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError } from '../../api';
import type { WorkerSettings } from '../../types';
import { WORKER_MODEL_LABEL, WorkerModelSetting } from './WorkerModelSetting';

const base: WorkerSettings = {
  maxRunning: 4,
  finishAfterMinutes: 5,
  workerLeadReservePct: 15,
  workerExpectedRunMinutes: 45,
  workerAutoArchive: true,
  workerAccounts: 'active',
  workerModel: '',
};

afterEach(() => {
  vi.restoreAllMocks();
});

describe('WorkerModelSetting', () => {
  it("shows Claude Code's default when no worker model is set", async () => {
    vi.spyOn(api, 'workerSettings').mockResolvedValue(base);
    render(<WorkerModelSetting />);
    expect(await screen.findByLabelText(WORKER_MODEL_LABEL)).toHaveValue('');
  });

  it('saves a pick and shows what the daemon kept', async () => {
    vi.spyOn(api, 'workerSettings').mockResolvedValue(base);
    const put = vi.spyOn(api, 'updateWorkerSettings').mockResolvedValue({ ...base, workerModel: 'opus' });
    render(<WorkerModelSetting />);
    const select = await screen.findByLabelText(WORKER_MODEL_LABEL);
    fireEvent.change(select, { target: { value: 'opus' } });
    await waitFor(() => expect(put).toHaveBeenCalledWith({ workerModel: 'opus' }));
    expect(select).toHaveValue('opus');
  });

  it('keeps a saved model that is not in the list', async () => {
    vi.spyOn(api, 'workerSettings').mockResolvedValue({ ...base, workerModel: 'claude-opus-5-5' });
    render(<WorkerModelSetting />);
    expect(await screen.findByLabelText(WORKER_MODEL_LABEL)).toHaveValue('claude-opus-5-5');
  });

  it("puts the old value back and shows the daemon's refusal", async () => {
    vi.spyOn(api, 'workerSettings').mockResolvedValue({ ...base, workerModel: 'sonnet' });
    vi.spyOn(api, 'updateWorkerSettings').mockRejectedValue(
      new ApiError(403, '{}', { error: 'human_required', message: 'Only you can change the worker model.' }),
    );
    render(<WorkerModelSetting />);
    const select = await screen.findByLabelText(WORKER_MODEL_LABEL);
    fireEvent.change(select, { target: { value: '' } });
    expect(await screen.findByText('Only you can change the worker model.')).toBeInTheDocument();
    expect(select).toHaveValue('sonnet');
  });

  it('stays hidden on a daemon without the setting', async () => {
    const { workerModel: _omitted, ...older } = base;
    vi.spyOn(api, 'workerSettings').mockResolvedValue(older);
    const { container } = render(<WorkerModelSetting />);
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });
});
