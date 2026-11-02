import { describe, expect, it } from 'vitest';
import {
  canonicalDeviceQuery,
  deviceCodeFor,
  deviceCodeV3For,
  deviceCommitV3For,
  deviceSPKIHashFor,
  newDeviceNonceV3,
  deviceIdFor,
  deviceEnrolledFor,
  deviceSigMessage,
  markDeviceEnrolled,
  noteMachineBackends,
  relayPrefixFor,
  relayTarget,
  withDeviceSignatureQuery,
} from './deviceIdentity';
import v3 from './testdata/device-code-v3.json';

// Reference values from the daemon's own deviceID / deviceCode /
// deviceSigMessage (daemon/trusted_devices.go) for a fixed key.
const SPKI =
  'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAECqJG/QG2oPDwtUD/pEtTZ7kr1umboQSCs3VBTHognZiHan6FatqeXN1oOr+YXgR089/a3+vdtLMcEdJJHyqdBA==';

describe('deviceIdentity', () => {
  it('matches the shared DER hash and refuses alternative spellings of the key', async () => {
    expect(await deviceSPKIHashFor(v3.keyHash.spki)).toBe(v3.keyHash.derSHA256);
    expect(atob(v3.keyHash.nonCanonicalSPKI)).toBe(atob(v3.keyHash.spki));
    for (const spki of [v3.keyHash.nonCanonicalSPKI, v3.keyHash.spki.replace(/=+$/, ''), v3.keyHash.spki + '\n', 'AA==']) {
      await expect(deviceSPKIHashFor(spki)).rejects.toThrow();
    }
    await expect(deviceCodeV3For(v3.keyHash.nonCanonicalSPKI, v3.daemonSPKI, v3.enrolmentId, v3.deviceNonce, v3.daemonNonce)).rejects.toThrow();
  });

  it('matches the shared V3 commit and comparison vectors', async () => {
    expect(await deviceIdFor(v3.deviceSPKI)).toBe(v3.deviceId);
    expect(await deviceCommitV3For(v3.deviceId, v3.deviceNonce)).toBe(v3.commit);
    expect(await deviceCodeV3For(v3.deviceSPKI, v3.daemonSPKI, v3.enrolmentId, v3.deviceNonce, v3.daemonNonce)).toBe(v3.code);
    expect(newDeviceNonceV3()).toMatch(/^[0-9a-f]{64}$/);
  });

  it('rejects noncanonical V3 fields', async () => {
    await expect(deviceCommitV3For(v3.deviceId, v3.deviceNonce.toUpperCase())).rejects.toThrow();
    for (const [key, value] of [
      ['deviceSPKI', v3.deviceSPKI + '\n'], ['enrolmentId', 'enr_short'],
      ['deviceNonce', '00'], ['daemonNonce', v3.daemonNonce.toUpperCase()],
    ]) {
      const v = { ...v3, [key]: value };
      await expect(deviceCodeV3For(v.deviceSPKI, v.daemonSPKI, v.enrolmentId, v.deviceNonce, v.daemonNonce)).rejects.toThrow();
    }
  });

  it('derives the same id and approval code as the daemon', async () => {
    expect(await deviceIdFor(SPKI)).toBe('dev_b71SY8ikOq3NEn-6MV-j9z');
    expect(await deviceCodeFor(SPKI)).toBe('GJ23-QFPG-G9PE-ASX5');
    // V2 covers the computer's identity key too (daemon deviceCode with both).
    expect(await deviceCodeFor(SPKI, 'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEtestdaemonkey')).toBe('9ZXW-CWEW-S399-3DMA');
  });

  it('builds the exact message the daemon verifies', async () => {
    const msg = await deviceSigMessage('mach_x/wsl', 'post', '/api/panes', 'a=1', '1800000000', 'n1', new TextEncoder().encode('{}'));
    expect(msg).toBe(
      'SOLU-DEVICE-V1\nmach_x/wsl\nPOST\n/api/panes?a=1\n1800000000\nn1\n44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a',
    );
  });

  it('canonicalises the query the way the daemon does', () => {
    expect(canonicalDeviceQuery('b=2&a=1&a=0&x')).toBe('a=1&a=0&b=2&x=');
    expect(canonicalDeviceQuery('q=a+b%7E&dir=%2Ftmp&cols=80')).toBe('cols=80&dir=%2Ftmp&q=a%20b~');
    expect(canonicalDeviceQuery('')).toBe('');
  });

  it('maps relay prefixes to daemon targets', () => {
    expect(relayTarget('/m/mach_x', '')).toBe('mach_x');
    expect(relayTarget('/m/mach_x', '/b/wsl')).toBe('mach_x/wsl');
    expect(relayTarget('/m/mach%2Fy', '')).toBe('mach/y');
    expect(relayTarget('', '')).toBeNull();
    noteMachineBackends('mach_d', ['win', 'wsl']);
    expect(relayTarget('/m/mach_d', '')).toBe('mach_d/wsl'); // the cloud's default route
    noteMachineBackends('mach_h', ['host']);
    expect(relayTarget('/m/mach_h', '')).toBe('mach_h/host');
  });

  it('remembers enrolment per daemon, not per machine', () => {
    localStorage.clear();
    markDeviceEnrolled('mach_d/wsl');
    expect(deviceEnrolledFor('mach_d/wsl')).toBe(true);
    expect(deviceEnrolledFor('mach_d/win')).toBe(false); // the Windows backend keeps its own list
    expect(relayPrefixFor('mach_d/win')).toBe('/m/mach_d/b/win');
    expect(relayPrefixFor('mach_x')).toBe('/m/mach_x');
  });

  it('appends stream signatures after the existing query', () => {
    const s = { id: 'dev_a', time: '1', nonce: 'n+1', target: 'm/wsl', sig: 'abc' };
    expect(withDeviceSignatureQuery('wss://h/m/m/api/panes/p/io?cols=80', s)).toBe(
      'wss://h/m/m/api/panes/p/io?cols=80&solu_dev=dev_a&solu_dev_t=1&solu_dev_n=n%2B1&solu_dev_to=m%2Fwsl&solu_dev_sig=abc',
    );
    expect(withDeviceSignatureQuery('wss://h/x', s).startsWith('wss://h/x?solu_dev=')).toBe(true);
  });
});
