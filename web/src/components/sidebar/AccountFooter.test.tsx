import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useCloud } from '../../cloud';
import { useEntitlements } from '../../entitlements';
import { AccountFooter } from './AccountFooter';
import type { Session } from '@supabase/supabase-js';

// Usage moved out of the sidebar sections: its entry point is now a compact
// gauge button on the account footer's identity row (usage is account-level
// cloud data, not a live sidebar ticker). These tests run at jsdom's desktop
// width, where the footer renders only with a signed-in session.

const fakeSession = {
  user: { email: 'ben@example.com' },
} as unknown as Session;

describe('AccountFooter usage entry point', () => {
  afterEach(() => {
    useCloud.setState({ session: null });
  });

  it('renders the Usage button on the identity row and opens the page', () => {
    useCloud.setState({ session: fakeSession });
    const onOpenUsage = vi.fn();
    render(<AccountFooter onOpenUsage={onOpenUsage} />);

    const btn = screen.getByRole('button', { name: /open usage/i });
    fireEvent.click(btn);
    expect(onOpenUsage).toHaveBeenCalledTimes(1);
  });

  it('omits the Usage button when no opener is wired', () => {
    useCloud.setState({ session: fakeSession });
    render(<AccountFooter />);

    expect(screen.queryByRole('button', { name: /open usage/i })).toBeNull();
    // Identity row still renders (email + sign out).
    expect(screen.getByText('ben@example.com')).toBeInTheDocument();
  });
});

describe('AccountFooter email privacy', () => {
  afterEach(() => {
    useCloud.setState({ session: null });
    localStorage.removeItem('solu.hideIdentities');
    useEntitlements.setState({ loaded: false, viewPersonal: false });
  });

  it('hides the email in Presentation mode', () => {
    localStorage.setItem('solu.hideIdentities', '1');
    useCloud.setState({ session: fakeSession });
    const { container } = render(<AccountFooter />);
    expect(container.innerHTML).not.toContain('ben@example.com');
    expect(document.querySelector('.sidebar-footer-identity')).toHaveTextContent('Signed in');
    expect(document.querySelector('.sidebar-footer')?.getAttribute('title')).toBe('Signed in');
  });

  it('hides the email in the personal view', () => {
    useEntitlements.setState({ loaded: true, viewPersonal: true });
    useCloud.setState({ session: fakeSession });
    render(<AccountFooter />);
    expect(screen.queryByText('ben@example.com')).toBeNull();
  });
});
