import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { CommandPalette } from './CommandPalette';
import { useEntitlements } from '../entitlements';
import { setExtensionsForTests } from '../extensions';

vi.mock('../api', () => ({
  onRoutingScopeChange: vi.fn(() => () => {}),
  currentRoutingScope: vi.fn(() => ({ prefix: '', backendPrefix: '', key: '|' })),
  api: { health: vi.fn().mockResolvedValue({ version: '1.2.3' }) },
}));

Element.prototype.scrollIntoView = vi.fn();

describe('CommandPalette: Send feedback', () => {
  beforeEach(() => {
    useEntitlements.setState({ loaded: true, team: false, operator: false });
    setExtensionsForTests([]);
  });

  afterEach(() => {
    setExtensionsForTests(null);
    vi.restoreAllMocks();
  });

  it.each(['bug', 'report', 'problem', 'issue', 'idea', 'feedback', 'Repor'])(
    'ranks Send feedback first for "%s"',
    async (query) => {
      const onOpenFeedback = vi.fn();
      render(
        <CommandPalette
          open
          onClose={vi.fn()}
          onOpenHelp={vi.fn()}
          onOpenNewPane={vi.fn()}
          onOpenBroadcast={vi.fn()}
          onOpenActivity={vi.fn()}
          onOpenFeedback={onOpenFeedback}
        />,
      );
      const input = document.querySelector<HTMLInputElement>('.quickopen-input')!;
      fireEvent.change(input, { target: { value: query } });
      expect(screen.getAllByRole('option')[0].textContent).toContain('Send feedback');
      // The palette runs an action one frame after it closes.
      fireEvent.keyDown(input, { key: 'Enter' });
      await waitFor(() => expect(onOpenFeedback).toHaveBeenCalledTimes(1));
    },
  );
});
