import type { ReactNode } from 'react';
import type { StateCreator } from 'zustand';
import type { State } from '../store';


/** One in-app dialog request, mirroring the toast queue model. Replaces
 *  native window.confirm/prompt/alert (which no-op in Tauri's WebView2).
 *  Only ever one at a time: openDialog resolves any prior request as
 *  cancelled before installing the new one. `resolve` settles the Promise
 *  returned by the lib/dialog helpers — boolean for confirm, string|null
 *  for prompt, and (ignored) for alert. */
export interface DialogRequest {
  id: number;
  kind: 'confirm' | 'prompt' | 'alert' | 'select';
  title?: string;
  message: string;
  confirmLabel?: string;
  cancelLabel?: string;
  danger?: boolean;
  defaultValue?: string;
  placeholder?: string;
  options?: string[];
  selectLabel?: string;
  /** Confirm dialogs only: Escape, a click outside, or being replaced by
   *  another dialog resolve null instead of false, so the caller can tell a
   *  dismissal apart from the Cancel button. */
  distinguishDismiss?: boolean;
  resolve: (value: boolean | string | null) => void;
}

export type ToastKind = 'info' | 'success' | 'warn' | 'error';
/** Where the transient toast stack anchors. User-configurable (default
 *  bottom-right — least-intrusive, most-conventional). */
export type ToastPosition = 'bottom-right' | 'bottom-left' | 'top-right' | 'top-center';
export interface Toast {
  id: number;
  message: string;
  kind: ToastKind;
  icon?: ReactNode;
  /** How long it shows (default 3 s); 0 keeps it until the person dismisses it. */
  ttlMs?: number;
  /** When set, clicking the toast reveals this pane (loads its workspace
   *  if needed, or adopts it from Background) and focuses it. The chip
   *  renders with a navigable affordance (pointer cursor + arrow) when
   *  this is present. */
  paneId?: string;
  /** When set, clicking the toast opens this URL in the in-app browser
   *  tile (openBrowserHere) — the "dev server just came up → preview it"
   *  affordance. Same navigable treatment as paneId; paneId wins if both
   *  are ever set. A plain string (not a callback) so the notification-log
   *  copy stays serializable and can't go stale. */
  browserUrl?: string;
  /** When set, clicking opens the workflow Tasks surface at this decision. */
  workflowTaskId?: string;
  /** An inline action ("Undo", "Reopen"): clicking the toast runs it. Wins
   *  over the navigable targets above. Not copied to the notification log —
   *  a callback can go stale once the toast is gone. */
  action?: { label: string; run: () => void };
}
/** A persisted-in-session copy of every toast, so the alerts bell can act as a
 *  notification center — transient toasts auto-dismiss, but the user can review
 *  what they missed here. In-memory only (resets on reload), like the toasts. */
export interface NotificationEntry {
  id: number;
  ts: number;
  message: string;
  kind: ToastKind;
  icon?: ReactNode;
  paneId?: string;
  browserUrl?: string;
  workflowTaskId?: string;
}

export interface ToastDialogSlice {
  toasts: Toast[];
  toast: (message: string, opts?: { kind?: ToastKind; icon?: ReactNode; ttlMs?: number; paneId?: string; browserUrl?: string; workflowTaskId?: string; logToRecent?: boolean; action?: { label: string; run: () => void } }) => void;
  dismissToast: (id: number) => void;

  /** Where the toast stack anchors (persisted). */
  toastPosition: ToastPosition;
  setToastPosition: (p: ToastPosition) => void;
  /** Notification center backing the alerts bell — a session log of every toast,
   *  newest first, capped. `unreadNotifications` counts entries since the bell
   *  was last opened; markNotificationsSeen zeroes it. */
  notificationLog: NotificationEntry[];
  unreadNotifications: number;
  markNotificationsSeen: () => void;
  clearNotificationLog: () => void;

  // In-app modal dialog — single source of truth for confirm/prompt/alert,
  // rendered by <DialogHost>. Native window.confirm/prompt/alert no-op in
  // Tauri's WebView2, so every call site routes through openDialog (usually
  // via the lib/dialog.ts async helpers). Only one dialog at a time.
  dialog: DialogRequest | null;
  openDialog: (req: Omit<DialogRequest, 'id'>) => void;
  closeDialog: (value: boolean | string | null) => void;
}

// Toast anchor, persisted per-browser. Default bottom-right (least-intrusive,
// most-conventional). Unknown/absent → default.
const TOAST_POSITION_KEY = 'solu.toastPosition';
const TOAST_POSITIONS: ToastPosition[] = ['bottom-right', 'bottom-left', 'top-right', 'top-center'];
const toastPositionDefault: ToastPosition = (() => {
  try {
    const raw = localStorage.getItem(TOAST_POSITION_KEY) as ToastPosition | null;
    return raw && TOAST_POSITIONS.includes(raw) ? raw : 'bottom-right';
  } catch { return 'bottom-right'; }
})();
const NOTIFICATION_LOG_CAP = 40;

let nextToastId = 1;
let nextDialogId = 1;

export const createToastDialogSlice: StateCreator<State, [], [], ToastDialogSlice> = (set, get) => ({
  toasts: [],
  toastPosition: toastPositionDefault,
  setToastPosition: (p) => {
    try { localStorage.setItem(TOAST_POSITION_KEY, p); } catch { /* ignore */ }
    set({ toastPosition: p });
  },
  notificationLog: [],
  unreadNotifications: 0,
  markNotificationsSeen: () => set({ unreadNotifications: 0 }),
  clearNotificationLog: () => set({ notificationLog: [], unreadNotifications: 0 }),
  toast: (message, opts = {}) => {
    const id = nextToastId++;
    const t: Toast = {
      id,
      message,
      kind: opts.kind ?? 'info',
      icon: opts.icon,
      ttlMs: opts.ttlMs,
      paneId: opts.paneId,
      browserUrl: opts.browserUrl,
      // Task decisions open the team-only Tasks console; a public install
      // shows the message without a link into it.

      action: opts.action,
    };
    const entry: NotificationEntry = {
      id, ts: Date.now(), message: t.message, kind: t.kind, icon: t.icon, paneId: t.paneId,
      browserUrl: t.browserUrl,
      workflowTaskId: t.workflowTaskId,
    };
    set((s) => {
      // Drop an immediately-prior duplicate (same kind + message): ARIA
      // and push sources can fire the same observation in tight bursts
      // and stacking identical copies is noise, not signal.
      const prev = s.toasts.length > 0 ? s.toasts[s.toasts.length - 1] : null;
      const base = prev && prev.kind === t.kind && prev.message === t.message
        ? s.toasts.slice(0, -1)
        : s.toasts;
      // Cap visible queue so a burst doesn't pile up into a scrolling
      // list — newer wins, older drops. Errors retained preferentially
      // by sliding the window from the end.
      const MAX_VISIBLE = 3;
      const next = [...base, t];
      // Notification-center log: newest-first, capped. Collapse an
      // immediately-prior duplicate the same way the toast queue does so a
      // burst reads as one entry (and counts as one unread).
      //
      // Some toasts opt OUT of Recent (logToRecent:false): agent/schedule
      // run-results already live in the Inbox, so logging them here just
      // duplicates that surface. They still show transiently — they just
      // don't accumulate in the history or the counter.
      const logToRecent = opts.logToRecent !== false;
      const logPrev = s.notificationLog[0];
      const logDup = logPrev && logPrev.kind === entry.kind && logPrev.message === entry.message;
      const logBase = logDup ? s.notificationLog.slice(1) : s.notificationLog;
      const log = logToRecent ? [entry, ...logBase].slice(0, NOTIFICATION_LOG_CAP) : s.notificationLog;
      // Badge only for things you might have MISSED and would act on — warn/error.
      // info/success (e.g. "apply ok", "dev server up") still get logged to Recent
      // but don't drive a persistent unread count; the attention bell and inbox own
      // the actionable "needs you" signals, so an info-toast counter is just noise.
      const bumpsUnread = logToRecent && !logDup && (entry.kind === 'warn' || entry.kind === 'error');
      return {
        toasts: next.length > MAX_VISIBLE ? next.slice(-MAX_VISIBLE) : next,
        notificationLog: log,
        unreadNotifications: bumpsUnread ? s.unreadNotifications + 1 : s.unreadNotifications,
      };
    });
  },
  dismissToast: (id) => {
    set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) }));
  },

  dialog: null,
  openDialog: (req) => {
    set((s) => {
      // Resolve any already-open dialog as cancelled before replacing it,
      // so its awaiting Promise never leaks.
      if (s.dialog) s.dialog.resolve(s.dialog.kind === 'confirm' && !s.dialog.distinguishDismiss ? false : null);
      return { dialog: { ...req, id: nextDialogId++ } };
    });
  },
  closeDialog: (value) => {
    const cur = get().dialog;
    if (cur) cur.resolve(value);
    set({ dialog: null });
  },
});
