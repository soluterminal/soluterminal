import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError, setApiPrincipalAccess, setBackend } from './api';
import { setCloudDualBackend } from './dualBackend';
import { useStore } from './store';

const refusal = () => new ApiError(401, 'device not approved', { error: 'device_not_approved' });

function stubOtherLoads() {
  vi.spyOn(api, 'listWorkspaces').mockResolvedValue([]);
  vi.spyOn(api, 'listPorts').mockResolvedValue([]);
  for (const method of ['refreshUpdateStatus', 'loadRecoverySettings', 'loadNativeSettings',
    'loadUpdateSettings', 'loadOrchestratorHosts', 'refreshSchedules'] as const) {
    vi.spyOn(useStore.getState(), method).mockResolvedValue(undefined);
  }
}

beforeEach(() => {
  localStorage.clear();
  setCloudDualBackend(false);
  setBackend(null);
  setApiPrincipalAccess(true);
  useStore.setState({ panes: {}, layout: null, bootstrapError: null, bootstrapNeedsApproval: false });
  stubOtherLoads();
});
afterEach(() => vi.restoreAllMocks());

// The approval screen is chosen from the error that failed the current load.
describe('bootstrap approval state', () => {
  it('marks a load the daemon refused for this device', async () => {
    vi.spyOn(api, 'listPanes').mockRejectedValue(refusal());
    await useStore.getState().bootstrap();
    expect(useStore.getState().bootstrapError).toBeTruthy();
    expect(useStore.getState().bootstrapNeedsApproval).toBe(true);
  });

  it('clears it when a retry fails for another reason', async () => {
    const listPanes = vi.spyOn(api, 'listPanes').mockRejectedValueOnce(refusal());
    await useStore.getState().bootstrap();
    listPanes.mockRejectedValueOnce(new TypeError('Failed to fetch'));
    await useStore.getState().bootstrap();
    expect(useStore.getState().bootstrapError).toBeTruthy();
    expect(useStore.getState().bootstrapNeedsApproval).toBe(false);
  });

  it('ignores a late refusal from an earlier load', async () => {
    let refuseFirst!: (e: unknown) => void;
    vi.spyOn(api, 'listPanes')
      .mockImplementationOnce(() => new Promise((_, reject) => { refuseFirst = reject; }))
      .mockRejectedValueOnce(new TypeError('Failed to fetch'));
    const first = useStore.getState().bootstrap();
    await useStore.getState().bootstrap();
    refuseFirst(refusal());
    await first;
    expect(useStore.getState().bootstrapError).toBeTruthy();
    expect(useStore.getState().bootstrapNeedsApproval).toBe(false);
  });
});
