import { useEffect, useRef } from 'react';
import type { RefObject } from 'react';

// Everything Tab can land on. [tabindex="-1"] is excluded: those are
// programmatic-focus targets, not tab stops.
const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), ' +
  'select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

// Keep Tab / Shift+Tab cycling inside `ref` while `active`, and restore focus
// to the element that had it when the trap engaged (the opener) on release.
// Without this a modal lets Tab walk into the app behind it — the dialog is
// visually modal but not keyboard-modal, and a keyboard user ends up focused
// on something they cannot see.
//
// The listener is document-level CAPTURE on purpose: dialog overlays
// stopPropagation() on keys so they don't leak to global hotkey handlers,
// which would also swallow a bubble-phase trap listener.
// A parent modal can pause its trap while a confirmation is in front without
// releasing the original opener or restoring focus in the middle of that dialog.
export function useFocusTrap(ref: RefObject<HTMLElement | null>, active: boolean, paused = false) {
  const pausedRef = useRef(paused);
  pausedRef.current = paused;
  useEffect(() => {
    if (!active) return;
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const onKeyDown = (e: KeyboardEvent) => {
      if (pausedRef.current || e.key !== 'Tab') return;
      const root = ref.current;
      if (!root) return;
      // offsetParent filters display:none / detached candidates cheaply.
      const stops = Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
        (el) => el.offsetParent !== null,
      );
      if (stops.length === 0) {
        e.preventDefault();
        return;
      }
      const first = stops[0];
      const last = stops[stops.length - 1];
      const cur = document.activeElement;
      const inside = cur instanceof HTMLElement && root.contains(cur);
      if (e.shiftKey) {
        if (!inside || cur === first) {
          e.preventDefault();
          last.focus();
        }
      } else if (!inside || cur === last) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener('keydown', onKeyDown, true);
    return () => {
      document.removeEventListener('keydown', onKeyDown, true);
      // Restore focus to the opener so closing a dialog doesn't strand focus
      // on <body> (which breaks the next Tab press and screen-reader context).
      if (opener && document.contains(opener)) opener.focus();
    };
  }, [ref, active]);
}
