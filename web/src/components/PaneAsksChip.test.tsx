import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { api } from '../api';
import { useStore } from '../store';
import type { PaneInfo } from '../types';
import { PaneAsksChip } from './PaneAsksChip';

afterEach(() => vi.restoreAllMocks());
const ask = (id: string, text: string) => ({ id, text, source: '@lead', createdAt: Date.now() });

it('shows a single ask and dismisses that ask', async () => {
  const dismiss = vi.spyOn(api, 'dismissAsk').mockResolvedValue({ ok: true });
  const refresh = vi.fn().mockResolvedValue(undefined);
  useStore.setState({ refreshPanes: refresh });
  render(<PaneAsksChip pane={{ id: 'p1', asks: [ask('a1', '<b>OK</b> to deploy?')] } as PaneInfo} />);
  // Plain text, never markup.
  expect(screen.getByRole('status').textContent).toContain('Waiting on you: <b>OK</b> to deploy?');
  fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
  await waitFor(() => expect(refresh).toHaveBeenCalled());
  expect(dismiss).toHaveBeenCalledExactlyOnceWith('a1');
});

it('counts several asks and lists them on hover, without a dismiss-all', () => {
  render(<PaneAsksChip pane={{ id: 'p1', asks: [ask('a1', 'one'), ask('a2', 'two')] } as PaneInfo} />);
  const chip = screen.getByRole('status');
  expect(chip.textContent).toBe('2 asks waiting on you');
  expect(chip.getAttribute('title')).toBe('• one\n• two');
  expect(screen.queryByRole('button')).toBeNull();
});

it('renders nothing without asks', () => {
  const { container } = render(<PaneAsksChip pane={{ id: 'p1' } as PaneInfo} />);
  expect(container.textContent).toBe('');
});
