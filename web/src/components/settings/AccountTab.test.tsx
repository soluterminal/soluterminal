import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Session } from '@supabase/supabase-js';
import { useCloud } from '../../cloud';
import { AccountTab } from './AccountTab';

vi.mock('../../api', () => ({
  onRoutingScopeChange: vi.fn(() => () => {}),
  currentRoutingScope: vi.fn(() => ({ prefix: '', backendPrefix: '', key: '|' })),
  currentPrefix: vi.fn(() => ''),
  api: { getMarketingConsent: vi.fn().mockResolvedValue({ consent: false }) },
}));

const session = { user: { email: 'ben@example.com' } } as unknown as Session;

// Settings › Account is one of the surfaces Presentation mode promises to
// keep the email off (PresentationModeSetting's hint).
describe('AccountTab in Presentation mode', () => {
  afterEach(() => {
    useCloud.setState({ session: null });
    localStorage.removeItem('solu.hideIdentities');
  });

  it('names the signed-in account normally', () => {
    useCloud.setState({ session });
    render(<AccountTab />);
    expect(screen.getByText('Signed in as ben@example.com')).toBeInTheDocument();
  });

  it('shows no email while presenting', () => {
    localStorage.setItem('solu.hideIdentities', '1');
    useCloud.setState({ session });
    const { container } = render(<AccountTab />);
    expect(container.innerHTML).not.toContain('ben@example.com');
    expect(screen.getByText('Signed in')).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'Presentation mode' })).toBeChecked();
  });
});
