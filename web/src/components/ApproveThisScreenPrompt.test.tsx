import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { LocalProofStatus } from '../lib/localProof';

const proof: { status: LocalProofStatus | null } = { status: null };
const reload = vi.fn(async () => {});

vi.mock('../lib/useLocalProof', () => ({
  useLocalProof: () => ({ status: proof.status, backendPrefix: '', reload }),
}));
vi.mock('../lib/localProof', async (orig) => ({
  ...(await orig<typeof import('../lib/localProof')>()),
  localProofStatus: () => proof.status,
  askToApproveThisBrowser: vi.fn(async () => {
    // As the real one does: the daemon now reports this browser waiting.
    proof.status = { ...proof.status!, device: 'pending' };
    return 'LOCA-LV3C-ODE1-2345';
  }),
}));

import { api } from '../api';
import { askToApproveThisBrowser } from '../lib/localProof';
import { APPROVE_SCREEN_ASK, APPROVE_SCREEN_TITLE, ApproveThisScreenPrompt, openApproveThisScreenPrompt } from './ApproveThisScreenPrompt';

const base: LocalProofStatus = {
  mode: 'report', effective: 'report', target: 'local/dmn_t', serverTime: Date.now(), envOff: false,
  approvedDevices: 1, canBootstrap: false, device: '',
};

beforeEach(() => {
  proof.status = { ...base };
  sessionStorage.clear();
});
afterEach(() => vi.clearAllMocks());

describe('ApproveThisScreenPrompt', () => {
  it('appears on a screen that is not approved while another device is, and offers only to ask', () => {
    render(<ApproveThisScreenPrompt />);
    const region = screen.getByRole('region', { name: APPROVE_SCREEN_TITLE });
    const buttons = Array.from(region.querySelectorAll('button')).map((b) => b.textContent);
    expect(buttons).toEqual(['Not now', APPROVE_SCREEN_ASK]);
    // Never a recovery switch; the help page's recovery section is linked.
    expect(screen.queryByRole('button', { name: /recover|SOLU_HUMAN_PROOF/i })).toBeNull();
    expect(screen.getByRole('link', { name: 'Locked out?' })).toHaveAttribute('href', 'https://soluterminal.com/help/fix-a-problem/#lost-devices');
    // It says where to approve, including the computer itself outside require.
    expect(region).toHaveTextContent('a browser or app you’ve already approved');
    expect(region).toHaveTextContent('on the computer itself');
  });

  it('is not modal: no backdrop, no dialog', () => {
    render(<ApproveThisScreenPrompt />);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.querySelector('.dialog-backdrop')).toBeNull();
  });

  it('leaves the first-device path alone when no device is approved', () => {
    proof.status = { ...base, approvedDevices: 0, canBootstrap: true };
    render(<ApproveThisScreenPrompt />);
    expect(screen.queryByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeNull();
  });

  it('is not shown on an approved screen or for a remote view', () => {
    proof.status = { ...base, device: 'approved' };
    const { rerender } = render(<ApproveThisScreenPrompt />);
    expect(screen.queryByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeNull();
    proof.status = null;
    rerender(<ApproveThisScreenPrompt />);
    expect(screen.queryByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeNull();
  });

  it('enrols and shows the code this screen computed, and approves nothing', async () => {
    const approve = vi.spyOn(api, 'approveRemoteDevice');
    render(<ApproveThisScreenPrompt />);
    fireEvent.click(screen.getByRole('button', { name: APPROVE_SCREEN_ASK }));
    expect(await screen.findByTestId('approve-screen-code')).toHaveTextContent('LOCA-LV3C-ODE1-2345');
    expect(screen.getByText('V3')).toBeInTheDocument();
    expect(askToApproveThisBrowser).toHaveBeenCalledWith('');
    expect(approve).not.toHaveBeenCalled();
  });

  it('drops the code once its request is no longer the one waiting', async () => {
    const { rerender } = render(<ApproveThisScreenPrompt />);
    fireEvent.click(screen.getByRole('button', { name: APPROVE_SCREEN_ASK }));
    expect(await screen.findByTestId('approve-screen-code')).toBeInTheDocument();
    // Removed on the approving device: this browser has no request now.
    proof.status = { ...base, device: '' };
    rerender(<ApproveThisScreenPrompt />);
    await waitFor(() => expect(screen.queryByTestId('approve-screen-code')).toBeNull());
    expect(screen.getByRole('button', { name: APPROVE_SCREEN_ASK })).toBeInTheDocument();
    // Waiting again (a new request) does not bring the old code back.
    proof.status = { ...base, device: 'pending' };
    rerender(<ApproveThisScreenPrompt />);
    expect(screen.queryByTestId('approve-screen-code')).toBeNull();
  });

  it('drops the code when another computer is in view', async () => {
    const { rerender } = render(<ApproveThisScreenPrompt />);
    fireEvent.click(screen.getByRole('button', { name: APPROVE_SCREEN_ASK }));
    expect(await screen.findByTestId('approve-screen-code')).toBeInTheDocument();
    proof.status = { ...base, target: 'local/dmn_other', device: 'pending' };
    rerender(<ApproveThisScreenPrompt />);
    await waitFor(() => expect(screen.queryByTestId('approve-screen-code')).toBeNull());
  });

  it('ignores a late answer for a computer no longer in view', async () => {
    let finish: (code: string) => void = () => {};
    vi.mocked(askToApproveThisBrowser).mockImplementationOnce(() => new Promise<string>((resolve) => { finish = resolve; }));
    const { rerender } = render(<ApproveThisScreenPrompt />);
    fireEvent.click(screen.getByRole('button', { name: APPROVE_SCREEN_ASK }));
    proof.status = { ...base, target: 'local/dmn_other', device: 'pending' };
    rerender(<ApproveThisScreenPrompt />);
    await act(async () => { finish('LATE-CODE-LATE-CODE'); await Promise.resolve(); });
    expect(screen.queryByText('LATE-CODE-LATE-CODE')).toBeNull();
  });

  it('under require names only approved devices as approvers', () => {
    proof.status = { ...base, mode: 'require', effective: 'require' };
    render(<ApproveThisScreenPrompt />);
    expect(screen.getByRole('region', { name: APPROVE_SCREEN_TITLE })).not.toHaveTextContent('computer itself');
  });

  it('stays away for the session once put away, until the standing changes', async () => {
    const first = render(<ApproveThisScreenPrompt />);
    fireEvent.click(screen.getByRole('button', { name: 'Not now' }));
    expect(screen.queryByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeNull();
    first.unmount();
    // A reload in the same session.
    const again = render(<ApproveThisScreenPrompt />);
    expect(screen.queryByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeNull();
    again.unmount();
    // This browser's request ended: the standing changed.
    proof.status = { ...base, device: 'failed' };
    render(<ApproveThisScreenPrompt />);
    await waitFor(() => expect(screen.getByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeInTheDocument());
  });

  it('while waiting offers no second request', () => {
    proof.status = { ...base, device: 'pending' };
    render(<ApproveThisScreenPrompt />);
    expect(screen.queryByRole('button', { name: APPROVE_SCREEN_ASK })).toBeNull();
    expect(screen.getByRole('region', { name: APPROVE_SCREEN_TITLE })).toHaveTextContent('waiting for approval');
  });

  // ST-54: the claim chip's "Approve this screen" opens it on purpose.
  describe('opened on purpose', () => {
    it('shows at 0 approved devices and still only offers to ask', () => {
      proof.status = { ...base, approvedDevices: 0, canBootstrap: true };
      render(<ApproveThisScreenPrompt />);
      expect(screen.queryByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeNull();
      act(() => { expect(openApproveThisScreenPrompt()).toBe(true); });
      const region = screen.getByRole('region', { name: APPROVE_SCREEN_TITLE });
      const buttons = Array.from(region.querySelectorAll('button')).map((b) => b.textContent);
      expect(buttons).toEqual(['Not now', APPROVE_SCREEN_ASK]);
      fireEvent.click(screen.getByRole('button', { name: 'Not now' }));
      expect(screen.queryByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeNull();
    });

    it('comes back after Not now', () => {
      render(<ApproveThisScreenPrompt />);
      fireEvent.click(screen.getByRole('button', { name: 'Not now' }));
      expect(screen.queryByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeNull();
      act(() => { openApproveThisScreenPrompt(); });
      expect(screen.getByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: 'Not now' }));
    });

    it('has nothing to offer an approved screen', () => {
      proof.status = { ...base, device: 'approved' };
      expect(openApproveThisScreenPrompt()).toBe(false);
    });
  });
});
