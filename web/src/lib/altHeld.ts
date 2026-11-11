// The title's tooltip carries the pane's number: the #N badge itself shows
// only while Alt is held.
export function paneTitleHint(paneIndex: number): string {
  return paneIndex > 0
    ? `#${paneIndex} · Alt+${paneIndex} focuses this pane · Double-click to rename`
    : 'Double-click to rename';
}

// While Alt is held, <html data-alt-held> lets CSS show each pane's #N, the
// number Alt+N focuses. One listener for the whole app; no React state, so
// holding Alt re-renders nothing.
export function installAltHeld(): () => void {
  const root = document.documentElement;
  const set = (held: boolean) => {
    if (held) root.dataset.altHeld = '';
    else delete root.dataset.altHeld;
  };
  // AltGr reports Ctrl+Alt on Windows; it types characters, so it never counts.
  const onKey = (e: KeyboardEvent) => set(e.altKey && !e.ctrlKey && !e.metaKey);
  // Alt+Tab takes the key-up with it: clear when the window loses focus.
  const clear = () => set(false);
  window.addEventListener('keydown', onKey, true);
  window.addEventListener('keyup', onKey, true);
  window.addEventListener('blur', clear);
  document.addEventListener('visibilitychange', clear);
  return () => {
    window.removeEventListener('keydown', onKey, true);
    window.removeEventListener('keyup', onKey, true);
    window.removeEventListener('blur', clear);
    document.removeEventListener('visibilitychange', clear);
    clear();
  };
}
