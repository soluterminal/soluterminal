import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { PaneConfirmHost } from './PaneConfirmHost';
import { notePaneConfirm, resetPaneConfirmsForTest } from '../lib/paneConfirm';

const reviewMock = vi.hoisted(() => vi.fn());
vi.mock('../lib/paneConfirm', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../lib/paneConfirm')>()),
  reviewPaneConfirm: reviewMock,
}));

describe('PaneConfirmHost', () => {
  afterEach(() => {
    act(() => resetPaneConfirmsForTest());
    reviewMock.mockReset();
  });

  it('renders nothing without requests', () => {
    const { container } = render(<PaneConfirmHost />);
    expect(container.firstChild).toBeNull();
  });

  it('lists each open request with a Review button', () => {
    render(<PaneConfirmHost />);
    act(() => notePaneConfirm('req-1', '@cmo wants to close pane #4 “Social loop”.'));
    expect(screen.getByText('@cmo wants to close pane #4 “Social loop”.')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: /Review: @cmo wants to close/ }));
    expect(reviewMock).toHaveBeenCalledWith('req-1');
  });
});
