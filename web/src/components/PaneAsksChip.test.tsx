import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { api } from '../api';
import { useStore } from '../store';
import type { PaneInfo } from '../types';
import { PaneAsksChip } from './PaneAsksChip';

afterEach(() => vi.restoreAllMocks());
const ask = (id: string, text: string, source = '@lead') => ({ id, text, source, createdAt: Date.now() });

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
  // Only the badge itself is a button until it is opened.
  expect(screen.getAllByRole('button')).toHaveLength(1);
});

it('opens every ask in full from the badge and dismisses just the chosen one', async () => {
  const dismiss = vi.spyOn(api, 'dismissAsk').mockResolvedValue({ ok: true });
  const refresh = vi.fn().mockResolvedValue(undefined);
  useStore.setState({ refreshPanes: refresh });
  const long = 'Should I rotate the production signing key now, or wait for the release freeze to lift on Friday?';
  render(<PaneAsksChip pane={{ id: 'p1', asks: [ask('a1', long, '@lead'), ask('a2', '<i>two</i>', '@ops')] } as PaneInfo} />);
  expect(screen.queryByRole('dialog')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: '2 asks waiting on you' }));
  const card = screen.getByRole('dialog');
  // The full text of each ask, as plain text.
  expect(within(card).getByText(long)).toBeTruthy();
  expect(within(card).getByText('<i>two</i>')).toBeTruthy();
  fireEvent.click(within(card).getByRole('button', { name: 'Dismiss ask from @ops' }));
  await waitFor(() => expect(refresh).toHaveBeenCalled());
  expect(dismiss).toHaveBeenCalledExactlyOnceWith('a2');
});

it('opens the pane from the card, like the bell row', () => {
  const reveal = vi.fn().mockResolvedValue(undefined);
  useStore.setState({ revealPane: reveal });
  render(<PaneAsksChip pane={{ id: 'p1', asks: [ask('a1', 'one')] } as PaneInfo} />);
  fireEvent.click(screen.getByRole('button', { name: 'Waiting on you: one' }));
  fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Open pane' }));
  expect(reveal).toHaveBeenCalledExactlyOnceWith('p1');
});

it('closes the card on Escape', () => {
  render(<PaneAsksChip pane={{ id: 'p1', asks: [ask('a1', 'one')] } as PaneInfo} />);
  fireEvent.click(screen.getByRole('button', { name: 'Waiting on you: one' }));
  fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
  expect(screen.queryByRole('dialog')).toBeNull();
});

it('does not reopen its card for a later ask once the badge has gone', () => {
  const { rerender } = render(<PaneAsksChip pane={{ id: 'p1', asks: [ask('a1', 'one')] } as PaneInfo} />);
  fireEvent.click(screen.getByRole('button', { name: 'Waiting on you: one' }));
  rerender(<PaneAsksChip pane={{ id: 'p1', asks: [] as ReturnType<typeof ask>[] } as PaneInfo} />);
  expect(screen.queryByRole('dialog')).toBeNull();
  // A resize with the badge gone must not touch the vanished anchor.
  expect(() => window.dispatchEvent(new Event('resize'))).not.toThrow();
  rerender(<PaneAsksChip pane={{ id: 'p1', asks: [ask('a2', 'two')] } as PaneInfo} />);
  expect(screen.queryByRole('dialog')).toBeNull();
});

it('renders nothing without asks', () => {
  const { container } = render(<PaneAsksChip pane={{ id: 'p1' } as PaneInfo} />);
  expect(container.textContent).toBe('');
});
