import { createContext, useContext, useEffect, useLayoutEffect, useRef, useState, type ReactNode, type RefObject } from 'react';
import { createPortal } from 'react-dom';

// Collapse the secondary details as a group. Their actual rendered presence
// determines the count, including components that return null or expire later.
export function overflowDecision(available: number, required: number, count: number) {
  return count > 0 && required > available + 1 ? count : 0;
}

const DetailsContext = createContext<{ collapsed: boolean; panel: HTMLElement | null; enabled: boolean; close: () => void } | null>(null);

const DetailVisibilityContext = createContext(true);
export function usePaneDetailVisible() { return useContext(DetailVisibilityContext); }

export function PaneDetail({ children }: { children: ReactNode }) {
  const ctx = useContext(DetailsContext);
  const inline = useRef<HTMLSpanElement>(null);
  if (ctx && !ctx.enabled) return <>{children}</>;
  return <>
    <span ref={inline} data-pane-detail="" hidden={ctx?.collapsed}><DetailVisibilityContext.Provider value={!ctx?.collapsed}>{children}</DetailVisibilityContext.Provider></span>
    {ctx?.collapsed && ctx.panel && !!inline.current?.childElementCount && createPortal(<div className="pane-detail-row"
      onClick={e => e.stopPropagation()} onMouseDown={e => e.stopPropagation()} onDoubleClick={e => e.stopPropagation()}
      onKeyDown={e => { e.stopPropagation(); if (e.key === 'Escape') { e.preventDefault(); ctx.close(); } }}><DetailVisibilityContext.Provider value={true}>{children}</DetailVisibilityContext.Provider></div>, ctx.panel)}
  </>;
}

export function PaneHeaderOverflow({ headerRef, children, enabled = true }: { headerRef: RefObject<HTMLDivElement>; children: ReactNode; enabled?: boolean }) {
  const [count, setCount] = useState(0);
  const [open, setOpen] = useState(false);
  const lastCount = useRef(0);
  const [panel, setPanel] = useState<HTMLDivElement | null>(null);
  const button = useRef<HTMLButtonElement>(null);
  const [pos, setPos] = useState({ top: 0, left: 0 });

  useEffect(() => {
    const header = headerRef.current;
    if (!enabled) { header?.removeAttribute('data-details-overflow'); lastCount.current = 0; setCount(0); setOpen(false); return; }
    if (!header || typeof ResizeObserver === 'undefined') return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let disposed = false;
    const measure = () => {
      // Synchronous, paint-free measurement: use full labels, a ten-character
      // title, and no shrinking. Never render per animation frame while dragging.
      header.dataset.measuringDetails = '';
      const details = [...header.querySelectorAll<HTMLElement>('[data-pane-detail]')];
      const n = details.filter(el => [...el.children].some(child => child.getBoundingClientRect().width > 0)).length;
      const required = header.scrollWidth;
      delete header.dataset.measuringDetails;
      const next = overflowDecision(header.clientWidth, required, n);
      header.toggleAttribute('data-details-overflow', required > header.clientWidth + 1);
      if (lastCount.current !== next) { lastCount.current = next; setCount(next); if (!next) setOpen(false); }
    };
    const schedule = () => { if (disposed) return; clearTimeout(timer); timer = setTimeout(measure, 100); };
    measure();
    let lastWidth = header.clientWidth;
    const resize = new ResizeObserver(() => {
      const width = header.clientWidth;
      if (width !== lastWidth) { lastWidth = width; setOpen(false); }
      // Attention can add a row without moving the anchor. Preserve open
      // chip dialogs (and their drafts) through these height-only changes.
      schedule();
    });
    resize.observe(header);
    // No resize observer per chip. Store updates and expiring message/schedule
    // labels can change detail presence without changing the header's box.
    const mutations = new MutationObserver(records => {
      const changedWidth = records.some(record => {
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
  return <DetailsContext.Provider value={{ collapsed: enabled && count > 0, panel: open ? panel : null, enabled, close }}>
    {children}
    {count > 0 && <button ref={button} type="button" className="pane-details-more" aria-label={`${count} more pane details`} aria-haspopup="dialog" aria-expanded={open}
      onMouseDown={e => e.stopPropagation()} onDoubleClick={e => e.stopPropagation()}
      onClick={e => { e.stopPropagation(); setOpen(v => !v); }}
      onKeyDown={e => { e.stopPropagation(); if (e.key === 'Escape') close(); }}>+{count}</button>}
    {open && count > 0 && createPortal(<div ref={setPanel} className="pane-details-popover" role="dialog" aria-label="More pane details" tabIndex={-1}
      style={{ top: pos.top, left: pos.left }} onClick={e => e.stopPropagation()} onMouseDown={e => e.stopPropagation()} onDoubleClick={e => e.stopPropagation()}
      onKeyDown={e => { e.stopPropagation(); if (e.key === 'Escape') { e.preventDefault(); close(); } }} />, document.body)}
  </DetailsContext.Provider>;
}
