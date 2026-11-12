import { StrictMode } from 'react';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { PhoneAccessPairPanelGate } from './PhoneAccessPairPanel';
import { useStore } from '../store';
import { useEntitlements } from '../entitlements';

const mocks = vi.hoisted(() => ({
  start: vi.fn(), cancel: vi.fn(), status: vi.fn(), disconnect: vi.fn(),
}));
vi.mock('../cloudPair', () => ({
  isDesktopShell: () => true,
  secondsUntil: () => 300,
  usePhoneAccessStatus: () => ({status:mocks.status()}),
  cloudPairApi: {pairStart:mocks.start, pairCancel:mocks.cancel, disconnect:mocks.disconnect},
}));

beforeEach(() => {
  useEntitlements.setState({ loaded: true, team: true, operator: true });
  mocks.start.mockReset().mockResolvedValue({code:'TEST12'});
  mocks.cancel.mockReset().mockResolvedValue(undefined);
  mocks.disconnect.mockReset().mockResolvedValue(undefined);
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

const refused = {configured:true, hasCreds:true, credsInvalid:true, relayState:'offline'};

it('offers Pair again as the fix for a refused pairing, and Disconnect as the off-switch', async () => {
  mocks.status.mockReturnValue(refused);
  render(<PhoneAccessPairPanelGate />);
  const dialog = await screen.findByRole('dialog');
  expect(dialog).toHaveTextContent('Sign-in needed');
  const text = dialog.textContent!.replace(/\s+/g, ' ');
  expect(text).toContain('This computer’s cloud connection is no longer valid: the sign-in expired, or this computer was removed from your account. Choose Pair again to get a new code, then enter it on your phone, signed in with the same account.');
  expect(text).toContain('If you don’t use the cloud on this computer, Disconnect stops these reminders. You can set it up again any time.');
  expect(screen.getByRole('button', {name:'Pair again'})).toHaveClass('primary');
  expect(screen.getByRole('button', {name:'Disconnect'})).toBeInTheDocument();
  expect(screen.getByRole('button', {name:'Close'})).toBeInTheDocument();
});

it('Pair again removes the refused pairing, then shows a new code without closing', async () => {
  mocks.status.mockReturnValue(refused);
  const order: string[] = [];
  mocks.disconnect.mockImplementation(async () => { order.push('disconnect'); });
  mocks.start.mockImplementation(async () => { order.push('start'); return {code: order.length > 1 ? 'NEW456' : 'OLD123'}; });
  render(<PhoneAccessPairPanelGate />);
  await waitFor(() => expect(mocks.start).toHaveBeenCalledOnce());
  // The code fetched on open is not shown while the pairing is refused.
  expect(screen.queryByText('OLD123')).not.toBeInTheDocument();

  fireEvent.click(screen.getByRole('button', {name:'Pair again'}));
  expect(await screen.findByText('NEW456')).toBeInTheDocument();
  // The old pairing goes first: Disconnect also cancels a pending code, and
  // the start after it is what records that the person wants to be connected.
  expect(order).toEqual(['start', 'disconnect', 'start']);
  const dialog = screen.getByRole('dialog');
  expect(dialog).toHaveTextContent('Set up phone access');
  expect(dialog).not.toHaveTextContent('Sign-in needed');
  expect(useStore.getState().phoneAccessOpen).toBe(true);
});

it('Pair again works under React StrictMode, where the panel is mounted twice in development', async () => {
  mocks.status.mockReturnValue(refused);
  let starts = 0;
  mocks.start.mockImplementation(async () => ({code: `CODE0${++starts}`}));
  render(<StrictMode><PhoneAccessPairPanelGate /></StrictMode>);
  await waitFor(() => expect(mocks.start).toHaveBeenCalled());
  const before = starts;
  fireEvent.click(screen.getByRole('button', {name:'Pair again'}));
  await waitFor(() => expect(starts).toBe(before + 1));
  expect(await screen.findByText(`CODE0${before + 1}`)).toBeInTheDocument();
  expect(screen.getByRole('dialog')).toHaveTextContent('Set up phone access');
});

it('Close on the sign-in screen cancels the code fetched when the panel opened', async () => {
  mocks.status.mockReturnValue(refused);
  render(<PhoneAccessPairPanelGate />);
  await waitFor(() => expect(mocks.start).toHaveBeenCalledOnce());
  fireEvent.click(screen.getByRole('button', {name:'Close'}));
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  expect(mocks.cancel).toHaveBeenCalledOnce();
  expect(mocks.disconnect).not.toHaveBeenCalled();
});

it('Pair again says so and stays on the sign-in screen when the old pairing cannot be removed', async () => {
  mocks.status.mockReturnValue(refused);
  mocks.disconnect.mockRejectedValue(Object.assign(new Error('boom'), {status:500}));
  const realToast = useStore.getState().toast;
  const toast = vi.fn();
  useStore.setState({toast} as never);
  try {
    render(<PhoneAccessPairPanelGate />);
    await waitFor(() => expect(mocks.start).toHaveBeenCalledOnce());
    fireEvent.click(screen.getByRole('button', {name:'Pair again'}));
    await waitFor(() => expect(toast).toHaveBeenCalledWith('Couldn’t disconnect: boom', expect.anything()));
    expect(mocks.start).toHaveBeenCalledOnce();
    expect(screen.getByRole('dialog')).toHaveTextContent('Sign-in needed');
    expect(screen.getByRole('button', {name:'Pair again'})).toBeEnabled();
  } finally {
    useStore.setState({toast: realToast});
  }
});

it('Pair again starts no unseen pairing when the panel is closed while the old pairing is being removed', async () => {
  mocks.status.mockReturnValue(refused);
  let finishDisconnect!: () => void;
  mocks.disconnect.mockReturnValue(new Promise<void>((resolve) => { finishDisconnect = resolve; }));
  const realToast = useStore.getState().toast;
  const toast = vi.fn();
  useStore.setState({toast} as never);
  try {
    render(<PhoneAccessPairPanelGate />);
    await waitFor(() => expect(mocks.start).toHaveBeenCalledOnce());
    fireEvent.click(screen.getByRole('button', {name:'Pair again'}));
    expect(screen.getByRole('button', {name:'Getting a new code…'})).toBeDisabled();
    fireEvent.click(screen.getByRole('button', {name:'Close'}));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());

    finishDisconnect();
    await waitFor(() => expect(toast).toHaveBeenCalledWith(
      'Cloud disconnected — this computer no longer connects to the cloud.', expect.anything()));
    // Only the start from opening the panel; none after the dismissal.
    expect(mocks.start).toHaveBeenCalledOnce();
  } finally {
    useStore.setState({toast: realToast});
  }
});

it('Pair again cancels the new code when the panel is closed while the code is on its way', async () => {
  mocks.status.mockReturnValue(refused);
  render(<PhoneAccessPairPanelGate />);
  await waitFor(() => expect(mocks.start).toHaveBeenCalledOnce());
  let finishStart!: (v: {code: string}) => void;
  mocks.start.mockReturnValue(new Promise((resolve) => { finishStart = resolve; }));
  fireEvent.click(screen.getByRole('button', {name:'Pair again'}));
  await waitFor(() => expect(mocks.start).toHaveBeenCalledTimes(2));
  // The panel is already on the pairing screen ("Getting a code…"), whose
  // button is Cancel. That cancel can reach the daemon before the start does.
  fireEvent.click(screen.getByRole('button', {name:'Cancel'}));
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  expect(mocks.cancel).toHaveBeenCalledOnce();

  // So the late code is cancelled again once it arrives.
  finishStart({code:'LATE99'});
  await waitFor(() => expect(mocks.cancel).toHaveBeenCalledTimes(2));
});

it('Disconnect stays an off-switch: it closes the panel and starts no new pairing', async () => {
  mocks.status.mockReturnValue(refused);
  render(<PhoneAccessPairPanelGate />);
  await waitFor(() => expect(mocks.start).toHaveBeenCalledOnce());
  fireEvent.click(screen.getByRole('button', {name:'Disconnect'}));
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  expect(mocks.disconnect).toHaveBeenCalledOnce();
  expect(mocks.start).toHaveBeenCalledOnce();
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
