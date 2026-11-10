import { act, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import { useEntitlements } from '../entitlements';
import { StatusBar } from './StatusBar';

// The identity badge beside the version (lib/identityBadge.ts): one quiet word,
// spoken through the button's name, gone in Presentation mode and the
// personal view.

const apiMock = vi.hoisted(() => ({
  health: vi.fn().mockResolvedValue({ version: '0.0.1239.10', build: { channel: 'preview', flavor: 'team' } }),
  workflowInbox: vi.fn().mockResolvedValue({ decide: [], recovering: [], waiting: [], history: [], counts: { decide: 0 } }),
}));

vi.mock('../api', () => ({
  onRoutingScopeChange: vi.fn(() => () => {}),
  currentRoutingScope: vi.fn(() => ({ prefix: '', backendPrefix: '', key: '|' })),
  currentPrefix: vi.fn(() => ''),
  api: apiMock,
}));

const TEAM = { tier: 'team' as const, team: true, operator: true, account: true, accountSwitching: true, teamCredentials: true, smartApprovals: null };

function setPresentation(on: boolean) {
  localStorage.setItem('solu.hideIdentities', on ? '1' : '0');
  window.dispatchEvent(new CustomEvent('solu:hide-identities-changed'));
}

describe('StatusBar identity badge', () => {
  beforeEach(() => {
    useEntitlements.setState({ loaded: true, ...TEAM, real: TEAM, viewPersonal: false });
    useStore.setState({ panes: {}, focusedPaneId: null, ports: [], updateStatus: null, deferredUpdate: null, updateApplyRequestInFlight: false });
  });

  afterEach(() => {
    localStorage.removeItem('solu.hideIdentities');
    useEntitlements.setState({ loaded: false, viewPersonal: false });
  });

  it('shows one aria-hidden word and says it in the button name', async () => {
    render(<StatusBar />);
    const button = await screen.findByRole('button', { name: 'v0.0.1239.10 · Preview channel' });
    const badge = screen.getByTestId('statusbar-identity-badge');
    expect(badge).toHaveTextContent('PREVIEW');
    expect(badge).toHaveAttribute('aria-hidden', 'true');
    expect(button).toContainElement(badge);
    expect(button).toHaveTextContent('v0.0.1239.10');
  });

  it('drops the badge in Presentation mode and keeps the version', async () => {
    render(<StatusBar />);
    await screen.findByTestId('statusbar-identity-badge');
    act(() => setPresentation(true));
    expect(screen.queryByTestId('statusbar-identity-badge')).toBeNull();
    expect(screen.getByRole('button', { name: 'v0.0.1239.10' })).toHaveTextContent('v0.0.1239.10');
  });

  it('drops the badge in the personal view', async () => {
    const free = { ...TEAM, tier: 'account' as const, team: false, operator: false, accountSwitching: false, teamCredentials: false };
    useEntitlements.setState({ loaded: true, ...free, real: TEAM, viewPersonal: true });
    render(<StatusBar />);
    await screen.findByRole('button', { name: 'v0.0.1239.10' });
    expect(screen.queryByTestId('statusbar-identity-badge')).toBeNull();
  });
});
