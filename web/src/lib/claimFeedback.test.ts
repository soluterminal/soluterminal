import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../embed', () => ({ sendEmbedClaimRefused: vi.fn() }));

import { sendEmbedClaimRefused } from '../embed';
import {
  CLAIM_FEEDBACK_TIMEOUT_MS,
  CLAIM_REFUSED_SETTLE_MS,
  claimAttempt,
  beginTypeHere,
  clearClaimFeedback,
  parseClaimDenied,
  reportClaimRefused,
  useClaimFeedback,
} from './claimFeedback';

beforeEach(() => {
  vi.useFakeTimers();
  clearClaimFeedback();
  vi.mocked(sendEmbedClaimRefused).mockClear();
});
afterEach(() => {
  clearClaimFeedback();
  vi.useRealTimers();
});

describe('claimFeedback', () => {
  it('reads only the daemon reasons it knows', () => {
    expect(parseClaimDenied('not-person')).toBe('not-person');
    expect(parseClaimDenied('view-only')).toBe('view-only');
    expect(parseClaimDenied('owner-active')).toBe('owner-active');
    expect(parseClaimDenied('unreachable')).toBeNull();
    expect(parseClaimDenied(undefined)).toBeNull();
    expect(parseClaimDenied(1)).toBeNull();
  });

  it('a click with no answer ends as unreachable, never as nothing', () => {
    beginTypeHere();
    expect(useClaimFeedback.getState()).toEqual({ state: 'pending' });
    vi.advanceTimersByTime(CLAIM_FEEDBACK_TIMEOUT_MS);
    expect(useClaimFeedback.getState()).toEqual({ state: 'refused', reason: 'unreachable' });
  });

  it('a refusal ends the wait with its reason', () => {
    beginTypeHere();
    reportClaimRefused('not-person');
    vi.advanceTimersByTime(CLAIM_FEEDBACK_TIMEOUT_MS);
    expect(useClaimFeedback.getState()).toEqual({ state: 'refused', reason: 'not-person' });
  });

  it('keeps the reason the person can act on first across panes', () => {
    beginTypeHere();
    reportClaimRefused('unreachable');
    reportClaimRefused('not-person');
    reportClaimRefused('owner-active');
    expect(useClaimFeedback.getState()).toEqual({ state: 'refused', reason: 'not-person' });
  });

  it('ignores refusals with no click, except a re-grab', () => {
    reportClaimRefused('owner-active');
    expect(useClaimFeedback.getState()).toEqual({ state: 'idle' });
    reportClaimRefused('regrabbed');
    expect(useClaimFeedback.getState()).toEqual({ state: 'refused', reason: 'regrabbed' });
  });

  it('clearing drops a pending timeout', () => {
    beginTypeHere();
    clearClaimFeedback();
    vi.advanceTimersByTime(CLAIM_FEEDBACK_TIMEOUT_MS);
    expect(useClaimFeedback.getState()).toEqual({ state: 'idle' });
  });

  it('ignores an answer for a click that has ended', () => {
    beginTypeHere();
    const first = claimAttempt();
    clearClaimFeedback(true); // another workspace came into view
    beginTypeHere();
    reportClaimRefused('not-person', first);
    expect(useClaimFeedback.getState()).toEqual({ state: 'pending' });
    reportClaimRefused('owner-active', claimAttempt());
    expect(useClaimFeedback.getState()).toEqual({ state: 'refused', reason: 'owner-active' });
  });

  it('a re-grab after the chip went still reports; after a view change it does not', () => {
    beginTypeHere();
    const a = claimAttempt();
    clearClaimFeedback(); // granted: the chip goes
    reportClaimRefused('regrabbed', a);
    expect(useClaimFeedback.getState()).toEqual({ state: 'refused', reason: 'regrabbed' });
    clearClaimFeedback(true);
    reportClaimRefused('regrabbed', a);
    expect(useClaimFeedback.getState()).toEqual({ state: 'idle' });
  });
});

// ST-248: an embedding host hears of each refused click once.
describe('claim-refused for the host', () => {
  it('sends the settled reason once per refused click', () => {
    beginTypeHere();
    reportClaimRefused('owner-active', claimAttempt());
    reportClaimRefused('not-person', claimAttempt()); // a later pane raises it
    reportClaimRefused('not-person', claimAttempt());
    expect(sendEmbedClaimRefused).not.toHaveBeenCalled();
    vi.advanceTimersByTime(CLAIM_REFUSED_SETTLE_MS);
    expect(sendEmbedClaimRefused).toHaveBeenCalledExactlyOnceWith('not-person');
    vi.advanceTimersByTime(CLAIM_FEEDBACK_TIMEOUT_MS);
    expect(sendEmbedClaimRefused).toHaveBeenCalledTimes(1);
    // Try again is a new click.
    beginTypeHere();
    vi.advanceTimersByTime(CLAIM_FEEDBACK_TIMEOUT_MS + CLAIM_REFUSED_SETTLE_MS);
    expect(sendEmbedClaimRefused).toHaveBeenLastCalledWith('unreachable');
    expect(sendEmbedClaimRefused).toHaveBeenCalledTimes(2);
  });

  it('sends nothing for a click granted before the reason settles, or one that ended', () => {
    beginTypeHere();
    reportClaimRefused('owner-active', claimAttempt());
    clearClaimFeedback(); // granted: the chip went
    vi.advanceTimersByTime(CLAIM_REFUSED_SETTLE_MS);
    beginTypeHere();
    reportClaimRefused('owner-active', claimAttempt());
    clearClaimFeedback(true); // the view changed
    vi.advanceTimersByTime(CLAIM_REFUSED_SETTLE_MS);
    expect(sendEmbedClaimRefused).not.toHaveBeenCalled();
  });

  it('reports a re-grab after a grant', () => {
    beginTypeHere();
    const from = claimAttempt();
    clearClaimFeedback(); // granted
    reportClaimRefused('regrabbed', from);
    vi.advanceTimersByTime(CLAIM_REFUSED_SETTLE_MS);
    expect(sendEmbedClaimRefused).toHaveBeenCalledExactlyOnceWith('regrabbed');
  });
});
