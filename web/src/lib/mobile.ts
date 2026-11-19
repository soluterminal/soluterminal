import { isEmbedded } from '../embed';
const DEFAULT_MOBILE_BREAKPOINT_PX = 800;

/** The SoluTerminal desktop window (Tauri). Browser hosts such as SoluBrowse
 *  also inject Tauri globals into external pages; only our own shell marker,
 *  outside an embed, identifies it. It is never a phone, even when narrow. */
export function isDesktopShellWindow(): boolean {
  if (typeof window === 'undefined') return false;
  const w = window as unknown as { __SOLU_TAURI__?: unknown };
  return !!w.__SOLU_TAURI__ && !isEmbedded();
}

export function isMobileViewport(breakpoint = DEFAULT_MOBILE_BREAKPOINT_PX): boolean {
  if (typeof window === 'undefined') return false;
  if (isDesktopShellWindow()) return false;

  const width = window.visualViewport?.width ?? window.innerWidth;
  const height = window.visualViewport?.height ?? window.innerHeight;
  const coarsePointer = window.matchMedia?.('(pointer: coarse)').matches ?? false;

  return width < breakpoint || (coarsePointer && Math.min(width, height) < breakpoint);
}

export function isMobileLandscapeViewport(breakpoint = DEFAULT_MOBILE_BREAKPOINT_PX): boolean {
  if (typeof window === 'undefined') return false;

  // Use the layout viewport for orientation. visualViewport.height shrinks
  // when the soft keyboard opens, so a portrait phone can otherwise look
  // "landscape" and incorrectly trigger the rotate overlay while typing.
  const width = window.innerWidth;
  const height = window.innerHeight;
  const coarsePointer = window.matchMedia?.('(pointer: coarse)').matches ?? false;

  return coarsePointer && width > height && Math.min(width, height) < breakpoint;
}

export function isProbablyMobileDevice(breakpoint = DEFAULT_MOBILE_BREAKPOINT_PX): boolean {
  if (typeof window === 'undefined') return false;

  const width = window.visualViewport?.width ?? window.innerWidth;
  const coarsePointer = window.matchMedia?.('(pointer: coarse)').matches ?? false;
  const touchPoints = navigator.maxTouchPoints ?? 0;

  return (coarsePointer || touchPoints > 1) && width < breakpoint;
}

// Soft-keyboard height in CSS px, derived from the visual viewport. Single
// source of truth for the keyboard inset — consumed BOTH by the bars'
// `bottom: var(--keyboard-inset)` (via syncVisualViewportCssVars below) and by
// useKeyboardInsetShift's pane transform, so the two never drift apart.
export function computeKeyboardInset(): number {
  if (typeof window === 'undefined') return 0;
  const vv = window.visualViewport;
  const height = vv?.height ?? window.innerHeight;
  const offsetTop = vv?.offsetTop ?? 0;
  return Math.max(0, window.innerHeight - height - offsetTop);
}

export function syncVisualViewportCssVars(): void {
  if (typeof window === 'undefined' || typeof document === 'undefined') return;
  const vv = window.visualViewport;
  const root = document.documentElement;
  const width = vv?.width ?? window.innerWidth;
  const height = vv?.height ?? window.innerHeight;
  const offsetTop = vv?.offsetTop ?? 0;
  const offsetLeft = vv?.offsetLeft ?? 0;
  const keyboardInset = computeKeyboardInset();

  root.style.setProperty('--vvw', `${width}px`);
  root.style.setProperty('--vvh', `${height}px`);
  root.style.setProperty('--vv-offset-top', `${offsetTop}px`);
  root.style.setProperty('--vv-offset-left', `${offsetLeft}px`);
  root.style.setProperty('--keyboard-inset', `${keyboardInset}px`);
}

// Escape hatch for the mobile "pin the xterm helper textarea off-screen" fix
// (the textarea otherwise chases the cursor during active output, which makes
// the browser pan-to-caret thrash the view while typing during streaming). The
// pin is ON by default (it's the fix); the actual pinning is a mobile CSS rule
// gated on `body:not(.no-textarea-pin)`. Disable it INSTANTLY without a
// redeploy via any of:
//   - console:    document.body.classList.add('no-textarea-pin')   (this session)
//   - URL:        ?textareapin=0   (sticky — persisted to localStorage)
//   - localStorage.setItem('solu.textareaPin','0'); then reload
// Re-enable with ?textareapin=1 (or remove the localStorage key).
export function syncTextareaPinPref(): void {
  if (typeof window === 'undefined' || typeof document === 'undefined') return;
  // The pin is for phones; a narrow desktop window (600-799px) also matches
  // the mobile media query but needs the textarea at the caret for its IME
  // candidate bar (feedback #73).
  document.body.classList.toggle('desktop-shell', isDesktopShellWindow());
  try {
    const q = new URLSearchParams(window.location.search).get('textareapin');
    if (q === '0') localStorage.setItem('solu.textareaPin', '0');
    else if (q === '1') localStorage.removeItem('solu.textareaPin');
    const disabled = localStorage.getItem('solu.textareaPin') === '0';
    document.body.classList.toggle('no-textarea-pin', disabled);
  } catch {
    /* ignore */
  }
}
