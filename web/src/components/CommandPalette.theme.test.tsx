import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { __recomputeEmbedForTests, installEmbedBridge } from '../embed';
import { useStore } from '../store';
import { CommandPalette } from './CommandPalette';

vi.mock('../api', () => ({
  onRoutingScopeChange: vi.fn(() => () => {}),
  currentRoutingScope: vi.fn(() => ({ prefix: '', backendPrefix: '', key: '|' })),
  api: { health: vi.fn().mockResolvedValue({ version: '1.2.3' }) },
}));
Element.prototype.scrollIntoView = vi.fn();

function setEmbed(framed: boolean, theme = '') {
  __recomputeEmbedForTests({
    framed, search: `?embed=shinoque${theme ? `&theme=${theme}` : ''}`,
    saved: null, ancestorOrigin: 'http://localhost:7777', referrer: '',
  });
}

function initHost() {
  window.dispatchEvent(new MessageEvent('message', {
    source: window.parent, origin: 'http://localhost:7777',
    data: { source: 'shinoque', v: 1, kind: 'init', theme: 'dark' },
  }));
}

function renderPalette(initialQuery = 'Switch to light theme', onClose = vi.fn()) {
  render(<CommandPalette open initialQuery={initialQuery} onClose={onClose}
    onOpenHelp={vi.fn()} onOpenNewPane={vi.fn()} onOpenBroadcast={vi.fn()}
    onOpenActivity={vi.fn()} onOpenFeedback={vi.fn()} />);
  return document.querySelector<HTMLInputElement>('.quickopen-input')!;
}

describe('CommandPalette app theme', () => {
  const initial = useStore.getState();
  beforeEach(() => {
    setEmbed(false);
    useStore.setState({ appTheme: 'system', appThemeLight: false });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    setEmbed(false);
    useStore.setState({ appTheme: initial.appTheme, appThemeLight: initial.appThemeLight,
      realignAllPanes: initial.realignAllPanes });
  });

  it.each(['dark', 'light'])('hides the toggle under a %s host theme and Enter cannot change the preference', (theme) => {
    setEmbed(true, theme);
    const input = renderPalette();
    expect(screen.queryByText(/Switch to (dark|light) theme/)).toBeNull();
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(useStore.getState().appTheme).toBe('system');
  });

  it.each([false, true])('keeps the toggle working without a host theme (embedded: %s)', async (embedded) => {
    setEmbed(embedded);
    renderPalette();
    fireEvent.mouseDown(screen.getByText('Switch to light theme'));
    await waitFor(() => expect(useStore.getState().appTheme).toBe('light'));
  });

  it('removes a visible toggle immediately when the host initializes', () => {
    setEmbed(true);
    const dispose = installEmbedBridge(() => {});
    try {
      const input = renderPalette();
      expect(screen.getByText('Switch to light theme')).toBeVisible();
      act(initHost);
      expect(screen.queryByText(/Switch to (dark|light) theme/)).toBeNull();
      fireEvent.keyDown(input, { key: 'Enter' });
      expect(useStore.getState().appTheme).toBe('system');
    } finally {
      dispose();
    }
  });

  it('does not run a selected theme toggle if host initialization arrives before its animation frame', () => {
    setEmbed(true);
    const dispose = installEmbedBridge(() => {});
    try {
      renderPalette();
      let deferred: FrameRequestCallback | undefined;
      vi.spyOn(window, 'requestAnimationFrame').mockImplementation((callback) => {
        deferred = callback;
        return 1;
      });
      fireEvent.mouseDown(screen.getByText('Switch to light theme'));
      expect(deferred).toBeDefined();
      const runSelected = deferred!;
      act(() => {
        initHost();
        runSelected(0);
      });
      expect(useStore.getState().appTheme).toBe('system');
    } finally {
      dispose();
    }
  });

  it('clears a removed highlighted row without letting Enter run the command at its old index', () => {
    setEmbed(true);
    const dispose = installEmbedBridge(() => {});
    const onClose = vi.fn();
    const realign = vi.fn();
    useStore.setState({ realignAllPanes: realign });
    try {
      const input = renderPalette('', onClose);
      const options = screen.getAllByRole('option');
      const themeIndex = options.findIndex((option) => option.textContent?.includes('Switch to light theme'));
      expect(themeIndex).toBeGreaterThan(0);
      expect(options[themeIndex + 1]).toHaveTextContent('Refresh view');
      for (let i = 0; i < themeIndex; i++) fireEvent.keyDown(input, { key: 'ArrowDown' });
      expect(options[themeIndex]).toHaveAttribute('aria-selected', 'true');

      act(initHost);
      expect(screen.getAllByRole('option')[themeIndex]).toHaveTextContent('Refresh view');
      expect(screen.queryByRole('option', { selected: true })).toBeNull();
      fireEvent.keyDown(input, { key: 'Enter' });
      expect(onClose).not.toHaveBeenCalled();
      expect(realign).not.toHaveBeenCalled();
      expect(useStore.getState().appTheme).toBe('system');

      // Choosing again after the neutral state starts with the first item.
      fireEvent.keyDown(input, { key: 'ArrowDown' });
      expect(screen.getAllByRole('option')[0]).toHaveAttribute('aria-selected', 'true');
    } finally {
      dispose();
    }
  });

  it('keeps a surviving command highlighted by id when a preceding row disappears', async () => {
    setEmbed(true);
    const dispose = installEmbedBridge(() => {});
    const realign = vi.fn();
    useStore.setState({ realignAllPanes: realign });
    try {
      const input = renderPalette('');
      const options = screen.getAllByRole('option');
      const refreshIndex = options.findIndex((option) => option.textContent?.includes('Refresh view'));
      for (let i = 0; i < refreshIndex; i++) fireEvent.keyDown(input, { key: 'ArrowDown' });
      expect(screen.getByRole('option', { selected: true })).toHaveTextContent('Refresh view');
      act(initHost);
      expect(screen.getAllByRole('option')[refreshIndex - 1]).toHaveTextContent('Refresh view');
      expect(screen.getByRole('option', { selected: true })).toHaveTextContent('Refresh view');
      fireEvent.keyDown(input, { key: 'Enter' });
      await waitFor(() => expect(realign).toHaveBeenCalledOnce());
    } finally {
      dispose();
    }
  });
});
