import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import { api, currentRoutingScope, onRoutingScopeChange } from '../../api';
import { deviceIdentity, pinnedComputerIdentity } from '../../lib/deviceIdentity';
import { beginDeviceVerificationV3 } from '../../lib/deviceEnrolment';
import { privateRelayTarget } from '../../lib/privateRelay';
import { SettingsRow } from './SettingsRow';

export function DeviceVerification() {
  const [code, setCode] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const generation = useRef(0);
  const scopeKey = useSyncExternalStore(onRoutingScopeChange, () => currentRoutingScope().key);
  const scope = currentRoutingScope();
  useEffect(() => {
    generation.current++;
    setCode('');
    setError('');
    setBusy(false);
    return () => { generation.current++; };
  }, [scopeKey]);
  if (!privateRelayTarget(scope.prefix, scope.backendPrefix)) return null;
  const start = async () => {
    const current = ++generation.current;
    setBusy(true);
    setCode('');
    setError('');
    try {
      const target = privateRelayTarget(scope.prefix, scope.backendPrefix);
      const pinned = target ? pinnedComputerIdentity(target) : null;
      if (!pinned) throw new Error('Computer identity is unavailable for the private connection');
      const me = await deviceIdentity();
      const comparison = await beginDeviceVerificationV3(me,
        (commit) => api.verifyRemoteDevice(scope, me.id, commit),
        (id, enrolment, nonce) => api.revealDeviceVerification(scope, id, enrolment, nonce), pinned);
      if (current === generation.current && scope.key === currentRoutingScope().key) setCode(comparison.code);
    } catch (e) {
      if (current === generation.current && scope.key === currentRoutingScope().key) setError(String(e));
    } finally {
      if (current === generation.current) setBusy(false);
    }
  };
  return (
    <SettingsRow label="Verify this browser" hint={
      <>
        Compare a fresh code on this browser and the computer. Finish in the computer's Remote devices settings.
        {code && <div>Verify V3 <code>{code}</code></div>}
        {error && <div className="settings-error" role="alert">{error}</div>}
      </>
    }>
      <button type="button" className="settings-btn" disabled={busy} onClick={() => void start()}>
        {busy ? 'Preparing…' : 'Compare V3 codes'}
      </button>
    </SettingsRow>
  );
}
