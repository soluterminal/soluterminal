import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { useStore } from '../store';
import type { OrchestratorHostInfo } from '../types';
import { NewPaneDialog } from './NewPaneDialog';
import { useEntitlements } from '../entitlements';



// Host availability as a current daemon reports it.
const HOSTS: OrchestratorHostInfo[] = [
  { agent: 'claude', label: 'Claude', supported: true, installed: true, available: true },
  { agent: 'codex', label: 'Codex', supported: true, installed: true, available: true },
  { agent: 'muse', label: 'Muse', supported: false, installed: false, available: false },
];

const originalCreatePane = useStore.getState().createPane;
const originalSplitFocused = useStore.getState().splitFocused;
const originalPanes = useStore.getState().panes;
const originalLoadOrchestratorHosts = useStore.getState().loadOrchestratorHosts;

function renderDialog(
  createPane: ReturnType<typeof vi.fn>,
  workspaceContextPack = '',
  hosts: OrchestratorHostInfo[] | null = HOSTS,
) {
  useStore.setState({
    createPane: createPane as typeof originalCreatePane,
    focusedPaneId: null,
    activeWorkspace: 'Default',
    workspaces: [{ name: 'Default', layout: null, paneIds: [], activeAt: new Date(0).toISOString(), contextPack: workspaceContextPack }],
    orchestratorHosts: hosts,
    loadOrchestratorHosts: vi.fn().mockResolvedValue(undefined),
  });
  return render(
    <NewPaneDialog open onClose={vi.fn()} splitDirection={null} />,
  );
}

describe('NewPaneDialog agent choices', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.spyOn(api, 'listDirs').mockResolvedValue([]);
    useEntitlements.setState({ loaded: true, team: true, operator: true });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    useStore.setState({
      createPane: originalCreatePane,
      splitFocused: originalSplitFocused,
      focusedPaneId: null,
      panes: originalPanes,
      orchestratorHosts: null,
      loadOrchestratorHosts: originalLoadOrchestratorHosts,
    });
  });

  it('offers no Focus chips and creates a terminal pane', async () => {
    const createPane = vi.fn().mockResolvedValue({});
    renderDialog(createPane);
    expect(screen.queryByRole('button', { name: 'claude · Focus' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'codex · Focus' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'muse · Focus' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'claude' }));
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));
    await waitFor(() => expect(createPane).toHaveBeenCalled());
    expect(createPane.mock.calls[0][0]).not.toHaveProperty('kind');
  });

  it('creates Codex in Terminal mode with explicit agent identity', async () => {
    const createPane = vi.fn().mockResolvedValue({});
    renderDialog(createPane);

    fireEvent.click(screen.getByRole('button', { name: 'codex' }));
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));

    await waitFor(() => expect(createPane).toHaveBeenCalledWith(expect.objectContaining({
      cmd: ['codex'],
      title: 'codex',
      agent: 'codex',
    })));
    expect(createPane.mock.calls[0][0]).not.toHaveProperty('kind');
    expect(createPane.mock.calls[0][0]).not.toHaveProperty('provider');
    expect(createPane.mock.calls[0][0]).not.toHaveProperty('effort');
  });

  it('sends a workspace context-pack default for Claude', async () => {
    const createPane = vi.fn().mockResolvedValue({});
    renderDialog(createPane, '/packs/strategy');

    fireEvent.click(screen.getByRole('button', { name: 'claude' }));
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));

    await waitFor(() => expect(createPane).toHaveBeenCalledWith(expect.objectContaining({
      cmd: ['claude'],
      contextPack: '/packs/strategy',
    })));
  });

  it('sends the none sentinel when opting out of a workspace default', async () => {
    const createPane = vi.fn().mockResolvedValue({});
    renderDialog(createPane, '/packs/strategy');

    fireEvent.click(screen.getByRole('button', { name: 'claude' }));
    fireEvent.click(screen.getByRole('button', { name: 'None' }));
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));

    await waitFor(() => expect(createPane).toHaveBeenCalledWith(expect.objectContaining({
      cmd: ['claude'],
      contextPack: 'none',
    })));
  });

  it('does not send contextPack for a shell command', async () => {
    const createPane = vi.fn().mockResolvedValue({});
    renderDialog(createPane, '/packs/strategy');

    fireEvent.click(screen.getByRole('button', { name: 'Create' }));
    await waitFor(() => expect(createPane).toHaveBeenCalled());
    expect(createPane.mock.calls[0][0]).not.toHaveProperty('contextPack');
  });

  it('offers no Orchestrator or Muse presets to a public install', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false });
    renderDialog(vi.fn());
    expect(screen.queryByRole('button', { name: /Orchestrator/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /^muse/ })).toBeNull();
    expect(screen.getByRole('button', { name: 'codex' })).toBeInTheDocument();
    expect(screen.queryByPlaceholderText(/wsl-projects/)).toBeNull();
  });

  it('hides the GLM model chip and the raw resume chip from a public install', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false });
    renderDialog(vi.fn());
    fireEvent.click(screen.getByRole('button', { name: 'claude' }));
    expect(screen.queryByRole('button', { name: 'GLM' })).toBeNull();
    expect(screen.queryByText(/Z\.ai/)).toBeNull();
    expect(screen.queryByRole('button', { name: 'claude --resume' })).toBeNull();
    expect(screen.getByRole('button', { name: 'max' })).toBeInTheDocument();
  });

  it('keeps the resume chip for a team plan without operator tooling', () => {
    useEntitlements.setState({ loaded: true, team: true, operator: false });
    renderDialog(vi.fn());
    expect(screen.getByRole('button', { name: 'claude --resume' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'claude' }));
    expect(screen.queryByRole('button', { name: 'GLM' })).toBeNull();
  });

  it('highlights only the chip matching the command', () => {
    renderDialog(vi.fn());
    const strong = (name: string) => screen.getByRole('button', { name }).classList.contains('dialog-chip-strong');
    expect(strong('shell')).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'bash' }));
    expect(strong('shell')).toBe(false);
    expect(strong('bash')).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'codex' }));
    expect(strong('bash')).toBe(false);
    expect(strong('codex')).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'shell' }));
    expect(strong('shell')).toBe(true);
    expect(strong('codex')).toBe(false);
  });

  it('returns effort to default when leaving GLM, unless the user picked it', async () => {
    const createPane = vi.fn().mockResolvedValue({});
    renderDialog(createPane);
    const strong = (name: string) => screen.getByRole('button', { name }).classList.contains('dialog-chip-strong');
    fireEvent.click(screen.getByRole('button', { name: 'claude' }));
    fireEvent.click(screen.getByRole('button', { name: 'GLM' }));
    expect(strong('max')).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'Claude' }));
    expect(strong('max')).toBe(false);
    expect(strong('default')).toBe(true);
    // An explicit choice survives the switch.
    fireEvent.click(screen.getByRole('button', { name: 'GLM' }));
    fireEvent.click(screen.getByRole('button', { name: 'high' }));
    fireEvent.click(screen.getByRole('button', { name: 'Claude' }));
    expect(strong('high')).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));
    await waitFor(() => expect(createPane).toHaveBeenCalled());
    expect(createPane.mock.calls[0][0]).toMatchObject({ cmd: ['claude'], effort: 'high' });
    expect(createPane.mock.calls[0][0]).not.toHaveProperty('provider');
  });
});
