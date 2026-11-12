import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { Session } from '@supabase/supabase-js';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { useCloud } from '../cloud';
import { useEntitlements } from '../entitlements';
import { SmartApprovalsHint, resetSmartApprovalsCacheForTests, SMART_APPROVALS_CONNECT, SMART_APPROVALS_CONNECTED, SMART_APPROVALS_USED_UP } from './SmartApprovalsHint';
import { CONNECT_PROMPT_TITLE } from '../accountConnect';
import { useStore } from '../store';

function keyStatus(configured: boolean) {
  return {
    runtime: 'typesafe-api',
    secrets: [{ env: 'TYPESAFE_API_KEY', required: true, configured }],
  };
}

beforeEach(() => {
  resetSmartApprovalsCacheForTests();
  // Older-daemon answer by default; the tests below override it.
  vi.spyOn(api, 'smartApprovalsStatus').mockResolvedValue(null);
  useCloud.setState({
    mode: 'cloud',
    isLocal: true,
    accountOptional: true,
    withoutAccount: true,
    session: null,
  });
  useEntitlements.setState({ loaded: true, team: false, operator: false, account: false, smartApprovals: null });
});

afterEach(() => {
  vi.restoreAllMocks();
  useCloud.setState({ withoutAccount: false, session: null, mode: 'detecting', isLocal: false, accountOptional: false });
  useEntitlements.setState({ loaded: false, team: false, operator: false, account: false, smartApprovals: null });
});

describe('SmartApprovalsHint', () => {
  it('no account and no local key: offers a free sign-in', async () => {
    vi.spyOn(api, 'workflowRuntimeSecrets').mockResolvedValue(keyStatus(false));
    const requestSignIn = vi.fn();
    useCloud.setState({ requestSignIn });
    render(<SmartApprovalsHint />);
    fireEvent.click(await screen.findByRole('menuitem', { name: 'Sign in (free) to use smart approvals' }));
    expect(requestSignIn).toHaveBeenCalled();
  });

  it('free account and no local key: names the account and its daily allowance', async () => {
    vi.spyOn(api, 'workflowRuntimeSecrets').mockResolvedValue(keyStatus(false));
    useCloud.setState({ withoutAccount: false, session: { access_token: 't', user: { id: 'u' } } as unknown as Session });
    render(<SmartApprovalsHint />);
    expect(await screen.findByText('Smart approvals: via your free account (100/day)')).toBeInTheDocument();
  });

  it('team install: no free-tier allowance in the copy', async () => {
    vi.spyOn(api, 'workflowRuntimeSecrets').mockResolvedValue(keyStatus(false));
    useEntitlements.setState({ loaded: true, team: true, operator: true, account: true });
    render(<SmartApprovalsHint />);
    expect(await screen.findByText('Smart approvals: via your account')).toBeInTheDocument();
  });

  it('local Jev key: says nothing', async () => {
    const spy = vi.spyOn(api, 'workflowRuntimeSecrets').mockResolvedValue(keyStatus(true));
    const { container } = render(<SmartApprovalsHint />);
    await waitFor(() => expect(spy).toHaveBeenCalled());
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(container).toBeEmptyDOMElement();
  });

  it('cloud smart approvals switched off: no account claim', async () => {
    vi.spyOn(api, 'workflowRuntimeSecrets').mockResolvedValue(keyStatus(false));
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: true, smartApprovals: false });
    useCloud.setState({ withoutAccount: false });
    const { container } = render(<SmartApprovalsHint />);
    await waitFor(() => expect(api.workflowRuntimeSecrets).toHaveBeenCalled());
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(container).toBeEmptyDOMElement();
  });

  const signedIn = () =>
    useCloud.setState({ withoutAccount: false, session: { access_token: 't', user: { id: 'u' } } as unknown as Session });

  it('free account: shows what is left of today', async () => {
    vi.spyOn(api, 'workflowRuntimeSecrets').mockResolvedValue(keyStatus(false));
    vi.spyOn(api, 'smartApprovalsStatus').mockResolvedValue({ source: 'cloud', remaining: 87, limit: 100 });
    signedIn();
    render(<SmartApprovalsHint />);
    expect(await screen.findByText('Smart approvals: 87 of 100 left today')).toBeInTheDocument();
  });

  it('free account at its daily limit: says so', async () => {
    vi.spyOn(api, 'workflowRuntimeSecrets').mockResolvedValue(keyStatus(false));
    vi.spyOn(api, 'smartApprovalsStatus').mockResolvedValue({ source: 'cloud', paused: 'cloud_quota' });
    signedIn();
    render(<SmartApprovalsHint />);
    expect(await screen.findByText(SMART_APPROVALS_USED_UP)).toBeInTheDocument();
  });

  // ST-208: the connect warning first; only Connect this PC connects.
  it('signed in but not connected: warns first, connects only on Connect this PC', async () => {
    vi.spyOn(api, 'workflowRuntimeSecrets').mockResolvedValue(keyStatus(false));
    vi.spyOn(api, 'smartApprovalsStatus').mockResolvedValue({ source: 'none' });
    const bodies: unknown[] = [];
    const answers = [{ status: 'available' }, { status: 'connecting' }];
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (_url, init) => {
      bodies.push(JSON.parse(String((init as RequestInit).body)));
      return new Response(JSON.stringify(answers.shift() ?? { status: 'error' }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    });
    let choice = false;
    const opened: Array<{ title?: string; confirmLabel?: string; focusCancel?: boolean }> = [];
    const prev = useStore.getState().openDialog;
    useStore.setState({ openDialog: ((req: { title?: string; confirmLabel?: string; focusCancel?: boolean; resolve: (v: unknown) => void }) => {
      opened.push(req);
      req.resolve(choice);
    }) as never });
    try {
      signedIn();
      render(<SmartApprovalsHint />);
      const item = await screen.findByRole('menuitem', { name: SMART_APPROVALS_CONNECT });
      fireEvent.click(item);
      await waitFor(() => expect(opened).toHaveLength(1));
      expect(opened[0]).toMatchObject({ title: CONNECT_PROMPT_TITLE, confirmLabel: 'Connect this PC', focusCancel: true });
      expect(bodies).toEqual([{ force: false, check: true }]);

      choice = true;
      answers.unshift({ status: 'available' });
      fireEvent.click(item);
      await waitFor(() => expect(bodies).toEqual([{ force: false, check: true }, { force: false, check: true }, { force: true }]));
      await waitFor(() => expect(useStore.getState().toasts.at(-1)?.message).toBe(SMART_APPROVALS_CONNECTED));
    } finally {
      useStore.setState({ openDialog: prev });
    }
  });

  it('daemon reports a local key: says nothing', async () => {
    vi.spyOn(api, 'workflowRuntimeSecrets').mockResolvedValue(keyStatus(false));
    vi.spyOn(api, 'smartApprovalsStatus').mockResolvedValue({ source: 'local' });
    signedIn();
    const { container } = render(<SmartApprovalsHint />);
    await waitFor(() => expect(api.smartApprovalsStatus).toHaveBeenCalled());
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(container).toBeEmptyDOMElement();
  });
});
