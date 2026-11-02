import { deviceCodeFor, deviceCodeV3For, deviceCommitV3For, newDeviceNonceV3, validDeviceNonceV3, validEnrolmentIdV3, type DeviceIdentity, type DeviceEnrolmentReply } from './deviceIdentity';

/** Keep the device nonce local; accept a V2 response only at negotiation. */
export async function beginDeviceEnrolmentV3<T extends DeviceEnrolmentReply>(
  me: Pick<DeviceIdentity, 'id' | 'spki'>,
  enroll: (commit: string) => Promise<T>,
  reveal: (id: string, enrolmentId: string, nonce: string) => Promise<T>,
): Promise<{ device: T; code: string; codeVersion: 2 | 3 }> {
  return beginDeviceComparisonV3(me, enroll, reveal, false);
}

export async function beginDeviceVerificationV3<T extends DeviceEnrolmentReply>(
  me: Pick<DeviceIdentity, 'id' | 'spki'>,
  commit: (commit: string) => Promise<T>,
  reveal: (id: string, enrolmentId: string, nonce: string) => Promise<T>,
  pinnedIdentity: string,
): Promise<{ device: T; code: string; codeVersion: 2 | 3 }> {
  return beginDeviceComparisonV3(me, commit, reveal, true, pinnedIdentity);
}

async function beginDeviceComparisonV3<T extends DeviceEnrolmentReply>(
  me: Pick<DeviceIdentity, 'id' | 'spki'>,
  enroll: (commit: string) => Promise<T>,
  reveal: (id: string, enrolmentId: string, nonce: string) => Promise<T>,
  verifying: boolean,
  pinnedIdentity?: string,
): Promise<{ device: T; code: string; codeVersion: 2 | 3 }> {
  const nonce = newDeviceNonceV3();
  const committed = await enroll(await deviceCommitV3For(me.id, nonce));
  if (!verifying && committed.status === 'approved' && committed.daemonNonce === undefined) {
    return { device: committed, code: '', codeVersion: committed.codeVersion === 3 ? 3 : 2 };
  }
  if (committed.daemonNonce === undefined) {
    if (verifying || committed.status !== 'pending' || (committed.codeVersion !== undefined && committed.codeVersion !== 2)
      || !committed.daemonIdentity) throw new Error('Unsupported comparison response');
    return { device: committed, code: await deviceCodeFor(me.spki, committed.daemonIdentity), codeVersion: 2 };
  }
  if (committed.id !== me.id || committed.status !== 'committed' || !committed.daemonIdentity
    || !committed.enrolmentId || !validEnrolmentIdV3(committed.enrolmentId)
    || !validDeviceNonceV3(committed.daemonNonce)
    || (committed.codeVersion !== undefined && committed.codeVersion !== 3)) {
    throw new Error('Invalid V3 commitment response');
  }
  if (verifying && (!pinnedIdentity || committed.daemonIdentity !== pinnedIdentity)) {
    throw new Error('Computer identity does not match the private connection');
  }
  const code = await deviceCodeV3For(me.spki, committed.daemonIdentity, committed.enrolmentId, nonce, committed.daemonNonce);
  const device = await reveal(me.id, committed.enrolmentId, nonce);
  const sameAttempt = verifying
    ? device.status === 'approved' && device.verify?.state === 'revealed' && device.verify.enrolmentId === committed.enrolmentId
    : device.status === 'pending' && device.codeVersion === 3 && device.enrolmentId === committed.enrolmentId;
  if (device.id !== me.id || !sameAttempt || device.daemonIdentity !== committed.daemonIdentity) {
    throw new Error('V3 comparison changed; start again');
  }
  return { device, code, codeVersion: 3 };
}
