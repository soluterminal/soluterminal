import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const state = vi.hoisted(() => ({
  ownership: { p1: false } as Record<string, boolean>,
  ownerState: { p1: 'other' } as Record<string, string>,
  claimAllPanes: vi.fn(),
  embedded: false,
  promptOpens: true,
}));

vi.mock('../api', () => ({ currentRoutingScope: () => ({ key: 'local', prefix: '', backendPrefix: '' }) }));
vi.mock('../embed', () => ({ isEmbedded: () => state.embedded }));
vi.mock('./ApproveThisScreenPrompt', () => ({ openApproveThisScreenPrompt: vi.fn(() => state.promptOpens) }));
vi.mock('../store', () => {
  const snapshot = () => ({ paneOwnership: state.ownership, paneOwnerState: state.ownerState, claimAllPanes: state.claimAllPanes });
  // Only what the chip reads: its selectors run against this snapshot.
  const useStore = <T,>(sel: (s: ReturnType<typeof snapshot>) => T): T => sel(snapshot());
  return {
    useStore,
    effectiveVisiblePaneIds: () => ['p1'],
    terminalSessionKey: (_scope: unknown, id: string) => id,
  };
});

import { CLAIM_FEEDBACK_TIMEOUT_MS, clearClaimFeedback, reportClaimRefused } from '../lib/claimFeedback';
import { openApproveThisScreenPrompt } from './ApproveThisScreenPrompt';
import { SessionClaimOverlay } from './SessionClaimOverlay';

beforeEach(() => {
  vi.useFakeTimers();
  state.ownership = { p1: false };
  state.ownerState = { p1: 'other' };
  state.embedded = false;
  state.promptOpens = true;
  clearClaimFeedback();
});
afterEach(() => {
  clearClaimFeedback();
  vi.clearAllMocks();
  vi.useRealTimers();
});

const chip = () => screen.getByRole('status');
const buttons = () => Array.from(chip().querySelectorAll('button')).map((b) => b.textContent);

describe('SessionClaimOverlay', () => {
  it('Type here claims and shows it is working on it', () => {
    render(<SessionClaimOverlay />);
    fireEvent.click(screen.getByRole('button', { name: 'Type here' }));
    expect(state.claimAllPanes).toHaveBeenCalledTimes(1);
    expect(screen.getByRole('button', { name: 'Taking over…' })).toBeDisabled();
  });

  it('a screen the daemon says is not the person offers to approve it, and to try again', () => {
    render(<SessionClaimOverlay />);
    fireEvent.click(screen.getByRole('button', { name: 'Type here' }));
    act(() => reportClaimRefused('not-person'));
    expect(chip()).toHaveTextContent("This screen isn't approved yet, so it can't take typing from another device.");
    expect(buttons()).toEqual(['Approve this screen', 'Try again']);
    fireEvent.click(screen.getByRole('button', { name: 'Approve this screen' }));
    // It only opens ST-9's enrol-only prompt; it never claims or approves.
    expect(openApproveThisScreenPrompt).toHaveBeenCalledTimes(1);
    expect(state.claimAllPanes).toHaveBeenCalledTimes(1);
  });

  it('names the Shinoque Code window', () => {
    state.embedded = true;
    render(<SessionClaimOverlay />);
    fireEvent.click(screen.getByRole('button', { name: 'Type here' }));
    act(() => reportClaimRefused('view-only'));
    expect(chip()).toHaveTextContent("This Shinoque Code window can only view this computer's panes until it's approved.");
  });

  it('says where to approve when the prompt has nothing to offer here', () => {
    state.promptOpens = false;
    render(<SessionClaimOverlay />);
    fireEvent.click(screen.getByRole('button', { name: 'Type here' }));
    act(() => reportClaimRefused('not-person'));
    fireEvent.click(screen.getByRole('button', { name: 'Approve this screen' }));
    expect(chip()).toHaveTextContent('Settings → Privacy & security → Remote devices');
    expect(buttons()).toEqual(['Try again']);
  });

  it('another device typing says so, and Try again claims again', () => {
    render(<SessionClaimOverlay />);
    fireEvent.click(screen.getByRole('button', { name: 'Type here' }));
    act(() => reportClaimRefused('owner-active'));
    expect(chip()).toHaveTextContent('Another device is typing now. Ask it to stop, or try again.');
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(state.claimAllPanes).toHaveBeenCalledTimes(2);
  });

  it('no answer at all ends with a reason, not silence', () => {
    render(<SessionClaimOverlay />);
    fireEvent.click(screen.getByRole('button', { name: 'Type here' }));
    act(() => { vi.advanceTimersByTime(CLAIM_FEEDBACK_TIMEOUT_MS); });
    expect(chip()).toHaveTextContent("Couldn't reach this pane. Check the connection, then try again.");
    expect(buttons()).toEqual(['Try again']);
  });

  it('keeps the original copy until a click', () => {
    render(<SessionClaimOverlay />);
    expect(chip()).toHaveTextContent('Typing is on another device. You can watch here.');
  });
});
