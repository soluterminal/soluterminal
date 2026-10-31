import { afterEach, expect, it } from 'vitest';
import { canExitWorkspaceView } from './workspaceViewKeyboard';
afterEach(() => document.body.replaceChildren());

it('yields Escape to xterm, including focused non-editable descendants', () => {
  const terminal = document.createElement('div'); terminal.className = 'xterm';
  const helper = document.createElement('textarea'); helper.className = 'xterm-helper-textarea';
  const screen = document.createElement('div'); screen.tabIndex = 0;
  terminal.append(helper, screen); document.body.append(terminal);
  for (const element of [helper, screen]) {
    element.focus();
    expect(canExitWorkspaceView(element, document.activeElement)).toBe(false);
    expect(canExitWorkspaceView(document.body, document.activeElement)).toBe(false);
  }
});
it('yields Escape to all editable controls and contenteditable descendants', () => {
  for (const tag of ['input', 'textarea', 'select']) {
    const input = document.createElement(tag); document.body.append(input); input.focus();
    expect(canExitWorkspaceView(input, document.activeElement)).toBe(false);
  }
  const editor = document.createElement('div'); editor.setAttribute('contenteditable', 'true');
  const child = document.createElement('span'); editor.append(child); document.body.append(editor);
  expect(canExitWorkspaceView(child, editor)).toBe(false);
});
it('allows Escape from tree, sidebar and view chrome, or with nothing focused', () => {
  for (const className of ['sidebar', 'topbar', 'pane-header', 'statusbar', 'mobile-bottombar']) {
    const parent = document.createElement('div'); parent.className = className;
    const button = document.createElement('button'); parent.append(button); document.body.append(parent); button.focus();
    expect(canExitWorkspaceView(button, document.activeElement)).toBe(true);
  }
  const tree = document.createElement('div'); tree.setAttribute('role', 'tree'); tree.tabIndex = 0; document.body.append(tree); tree.focus();
  expect(canExitWorkspaceView(tree, document.activeElement)).toBe(true);
  expect(canExitWorkspaceView(document.body, document.body)).toBe(true);
  expect(canExitWorkspaceView(null, null)).toBe(true);
  const unrelated = document.createElement('button'); document.body.append(unrelated); unrelated.focus();
  expect(canExitWorkspaceView(unrelated, document.activeElement)).toBe(false);
});
