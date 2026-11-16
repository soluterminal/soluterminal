// Account-wide device policy in "Your machines" (ST-250).
//
// The cloud reports, per daemon (backend) of each machine, its remote-access
// policy (from its live connection when connected, else the last one it
// reported), its platform, and whether the account's strictest policy refuses
// it (refused: now; wouldRefuse: once the cloud enforces it). These helpers
// turn that into the words the machine list shows.

export type DevicePolicyValue = 'approved' | 'any' | 'unknown';

export interface BackendDevicePolicy {
  backend: string;
  policy: DevicePolicyValue;
  platform?: 'windows' | 'linux-wsl' | 'linux' | 'darwin';
  // From the live connection; otherwise the last report, which can be old.
  connected?: boolean;
  refused?: boolean;
  wouldRefuse?: boolean;
}

const PLATFORM_LABELS: Record<string, string> = {
  windows: 'Windows',
  'linux-wsl': 'Linux (WSL)',
  linux: 'Linux',
  darwin: 'macOS',
};

const BACKEND_LABELS: Record<string, string> = {
  wsl: 'Linux (WSL)',
  win: 'Windows',
};

// platformLabel names where a daemon runs: its reported platform, else what
// its backend id implies, else nothing (an older daemon that never said).
export function platformLabel(row: Pick<BackendDevicePolicy, 'backend' | 'platform'>): string {
  return (row.platform && PLATFORM_LABELS[row.platform]) || BACKEND_LABELS[row.backend] || '';
}

export function policyLabel(policy: DevicePolicyValue, connected = true): string {
  let label: string;
  switch (policy) {
    case 'approved':
      label = 'Only approved devices';
      break;
    case 'any':
      label = 'Any device';
      break;
    case 'unknown':
      return 'Policy unknown (older version)';
    default:
      return 'Policy unknown';
  }
  return connected ? label : `${label} (last reported)`;
}

export const REFUSED_TEXT =
  "Blocked: your account requires approval, because a computer on it has been set to “Only devices I approve”. Turn that on for this computer too (Settings → Privacy & security) and approve your phone there, or remove it.";
export const WOULD_REFUSE_TEXT =
  "Will be blocked soon: your account requires approval, because a computer on it has been set to “Only devices I approve”. Turn that on for this computer too (Settings → Privacy & security) and approve your phone there, or remove it.";

// refusalText says why a daemon is (or soon will be) blocked, or null.
export function refusalText(row: BackendDevicePolicy): string | null {
  if (row.refused) return REFUSED_TEXT;
  if (row.wouldRefuse) return WOULD_REFUSE_TEXT;
  return null;
}

// machineBlocked: every reported daemon of the machine is refused now, so
// opening it would only show the refusal.
export function machineBlocked(rows: BackendDevicePolicy[] | undefined): boolean {
  return !!rows && rows.length > 0 && rows.every((r) => r.refused);
}

// lastSeenText renders "last seen 5 min ago" style text, or null.
export function lastSeenText(iso: string | undefined, now: number = Date.now()): string | null {
  if (!iso) return null;
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return null;
  const mins = Math.max(0, Math.round((now - t) / 60_000));
  if (mins < 1) return 'last seen just now';
  if (mins < 60) return `last seen ${mins} min ago`;
  const hours = Math.round(mins / 60);
  if (hours < 48) return `last seen ${hours} h ago`;
  return `last seen ${Math.round(hours / 24)} days ago`;
}

// duplicateNames returns the names (as shown) that two or more machines
// share, compared without case or surrounding spaces. A WSL copy and the
// Windows app on one PC normally both carry the PC's hostname.
export function duplicateNames(machines: { name: string }[]): string[] {
  const seen = new Map<string, { name: string; count: number }>();
  for (const m of machines) {
    const key = m.name.trim().toLowerCase();
    if (!key) continue;
    const entry = seen.get(key);
    if (entry) entry.count += 1;
    else seen.set(key, { name: m.name.trim(), count: 1 });
  }
  return [...seen.values()].filter((e) => e.count > 1).map((e) => e.name);
}

export function duplicateNameWarning(name: string): string {
  return `Two entries for “${name}”. One may be an old background copy of SoluTerminal (often in Linux (WSL)); check which one you use before removing the other.`;
}

// STRICTEST_WINS_TEXT is shown when the account has two or more computers and
// one requires approval (release-lead's Q6: warn before enforcement).
export const STRICTEST_WINS_TEXT =
  "On this account, the strictest setting wins. While any computer here is set to “Only devices I approve”, computers that let any device in are blocked from your phone and the web; the list says which ones are blocked now or will be soon. Turn on “Only devices I approve” on them too, or remove the ones you don't use. If several people share this login, this applies to everyone's computers.";
