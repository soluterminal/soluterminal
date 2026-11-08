import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { setBackend } from '../api';
import type { LocalProofStatus } from './localProof';

const statuses: Record<string, LocalProofStatus> = {};
const refresh = vi.fn(async (b: string) => statuses[b]);
vi.mock('./localProof', () => ({
  localProofStatus: (b: string) => statuses[b] ?? null,
  onLocalProofChange: () => () => {},
  refreshLocalProof: (b: string) => refresh(b),
}));

import { useLocalProof } from './useLocalProof';

const mk = (mode: LocalProofStatus['mode']): LocalProofStatus => ({
  mode, effective: mode, target: 't', serverTime: 0, envOff: false, approvedDevices: 0, canBootstrap: false,
});

afterEach(() => { act(() => setBackend(null)); refresh.mockClear(); });

describe('useLocalProof', () => {
  it('drops the old backend’s mode on a backend switch and shows the new one', async () => {
    statuses['/b/wsl'] = mk('report');
    statuses['/b/win'] = mk('off');
    act(() => setBackend('wsl'));
    const { result } = renderHook(() => useLocalProof());
    await waitFor(() => expect(result.current.status?.mode).toBe('report'));

    let release!: (s: LocalProofStatus) => void;
    refresh.mockImplementationOnce(() => new Promise((r) => { release = r; }));
    delete statuses['/b/win'];
    act(() => setBackend('win'));
    // Not WSL's “Log only” while the Windows reply is in flight.
    expect(result.current.status).toBeNull();
    await act(async () => release(mk('off')));
    expect(result.current.status?.mode).toBe('off');
    expect(result.current.backendPrefix).toBe('/b/win');
  });

  it('ignores a slow reply from the backend just left', async () => {
    statuses['/b/wsl'] = mk('report');
    act(() => setBackend('wsl'));
    let late!: (s: LocalProofStatus) => void;
    refresh.mockImplementationOnce(() => new Promise((r) => { late = r; }));
    const { result } = renderHook(() => useLocalProof());
    statuses['/b/win'] = mk('off');
    act(() => setBackend('win'));
    await waitFor(() => expect(result.current.status?.mode).toBe('off'));
    await act(async () => late(mk('report')));
    expect(result.current.status?.mode).toBe('off');
  });
});
