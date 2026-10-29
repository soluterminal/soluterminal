
import { useEffect, useMemo, useRef, useState, type MouseEvent as ReactMouseEvent, type PointerEvent as ReactPointerEvent, type UIEvent as ReactUIEvent } from 'react';
import { paneIdsInOrder, useStore } from '../store';
import { confirmDialog } from '../lib/dialog';
import { PortPanel } from './PortPanel';
import { isRouterMode } from '../dualBackend';
import { AccountFooter } from './sidebar/AccountFooter';
import { BackgroundSessionsSection } from './sidebar/BackgroundSessionsSection';
import { FilesSection } from './sidebar/FilesSection';
import { MCPSection } from './sidebar/MCPSection';
import { SchedulesSection } from './sidebar/SchedulesSection';
import { sidebarSectionSlot } from '../extensions';
import { SectionHeader } from './sidebar/SectionHeader';
import { AgentPanesSection } from './sidebar/AgentPanesSection';
import { IconGlobe, IconPlus } from './ui/icons';
import { WorkspaceSection } from './sidebar/WorkspaceSection';
import { UsagePage } from './UsagePage';
import type { WorkspaceDropTarget } from './sidebar/WorkspaceSection';
import type { Workspace } from '../types';
import { usePrefs } from '../prefs';
import { useSettings } from '../settings';
import { useClampedMenuPosition } from './useClampedMenuPosition';
import { effectiveZoom } from '../lib/effectiveZoom';
import {
  MIDDLE_SECTION_LABELS,
  effectiveHidden,
  effectiveMiddleOrder,
  hiddenMiddleKeys,
  isMiddleKey,
  isUnclaimedSidebarSpace,
  sectionOffered,
  withoutTeamOnly,
  type MiddleKey,
} from './sidebar/sidebarSections';
import { useOperatorFeatures, usePersonalView, useTeamFeatures } from '../entitlements';
import { isWebService, workspacePorts } from '../lib/ports';

// Three-zone sidebar layout:
//   * Workspace zone (top) — collapsible, expanded by default
//   * Middle accordion — at most one of (background, ports, files,
//     schedules, mcp) expanded at a time
//   * ARIA zone (bottom) — collapsible, collapsed by default (the header
//     strip carries live status + the cap-reset action, so day-to-day the
//     panel doesn't need to be open)
//
// Sizing: when something is open in the middle, the three zones split the
// available height 1:1:1. When nothing is open, the middle collapses to
// just its row of headers and workspace + ARIA share the remainder 1:1.
// A collapsed zone shrinks to its header strip and the remaining expanded
// zone(s) absorb the slack. Each zone scrolls internally so the
// proportions stay stable as content grows.

const OPEN_MIDDLE_KEY_V1 = 'solu.sidebarOpenMiddle.v1';
const OPEN_MIDDLE_KEY = 'solu.sidebarOpenMiddle.v2';
const WS_COLLAPSED_KEY = 'solu.sidebarWorkspacesCollapsed.v1';

// Multi-open: several middle sections can be expanded at once, so open-state is
// a Set (v2 stores a JSON array). v1 held a single scalar key — migrate it to a
// one-element set so a user's one open section survives the upgrade.
// "Started by agents" opens once for everyone (new and existing sidebars), so
// the panes agents start are noticed; collapsing it afterwards sticks.
const AGENT_PANES_INTRODUCED_KEY = 'solu.sidebar.agentPanesIntroduced';

function loadOpenMiddle(): Set<MiddleKey> {
  const open = loadStoredOpenMiddle();
  try {
    if (localStorage.getItem(AGENT_PANES_INTRODUCED_KEY) === null) {
      open.add('agentPanes');
      localStorage.setItem(AGENT_PANES_INTRODUCED_KEY, '1');
      saveOpenMiddle(open);
    }
  } catch { /* ignore */ }
  return open;
}
function loadStoredOpenMiddle(): Set<MiddleKey> {
  try {
    const raw = localStorage.getItem(OPEN_MIDDLE_KEY);
    if (raw) {
      const arr: unknown = JSON.parse(raw);
      if (Array.isArray(arr)) return new Set(arr.filter(isMiddleKey));
    }
    const legacy = localStorage.getItem(OPEN_MIDDLE_KEY_V1);
    if (legacy && legacy !== 'null' && isMiddleKey(legacy)) {
      return new Set<MiddleKey>([legacy]);
    }
  } catch { /* ignore */ }
  try { if (localStorage.getItem(OPEN_MIDDLE_KEY) === null && localStorage.getItem(OPEN_MIDDLE_KEY_V1) === null) return new Set<MiddleKey>(['owners']); } catch {}
  return new Set<MiddleKey>();
}

function saveOpenMiddle(keys: Set<MiddleKey>) {
  try { localStorage.setItem(OPEN_MIDDLE_KEY, JSON.stringify([...keys])); } catch { /* ignore */ }
}

function loadWsCollapsed(): boolean {
  try { return localStorage.getItem(WS_COLLAPSED_KEY) === '1'; } catch { return false; }
}
function saveWsCollapsed(v: boolean) {
  try { localStorage.setItem(WS_COLLAPSED_KEY, v ? '1' : '0'); } catch { /* ignore */ }
}

// Per-section drag-resized heights (px), persisted. Every open section except
// the LAST gets a drag handle; its saved height applies as a max-height CAP,
// so a section whose content is shorter shrink-wraps (the card ends flush with
// its last row) and only content taller than the cap scrolls. The last open
// one has no cap — it too shrink-wraps, and leftover space stays clean sidebar
// background below the trailing collapsed headers.
const SECTION_HEIGHTS_KEY = 'solu.sidebarSectionHeights.v1';
const DEFAULT_SECTION_HEIGHT = 220;
const MIN_SECTION_HEIGHT = 96;
const MAX_SECTION_HEIGHT = 640;

function loadSectionHeights(): Record<string, number> {
  try {
    const raw = localStorage.getItem(SECTION_HEIGHTS_KEY);
    const obj: unknown = raw ? JSON.parse(raw) : null;
    if (obj && typeof obj === 'object') return obj as Record<string, number>;
  } catch { /* ignore */ }
  return {};
}
function saveSectionHeights(h: Record<string, number>) {
  try { localStorage.setItem(SECTION_HEIGHTS_KEY, JSON.stringify(h)); } catch { /* ignore */ }
}

// Drag-set height for the pinned Workspaces zone — lets you resize the split
// between the workspace list and the panels below it (the top boundary the
// section handles couldn't reach). null ⇒ use the default flex sizing.
const WS_HEIGHT_KEY = 'solu.sidebarWorkspaceHeight.v1';
const MIN_WS_HEIGHT = 90;
const MAX_WS_HEIGHT = 720;
function loadWsHeight(): number | null {
  try {
    const n = parseInt(localStorage.getItem(WS_HEIGHT_KEY) ?? '', 10);
    return Number.isFinite(n) ? n : null;
  } catch { return null; }
}
function saveWsHeight(n: number) {
  try { localStorage.setItem(WS_HEIGHT_KEY, String(n)); } catch { /* ignore */ }
}

function workspaceKey(w: Workspace): string {
  return w.backend ? `${w.backend}:${w.name}` : w.name;
}

export function Sidebar({
  onToggleCollapse,
  onOpenHelp,
}: {
  onToggleCollapse?: () => void;
  // Click handler for the "Press ? for shortcuts" footer. Makes the
  // shortcut overlay reachable without the keyboard, which matters when
  // the user is focused in a terminal where "?" is a real character.
  onOpenHelp?: () => void;
} = {}) {
  const teamFeatures = useOperatorFeatures();
  const serviceCount = useStore(s => workspacePorts(s.ports,s.activeWorkspace,s.activeBackend,s.workspaces).filter(isWebService).length);

  // Credentials is a team-plan section (saved accounts, team-only key rows).
  const teamPlan = useTeamFeatures();
  const [openMiddle, setOpenMiddle] = useState<Set<MiddleKey>>(loadOpenMiddle);
  const toggleMiddle = (k: MiddleKey) => {
    setOpenMiddle((prev) => {
      const next = new Set(prev);
      if (next.has(k)) next.delete(k); else next.add(k);
      saveOpenMiddle(next);
      return next;
    });
  };
  const isOpen = (k: MiddleKey) => openMiddle.has(k);
  const [sectionHeights, setSectionHeights] = useState<Record<string, number>>(loadSectionHeights);
  const heightFor = (k: MiddleKey) => sectionHeights[k] ?? DEFAULT_SECTION_HEIGHT;
  // Keep in sync with the CSS floors: .sidebar-section-slot.is-open:not(.is-fill)
  // min-height (shrink floor) and .is-fill min-height (same header floor now
  // that the fill slot shrink-wraps to content instead of stretching).
  const SLOT_SHRINK_FLOOR = 36;
  const FILL_MIN_HEIGHT = 36;
  // Drag a section's bottom handle to set its height. Updates the slot's inline
  // height IMPERATIVELY during the drag (no per-move re-render of the whole
  // sidebar), committing to state + localStorage on release. All math is in
  // LAYOUT px; pointer deltas are divided by the effective zoom (see above).
  const startResize = (e: ReactPointerEvent, key: MiddleKey) => {
    e.preventDefault();
    const handle = e.currentTarget as HTMLElement;
    const slot = handle.parentElement as HTMLElement | null;
    const zone = slot?.parentElement as HTMLElement | null;
    if (!slot || !zone) return;
    const zf = effectiveZoom(slot, 'y');
    // Measure EVERYTHING at the current rendered state first — before any
    // freezing/rigidity below changes layout. A slot squeezed below its saved
    // height would otherwise snap to the stale basis the moment shrink is
    // disabled, and measuring after that snap starts the drag from the wrong
    // baseline (the grip sits offset from the pointer for the whole drag).
    const startY = e.clientY;
    const startH = slot.offsetHeight;
    const siblings = (Array.from(zone.children) as HTMLElement[]).filter(
      (c) => c !== slot && c.classList.contains('is-open') && !c.classList.contains('is-fill'),
    );
    // Realizable ceiling: the zone minus what every other child keeps (frozen
    // opens + collapsed headers at current height, the fill slot at its floor).
    // Clamping here keeps the grip ON the pointer at the travel limit instead
    // of banking unrealizable height that snaps back after release.
    let othersMin = 0;
    for (const c of Array.from(zone.children) as HTMLElement[]) {
      if (c === slot) continue;
      othersMin += c.classList.contains('is-fill') ? Math.min(c.offsetHeight, FILL_MIN_HEIGHT) : c.offsetHeight;
    }
    const zoneBudget = zone.clientHeight - othersMin;
    // Now freeze the dragged slot AND every open sibling at their CURRENT
    // rendered heights and make open slots rigid for the drag (.is-resizing).
    // The header-visibility shrink floors otherwise re-distribute space
    // mid-drag, absorbing part of each height change so the grip lags the
    // pointer. The dragged slot's max-height CAP (the React-rendered style)
    // must lift for the drag or growing past the old cap would be silently
    // blocked.
    slot.style.height = `${startH}px`;
    slot.style.maxHeight = 'none';
    for (const s of siblings) s.style.height = `${s.offsetHeight}px`;
    zone.classList.add('is-resizing');
    // The drag floor must never exceed the slot's CURRENT height — a slot
    // squeezed below MIN_SECTION_HEIGHT by the shrink floors would otherwise
    // jump to the minimum the instant it's grabbed.
    const minH = Math.min(MIN_SECTION_HEIGHT, startH);
    const maxH = Math.max(minH, Math.min(MAX_SECTION_HEIGHT, zoneBudget));
    let finalH = startH;
    handle.setPointerCapture(e.pointerId);
    const onMove = (ev: PointerEvent) => {
      finalH = Math.max(minH, Math.min(maxH, startH + (ev.clientY - startY) / zf));
      slot.style.height = `${finalH}px`;
    };
    const onUp = () => {
      handle.removeEventListener('pointermove', onMove);
      handle.removeEventListener('pointerup', onUp);
      handle.removeEventListener('pointercancel', onUp);
      zone.classList.remove('is-resizing');
      // Persist REALIZED layout heights — the dragged slot AND the frozen
      // siblings — so saved state matches the DOM exactly. This self-heals
      // stale saves (heights recorded while squeezed, or corrupted by the old
      // visual-px math) instead of re-materializing them on the next render.
      const realized: Record<string, number> = { [key]: Math.round(slot.offsetHeight) };
      for (const s of siblings) {
        const k = s.dataset.sectionKey;
        if (k) realized[k] = Math.round(s.offsetHeight);
      }
      // Hand control back to the React-rendered max-height caps: re-cap the
      // dragged slot at its realized height IMPERATIVELY first (so there's no
      // uncapped frame before React commits the same value), then drop the
      // frozen inline heights — slots settle at min(content, cap), which is
      // exactly what they measured at.
      slot.style.maxHeight = `${realized[key]}px`;
      slot.style.height = '';
      for (const s of siblings) s.style.height = '';
      setSectionHeights((prev) => {
        const next = { ...prev, ...realized };
        saveSectionHeights(next);
        return next;
      });
    };
    handle.addEventListener('pointermove', onMove);
    handle.addEventListener('pointerup', onUp);
    handle.addEventListener('pointercancel', onUp);
  };
  // Sticky-header scroll cue: flag a section's scroll container once content
  // has passed under the pinned header (CSS draws the hairline + shadow on
  // .is-scrolled). Imperative class toggle — a per-scroll-event React state
  // write would re-render the whole sidebar.
  const onSectionScroll = (e: ReactUIEvent<HTMLDivElement>) => {
    const el = e.currentTarget;
    el.classList.toggle('is-scrolled', el.scrollTop > 0);
  };
  // Collapsing a section unmounts its body and zeroes scrollTop WITHOUT firing
  // a scroll event, which would strand a stale .is-scrolled — clear it as the
  // collapsed slot (re)renders so reopening never shows a phantom shadow.
  const clearScrolled = (el: HTMLDivElement | null) => {
    el?.classList.remove('is-scrolled');
  };
  // Double-click a section handle to reset it to the default height — the
  // same "undo my drag" affordance the app-level sidebar handle offers via
  // its double-click (collapse/expand).
  const resetSectionHeight = (key: MiddleKey) => {
    setSectionHeights((prev) => {
      const next = { ...prev };
      delete next[key];
      saveSectionHeights(next);
      return next;
    });
  };

  const [wsHeight, setWsHeight] = useState<number | null>(loadWsHeight);
  const startWsResize = (e: ReactPointerEvent) => {
    e.preventDefault();
    const handle = e.currentTarget as HTMLElement;
    const zone = handle.previousElementSibling as HTMLElement | null; // the workspace zone
    const content = zone?.parentElement as HTMLElement | null; // .sidebar-content
    if (!zone || !content) return;
    const zf = effectiveZoom(zone, 'y');
    // Realizable ceiling: the content column minus the handle's flow height
    // and the middle zone's floor (collapsed headers stay whole; open slots
    // shrink to their header floors). Same rationale as startResize's clamp.
    let middleMin = 0;
    const middle = content.querySelector('.sidebar-zone-middle');
    if (middle) {
      for (const c of Array.from(middle.children) as HTMLElement[]) {
        if (!c.classList.contains('is-open')) middleMin += (c as HTMLElement).offsetHeight;
        else middleMin += c.classList.contains('is-fill') ? FILL_MIN_HEIGHT : SLOT_SHRINK_FLOOR;
      }
    }
    const handleFlow = handle.offsetHeight + (parseFloat(getComputedStyle(handle).marginTop) || 0);
    const startY = e.clientY;
    const startH = zone.offsetHeight; // layout px — matches the px written back below
    // Same floor rule as startResize: never force-grow a zone that's already
    // rendering below the drag minimum.
    const minH = Math.min(MIN_WS_HEIGHT, startH);
    const maxH = Math.max(minH, Math.min(MAX_WS_HEIGHT, content.clientHeight - handleFlow - middleMin));
    let finalH = startH;
    handle.setPointerCapture(e.pointerId);
    const onMove = (ev: PointerEvent) => {
      finalH = Math.max(minH, Math.min(maxH, startH + (ev.clientY - startY) / zf));
      zone.style.flex = `0 0 ${finalH}px`;
      zone.style.maxHeight = 'none';
    };
    const onUp = () => {
      handle.removeEventListener('pointermove', onMove);
      handle.removeEventListener('pointerup', onUp);
      handle.removeEventListener('pointercancel', onUp);
      const h = Math.round(zone.offsetHeight); // realized layout height
      setWsHeight(h);
      saveWsHeight(h);
    };
    handle.addEventListener('pointermove', onMove);
    handle.addEventListener('pointerup', onUp);
    handle.addEventListener('pointercancel', onUp);
  };
  const resetWsHeight = () => {
    setWsHeight(null);
    try { localStorage.removeItem(WS_HEIGHT_KEY); } catch { /* ignore */ }
  };
  // Full-screen Usage page overlay. Open-state lives here (this SPA has no
  // router; overlays are React-state toggled — mirrors how App.tsx mounts
  // WelcomeOverlay / HotkeyHelp). The UsagePage is position:fixed so it
  // escapes the sidebar panel's layout despite being rendered from here,
  // which keeps the section + its page co-located in one component.
  const [usagePageOpen, setUsagePageOpen] = useState(false);
  useEffect(() => {
    const openUsage = () => setUsagePageOpen(true);
    window.addEventListener('solu:open-usage', openUsage);
    return () => window.removeEventListener('solu:open-usage', openUsage);
  }, []);
  const [wsCollapsed, setWsCollapsed] = useState<boolean>(loadWsCollapsed);
  const toggleWs = () => {
    setWsCollapsed((prev) => {
      const next = !prev;
      saveWsCollapsed(next);
      return next;
    });
  };

  const workspaces = useStore((s) => s.workspaces);
  const ownerVisible = useStore(s=>s.mainPresentation.kind === 'owner');
  const managedOwnerBindings = useStore(s=>s.managedOwnerBindings);
  const activeWorkspace = useStore((s) => s.activeWorkspace);
  const activeBackend = useStore((s) => s.activeBackend);
  const loadWorkspace = useStore((s) => s.loadWorkspace);
  const deleteWorkspace = useStore((s) => s.deleteWorkspace);
  const renameWorkspace = useStore((s) => s.renameWorkspace);
  const setWorkspaceContextPack = useStore((s) => s.setWorkspaceContextPack);
  const saveWorkspace = useStore((s) => s.saveWorkspace);
  const clearLayout = useStore((s) => s.clearLayout);
  const panes = useStore((s) => s.panes);
  const layout = useStore((s) => s.layout);
  const revealPane = useStore((s) => s.revealPane);
  const killPane = useStore((s) => s.killPane);
  const allPanes = useStore((s) => s.panes);
  const wsOrder = useStore((s) => s.wsOrder);
  const setWsOrder = useStore((s) => s.setWsOrder);
  const wsLastViewedMs = useStore((s) => s.wsLastViewedMs);
  const markWorkspaceViewed = useStore((s) => s.markWorkspaceViewed);
  const focusedPaneId = useStore((s) => s.focusedPaneId);
  const [wsDropTarget, setWsDropTarget] = useState<WorkspaceDropTarget>(null);
  const [renamingName, setRenamingName] = useState<string | null>(null);
  const [renameDraft, setRenameDraft] = useState('');
  const [creating, setCreating] = useState(false);
  const [createDraft, setCreateDraft] = useState('');
  const createCancelRef = useRef(false);
  const [ctxMenu, setCtxMenu] = useState<{ x: number; y: number; wsName: string } | null>(null);
  const ctxMenuRef = useRef<HTMLDivElement | null>(null);
  // The personal view shows a stranger's sidebar: the shipped defaults, not
  // this install's own arrangement, which stays untouched (edits are off).
  const personalView = usePersonalView();
  const ownSidebarPrefs = usePrefs((s) => s.prefs.sidebar);
  const sidebarPrefs = personalView ? undefined : ownSidebarPrefs;
  const setPrefs = usePrefs((s) => s.setPrefs);
  // key: a MiddleKey → the per-section menu (Hide + Show-hidden + Customize);
  // null → the generic fallback used when the right-click lands on unclaimed
  // sidebar space (workspace header, ARIA zone, gaps, footer). Both variants
  // list any hidden sections as "Show …" items, so a hidden section always
  // has a one-click way back from the same menu that hid it.
  const [sectionCtx, setSectionCtx] = useState<{ x: number; y: number; key: MiddleKey | null } | null>(null);
  const sectionCtxRef = useRef<HTMLDivElement | null>(null);
  const sectionCtxPos = useClampedMenuPosition(sectionCtxRef, sectionCtx);
  const toast = useStore((s) => s.toast);

  // Keep the ACTIVE workspace marked "viewed" so its own panes' activity
  // doesn't accrue an unread "activity" badge on the workspace you're
  // currently sitting in (see WorkspaceSection's lastActivity > viewedAt
  // gate). markWorkspaceViewed does a synchronous localStorage write + a
  // new-object store `set` on every call, so the old unconditional 5s
  // interval churned that work even when nothing changed. Two-part fix:
  //   * mark IMMEDIATELY when the active workspace changes (instant badge
  //     clear on switch — the responsive bit),
  //   * then refresh on a much slower cadence to absorb in-place activity,
  //     skipping the write entirely if the active workspace drifted away.
  const lastMarkedWsRef = useRef<string | null>(null);
  useEffect(() => {
    if (ownerVisible || !activeWorkspace || activeWorkspace === '_current') return;
    // Eager mark on (re)entry — only when it actually changed since last
    // time, so re-renders that don't change the active workspace don't
    // re-fire the store write.
    if (lastMarkedWsRef.current !== activeWorkspace) {
      lastMarkedWsRef.current = activeWorkspace;
      markWorkspaceViewed(activeWorkspace);
    }
    const h = setInterval(() => {
      // The active workspace is stable for this effect's lifetime; the
      // periodic refresh just keeps viewedAt ahead of fresh in-pane
      // activity. 30s (vs the old 5s) cuts the write churn 6x while
      // staying well under any human "why is my current workspace
      // showing unread" perception window.
      markWorkspaceViewed(activeWorkspace);
    }, 30000);
    return () => clearInterval(h);
  }, [activeWorkspace, markWorkspaceViewed, ownerVisible]);

  useEffect(() => {
    if (!ctxMenu) return;
    const onAway = (e: MouseEvent | KeyboardEvent) => {
      if (e instanceof KeyboardEvent) {
        if (e.key === 'Escape') setCtxMenu(null);
        return;
      }
      const target = e.target as Node | null;
      if (target && ctxMenuRef.current?.contains(target)) return;
      setCtxMenu(null);
    };
    window.addEventListener('mousedown', onAway, { capture: true });
    window.addEventListener('keydown', onAway as EventListener);
    return () => {
      window.removeEventListener('mousedown', onAway, { capture: true });
      window.removeEventListener('keydown', onAway as EventListener);
    };
  }, [ctxMenu]);

  useEffect(() => {
    if (!sectionCtx) return;
    const onAway = (e: MouseEvent | KeyboardEvent) => {
      if (e instanceof KeyboardEvent) {
        if (e.key === 'Escape') setSectionCtx(null);
        return;
      }
      const target = e.target as Node | null;
      if (target && sectionCtxRef.current?.contains(target)) return;
      setSectionCtx(null);
    };
    window.addEventListener('mousedown', onAway, { capture: true });
    window.addEventListener('keydown', onAway as EventListener);
    return () => {
      window.removeEventListener('mousedown', onAway, { capture: true });
      window.removeEventListener('keydown', onAway as EventListener);
    };
  }, [sectionCtx]);

  const named = useMemo(() => {
    const filtered = workspaces.filter((w) => w.name !== '_current');
    const byName = Object.fromEntries(filtered.map((w) => [workspaceKey(w), w]));
    const byLegacyName = Object.fromEntries(filtered.map((w) => [w.name, w]));
    const ordered: typeof filtered = [];
    for (const key of wsOrder) {
      const w = byName[key] ?? byLegacyName[key];
      if (w) {
        ordered.push(w);
        delete byName[workspaceKey(w)];
      }
    }
    const leftovers = Object.values(byName).sort((a, b) =>
      workspaceKey(a).localeCompare(workspaceKey(b), undefined, { sensitivity: 'base' }),
    );
    return [...ordered, ...leftovers];
  }, [workspaces, wsOrder]);

  function reorderWorkspaces(fromKey: string, toKey: string, position: 'before' | 'after') {
    if (fromKey === toKey) return;
    const current = named.map(workspaceKey);
    const filteredOrder = current.filter((n) => n !== fromKey);
    const toIdx = filteredOrder.indexOf(toKey);
    if (toIdx < 0) return;
    const insertAt = position === 'before' ? toIdx : toIdx + 1;
    filteredOrder.splice(insertAt, 0, fromKey);
    setWsOrder(filteredOrder);
  }

  const background = useMemo(() => {
    const claimedByNamed = new Set<string>();
    for (const w of named) {
      for (const id of w.paneIds) claimedByNamed.add(id);
    }

    const visibleIds = new Set(paneIdsInOrder(layout));
    return Object.values(panes)
      .filter(
        (p) =>
          // Suspended-resumable panes count too: after a full teardown
          // (reboot) the daemon parks Claude panes as exited+resumable, and
          // any that no workspace claims would otherwise be stranded
          // invisibly — this list is their only recovery surface (reveal →
          // Resume button).
          (p.status === 'running' || (p.status === 'exited' && p.resumable)) &&
          (!isRouterMode() || !activeBackend || !p.backend || p.backend === activeBackend) &&
          p.role !== 'task_owner' && !managedOwnerBindings.has(p.id) &&
          !visibleIds.has(p.id) &&
          !claimedByNamed.has(p.id),
      )
      .sort((a, b) => (b.lastActivityMs || 0) - (a.lastActivityMs || 0));
  }, [activeBackend, layout, named, panes, managedOwnerBindings]);

  async function commitCreate(rawName: string) {
    const name = rawName.trim();
    setCreating(false);
    setCreateDraft('');
    if (!name) return;
    if (name === '_current') {
      toast('"_current" is reserved — pick a different name', { kind: 'error', icon: '⚠', ttlMs: 4000 });
      return;
    }
    if (named.some((w) => w.name.toLowerCase() === name.toLowerCase())) {
      toast(`"${name}" already exists`, { kind: 'error', icon: '⚠', ttlMs: 4000 });
      return;
    }
    if (activeWorkspace !== '_current') {
      clearLayout();
    }
    await saveWorkspace(name);
  }

  async function killAllBackground() {
    if (background.length === 0) return;
    const total = background.length;
    const ok = await confirmDialog({
      message:
        `Close all ${total} detached session${total === 1 ? '' : 's'}?\n\n` +
        'These panes are still running but not in any workspace. ' +
        'Closing them stops whatever is running in them.\n\n' +
        'This cannot be undone.',
      confirmLabel: 'Close all',
      danger: true,
    });
    if (!ok) return;
    const results = await Promise.all(
      background.map((p) => killPane(p.id, { silent: true })),
    );
    const killed = results.filter(Boolean).length;
    const failed = total - killed;
    if (failed === 0) {
      toast(
        `Closed ${killed} detached session${killed === 1 ? '' : 's'}`,
        { kind: 'success', icon: '✓' },
      );
    } else {
      toast(
        `Closed ${killed} of ${total}. ${failed} failed; try again to close ${failed === 1 ? 'it' : 'them'}.`,
        { kind: 'error', icon: '⚠', ttlMs: 6000 },
      );
    }
  }

  const middleList = useMemo(
    () => withoutTeamOnly(effectiveMiddleOrder(sidebarPrefs?.order, sidebarPrefs?.hidden, teamFeatures), teamFeatures, teamPlan).filter(key => key !== 'owners' && sectionOffered(key)),
    [ sidebarPrefs?.order, sidebarPrefs?.hidden, teamFeatures, teamPlan],
  );
  const hiddenSections = useMemo(
    () => withoutTeamOnly(hiddenMiddleKeys(sidebarPrefs?.order, sidebarPrefs?.hidden, teamFeatures), teamFeatures, teamPlan).filter(key => key !== 'owners' && sectionOffered(key)),
    [ sidebarPrefs?.order, sidebarPrefs?.hidden, teamFeatures, teamPlan],
  );
  // hide/show base their edit on the EFFECTIVE hidden set (see effectiveHidden):
  // toggling a section for the first time from the shipped default materializes
  // DEFAULT_HIDDEN into an explicit set instead of dropping it.
  // In personal view the sidebar shows the public arrangement, not the user's
  // own, so hide/show must not overwrite their saved one.
  const hideSection = (key: MiddleKey) => {
    if (personalView) return;
    setPrefs((p) => {
      const next = new Set(effectiveHidden(p.sidebar?.order, p.sidebar?.hidden, teamFeatures));
      next.add(key);
      return { ...p, sidebar: { ...p.sidebar, hidden: [...next] } };
    });
  };
  const showSection = (key: MiddleKey) => {
    if (personalView) return;
    setPrefs((p) => {
      const next = new Set(effectiveHidden(p.sidebar?.order, p.sidebar?.hidden, teamFeatures));
      next.delete(key);
      return { ...p, sidebar: { ...p.sidebar, hidden: [...next] } };
    });
  };
  // Make a section findable on demand: un-hide it (no-op when visible),
  // un-collapse it, and scroll its slot into view. Used by the mobile footer's
  // Schedules button — on a phone there's no right-click path to re-show a
  // hidden section, so the button must work regardless of saved prefs.
  const revealSection = (key: MiddleKey) => {
    // A section this install doesn't offer stays out, prefs untouched.
    if (withoutTeamOnly([key], teamFeatures, teamPlan).length === 0 || !sectionOffered(key)) return;
    showSection(key);
    if (!isOpen(key)) toggleMiddle(key);
    // Wait a beat for the prefs update to render the slot before scrolling.
    // Both the desktop sidebar and the mobile drawer mount a Sidebar, so the
    // slot exists twice — scroll the VISIBLE one (offsetParent is null for the
    // display:none'd desktop panel on phones, and vice versa).
    window.setTimeout(() => {
      for (const el of document.querySelectorAll<HTMLElement>(`[data-section-key="${key}"]`)) {
        if (el.offsetParent !== null) {
          el.scrollIntoView({ behavior: 'smooth', block: 'start' });
          break;
        }
      }
    }, 120);
  };
  // `solu:reveal-sidebar-section` lets a surface that isn't the sidebar send a
  // user to a section that may be hidden — today the Autonomy overview's
  // "owned by Credentials / Schedules" links. The listener lives HERE because
  // Sidebar is always mounted: a listener inside the target section would be
  // dropped for exactly the users who hid it (the SchedulesSection note on
  // solu:open-schedule-dialog records the same lesson).
  const revealSectionRef = useRef(revealSection);
  revealSectionRef.current = revealSection;
  useEffect(() => {
    const onReveal = (e: Event) => {
      const key = (e as CustomEvent<{ key?: string }>).detail?.key;
      if (typeof key === 'string' && isMiddleKey(key)) revealSectionRef.current(key);
    };
    window.addEventListener('solu:reveal-sidebar-section', onReveal);
    return () => window.removeEventListener('solu:reveal-sidebar-section', onReveal);
  }, []);
  const openSectionCtx = (e: ReactMouseEvent, key: MiddleKey) => {
    e.preventDefault();
    e.stopPropagation();
    setSectionCtx({ x: e.clientX, y: e.clientY, key });
  };
  // Fallback right-click for the sidebar's unclaimed space so "Customize
  // sidebar…" is reachable everywhere, not just on the middle sections.
  // Section slots and workspace rows call preventDefault for their own
  // (richer) menus, so `defaultPrevented` gates this to genuinely empty
  // areas — the workspace header/list padding, the ARIA zone, inter-zone
  // gaps and the footer. Skip inputs so native copy/paste menus survive in
  // the rename/create fields.
  const openSidebarCtx = (e: ReactMouseEvent) => {
    if (!isUnclaimedSidebarSpace(e.defaultPrevented, e.target as Element | null)) return;
    e.preventDefault();
    setSectionCtx({ x: e.clientX, y: e.clientY, key: null });
  };

  const renderMiddleSection = (key: MiddleKey) => {
    switch (key) {
      case 'owners': return null;
      case 'agentPanes':
        return (
          <AgentPanesSection
            panes={allPanes}
            collapsed={!isOpen('agentPanes')}
            onToggle={() => toggleMiddle('agentPanes')}
            onRevealPane={(paneId) => { void revealPane(paneId); }}
            onKillPane={(paneId) => killPane(paneId)}
          />
        );
      case 'background':
        // No background sessions ⇒ the section renders nothing. Return null (not
        // an empty <BackgroundSessionsSection/>) so the slot map below can drop
        // the slot entirely — otherwise an empty collapsed slot still draws its
        // border-bottom, doubling the divider next to the section above it.
        if (background.length === 0) return null;
        return (
          <BackgroundSessionsSection
            panes={background}
            collapsed={!isOpen('background')}
            onToggle={() => toggleMiddle('background')}
            onKillAll={killAllBackground}
            onRevealPane={(paneId) => { void revealPane(paneId); }}
            onKillPane={(paneId) => { void killPane(paneId); }}
          />
        );
      case 'ports':
        return (
          <div className="sidebar-section">
            <SectionHeader
              icon={<IconGlobe size={14} />}
              title="Services"
              count={serviceCount || undefined}
              subtitle="Web apps in this workspace"
              className="section-header--compact-extra"
              collapsed={!isOpen('ports')}
              onToggle={() => toggleMiddle('ports')}
              extra={
                <>
                  <button
                    className="port-new-browser"
                    title="Open a new browser tile (any URL — use the address bar)"
                    aria-label="New Tile"
                    onClick={() => useStore.getState().openBrowserHere('')}
                  ><IconPlus size={12} /><span className="section-extra-label">New Tile</span></button>
                </>
              }
            />
            {isOpen('ports') && <PortPanel />}
          </div>
        );
      case 'files':
        return (
          <FilesSection
            collapsed={!isOpen('files')}
            onToggle={() => toggleMiddle('files')}
          />
        );
      case 'schedules':
        return (
          <SchedulesSection
            collapsed={!isOpen('schedules')}
            onToggle={() => toggleMiddle('schedules')}
          />
        );
      case 'mcp':
        return (
          <MCPSection
            collapsed={!isOpen('mcp')}
            onToggle={() => toggleMiddle('mcp')}
          />
        );
      default: {
        // Sections registered by optional features (extensions.ts).
        const Section = sidebarSectionSlot(key);
        return Section ? <Section collapsed={!isOpen(key)} onToggle={() => toggleMiddle(key)} /> : null;
      }
    }
  };

  // Space model: two zones (Workspace + Middle) + footer. Exactly ONE grows,
  // putting space where the user is working — an open middle section wins, else
  // the workspace list; if the workspace is collapsed with nothing open, the
  // (header-only) middle absorbs the slack so there's no dead gap above the
  // footer. The workspace list is otherwise content-sized and capped at 45% so
  // a long list can't swallow the sidebar. Multi-open: the middle scrolls
  // internally with sticky section headers (see .sidebar-zone-middle
  // .section-header), so opening Files AND Ports keeps both reachable.
  const middleOpen = middleList.some((k) => openMiddle.has(k));
  const grower: 'middle' | 'workspace' =
    middleOpen ? 'middle' : !wsCollapsed ? 'workspace' : 'middle';
  const workspaceFlex = wsCollapsed ? '0 0 auto' : grower === 'workspace' ? '1 1 0' : '0 1 auto';
  const workspaceMax = wsCollapsed || grower === 'workspace' ? undefined : '45%';
  const middleFlex = grower === 'middle' ? '1 1 0' : '0 0 auto';
  // When the user has drag-resized the workspace zone AND there are panels to
  // share space with (workspace expanded, a middle section open), honor that
  // fixed height; otherwise fall back to the default flex sizing.
  const wsResizable = !wsCollapsed && middleOpen;
  const workspaceStyle = wsResizable && wsHeight != null
    ? { flex: `0 0 ${wsHeight}px`, maxHeight: 'none' }
    : { flex: workspaceFlex, maxHeight: workspaceMax };

  return (
    <div className="sidebar" onContextMenu={openSidebarCtx}>
      <div className="sidebar-content">
        {/* Zone 1 — Workspace. is-open/has-resize drive the same card
            treatment the middle-zone slots get (see .sidebar-zone-workspace
            rules in styles.css). */}
        <div
          className={`sidebar-zone sidebar-zone-workspace${wsCollapsed ? '' : ' is-open'}${wsResizable ? ' has-resize' : ''}`}
          style={workspaceStyle}
        >
          <WorkspaceSection
            collapsed={wsCollapsed}
            named={named}
            panes={panes}
            activeWorkspace={activeWorkspace}
            activeBackend={activeBackend}
            layout={layout}
            focusedPaneId={focusedPaneId}
            wsLastViewedMs={wsLastViewedMs}
            creating={creating}
            createDraft={createDraft}
            createCancelRef={createCancelRef}
            dropTarget={wsDropTarget}
            renamingName={renamingName}
            renameDraft={renameDraft}
            ctxMenu={ctxMenu}
            ctxMenuRef={ctxMenuRef}
            onToggle={toggleWs}
            onToggleCollapse={onToggleCollapse}
            onStartCreate={() => {
              setCreateDraft('');
              setCreating(true);
            }}
            onCommitCreate={(name) => { void commitCreate(name); }}
            setCreateDraft={setCreateDraft}
            setCreating={setCreating}
            setDropTarget={setWsDropTarget}
            setRenamingName={setRenamingName}
            setRenameDraft={setRenameDraft}
            setCtxMenu={setCtxMenu}
            onLoadWorkspace={(w) => {
              const storeActiveBackend = useStore.getState().activeBackend;
              if (isRouterMode() && w.backend && w.backend !== storeActiveBackend) {
                void useStore.getState().switchBackend(w.backend, w.name);
                return;
              }
              if (useStore.getState().mainPresentation.kind === 'owner') {
                useStore.getState().returnToWorkspace();
                if (useStore.getState().activeWorkspace === w.name) return;
              }
              void loadWorkspace(w.name);
            }}
            onRenameWorkspace={renameWorkspace}
            onSetWorkspaceContextPack={setWorkspaceContextPack}
            onDeleteWorkspace={(name) => { void deleteWorkspace(name); }}
            onClearLayout={clearLayout}
            onReorderWorkspaces={reorderWorkspaces}
          />
        </div>

        {/* Resize the split between the Workspaces panel and the sections below.
            Only shown when there are panels to trade space with. Its previous
            sibling (the workspace zone) is the element it drags. */}
        {wsResizable && (
          <div
            className="sidebar-section-resize sidebar-zone-resize"
            role="separator"
            aria-orientation="horizontal"
            aria-label="Resize the Workspaces panel"
            title="Drag to resize the Workspaces panel · double-click to reset"
            onPointerDown={startWsResize}
            onDoubleClick={resetWsHeight}
          />
        )}

        {/* Zone 2 — Middle accordion. All section headers stay visible;
            only the open section's body renders. */}
        <div
          className={`sidebar-zone sidebar-zone-middle${middleOpen ? ' has-open' : ''}`}
          style={{ flex: middleFlex }}
        >
          {(() => {
            // Render each section first, then drop the ones that opted out
            // (returned null — e.g. Background sessions with none live). Deriving
            // the open-set + fill target from the SURVIVING list keeps an empty
            // section from leaving a phantom bordered slot AND from stealing the
            // fill slot when it happens to be the last "open" key.
            const rendered = middleList
              .map((key) => ({ key, node: renderMiddleSection(key) }))
              .filter((r) => r.node != null);
            const openKeys = rendered.filter((r) => openMiddle.has(r.key)).map((r) => r.key);
            const lastOpenKey = openKeys[openKeys.length - 1];
            return rendered.map(({ key, node }) => {
              const open = isOpen(key);
              // The last open section has no cap or handle; every other open
              // one carries its drag-set height as a max-height CAP (content
              // shorter than the cap shrink-wraps; taller scrolls) + a handle.
              const isFill = open && key === lastOpenKey;
              return (
                <div
                  key={key}
                  data-section-key={key}
                  className={`sidebar-section-slot${open ? ' is-open' : ''}${isFill ? ' is-fill' : ''}`}
                  style={open && !isFill ? { maxHeight: heightFor(key) } : undefined}
                  onContextMenu={(e) => openSectionCtx(e, key)}
                >
                  <div
                    className="sidebar-section-scroll"
                    ref={open ? undefined : clearScrolled}
                    onScroll={open ? onSectionScroll : undefined}
                  >
                    {node}
                  </div>
                  {open && !isFill && (
                    <div
                      className="sidebar-section-resize"
                      role="separator"
                      aria-orientation="horizontal"
                      aria-label={`Resize the ${MIDDLE_SECTION_LABELS[key]} section`}
                      title="Drag to resize this section · double-click to reset"
                      onPointerDown={(e) => startResize(e, key)}
                      onDoubleClick={() => resetSectionHeight(key)}
                    />
                  )}
                </div>
              );
            });
          })()}
        </div>
      </div>

      <AccountFooter
        onOpenHelp={onOpenHelp}
        onOpenUsage={() => setUsagePageOpen(true)}
        onOpenSystem={() => useStore.getState().setSystemPageOpen(true)}
        onOpenSchedules={() => revealSection('schedules')}
      />

      {usagePageOpen && <UsagePage onClose={() => setUsagePageOpen(false)} />}

      {sectionCtx && (
        <div
          className="pane-ctx-menu"
          role="menu"
          ref={sectionCtxRef}
          style={{ position: 'fixed', left: sectionCtxPos.left, top: sectionCtxPos.top, zIndex: 1000 }}
        >
          {sectionCtx.key !== null && (
            <button type="button" onClick={() => { hideSection(sectionCtx.key as MiddleKey); setSectionCtx(null); }}>
              Hide “{MIDDLE_SECTION_LABELS[sectionCtx.key]}”
            </button>
          )}
          {hiddenSections.length > 0 && (
            <>
              {sectionCtx.key !== null && <div className="pane-ctx-menu-sep" />}
              {hiddenSections.map((k) => (
                <button key={k} type="button" onClick={() => { showSection(k); setSectionCtx(null); }}>
                  Show “{MIDDLE_SECTION_LABELS[k]}”
                </button>
              ))}
              <div className="pane-ctx-menu-sep" />
            </>
          )}
          <button type="button" onClick={() => { useSettings.getState().openSettings('sidebar'); setSectionCtx(null); }}>
            Customize sidebar…
          </button>
        </div>
      )}

    </div>
  );
}
