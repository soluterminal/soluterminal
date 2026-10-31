import { afterEach, describe, expect, it, vi } from 'vitest';
import { canUpdateThisComputerHere, resetUpdateThisComputerForTest, updateThisComputer } from './updateThisComputer';

function shell(backend: Record<string, unknown> | undefined, invoke: ReturnType<typeof vi.fn> = vi.fn(async (): Promise<unknown> => ({ state: 'launched' }))) {
  Object.defineProperty(window, '__SOLU_TAURI__', { value: true, configurable: true });
  Object.defineProperty(window, '__TAURI_INTERNALS__', { value: { invoke }, configurable: true });
  Object.defineProperty(window, '__SOLU_BACKEND__', { value: backend, configurable: true });
  return invoke;
}

afterEach(() => {
  for (const key of ['__SOLU_TAURI__', '__TAURI_INTERNALS__', '__SOLU_BACKEND__']) {
    delete (window as unknown as Record<string, unknown>)[key];
  }
  resetUpdateThisComputerForTest();
});

describe('updateThisComputer', () => {
  it('is offered only by a Cloud-mode shell that says it has the command', () => {
    shell({ launchMode: 'cloud', updateThisComputer: true });
    expect(canUpdateThisComputerHere()).toBe(true);
    shell({ launchMode: 'cloud' });
    expect(canUpdateThisComputerHere()).toBe(false);
    shell({ launchMode: 'local', updateThisComputer: true });
    expect(canUpdateThisComputerHere()).toBe(false);
    shell({ launchMode: 'cloud', updateThisComputer: 'yes' });
    expect(canUpdateThisComputerHere()).toBe(false);
  });

  it('is not offered in a browser tab', () => {
    Object.defineProperty(window, '__SOLU_BACKEND__', { value: { launchMode: 'cloud', updateThisComputer: true }, configurable: true });
    expect(canUpdateThisComputerHere()).toBe(false);
  });

  it('never invokes where it is not offered', async () => {
    const invoke = shell({ launchMode: 'local', updateThisComputer: true });
    expect((await updateThisComputer(vi.fn())).state).toBe('unavailable');
    expect(invoke).not.toHaveBeenCalled();
  });

  it('runs once at a time and treats an unknown answer as a failure', async () => {
    let finish: (v: unknown) => void = () => {};
    const invoke = shell({ launchMode: 'cloud', updateThisComputer: true }, vi.fn(() => new Promise<unknown>((r) => { finish = r; })));
    const toast = vi.fn();
    const first = updateThisComputer(toast);
    expect((await updateThisComputer(toast)).state).toBe('busy');
    finish({ state: 'something-new' });
    expect((await first).state).toBe('failed');
    expect(invoke).toHaveBeenCalledOnce();
    expect(toast).not.toHaveBeenCalled();
  });

  it('a cancelled confirmation is quiet', async () => {
    shell({ launchMode: 'cloud', updateThisComputer: true }, vi.fn(async (): Promise<unknown> => ({ state: 'cancelled' })));
    const toast = vi.fn();
    expect((await updateThisComputer(toast)).state).toBe('cancelled');
    expect(toast).not.toHaveBeenCalled();
  });
});
