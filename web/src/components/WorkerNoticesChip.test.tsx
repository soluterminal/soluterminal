import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { api } from '../api';
import { useStore } from '../store';
import type { PaneInfo } from '../types';
import { WorkerNoticesChip } from './WorkerNoticesChip';

afterEach(() => vi.restoreAllMocks());
const note = (id: string, text: string) => ({ id, text, source: '@lead', workerPane: 'worker', createdAt: 1 });

it('shows a quiet plain-text memo with no waiting-on-you or live status', () => {
  render(<WorkerNoticesChip pane={{ id: 'lead', workerNotices: [note('n1', 'Worker <b>@x</b> kept open: uncommitted work')] } as PaneInfo} />);
  expect(screen.getByText('Worker <b>@x</b> kept open: uncommitted work')).toBeTruthy();
  expect(screen.queryByRole('status')).toBeNull();
  expect(screen.queryByText(/Waiting on you/)).toBeNull();
  expect(document.querySelector('.pane-worker-notices b')).toBeNull();
});

it('dismisses only the visible notes, then refreshes', async () => {
  const dismiss = vi.spyOn(api, 'dismissAsk').mockResolvedValue({ ok: true });
  const refresh = vi.fn().mockResolvedValue(undefined);
  useStore.setState({ refreshPanes: refresh });
  render(<WorkerNoticesChip pane={{ id: 'lead', workerNotices: [note('n1', 'one'), note('n2', 'two')] } as PaneInfo} />);
  expect(screen.getByText('Workers kept open: 2').parentElement?.title).toBe('• one\n• two');
  fireEvent.click(screen.getByRole('button', { name: 'Dismiss notes' }));
  await waitFor(() => expect(refresh).toHaveBeenCalled());
  expect(dismiss.mock.calls.map(([id]) => id)).toEqual(['n1', 'n2']);
});

it('does not render without quiet notes', () => {
  const { container } = render(<WorkerNoticesChip pane={{ id: 'lead' } as PaneInfo} />);
  expect(container.textContent).toBe('');
});
