import { fireEvent, render, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { readDisplayFixTelemetry, resetDisplayFixTelemetry } from '../lib/displayFixTelemetry';
import { useStore } from '../store';
import { CommandPalette } from './CommandPalette';

vi.mock('../api', () => ({
  onRoutingScopeChange: vi.fn(() => () => {}),
  currentRoutingScope: vi.fn(() => ({ prefix: '', backendPrefix: '', key: '|' })),
  api: { health: vi.fn().mockResolvedValue({ version: '1.2.3' }) },
}));
Element.prototype.scrollIntoView = vi.fn();

const initial = useStore.getState();
beforeEach(() => {
  localStorage.clear();
  resetDisplayFixTelemetry();
});
afterEach(() => {
  useStore.setState({ realignAllPanes: initial.realignAllPanes });
  resetDisplayFixTelemetry();
});

it('Refresh view from the palette realigns and counts a palette press', async () => {
  const realign = vi.fn();
  useStore.setState({ realignAllPanes: realign });
  render(<CommandPalette open initialQuery="Refresh view" onClose={vi.fn()}
    onOpenHelp={vi.fn()} onOpenNewPane={vi.fn()} onOpenBroadcast={vi.fn()}
    onOpenActivity={vi.fn()} onOpenFeedback={vi.fn()} />);
  fireEvent.keyDown(document.querySelector<HTMLInputElement>('.quickopen-input')!, { key: 'Enter' });
  await waitFor(() => expect(realign).toHaveBeenCalledOnce());
  expect(readDisplayFixTelemetry()).toMatchObject({ palette: 1, altR: 0, fixDisplay: 0 });
});
