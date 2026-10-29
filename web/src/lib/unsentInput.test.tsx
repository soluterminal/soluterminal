import { afterEach, describe, expect, it } from 'vitest';
import { renderHook } from '@testing-library/react';
import { hasRegisteredUnsentInput, resetUnsentInputForTests, useUnsentInput } from './unsentInput';

describe('useUnsentInput', () => {
  afterEach(() => resetUnsentInputForTests());

  it('holds while a mounted box has text and lets go when it is sent, cleared or closed', () => {
    const box = renderHook(({ text }) => useUnsentInput(text), { initialProps: { text: '' } });
    expect(hasRegisteredUnsentInput()).toBe(false);

    box.rerender({ text: 'half a sentence' });
    expect(hasRegisteredUnsentInput()).toBe(true);

    box.rerender({ text: '   ' });
    expect(hasRegisteredUnsentInput()).toBe(false);

    box.rerender({ text: 'again' });
    const other = renderHook(() => useUnsentInput('second box'));
    box.unmount();
    expect(hasRegisteredUnsentInput()).toBe(true);
    other.unmount();
    expect(hasRegisteredUnsentInput()).toBe(false);
  });
});
