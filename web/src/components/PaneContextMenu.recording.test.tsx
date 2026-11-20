import { createRef } from 'react';
import { act, render, renderHook, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Health, PaneInfo } from '../types';
import { PaneContextMenu } from './PaneContextMenu';

// ST-315: a desktop setup can update this UI while the daemon stays a
// version behind. Recording is offered only when the daemon in the pane's own
// scope says it serves it; an older daemon answered "404 page not found".

const apiMock = vi.hoisted(() => ({ healthScoped: vi.fn(), currentKey: 'b|wsl' }));
vi.mock('../api', async (importOriginal) => {
  const real = await importOriginal<typeof import('../api')>();
  return {
    ...real,
    api: { ...real.api, healthScoped: apiMock.healthScoped },
    currentRoutingScope: () => ({ prefix: '', backendPrefix: '/b/wsl', key: apiMock.currentKey }),
  };
});
vi.mock('./SmartApprovalsHint', () => ({ SmartApprovalsHint: () => null }));

import { usePaneContextMenu } from './usePaneContextMenu';

function renderMenu(pane: Partial<PaneInfo>, recordingServed?: boolean) {
  const noop = () => {};
  return render(
    <PaneContextMenu
      pane={{ id: 'pane-1', title: 'Agent', status: 'running', cmd: ['claude'], ...pane } as PaneInfo}
      x={0} y={0} menuRef={createRef<HTMLDivElement>()} isClaudePane
      onSplitRight={noop} onSplitRightShellHere={noop} onSplitRightClaudeHere={noop} onSplitDown={noop}
      onClonePane={noop} onNewSession={noop} onCreateSchedule={noop} onSetAutoMode={noop} onEditFlagMemo={noop}
      onUnflag={noop} onFlagForAttention={noop} onRename={noop} onClosePane={noop} onRecording={noop}
      recordingServed={recordingServed}
    />,
  );
}

afterEach(() => { apiMock.healthScoped.mockReset(); apiMock.currentKey = 'b|wsl'; });

describe('pane recording follows what the daemon serves (ST-315)', () => {
  it('offers Start recording when the pane scope daemon serves it', () => {
    renderMenu({}, true);
    expect(screen.getByRole('menuitem', { name: /Start recording output/ })).toBeInTheDocument();
  });

  it('hides recording when it is not served, or not yet known', () => {
    const { unmount } = renderMenu({}, false);
    expect(screen.queryByRole('menuitem', { name: /Start recording output/ })).toBeNull();
    expect(screen.queryByText(/^Recording/)).toBeNull();
    unmount();
    renderMenu({});
    expect(screen.queryByRole('menuitem', { name: /Start recording output/ })).toBeNull();
  });

  it('still offers Stop when a recording is on', () => {
    renderMenu({ recording: { on: true } as PaneInfo['recording'] }, false);
    expect(screen.getByRole('menuitem', { name: 'Stop recording' })).toBeInTheDocument();
  });
});

describe('usePaneContextMenu asks the pane scope daemon (ST-315)', () => {
  const pane = { id: 'pane-1', title: 'Agent', status: 'running', cmd: ['claude'], pid: 1 } as PaneInfo;
  const args = { pane, setFocus: vi.fn(), setTitleDraft: vi.fn(), setEditingTitle: vi.fn(), setFlagMemoDraft: vi.fn(), setEditingFlagMemo: vi.fn(), killPane: vi.fn() };
  const open = (r: { current: ReturnType<typeof usePaneContextMenu> }) =>
    act(() => r.current.openContextMenu({ preventDefault() {}, stopPropagation() {}, clientX: 1, clientY: 1 } as never));

  it('probes health in the scope the recording request will use', async () => {
    apiMock.healthScoped.mockResolvedValue({ status: 'ok', version: '0.0.1248', features: ['pane-recording.v1'] } as Health);
    const scope = { prefix: '', backendPrefix: '/b/wsl', key: 'b|wsl' };
    const { result } = renderHook(() => usePaneContextMenu({ ...args, scope } as never));
    await open(result);
    await act(async () => {});
    expect(apiMock.healthScoped).toHaveBeenCalledWith(scope);
    expect(result.current.recordingServed).toBe(true);
  });

  it('says no for a daemon without the feature, on an error, and for another scope', async () => {
    apiMock.healthScoped.mockResolvedValue({ status: 'ok', version: '0.0.1246', features: ['ui-prefs.v1'] } as Health);
    const first = renderHook(() => usePaneContextMenu({ ...args } as never));
    await open(first.result);
    await act(async () => {});
    expect(first.result.current.recordingServed).toBe(false);
    first.unmount();

    apiMock.healthScoped.mockRejectedValue(new Error('404'));
    const second = renderHook(() => usePaneContextMenu({ ...args } as never));
    await open(second.result);
    await act(async () => {});
    expect(second.result.current.recordingServed).toBe(false);
    second.unmount();

    // Answered for b|wsl, then the view moves to b|win: the old answer no longer counts.
    apiMock.healthScoped.mockResolvedValue({ status: 'ok', version: '0.0.1248', features: ['pane-recording.v1'] } as Health);
    const third = renderHook(() => usePaneContextMenu({ ...args } as never));
    await open(third.result);
    await act(async () => {});
    expect(third.result.current.recordingServed).toBe(true);
    apiMock.currentKey = 'b|win';
    third.rerender();
    expect(third.result.current.recordingServed).toBe(false);
  });
});
