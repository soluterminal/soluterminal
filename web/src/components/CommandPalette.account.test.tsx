import { act, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Session } from '@supabase/supabase-js';
import { useCloud } from '../cloud';
import { useEntitlements } from '../entitlements';
import { CommandPalette } from './CommandPalette';

vi.mock('../api', () => ({
  onRoutingScopeChange: vi.fn(() => () => {}),
  currentRoutingScope: vi.fn(() => ({ prefix: '', backendPrefix: '', key: '|' })),
  api: { health: vi.fn().mockResolvedValue({ version: '1.2.3' }) },
}));
Element.prototype.scrollIntoView = vi.fn();

const session = { user: { email: 'ben@example.com' } } as unknown as Session;

function renderPalette() {
  render(<CommandPalette open initialQuery="Sign out" onClose={vi.fn()}
    onOpenHelp={vi.fn()} onOpenNewPane={vi.fn()} onOpenBroadcast={vi.fn()}
    onOpenActivity={vi.fn()} onOpenFeedback={vi.fn()} />);
}

// The Sign out action's hint (the row's title) names the account, except
// while presenting or in the personal view.
function signOutHint(): string | null {
  return screen.getByText('Sign out').closest('.quickopen-row')!.getAttribute('title');
}

describe('CommandPalette Sign out hint', () => {
  afterEach(() => {
    useCloud.setState({ session: null });
    localStorage.removeItem('solu.hideIdentities');
    useEntitlements.setState({ loaded: false, viewPersonal: false });
  });

  it('names the signed-in email normally', () => {
    useCloud.setState({ session });
    renderPalette();
    expect(signOutHint()).toBe('Signed in as ben@example.com');
  });

  it('hides the email in Presentation mode', () => {
    localStorage.setItem('solu.hideIdentities', '1');
    useCloud.setState({ session });
    renderPalette();
    expect(signOutHint()).toBe('Signed in');
  });

  it('drops the email when masking turns on while the palette is open', () => {
    useCloud.setState({ session });
    renderPalette();
    expect(signOutHint()).toBe('Signed in as ben@example.com');
    act(() => {
      localStorage.setItem('solu.hideIdentities', '1');
      window.dispatchEvent(new CustomEvent('solu:hide-identities-changed'));
    });
    expect(signOutHint()).toBe('Signed in');
    act(() => {
      localStorage.setItem('solu.hideIdentities', '0');
      window.dispatchEvent(new CustomEvent('solu:hide-identities-changed'));
      useEntitlements.setState({ loaded: true, viewPersonal: true });
    });
    expect(signOutHint()).toBe('Signed in');
  });

  it('hides the email in the personal view', () => {
    useEntitlements.setState({ loaded: true, viewPersonal: true });
    useCloud.setState({ session });
    renderPalette();
    expect(signOutHint()).toBe('Signed in');
  });
});
