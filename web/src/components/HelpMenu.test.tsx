import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { HelpMenu } from './HelpMenu';
import { useStore } from '../store';

const openInSystemBrowser = vi.hoisted(() => vi.fn().mockResolvedValue(true));
vi.mock('../tauri-browser', () => ({ openInSystemBrowser }));

function renderMenu() {
  const onOpenGettingStarted = vi.fn();
  const onOpenShortcuts = vi.fn();
  render(<HelpMenu onOpenGettingStarted={onOpenGettingStarted} onOpenShortcuts={onOpenShortcuts} />);
  const trigger = screen.getByRole('button', { name: 'Help' });
  return { trigger, onOpenGettingStarted, onOpenShortcuts };
}

function openMenu(trigger: HTMLElement) {
  fireEvent.click(trigger);
  return screen.getByRole('menu', { name: 'Help' });
}

describe('HelpMenu', () => {
  afterEach(() => {
    useStore.setState({ supportBundleOpen: false });
    vi.clearAllMocks();
  });

  it('is a "Help" menu button that lists the help items in order', () => {
    const { trigger } = renderMenu();
    expect(trigger).toHaveAttribute('title', 'Help');
    expect(trigger).toHaveAttribute('aria-haspopup', 'menu');
    expect(trigger).toHaveAttribute('aria-expanded', 'false');

    openMenu(trigger);
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    const labels = screen.getAllByRole('menuitem').map((b) => b.textContent);
    expect(labels).toEqual([
      'Getting started',
      'Help ↗',
      'Keyboard shortcuts?',
      'Send feedback',
      'Create support bundle…',
    ]);
  });

  it('moves focus with the arrow keys and closes on Escape back to the button', () => {
    const { trigger } = renderMenu();
    const menu = openMenu(trigger);
    const items = screen.getAllByRole('menuitem');
    expect(items[0]).toHaveFocus();

    fireEvent.keyDown(menu, { key: 'ArrowDown' });
    expect(items[1]).toHaveFocus();
    fireEvent.keyDown(menu, { key: 'ArrowUp' });
    fireEvent.keyDown(menu, { key: 'ArrowUp' });
    expect(items[items.length - 1]).toHaveFocus();
    fireEvent.keyDown(menu, { key: 'Home' });
    expect(items[0]).toHaveFocus();
    fireEvent.keyDown(menu, { key: 'End' });
    expect(items[items.length - 1]).toHaveFocus();

    fireEvent.keyDown(menu, { key: 'Escape' });
    expect(screen.queryByRole('menu')).toBeNull();
    expect(trigger).toHaveFocus();
  });

  it('runs each item and closes the menu', () => {
    const { trigger, onOpenGettingStarted, onOpenShortcuts } = renderMenu();

    openMenu(trigger);
    fireEvent.click(screen.getByRole('menuitem', { name: 'Getting started' }));
    expect(onOpenGettingStarted).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('menu')).toBeNull();

    openMenu(trigger);
    fireEvent.click(screen.getByRole('menuitem', { name: /Keyboard shortcuts/ }));
    expect(onOpenShortcuts).toHaveBeenCalledTimes(1);

    openMenu(trigger);
    fireEvent.click(screen.getByRole('menuitem', { name: /^Help/ }));
    expect(openInSystemBrowser).toHaveBeenCalledWith('https://soluterminal.com/help/', expect.any(Event));

    const inbox = vi.fn();
    window.addEventListener('solu:open-inbox', inbox);
    openMenu(trigger);
    act(() => { fireEvent.click(screen.getByRole('menuitem', { name: 'Send feedback' })); });
    window.removeEventListener('solu:open-inbox', inbox);
    expect(inbox).toHaveBeenCalledTimes(1);
    expect((inbox.mock.calls[0][0] as CustomEvent).detail).toEqual({ view: 'compose' });

    openMenu(trigger);
    fireEvent.click(screen.getByRole('menuitem', { name: 'Create support bundle…' }));
    expect(useStore.getState().supportBundleOpen).toBe(true);
  });
});
