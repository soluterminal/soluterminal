import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { PhoneAccessPairPanelGate } from './PhoneAccessPairPanel';
import { useStore } from '../store';
import { useEntitlements } from '../entitlements';

const mocks = vi.hoisted(() => ({
  start: vi.fn(), cancel: vi.fn(), status: vi.fn(),
}));
vi.mock('../cloudPair', () => ({
  isDesktopShell: () => true,
  secondsUntil: () => 300,
  usePhoneAccessStatus: () => ({status:mocks.status()}),
  cloudPairApi: {pairStart:mocks.start, pairCancel:mocks.cancel},
}));

beforeEach(() => {
  useEntitlements.setState({ loaded: true, team: true, operator: true });
  mocks.start.mockReset().mockResolvedValue({code:'TEST12'});
  mocks.cancel.mockReset().mockResolvedValue(undefined);
  mocks.status.mockReset().mockReturnValue({configured:false, relayState:'unconfigured'});
  useStore.setState({phoneAccessOpen:true});
  vi.spyOn(HTMLElement.prototype, 'offsetParent', 'get').mockImplementation(function(this: HTMLElement) { return this.parentElement; });
});
afterEach(() => {cleanup(); useStore.setState({phoneAccessOpen:false}); vi.restoreAllMocks();});

it('contains keyboard focus and Escape cancels pending pairing before restoring the opener', async () => {
  const opener = document.createElement('button');
  document.body.append(opener); opener.focus();
  render(<PhoneAccessPairPanelGate />);
  await waitFor(() => expect(mocks.start).toHaveBeenCalledOnce());
  const dialog = screen.getByRole('dialog');
  expect(dialog).toHaveFocus();
  const buttons = dialog.querySelectorAll<HTMLButtonElement>('button:not(:disabled)');
  buttons[buttons.length-1].focus();
  fireEvent.keyDown(document.activeElement!, {key:'Tab'});
  expect(buttons[0]).toHaveFocus();
  fireEvent.keyDown(document.activeElement!, {key:'Escape'});
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  expect(mocks.cancel).toHaveBeenCalledOnce();
  expect(opener).toHaveFocus(); opener.remove();
});

it('Escape dismisses an established connection without cancelling it', async () => {
  mocks.status.mockReturnValue({configured:true, relayState:'online'});
  render(<PhoneAccessPairPanelGate />);
  await waitFor(() => expect(mocks.start).toHaveBeenCalledOnce());
  fireEvent.keyDown(screen.getByRole('dialog'), {key:'Escape'});
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  expect(mocks.cancel).not.toHaveBeenCalled();
});

it('opens for a free account', async () => {
  useEntitlements.setState({ loaded: true, team: false, operator: false, account: true });
  useStore.setState({ phoneAccessOpen: true });
  render(<PhoneAccessPairPanelGate />);
  expect(await screen.findByRole('dialog')).toBeInTheDocument();
});

it('stays closed without an account (phone access needs one)', () => {
  useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
  useStore.setState({ phoneAccessOpen: true });
  render(<PhoneAccessPairPanelGate />);
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
});

it('tells the user the branded address to open, never the raw cloud host', async () => {
  useEntitlements.setState({ loaded: true, team: false, operator: false, account: true });
  render(<PhoneAccessPairPanelGate />);
  expect(await screen.findByText('https://app.soluterminal.com')).toBeInTheDocument();
  expect(screen.getByRole('dialog').textContent).not.toMatch(/onrender/);
});

it('names the phone button that really exists, and where it is', async () => {
  render(<PhoneAccessPairPanelGate />);
  const dialog = await screen.findByRole('dialog');
  await waitFor(() => expect(dialog.textContent).toContain(
    'Tap “+ Claim a new machine” (under “Other ways to connect” if you don’t see it) and enter this code.'));
  expect(dialog.textContent).not.toContain('Add a computer');
});
