import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { MouseEvent as ReactMouseEvent, ReactNode } from 'react';
import { useStore } from '../store';
import type { ToastKind } from '../store';
import {
  IconAlertTriangle, IconCheck, IconBell, IconArrowUp, IconRefresh, IconBolt,
  IconFileText, IconClock, IconPause, IconFolder, IconCircle, IconX,
  IconTerminalSquare, IconInfo, IconDiamond, IconShield,
} from './ui/icons';

const TOAST_ICON_STYLE = { verticalAlign: '-2px' } as const;

// Legacy call sites pass a single glyph string as `icon`. Toasts are the one
// remaining glyph hot-spot (hundreds of transient call sites), so rather than
// churn every one we resolve the mark centrally to the shared SVG set here —
// no toast renders an emoji, and call sites can now also pass an SVG node
// directly. Glyphs whose meaning the kind already conveys (⚠/✓) intentionally
// fall through to the kind default below.
const LEGACY_GLYPH_ICON: Record<string, ReactNode> = {
  '🔔': <IconBell size={14} style={TOAST_ICON_STYLE} />,
  '📢': <IconBell size={14} style={TOAST_ICON_STYLE} />,
  '⬆': <IconArrowUp size={14} style={TOAST_ICON_STYLE} />,
  '↑': <IconArrowUp size={14} style={TOAST_ICON_STYLE} />,
  '↻': <IconRefresh size={14} style={TOAST_ICON_STYLE} />,
  '⇄': <IconRefresh size={14} style={TOAST_ICON_STYLE} />,
  '⚡': <IconBolt size={14} style={TOAST_ICON_STYLE} />,
  '📄': <IconFileText size={14} style={TOAST_ICON_STYLE} />,
  '⏳': <IconClock size={14} style={TOAST_ICON_STYLE} />,
  '⏰': <IconClock size={14} style={TOAST_ICON_STYLE} />,
  '💤': <IconPause size={14} style={TOAST_ICON_STYLE} />,
  '📁': <IconFolder size={14} style={TOAST_ICON_STYLE} />,
  '○': <IconCircle size={14} style={TOAST_ICON_STYLE} />,
  '×': <IconX size={14} style={TOAST_ICON_STYLE} />,
  '⎋': <IconTerminalSquare size={14} style={TOAST_ICON_STYLE} />,
  '!': <IconInfo size={14} style={TOAST_ICON_STYLE} />,
  '🤖': <IconDiamond size={14} style={TOAST_ICON_STYLE} />,
  '◇': <IconDiamond size={14} style={TOAST_ICON_STYLE} />,
  '🦉': <IconShield size={14} style={TOAST_ICON_STYLE} />,
};

// Toasts get a mark from their kind by default (warn/error → alert, success →
// check); info stays iconless unless a semantic glyph/node is supplied.
function defaultToastIcon(kind: ToastKind): ReactNode {
  if (kind === 'error' || kind === 'warn') return <IconAlertTriangle size={14} style={TOAST_ICON_STYLE} />;
  if (kind === 'success') return <IconCheck size={14} style={TOAST_ICON_STYLE} />;
  return null;
}

function resolveToastIcon(icon: ReactNode, kind: ToastKind): ReactNode {
  // A supplied SVG node (new call sites) wins outright.
  if (icon != null && icon !== false && typeof icon !== 'string') return icon;
  // A legacy glyph string maps to the shared set; unmapped ones (rare one-offs
  // like 'Δ'/'$') defer to the kind default rather than rendering raw.
  if (typeof icon === 'string') return LEGACY_GLYPH_ICON[icon] ?? defaultToastIcon(kind);
  return defaultToastIcon(kind);
}

export function ToastHost() {
  const toasts = useStore((s) => s.toasts);
  const dismissToast = useStore((s) => s.dismissToast);
  const revealPane = useStore((s) => s.revealPane);
  const toastPosition = useStore((s) => s.toastPosition);

  // Top-right toasts must clear the top chrome (topbar, plus any banner in the
  // grid row above it that pushes the topbar down). Measure the topbar's live
  // bottom edge rather than hard-coding an offset — otherwise the stack lands
  // over the toolbar icons whenever a banner is up. Remeasured when a toast
  // appears (captures the current banner state) and on resize. (top-center
  // renders inline in the title bar via TopbarToast, not here.)
  const isTop = toastPosition === 'top-right';
  const [topOffset, setTopOffset] = useState(44);
  useLayoutEffect(() => {
    if (!isTop) return;
    const measure = () => {
      const tb = document.querySelector('.topbar');
      const bottom = tb ? tb.getBoundingClientRect().bottom : 36;
      setTopOffset(Math.round(bottom + 8));
    };
    measure();
    window.addEventListener('resize', measure);
    return () => window.removeEventListener('resize', measure);
  }, [isTop, toasts.length]);

  // Per-toast dismissal timers kept across renders so a new arrival
  // doesn't reset the countdown of toasts already on screen. Without
  // this, a burst of toasts arriving faster than ttlMs piles up: each
  // new render clears every existing timer and reschedules them,
  // indefinitely extending dismissal.
  const timersRef = useRef<Map<number, number>>(new Map());

  useEffect(() => {
    const seen = new Set(toasts.map((t) => t.id));
    for (const t of toasts) {
      // ttlMs 0: stays until the person dismisses it.
      if (timersRef.current.has(t.id) || t.ttlMs === 0) continue;
      const handle = window.setTimeout(() => {
        timersRef.current.delete(t.id);
        dismissToast(t.id);
      }, t.ttlMs ?? 3000);
      timersRef.current.set(t.id, handle);
    }
    for (const [id, handle] of timersRef.current) {
      if (!seen.has(id)) {
        clearTimeout(handle);
        timersRef.current.delete(id);
      }
    }
  }, [toasts, dismissToast]);

  useEffect(() => () => {
    for (const handle of timersRef.current.values()) clearTimeout(handle);
    timersRef.current.clear();
  }, []);

  // top-center is rendered inline in the title bar (TopbarToast); suppress the
  // floating stack for it. Dismiss timers above still run.
  if (toasts.length === 0 || toastPosition === 'top-center') return null;

  return (
    <div
      className={`toast-host pos-${toastPosition}`}
      role="status"
      aria-live="polite"
      style={isTop ? { top: topOffset } : undefined}
    >
      {toasts.map((t) => {
        const clickable = !!t.action || !!t.paneId;
        const iconNode = resolveToastIcon(t.icon, t.kind);
        const activateToast = (e: ReactMouseEvent) => {
          // Navigable toast -> reveal the referenced pane (loads its
          // workspace if needed, adopts from Background, or just
          // focuses if already in the current layout) — before
          // dismissing. Non-navigable toasts just dismiss.
          if (t.action) t.action.run(e.nativeEvent);
          else if (t.paneId) void revealPane(t.paneId);
          dismissToast(t.id);
        };
        const title = t.action ? t.action.label : t.paneId ? 'Go to the pane' : 'Dismiss notification';
        const label = t.action ? t.action.label : t.paneId ? 'Go to pane' : 'Dismiss notification';
        return (
          <button
            type="button"
            key={t.id}
            className={`toast toast-${t.kind} toast-dismissible ${clickable ? 'toast-clickable' : ''}`}
            onClick={activateToast}
            title={title}
            aria-label={`${label}: ${t.message}`}
          >
            {iconNode && <span className="toast-icon" aria-hidden="true">{iconNode}</span>}
            <span className="toast-msg">{t.message}</span>
            {t.action
              ? <span className="toast-go toast-action-label" aria-hidden>{t.action.label}</span>
              : clickable && <span className="toast-go" aria-hidden>→</span>}
          </button>
        );
      })}
    </div>
  );
}
