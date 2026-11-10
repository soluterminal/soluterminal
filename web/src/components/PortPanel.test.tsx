import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, configureApi, setBackend } from '../api';
import { useStore } from '../store';
import type { PaneInfo, Port } from '../types';
import { PortPanel } from './PortPanel';
import { usePortPreferences } from '../lib/portPreferences';
import { openInSystemBrowser } from '../tauri-browser';
vi.mock('../tauri-browser', async (original) => ({
  ...(await original<typeof import('../tauri-browser')>()),
  openInSystemBrowser: vi.fn(),
}));
const port = (over: Partial<Port> = {}): Port => ({
  port: 5173,
  addr: '127.0.0.1',
  pid: 42,
  process: 'node',
  cmdline: 'node vite',
  cwd: '/home/user/project',
  firstAt: '2026-10-24T00:00:00Z',
  proto: 'tcp',
  scheme: 'http',
  webStatus: 'ready',
  paneId: 'pane-1',
  workspace: 'Project',
  paneTitle: 'Frontend',
  previewPath: '/api/proxy/5173~YWJj/',
  ...over,
});
const pane = { id: 'pane-1', title: 'Frontend', cwd: '/home/user/project' } as PaneInfo;
describe('workspace services', () => {
  beforeEach(() => {
    vi.mocked(openInSystemBrowser).mockReset().mockResolvedValue(true);
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () => new Response(JSON.stringify({ ticket: 'spp1.fixture.pass' }), { status: 200 }),
      ),
    );
    localStorage.clear();
    configureApi({ prefix: '', token: null });
    setBackend(null);
    usePortPreferences.setState({ scope: 'workspace', includeOther: false });
    useStore.setState({
      activeWorkspace: 'Project',
      activeBackend: 'wsl',
      panes: { 'pane-1': pane },
      workspaces: [],
      ports: [
        port(),
        port({ port: 6000, paneId: undefined, workspace: undefined }),
        port({ port: 7000, workspace: 'Other' }),
        port({ port: 5432, scheme: undefined, webStatus: 'internal', process: 'postgres' }),
      ],
      portsError: null,
      portsUpdatedAt: 1,
    });
  });
  afterEach(() => {
    configureApi({ prefix: '', token: null });
    setBackend(null);
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });
  it('shows current-workspace web services and hides other projects and TCP tools', () => {
    render(<PortPanel />);
    expect(screen.getByText(':5173')).toBeInTheDocument();
    expect(screen.queryByText(':7000')).toBeNull();
    expect(screen.queryByText(':5432')).toBeNull();
    expect(screen.getByRole('combobox', { name: 'Service scope' })).toHaveValue('workspace');
  });
  it('offers all services and other TCP listeners explicitly', () => {
    render(<PortPanel />);
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'all' } });
    expect(screen.getByText(':6000')).toBeInTheDocument();
    expect(screen.getByText(':7000')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('checkbox'));
    expect(screen.getByText(':5432')).toBeInTheDocument();
    const row = screen.getByText(':5432').closest('.port-row') as HTMLElement;
    expect(within(row).getByRole('button', { name: 'Open preview' })).toBeDisabled();
  });
  it('shares scope and listener preferences across simultaneously mounted panels', () => {
    render(
      <>
        <PortPanel />
        <PortPanel />
      </>,
    );
    const selectors = screen.getAllByRole('combobox');
    fireEvent.change(selectors[0], { target: { value: 'all' } });
    expect(selectors[1]).toHaveValue('all');
    fireEvent.click(screen.getAllByRole('checkbox')[0]);
    expect(screen.getAllByRole('checkbox')[1]).toBeChecked();
    expect(JSON.parse(localStorage.getItem('solu.ports.view.v3')!)).toEqual({
      scope: 'all',
      includeOther: true,
    });
  });
  it('keeps identical port numbers on different backends distinct when opened', () => {
    const open = vi.spyOn(useStore.getState(), 'openBrowserHere').mockImplementation(() => {});
    useStore.setState({ ports: [port({ backend: 'wsl' }), port({ backend: 'win' })] });
    usePortPreferences.setState({ scope: 'all' });
    const error = vi.spyOn(console, 'error').mockImplementation(() => {});
    render(<PortPanel />);
    fireEvent.click(
      within(screen.getByText('Windows').closest('.port-row') as HTMLElement).getByRole('button', {
        name: 'Open preview',
      }),
    );
    expect(open).toHaveBeenCalledWith(
      expect.objectContaining({ url: 'http://127.0.0.1:5173', backend: 'win', previewPath: '/api/proxy/5173~YWJj/' }),
    );
    expect(error).not.toHaveBeenCalled();
  });
  it('disables remote opening for an unowned service but preserves copying', () => {
    configureApi({ prefix: '/m/host', token: 'secret' });
    useStore.setState({ ports: [port({ paneId: undefined })] });
    render(<PortPanel />);
    expect(screen.getByRole('button', { name: 'Open preview' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Copy address' })).toBeEnabled();
    expect(screen.getByText(/Remote preview requires/)).toBeInTheDocument();
  });
  it('opens a cloud service in the system browser with its exact listener and a scoped pass', async () => {
    configureApi({ prefix: '/m/host', token: 'secret' });
    useStore.setState({ ports: [port({ backend: 'win' })] });
    usePortPreferences.setState({ scope: 'all' });
    render(<PortPanel />);
    await act(async () => fireEvent.click(screen.getByRole('button', { name: 'Open in browser' })));
    await vi.waitFor(() => expect(openInSystemBrowser).toHaveBeenCalledTimes(1));
    const [opened] = vi.mocked(openInSystemBrowser).mock.calls[0];
    expect(opened).toContain('/m/host/b/win/api/proxy/5173~YWJj/?access_token=spp1.fixture.pass');
    expect(opened).not.toContain('secret');
  });
  it('opens a local service at its own address and reports no popup error', async () => {
    const toast = vi.spyOn(useStore.getState(), 'toast');
    vi.mocked(openInSystemBrowser).mockResolvedValue(false); // window.open in a browser tab
    render(<PortPanel />);
    await act(async () => fireEvent.click(screen.getByRole('button', { name: 'Open in browser' })));
    await vi.waitFor(() => expect(openInSystemBrowser).toHaveBeenCalledWith('http://127.0.0.1:5173', expect.any(Event)));
    expect(toast).not.toHaveBeenCalled();
  });
  it('says why a WSL service sharing a Windows app\'s port can\'t open in the browser', async () => {
    const toast = vi.spyOn(useStore.getState(), 'toast');
    useStore.setState({ ports: [port({ backend: 'wsl' }), port({ backend: 'win', paneId: undefined })] });
    usePortPreferences.setState({ scope: 'all' });
    render(<PortPanel />);
    const wslRow = screen.getByText('WSL').closest('.port-row') as HTMLElement;
    await act(async () => fireEvent.click(within(wslRow).getByRole('button', { name: 'Open in browser' })));
    await vi.waitFor(() => expect(toast).toHaveBeenCalledWith(expect.stringContaining('A Windows app also uses port 5173'), expect.objectContaining({ kind: 'warn' })));
    expect(openInSystemBrowser).not.toHaveBeenCalled();
  });
  it('previews only pane services; an unowned local one opens in the browser instead', () => {
    useStore.setState({ ports: [port({ paneId: undefined })] });
    usePortPreferences.setState({ scope: 'all' });
    render(<PortPanel />);
    expect(screen.getByRole('button', { name: 'Open preview' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Open in browser' })).toBeEnabled();
    expect(screen.getByText(/Preview shows services started in a soluterminal pane/)).toBeInTheDocument();
  });
  it('copies the service address without copying cloud credentials', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    configureApi({ prefix: '/m/host', token: 'secret' });
    useStore.setState({ ports: [port()] });
    render(<PortPanel />);
    await act(async () => fireEvent.click(screen.getByRole('button', { name: 'Copy address' })));
    expect(writeText).toHaveBeenCalledWith('http://127.0.0.1:5173');
  });
  it('reveals the owning pane', () => {
    const reveal = vi.spyOn(useStore.getState(), 'revealPane').mockResolvedValue();
    render(<PortPanel />);
    fireEvent.click(screen.getByRole('button', { name: 'Go to pane' }));
    expect(reveal).toHaveBeenCalledWith('pane-1');
  });
  it('shows stale data and a recovery action after a refresh failure', () => {
    useStore.setState({ portsError: 'Could not refresh services.' });
    render(<PortPanel />);
    expect(screen.getByRole('status')).toHaveTextContent('Showing the last available services');
    expect(screen.getByRole('button', { name: 'Refresh services' })).toBeEnabled();
  });

  it('checks an unowned listener only after the user asks', async () => {
    const unknown = port({
      paneId: undefined,
      workspace: undefined,
      webStatus: 'unverified',
      scheme: undefined,
    });
    useStore.setState({ ports: [unknown] });
    usePortPreferences.setState({ scope: 'all' });
    const probe = vi
      .spyOn(api, 'probePort')
      .mockResolvedValue({ ...unknown, scheme: 'http', webStatus: 'ready' });
    const refresh = vi.spyOn(useStore.getState(), 'refreshPorts').mockResolvedValue();
    render(<PortPanel />);
    expect(probe).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Open preview' })).toBeDisabled();
    await act(async () =>
      fireEvent.click(screen.getByRole('button', { name: 'Check web service' })),
    );
    expect(probe).toHaveBeenCalledWith(unknown);
    expect(refresh).toHaveBeenCalled();
  });
});
