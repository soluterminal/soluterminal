import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { UpdateStatus } from './types';

// Benjamin, 2026-10-28: "I can click once on doing the update, but after the
// app refreshes I had to click again." "Update now (app + installer)" launches
// the installer first (applyAllUpdates), and the installer relaunches the app.
// The installer usually carries an older daemon than the manifest's latest
// (daemon-only releases carry it forward), so the relaunched app still had the
// daemon update waiting behind a second click. The click now carries over.

let appVersion = '0.0.900';

function desktop() {
  Object.defineProperty(window, '__SOLU_TAURI__', { value: true, configurable: true });
  Object.defineProperty(window, '__TAURI_INTERNALS__', {
    value: { invoke: vi.fn(async (command: string) => (command === 'browser_pane_ping' ? appVersion : undefined)) },
    configurable: true,
  });
}

// Each import after resetModules is a fresh page: new module state, same
// localStorage — what the app sees after the installer relaunches it.
async function loadPage() {
  vi.resetModules();
  const store = await import('./store');
  const applyUpdate = vi.fn(async () => {});
  const toast = vi.fn();
  store.useStore.setState({ applyUpdate, toast, deferredUpdate: null, updateApplyRequestInFlight: false });
  return { useStore: store.useStore, applyUpdate, toast };
}

const before: UpdateStatus = {
  current: '0.0.900', latest: '0.0.932', available: true,
  shellCurrent: '0.0.900', shellLatest: '0.0.916', shellAvailable: true,
};
// The relaunched app runs the installer's daemon (0.0.916); 0.0.932 waits.
const relaunched: UpdateStatus = {
  current: '0.0.916', latest: '0.0.932', available: true,
  shellCurrent: '0.0.916', shellLatest: '0.0.916', shellAvailable: false,
};

async function clickAppPlusInstaller() {
  const page = await loadPage();
  const applyShell = vi.fn(async () => {});
  page.useStore.setState({ updateStatus: before });
  await page.useStore.getState().applyAllUpdates({ applyShell, shellActionable: true });
  expect(applyShell).toHaveBeenCalledTimes(1);
  expect(page.applyUpdate).not.toHaveBeenCalled();
  return page;
}

describe('one click finishes a combined update after the installer relaunch', () => {
  beforeEach(() => {
    localStorage.clear();
    appVersion = '0.0.900';
    desktop();
  });

  afterEach(() => {
    delete (window as unknown as { __SOLU_BACKEND__?: unknown }).__SOLU_BACKEND__;
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it('applies the waiting daemon update on the relaunched app, once', async () => {
    const first = await clickAppPlusInstaller();
    // The page that launched the installer never continues by itself.
    await first.useStore.getState().continueUpdateAfterInstaller();
    expect(first.applyUpdate).not.toHaveBeenCalled();

    appVersion = '0.0.916';
    const second = await loadPage();
    second.useStore.setState({ updateStatus: relaunched });
    await second.useStore.getState().continueUpdateAfterInstaller();
    expect(second.applyUpdate).toHaveBeenCalledTimes(1);
    expect(second.applyUpdate).toHaveBeenCalledWith();
    expect(second.toast).toHaveBeenCalledWith(
      'Desktop app updated — finishing the update to v0.0.932…',
      expect.objectContaining({ kind: 'info' }),
    );

    await second.useStore.getState().continueUpdateAfterInstaller();
    const third = await loadPage();
    third.useStore.setState({ updateStatus: relaunched });
    await third.useStore.getState().continueUpdateAfterInstaller();
    expect(second.applyUpdate).toHaveBeenCalledTimes(1);
    expect(third.applyUpdate).not.toHaveBeenCalled();
  });

  it('runs from the normal status refresh', async () => {
    await clickAppPlusInstaller();
    appVersion = '0.0.916';
    const second = await loadPage();
    const { api } = await import('./api');
    vi.spyOn(api, 'getUpdateStatus').mockResolvedValue(relaunched);
    await second.useStore.getState().refreshUpdateStatus();
    await vi.waitFor(() => expect(second.applyUpdate).toHaveBeenCalledTimes(1));
  });

  it('keeps the intent while the relaunched daemon has not fetched its manifest yet', async () => {
    await clickAppPlusInstaller();
    appVersion = '0.0.916';
    const second = await loadPage();
    // NewUpdater's first seconds: only the running version is known.
    second.useStore.setState({ updateStatus: { current: '0.0.916', available: false } });
    await second.useStore.getState().continueUpdateAfterInstaller();
    expect(second.applyUpdate).not.toHaveBeenCalled();
    expect(localStorage.getItem('solu.update.continueAfterInstaller')).not.toBeNull();

    second.useStore.setState({ updateStatus: relaunched });
    await second.useStore.getState().continueUpdateAfterInstaller();
    expect(second.applyUpdate).toHaveBeenCalledTimes(1);
  });

  it('waits while the installer has not replaced the app (cancelled or still running)', async () => {
    await clickAppPlusInstaller();
    const reloaded = await loadPage(); // same app version: a plain reload
    reloaded.useStore.setState({ updateStatus: before });
    await reloaded.useStore.getState().continueUpdateAfterInstaller();
    expect(reloaded.applyUpdate).not.toHaveBeenCalled();
    expect(localStorage.getItem('solu.update.continueAfterInstaller')).not.toBeNull();
  });

  it('drops the intent once the daemon already reached the target', async () => {
    await clickAppPlusInstaller();
    appVersion = '0.0.916';
    const second = await loadPage();
    second.useStore.setState({ updateStatus: { ...relaunched, current: '0.0.932', available: false } });
    await second.useStore.getState().continueUpdateAfterInstaller();
    expect(second.applyUpdate).not.toHaveBeenCalled();
    expect(localStorage.getItem('solu.update.continueAfterInstaller')).toBeNull();
  });

  it('leaves a swap already in motion alone', async () => {
    await clickAppPlusInstaller();
    appVersion = '0.0.916';
    const second = await loadPage();
    second.useStore.setState({ updateStatus: { ...relaunched, applying: true } });
    await second.useStore.getState().continueUpdateAfterInstaller();
    expect(second.applyUpdate).not.toHaveBeenCalled();
  });

  it('lapses after the TTL', async () => {
    await clickAppPlusInstaller();
    const stored = JSON.parse(localStorage.getItem('solu.update.continueAfterInstaller')!);
    const { UPDATE_CONTINUE_TTL_MS } = await import('./store');
    localStorage.setItem('solu.update.continueAfterInstaller', JSON.stringify({ ...stored, at: Date.now() - UPDATE_CONTINUE_TTL_MS - 1 }));
    appVersion = '0.0.916';
    const second = await loadPage();
    second.useStore.setState({ updateStatus: relaunched });
    await second.useStore.getState().continueUpdateAfterInstaller();
    expect(second.applyUpdate).not.toHaveBeenCalled();
    expect(localStorage.getItem('solu.update.continueAfterInstaller')).toBeNull();
  });

  it('records nothing in cloud launch mode, where the daemon is not this computer', async () => {
    Object.defineProperty(window, '__SOLU_BACKEND__', { value: { launchMode: 'cloud' }, configurable: true });
    await clickAppPlusInstaller();
    expect(localStorage.getItem('solu.update.continueAfterInstaller')).toBeNull();
  });
});
