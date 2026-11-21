import { createRef } from 'react';
import { act, render, renderHook, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Health, PaneInfo, RecordingCheck } from '../types';
import { PaneContextMenu } from './PaneContextMenu';
import type { RecordingServed } from '../lib/paneRecording';

// ST-315: a desktop setup can update this UI while the daemon stays a
// version behind, so what the menu offers follows the daemon in the pane's own
// scope. ST-317 (C3): the Recording item always shows; when it can't be
// switched on it is greyed with the reason and what to do, never hidden.

const apiMock = vi.hoisted(() => ({ healthScoped: vi.fn(), getPaneRecording: vi.fn(), currentKey: 'b|wsl' }));
vi.mock('../api', async (importOriginal) => {
  const real = await importOriginal<typeof import('../api')>();
  return {
    ...real,
    api: { ...real.api, healthScoped: apiMock.healthScoped, getPaneRecording: apiMock.getPaneRecording },
    currentRoutingScope: () => ({ prefix: '', backendPrefix: '/b/wsl', key: apiMock.currentKey }),
  };
});
vi.mock('./SmartApprovalsHint', () => ({ SmartApprovalsHint: () => null }));

import { usePaneContextMenu } from './usePaneContextMenu';

function renderMenu(pane: Partial<PaneInfo>, served?: RecordingServed, check?: RecordingCheck | null, workspace?: { anyOn: boolean; count: number }) {
  const noop = () => {};
  const onRecording = vi.fn();
  render(
    <PaneContextMenu
      pane={{ id: 'pane-1', title: 'Agent', status: 'running', cmd: ['claude'], ...pane } as PaneInfo}
      x={0} y={0} menuRef={createRef<HTMLDivElement>()} isClaudePane
      onSplitRight={noop} onSplitRightShellHere={noop} onSplitRightClaudeHere={noop} onSplitDown={noop}
      onClonePane={noop} onNewSession={noop} onCreateSchedule={noop} onSetAutoMode={noop} onEditFlagMemo={noop}
      onUnflag={noop} onFlagForAttention={noop} onRename={noop} onClosePane={noop} onRecording={onRecording}
      recordingServed={served} recordingCheck={check} recordingWorkspace={workspace}
    />,
  );
  return onRecording;
}

const start = () => screen.getByRole('menuitem', { name: /Start recording output/ });

afterEach(() => {
  apiMock.healthScoped.mockReset();
  apiMock.getPaneRecording.mockReset();
  apiMock.currentKey = 'b|wsl';
});

describe('the Recording item is never hidden (ST-317 C3)', () => {
  it('offers Start when the daemon serves it and its rules allow the pane', () => {
    renderMenu({}, 'v2', { defaults: [] });
    expect(start()).toBeEnabled();
  });

  it('offers Start for a pane whose defaults can be overridden in the dialog', () => {
    renderMenu({ cmd: ['bash'] }, 'v2', { defaults: ['shell', 'handle'] });
    expect(start()).toBeEnabled();
  });

  const greyed: [string, RecordingServed, RecordingCheck | null | undefined, RegExp][] = [
    ['an older daemon', 'none', undefined, /Update SoluTerminal to record panes/],
    ['a v1 daemon and a shell pane', 'v1', undefined, /Update SoluTerminal to record this pane/],
    ['an unreachable daemon', 'unreachable', undefined, /Couldn't reach SoluTerminal/],
    ['while asking', 'checking', undefined, /Checking whether this pane can be recorded/],
    ['a Windows pane', 'v2', { unavailable: 'windows' }, /isn't available for Windows panes yet/],
    ['a pane that is not running', 'v2', { unavailable: 'not_running' }, /Restart it, then record/],
    ['no data folder', 'v2', { unavailable: 'no_data_dir' }, /no folder to save recordings in/],
    ['a hard folder', 'v2', { hard: 'hard_folder', defaults: ['shell'] }, /never recorded \(keys and tokens live there\)/],
    ['an unreadable folder', 'v2', { hard: 'cwd_unknown' }, /can't read this pane's folder/],
  ];
  for (const [name, served, check, reason] of greyed) {
    it(`is greyed with the reason for ${name}`, () => {
      const pane = name.includes('shell') ? { cmd: ['bash'], agent: undefined, hasClaude: false } : {};
      const onRecording = renderMenu(pane, served, check);
      expect(screen.getByText('Recording', { selector: '.pane-ctx-menu-label' })).toBeInTheDocument();
      const item = start();
      expect(item).toBeDisabled();
      expect(screen.getByText(reason)).toBeInTheDocument();
      item.click();
      expect(onRecording).not.toHaveBeenCalled();
    });
  }

  it('offers Start on a v1 daemon for an agent pane, as before', () => {
    renderMenu({ agent: 'claude' }, 'v1');
    expect(start()).toBeEnabled();
  });

  it('still offers Stop when a recording is on, whatever is known', () => {
    renderMenu({ recording: { on: true } }, 'checking');
    expect(screen.getByRole('menuitem', { name: 'Stop recording' })).toBeInTheDocument();
  });

  it('offers the workspace switch and its stop when the daemon serves them', () => {
    const onRecording = renderMenu({}, 'v2', { defaults: [] }, { anyOn: true, count: 3 });
    screen.getByRole('menuitem', { name: 'Record every pane in this workspace…' }).click();
    screen.getByRole('menuitem', { name: 'Stop recording this workspace' }).click();
    expect(onRecording.mock.calls).toEqual([['workspace-start'], ['workspace-stop']]);
  });
});

describe('usePaneContextMenu asks the pane scope daemon (ST-315, ST-317)', () => {
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
    expect(result.current.recordingServed).toBe('v1');
    expect(apiMock.getPaneRecording).not.toHaveBeenCalled();
  });

  it('reads the rule check from a v2 daemon', async () => {
    apiMock.healthScoped.mockResolvedValue({ status: 'ok', version: '0.0.1248', features: ['pane-recording.v1', 'pane-recording.v2'] } as Health);
    apiMock.getPaneRecording.mockResolvedValue({ on: false, check: { defaults: ['shell'] } });
    const { result } = renderHook(() => usePaneContextMenu({ ...args } as never));
    await open(result);
    await act(async () => {});
    expect(result.current.recordingServed).toBe('v2');
    expect(result.current.recordingCheck).toEqual({ defaults: ['shell'] });
  });

  it('says none, unreachable, or checking for another scope', async () => {
    apiMock.healthScoped.mockResolvedValue({ status: 'ok', version: '0.0.1246', features: ['ui-prefs.v1'] } as Health);
    const first = renderHook(() => usePaneContextMenu({ ...args } as never));
    await open(first.result);
    await act(async () => {});
    expect(first.result.current.recordingServed).toBe('none');
    first.unmount();

    apiMock.healthScoped.mockRejectedValue(new Error('404'));
    const second = renderHook(() => usePaneContextMenu({ ...args } as never));
    await open(second.result);
    await act(async () => {});
    expect(second.result.current.recordingServed).toBe('unreachable');
    second.unmount();

    // Answered for b|wsl, then the view moves to b|win: the old answer no longer counts.
    apiMock.healthScoped.mockResolvedValue({ status: 'ok', version: '0.0.1248', features: ['pane-recording.v1'] } as Health);
    const third = renderHook(() => usePaneContextMenu({ ...args } as never));
    await open(third.result);
    await act(async () => {});
    expect(third.result.current.recordingServed).toBe('v1');
    apiMock.currentKey = 'b|win';
    third.rerender();
    expect(third.result.current.recordingServed).toBe('checking');
  });
});
