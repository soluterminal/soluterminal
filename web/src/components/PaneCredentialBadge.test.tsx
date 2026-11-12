import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { PaneCredentialBadge, paneAccountStatus } from './PaneCredentialBadge';

import { api } from '../api';
import { useStore } from '../store';
import { useEntitlements } from '../entitlements';
import { setExtensionsForTests } from '../extensions';
import * as paneRestart from '../lib/paneRestart';

beforeEach(() => {
 localStorage.clear();
 useEntitlements.setState({ loaded: true, tier: 'team', team: true, operator: true, account: true });
});
afterEach(() => vi.restoreAllMocks());

const name = (account?: string) => account || 'unknown';
const thread = { id: 'pane_test' };

describe('healthy sessions stay silent', () => {
 it('renders nothing for panes without launch evidence', () => {
  const { container } = render(<PaneCredentialBadge pane={{}} />);
  expect(container).toBeEmptyDOMElement();
 });
 it('renders nothing for a live Codex session', () => {
  const { container } = render(<PaneCredentialBadge pane={{ ...thread, codexAuth: { state: 'live', account: 'support' } }} />);
  expect(container).toBeEmptyDOMElement();
 });
 it('renders nothing when the launch account is still the selected one', () => {
  const { container } = render(<PaneCredentialBadge pane={{ credential: { provider: 'claude', account: 'work', activeAccount: 'work', source: 'launch' } }} />);
  expect(container).toBeEmptyDOMElement();
 });
 it('renders nothing for a broker following the active team account', () => {
  expect(paneAccountStatus({ credential: { provider: 'codex', account: 'team__old', activeAccount: 'team__new', source: 'broker', teamId: 'one', activeTeamId: 'one' } }, name)).toBeNull();
 });
 it('says nothing about an unbridged custom launch until the selection moves', () => {
  expect(paneAccountStatus({ credential: { provider: 'codex', account: 'work', activeAccount: 'work', source: 'launch' }, codexAuth: { state: 'reconnect-required', blocker: 'custom-launch' } }, name)).toBeNull();
 });
});

describe('problems name the account and offer the fix', () => {
 it('a mismatched unbridged Codex pane offers reconnect', () => {
  const status = paneAccountStatus({ ...thread, credential: { provider: 'codex', account: 'northwind', activeAccount: 'support', source: 'launch', restartRequired: true }, codexAuth: { state: 'reconnect-required', canReconnect: true } }, name)!;
  expect(status.tone).toBe('warn');
  expect(status.label).toBe('still on northwind');
  expect(status.detail?.body).toContain('You selected support');
  expect(status.detail?.action).toEqual({ kind: 'reconnect', label: 'Reconnect' });
 });
 it('an unbridged pane on the right account is a quiet, fixable gap', () => {
  const status = paneAccountStatus({ ...thread, codexAuth: { state: 'reconnect-required', canReconnect: true } }, name)!;
  expect(status.tone).toBe('muted');
  expect(status.label).toBe('not live');
  expect(status.detail?.action?.kind).toBe('reconnect');
 });
 it('warns that a shell pane is replaced on reconnect', () => {
  const status = paneAccountStatus({ ...thread, codexAuth: { state: 'reconnect-required', canReconnect: true, reconnectMode: 'shell' } }, name)!;
  expect(status.detail?.note).toContain('background jobs are replaced');
 });
 it('a queued reconnect can be cancelled', () => {
  const status = paneAccountStatus({ ...thread, codexAuth: { state: 'reconnect-required', canReconnect: true, reconnectArmed: true } }, name)!;
  expect(status.label).toBe('reconnects when idle');
  expect(status.detail?.action?.kind).toBe('cancel');
 });
 it('a failed queued reconnect shows why and offers a retry', () => {
  const status = paneAccountStatus({ ...thread, codexAuth: { state: 'reconnect-required', canReconnect: true, reconnectError: 'a working ChatGPT login is required' } }, name)!;
  expect(status.tone).toBe('warn');
  expect(status.detail?.error).toContain('ChatGPT login');
  expect(status.detail?.action?.label).toBe('Try again');
 });
 it('a team selection asks for a restart', () => {
  const status = paneAccountStatus({ ...thread, codexAuth: { state: 'reconnect-required', blocker: 'team' } }, name)!;
  expect(status.detail?.action?.kind).toBe('restart');
 });
 it('a mismatched custom launch offers a restart and says what is dropped', () => {
  const status = paneAccountStatus({ ...thread, credential: { provider: 'codex', account: 'old', activeAccount: 'new', source: 'launch', restartRequired: true }, codexAuth: { state: 'reconnect-required', blocker: 'custom-launch' } }, name)!;
  expect(status.detail?.action?.kind).toBe('restart');
  expect(status.detail?.note).toContain('without the custom launch options');
 });
 it('a restart-required CLI without live auth offers a restart', () => {
  const status = paneAccountStatus({ ...thread, credential: { provider: 'claude', account: 'work', activeAccount: 'team__x', source: 'launch', restartRequired: true } }, name)!;
  expect(status.label).toBe('still on work');
  expect(status.detail?.action?.kind).toBe('restart');
 });
 it('signed-out sessions point to Credentials', () => {
  const status = paneAccountStatus({ ...thread, codexAuth: { state: 'signed-out' } }, name)!;
  expect(status.detail?.action?.kind).toBe('credentials');
 });
});

describe('in-motion states are plain chips', () => {
 it('explains a deferred handoff while a turn is running', () => {
  render(<PaneCredentialBadge pane={{ ...thread, codexAuth: { state: 'waiting-for-idle' } }} />);
  expect(screen.getByText('switch after current turn').title).toContain('approvals');
  expect(screen.queryByRole('button')).not.toBeInTheDocument();
 });
 it('preserves capacity recovery on a live session', () => {
  render(<PaneCredentialBadge pane={{ codexAuth: { state: 'live', account: 'new', recovery: 'waiting', retryAt: 1791508245000 } }} />);
  expect(screen.getByText('model busy · retry scheduled').title).toContain('Type in the terminal to cancel');
 });
 it('keeps a changed-team restart warning ahead of capacity recovery', () => {
  render(<PaneCredentialBadge pane={{ ...thread, credential: { provider: 'codex', source: 'broker', account: 'team__a', activeAccount: 'team__b', teamId: 'one', activeTeamId: 'two', restartRequired: true, liveState: 'live', recovery: 'waiting' } }} />);
  expect(screen.getByRole('button', { name: /Started on team__a/ })).toHaveTextContent('still on team__a');
 });
});

describe('the popover fixes the problem', () => {
 const unbridged = { ...thread, credential: { provider: 'codex', account: 'northwind', activeAccount: 'support', source: 'launch' as const, restartRequired: true }, codexAuth: { state: 'reconnect-required', canReconnect: true } };

 it('reconnect asks the daemon to wait for idle and refreshes', async () => {
  const reconnect = vi.spyOn(api, 'reconnectCodexAuth').mockResolvedValue({ paneId: 'pane_test', armed: true });
  const refresh = vi.fn().mockResolvedValue(undefined); const toast = vi.fn();
  const original = useStore.getState();
  useStore.setState({ refreshPanes: refresh, toast });
  try {
   render(<PaneCredentialBadge pane={unbridged} />);
   fireEvent.click(screen.getByText('still on northwind'));
   expect(screen.getByRole('dialog', { name: 'Still using northwind' })).toBeInTheDocument();
   fireEvent.click(screen.getByRole('button', { name: 'Reconnect' }));
   await waitFor(() => expect(refresh).toHaveBeenCalled());
   expect(reconnect).toHaveBeenCalledWith('pane_test', { when: 'idle' });
   expect(toast).toHaveBeenCalledWith(expect.stringContaining('next idle prompt'), expect.anything());
  } finally { useStore.setState({ refreshPanes: original.refreshPanes, toast: original.toast }); }
 });

 it('a refusal stays in the popover with the reason', async () => {
  vi.spyOn(api, 'reconnectCodexAuth').mockRejectedValue(new Error('custom Codex launch requires a manual reconnect'));
  const refresh = vi.fn();
  const original = useStore.getState();
  useStore.setState({ refreshPanes: refresh });
  try {
   render(<PaneCredentialBadge pane={unbridged} />);
   fireEvent.click(screen.getByText('still on northwind'));
   fireEvent.click(screen.getByRole('button', { name: 'Reconnect' }));
   expect(await screen.findByRole('alert')).toHaveTextContent('custom Codex launch');
   expect(refresh).not.toHaveBeenCalled();
   expect(screen.getByRole('button', { name: 'Reconnect' })).toBeEnabled();
  } finally { useStore.setState({ refreshPanes: original.refreshPanes }); }
 });

 it('cancel withdraws a queued reconnect', async () => {
  const reconnect = vi.spyOn(api, 'reconnectCodexAuth').mockResolvedValue({ paneId: 'pane_test', armed: false });
  const original = useStore.getState();
  useStore.setState({ refreshPanes: vi.fn().mockResolvedValue(undefined) });
  try {
   render(<PaneCredentialBadge pane={{ ...thread, codexAuth: { state: 'reconnect-required', canReconnect: true, reconnectArmed: true } }} />);
   fireEvent.click(screen.getByText('reconnects when idle'));
   fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
   await waitFor(() => expect(reconnect).toHaveBeenCalledWith('pane_test', { cancel: true }));
  } finally { useStore.setState({ refreshPanes: original.refreshPanes }); }
 });

 it('restart uses the guarded pane restart flow', async () => {
  const restart = vi.spyOn(paneRestart, 'restartPaneFlow').mockResolvedValue(undefined);
  render(<PaneCredentialBadge pane={{ ...thread, codexAuth: { state: 'reconnect-required', blocker: 'team' } }} />);
  fireEvent.click(screen.getByText('restart for account'));
  fireEvent.click(screen.getByRole('button', { name: 'Restart session' }));
  await waitFor(() => expect(restart).toHaveBeenCalledWith('pane_test'));
 });

 it('Open Credentials reveals the Credentials sidebar section', () => {
  const listener = vi.fn();
  window.addEventListener('solu:reveal-sidebar-section', listener);
  setExtensionsForTests([{ sidebarSections: { credentials: () => null } }]);
  try {
   render(<PaneCredentialBadge pane={{ ...thread, codexAuth: { state: 'signed-out' } }} />);
   fireEvent.click(screen.getByText('signed out'));
   fireEvent.click(screen.getByRole('button', { name: 'Open Credentials' }));
   expect(listener).toHaveBeenCalledTimes(1);
   expect((listener.mock.calls[0][0] as CustomEvent).detail).toEqual({ key: 'credentials' });
  } finally { window.removeEventListener('solu:reveal-sidebar-section', listener); setExtensionsForTests(null); }
 });

 it('points a team install at codex login when the build has no Credentials section', () => {
  setExtensionsForTests([]);
  try {
   render(<PaneCredentialBadge pane={{ ...thread, codexAuth: { state: 'signed-out' } }} />);
   fireEvent.click(screen.getByText('signed out'));
   expect(screen.getByText(/Run codex login in a shell pane/)).toBeInTheDocument();
   expect(screen.queryByRole('button', { name: 'Open Credentials' })).toBeNull();
  } finally { setExtensionsForTests(null); }
 });

 it('points a public install at codex login instead of team saved accounts', async () => {
  useEntitlements.setState({ loaded: true, tier: 'none', team: false, operator: false, account: false });
  const restart = vi.spyOn(paneRestart, 'restartPaneFlow').mockResolvedValue(undefined as never);
  render(<PaneCredentialBadge pane={{ ...thread, codexAuth: { state: 'signed-out' } }} />);
  fireEvent.click(screen.getByText('signed out'));
  expect(screen.getByText(/Run codex login in a shell pane/)).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Open Credentials' })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Restart pane' }));
  await waitFor(() => expect(restart).toHaveBeenCalledWith('pane_test'));
 });
});

it('reads sensibly when the launch evidence lacks account names', () => {
 const status = paneAccountStatus({ ...thread, credential: { provider: 'codex', source: 'broker', restartRequired: true } }, name)!;
 expect(status.label).toBe('account changed');
 expect(status.detail?.heading).toBe('Still using its launch account');
 expect(status.detail?.body).toMatch(/^another account is selected/);
});

it('hides identities in chip and popover for screen sharing', () => {
 localStorage.setItem('solu.hideIdentities', '1');
 const { container } = render(<PaneCredentialBadge pane={{ ...thread, credential: { provider: 'codex', account: 'secret@example.com', activeAccount: 'other@example.com', source: 'launch', restartRequired: true }, codexAuth: { state: 'reconnect-required', canReconnect: true } }} />);
 fireEvent.click(container.querySelector('button')!);
 expect(document.body.innerHTML).not.toContain('secret@example.com');
 expect(document.body.innerHTML).not.toContain('other@example.com');
});

describe('a Backend setting change that cut the pane off', () => {
 it('a team session offers a restart that keeps the conversation', async () => {
  const restart = vi.spyOn(paneRestart, 'restartPaneFlow').mockResolvedValue(undefined as never);
  render(<PaneCredentialBadge pane={{ ...thread, credential: { provider: 'claude', account: 'team__solux', activeAccount: 'team__solux', source: 'broker', connectionLost: true } }} />);
  fireEvent.click(screen.getByText('connection lost'));
  expect(screen.getByText('This session lost its team connection after the Backend setting was changed. Restart it to continue; the conversation is kept.')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Restart session' }));
  await waitFor(() => expect(restart).toHaveBeenCalledWith('pane_test'));
 });
 it('outranks an account mismatch: restarting fixes both', () => {
  const status = paneAccountStatus({ ...thread, credential: { provider: 'codex', account: 'team__old', activeAccount: 'work', source: 'broker', restartRequired: true, connectionLost: true } }, name)!;
  expect(status.tone).toBe('warn');
  expect(status.detail?.heading).toBe('Team connection lost');
 });
 it('a terminal says what to do and warns what a restart ends', async () => {
  const resume = vi.spyOn(paneRestart, 'restartPaneFlow').mockResolvedValue(undefined as never);
  const restart = vi.spyOn(paneRestart, 'restartTerminalFlow').mockResolvedValue(undefined as never);
  render(<PaneCredentialBadge pane={{ ...thread, terminalAddressLost: true }} />);
  fireEvent.click(screen.getByText('needs restart'));
  // The two commands render as code, not as backticks.
  expect(screen.getByText('claude --resume').tagName).toBe('CODE');
  expect(screen.getByText('codex resume').tagName).toBe('CODE');
  expect(document.body.textContent).toContain('start them again: claude --resume or codex resume keeps the conversation.');
  expect(screen.getByText('Restarting ends anything still running in this terminal.')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Restart terminal' }));
  await waitFor(() => expect(restart).toHaveBeenCalledWith('pane_test'));
  // Never the resuming restart: it would relaunch an adopted agent
  // conversation in place of the shell.
  expect(resume).not.toHaveBeenCalled();
 });
 it('stays silent for a terminal that is fine', () => {
  expect(paneAccountStatus({ ...thread, terminalAddressLost: false }, name)).toBeNull();
 });
});
