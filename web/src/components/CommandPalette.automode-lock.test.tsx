import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { CommandPalette } from './CommandPalette';
import { useStore } from '../store';
import type { PaneInfo } from '../types';

vi.mock('../api', () => ({
  onRoutingScopeChange: vi.fn(() => () => {}),
  currentRoutingScope: vi.fn(() => ({ prefix: '', backendPrefix: '', key: '|' })),
  api: { health: vi.fn().mockResolvedValue({ version: '1.2.3' }) },
}));
Element.prototype.scrollIntoView = vi.fn();
const original = useStore.getState();
afterEach(() => {
  cleanup();
  useStore.setState({ panes: original.panes, focusedPaneId: original.focusedPaneId, setPaneAutoMode: original.setPaneAutoMode });
});

it('removes palette mode actions immediately when the focused pane locks', () => {
  const pane = { id: 'palette-locked', cmd: ['claude'], cwd: '/project', title: 'Guarded', status: 'running', autoMode: '', neverAutoAnswer: false } as PaneInfo;
  const change = vi.fn();
  useStore.setState({ panes: { [pane.id]: pane }, focusedPaneId: pane.id, setPaneAutoMode: change });
  render(<CommandPalette open onClose={vi.fn()} onOpenHelp={vi.fn()} onOpenNewPane={vi.fn()} onOpenBroadcast={vi.fn()} onOpenActivity={vi.fn()} onOpenFeedback={vi.fn()} />);
  const input = document.querySelector<HTMLInputElement>('.quickopen-input')!;
  fireEvent.change(input, { target: { value: 'Set focused pane auto-approval' } });
  expect(screen.getAllByText(/Set focused pane auto-approval:/).length).toBeGreaterThan(0);
  act(() => useStore.setState({ panes: { [pane.id]: { ...pane, neverAutoAnswer: true } } }));
  expect(screen.queryByText(/Set focused pane auto-approval:/)).toBeNull();
  fireEvent.keyDown(input, { key: 'Enter' });
  expect(change).not.toHaveBeenCalled();
});
