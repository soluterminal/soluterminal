import { fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import { MobileApprovalBar, MOBILE_APPROVAL_DEBOUNCE_MS } from './MobileApprovalBar';

const always = () => () => true;

describe('MobileApprovalBar', () => {
  afterEach(() => {
    useStore.setState({ senders: {}, paneSnapshots: {} });
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('renders nothing until the session has an input sender', () => {
    const { container } = render(<MobileApprovalBar sessionKey="k" capture={always} />);
    expect(container).toBeEmptyDOMElement();
    expect(document.body.classList.contains('mobile-approval-visible')).toBe(false);
  });

  it('labels keys by key and shared meaning, never claiming what option 1 is', () => {
    useStore.setState({ senders: { k: vi.fn() } });
    render(<MobileApprovalBar sessionKey="k" capture={always} />);
    const group = screen.getByRole('group', { name: 'Answer terminal prompt' });
    const buttons = within(group).getAllByRole('button');
    expect(buttons.map((b) => b.textContent)).toEqual(['1', '2', '3', '⏎ Enter', 'Esc · Cancel']);
    expect(buttons.map((b) => b.getAttribute('aria-label'))).toEqual([
      'Press 1 — menu option 1',
      'Press 2 — menu option 2',
      'Press 3 — menu option 3',
      'Press Enter — confirm the highlighted option',
      'Press Esc — cancel',
    ]);
    expect(group.textContent).not.toMatch(/approve|allow|yes/i);
  });

  it('hides the desktop strip while shown', () => {
    useStore.setState({ senders: { k: vi.fn() } });
    const { unmount } = render(<MobileApprovalBar sessionKey="k" capture={always} />);
    expect(document.body.classList.contains('mobile-approval-visible')).toBe(true);
    unmount();
    expect(document.body.classList.contains('mobile-approval-visible')).toBe(false);
  });

  it('answers with one tap: the bare digit, no Enter after it', () => {
    vi.useFakeTimers();
    const sender = vi.fn(() => true);
    useStore.setState({ senders: { k: sender }, paneSnapshots: { k: () => ' Do you want to proceed?\n ❯ 1. Yes\n   2. No' } });
    render(<MobileApprovalBar sessionKey="k" capture={always} />);
    fireEvent.click(screen.getByRole('button', { name: 'Press 1 — menu option 1' }));
    vi.advanceTimersByTime(2000);
    expect(sender.mock.calls).toEqual([['1']]);
  });

  it('sends the exact bytes for Enter and Esc', () => {
    vi.useFakeTimers();
    const sender = vi.fn(() => true);
    useStore.setState({ senders: { k: sender } });
    render(<MobileApprovalBar sessionKey="k" capture={always} />);
    fireEvent.click(screen.getByRole('button', { name: 'Press Enter — confirm the highlighted option' }));
    vi.advanceTimersByTime(MOBILE_APPROVAL_DEBOUNCE_MS);
    fireEvent.click(screen.getByRole('button', { name: 'Press Esc — cancel' }));
    expect(sender.mock.calls).toEqual([['\r'], ['\x1b']]);
  });

  it('drops a bounce tap that would answer the next screen', () => {
    vi.useFakeTimers();
    const sender = vi.fn(() => true);
    useStore.setState({ senders: { k: sender } });
    render(<MobileApprovalBar sessionKey="k" capture={always} />);
    const one = screen.getByRole('button', { name: 'Press 1 — menu option 1' });
    fireEvent.click(one);
    fireEvent.click(one);
    expect(sender).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(MOBILE_APPROVAL_DEBOUNCE_MS);
    fireEvent.click(one);
    expect(sender).toHaveBeenCalledTimes(2);
  });

  it('prevents mousedown default so a tap does not move focus', () => {
    useStore.setState({ senders: { k: vi.fn() } });
    render(<MobileApprovalBar sessionKey="k" capture={always} />);
    expect(fireEvent.mouseDown(screen.getByRole('button', { name: 'Press 2 — menu option 2' }))).toBe(false);
  });

  it('uses only its session sender and refuses taps after the view changes', () => {
    let active = true;
    const scoped = vi.fn(() => true), other = vi.fn(() => true);
    useStore.setState({ senders: { p1: other, 'wsl-session': scoped } });
    render(<MobileApprovalBar sessionKey="wsl-session" capture={() => () => active} />);
    fireEvent.click(screen.getByRole('button', { name: 'Press 2 — menu option 2' }));
    expect(scoped).toHaveBeenCalledWith('2');
    expect(other).not.toHaveBeenCalled();
    active = false;
    vi.spyOn(Date, 'now').mockReturnValue(Date.now() + 10_000);
    fireEvent.click(screen.getByRole('button', { name: 'Press 3 — menu option 3' }));
    expect(scoped).toHaveBeenCalledTimes(1);
  });
});
