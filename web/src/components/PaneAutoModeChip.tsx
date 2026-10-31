import { useEffect, useLayoutEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type MouseEvent as ReactMouseEvent } from 'react';
import { createPortal } from 'react-dom';
import type { PaneAutoMode, PaneInfo } from '../types';
import { AUTO_MODES, autoModeBadgeLabel, autoModeDescription, autoModeMenuLabel } from '../lib/autoMode';
import { applyAutoModeChange } from '../lib/autoModeConfirm';

const EDGE_MARGIN = 8;

// The pane-header auto-approve chip. Clicking it opens a small menu with the
// three modes; it never changes the mode by itself. Choosing Always approve
// asks for an explicit confirmation first (confirmAutoModeChange). The chip
// sits next to the title, which is double-clicked to rename, so the second
// click of a double-click is ignored on the chip and on the menu: a
// double-click can open the menu, never pick from it.
export function PaneAutoModeChip({ pane }: { pane: PaneInfo }) {
  const cur = (pane.autoMode ?? '') as PaneAutoMode;
  const btnRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(false);
  const [pos, setPos] = useState<{ left: number; top: number }>({ left: 0, top: 0 });

  // Below the chip, or above it when there is no room below. Never over it,
  // so a fast second click on the chip can't land on a menu item.
  useLayoutEffect(() => {
    if (!open) return;
    const btn = btnRef.current;
    const menu = menuRef.current;
    if (!btn || !menu) return;
    const r = btn.getBoundingClientRect();
    const m = menu.getBoundingClientRect();
    let top = r.bottom + 4;
    if (top + m.height > window.innerHeight - EDGE_MARGIN) top = Math.max(EDGE_MARGIN, r.top - 4 - m.height);
    let left = r.left;
    if (left + m.width > window.innerWidth - EDGE_MARGIN) left = Math.max(EDGE_MARGIN, window.innerWidth - EDGE_MARGIN - m.width);
    setPos({ left, top });
    menu.querySelector<HTMLElement>('[aria-checked="true"]')?.focus();
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      const t = e.target as Node;
      if (menuRef.current?.contains(t) || btnRef.current?.contains(t)) return;
      setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      e.stopPropagation();
      setOpen(false);
      btnRef.current?.focus();
    };
    document.addEventListener('mousedown', onDown, true);
    document.addEventListener('keydown', onKey, true);
    return () => {
      document.removeEventListener('mousedown', onDown, true);
      document.removeEventListener('keydown', onKey, true);
    };
  }, [open]);

  const choose = (mode: PaneAutoMode) => {
    setOpen(false);
    void applyAutoModeChange(pane, mode);
  };

  // Header drag, title rename and pane focus all listen above us; the menu is
  // portaled, but React still bubbles its events through this component.
  const stop = (e: ReactMouseEvent) => e.stopPropagation();

  const onMenuKey = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
    e.preventDefault();
    const items = Array.from(menuRef.current?.querySelectorAll<HTMLElement>('[role="menuitemradio"]') ?? []);
    const i = items.indexOf(document.activeElement as HTMLElement);
    const next = e.key === 'ArrowDown' ? (i + 1) % items.length : (i - 1 + items.length) % items.length;
    items[next]?.focus();
  };

  return (
    <>
      <button
        ref={btnRef}
        type="button"
        className={`pane-automode-badge pane-automode-${cur || 'off'}`}
        aria-label={`Auto-approve: ${autoModeMenuLabel(cur)}. Change auto-approve`}
        aria-haspopup="menu"
        aria-expanded={open}
        title={`Auto-approve — ${autoModeDescription(cur)}\nClick to change`}
        onMouseDownCapture={stop}
        onDoubleClick={stop}
        onClick={(e) => {
          e.stopPropagation();
          // The second click of a double-click: leave the menu as it is.
          if (e.detail > 1) return;
          setOpen((v) => !v);
        }}
      >
        {autoModeBadgeLabel(cur)}
      </button>
      {open && createPortal(
        <div
          ref={menuRef}
          className="pane-ctx-menu pane-automode-menu"
          role="menu"
          aria-label="Auto-approve"
          style={{ position: 'fixed', left: pos.left, top: pos.top, zIndex: 'var(--z-menu)' }}
          onMouseDownCapture={stop}
          onDoubleClick={stop}
          onClick={stop}
          onContextMenu={stop}
          onKeyDown={onMenuKey}
        >
          <div className="pane-ctx-menu-label">Auto-approve</div>
          {AUTO_MODES.map((mode) => {
            const checked = mode === cur;
            return (
              <button
                key={mode || 'off'}
                type="button"
                role="menuitemradio"
                aria-checked={checked}
                className={checked ? 'is-checked' : ''}
                title={autoModeDescription(mode)}
                onClick={(e) => {
                  e.stopPropagation();
                  if (e.detail > 1) return;
                  choose(mode);
                }}
              >
                <span className="pane-ctx-menu-check" aria-hidden>{checked ? '●' : '○'}</span>
                {autoModeMenuLabel(mode)}{mode === 'always' && !checked ? '…' : ''}
              </button>
            );
          })}
        </div>,
        document.body,
      )}
    </>
  );
}
