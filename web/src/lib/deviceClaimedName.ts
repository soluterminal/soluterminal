// A pending device's name and surface come from the enrolling device itself
// (daemon trusted_devices.go), so they are its claim, not a fact. Shown as
// plain text only: control and direction-override characters removed (they
// could reorder or hide what the person reads), whitespace collapsed and the
// length capped.

export const DEVICE_NAME_MAX = 64;

// C0/C1 controls, bidi embeddings/overrides/isolates and marks, zero-width
// characters and the BOM.
const UNSAFE = /[\u0000-\u001f\u007f-\u009f؜​-‏‪-‮⁠-⁩﻿]/g;

export function deviceClaimedName(name: string): string {
  const clean = (name ?? '').replace(UNSAFE, '').replace(/\s+/g, ' ').trim();
  const chars = Array.from(clean);
  if (chars.length === 0) return '(no name)';
  return chars.length > DEVICE_NAME_MAX ? chars.slice(0, DEVICE_NAME_MAX - 1).join('') + '…' : clean;
}
