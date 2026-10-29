import { sidebarSectionSlot } from '../../extensions';

// Registry for the middle-zone sidebar sections. The live sidebar and the
// customizer both derive their ordering/visibility from these pure helpers,
// so the two can never disagree. User order + hidden set live in cloud-synced
// prefs (see prefs.ts → UserPrefs.sidebar); this module owns the defaults and
// the reconciliation rules (new sections default visible, in default position;
// unknown keys are dropped).

// NOTE: 'usage' was retired as a section — the Usage page now opens from the
// account footer. Stale 'usage' entries in saved prefs are unknown keys and
// get dropped by resolvedMiddleOrder.
// NOTE: 'aria' was retired as a section in 2026-07 — the panel was removed;
// the auto-approver's controls live on pane badges + the account footer.
// Stale 'aria' entries in saved prefs are unknown keys and get dropped.
// NOTE: 'todos' was retired in 2026-10 with the Todos feature; stale entries
// are unknown keys too and get dropped the same way.
export type MiddleKey =
  | 'owners'
  | 'agentPanes'
  | 'files'
  | 'ports'
  | 'background'
  | 'credentials'
  | 'schedules'
  | 'agents'
  | 'mcp';

// Everyday-first: the sections a typical user touches daily lead; operator /
// assistant surfaces (credentials, mcp) trail and default hidden (see
// DEFAULT_HIDDEN) so a new user isn't greeted by a wall of power-user panels.
export const DEFAULT_MIDDLE_ORDER: MiddleKey[] = [
  'agentPanes',
  'files',
  'ports',
  'background',
  'credentials',
  'schedules',
  'agents',
  'mcp',
];

// Sections hidden for a user who has never customized the sidebar — the
// operator/power/assistant surfaces most users don't need in week one. They're
// one click away via the "Add sections" affordance. Once a user customizes
// anything, their explicit hidden set takes over and this no longer applies.
//
// 'schedules' left this list in v0.0.872: with the recipe shelf and the
// "Run a Claude task" kind it is a day-one surface (schedule a morning digest
// in two clicks), not a power-user panel.
//
// 'agents' was never added, for a sharper version of the same reason
// (docs/DESIGN-agents-surface.md §1.3): a surface whose entire purpose is "you
// can see what runs unattended" is useless if it starts hidden. Autonomy the
// user has to go looking for is autonomy they don't know about.
export const DEFAULT_HIDDEN: MiddleKey[] = ['credentials', 'mcp'];

// Public beta installs (not team) also start with Files and Localhost ports
// tucked away: useful, but not what a first-time user needs on screen.
// The ports status chip and "dev server up" toast still work, file tiles still
// open from paths and Ctrl+P, and every section is one click away in
// Settings → Sidebar.
export const PUBLIC_DEFAULT_HIDDEN: MiddleKey[] = [...DEFAULT_HIDDEN, 'files', 'ports'];

export function defaultHidden(team: boolean): MiddleKey[] {
  return team ? [...DEFAULT_HIDDEN] : [...PUBLIC_DEFAULT_HIDDEN];
}

// Sections only a confirmed team install shows (the workflow Task Owners list
// and the managed-agents fleet). Public beta installs drop them from the live
// sidebar, the right-click menu and the customizer alike.
export const TEAM_ONLY_SECTIONS: readonly MiddleKey[] = ['owners', 'agents'];

// Sections a team plan carries but a public install doesn't. Credentials holds
// saved-account switching (team) and API-key rows that only feed team tooling,
// so a stranger who added it back would find nothing that works for them.
export const TEAM_PLAN_SECTIONS: readonly MiddleKey[] = ['credentials'];

// `team` is the operator verdict (task owners, agents); `teamPlan` is the team
// verdict (credentials), which an operator always has too.
export function withoutTeamOnly(
  keys: readonly MiddleKey[],
  team: boolean,
  teamPlan: boolean = team,
): MiddleKey[] {
  return keys.filter((k) => (
    (team || !TEAM_ONLY_SECTIONS.includes(k))
    && (teamPlan || !TEAM_PLAN_SECTIONS.includes(k))
  ));
}

// Sections an optional feature renders (extensions.ts). Without a registered
// renderer they are not offered anywhere: not in the live sidebar, its
// right-click menu or the customizer.
const SLOTTED_SECTIONS: readonly MiddleKey[] = ['credentials', 'agents'];

export function sectionOffered(key: MiddleKey): boolean {
  return !SLOTTED_SECTIONS.includes(key) || !!sidebarSectionSlot(key);
}

// Human labels for the customizer + the right-click "Hide …" item.
export const MIDDLE_SECTION_LABELS: Record<MiddleKey, string> = {
  owners: 'Task Owners',
  files: 'Files',
  ports: 'Services',
  background: 'Detached sessions',
  agentPanes: 'Started by agents',
  credentials: 'Credentials',
  schedules: 'Automations',
  agents: 'Agents',
  mcp: 'MCP servers',
};

const KNOWN = new Set<MiddleKey>(DEFAULT_MIDDLE_ORDER);

export function isMiddleKey(k: string): k is MiddleKey {
  return KNOWN.has(k as MiddleKey);
}

// The full ordering of every known section: the user's saved order (valid keys
// only, de-duplicated) followed by any sections they've never ordered — new
// defaults land in their default position. Guarantees each known section
// appears exactly once, so a shipped-later section never silently vanishes.
export function resolvedMiddleOrder(order: readonly string[] | undefined): MiddleKey[] {
  const seen = new Set<MiddleKey>();
  const out: MiddleKey[] = [];
  for (const k of order ?? []) {
    if (isMiddleKey(k) && !seen.has(k)) {
      seen.add(k);
      out.push(k);
    }
  }
  for (const k of DEFAULT_MIDDLE_ORDER) {
    if (!seen.has(k)) {
      seen.add(k);
      out.push(k);
    }
  }
  return out;
}

// The hidden set that actually applies. A user who has never customized the
// sidebar (no saved order AND no saved hidden) gets the shipped DEFAULT_HIDDEN,
// so operator/assistant sections start tucked away. The instant they customize
// anything — reorder or toggle — their explicit hidden set becomes
// authoritative and the shipped defaults no longer apply. hide/show helpers must
// base their edits on THIS (not the raw saved set) so a first toggle from the
// default state materializes DEFAULT_HIDDEN rather than dropping it.
export function effectiveHidden(
  order: readonly string[] | undefined,
  hidden: readonly string[] | undefined,
  team = true,
): string[] {
  if (order === undefined && hidden === undefined) return defaultHidden(team);
  return [...(hidden ?? [])];
}

// The sections the live sidebar should render, in order: resolved order minus
// the effective hidden set.
export function effectiveMiddleOrder(
  order: readonly string[] | undefined,
  hidden: readonly string[] | undefined,
  team = true,
): MiddleKey[] {
  const hiddenSet = new Set(effectiveHidden(order, hidden, team));
  return resolvedMiddleOrder(order).filter((k) => !hiddenSet.has(k));
}

// The known sections currently hidden, in order — powers the "Add sections"
// menu so a hidden section always has a visible way back.
export function hiddenMiddleKeys(
  order: readonly string[] | undefined,
  hidden: readonly string[] | undefined,
  team = true,
): MiddleKey[] {
  const hiddenSet = new Set(effectiveHidden(order, hidden, team));
  return resolvedMiddleOrder(order).filter((k) => hiddenSet.has(k));
}

// Whether a sidebar right-click landed on "unclaimed" space and should get the
// generic "Customize sidebar…" fallback menu. Returns false when a more
// specific handler already claimed the event (workspace rows and section slots
// call preventDefault) or when the click was inside a text field (so native
// copy/paste menus survive in the rename/create inputs).
export function isUnclaimedSidebarSpace(
  defaultPrevented: boolean,
  target: Element | null,
): boolean {
  if (defaultPrevented) return false;
  if (target?.closest('input, textarea')) return false;
  return true;
}

// Move `from` to just before/after `to` within the full resolved order,
// returning a new complete order array. A no-op (returns the resolved order
// unchanged) if `to` isn't found.
export function reorderMiddle(
  order: readonly string[] | undefined,
  from: MiddleKey,
  to: MiddleKey,
  pos: 'before' | 'after',
): MiddleKey[] {
  const resolved = resolvedMiddleOrder(order);
  if (from === to) return resolved;
  const base = resolved.filter((k) => k !== from);
  const idx = base.indexOf(to);
  if (idx < 0) return resolved;
  base.splice(pos === 'before' ? idx : idx + 1, 0, from);
  return base;
}

