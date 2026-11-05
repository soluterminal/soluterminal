// What an approval grants, shown next to a device's code in the device list
// and the approve dialog (daemon trusted_devices.go scope).
export const ALERTS_SCOPE_TEXT = 'Alerts only: can take over push alerts, nothing else';
export const FULL_SCOPE_TEXT = 'Full access';

export function deviceScopeText(scope?: string): string {
  return scope === 'alerts' ? ALERTS_SCOPE_TEXT : FULL_SCOPE_TEXT;
}

// One of the person's own approved devices: it can approve others and act
// for them. An alerts-only device is approved but can do neither.
export function isFullApprovedDevice(d: { status: string; scope?: string }): boolean {
  return d.status === 'approved' && d.scope !== 'alerts';
}
