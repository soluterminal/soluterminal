import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Session } from '@supabase/supabase-js';
import { useCloud } from '../../cloud';
import { AccountFooter } from './AccountFooter';

const fakeSession = {
  user: { email: 'ben@example.com' },
} as unknown as Session;

describe('AccountFooter System entry point', () => {
  afterEach(() => {
    useCloud.setState({ session: null });
  });

  it('opens System without requiring a cloud session', () => {
    const onOpenSystem = vi.fn();
    const { container } = render(<AccountFooter onOpenSystem={onOpenSystem} />);

    fireEvent.click(screen.getByRole('button', { name: /open system/i }));
    expect(onOpenSystem).toHaveBeenCalledTimes(1);
    expect(container.querySelector('.sidebar-footer-system-row')).not.toBeNull();
    expect(container.querySelector('.sidebar-footer-identity-row')).toBeNull();
  });

  it('leaves System to the status bar on desktop once signed in', () => {
    useCloud.setState({ session: fakeSession });
    render(<AccountFooter onOpenUsage={vi.fn()} onOpenSystem={vi.fn()} />);

    expect(screen.getByRole('button', { name: /open usage/i })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /open system/i })).toBeNull();
  });
});
