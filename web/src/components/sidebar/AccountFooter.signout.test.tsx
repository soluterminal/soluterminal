import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Session } from '@supabase/supabase-js';
import { useCloud } from '../../cloud';
import { useStore } from '../../store';
import { DialogHost } from '../DialogHost';
import { AccountFooter } from './AccountFooter';

const fakeSession = {
  user: { email: 'ben@example.com' },
} as unknown as Session;

const initialSignOut = useCloud.getState().signOut;

function renderFooter() {
  const signOut = vi.fn(async () => {});
  useCloud.setState({ session: fakeSession, signOut });
  render(
    <>
      <AccountFooter />
      <DialogHost />
    </>,
  );
  return signOut;
}

describe('AccountFooter sign out', () => {
  afterEach(() => {
    useCloud.setState({ session: null, signOut: initialSignOut });
    useStore.setState({ dialog: null, ariaAutoFiresHr: 0, ariaPaused: false });
  });

  it('asks first, like Settings › Account, and does nothing on Cancel', async () => {
    const signOut = renderFooter();
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    expect(await screen.findByText(/Your panes keep running/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByText(/Your panes keep running/)).toBeNull());
    expect(signOut).not.toHaveBeenCalled();
  });

  it('signs out once confirmed', async () => {
    const signOut = renderFooter();
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    await screen.findByText(/Your panes keep running/);
    const dialogConfirm = screen.getAllByRole('button', { name: 'Sign out' }).at(-1)!;
    fireEvent.click(dialogConfirm);
    await waitFor(() => expect(signOut).toHaveBeenCalledTimes(1));
  });
});

describe('AccountFooter auto-approve count', () => {
  afterEach(() => {
    useCloud.setState({ session: null });
    useStore.setState({ ariaAutoFiresHr: 0, ariaPaused: false });
  });

  it('says what it counts and can take keyboard focus for its tooltip', () => {
    useStore.setState({ ariaAutoFiresHr: 219, ariaPaused: false });
    useCloud.setState({ session: fakeSession });
    const { container } = render(<AccountFooter />);
    const count = screen.getByText('219 auto-approved/hr');
    expect(count).toHaveAttribute('tabindex', '0');
    expect(container.querySelector('[title="Auto-approve status"]')).toBeNull();
  });
});
