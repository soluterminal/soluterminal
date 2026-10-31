import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PaneAutoMode, PaneInfo } from '../types';
import { useStore } from '../store';
import { PaneAutoModeChip } from './PaneAutoModeChip';
import { PaneHeaderBadges } from './PaneHeaderBadges';

const pane: PaneInfo = {
  id: 'auto-pane', cwd: '/project', cmd: ['claude'], title: 'Build',
  status: 'running', cols: 80, rows: 24, pid: 0,
  lastActivityMs: 0, hasClaude: true, hasCodex: false, autoMode: '',
  state: 'awaiting-input', createdAt: '2026-10-31T00:00:00Z',
};

type DialogArg = { title?: string; message: string; confirmLabel?: string; resolve: (v: unknown) => void };
const original = useStore.getState();
let setPaneAutoMode: ReturnType<typeof vi.fn>;
let openDialog: ReturnType<typeof vi.fn>;
let toast: ReturnType<typeof vi.fn>;

beforeEach(() => {
  setPaneAutoMode = vi.fn().mockResolvedValue(null);
  openDialog = vi.fn();
  toast = vi.fn();
  useStore.setState({ setPaneAutoMode, openDialog, toast, panes: { [pane.id]: pane } } as never);
});
afterEach(() => {
  cleanup();
  useStore.setState({
    setPaneAutoMode: original.setPaneAutoMode, openDialog: original.openDialog, toast: original.toast,
    panes: original.panes, navigationGeneration: original.navigationGeneration,
  } as never);
});

function chip(mode: PaneAutoMode = '') {
  render(<PaneAutoModeChip pane={{ ...pane, autoMode: mode }} />);
  return screen.getByRole('button', { name: /^Auto-approve:/ });
}

const flush = () => act(async () => { await Promise.resolve(); await Promise.resolve(); });

describe('auto-approve chip labels', () => {
  it.each([
    ['', 'manual', 'pane-automode-off'],
    ['cautious', 'auto?', 'pane-automode-cautious'],
    ['always', 'AUTO', 'pane-automode-always'],
  ] as const)('mode %j reads %j with its own look', (mode, label, cls) => {
    const btn = chip(mode);
    expect(btn).toHaveTextContent(new RegExp(`^${label.replace('?', '\\?')}$`));
    expect(btn).toHaveClass(cls);
  });

  it('never reads "auto" while off', () => {
    expect(chip('').textContent?.toLowerCase()).not.toContain('auto');
  });
});

describe('auto-approve chip menu', () => {
  it('opens a menu with the three modes and changes nothing by itself', () => {
    fireEvent.click(chip(''));
    const menu = screen.getByRole('menu', { name: 'Auto-approve' });
    expect(menu).toBeInTheDocument();
    expect(screen.getAllByRole('menuitemradio').map((b) => b.textContent)).toEqual([
      expect.stringContaining('Off (manual)'),
      expect.stringContaining('Approve with caution'),
      expect.stringContaining('Always approve'),
    ]);
    expect(screen.getByRole('menuitemradio', { name: /Off \(manual\)/ })).toHaveAttribute('aria-checked', 'true');
    expect(setPaneAutoMode).not.toHaveBeenCalled();
  });

  it('a double-click on the chip never changes the mode', () => {
    const btn = chip('');
    fireEvent.click(btn, { detail: 1 });
    fireEvent.click(btn, { detail: 2 });
    fireEvent.doubleClick(btn);
    expect(setPaneAutoMode).not.toHaveBeenCalled();
    expect(openDialog).not.toHaveBeenCalled();
    // The second click was ignored, so the menu the first one opened stays.
    expect(screen.getByRole('menu', { name: 'Auto-approve' })).toBeInTheDocument();
  });

  it('ignores the second click of a double-click on a menu item', () => {
    fireEvent.click(chip(''));
    fireEvent.click(screen.getByRole('menuitemradio', { name: /Approve with caution/ }), { detail: 2 });
    expect(setPaneAutoMode).not.toHaveBeenCalled();
  });

  it('applies Approve with caution and Off at once, with no confirmation', async () => {
    fireEvent.click(chip(''));
    fireEvent.click(screen.getByRole('menuitemradio', { name: /Approve with caution/ }));
    await flush();
    expect(openDialog).not.toHaveBeenCalled();
    expect(setPaneAutoMode).toHaveBeenCalledWith('auto-pane', 'cautious');
    expect(screen.queryByRole('menu')).toBeNull();

    cleanup();
    setPaneAutoMode.mockClear();
    fireEvent.click(chip('always'));
    fireEvent.click(screen.getByRole('menuitemradio', { name: /Off \(manual\)/ }));
    await flush();
    expect(openDialog).not.toHaveBeenCalled();
    expect(setPaneAutoMode).toHaveBeenCalledWith('auto-pane', '');
  });

  it('asks before Always approve, and does nothing when cancelled', async () => {
    fireEvent.click(chip('cautious'));
    fireEvent.click(screen.getByRole('menuitemradio', { name: /Always approve/ }));
    await flush();
    expect(openDialog).toHaveBeenCalledTimes(1);
    const arg = openDialog.mock.calls[0][0] as DialogArg;
    expect(arg.title).toBe('Turn on Always approve?');
    expect(arg.message).toMatch(/approved automatically, with no cancel window/);
    expect(arg.message).toContain('“Build”');
    expect(arg.confirmLabel).toBe('Always approve');
    expect(setPaneAutoMode).not.toHaveBeenCalled();
    await act(async () => { arg.resolve(false); });
    await flush();
    expect(setPaneAutoMode).not.toHaveBeenCalled();
  });

  it('turns on Always approve only after the confirmation', async () => {
    fireEvent.click(chip(''));
    fireEvent.click(screen.getByRole('menuitemradio', { name: /Always approve/ }));
    await flush();
    const arg = openDialog.mock.calls[0][0] as DialogArg;
    await act(async () => { arg.resolve(true); });
    await flush();
    expect(setPaneAutoMode).toHaveBeenCalledWith('auto-pane', 'always');
  });

  it('does not turn on Always approve if the view changed while confirming', async () => {
    fireEvent.click(chip(''));
    fireEvent.click(screen.getByRole('menuitemradio', { name: /Always approve/ }));
    await flush();
    const arg = openDialog.mock.calls[0][0] as DialogArg;
    useStore.setState({ navigationGeneration: useStore.getState().navigationGeneration + 1 } as never);
    await act(async () => { arg.resolve(true); });
    await flush();
    expect(setPaneAutoMode).not.toHaveBeenCalled();
    expect(toast).toHaveBeenCalledWith(expect.stringMatching(/Always approve was not turned on/), expect.anything());
  });

  it('does not turn on Always approve for a different pane that took the same id', async () => {
    fireEvent.click(chip(''));
    fireEvent.click(screen.getByRole('menuitemradio', { name: /Always approve/ }));
    await flush();
    const arg = openDialog.mock.calls[0][0] as DialogArg;
    useStore.setState({ panes: { [pane.id]: { ...pane, createdAt: '2026-10-31T01:00:00Z' } } } as never);
    await act(async () => { arg.resolve(true); });
    await flush();
    expect(setPaneAutoMode).not.toHaveBeenCalled();
  });

  it('closes on Escape and on an outside click without changing the mode', () => {
    fireEvent.click(chip(''));
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('menu')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: /^Auto-approve:/ }));
    fireEvent.mouseDown(document.body);
    expect(screen.queryByRole('menu')).toBeNull();
    expect(setPaneAutoMode).not.toHaveBeenCalled();
  });

  it('keeps clicks inside the menu from reaching the header (drag, rename, focus)', () => {
    const onHeaderClick = vi.fn();
    const onHeaderDouble = vi.fn();
    const onHeaderDown = vi.fn();
    render(
      <div onClick={onHeaderClick} onDoubleClick={onHeaderDouble} onMouseDown={onHeaderDown}>
        <PaneHeaderBadges pane={pane} paneIndex={1} />
      </div>,
    );
    const btn = screen.getByRole('button', { name: /^Auto-approve:/ });
    fireEvent.mouseDown(btn);
    fireEvent.click(btn);
    fireEvent.doubleClick(btn);
    const item = screen.getByRole('menuitemradio', { name: /Approve with caution/ });
    fireEvent.mouseDown(item);
    fireEvent.doubleClick(item);
    expect(onHeaderClick).not.toHaveBeenCalled();
    expect(onHeaderDouble).not.toHaveBeenCalled();
    expect(onHeaderDown).not.toHaveBeenCalled();
  });
});
