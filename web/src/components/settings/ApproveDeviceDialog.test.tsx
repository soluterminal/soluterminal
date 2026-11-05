import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, j } from '../../api';
import type { RemoteDevice, RemoteDevicesState } from '../../api';
import { DEVICE_NAME_MAX } from '../../lib/deviceClaimedName';
import {
  ApproveDeviceDialog,
  CODES_DIFFER_REMOVE,
  CODES_MATCH_APPROVE,
  NO_CODE_CONFIRM,
  NO_CODE_WARNING,
  RECOVERY_BANNER,
  REQUEST_CHANGED,
} from './ApproveDeviceDialog';

afterEach(() => vi.restoreAllMocks());

const v3: RemoteDevice = {
  id: 'dev_1', name: 'Chrome on Android', status: 'pending', code: 'GJ23-QFPG-G9PE-ASX5', codeVersion: 3,
  enrolmentId: 'enr_AAECAwQFBgcICQoLDA0ODw', createdAt: '2026-11-04T01:02:03Z', surface: 'relay',
};

function open(device: RemoteDevice, opts: { recovery?: boolean; reload?: () => Promise<RemoteDevicesState | null> } = {}) {
  const reload = vi.fn(opts.reload ?? (async () => ({ policy: 'approved' as const, devices: [device], canManageDevices: true })));
  const onClose = vi.fn();
  const view = render(<ApproveDeviceDialog device={device} recovery={!!opts.recovery} reload={reload} onClose={onClose} />);
  return { reload, onClose, view };
}

describe('ApproveDeviceDialog', () => {
  it('says what the approval grants next to the V3 code', () => {
    const { view } = open(v3);
    expect(screen.getByTestId('approve-device-scope')).toHaveTextContent('Full access');
    view.unmount();
    open({ ...v3, scope: 'alerts' });
    expect(screen.getByTestId('approve-device-scope')).toHaveTextContent('Alerts only: can take over push alerts, nothing else');
  });

  it('shows the V3 code large, labelled V3, with the request time and where it came from', () => {
    open(v3);
    expect(screen.getByTestId('approve-device-code')).toHaveTextContent('GJ23-QFPG-G9PE-ASX5');
    expect(screen.getByText('V3')).toBeInTheDocument();
    expect(screen.getByText('Name the device gave')).toBeInTheDocument();
    expect(screen.getByText(new Date(v3.createdAt).toLocaleString())).toBeInTheDocument();
    expect(screen.getByText('through the relay')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: CODES_MATCH_APPROVE })).toBeEnabled();
    expect(screen.getByRole('button', { name: CODES_DIFFER_REMOVE })).toBeEnabled();
    expect(screen.queryByText(RECOVERY_BANNER)).toBeNull();
  });

  it('sends the enrolment that was displayed', async () => {
    const approve = vi.spyOn(api, 'approveRemoteDevice').mockResolvedValue({ ...v3, status: 'approved' });
    const { onClose } = open(v3);
    fireEvent.click(screen.getByRole('button', { name: CODES_MATCH_APPROVE }));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(approve).toHaveBeenCalledTimes(1);
    expect(approve).toHaveBeenCalledWith(v3.id, v3.enrolmentId);
  });

  it('approves nothing on open, on focus or on Enter; default focus is not on Approve', async () => {
    const approve = vi.spyOn(api, 'approveRemoteDevice').mockResolvedValue({ ...v3, status: 'approved' });
    open(v3);
    const button = screen.getByRole('button', { name: CODES_MATCH_APPROVE });
    expect(document.activeElement).not.toBe(button);
    expect(document.activeElement).toBe(screen.getByRole('dialog'));
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Enter' });
    act(() => button.focus());
    // A browser turns Enter on a focused button into a click unless the
    // keydown is cancelled: the dialog cancels it.
    expect(fireEvent.keyDown(button, { key: 'Enter' })).toBe(false);
    await act(async () => { await Promise.resolve(); });
    expect(approve).not.toHaveBeenCalled();
  });

  it('removes on "Codes differ: remove" and never approves', async () => {
    const remove = vi.spyOn(api, 'removeRemoteDevice').mockResolvedValue();
    const approve = vi.spyOn(api, 'approveRemoteDevice');
    const { onClose } = open(v3);
    fireEvent.click(screen.getByRole('button', { name: CODES_DIFFER_REMOVE }));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(remove).toHaveBeenCalledWith(v3.id);
    expect(approve).not.toHaveBeenCalled();
  });

  it.each([
    ['a V2 request', { ...v3, codeVersion: 2, enrolmentId: undefined, code: 'OLDV2-CODE' }],
    ['a V3 request with no revealed code', { ...v3, code: undefined }],
  ])('warns there is no code for %s and approves only after a second, explicit confirmation', async (_, device) => {
    const approve = vi.spyOn(api, 'approveRemoteDevice').mockResolvedValue({ ...device, status: 'approved' });
    open(device as RemoteDevice);
    expect(screen.getByRole('alert')).toHaveTextContent(NO_CODE_WARNING);
    expect(screen.queryByTestId('approve-device-code')).toBeNull();
    const button = screen.getByRole('button', { name: CODES_MATCH_APPROVE });
    expect(button).toBeDisabled();
    fireEvent.click(button);
    expect(approve).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('checkbox', { name: NO_CODE_CONFIRM }));
    expect(button).toBeEnabled();
    fireEvent.click(button);
    await waitFor(() => expect(approve).toHaveBeenCalledTimes(1));
  });

  it('on 409 re-reads and shows the new request without retrying', async () => {
    const changed = { ...v3, code: 'NEW2-CODE-NEW2-CODE', enrolmentId: 'enr_BBECAwQFBgcICQoLDA0ODw' };
    const approve = vi.spyOn(api, 'approveRemoteDevice').mockImplementation(() =>
      j<never>(new Response('device enrolment changed', { status: 409 })));
    const { reload } = open(v3, { reload: async () => ({ policy: 'approved', devices: [changed], canManageDevices: true }) });
    fireEvent.click(screen.getByRole('button', { name: CODES_MATCH_APPROVE }));
    expect(await screen.findByText(REQUEST_CHANGED)).toBeInTheDocument();
    expect(reload).toHaveBeenCalledTimes(1);
    expect(screen.getByTestId('approve-device-code')).toHaveTextContent('NEW2-CODE-NEW2-CODE');
    expect(approve).toHaveBeenCalledTimes(1);
    // Approving the new request is a new, deliberate click with its own enrolment.
    fireEvent.click(screen.getByRole('button', { name: CODES_MATCH_APPROVE }));
    await waitFor(() => expect(approve).toHaveBeenCalledTimes(2));
    expect(approve).toHaveBeenLastCalledWith(changed.id, changed.enrolmentId);
  });

  it('shows a device_required refusal and does not retry', async () => {
    const message = "This screen isn't an approved device yet, so it can't approve a device. Open Settings → Privacy & security → Ask to approve this browser.";
    const approve = vi.spyOn(api, 'approveRemoteDevice').mockImplementation(() =>
      j<never>(new Response(JSON.stringify({ error: 'device_required', message }), { status: 403 })));
    const { onClose } = open(v3);
    fireEvent.click(screen.getByRole('button', { name: CODES_MATCH_APPROVE }));
    expect(await screen.findByRole('alert')).toHaveTextContent(message);
    await act(async () => { await new Promise((r) => setTimeout(r, 20)); });
    expect(approve).toHaveBeenCalledTimes(1);
    expect(onClose).not.toHaveBeenCalled();
  });

  it('renders a long or odd device name as inert, capped text', () => {
    const odd = '<img src=x onerror="window.__pwned=1"> ‮gnp.exe‬' + 'A'.repeat(200) + '\u0007';
    open({ ...v3, name: odd });
    const shown = screen.getByTestId('approve-device-name');
    expect(shown.querySelector('img')).toBeNull();
    expect(document.querySelector('img[src="x"]')).toBeNull();
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined();
    const text = shown.textContent ?? '';
    expect(text.startsWith('<img src=x')).toBe(true);
    expect(text).not.toMatch(/[‪-‮⁦-⁩\u0000-\u001f]/);
    expect(Array.from(text).length).toBeLessThanOrEqual(DEVICE_NAME_MAX);
    expect(text.endsWith('…')).toBe(true);
  });

  it('shows the recovery banner while SOLU_HUMAN_PROOF=off', () => {
    open(v3, { recovery: true });
    expect(screen.getByText(RECOVERY_BANNER)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /recovery/i })).toBeNull();
  });

  it('is drawn on the page body, never inside a pane', () => {
    const pane = document.createElement('div');
    pane.className = 'pane';
    document.body.appendChild(pane);
    render(<ApproveDeviceDialog device={v3} recovery={false} reload={async () => null} onClose={() => {}} />, { container: pane });
    const dialog = screen.getByRole('dialog');
    expect(pane.contains(dialog)).toBe(false);
    expect(dialog.closest('.pane')).toBeNull();
    pane.remove();
  });
});
