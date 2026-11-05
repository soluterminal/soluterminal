import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { useStore } from '../store';
import { confirmDialog, confirmOrDismissDialog, promptDialog } from '../lib/dialog';
import { DialogHost } from './DialogHost';

afterEach(() => {
  act(() => useStore.getState().closeDialog(null));
});

function open<T>(ask: () => Promise<T>): Promise<T> {
  let p!: Promise<T>;
  act(() => { p = ask(); });
  return p;
}

describe('DialogHost dismissal', () => {
  it.each([{ label: 'Cancel', expected: false }, { label: 'Confirm', expected: true }])(
    'lets Enter activate the focused $label button natively', async ({ label, expected }) => {
      render(<DialogHost />);
      const answer = open(() => confirmDialog({ message: 'Sure?' }));
      const button = screen.getByRole('button', { name: label });
      button.focus();
      // jsdom does not synthesize the native keyboard click. Verify the key
      // remains uncancelled, then deliver that click; Chromium covers both.
      expect(fireEvent.keyDown(button, { key: 'Enter', cancelable: true })).toBe(true);
      expect(useStore.getState().dialog).not.toBeNull();
      fireEvent.click(button);
      await expect(answer).resolves.toBe(expected);
    },
  );

  it('still submits a prompt when Enter is pressed in its input', async () => {
    render(<DialogHost />);
    const answer = open(() => promptDialog({ message: 'Name?', defaultValue: 'Preview' }));
    fireEvent.keyDown(screen.getByRole('textbox'), { key: 'Enter' });
    await expect(answer).resolves.toBe('Preview');
  });

  it('a plain confirm treats Escape like Cancel', async () => {
    render(<DialogHost />);
    const answer = open(() => confirmDialog({ message: 'Sure?' }));
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    await expect(answer).resolves.toBe(false);
  });

  it('confirmOrDismissDialog tells Escape and a click outside apart from Cancel', async () => {
    render(<DialogHost />);
    let answer = open(() => confirmOrDismissDialog({ message: 'Connect?', cancelLabel: 'Not now' }));
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    await expect(answer).resolves.toBeNull();

    answer = open(() => confirmOrDismissDialog({ message: 'Connect?', cancelLabel: 'Not now' }));
    fireEvent.mouseDown(screen.getByRole('presentation'));
    await expect(answer).resolves.toBeNull();

    answer = open(() => confirmOrDismissDialog({ message: 'Connect?', cancelLabel: 'Not now' }));
    fireEvent.click(screen.getByRole('button', { name: 'Not now' }));
    await expect(answer).resolves.toBe(false);

    answer = open(() => confirmOrDismissDialog({ message: 'Connect?', confirmLabel: 'Connect' }));
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    await expect(answer).resolves.toBe(true);
  });

  it('a dismissable confirm replaced by another dialog resolves null, not false', async () => {
    const first = open(() => confirmOrDismissDialog({ message: 'Connect?' }));
    const second = open(() => confirmDialog({ message: 'Other?' }));
    await expect(first).resolves.toBeNull();
    act(() => useStore.getState().closeDialog(false));
    await expect(second).resolves.toBe(false);
  });
});
