import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import { MobileApprovalBar, MOBILE_APPROVAL_DEBOUNCE_MS, MOBILE_APPROVAL_LABEL_POLL_MS } from './MobileApprovalBar';

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

  it('shows the highlighted option next to Enter, read from the live screen', () => {
    vi.useFakeTimers();
    let live = ' Do you want to proceed?\n ❯ 1. Yes\n   2. No';
    const snapshot = vi.fn((opts?: { screen?: boolean }) => (opts?.screen ? live : 'scrollback ❯ 1. Stale'));
    useStore.setState({ senders: { k: vi.fn() }, paneSnapshots: { k: snapshot } });
    render(<MobileApprovalBar sessionKey="k" capture={always} />);
    const enter = screen.getByRole('button', { name: 'Press Enter — confirm the highlighted option: Yes' });
    expect(enter.textContent).toBe('⏎ EnterYes');
    expect(enter.querySelector('.mobile-approval-option')?.textContent).toBe('Yes');
    expect(snapshot).toHaveBeenCalledWith({ screen: true });
    expect(snapshot).not.toHaveBeenCalledWith();

    // Focus moves (arrow keys): the label follows on the next read.
    live = ' Do you want to proceed?\n   1. Yes\n ❯ 2. No';
    act(() => { vi.advanceTimersByTime(MOBILE_APPROVAL_LABEL_POLL_MS); });
    expect(screen.getByRole('button', { name: 'Press Enter — confirm the highlighted option: No' })).toBeInTheDocument();

    // Unreadable (no focus marker): back to a plain Enter.
    live = ' Do you want to proceed?\n   1. Yes\n   2. No';
    act(() => { vi.advanceTimersByTime(MOBILE_APPROVAL_LABEL_POLL_MS); });
    const plain = screen.getByRole('button', { name: 'Press Enter — confirm the highlighted option' });
    expect(plain.textContent).toBe('⏎ Enter');
    expect(plain.querySelector('.mobile-approval-option')).toBeNull();
  });

  it('caps a long option label', () => {
    const long = `Yes, and don't ask again for ${'x'.repeat(100)}`;
    useStore.setState({ senders: { k: vi.fn() }, paneSnapshots: { k: () => ` Proceed?\n ❯ 1. ${long}\n   2. No` } });
    render(<MobileApprovalBar sessionKey="k" capture={always} />);
    const label = document.querySelector('.mobile-approval-option')!.textContent!;
    expect(label.length).toBe(60);
    expect(label.endsWith('…')).toBe(true);
    expect(long.startsWith(label.slice(0, -1))).toBe(true);
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
