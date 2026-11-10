// Watch window.devicePixelRatio for display-scale changes: moving the window
// to a monitor with a different scale, changing Windows display scaling, or
// browser zoom. A `(resolution: Ndppx)` media query only matches the ratio it
// was built for, so it fires once on the change and is then re-armed for the
// new ratio.

export function watchDevicePixelRatio(onChange: (dpr: number) => void): () => void {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return () => {};
  let disposed = false;
  let mql: MediaQueryList | null = null;
  const onMediaChange = () => {
    if (disposed) return;
    arm();
    onChange(window.devicePixelRatio || 1);
  };
  const arm = () => {
    mql?.removeEventListener('change', onMediaChange);
    try {
      mql = window.matchMedia(`(resolution: ${window.devicePixelRatio || 1}dppx)`);
      mql.addEventListener('change', onMediaChange);
    } catch {
      mql = null;
    }
  };
  arm();
  return () => {
    disposed = true;
    mql?.removeEventListener('change', onMediaChange);
    mql = null;
  };
}
