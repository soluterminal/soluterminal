import { createContext, useContext, useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode, type RefObject } from 'react';
import { createPortal, flushSync } from 'react-dom';

// Secondary details collapse one at a time, least important first, until the
// header fits. Lower number = more important = last to move into "+N". Every
// PaneDetail names its own rank so the order is reviewable in one place.
export const DETAIL_PRIORITY = {
  autoMode: 1,    // what the pane may do unattended; changes what you must watch
  role: 2,        // orchestrator / task owner: who this pane is in the squad
  credential: 3,  // which account pays for the pane
  schedule: 4,    // a prompt is about to fire
  review: 5,      // verdict on the pane's work
  message: 6,     // message from the lead (also carries the origin when combined)
  identity: 7,    // @handle and working-on; the title usually says the same
  origin: 8,      // who started the pane
  mode: 9,        // renderer toggle (only on panes that need a way back)
  pack: 10,       // context-pack status, informational
  resume: 11,     // copy the session id; also in the pane menu
  memory: 12,     // display-only RSS readout
  maximise: 13,   // Alt+Z and double-clicking the header do the same
} as const;

export interface DetailMetric { id: string; priority: number; width: number }

// Room kept for the "+N" button (its width plus one flex gap), and how much
// extra room a collapsed item needs before it comes back. The latter stops a
// one-pixel jitter at a boundary width from toggling an item on every layout.
export const MORE_RESERVE = 48;
export const RESTORE_MARGIN = 8;
// A hidden flex item that CSS reveals only while measuring: a 20000px margin
// plus its own 1px box forces overflow, so scrollWidth reports the content
// width even when the header has room to spare. (A ::after does not count.)
export const MEASURE_SENTINEL = 20001;

// Decide which details move into "+N". `fixed` is everything that never
// collapses (title reserve, actions, attention); `prevCount` is how many were
// hidden last time. Details leave the row lowest priority first; `hidden` is
// returned most important first. `wrap` means even with every detail hidden the
// attention group does not fit beside the title, so it takes its own row.
export function chooseCollapsed(available: number, fixed: number, items: DetailMetric[], prevCount = 0) {
  const sorted = [...items].sort((a, b) => a.priority - b.priority);
  const n = sorted.length;
  const needed = (k: number) => fixed + sorted.slice(0, n - k).reduce((sum, item) => sum + item.width, 0) + (k > 0 ? MORE_RESERVE : 0);
  const fits = (k: number, slack: number) => needed(k) <= available + slack;
  let k = Math.min(prevCount, n);
  if (fits(k, 1)) {
    // Not monotonic at the bottom: one hidden item pays for the +N button, so look for any smaller count.
    for (let j = 0; j < k; j++) if (fits(j, -RESTORE_MARGIN)) { k = j; break; }
  }
  else { while (k < n) { k++; if (fits(k, 1)) break; } }
  return { hidden: sorted.slice(n - k).map(item => item.id), wrap: needed(n) > available + 1 };
}

const px = (value: string) => parseFloat(value) || 0;

// Synchronous, paint-free measurement: full labels, a ten-character title, no
// shrinking and no wrapping; every detail (including collapsed ones) shown.
function measureDetails(header: HTMLElement) {
  header.dataset.measuringDetails = '';
  try {
    const gap = px(getComputedStyle(header).columnGap);
    const items: DetailMetric[] = [];
    for (const el of header.querySelectorAll<HTMLElement>('[data-pane-detail]')) {
      const parentGap = el.parentElement && el.parentElement !== header ? px(getComputedStyle(el.parentElement).columnGap) : gap;
      let width = 0;
      for (const child of el.children) {
        const w = (child as HTMLElement).offsetWidth;
        if (w > 0) width += w + parentGap;
      }
      if (width > 0) items.push({ id: el.dataset.detailId!, priority: Number(el.dataset.priority), width });
    }
    // scrollWidth stops at the last item; the header's end padding still needs room.
    const required = header.scrollWidth - MEASURE_SENTINEL - gap + px(getComputedStyle(header).paddingRight);
    return { items, fixed: Math.max(0, required - items.reduce((sum, item) => sum + item.width, 0)) };
  } finally { delete header.dataset.measuringDetails; }
}

const NONE: readonly string[] = [];
const DetailsContext = createContext<{ hidden: readonly string[]; slots: Record<string, HTMLElement>; enabled: boolean; close: () => void } | null>(null);

const DetailVisibilityContext = createContext(true);
export function usePaneDetailVisible() { return useContext(DetailVisibilityContext); }

export function PaneDetail({ priority, children }: { priority: number; children: ReactNode }) {
  const ctx = useContext(DetailsContext);
  const id = useId();
  const inline = useRef<HTMLSpanElement>(null);
  if (ctx && !ctx.enabled) return <>{children}</>;
  const collapsed = !!ctx?.hidden.includes(id);
  const slot = collapsed ? ctx!.slots[id] : undefined;
  return <>
    <span ref={inline} data-pane-detail="" data-detail-id={id} data-priority={priority} hidden={collapsed}><DetailVisibilityContext.Provider value={!collapsed}>{children}</DetailVisibilityContext.Provider></span>
    {slot && !!inline.current?.childElementCount && createPortal(<div className="pane-detail-body"
      onClick={e => e.stopPropagation()} onMouseDown={e => e.stopPropagation()} onDoubleClick={e => e.stopPropagation()}
      onKeyDown={e => { e.stopPropagation(); if (e.key === 'Escape') { e.preventDefault(); ctx!.close(); } }}><DetailVisibilityContext.Provider value={true}>{children}</DetailVisibilityContext.Provider></div>, slot)}
  </>;
}

export function PaneHeaderOverflow({ headerRef, children, enabled = true }: { headerRef: RefObject<HTMLDivElement>; children: ReactNode; enabled?: boolean }) {
  const [hidden, setHidden] = useState<readonly string[]>(NONE);
  const [open, setOpen] = useState(false);
  const lastHidden = useRef<readonly string[]>(NONE);
  const [panel, setPanel] = useState<HTMLDivElement | null>(null);
  const [slots, setSlots] = useState<Record<string, HTMLElement>>({});
  const slotRefs = useRef(new Map<string, (el: HTMLElement | null) => void>());
  const button = useRef<HTMLButtonElement>(null);
  const [pos, setPos] = useState({ top: 0, left: 0 });
  const count = hidden.length;

  const slotRef = (id: string) => {
    let ref = slotRefs.current.get(id);
    if (!ref) {
      ref = el => setSlots(prev => {
        if (el ? prev[id] === el : !(id in prev)) return prev;
        const next = { ...prev };
        if (el) next[id] = el; else delete next[id];
        return next;
      });
      slotRefs.current.set(id, ref);
    }
    return ref;
  };

  useEffect(() => {
    const header = headerRef.current;
    const apply = (next: readonly string[]) => {
      const prev = lastHidden.current;
      if (prev.length === next.length && prev.every((id, i) => id === next[i])) return;
      lastHidden.current = next; setHidden(next);
      if (!next.length) setOpen(false);
    };
    if (!enabled) { header?.removeAttribute('data-details-overflow'); apply(NONE); setOpen(false); return; }
    if (!header || typeof ResizeObserver === 'undefined') return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let disposed = false;
    const measure = () => {
      const available = header.clientWidth;
      const { items, fixed } = measureDetails(header);
      const { hidden: next, wrap } = chooseCollapsed(available, fixed, items, lastHidden.current.length);
      header.toggleAttribute('data-details-overflow', wrap);
      apply(next);
    };
    const schedule = () => { if (disposed) return; clearTimeout(timer); timer = setTimeout(measure, 100); };
    measure();
    let lastWidth = header.clientWidth;
    const resize = new ResizeObserver(() => {
      const width = header.clientWidth;
      if (width !== lastWidth) {
        lastWidth = width; setOpen(false);
        // A width change must collapse before the next paint. The debounce
        // left the old collapse set in place for 100ms+ (and for a whole drag,
        // as every resize reset the timer), so the actions overflowed the
        // header. This callback runs after layout and before paint; commit now.
        clearTimeout(timer);
        if (!disposed) flushSync(measure);
        return;
      }
      // Attention can add a row without moving the anchor. Preserve open
      // chip dialogs (and their drafts) through these height-only changes.
      schedule();
    });
    resize.observe(header);
    // No resize observer per chip. Store updates and expiring message/schedule
    // labels can change detail presence without changing the header's box.
    const mutations = new MutationObserver(records => {
      const changedWidth = records.some(record => {
        // Our own "+N" button coming and going is not a content change.
        if (record.type === 'childList' && [...record.addedNodes, ...record.removedNodes].every(node => (node as Element).classList?.contains('pane-details-more'))) return false;
        if (record.type !== 'characterData' || !record.target.parentElement?.closest('.pane-autoapproving-chip')) return true;
        const before = record.oldValue?.match(/^\s*approving in (\d+)s\s*$/);
        const after = record.target.textContent?.match(/^\s*approving in (\d+)s\s*$/);
        // Countdown digits are tabular. Only a new digit count or phase can
        // change its width; don't force layout on every remaining-second tick.
        return !before || !after || before[1].length !== after[1].length;
      });
      if (changedWidth) schedule();
    });
    mutations.observe(header, { childList: true, characterData: true, characterDataOldValue: true, subtree: true });
    document.fonts?.ready.then(schedule);
    return () => { disposed = true; resize.disconnect(); mutations.disconnect(); clearTimeout(timer); header.removeAttribute('data-details-overflow'); };
  }, [headerRef, enabled]);

  useLayoutEffect(() => {
    if (!open || !button.current || !panel) return;
    const rect = button.current.getBoundingClientRect();
    const box = panel.getBoundingClientRect();
    setPos({ top: Math.max(8, Math.min(rect.bottom + 6, window.innerHeight - box.height - 8)), left: Math.max(8, Math.min(rect.right - box.width, window.innerWidth - box.width - 8)) });
    panel.focus();
  }, [open, panel]);

  useEffect(() => {
    if (!open) return;
    const dismiss = (e: MouseEvent) => {
      // Nested chip dialogs are portaled too: allow their buttons to work.
      if (e.target instanceof Element && e.target.closest('[role="dialog"], [role="menu"]')) return;
      if (!button.current?.contains(e.target as Node)) setOpen(false);
    };
    const scroll = (e: Event) => {
      // Only scrolling an ancestor can move the anchor. Streaming xterm
      // viewports elsewhere must not dismiss these controls or nested menus.
      if (e.target === document || (e.target instanceof Element && e.target.contains(button.current))) setOpen(false);
    };
    const resize = () => setOpen(false);
    document.addEventListener('click', dismiss);
    window.addEventListener('scroll', scroll, true);
    window.addEventListener('resize', resize);
    return () => { document.removeEventListener('click', dismiss); window.removeEventListener('scroll', scroll, true); window.removeEventListener('resize', resize); };
  }, [open]);

  const close = () => { setOpen(false); button.current?.focus(); };
  return <DetailsContext.Provider value={{ hidden, slots, enabled, close }}>
    {children}
    {enabled && <span data-details-sentinel="" aria-hidden="true" />}
    {count > 0 && <button ref={button} type="button" className="pane-details-more" aria-label={`${count} more pane details`} aria-haspopup="dialog" aria-expanded={open}
      onMouseDown={e => e.stopPropagation()} onDoubleClick={e => e.stopPropagation()}
      onClick={e => { e.stopPropagation(); setOpen(v => !v); }}
      onKeyDown={e => { e.stopPropagation(); if (e.key === 'Escape') close(); }}>+{count}</button>}
    {open && count > 0 && createPortal(<div ref={setPanel} className="pane-details-popover" role="dialog" aria-label="More pane details" tabIndex={-1}
      style={{ top: pos.top, left: pos.left }} onClick={e => e.stopPropagation()} onMouseDown={e => e.stopPropagation()} onDoubleClick={e => e.stopPropagation()}
      onKeyDown={e => { e.stopPropagation(); if (e.key === 'Escape') { e.preventDefault(); close(); } }}>
      {hidden.map(id => <div key={id} ref={slotRef(id)} className="pane-detail-row" />)}
    </div>, document.body)}
  </DetailsContext.Provider>;
}
