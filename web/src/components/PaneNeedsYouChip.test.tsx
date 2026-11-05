import { fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import type { PaneInfo } from '../types';
import { PaneNeedsYouChip, promptLines } from './PaneNeedsYouChip';

afterEach(() => {
  vi.restoreAllMocks();
  useStore.setState({ paneTails: {} });
});

const pane = (over: Partial<PaneInfo> = {}) =>
  ({ id: 'p1', status: 'running', state: 'choice-prompt', attention: 'needs-user', ...over }) as PaneInfo;

it('renders nothing unless the daemon says a human is needed', () => {
  const { container } = render(<PaneNeedsYouChip pane={pane({ attention: 'none' })} />);
  expect(container.textContent).toBe('');
});

it('says the prompt was detected, not posted, and shows its lines', () => {
  useStore.setState({
    paneTails: {
      p1: {
        paneId: 'p1', paneTitle: 'x', toState: 'choice-prompt', ts: Date.now(),
        text: '\n\nDo you want to run `rm -rf build`?\n❯ 1. Yes\n  2. No\n\n',
      },
    },
  });
  render(<PaneNeedsYouChip pane={pane({ attentionReason: 'touches files outside the repo' })} />);
  fireEvent.click(screen.getByRole('button', { name: 'needs you' }));
  const card = screen.getByRole('dialog');
  expect(within(card).getByText('A choice prompt in the terminal')).toBeTruthy();
  expect(within(card).getByText(/no agent posted an ask for it/)).toBeTruthy();
  expect(within(card).getByText('touches files outside the repo')).toBeTruthy();
  const lines = within(card).getByLabelText('Prompt lines');
  expect(lines.textContent).toBe('Do you want to run `rm -rf build`?\n❯ 1. Yes\n  2. No');
});

it('says when the lines are not captured yet, and can open the pane', () => {
  const reveal = vi.fn().mockResolvedValue(undefined);
  useStore.setState({ revealPane: reveal });
  render(<PaneNeedsYouChip pane={pane({ state: 'awaiting-input' })} />);
  fireEvent.click(screen.getByRole('button', { name: 'needs you' }));
  const card = screen.getByRole('dialog');
  expect(within(card).getByText('The terminal is waiting for your input')).toBeTruthy();
  expect(within(card).getByText(/not captured yet/)).toBeTruthy();
  fireEvent.click(within(card).getByRole('button', { name: 'Open pane' }));
  expect(reveal).toHaveBeenCalledExactlyOnceWith('p1');
});

it('keeps only the last lines, blank edges trimmed', () => {
  const text = Array.from({ length: 30 }, (_, i) => `line ${i + 1}`).join('\n') + '\n\n';
  const lines = promptLines(text);
  expect(lines).toHaveLength(14);
  expect(lines[13]).toBe('line 30');
});

it('does not reopen its card for the next prompt once attention has cleared', () => {
  const { rerender } = render(<PaneNeedsYouChip pane={pane()} />);
  fireEvent.click(screen.getByRole('button', { name: 'needs you' }));
  rerender(<PaneNeedsYouChip pane={pane({ attention: 'none' })} />);
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(() => window.dispatchEvent(new Event('resize'))).not.toThrow();
  rerender(<PaneNeedsYouChip pane={pane()} />);
  expect(screen.queryByRole('dialog')).toBeNull();
});
