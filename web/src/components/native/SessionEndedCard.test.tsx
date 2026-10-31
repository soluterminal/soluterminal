import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { SessionEndedCard } from './NativeEventViews';

describe('SessionEndedCard', () => {
  it('asks to resume a resumable session ("Ask me")', () => {
    render(<SessionEndedCard resumable />);
    expect(screen.getByText(/resume to continue/i)).toBeInTheDocument();
  });
  it("says the pane is paused, with no resume ask, under Don't resume", () => {
    render(<SessionEndedCard resumable restartPaused />);
    expect(screen.getByText('Paused after a restart')).toBeInTheDocument();
    expect(screen.queryByText(/resume to continue/i)).toBeNull();
  });
});
