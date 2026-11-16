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

vi.mock('../embed', async (orig) => ({
  ...(await orig<typeof import('../embed')>()),
  sendEmbedScreenApproval: vi.fn(),
}));

import { api } from '../api';
import { sendEmbedScreenApproval } from '../embed';
import { useStore } from '../store';
import { askToApproveThisBrowser } from '../lib/localProof';
import {
  APPROVE_SCREEN_ASK, APPROVE_SCREEN_TITLE, ApproveThisScreenPrompt, HOST_APPROVE_SCREEN_GAP_MS, __resetHostApproveScreenForTests,
  openApproveThisScreenPrompt, screenApprovalState, whereToApprove,
} from './ApproveThisScreenPrompt';

const base: LocalProofStatus = {
  mode: 'report', effective: 'report', target: 'local/dmn_t', serverTime: Date.now(), envOff: false,
  approvedDevices: 1, canBootstrap: false, device: '',
};

beforeEach(() => {
  proof.status = { ...base };
  sessionStorage.clear();
  __resetHostApproveScreenForTests();
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

// ST-248: what an embedding host hears, and what it may ask for.
describe('embed approval events', () => {
  it('maps the standing to none, pending or approved', () => {
    expect(screenApprovalState({ device: 'approved' })).toBe('approved');
    expect(screenApprovalState({ device: 'pending' })).toBe('pending');
    expect(screenApprovalState({ device: 'committed' })).toBe('pending');
    expect(screenApprovalState({ device: 'failed' })).toBe('none');
    expect(screenApprovalState({ device: '' })).toBe('none');
    expect(screenApprovalState({})).toBe('none');
  });

  it('reports the standing when known and when it changes, never a code', async () => {
    const view = render(<ApproveThisScreenPrompt />);
    expect(sendEmbedScreenApproval).toHaveBeenLastCalledWith('none');
    fireEvent.click(screen.getByRole('button', { name: APPROVE_SCREEN_ASK }));
    await waitFor(() => expect(askToApproveThisBrowser).toHaveBeenCalled());
    view.rerender(<ApproveThisScreenPrompt />);
    await waitFor(() => expect(sendEmbedScreenApproval).toHaveBeenLastCalledWith('pending'));
    for (const [arg] of vi.mocked(sendEmbedScreenApproval).mock.calls) expect(['none', 'pending', 'approved']).toContain(arg);
  });

  it('sends nothing while the standing is unknown', () => {
    proof.status = null;
    render(<ApproveThisScreenPrompt />);
    expect(sendEmbedScreenApproval).not.toHaveBeenCalled();
  });

  it("opens the prompt on the host's approve-screen, even at 0 approved devices", () => {
    proof.status = { ...base, approvedDevices: 0, canBootstrap: true };
    render(<ApproveThisScreenPrompt />);
    expect(screen.queryByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeNull();
    act(() => { window.dispatchEvent(new Event('solu:embed-approve-screen')); });
    const region = screen.getByRole('region', { name: APPROVE_SCREEN_TITLE });
    // Nothing is approved yet, so it never sends the person to an approved device.
    expect(region).toHaveTextContent('open SoluTerminal in a web browser on this computer');
    expect(region).not.toHaveTextContent('already approved');
    fireEvent.click(screen.getByRole('button', { name: 'Not now' }));
  });

  it('says so, rather than nothing, when there is no prompt to open', () => {
    const toast = vi.fn();
    const prior = useStore.getState().toast;
    useStore.setState({ toast });
    try {
      proof.status = { ...base, device: 'approved' };
      render(<ApproveThisScreenPrompt />);
      act(() => { window.dispatchEvent(new Event('solu:embed-approve-screen')); });
      expect(toast).toHaveBeenLastCalledWith(expect.stringContaining('already approved'), { kind: 'info' });
      proof.status = null;
      __resetHostApproveScreenForTests();
      act(() => { window.dispatchEvent(new Event('solu:embed-approve-screen')); });
      expect(toast).toHaveBeenLastCalledWith(expect.stringContaining("can't ask for approval right now"), { kind: 'warn' });
    } finally {
      useStore.setState({ toast: prior });
    }
  });

  // @secure: never while the prompt is open, and at most once per 10 s.
  it('ignores approve-screen while the prompt is open, and more than once per 10 s', () => {
    const toast = vi.fn();
    const prior = useStore.getState().toast;
    useStore.setState({ toast });
    const now = vi.spyOn(Date, 'now').mockReturnValue(1_000_000);
    try {
      proof.status = { ...base, approvedDevices: 0, canBootstrap: true };
      render(<ApproveThisScreenPrompt />);
      const ask = () => act(() => { window.dispatchEvent(new Event('solu:embed-approve-screen')); });
      ask();
      expect(screen.getByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeInTheDocument();
      // Open: a second ask, even after the gap, does nothing.
      now.mockReturnValue(1_000_000 + HOST_APPROVE_SCREEN_GAP_MS + 1);
      ask();
      fireEvent.click(screen.getByRole('button', { name: 'Not now' }));
      expect(screen.queryByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeNull();
      // Put away: the next ask opens it again, then one inside the gap doesn't.
      ask();
      expect(screen.getByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: 'Not now' }));
      now.mockReturnValue(1_000_000 + 2 * HOST_APPROVE_SCREEN_GAP_MS);
      ask();
      expect(screen.queryByRole('region', { name: APPROVE_SCREEN_TITLE })).toBeNull();
      expect(toast).not.toHaveBeenCalled();
    } finally {
      useStore.setState({ toast: prior });
    }
  });

  it('keeps the require rule: only approved devices, whatever the count', () => {
    expect(whereToApprove({ effective: 'require', approvedDevices: 0 })).toContain('already approved');
    expect(whereToApprove({ effective: 'report', approvedDevices: 2 })).toContain('on the computer itself');
  });
});
