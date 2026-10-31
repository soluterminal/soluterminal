import { useEffect, useRef, useState, type MouseEvent as ReactMouseEvent } from 'react';
import { api, currentRoutingScope, type RoutingScope } from '../api';
import { useStore } from '../store';
import { confirmDialog, selectDialog } from '../lib/dialog';
import { confirmAutoModeChange } from '../lib/autoModeConfirm';
import type { PaneInfo } from '../types';
import { agentLabel, effectivePaneAgent } from '../lib/agentKind';
import { paneRunningWork } from '../lib/paneWork';
import { clearOwnerURL } from '../lib/ownerView';

type PaneAutoMode = NonNullable<PaneInfo['autoMode']>;

interface UsePaneContextMenuArgs {
  pane: PaneInfo;
  actionGuard?: () => boolean;
  captureGuard?: () => () => boolean;
  managedOwner?: boolean;
  scope?: RoutingScope;
  onPaneChanged?:(pane:PaneInfo)=>void;
  killPane: (id: string) => void | Promise<unknown>;
  setFocus: (id: string) => void;
  setTitleDraft: (title: string) => void;
  setEditingTitle: (editing: boolean) => void;
  setFlagMemoDraft: (memo: string) => void;
  setEditingFlagMemo: (editing: boolean) => void;
}

export function closePaneConfirmMessage(pane: PaneInfo, runningWork = 0): string {
  const base = `Close "${pane.title || pane.cmd.join(' ')}"?`;
  if (runningWork > 0) {
    return `${base} ${runningWork} task${runningWork === 1 ? '' : 's'} in this session ${runningWork === 1 ? 'is' : 'are'} still running.`;
  }
  return base;
}

export function confirmClosePane(pane: PaneInfo): Promise<boolean> {
  // The harness client reports running work over OSC 7658; the dialog has no
  // other way to know a turn/background job is in flight before killing it.
  return confirmDialog({ message: closePaneConfirmMessage(pane, paneRunningWork(pane.id)), confirmLabel: 'Close', danger: true });
}

export function confirmConvertPaneRole(pane: PaneInfo, toOrchestrator: boolean): Promise<boolean> {
	const label = agentLabel(effectivePaneAgent(pane) ?? 'claude');
  if (!toOrchestrator) {
    return confirmDialog({
      message: `Remove the orchestrator role from "${pane.title}"? ${label} restarts in place (conversation kept) without the orchestrator brief.`,
      confirmLabel: 'Remove & restart',
    });
  }
  if (pane.claudeSessionId || pane.codexThreadId) {
    return confirmDialog({
      message: `Convert "${pane.title}" to an orchestrator? ${label} restarts in place (conversation kept) with the orchestrator brief.`,
      confirmLabel: 'Convert & restart',
    });
  }
  return confirmDialog({
    message: `Convert "${pane.title}" to an orchestrator? This ends what's running in the pane and starts a fresh ${label} session with the orchestrator brief.`,
    confirmLabel: 'Convert',
    danger: true,
  });
}

export function usePaneContextMenu({
  pane,
  actionGuard = ()=>true, captureGuard, managedOwner = false, scope, onPaneChanged,
  killPane,
  setFocus,
  setTitleDraft,
  setEditingTitle,
  setFlagMemoDraft,
  setEditingFlagMemo,
}: UsePaneContextMenuArgs) {
  const updatePane=async(request:()=>Promise<PaneInfo>)=> {
   const valid=captureGuard?.() ?? actionGuard;if(!valid())return;
   try {const changed=await request();if(valid()){if(onPaneChanged)onPaneChanged(changed);else useStore.setState(s=>s.panes[pane.id]?.pid===pane.pid?{panes:{...s.panes,[pane.id]:changed}}:{});}}catch{/* existing refresh retains authoritative data */}
  };
  const [ctxMenu, setCtxMenu] = useState<{ x: number; y: number } | null>(null);
  const ctxMenuRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!ctxMenu) return;
    if (!actionGuard()) { setCtxMenu(null); return; }
    const onAway = (e: MouseEvent | KeyboardEvent) => {
      if (e instanceof KeyboardEvent) {
        if (e.key === 'Escape') setCtxMenu(null);
        return;
      }
      const target = e.target as Node | null;
      if (target && ctxMenuRef.current?.contains(target)) return;
      if (!actionGuard()) return;
      setCtxMenu(null);
    };
    window.addEventListener('mousedown', onAway, { capture: true });
    window.addEventListener('keydown', onAway as EventListener);
    return () => {
      window.removeEventListener('mousedown', onAway, { capture: true });
      window.removeEventListener('keydown', onAway as EventListener);
    };
  }, [ctxMenu, actionGuard]);

  return {
    ctxMenu,
    ctxMenuRef,
    openContextMenu: (e: ReactMouseEvent) => {
      if (!actionGuard()) return;
      e.preventDefault();
      e.stopPropagation();
      setFocus(pane.id);
      setCtxMenu({ x: e.clientX, y: e.clientY });
    },
    closeContextMenu: () => setCtxMenu(null),
    actions: {
      splitRight: () => {
        if(managedOwner) return;
        if (!actionGuard()) return;
      setCtxMenu(null);
        window.dispatchEvent(new CustomEvent('solu:open-pane-dialog', { detail: { split: 'horizontal' } }));
      },
      splitDown: () => {
        if(managedOwner) return;
        if (!actionGuard()) return;
      setCtxMenu(null);
        window.dispatchEvent(new CustomEvent('solu:open-pane-dialog', { detail: { split: 'vertical' } }));
      },
      clonePane: () => {
        if(managedOwner) return;
        if (!actionGuard()) return;
      setCtxMenu(null);
        void useStore.getState().cloneFocused('horizontal');
      },
      moveToWorkspace: async () => {
        if (managedOwner || !actionGuard()) return;
        setCtxMenu(null);
        const state = useStore.getState();
        const originScope = scope ?? currentRoutingScope();
        const generation = state.navigationGeneration;
        const valid = captureGuard?.() ?? (() => actionGuard() && originScope.key === currentRoutingScope().key
          && generation === useStore.getState().navigationGeneration);
        const source = state.activeWorkspace;
        const destinations = state.workspaces
          .filter((w) => w.name !== '_current' && w.name !== source)
          .filter((w) => !state.activeBackend || !w.backend || w.backend === state.activeBackend)
          .map((w) => w.name)
          .sort((a, b) => a.localeCompare(b));
        if (!source || source === '_current') {
          state.toast('Save this pane in a named workspace before moving it.', { kind: 'warn', icon: '⚠' });
          return;
        }
        if (destinations.length === 0) {
          state.toast('Create another named workspace before moving this pane.', { kind: 'info', icon: '○' });
          return;
        }
        const destination = await selectDialog({
          title: 'Move pane to workspace',
          message: `Move “${pane.title || 'this pane'}” from ${source}. The session keeps running.`,
          options: destinations,
          selectLabel: 'Destination workspace',
          confirmLabel: 'Move',
        });
        if (!destination || !valid()) return;
        await state.movePaneToWorkspace(pane.id, destination, originScope);
      },
      newSession: () => {
        if(managedOwner) return;
        if (!actionGuard()) return;
      setCtxMenu(null);
        window.dispatchEvent(new CustomEvent('solu:open-pane-dialog', { detail: { split: null } }));
      },
      createSchedule: () => {
        if (!actionGuard()) return;
      setCtxMenu(null);
        window.dispatchEvent(new CustomEvent('solu:open-schedule-dialog', {
          detail: { paneId: pane.id, fromPaneMenu: true },
        }));
      },
      setAutoMode: (mode: PaneAutoMode) => {
        if (!actionGuard()) return;
      setCtxMenu(null);
        // Always approve asks first, the same as the header chip and palette.
        // The view/scope guard is captured at the click and checked again
        // after the confirmation; updatePane also re-checks the pane's pid.
        const originScope = scope ?? currentRoutingScope();
        const generation = useStore.getState().navigationGeneration;
        const valid = captureGuard?.() ?? (() => actionGuard() && originScope.key === currentRoutingScope().key
          && generation === useStore.getState().navigationGeneration);
        void confirmAutoModeChange((pane.autoMode ?? '') as PaneAutoMode, mode, pane.title).then((ok) => {
          if (ok && valid()) void updatePane(()=>api.setPaneAutoMode(pane.id,mode,originScope));
        });
      },
      toggleAnnounceDelegation: () => {
        if (!actionGuard()) return;
      setCtxMenu(null);
        // Always writes an EXPLICIT value: a user who unticks an orchestrator
        // pane means "off", not "back to the default that just turned it on".
        void updatePane(()=>api.setPaneAnnounceDelegation(pane.id,!(pane.announceDelegation===true),scope));
      },
      editFlagMemo: () => {
        if (!actionGuard()) return;
      setCtxMenu(null);
        setFlagMemoDraft(pane.flagMemo ?? '');
        setEditingFlagMemo(true);
      },
      unflag: () => {
        if (!actionGuard()) return;
      setCtxMenu(null);
        void updatePane(()=>api.setPaneFlag(pane.id,false,'',scope));
      },
      flagForAttention: () => {
        if (!actionGuard()) return;
      setCtxMenu(null);
        setFlagMemoDraft('');
        setEditingFlagMemo(true);
        void updatePane(()=>api.setPaneFlag(pane.id,true,'',scope));
      },
      rename: () => {
        if (!actionGuard()) return;
      setCtxMenu(null);
        setTitleDraft(pane.title);
        setEditingTitle(true);
      },
      convertRole: async () => {
        if(managedOwner) return;
        if (!actionGuard()) return;
      setCtxMenu(null);
        const valid = captureGuard?.() ?? actionGuard;
        const toOrchestrator = pane.role !== 'orchestrator';
        if (await confirmConvertPaneRole(pane, toOrchestrator) && valid()) {
          void updatePane(()=>api.setPaneRole(pane.id,toOrchestrator ? 'orchestrator':'',pane.cols,pane.rows,scope));
        }
      },
      closePane: async () => {
        if (!actionGuard()) return;
      setCtxMenu(null);
        if (managedOwner) { clearOwnerURL();useStore.getState().returnToWorkspace(); return; }
        const valid = captureGuard?.() ?? actionGuard;
        if (await confirmClosePane(pane) && valid()) void killPane(pane.id);
      },
    },
  };
}
