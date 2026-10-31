import type { PaneAutoMode } from '../types';

// Single source of truth for Auto-approve mode copy, shared by the pane-header
// chip (a three-option menu), the pane context-menu radio, and the command
// palette, so all three surfaces describe the SAME three modes identically.
// Moving a pane to Always approve goes through confirmAutoModeChange
// (lib/autoModeConfirm.ts) on every surface.

// Radio / menu order, least → most permissive.
export const AUTO_MODES: readonly PaneAutoMode[] = ['', 'cautious', 'always'];

// Full-word label for menus / palette / radio rows.
export function autoModeMenuLabel(mode: PaneAutoMode): string {
  if (mode === '') return 'Off (manual)';
  if (mode === 'cautious') return 'Approve with caution';
  return 'Always approve';
}

// Compact chip label for the header. Each mode reads differently, so the chip
// never says "auto" while auto-approve is off: "manual" (off), "auto?"
// (cautious: only when low-risk) and "AUTO" (always). The chip's styling
// (greyed / dotted outline / solid box) repeats the distinction.
export function autoModeBadgeLabel(mode: PaneAutoMode): string {
  if (mode === '') return 'manual';
  if (mode === 'cautious') return 'auto?';
  return 'AUTO';
}

// One-line explanation of what a mode does — shared tooltip copy.
export function autoModeDescription(mode: PaneAutoMode): string {
  if (mode === '') return 'off — you approve every prompt manually';
  if (mode === 'cautious') {
    return 'approve with caution — only low-risk prompts are auto-approved';
  }
  return 'always approve — recognized tool permissions are approved with no cancel window; questions and plan changes still need you';
}

// The palette's one-step toggle: Off ↔ Approve with caution, and Always back
// to Off. It never steps INTO Always approve; that is its own confirmed
// action, so no repeated or accidental activation can land there.
export function nextAutoMode(mode: PaneAutoMode): PaneAutoMode {
  return mode === '' ? 'cautious' : '';
}
