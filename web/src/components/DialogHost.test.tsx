import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { useStore } from '../store';
import { confirmDialog, confirmOrDismissDialog } from '../lib/dialog';
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
