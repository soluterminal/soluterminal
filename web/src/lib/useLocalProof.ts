import { useCallback, useEffect, useRef, useState } from 'react';
import { currentRoutingScope, onRoutingScopeChange } from '../api';
import { localProofStatus, onLocalProofChange, refreshLocalProof, type LocalProofStatus } from './localProof';

const REFRESH_MS = 30_000;

/**
 * This browser's standing with the local daemon in view (lib/localProof.ts),
 * refreshed on mount, every 30 s and when the backend changes. null when the
 * view is a remote machine or the daemon predates approved devices here.
 */
export function useLocalProof(): { status: LocalProofStatus | null; backendPrefix: string; reload: () => Promise<void> } {
  const [scope, setScope] = useState(currentRoutingScope);
  const [status, setStatus] = useState<LocalProofStatus | null>(() =>
    scope.prefix === '' ? localProofStatus(scope.backendPrefix) : null);

  // The scope a reply must still belong to: a slow reply from the backend
  // just left must not paint over the one now in view.
  const liveKey = useRef(scope.key);
  liveKey.current = scope.key;

  useEffect(() => onRoutingScopeChange(() => {
    const next = currentRoutingScope();
    liveKey.current = next.key;
    // Drop the old backend's standing at once; the refetch below fills it in.
    setStatus(next.prefix === '' ? localProofStatus(next.backendPrefix) : null);
    setScope(next);
  }), []);

  const reload = useCallback(async () => {
    if (scope.prefix !== '') {
      setStatus(null);
      return;
    }
    const key = scope.key;
    try {
      const fresh = await refreshLocalProof(scope.backendPrefix);
      if (liveKey.current === key) setStatus(fresh);
    } catch {
      // Unreachable daemon: keep what we had; the next refresh tries again.
    }
  }, [scope.prefix, scope.backendPrefix, scope.key]);

  useEffect(() => {
    void reload();
    const timer = window.setInterval(() => void reload(), REFRESH_MS);
    const off = onLocalProofChange(() => {
      if (scope.prefix === '') setStatus(localProofStatus(scope.backendPrefix));
    });
    return () => {
      window.clearInterval(timer);
      off();
    };
  }, [reload, scope.prefix, scope.backendPrefix]);

  return { status, backendPrefix: scope.backendPrefix, reload };
}
