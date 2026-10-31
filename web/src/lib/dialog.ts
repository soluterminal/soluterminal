// In-app dialog helpers — drop-in async replacements for native
// window.confirm / window.prompt / window.alert, which silently no-op in
// Tauri's WebView2 (no dialog renders, confirmations get skipped). These
// route through the store's openDialog action so the modal renders via
// <DialogHost>, and resolve when the user picks an action. Usable from
// ANY file — components and plain non-component functions/hooks — because
// they reach the store imperatively via getState().
import { useStore } from '../store';

/** Replacement for window.confirm. Resolves true only on explicit confirm;
 *  cancel / backdrop / Escape resolve false. */
export function confirmDialog(opts: {
  title?: string;
  message: string;
  confirmLabel?: string;
  cancelLabel?: string;
  danger?: boolean;
}): Promise<boolean> {
  return new Promise((resolve) =>
    useStore.getState().openDialog({
      kind: 'confirm',
      ...opts,
      resolve: (v) => resolve(v === true),
    }),
  );
}

/** A confirm that tells the Cancel button apart from a dismissal: resolves
 *  true on confirm, false on the Cancel button, and null on Escape, a click
 *  outside, or when another dialog replaces it. */
export function confirmOrDismissDialog(opts: {
  title?: string;
  message: string;
  confirmLabel?: string;
  cancelLabel?: string;
}): Promise<boolean | null> {
  return new Promise((resolve) =>
    useStore.getState().openDialog({
      kind: 'confirm',
      ...opts,
      distinguishDismiss: true,
      resolve: (v) => resolve(v === true ? true : v === false ? false : null),
    }),
  );
}

/** Replacement for window.prompt. Resolves the entered string on confirm,
 *  or null on cancel / backdrop / Escape. */
export function promptDialog(opts: {
  title?: string;
  message: string;
  defaultValue?: string;
  placeholder?: string;
  confirmLabel?: string;
}): Promise<string | null> {
  return new Promise((resolve) =>
    useStore.getState().openDialog({
      kind: 'prompt',
      ...opts,
      resolve: (v) => resolve(typeof v === 'string' ? v : null),
    }),
  );
}

/** Choose one existing value; cancellation resolves to null. */
export function selectDialog(opts: {
  title: string;
  message: string;
  options: string[];
  selectLabel?: string;
  confirmLabel?: string;
}): Promise<string | null> {
  return new Promise((resolve) =>
    useStore.getState().openDialog({
      kind: 'select', ...opts,
      resolve: (value) => resolve(typeof value === 'string' ? value : null),
    }),
  );
}

/** Replacement for window.alert. Resolves when the user acknowledges.
 *  Accepts either an options object or a bare message string. */
export function alertDialog(
  opts: { title?: string; message: string; confirmLabel?: string } | string,
): Promise<void> {
  const o = typeof opts === 'string' ? { message: opts } : opts;
  return new Promise((resolve) =>
    useStore.getState().openDialog({
      kind: 'alert',
      ...o,
      resolve: () => resolve(),
    }),
  );
}
