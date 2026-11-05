import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest';

const { request } = vi.hoisted(() => ({ request: vi.fn() }));
vi.mock('../api', () => ({ backgroundServiceClient: () => request }));
import { restartBackgroundService, useBackgroundService } from './backgroundService';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

beforeEach(() => {
  vi.useFakeTimers();
  request.mockReset();
  useBackgroundService.setState({ pending: false, message: '', error: false });
  vi.spyOn(window, 'confirm').mockReturnValue(true);
});
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });

describe('background service restart', () => {
  it('sends one restart, ignores the old PID and confirms a new PID', async () => {
    request.mockResolvedValueOnce(json({ supported: true, pid: 41, daemonIdentity: 'dmn_test', heldDeliveries: 2, putBackReceipts: 1 }))
      .mockResolvedValueOnce(json({ accepted: true }))
      .mockResolvedValueOnce(json({ pid: 41, daemonIdentity: 'dmn_test' }))
      .mockRejectedValueOnce(new Error('offline'))
      .mockResolvedValueOnce(json({ pid: 99, daemonIdentity: 'different_daemon' }))
      .mockResolvedValueOnce(json({ pid: 42, daemonIdentity: 'dmn_test' }));
    const result = restartBackgroundService();
    await vi.runAllTimersAsync();
    await result;
    expect(request.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1);
    expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('2 held deliveries and 1 put-back receipts'));
    expect(useBackgroundService.getState()).toMatchObject({ pending: false, error: false, message: 'Background service restarted. PID 41 → 42.' });
  });
  it('does not replay an uncertain mutation or treat it as success', async () => {
    request.mockResolvedValueOnce(json({ supported: true, pid: 41, daemonIdentity: 'dmn_test', heldDeliveries: 2, putBackReceipts: 1 })).mockRejectedValueOnce(new Error('connection lost'));
    await restartBackgroundService();
    expect(request).toHaveBeenCalledTimes(2);
    expect(useBackgroundService.getState().error).toBe(true);
    expect(useBackgroundService.getState().message).toContain('No automatic retry');
  });
  it('requires confirmation and refuses unsupported installs', async () => {
    request.mockResolvedValueOnce(json({ supported: false, pid: 41 }));
    await restartBackgroundService();
    expect(window.confirm).not.toHaveBeenCalled();
    request.mockResolvedValueOnce(json({ supported: true, pid: 41, daemonIdentity: 'dmn_test', heldDeliveries: 2, putBackReceipts: 1 }));
    vi.mocked(window.confirm).mockReturnValue(false);
    await restartBackgroundService();
    expect(request.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(0);
    expect(useBackgroundService.getState().message).toBe('');
  });
  it('bounds observation when only the old PID answers and prevents concurrent requests', async () => {
    request.mockResolvedValueOnce(json({ supported: true, pid: 41, daemonIdentity: 'dmn_test', heldDeliveries: 2, putBackReceipts: 1 }))
      .mockResolvedValueOnce(json({ accepted: true }))
      .mockImplementation(async () => json({ pid: 41, daemonIdentity: 'dmn_test' }));
    const first = restartBackgroundService();
    await restartBackgroundService();
    await vi.runAllTimersAsync();
    await first;
    expect(request.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1);
    expect(useBackgroundService.getState().message).toContain('has not returned with a new PID');
  });
});
