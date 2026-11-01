import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { api } from '../api';
import { useStore } from '../store';
import type { PaneInfo } from '../types';
import { UnsentMessageChip } from './UnsentMessageChip';

const pane = { id: 'p1', unsentMessage: { id: 'receipt', sender: '#5 Claude' } } as PaneInfo;
afterEach(() => vi.restoreAllMocks());

it.each(['send', 'clear'] as const)('binds %s to the displayed pane and receipt', async (action) => {
  const act = vi.spyOn(api, 'actOnUnsentMessage').mockResolvedValue({ outcome: action === 'clear' ? 'cleared' : 'submitted' });
  // refreshPane restarts a native agent; the chip must only reload pane data.
  const refresh = vi.fn().mockResolvedValue(undefined);
  const restart = vi.fn();
  useStore.setState({ refreshPanes: refresh, refreshPane: restart });
  render(<UnsentMessageChip pane={pane} />);
  expect(screen.getByRole('status').textContent).toContain('Unsent message from #5 Claude');
  fireEvent.click(screen.getByRole('button', { name: action === 'send' ? 'Send' : 'Clear' }));
  await waitFor(() => expect(refresh).toHaveBeenCalled());
  expect(restart).not.toHaveBeenCalled();
  expect(act).toHaveBeenCalledExactlyOnceWith('p1', 'receipt', action);
});

it('surfaces a refused clear and never falls back to raw keys', async () => {
  vi.spyOn(api, 'actOnUnsentMessage').mockRejectedValue(new Error('input changed; text preserved'));
  const toast = vi.fn();
  useStore.setState({ toast });
  render(<UnsentMessageChip pane={pane} />);
  fireEvent.click(screen.getByRole('button', { name: 'Clear' }));
  await waitFor(() => expect(toast).toHaveBeenCalledWith(expect.stringContaining('text preserved'), { kind: 'warn' }));
});

it('renders nothing without a receipt', () => {
  const { container } = render(<UnsentMessageChip pane={{ id: 'p1' } as PaneInfo} />);
  expect(container.textContent).toBe('');
});

it('shows the whole text of a message the pane was never given before Send types it', async () => {
  const act = vi.spyOn(api, 'actOnUnsentMessage').mockResolvedValue({ outcome: 'submitted' });
  const asked: { message: string; resolve: (v: unknown) => void }[] = [];
  useStore.setState({
    refreshPanes: vi.fn().mockResolvedValue(undefined),
    openDialog: ((req: { message: string; resolve: (v: unknown) => void }) => { asked.push(req); }) as never,
  });
  const text = 'make test && rm -rf build # the whole line, not a summary';
  render(<UnsentMessageChip pane={{ id: 'p3', unsentMessage: { id: 'r3', sender: '#5 Claude', notTyped: true, text } } as PaneInfo} />);
  fireEvent.click(screen.getByRole('button', { name: 'Send…' }));
  await waitFor(() => expect(asked).toHaveLength(1));
  expect(asked[0].message).toContain(text);
  asked[0].resolve(false); // Cancel types nothing
  await waitFor(() => expect(screen.getByRole('button', { name: 'Send…' })).not.toBeDisabled());
  expect(act).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole('button', { name: 'Send…' }));
  await waitFor(() => expect(asked).toHaveLength(2));
  asked[1].resolve(true);
  await waitFor(() => expect(act).toHaveBeenCalledExactlyOnceWith('p3', 'r3', 'send'));
});

it('offers Send or Discard for a message a busy pane was never given', async () => {
  const act = vi.spyOn(api, 'actOnUnsentMessage').mockResolvedValue({ outcome: 'cleared' });
  useStore.setState({ refreshPanes: vi.fn().mockResolvedValue(undefined) });
  render(<UnsentMessageChip pane={{ id: 'p2', unsentMessage: { id: 'r2', sender: '#5 Claude', notTyped: true } } as PaneInfo} />);
  expect(screen.getByRole('status').textContent).toContain('Message from #5 Claude not sent (the pane was busy)');
  expect(screen.queryByRole('button', { name: 'Clear' })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
  await waitFor(() => expect(act).toHaveBeenCalledExactlyOnceWith('p2', 'r2', 'clear'));
});
