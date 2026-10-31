import type { Layout, PaneInfo, Workspace } from '../types';
import { buildNxMFromLeaves, buildPresetLayout, type LayoutPresetId } from './layout';
import { createWorkspacePaneTreeSelector, flattenPaneTree } from './workspacePaneTree';

export interface WorkspaceView {
  workspace: string;
  backend: 'wsl' | 'win' | null;
  kind: 'single' | 'team' | 'leads';
  paneId?: string;
  preset?: LayoutPresetId;
  grid?: { rows: number; cols: number };
}
export interface WorkspaceViewState {
  layout: Layout | null;
  workspaceView?: WorkspaceView | null;
  activeWorkspace?: string;
  activeBackend?: 'wsl' | 'win' | null;
  workspaces?: Workspace[];
  panes?: Record<string, PaneInfo>;
}

const selectTrees = createWorkspacePaneTreeSelector();

/** A presentation only: canonical layout (and therefore autosave) never changes. */
export function workspaceViewLayout(s: WorkspaceViewState): Layout | null {
  const view = s.workspaceView;
  if (!view || view.workspace !== s.activeWorkspace || view.backend !== (s.activeBackend ?? null) || !s.panes) return s.layout;
  const workspaces = s.workspaces?.some(w => w.name === s.activeWorkspace && (!w.backend || w.backend === s.activeBackend))
    ? s.workspaces : [{ name: s.activeWorkspace!, layout: s.layout, paneIds: [] } as unknown as Workspace];
  const tree = selectTrees(workspaces, s.panes, s.activeWorkspace!, s.layout, s.activeBackend).find(t => t.workspace.name === view.workspace && (!t.workspace.backend || t.workspace.backend === view.backend));
  const nodes = flattenPaneTree(tree?.roots ?? []);
  const selected = nodes.find(n => n.pane.id === view.paneId);
  const visible = view.kind === 'leads' ? nodes.filter(n => n.isLead)
    : selected ? view.kind === 'single' ? [selected] : flattenPaneTree([selected]) : [];
  if (!visible.length) return view.kind === 'leads' ? null : s.layout;
  const leaves: Layout[] = visible.map(n => ({ type: 'pane', paneId: n.pane.id }));
  const cols = Math.ceil(Math.sqrt(leaves.length));
  const grid = view.grid;
  const result = buildNxMFromLeaves(leaves, grid ? Math.max(grid.rows, Math.ceil(leaves.length / grid.cols)) : Math.ceil(leaves.length / cols), grid?.cols ?? cols);
  return view.preset ? buildPresetLayout(result, view.preset) : result;
}
