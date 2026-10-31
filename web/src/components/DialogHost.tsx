import { useEffect, useRef, useState } from 'react';
import { useStore } from '../store';
import { useFocusTrap } from './useFocusTrap';

// Single in-app modal that renders the current store dialog request and
// resolves it on the user's choice. Mounted once in App (next to
// <ToastHost>). Replaces native confirm/prompt/alert — those no-op in
// Tauri's WebView2 — so confirmations actually render and block.
export function DialogHost() {
  const dialog = useStore((s) => s.dialog);
  const closeDialog = useStore((s) => s.closeDialog);

  // Controlled input value for prompt dialogs. Re-seeded from defaultValue
  // each time a new dialog opens (keyed on dialog.id).
  const [value, setValue] = useState('');
  const inputRef = useRef<HTMLInputElement>(null);
  const selectRef = useRef<HTMLSelectElement>(null);
  const confirmRef = useRef<HTMLButtonElement>(null);
  const cardRef = useRef<HTMLDivElement>(null);

  // Keyboard-modal, not just visually modal: Tab must not walk into the app
  // behind the backdrop, and closing returns focus to whatever opened us.
  useFocusTrap(cardRef, !!dialog);

  useEffect(() => {
    if (!dialog) return;
    setValue(dialog.kind === 'select' ? (dialog.options?.[0] ?? '') : dialog.kind === 'prompt' ? (dialog.defaultValue ?? '') : '');
    // Autofocus the most likely target so Enter/Escape work immediately
    // and the user can type into a prompt without a click. Defer one frame
    // so the element is mounted.
    const h = window.requestAnimationFrame(() => {
      if (dialog.kind === 'prompt') {
        const el = inputRef.current;
        if (el) {
          el.focus();
          el.select();
        }
      } else if (dialog.kind === 'select') {
        selectRef.current?.focus();
      } else {
        confirmRef.current?.focus();
      }
    });
    return () => window.cancelAnimationFrame(h);
  }, [dialog?.id, dialog?.kind, dialog?.defaultValue]);

  if (!dialog) return null;

  const isPrompt = dialog.kind === 'prompt';
  const isSelect = dialog.kind === 'select';
  const isAlert = dialog.kind === 'alert';
  const confirmLabel = dialog.confirmLabel ?? (isAlert ? 'OK' : 'Confirm');
  const cancelLabel = dialog.cancelLabel ?? 'Cancel';

  const onConfirm = () => {
    if (isSelect && !dialog.options?.includes(value)) return;
    closeDialog(isPrompt || isSelect ? value : true);
  };
  const onCancel = () => closeDialog(isPrompt || isSelect ? null : false);
  // Escape and a click outside: the same as Cancel, unless the caller asked
  // to tell a dismissal apart (it then resolves null).
  const onDismiss = () => (dialog.distinguishDismiss ? closeDialog(null) : onCancel());

  // Keys are handled on the overlay (capture not needed) so they don't leak
  // through to global app handlers (CommandPalette, pane hotkeys) while the
  // modal is up. stopPropagation on every key; act only on Enter/Escape.
  const onKeyDown = (e: React.KeyboardEvent) => {
    e.stopPropagation();
    if (e.key === 'Enter') {
      // In a prompt, Enter in the input (or anywhere) submits. For
      // confirm/alert, Enter activates the focused confirm button.
      if (!e.repeat) {
        e.preventDefault();
        onConfirm();
      }
    } else if (e.key === 'Escape') {
      e.preventDefault();
      onDismiss();
    }
  };

  return (
    <div
      className="app-dialog-backdrop"
      role="presentation"
      onMouseDown={onDismiss}
      onKeyDown={onKeyDown}
    >
      <div
        ref={cardRef}
        className="app-dialog"
        role={isAlert ? 'alertdialog' : 'dialog'}
        aria-modal="true"
        aria-label={dialog.title}
        onMouseDown={(e) => e.stopPropagation()}
      >
        {dialog.title && <div className="app-dialog-title">{dialog.title}</div>}
        <div className="app-dialog-msg">{dialog.message}</div>
        {isPrompt && (
          <input
            ref={inputRef}
            className="app-dialog-input"
            value={value}
            placeholder={dialog.placeholder}
            onChange={(e) => setValue(e.target.value)}
          />
        )}
        {isSelect && (
          <select ref={selectRef} className="app-dialog-input" aria-label={dialog.selectLabel ?? 'Choose an option'}
            value={value} onChange={(event) => setValue(event.target.value)}>
            {dialog.options?.map((option) => <option key={option} value={option}>{option}</option>)}
          </select>
        )}
        <div className="app-dialog-actions">
          {!isAlert && (
            <button type="button" className="app-dialog-btn" onClick={onCancel}>
              {cancelLabel}
            </button>
          )}
          <button
            ref={confirmRef}
            type="button"
            className={`app-dialog-btn ${dialog.danger ? 'danger' : 'primary'}`}
            onClick={onConfirm}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
