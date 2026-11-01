import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { CommandPalette } from './CommandPalette';
import { useEntitlements } from '../entitlements';
import { setExtensionsForTests } from '../extensions';
import { useStore } from '../store';
import type { PaneInfo } from '../types';

vi.mock('../api', () => ({
  onRoutingScopeChange: vi.fn(() => () => {}),
  currentRoutingScope: vi.fn(() => ({ prefix: '', backendPrefix: '', key: '|' })),
  api: { health: vi.fn().mockResolvedValue({ version: '1.2.3' }) },
}));

Element.prototype.scrollIntoView = vi.fn();
const initial = useStore.getState();
const pane = (id: string, title: string, handle?: string) =>
  ({ id, title, cmd: ['claude'], status: 'running', state: 'idle', lastActivityMs: 0, ...(handle ? { identity: { handle } } : {}) } as PaneInfo);

describe('palette pane entries', () => {
  beforeEach(() => {
    useEntitlements.setState({ loaded: true, team: false, operator: false });
    setExtensionsForTests([]);
    useStore.setState({
      panes: { a: pane('a', 'Shell'), b: pane('b', '@cmo · launch plan', 'cmo'), c: pane('c', 'Security review', 'secure') },
      layout: { type: 'split', direction: 'horizontal', ratio: 0.5, a: { type: 'pane', paneId: 'a' },
        b: { type: 'split', direction: 'horizontal', ratio: 0.5, a: { type: 'pane', paneId: 'b' }, b: { type: 'pane', paneId: 'c' } } },
      focusedPaneId: 'a',
    } as never);
  });
  afterEach(() => { setExtensionsForTests(null); vi.restoreAllMocks(); useStore.setState(initial); });

  it('names panes by handle and finds them by it', () => {
    render(<CommandPalette open onClose={vi.fn()} onOpenHelp={vi.fn()} onOpenNewPane={vi.fn()} onOpenBroadcast={vi.fn()} onOpenActivity={vi.fn()} onOpenFeedback={vi.fn()} />);
    const input = document.querySelector<HTMLInputElement>('.quickopen-input')!;
    fireEvent.change(input, { target: { value: 'secure' } });
    expect(screen.getAllByRole('option').map(o => o.textContent)).toContainEqual(expect.stringContaining('Focus pane: @secure · Security review'));
    fireEvent.change(input, { target: { value: 'cmo' } });
    const texts = screen.getAllByRole('option').map(o => o.textContent ?? '');
    expect(texts.some(t => t.includes('Focus pane: @cmo · launch plan'))).toBe(true);
    expect(texts.some(t => t.includes('@cmo · @cmo'))).toBe(false);
  });
});
