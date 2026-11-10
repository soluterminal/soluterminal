import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { useStore } from '../store';
import type { PaneInfo, PaneReview } from '../types';
import { PaneReviewChip, reviewChip, reviewSummary } from './PaneReviewChip';

function pane(review?: PaneReview): PaneInfo {
  return { id: 'p1', cwd: '/tmp', cmd: ['claude'], title: 'Fix X', status: 'running', review } as PaneInfo;
}

const changes: PaneReview = {
  verdict: 'request-changes', reviewer: 'codex', author: 'claude', round: 1, mustFix: 1, notes: 1, at: Date.now(),
  title: 'Fix the average', summary: 'Divides by zero on an empty list.',
  findings: [
    { severity: 'high', where: 'calc.go:3', what: 'Average panics on an empty slice', blocking: true },
    { severity: 'low', what: 'Name the helper', blocking: false },
  ],
};

describe('PaneReviewChip', () => {
  it('shows nothing on a pane whose work was never reviewed', () => {
    const { container } = render(<PaneReviewChip pane={pane()} />);
    expect(container.innerHTML).toBe('');
  });

  it('says which model reviewed whose work, and what must be fixed', () => {
    render(<PaneReviewChip pane={pane(changes)} />);
    const chip = screen.getByRole('button', { name: "Codex reviewed Claude's work: 1 to fix. Show review" });
    expect(chip.textContent).toBe('1 to fix');
    fireEvent.click(chip);
    expect(screen.getByRole('dialog')).toBeTruthy();
    expect(screen.getByText('Average panics on an empty slice')).toBeTruthy();
    expect(screen.getByText(/a different model from the author/)).toBeTruthy();
    expect(screen.getByText(/1 optional note/)).toBeTruthy();
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('labels each outcome plainly', () => {
    expect(reviewChip({ ...changes, verdict: 'approve' }).label).toBe('✓ Reviewed');
    expect(reviewChip({ ...changes, verdict: 'comment' }).label).toBe('Review: questions');
    expect(reviewSummary({ ...changes, verdict: 'comment' })).toBe("Codex reviewed Claude's work: has questions");
    expect(reviewChip({ ...changes, verdict: 'error' }).label).toBe('Review failed');
    expect(reviewSummary({ ...changes, verdict: 'approve', author: undefined })).toBe('Codex reviewed: approved');
  });

  it('shows the reviewed branch and commit without treating local approval as current PR approval', () => {
    render(<PaneReviewChip pane={pane({ ...changes, verdict: 'approve', branch: 'fix/average', head: '1234567890abcdef1234567890abcdef12345678' })} />);
    fireEvent.click(screen.getByText('✓ Reviewed'));
    expect(screen.getByText('fix/average')).toBeTruthy();
    expect(screen.getByText('1234567890ab')).toBeTruthy();
    expect(screen.getByText(/pull request's current commit and required reviews/)).toBeTruthy();
  });

  it('reads "Fixed after review" once the author recorded the fix, and shows the note', () => {
    const fixed = { ...changes, round: 2, followup: { note: 'Setting saves before it takes effect', at: Date.now() } };
    expect(reviewChip(fixed)).toEqual({ label: 'Fixed after review', tone: 'muted' });
    expect(reviewChip({ ...fixed, verdict: 'approve' }).label).toBe('✓ Reviewed');
    render(<PaneReviewChip pane={pane(fixed)} />);
    fireEvent.click(screen.getByText('Fixed after review'));
    expect(screen.getByText('Setting saves before it takes effect')).toBeTruthy();
    expect(screen.getByText(/not re-reviewed/)).toBeTruthy();
  });

  it('"Review: questions" opens to the pane\'s asks as well as the reviewer\'s questions', () => {
    const onHold: PaneReview = { ...changes, verdict: 'comment', mustFix: 0, findings: [], questions: ['Ship behind a flag?'] };
    const p = { ...pane(onHold), asks: [{ id: 'a1', text: 'Rotate the key now?', source: '@lead', createdAt: Date.now() }] } as PaneInfo;
    render(<PaneReviewChip pane={p} />);
    fireEvent.click(screen.getByRole('button', { name: /has questions/ }));
    expect(screen.getByText('Ship behind a flag?')).toBeTruthy();
    expect(screen.getByText('Rotate the key now?')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Dismiss ask from @lead' })).toBeTruthy();
  });

  it('Clear removes the result from the pane', async () => {
    const clear = vi.spyOn(api, 'clearPaneReview').mockResolvedValue(undefined);
    useStore.setState({ panes: { p1: pane(changes) } } as never);
    render(<PaneReviewChip pane={pane(changes)} />);
    fireEvent.click(screen.getByText('1 to fix'));
    fireEvent.click(screen.getByText('Clear'));
    await waitFor(() => expect(useStore.getState().panes.p1.review).toBeUndefined());
    expect(clear).toHaveBeenCalledWith('p1');
    expect(screen.queryByRole('dialog')).toBeNull();
  });
});

afterEach(() => { vi.restoreAllMocks(); });
