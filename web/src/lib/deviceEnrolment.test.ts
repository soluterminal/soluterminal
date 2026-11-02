import { beginDeviceEnrolmentV3, beginDeviceVerificationV3 } from './deviceEnrolment';
import { afterEach, describe, expect, it, vi } from 'vitest';
import * as identity from './deviceIdentity';
import { deviceCodeFor, deviceCodeV3For, deviceCommitV3For, type DeviceEnrolmentReply } from './deviceIdentity';
import v from './testdata/device-code-v3.json';

const me = { id: v.deviceId, spki: v.deviceSPKI };
const committed: DeviceEnrolmentReply = {
  id: me.id, status: 'committed', enrolmentId: v.enrolmentId, daemonIdentity: v.daemonSPKI,
  daemonNonce: v.daemonNonce, codeVersion: 3,
};
const pending: DeviceEnrolmentReply = { ...committed, status: 'pending', code: 'server code' };

afterEach(() => vi.restoreAllMocks());

describe('V3 device enrolment', () => {
  it('keeps an existing approval without claiming a new comparison', async () => {
    const reveal = vi.fn();
    const approved: DeviceEnrolmentReply = { id: me.id, status: 'approved', daemonIdentity: v.daemonSPKI, codeVersion: 3 };
    const result = await beginDeviceEnrolmentV3(me, async () => approved, reveal);
    expect(result.device).toBe(approved);
    expect(result.code).toBe('');
    expect(reveal).not.toHaveBeenCalled();
  });

  it('uses a separate revealed verify attempt on an approved device', async () => {
    const result = await beginDeviceVerificationV3(me, async () => committed, async () => ({
      id: me.id, status: 'approved', daemonIdentity: v.daemonSPKI, codeVersion: 2,
      verify: { state: 'revealed', enrolmentId: v.enrolmentId, code: 'server code' },
    }), v.daemonSPKI);
    expect(result.codeVersion).toBe(3);
    expect(result.code).not.toBe('server code');
  });

  it('refuses V2 for verification even though ordinary pairing accepts it', async () => {
    const oldDaemon: DeviceEnrolmentReply = { id: me.id, status: 'pending', daemonIdentity: v.daemonSPKI, codeVersion: 2 };
    await expect(beginDeviceVerificationV3(me, async () => oldDaemon, vi.fn(), v.daemonSPKI)).rejects.toThrow();
  });

  it.each([v.deviceSPKI, ''])('checks the pinned identity before computing a verification code (%s)', async (pin) => {
    const compute = vi.spyOn(identity, 'deviceCodeV3For');
    const reveal = vi.fn();
    await expect(beginDeviceVerificationV3(me, async () => committed, reveal, pin))
      .rejects.toThrow('Computer identity does not match the private connection');
    expect(compute).not.toHaveBeenCalled();
    expect(reveal).not.toHaveBeenCalled();
  });

  it('commits before revealing and computes its own comparison code', async () => {
    let commit = '';
    let nonce = '';
    const enroll = vi.fn(async (value: string) => { commit = value; return committed; });
    const reveal = vi.fn(async (id: string, enrolmentId: string, value: string) => {
      expect(commit).toBe(await deviceCommitV3For(id, value));
      expect(enrolmentId).toBe(v.enrolmentId);
      nonce = value;
      return pending;
    });
    const result = await beginDeviceEnrolmentV3(me, enroll, reveal);
    expect(enroll).toHaveBeenCalledTimes(1);
    expect(reveal).toHaveBeenCalledTimes(1);
    expect(result.codeVersion).toBe(3);
    expect(result.code).toBe(await deviceCodeV3For(me.spki, v.daemonSPKI, v.enrolmentId, nonce, v.daemonNonce));
    expect(result.code).not.toBe(pending.code);
  });

  it('negotiates V2 explicitly for a V2-only daemon without revealing', async () => {
    const reveal = vi.fn();
    const result = await beginDeviceEnrolmentV3(me, async () => ({
      id: me.id, status: 'pending', daemonIdentity: v.daemonSPKI, code: 'server code',
    }), reveal);
    expect(result.codeVersion).toBe(2);
    expect(result.code).toBe(await deviceCodeFor(me.spki, v.daemonSPKI));
    expect(reveal).not.toHaveBeenCalled();
  });

  it('does not fall back after a V3 reveal refusal', async () => {
    const enroll = vi.fn(async () => committed);
    await expect(beginDeviceEnrolmentV3(me, enroll, async () => { throw new Error('409'); })).rejects.toThrow('409');
    expect(enroll).toHaveBeenCalledTimes(1);
  });

  it.each([
    { daemonNonce: '' }, { enrolmentId: 'enr_short' }, { id: 'dev_other' },
    { status: 'approved' }, { codeVersion: 2 }, { daemonIdentity: '' },
    { daemonNonce: undefined },
  ])('refuses an inconsistent V3 commitment response: %j', async (fields) => {
    const reveal = vi.fn();
    await expect(beginDeviceEnrolmentV3(me, async () => ({ ...committed, ...fields }) as DeviceEnrolmentReply, reveal)).rejects.toThrow();
    expect(reveal).not.toHaveBeenCalled();
  });

  it.each([
    { enrolmentId: 'enr_other' }, { daemonIdentity: v.deviceSPKI }, { id: 'dev_other' },
    { codeVersion: 2 }, { status: 'approved' },
  ])('refuses a changed V3 reveal response: %j', async (fields) => {
    await expect(beginDeviceEnrolmentV3(me, async () => committed,
      async () => ({ ...pending, ...fields }) as DeviceEnrolmentReply)).rejects.toThrow();
  });
});
