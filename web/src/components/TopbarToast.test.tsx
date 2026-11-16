import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import { TopbarToast } from './TopbarToast';

let prior: ReturnType<typeof useStore.getState>;
beforeEach(() => { prior = useStore.getState(); });
afterEach(() => { cleanup(); useStore.setState(prior, true); });

// ST-134: an action such as "Open in browser" goes through the gated opener,
// which needs the click that triggered it (as ToastHost passes it).
it('hands the click to a toast action', () => {
  const run = vi.fn();
  useStore.setState({
    toastPosition: 'top-center',
    toasts: [{ id: 1, message: 'Site ready', kind: 'info', action: { label: 'Open in browser', run } }],
  });
  render(<TopbarToast />);
  fireEvent.click(screen.getByRole('button', { name: /Open in browser: Site ready/ }));
  expect(run).toHaveBeenCalledTimes(1);
  expect(run.mock.calls[0][0]).toBeInstanceOf(MouseEvent);
  expect(useStore.getState().toasts).toEqual([]);
});
