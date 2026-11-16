import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { useStore } from '../store';
import type { PaneInfo } from '../types';
import { RetiredFocusPane } from './RetiredFocusPane';

const focusPane = {
  id: 'p1', cwd: '/work', cmd: ['claude'], title: 'Claude', kind: 'native', status: 'running', state: 'busy',
  pid: 11, agentMidTurn: true, pendingApproval: { requestId: 'r1', toolName: 'Bash' },
} as PaneInfo;

describe('RetiredFocusPane', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    useStore.setState({ panes: {} });
  });

  it('switches a still-native pane to a terminal now, even while it waits on an approval', async () => {
    const terminal = { ...focusPane, kind: '', pid: 22, pendingApproval: undefined } as PaneInfo;
    const switchMode = vi.spyOn(api, 'switchPaneMode').mockResolvedValue(terminal);
    useStore.setState({ panes: { p1: focusPane } });
    render(<RetiredFocusPane pane={focusPane} />);

    expect(screen.getByText('This pane uses Focus, which has been removed')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Open as a terminal' }));

    await waitFor(() => expect(useStore.getState().panes.p1.kind).toBe(''));
    expect(switchMode).toHaveBeenCalledWith('p1', '', { when: 'now' }, expect.anything());
    expect(useStore.getState().panes.p1.pid).toBe(22);
  });

  it('keeps the pane and says why when the switch fails', async () => {
    vi.spyOn(api, 'switchPaneMode').mockRejectedValue(new Error('pane is busy'));
    const toast = vi.fn();
    useStore.setState({ panes: { p1: focusPane }, toast });
    render(<RetiredFocusPane pane={focusPane} />);

    fireEvent.click(screen.getByRole('button', { name: 'Open as a terminal' }));

    await waitFor(() => expect(toast).toHaveBeenCalledWith(expect.stringContaining('pane is busy'), expect.anything()));
    expect(useStore.getState().panes.p1.kind).toBe('native');
    expect(screen.getByRole('button', { name: 'Open as a terminal' })).toBeEnabled();
  });
});
