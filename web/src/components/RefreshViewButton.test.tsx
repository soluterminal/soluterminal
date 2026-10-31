import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { currentRoutingScope } from '../api';
import { terminalSessionKey, useStore } from '../store';
import type { PaneInfo } from '../types';
import { RefreshViewButton } from './RefreshViewButton';

// Terminal handlers (realigners, senders) are registered under the
// view-session key, not the bare pane id. Rewrap used to look them up by pane
// id, so it silently did nothing.

describe('RefreshViewButton Rewrap', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('realigns and sends Ctrl+O to the focused pane through its session key', () => {
    vi.useFakeTimers();
    const key = terminalSessionKey(currentRoutingScope(), 'pane-1');
    const realign = vi.fn();
    const send = vi.fn();
    useStore.setState((s) => ({
      focusedPaneId: 'pane-1',
      panes: { ...s.panes, 'pane-1': { id: 'pane-1', hasClaude: true } as PaneInfo },
      realigners: { ...s.realigners, [key]: realign },
      senders: { ...s.senders, [key]: send },
    }));

    render(<RefreshViewButton />);
    fireEvent.click(screen.getByRole('button', { name: 'More view fixes' }));
    fireEvent.click(screen.getByRole('button', { name: 'Rewrap' }));

    expect(realign).toHaveBeenCalledTimes(1);
    act(() => { vi.advanceTimersByTime(800); });
    expect(send).toHaveBeenCalledWith('\x0f');
  });
});
