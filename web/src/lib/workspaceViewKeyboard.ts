/** Temporary views yield Escape to terminals and editors. Only workspace
 * chrome (or the document with no focused control) owns the exit shortcut. */
export function canExitWorkspaceView(target: EventTarget | null, focused: Element | null): boolean {
  const editing = (node: EventTarget | null) => node instanceof Element && (
    node.closest('input, textarea, select, [role="textbox"], .xterm, .xterm-helper-textarea, [contenteditable]:not([contenteditable="false"])') !== null ||
    (node as HTMLElement).isContentEditable
  );
  if (editing(target) || editing(focused)) return false;
  if (!focused || focused === document.body || focused === document.documentElement) return true;
  return focused.closest('[role="tree"], .sidebar, .topbar, .pane-header, .pane-empty, .statusbar, .mobile-bottombar') !== null;
}
