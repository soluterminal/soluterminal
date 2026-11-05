import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { useStore } from '../../store';
import { useBackgroundService } from '../../lib/backgroundService';
import { DialogHost } from '../DialogHost';
import { BackgroundServiceSetting } from './BackgroundServiceSetting';

const { request } = vi.hoisted(() => ({ request: vi.fn() }));
vi.mock('../../api', async (original) => ({
  ...await original<typeof import('../../api')>(),
  backgroundServiceClient: () => request,
}));

beforeEach(() => {
  useBackgroundService.setState({ pending: false, message: '', error: false });
  request.mockReset().mockImplementation(async (url: string) => new Response(JSON.stringify(
    url.startsWith('/api/health')
      ? { pid: 42, daemonIdentity: 'dmn_test' }
      : { supported: true, pid: 41, daemonIdentity: 'dmn_test', heldDeliveries: 2, putBackReceipts: 1 },
  )));
});

afterEach(() => {
  act(() => useStore.getState().closeDialog(null));
  vi.restoreAllMocks();
});

it('does not restart when no dialog host is mounted', async () => {
  render(<BackgroundServiceSetting />);
  fireEvent.click(await screen.findByRole('button', { name: 'Restart background service' }));
  await waitFor(() => expect(useStore.getState().dialog).not.toBeNull());
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(request.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(0);
  act(() => useStore.getState().closeDialog(false));
  await waitFor(() => expect(useBackgroundService.getState().pending).toBe(false));
  expect(request.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(0);
});

it.each(['browser', 'async plugin', 'refused plugin'])('waits for an in-app choice before restarting (%s)', async (native) => {
  // The plugin changes the return type at runtime despite the DOM typings.
  const confirm = vi.spyOn(window, 'confirm').mockImplementation((() => {
    if (native === 'refused plugin') return Promise.reject(new Error('dialog not allowed'));
    return native === 'async plugin' ? Promise.resolve(false) : true;
  }) as typeof window.confirm);
  const posts = () => request.mock.calls.filter(([, init]) => init?.method === 'POST');
  render(<><BackgroundServiceSetting /><DialogHost /></>);
  fireEvent.click(await screen.findByRole('button', { name: 'Restart background service' }));
  expect(await screen.findByRole('dialog')).toHaveTextContent('Restart the background service? Panes keep running and reconnect. Changes to the service env file take effect. 2 held deliveries and 1 put-back receipts may be lost (counts at confirmation).');
  expect(posts()).toHaveLength(0);
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
  await waitFor(() => expect(useBackgroundService.getState()).toMatchObject({ pending: false, message: '', error: false }));
  expect(posts()).toHaveLength(0);

  fireEvent.click(screen.getByRole('button', { name: 'Restart background service' }));
  await screen.findByRole('dialog');
  expect(posts()).toHaveLength(0);
  fireEvent.click(screen.getByRole('button', { name: 'Confirm' }));
  await waitFor(() => expect(posts()).toHaveLength(1));
  expect(JSON.parse(posts()[0][1].body)).toEqual({ pid: 41 });
  await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Background service restarted. PID 41 → 42.'), { timeout: 3000 });
  expect(posts()).toHaveLength(1);
  expect(confirm).not.toHaveBeenCalled();
});
