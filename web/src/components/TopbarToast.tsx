import { useLayoutEffect, useRef, useState } from 'react';
import { useStore } from '../store';

// When the toast position is "top-center", notifications render HERE — inline in
// the title bar's otherwise-empty center (a drag region) — as a compact pill,
// instead of a floating card below the bar. Puts that dead space to work.
//
// The bar is only ~36px tall, so it shows the most-recent few toasts side by
// side (as many as the free center gap fits), with any older overflow as a
// leading "+N" chip. The floating ToastHost stack is suppressed for this
// position (see ToastHost); auto-dismiss timers still live in ToastHost. When
// there's no toast, this is just the draggable center region.

export function TopbarToast() {
  const toasts = useStore((s) => s.toasts);
  const position = useStore((s) => s.toastPosition);
  const dismissToast = useStore((s) => s.dismissToast);
  const revealPane = useStore((s) => s.revealPane);

  // The toast row lives IN FLOW inside .topbar-center — the flex spacer between
  // the left and right button clusters — so it can use the WHOLE free gap,
  // asymmetric or not, and only ellipsizes when that gap is genuinely too narrow.
  // (The old build centered an absolute overlay on the true window midpoint and
  // clamped its width to twice the SMALLER side gap, so a wide-left / narrow-right
  // title bar cut messages off even with empty space sitting on the other side.)
  // We still measure the gap — but only to decide how MANY pills to show before
  // the rest fold into a "+N" chip; the actual truncation is pure CSS now.
  const centerRef = useRef<HTMLDivElement>(null);
  const [centerW, setCenterW] = useState(540);
  useLayoutEffect(() => {
    const el = centerRef.current;
    if (!el || typeof ResizeObserver === 'undefined') return;
    const measure = () => setCenterW(el.clientWidth);
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // Side-by-side: show the most-recent few toasts TOGETHER rather than one pill
  // that flashes and gets replaced (with a "+N" you can't read). How many fit is
  // a function of the free center gap — roughly one per 150px, capped at
  // MAX_INLINE — so a narrow window shows fewer instead of cramming. Any older
  // overflow collapses into a single leading "+N" chip.
  const MAX_INLINE = 3;
  const fitCount = Math.max(1, Math.min(MAX_INLINE, Math.floor(centerW / 150)));
  const visible = position === 'top-center' ? toasts.slice(-fitCount) : [];
  const overflow = toasts.length - visible.length;

  return (
    <div className="topbar-center" data-tauri-drag-region ref={centerRef}>
      {visible.length > 0 && (
        // In-flow row, centered within the free gap by .topbar-center's own
        // justify-content. pointer-events pass through the row itself so the
        // drag region still works — only the pills / "+N" chip capture clicks.
        <div className="topbar-toast-anchor">
          {overflow > 0 && (
            <span
              className="topbar-toast-more"
              title={`${overflow} earlier notification${overflow === 1 ? '' : 's'}`}
            >
              +{overflow}
            </span>
          )}
          {visible.map((t) => (
            <button
              key={t.id}
              type="button"
              className={`topbar-toast toast-${t.kind}${t.action || t.paneId ? ' topbar-toast-clickable' : ''}`}
              data-tauri-drag-region={false}
              onClick={(e) => {
                // The click is the gesture the gated opener checks (embed
                // links, ST-133/134), as ToastHost passes it.
                if (t.action) t.action.run(e.nativeEvent);
                else if (t.paneId) void revealPane(t.paneId);
                dismissToast(t.id);
              }}
              title={t.action ? t.action.label : t.paneId ? 'Go to the pane' : 'Dismiss notification'}
              aria-label={`${t.action ? t.action.label : t.paneId ? 'Go to pane' : 'Dismiss notification'}: ${t.message}`}
            >
              <span className="topbar-toast-dot" aria-hidden="true" />
              <span className="topbar-toast-msg">{t.message}</span>
              {t.action
                ? <span className="topbar-toast-go" aria-hidden="true">{t.action.label}</span>
                : (t.paneId) && <span className="topbar-toast-go" aria-hidden="true">→</span>}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
