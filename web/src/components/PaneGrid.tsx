import { workspaceViewLayout } from '../lib/workspaceView';
import { useEffect, useMemo, useRef, useState } from 'react';
import { Panel, PanelGroup, PanelResizeHandle } from 'react-resizable-panels';
import type { ImperativePanelGroupHandle } from 'react-resizable-panels';
import { SessionClaimOverlay } from './SessionClaimOverlay';
import { leafCount, useStore } from '../store';
import { isMobileViewport } from '../lib/mobile';
import { collectLeaves } from '../lib/layout';
import { createViewportLayoutTracker } from '../lib/autoLayout';
import { currentRoutingScope } from '../api';
import type { Layout, PaneInfo } from '../types';
import { PaneRecovery } from './PaneRecovery';
import { PaneSlot } from './PaneSlot';
import { NativePane } from './NativePane';
import { BrowserPane } from './BrowserPane';
import { FileTile } from './FileTile';
import { DiffTile } from './DiffTile';

function renderPaneLeaf(pane: PaneInfo, _paneIndex: number) {
 if (pane.recoveryError) return <PaneRecovery key={pane.id + ':r'} pane={pane} />;
 if (pane.kind === 'native') return <NativePane key={pane.id + ':n'} pane={pane} />;
 return <PaneSlot key={pane.id} paneId={pane.id} />;
}

// Mobile breakpoint. Keep in sync with styles.css and App.tsx.
const MOBILE_BREAKPOINT_PX = 800;
function useIsMobile() {
  const [m, setM] = useState(
    isMobileViewport(MOBILE_BREAKPOINT_PX),
  );
  useEffect(() => {
    const on = () => setM(isMobileViewport(MOBILE_BREAKPOINT_PX));
    window.addEventListener('resize', on);
    window.visualViewport?.addEventListener('resize', on);
    return () => {
      window.removeEventListener('resize', on);
      window.visualViewport?.removeEventListener('resize', on);
    };
  }, []);
  return m;
}

function DeadPane({ paneId }: { paneId: string }) {
  const closePaneInLayout = useStore((s) => s.closePaneInLayout);
  return (
    <div className="pane">
      <div className="pane-header">
        <span className="pane-title">Session ended</span>
        <span className="pane-cwd">{paneId}</span>
        <div className="pane-actions">
          <button className="iconbtn" onClick={() => closePaneInLayout(paneId)}>×</button>
        </div>
      </div>
      <div className="pane-empty">
        This session is no longer running. Close this tile and start a new one.
      </div>
    </div>
  );
}

function LayoutNode({ node, paneIndexById }: { node: Layout; paneIndexById: Record<string, number> }) {
  // IMPORTANT: All hooks MUST be called unconditionally at the top of
  // the function, before any early returns. React reconciliation can
  // reuse the SAME LayoutNode instance across renders where `node`
  // changes type (e.g. a split becoming a pane after a close, or
  // positional reuse during workspace-switch reconciliation). If a
  // hook is only called when node.type === 'split', React sees the
  // hook count change and throws Error #310. Keep the refs/effects
  // up here and only CONSUME them in the split branch below.
  const pane = useStore((s) => (node.type === 'pane' ? s.panes[node.paneId] : null));
  const browser = useStore((s) => (node.type === 'browser' ? s.browsers[node.browserId] : null));
  const file = useStore((s) => (node.type === 'file' ? s.files[node.fileId] : null));
  const setLayout = useStore((s) => s.setLayout);
  const presentationActive = useStore(s=>s.mainPresentation.kind === 'workspace');
  const temporaryView = useStore(s => !!s.workspaceView);
  const userResizing = useRef(false);
  const groupRef = useRef<ImperativePanelGroupHandle>(null);

  const splitRatio = node.type === 'split' ? node.ratio : null;
  // react-resizable-panels only applies `defaultSize` on initial mount.
  // When a workspace switch changes `node.ratio`, React reuses the same
  // PanelGroup instance in the same tree position — so the library
  // keeps the PREVIOUS workspace's pixel sizes. Without this effect,
  // onLayout would then fire with the stale sizes and overwrite the
  // target workspace's saved ratio with the outgoing workspace's
  // ratio. Imperatively setLayout on the ref matches the component to
  // whatever the store says `splitRatio` is, without remounting panes.
  useEffect(() => {
    if (splitRatio === null) return;
    const target = splitRatio * 100;
    const cur = groupRef.current?.getLayout();
    if (!cur || cur.length < 2) return;
    if (Math.abs(cur[0] - target) < 0.1) return;
    groupRef.current?.setLayout([target, 100 - target]);
  }, [splitRatio, presentationActive]);

  if (node.type === 'pane') {
    return pane
      ? renderPaneLeaf(pane, paneIndexById[pane.id] ?? 0)
      : <DeadPane paneId={node.paneId} />;
  }
  if (node.type === 'browser') {
    return browser ? <BrowserPane info={browser} /> : <div className="pane" />;
  }
  if (node.type === 'file') {
    return file ? <FileTile info={file} /> : <div className="pane" />;
  }
  if (node.type === 'diff') {
    // Diff leaves are self-contained — render straight from the node.
    return <DiffTile leaf={node} />;
  }
  if (node.type === 'tasks') return null;

  const direction = node.direction === 'horizontal' ? 'horizontal' : 'vertical';
  const initialA = (splitRatio ?? node.ratio) * 100;

  const onLayout = (sizes: number[]) => {
    if (!presentationActive || useStore.getState().mainPresentation.kind !== 'workspace' || useStore.getState().workspaceView) return;
    // Read at event time instead of subscribing: a per-node rootLayout
    // subscription meant every LayoutNode re-ran its selector (and
    // re-rendered) on every drag-frame setLayout, at every tree depth.
    const rootLayout = useStore.getState().layout;
    if (!rootLayout || sizes.length < 2) return;
    // Mount-time and imperative layout settling are not user edits. Only
    // a pointer/keyboard resize can opt the workspace into custom mode.
    if (!userResizing.current) return;
    const newRatio = sizes[0] / (sizes[0] + sizes[1]);
    // Ignore rounding / pixel-snap drift (<0.5%) — those come from
    // mount-time layout settling and would otherwise save a slightly
    // different ratio than the one on disk, accumulating error over
    // many sessions.
    if (Math.abs(newRatio - node.ratio) < 0.005) return;
    const updated = replaceNode(rootLayout, node, { ...node, ratio: newRatio });
    if (updated !== rootLayout) setLayout(updated);
  };

  return (
    <PanelGroup ref={groupRef} direction={direction} onLayout={onLayout}>
      <Panel defaultSize={initialA} minSize={5}>
        <LayoutNode node={node.a} paneIndexById={paneIndexById} />
      </Panel>
      <PanelResizeHandle
        disabled={temporaryView}
        onDragging={(dragging) => {
          userResizing.current = dragging;
          if (dragging) useStore.getState().beginCustomLayout();
        }}
        onKeyDownCapture={(event) => {
          if (event.key.startsWith('Arrow')) { userResizing.current = true; useStore.getState().beginCustomLayout(); }
        }}
        onKeyUp={() => { userResizing.current = false; }}
      />
      <Panel defaultSize={100 - initialA} minSize={5}>
        <LayoutNode node={node.b} paneIndexById={paneIndexById} />
      </Panel>
    </PanelGroup>
  );
}

function replaceNode(root: Layout, target: Layout, replacement: Layout): Layout {
  if (root === target) return replacement;
  if (root.type === 'split') {
    const a = replaceNode(root.a, target, replacement);
    const b = replaceNode(root.b, target, replacement);
    if (a !== root.a || b !== root.b) return { ...root, a, b };
  }
  return root;
}


export function PaneGrid() {
  const savedLayout = useStore((s) => s.layout);
  const view = useStore((s) => s.workspaceView);
  const panes = useStore((s) => s.panes);
  const workspaces = useStore((s) => s.workspaces);
  const activeWorkspace = useStore((s) => s.activeWorkspace);
  const activeBackend = useStore((s) => s.activeBackend);
  const layout = useMemo(() => workspaceViewLayout({ layout: savedLayout, workspaceView: view, panes, workspaces, activeWorkspace, activeBackend }), [savedLayout, view, panes, workspaces, activeWorkspace, activeBackend]);
  const zoomedPaneId = useStore((s) => s.zoomedPaneId);
  const isMobile = useIsMobile();
  const rootRef = useRef<HTMLDivElement>(null);
  const viewportChanged = useRef(createViewportLayoutTracker());
  const mode = useStore(s => s.paneLayoutMode);
  const count = leafCount(layout);
  useEffect(() => {
    const root = rootRef.current;
    if (!root || isMobile || view) return;
    let timer = 0;
    const measure = () => {
      const bounds = root.getBoundingClientRect();
      if (bounds.width <= 0 || bounds.height <= 0) return;
      const state = useStore.getState();
      const saved = state.workspaces.find(w => w.name === state.activeWorkspace && (!w.backend || w.backend === state.activeBackend));
      const key = `${currentRoutingScope().key}:${state.activeWorkspace}`;
      if (viewportChanged.current(key, bounds.width, bounds.height, !!saved?.layoutMode && !!saved.layoutColumns)) {
        state.updateLayoutViewport(bounds.width, bounds.height);
      }
    };
    measure();
    const observer = new ResizeObserver(() => {
      window.clearTimeout(timer); timer = window.setTimeout(measure, 100);
    });
    observer.observe(root);
    return () => { observer.disconnect(); window.clearTimeout(timer); };
  }, [activeWorkspace, count, mode, isMobile, view]);
  // Which leaf to show in "single-leaf" mode. On mobile we always run in
  // this mode (pane switching is via tab bar); on desktop we only hit it
  // when the user explicitly hit Alt+Z to zoom.
  const mobileFallbackId = useStore((s) => {
    const leaves = collectLeaves(workspaceViewLayout(s));
    const ids = leaves.flatMap((leaf) => {
      if (leaf.type === 'pane') return [leaf.paneId];
      if (leaf.type === 'browser') return [leaf.browserId];
      if (leaf.type === 'file') return [leaf.fileId];
      if (leaf.type === 'diff') return [leaf.diffId];
      if (leaf.type === 'tasks') return [leaf.tasksId];
      return [];
    });
    if (s.focusedPaneId && ids.includes(s.focusedPaneId)) return s.focusedPaneId;
    return ids[0] ?? null;
  });
  const singleId = zoomedPaneId ?? (isMobile ? mobileFallbackId : null);
  const singlePane = useStore((s) => (singleId ? s.panes[singleId] : null));
  const singleBrowser = useStore((s) => (singleId ? s.browsers[singleId] : null));
  const singleFile = useStore((s) => (singleId ? s.files[singleId] : null));
  // Diff leaves live in the layout, not a map — find the node by id. The
  // returned node is the actual tree object (stable ref while layout is
  // unchanged), so this selector doesn't churn React.
  const singleDiff = useStore((s) => {
    if (!singleId) return null;
    const leaf = collectLeaves(workspaceViewLayout(s)).find((l) => l.type === 'diff' && l.diffId === singleId);
    return leaf && leaf.type === 'diff' ? leaf : null;
  });
  const paneIndexById = useMemo(() => {
    const out: Record<string, number> = {};
    for (const [idx, leaf] of collectLeaves(layout).entries()) {
      if (leaf.type === 'pane') out[leaf.paneId] = idx + 1;
    }
    return out;
  }, [layout]);
  if (!layout) return view?.kind === 'leads' ? <div className="pane-empty">
    No live leads. <button type="button" onClick={() => useStore.getState().returnWorkspaceLayout()}>Show full workspace</button>
  </div> : null;
  // Render only one leaf — either because of zoom (desktop) or because
  // we're on mobile where split rendering isn't useful on small screens.
  if (singleId) {
    return (
      <div ref={rootRef} style={{ width: '100%', height: '100%', position: 'relative' }} className="zoomed workspace-pane-grid">
        {singlePane ? renderPaneLeaf(singlePane, paneIndexById[singlePane.id] ?? 0)
          : singleBrowser ? <BrowserPane info={singleBrowser} />
          : singleFile ? <FileTile info={singleFile} />
          : singleDiff ? <DiffTile leaf={singleDiff} />
          : <DeadPane paneId={singleId} />}
        <SessionClaimOverlay />
      </div>
    );
  }
  return (
    <div ref={rootRef} className="workspace-pane-grid" style={{ width: '100%', height: '100%', position: 'relative' }}>
      <LayoutNode node={layout} paneIndexById={paneIndexById} />
      <SessionClaimOverlay />
    </div>
  );
}
