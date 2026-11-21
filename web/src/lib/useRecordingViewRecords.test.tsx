import { act, cleanup, render, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { useStore } from '../store';
import { useCloud } from '../cloud';
import type { Health, PaneInfo } from '../types';
import { currentViewRecord, fitViewRecord, useRecordingViewRecords } from './useRecordingViewRecords';

// ST-317: the view record holds ids, rectangles, enums and integers only; it
// is sent only while something records and the daemon keeps a timeline.

const base = { cwd: '/home/u/SECRET-PATH', status: 'running', cols: 80, rows: 24, pid: 1, lastActivityMs: 0, state: 'idle', createdAt: '2026-11-20T00:00:00Z' };
const recorded = { ...base, id: 'pane_a', title: 'SECRET-TITLE', cmd: ['claude'], recording: { on: true } } as PaneInfo;
const plain = { ...base, id: 'pane_b', title: 'Other', cmd: ['bash'] } as PaneInfo;
const original = useStore.getState();
const originalCloud = useCloud.getState();

function mountPanes(ids: string[]) {
  return render(
    <div>
      {ids.map((id, i) => (
        <div key={id} data-pane-id={id} ref={(el) => {
          if (el) el.getBoundingClientRect = () => ({ left: i * 400, top: 30.6, width: 400, height: 500.4, right: 0, bottom: 0, x: 0, y: 0, toJSON: () => ({}) });
        }}>SECRET-TEXT typed here</div>
      ))}
    </div>,
  );
}

beforeEach(() => {
  useStore.setState({ panes: { pane_a: recorded, pane_b: plain }, focusedPaneId: 'pane_a', zoomedPaneId: null } as never);
});

// Lets the short retries for a just-started recording run out.
async function settle() {
  await act(async () => {});
  for (let i = 0; i < 12; i++) await act(async () => { vi.advanceTimersByTime(250); });
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
  useStore.setState({ panes: original.panes, focusedPaneId: original.focusedPaneId } as never);
});

describe('fitViewRecord', () => {
  it('drops panes to fit 4 KB, keeping the focused one', () => {
    const panes = Array.from({ length: 64 }, (_, i) => ({ id: `pane_with_a_long_identifier_${i}`, x: 1000, y: 1000, w: 1000, h: 1000, scroll: 12345 }));
    const v = fitViewRecord({ state: 'shown', window: { w: 1, h: 1 }, focus: 'pane_with_a_long_identifier_63', panes });
    expect(JSON.stringify(v).length).toBeLessThanOrEqual(4000);
    expect(v.panes?.some((p) => p.id === 'pane_with_a_long_identifier_63')).toBe(true);
    expect(v.panes!.length).toBeLessThan(64);
  });
});

describe('currentViewRecord', () => {
  it('holds ids, integer rectangles and enums, and no text', () => {
    mountPanes(['pane_a', 'pane_b']);
    const v = currentViewRecord({ sidebarCollapsed: true, panelOpen: false, wasHidden: false });
    expect(v?.panes).toEqual([
      { id: 'pane_a', x: 0, y: 31, w: 400, h: 500 },
      { id: 'pane_b', x: 400, y: 31, w: 400, h: 500 },
    ]);
    expect(v?.focus).toBe('pane_a');
    expect(v?.state).toBe('shown');
    expect(v?.sidebar).toEqual({ collapsed: true });
    expect(v?.panel).toBe('none');
    expect(v?.display?.font).toBe('jetbrains-mono');
    const json = JSON.stringify(v);
    for (const leak of ['SECRET', 'typed', '/home', 'Other', 'claude']) expect(json).not.toContain(leak);
  });

  it('says reopened once after the window was hidden, and null with no pane shown', () => {
    mountPanes(['pane_a']);
    expect(currentViewRecord({ sidebarCollapsed: false, panelOpen: true, wasHidden: true })?.state).toBe('reopened');
    cleanup();
    expect(currentViewRecord({ sidebarCollapsed: false, panelOpen: false, wasHidden: false })).toBeNull();
  });
});

describe('useRecordingViewRecords', () => {
  it('sends nothing to a daemon without the timeline', async () => {
    vi.spyOn(api, 'healthScoped').mockResolvedValue({ status: 'ok', features: ['pane-recording.v2'] } as Health);
    const send = vi.spyOn(api, 'sendRecordingView').mockResolvedValue(200);
    mountPanes(['pane_a']);
    renderHook(() => useRecordingViewRecords({ sidebarCollapsed: false, panelOpen: false }));
    await act(async () => {});
    expect(send).not.toHaveBeenCalled();
  });

  it('sends the view, and the bare away record when the daemon says nothing shown records', async () => {
    vi.spyOn(api, 'healthScoped').mockResolvedValue({ status: 'ok', features: ['pane-recording-timeline.v1'] } as Health);
    const send = vi.spyOn(api, 'sendRecordingView').mockResolvedValue(200);
    mountPanes(['pane_a']);
    const hook = renderHook(() => useRecordingViewRecords({ sidebarCollapsed: false, panelOpen: false }));
    await act(async () => {});
    expect(send).toHaveBeenCalledTimes(1);
    expect(send.mock.calls[0][0].panes?.[0].id).toBe('pane_a');
    hook.unmount();
    cleanup();
    send.mockReset();
    // A view of an unrecorded pane is still sent (it may share a recording
    // workspace); once the short retries for a just-started recording are
    // spent, the daemon's 409 turns it into the away record.
    vi.useFakeTimers({ shouldAdvanceTime: true });
    send.mockImplementation(async (b) => (b.away ? 200 : 409));
    mountPanes(['pane_b']);
    renderHook(() => useRecordingViewRecords({ sidebarCollapsed: false, panelOpen: false }));
    await settle();
    expect(send.mock.calls[0][0].panes?.[0].id).toBe('pane_b');
    expect(send.mock.calls.at(-1)?.[0]).toEqual({ state: 'shown', away: true });
  });

  it('records a hide to the tray at once', async () => {
    vi.spyOn(api, 'healthScoped').mockResolvedValue({ status: 'ok', features: ['pane-recording-timeline.v1'] } as Health);
    const send = vi.spyOn(api, 'sendRecordingView').mockResolvedValue(200);
    mountPanes(['pane_a']);
    renderHook(() => useRecordingViewRecords({ sidebarCollapsed: false, panelOpen: false }));
    await act(async () => {});
    await act(async () => {
      window.dispatchEvent(new CustomEvent('solu:desktop-will-background', { detail: { reason: 'close-to-tray' } }));
      window.dispatchEvent(new CustomEvent('solu:desktop-restored', { detail: { reason: 'tray' } }));
    });
    expect(send.mock.calls.map((c) => c[0].state)).toContain('tray');
  });

  it('asks again after a failed health check', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.spyOn(api, 'healthScoped')
      .mockRejectedValueOnce(new Error('offline'))
      .mockResolvedValue({ status: 'ok', features: ['pane-recording-timeline.v1'] } as Health);
    const send = vi.spyOn(api, 'sendRecordingView').mockResolvedValue(200);
    mountPanes(['pane_a']);
    renderHook(() => useRecordingViewRecords({ sidebarCollapsed: false, panelOpen: false }));
    await act(async () => {});
    expect(send).not.toHaveBeenCalled();
    await act(async () => { vi.advanceTimersByTime(6000); });
    expect(send).toHaveBeenCalled();
  });

  it('includes a browser tile by id, and says tray after the desktop hides to the tray', async () => {
    render(<div>
      <div data-pane-id="pane_a" ref={(el) => { if (el) el.getBoundingClientRect = () => ({ left: 0, top: 0, width: 300, height: 300, right: 0, bottom: 0, x: 0, y: 0, toJSON: () => ({}) }); }} />
      <div data-browser-id="browser_1" ref={(el) => { if (el) el.getBoundingClientRect = () => ({ left: 300, top: 0, width: 300, height: 300, right: 0, bottom: 0, x: 0, y: 0, toJSON: () => ({}) }); }} />
    </div>);
    const v = currentViewRecord({ sidebarCollapsed: false, panelOpen: false, wasHidden: false, tray: true });
    expect(v?.panes?.map((p) => p.id)).toEqual(['pane_a', 'browser_1']);
    expect(v?.state).toBe('tray');
  });

  it('tries a refused send again although nothing on screen changed', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.spyOn(api, 'healthScoped').mockResolvedValue({ status: 'ok', features: ['pane-recording-timeline.v1'] } as Health);
    const send = vi.spyOn(api, 'sendRecordingView')
      .mockRejectedValueOnce(new Error('409'))
      .mockResolvedValue(200);
    mountPanes(['pane_a']);
    renderHook(() => useRecordingViewRecords({ sidebarCollapsed: false, panelOpen: false }));
    await act(async () => {});
    expect(send).toHaveBeenCalledTimes(1);
    await act(async () => { vi.advanceTimersByTime(1500); });
    expect(send).toHaveBeenCalledTimes(2);
    await act(async () => { vi.advanceTimersByTime(1500); });
    expect(send).toHaveBeenCalledTimes(2);
  });

  it('does not re-send a refused view until what is shown changes', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.spyOn(api, 'healthScoped').mockResolvedValue({ status: 'ok', features: ['pane-recording-timeline.v1'] } as Health);
    const send = vi.spyOn(api, 'sendRecordingView').mockImplementation(async (b) => (b.away ? 200 : 409));
    mountPanes(['pane_b']);
    renderHook(() => useRecordingViewRecords({ sidebarCollapsed: false, panelOpen: false }));
    await settle();
    const settled = send.mock.calls.length;
    expect(send.mock.calls.at(-1)?.[0]).toEqual({ state: 'shown', away: true });
    await act(async () => { vi.advanceTimersByTime(3000); });
    expect(send).toHaveBeenCalledTimes(settled);
  });

  it('sends the unchanged view again when a recording starts in it after a 409', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.spyOn(api, 'healthScoped').mockResolvedValue({ status: 'ok', features: ['pane-recording-timeline.v1'] } as Health);
    let paneBRecords = false;
    const send = vi.spyOn(api, 'sendRecordingView').mockImplementation(async (b) => (b.away || paneBRecords ? 200 : 409));
    mountPanes(['pane_b']);
    renderHook(() => useRecordingViewRecords({ sidebarCollapsed: false, panelOpen: false }));
    // pane_b isn't recording: its view is refused and the window says away.
    await settle();
    const settled = send.mock.calls.length;
    expect(send.mock.calls.at(-1)?.[0]).toEqual({ state: 'shown', away: true });
    // A recording starts in the shown workspace; nothing on screen changed.
    paneBRecords = true;
    act(() => { useStore.setState({ panes: { pane_a: recorded, pane_b: { ...plain, recording: { on: true } } } } as never); });
    await act(async () => { vi.advanceTimersByTime(300); });
    expect(send).toHaveBeenCalledTimes(settled + 1);
    expect(send.mock.calls[settled][0].panes?.[0].id).toBe('pane_b');
    // Then no more until something changes again.
    await act(async () => { vi.advanceTimersByTime(2000); });
    expect(send).toHaveBeenCalledTimes(settled + 1);
  });

  it('keeps trying the view for a moment when the daemon has not taken the new recording in yet', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.spyOn(api, 'healthScoped').mockResolvedValue({ status: 'ok', features: ['pane-recording-timeline.v1'] } as Health);
    let accepts = false;
    const send = vi.spyOn(api, 'sendRecordingView').mockImplementation(async (b) => (b.away || accepts ? 200 : 409));
    mountPanes(['pane_b']);
    renderHook(() => useRecordingViewRecords({ sidebarCollapsed: false, panelOpen: false }));
    await settle();
    const settled = send.mock.calls.length;
    // The recording starts; the daemon still refuses the first re-send.
    act(() => { useStore.setState({ panes: { pane_a: recorded, pane_b: { ...plain, recording: { on: true } } } } as never); });
    await act(async () => { vi.advanceTimersByTime(300); });
    expect(send).toHaveBeenCalledTimes(settled + 1);
    expect(send.mock.calls[settled][0].away).toBeUndefined();
    // Not settled as away: it tries again and is accepted once the daemon catches up.
    accepts = true;
    await act(async () => { vi.advanceTimersByTime(700); });
    const views = send.mock.calls.slice(settled + 1).map((c) => c[0]);
    expect(views.length).toBeGreaterThan(0);
    expect(views.every((v) => !v.away)).toBe(true);
    const total = send.mock.calls.length;
    // Accepted: nothing more until something changes.
    await act(async () => { vi.advanceTimersByTime(2000); });
    expect(send).toHaveBeenCalledTimes(total);
  });

  it('tries again when the first recording starts before the daemon takes it in', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.spyOn(api, 'healthScoped').mockResolvedValue({ status: 'ok', features: ['pane-recording-timeline.v1'] } as Health);
    let accepts = false;
    const send = vi.spyOn(api, 'sendRecordingView').mockImplementation(async (b) => (b.away || accepts ? 200 : 409));
    mountPanes(['pane_a']);
    renderHook(() => useRecordingViewRecords({ sidebarCollapsed: false, panelOpen: false }));
    await act(async () => {});
    expect(send.mock.calls[0][0].away).toBeUndefined();
    accepts = true;
    await act(async () => { vi.advanceTimersByTime(700); });
    expect(send.mock.calls.every((c) => !c[0].away)).toBe(true);
  });

  it('after an accepted view, a refused one is answered as away at once', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.spyOn(api, 'healthScoped').mockResolvedValue({ status: 'ok', features: ['pane-recording-timeline.v1'] } as Health);
    const send = vi.spyOn(api, 'sendRecordingView').mockImplementation(async (b) => (b.away || b.panes?.[0].id === 'pane_a' ? 200 : 409));
    const first = mountPanes(['pane_a']);
    renderHook(() => useRecordingViewRecords({ sidebarCollapsed: false, panelOpen: false }));
    await act(async () => {});
    expect(send).toHaveBeenCalledTimes(1);
    // The person switches to a workspace with no recording pane.
    first.unmount();
    mountPanes(['pane_b']);
    await act(async () => { vi.advanceTimersByTime(300); });
    const after = send.mock.calls.slice(1).map((c) => c[0]);
    expect(after.length).toBe(2);
    expect(after[1]).toEqual({ state: 'shown', away: true });
  });

  it('sends nothing through the cloud', async () => {
    useCloud.setState({ mode: 'cloud', isLocal: false } as never);
    const health = vi.spyOn(api, 'healthScoped');
    mountPanes(['pane_a']);
    renderHook(() => useRecordingViewRecords({ sidebarCollapsed: false, panelOpen: false }));
    await act(async () => {});
    expect(health).not.toHaveBeenCalled();
    useCloud.setState({ mode: originalCloud.mode, isLocal: originalCloud.isLocal } as never);
  });

  it('is inert while nothing records', async () => {
    useStore.setState({ panes: { pane_b: plain } } as never);
    const health = vi.spyOn(api, 'healthScoped');
    renderHook(() => useRecordingViewRecords({ sidebarCollapsed: false, panelOpen: false }));
    await act(async () => {});
    expect(health).not.toHaveBeenCalled();
  });
});
