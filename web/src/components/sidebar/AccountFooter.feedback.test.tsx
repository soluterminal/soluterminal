import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { AccountFooter } from './AccountFooter';

vi.mock('../useIsMobileView', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../useIsMobileView')>()),
  useIsMobileView: () => true,
}));

describe('AccountFooter on a phone', () => {
  it('has Send feedback, which opens the feedback form', () => {
    const opened = vi.fn();
    const onOpen = (e: Event) => opened((e as CustomEvent).detail);
    window.addEventListener('solu:open-inbox', onOpen);
    try {
      render(<AccountFooter onOpenHelp={() => {}} />);
      fireEvent.click(screen.getByRole('button', { name: 'Send feedback' }));
      expect(opened).toHaveBeenCalledWith(expect.objectContaining({ view: 'compose' }));
    } finally {
      window.removeEventListener('solu:open-inbox', onOpen);
    }
  });
});
