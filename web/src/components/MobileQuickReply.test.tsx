import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { terminalSessionKey, useStore } from '../store';
import { currentRoutingScope } from '../api';
import { MobileQuickReply } from './MobileQuickReply';
import type { PaneInfo } from '../types';

const PANE = {
  id: 'p1', title: 'Claude', cwd: '/repo', cmd: ['claude'], status: 'running',
  createdAt: '2026-10-28T00:00:00Z', cols: 80, rows: 24, pid: 1, hasClaude: true,
} as unknown as PaneInfo;

function mount(state: PaneInfo['state']) {
  const sender = vi.fn(() => true);
  const key = terminalSessionKey(currentRoutingScope(), PANE.id);
  useStore.setState({
    panes: { [PANE.id]: { ...PANE, state } },
    focusedPaneId: PANE.id,
    layout: { type: 'pane', paneId: PANE.id },
    mainPresentation: { kind: 'workspace' },
    senders: { [key]: sender },
    paneSnapshots: { [key]: () => ' Do you want to proceed?\n ❯ 1. Yes\n   2. No' },
  });
  render(<MobileQuickReply />);
  return sender;
}

function reply(text: string) {
  const input = screen.getByRole('textbox');
  fireEvent.change(input, { target: { value: text } });
  fireEvent.keyDown(input, { key: 'Enter' });
}

describe('MobileQuickReply at a choice prompt', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => {
    vi.useRealTimers();
    useStore.setState({ panes: {}, focusedPaneId: null, layout: null, senders: {}, paneSnapshots: {} });
  });

  it('shows the answer bar only at choice-prompt', () => {
    mount('choice-prompt');
    expect(screen.getByRole('group', { name: 'Answer terminal prompt' })).toBeInTheDocument();
  });

  it('does not show the answer bar while awaiting free input', () => {
    mount('awaiting-input');
    expect(screen.queryByRole('group', { name: 'Answer terminal prompt' })).toBeNull();
  });

  it('sends a typed menu digit without the forced Enter', () => {
    const sender = mount('choice-prompt');
    reply('2');
    act(() => { vi.advanceTimersByTime(2000); });
    expect(sender.mock.calls).toEqual([['2']]);
  });

  it('still submits free text at a choice prompt with Enter', () => {
    const sender = mount('choice-prompt');
    reply('use the staging bucket');
    act(() => { vi.advanceTimersByTime(2000); });
    expect(sender.mock.calls).toEqual([['use the staging bucket'], ['\r']]);
  });

  it('keeps the Enter for a digit when the pane is not at a menu', () => {
    const sender = mount('awaiting-input');
    reply('2');
    act(() => { vi.advanceTimersByTime(2000); });
    expect(sender.mock.calls).toEqual([['2'], ['\r']]);
  });
});
