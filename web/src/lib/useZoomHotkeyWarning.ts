import { useEffect } from 'react';
import { useStore } from '../store';
import { globalHotkeyOwned } from '../tauri-browser';
import { prettyCombo } from '../keybindings';

// Pane-zoom hotkey conflict detector (desktop only). NVIDIA's and AMD's
// GPU overlays both claim Alt+Z as an EXCLUSIVE OS-level hotkey — when
// one of them holds it, Windows swallows the chord before the webview
// ever sees a keydown, so zoom just "stops working" with zero visible
// cause (the overlay UI often doesn't even open — a half-disabled
// overlay keeps the registration without responding). Cost a full
// debugging session on 2026-07-09; this hook turns that into a toast.
//
// Probes the EFFECTIVE zoom chord (default or user override) once per
// mount and again whenever the user rebinds it. Warns once per chord —
// dismissal persists in localStorage, and the flag clears whenever the
// chord probes free so a future re-grab (GPU driver updates re-enable
// overlays) warns again. The toast also lands in the alerts bell, so a
// missed 12s window is still reviewable.

function warnedKey(combo: string): string {
  return `solu.hotkeyOwnedWarned.${combo}`;
}

export function useZoomHotkeyWarning(): void {
  const combo = useStore((s) => s.customKeybindings['pane.zoom'] ?? 'alt+z');

  useEffect(() => {
    let cancelled = false;
    void globalHotkeyOwned(combo).then((owned) => {
      if (cancelled) return;
      if (!owned) {
        // Chord is free — re-arm the warning for this combo.
        try { localStorage.removeItem(warnedKey(combo)); } catch { /* ignore */ }
        return;
      }
      try {
        if (localStorage.getItem(warnedKey(combo)) === '1') return;
        localStorage.setItem(warnedKey(combo), '1');
      } catch { /* ignore — worst case the toast repeats next boot */ }
      useStore.getState().toast(
        `${prettyCombo(combo)} is reserved system-wide by another app (usually the NVIDIA/AMD overlay) — pane zoom can't receive it. Use Maximise pane in the pane menu, or press ? and rebind "Maximise / restore focused pane".`,
        { kind: 'warn', ttlMs: 12000 },
      );
    });
    return () => { cancelled = true; };
  }, [combo]);
}
