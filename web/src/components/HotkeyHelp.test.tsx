import { render } from '@testing-library/react';
import { expect, it } from 'vitest';
import { HotkeyHelp } from './HotkeyHelp';

// The sidebar rows match WorkspaceSection: one Expand all / Collapse all
// toggle in the Workspaces header, and Shift+F10 / Menu opening the focused
// row's own menu (a pane row's pane menu since #895).
it('describes the sidebar tree toggle and the row menu key as they work', () => {
  render(<HotkeyHelp open onClose={() => {}} />);
  const text = document.body.textContent ?? '';
  expect(text).toContain('Expand all / Collapse all — one toggle button in the Workspaces header');
  expect(text).toContain("Open the focused row's menu: the workspace menu on a workspace row, the pane menu on a pane row");
  expect(text).not.toContain('buttons above the workspace tree');
  expect(text).not.toContain('focus a workspace row first');
});
