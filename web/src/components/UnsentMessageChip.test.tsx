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

it('reports a partial Clear once and keeps the guarded receipt visible', async () => {
  const act = vi.spyOn(api, 'actOnUnsentMessage').mockResolvedValue({ outcome: 'partly_cleared' });
  const toast = vi.fn();
  useStore.setState({ toast, refreshPanes: vi.fn().mockResolvedValue(undefined) });
  render(<UnsentMessageChip pane={{ ...pane, unsentMessage: { ...pane.unsentMessage!, putBack: true, edited: true } }} />);
  fireEvent.click(screen.getByRole('button', { name: 'Clear' }));
  await waitFor(() => expect(toast).toHaveBeenCalledWith(expect.stringContaining('Partly cleared'), { kind: 'warn' }));
  expect(act).toHaveBeenCalledExactlyOnceWith('p1', 'receipt', 'clear');
  expect(screen.getByRole('status')).toHaveTextContent("Agents can't type");
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

it('shows a held message with its sender and acts on the oldest one', async () => {
  const act = vi.spyOn(api, 'actOnUnsentMessage').mockResolvedValue({ outcome: 'cleared' });
  const refresh = vi.fn().mockResolvedValue(undefined);
  useStore.setState({ refreshPanes: refresh });
  const held = [
    { id: 'h1', sender: '@cmo · #4 Claude', text: 'first', at: 1 },
    { id: 'h2', sender: '@lead · #1 Claude', text: 'second', at: 2 },
  ];
  render(<UnsentMessageChip pane={{ id: 'p5', heldMessages: held } as PaneInfo} />);
  const chip = screen.getByRole('status');
  expect(chip.textContent).toContain('Message from @cmo · #4 Claude waits while you type (+1 more)');
  expect(chip.getAttribute('title')).toBe('first');
  fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
  await waitFor(() => expect(refresh).toHaveBeenCalled());
  expect(act).toHaveBeenCalledExactlyOnceWith('p5', 'h1', 'clear');
});

it('surfaces a refused Send now', async () => {
  vi.spyOn(api, 'actOnUnsentMessage').mockRejectedValue(new Error('your input box has text: send or clear it first, then send this message'));
  const toast = vi.fn();
  useStore.setState({ toast });
  render(<UnsentMessageChip pane={{ id: 'p6', heldMessages: [{ id: 'h1', sender: '@cmo', text: 'x', at: 1 }] } as PaneInfo} />);
  fireEvent.click(screen.getByRole('button', { name: 'Send now' }));
  await waitFor(() => expect(toast).toHaveBeenCalledWith(expect.stringContaining('input box has text'), { kind: 'warn' }));
});

it('says a put-back message is in the input box, and held messages wait behind it', () => {
  render(<UnsentMessageChip pane={{
    id: 'p7',
    unsentMessage: { id: 'r7', sender: '@flag-guard', putBack: true },
    heldMessages: [{ id: 'h1', sender: '@release-lead', text: 'next', at: 1 }],
  } as PaneInfo} />);
  const [receipt, held] = screen.getAllByRole('status');
  expect(receipt.textContent).toContain('Unsent message from @flag-guard in the input box (put back after you stopped it). Agents can\'t type into this pane until it\'s sent or cleared');
  expect(screen.getByRole('button', { name: 'Send' })).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Clear' })).toBeTruthy();
  expect(held.textContent).toContain('Message from @release-lead waits behind the unsent message');
  // Send now could only be refused while the box holds that message.
  expect(screen.queryByRole('button', { name: 'Send now' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Discard' })).toBeTruthy();
});

it('offers Clear or Keep as my draft, never Send, on a put-back message the person edited', async () => {
  const act = vi.spyOn(api, 'actOnUnsentMessage').mockResolvedValue({ outcome: 'kept' });
  const refresh = vi.fn().mockResolvedValue(undefined);
  const toast = vi.fn();
  useStore.setState({ refreshPanes: refresh, toast });
  render(<UnsentMessageChip pane={{ id: 'p8', unsentMessage: { id: 'r8', sender: '@flag-guard', putBack: true, edited: true } } as PaneInfo} />);
  const chip = screen.getByRole('status');
  expect(chip.textContent).toContain("@flag-guard's unsent message is still in the input box after your edit. Agents can't type into this pane until it's gone");
  expect(screen.queryByRole('button', { name: 'Send' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Clear' })).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Keep as my draft' }));
  await waitFor(() => expect(refresh).toHaveBeenCalled());
  expect(act).toHaveBeenCalledExactlyOnceWith('p8', 'r8', 'keep');
  expect(toast).not.toHaveBeenCalled();
});

it('surfaces a refused Clear on a multi-line edited message', async () => {
  vi.spyOn(api, 'actOnUnsentMessage').mockRejectedValue(new Error('Nothing was cleared: the input box has more than one line'));
  const toast = vi.fn();
  useStore.setState({ toast });
  render(<UnsentMessageChip pane={{ id: 'p9', unsentMessage: { id: 'r9', sender: '@flag-guard', putBack: true, edited: true } } as PaneInfo} />);
  fireEvent.click(screen.getByRole('button', { name: 'Clear' }));
  await waitFor(() => expect(toast).toHaveBeenCalledWith(expect.stringContaining('more than one line'), { kind: 'warn' }));
});
