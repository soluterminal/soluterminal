import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PaneInfo } from '../types';
import { act } from 'react';
import { api, configureApi, type SystemStatus } from '../api';
import { useStore } from '../store';
import { resetAgentMemoryGrowthForTests } from '../lib/agentMemory';
import { resetHostMemoryForTests } from '../lib/useAgentMemoryPill';
import { AgentMemoryChip } from './AgentMemoryChip';
import { DialogHost } from './DialogHost';

const codexPane = {
  id: 'codex-focus',
  kind: 'native',
  agent: 'codex',
  status: 'running',
  codexThreadId: 'thread-1',
  agentRssMB: 384,
} as PaneInfo;

const hostWith = (memTotalMB: number | null) => ({ host: { memTotalMB } }) as unknown as SystemStatus;

function setWidth(value: number) {
  Object.defineProperty(window, 'innerWidth', { configurable: true, value });
}

describe('AgentMemoryChip', () => {
  const desktopWidth = window.innerWidth;
  let systemStatus: ReturnType<typeof vi.spyOn>;
  beforeEach(() => {
    systemStatus = vi.spyOn(api, 'systemStatus').mockResolvedValue(hostWith(16384));
  });
  afterEach(() => {
    useStore.setState({ dialog: null, alwaysShowAgentMemory: false });
    resetHostMemoryForTests();
    resetAgentMemoryGrowthForTests();
    setWidth(desktopWidth);
    vi.restoreAllMocks();
  });

  it('stays hidden while memory is usual, and does not ask for the machine RAM', () => {
    const { container } = render(<AgentMemoryChip pane={codexPane} />);
    expect(container).toBeEmptyDOMElement();
    expect(systemStatus).not.toHaveBeenCalled();
  });

  it('"Always show agent memory" adds a read-only readout: no button, no restart on click', () => {
    useStore.setState({ alwaysShowAgentMemory: true });
    const onRestart = vi.fn();
    render(<><AgentMemoryChip pane={codexPane} onRestart={onRestart} /><DialogHost /></>);
    expect(screen.queryByRole('button', { name: /Agent memory/ })).toBeNull();
    const chip = screen.getByLabelText('Agent memory 384 MB');
    expect(chip.tagName).toBe('SPAN');
    expect(chip).not.toHaveClass('heavy');
    fireEvent.click(chip);
    expect(screen.queryByText('Restart this agent?')).toBeNull();
    expect(onRestart).not.toHaveBeenCalled();
  });

  it('shows unusual memory as "Restart?" and restarts only after the confirm', async () => {
    const onRestart = vi.fn();
    render(<><AgentMemoryChip pane={{ ...codexPane, agentRssMB: 1843 }} onRestart={onRestart} /><DialogHost /></>);
    const pill = await screen.findByRole('button', { name: 'Agent memory 1.8 GB. Restart?' });
    expect(pill).toHaveTextContent('1.8 GB · Restart?');
    expect(pill).toHaveClass('heavy');

    fireEvent.click(pill);
    expect(await screen.findByText('Restart this agent?')).toBeInTheDocument();
    expect(screen.getByText(/The conversation is kept/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    await vi.waitFor(() => expect(screen.queryByText('Restart this agent?')).toBeNull());
    expect(onRestart).not.toHaveBeenCalled();

    fireEvent.click(pill);
    fireEvent.click(await screen.findByRole('button', { name: 'Restart' }));
    await vi.waitFor(() => expect(onRestart).toHaveBeenCalledTimes(1));
  });

  it('measures "unusual" against 10% of the machine RAM', async () => {
    systemStatus.mockResolvedValue(hostWith(32768));
    const { container, rerender } = render(<AgentMemoryChip pane={{ ...codexPane, agentRssMB: 2048 }} />);
    await vi.waitFor(() => expect(systemStatus).toHaveBeenCalledTimes(1));
    await Promise.resolve();
    expect(container).toBeEmptyDOMElement();
    rerender(<AgentMemoryChip pane={{ ...codexPane, agentRssMB: 3400 }} />);
    expect(await screen.findByLabelText('Agent memory 3.3 GB')).toHaveClass('heavy');
    expect(systemStatus).toHaveBeenCalledTimes(1);
  });

  it('asks the new machine for its RAM after a machine switch', async () => {
    systemStatus.mockResolvedValueOnce(hostWith(65536));
    const { container } = render(<AgentMemoryChip pane={{ ...codexPane, agentRssMB: 2048 }} />);
    await vi.waitFor(() => expect(systemStatus).toHaveBeenCalledTimes(1));
    await Promise.resolve();
    expect(container).toBeEmptyDOMElement();
    // The same header, now on a machine with 16 GB: 2 GB is unusual there.
    act(() => configureApi({ prefix: '/m/other' }));
    expect(await screen.findByLabelText('Agent memory 2.0 GB')).toHaveClass('heavy');
    expect(systemStatus).toHaveBeenCalledTimes(2);
    act(() => configureApi({ prefix: '' }));
  });

  it('falls back to the 1.5 GB floor when the machine RAM is unknown', async () => {
    systemStatus.mockRejectedValue(new Error('offline'));
    render(<AgentMemoryChip pane={{ ...codexPane, agentRssMB: 1536 }} />);
    expect(await screen.findByLabelText('Agent memory 1.5 GB')).toBeInTheDocument();
  });

  it('flags fast growth below the size floor', async () => {
    const { rerender } = render(<AgentMemoryChip pane={{ ...codexPane, agentRssMB: 600 }} onRestart={vi.fn()} />);
    rerender(<AgentMemoryChip pane={{ ...codexPane, agentRssMB: 1200 }} onRestart={vi.fn()} />);
    expect(await screen.findByRole('button', { name: 'Agent memory 1.2 GB. Restart?' })).toBeInTheDocument();
  });

  it('without a restart on offer, unusual memory is an amber readout', async () => {
    render(<AgentMemoryChip pane={{ ...codexPane, agentRssMB: 1843 }} />);
    const chip = await screen.findByLabelText('Agent memory 1.8 GB');
    expect(chip.tagName).toBe('SPAN');
    expect(chip).toHaveClass('heavy');
  });

  it('on a phone shows only unusual memory, even with "Always show"', async () => {
    setWidth(390);
    useStore.setState({ alwaysShowAgentMemory: true });
    const { container, unmount } = render(<AgentMemoryChip pane={codexPane} />);
    expect(container).toBeEmptyDOMElement();
    unmount();
    render(<AgentMemoryChip pane={{ ...codexPane, agentRssMB: 1843 }} />);
    expect(await screen.findByLabelText('Agent memory 1.8 GB')).toBeInTheDocument();
  });

  it('stays hidden until the daemon has sampled memory', () => {
    useStore.setState({ alwaysShowAgentMemory: true });
    const { container } = render(<AgentMemoryChip pane={{ ...codexPane, agentRssMB: 0 }} />);
    expect(container).toBeEmptyDOMElement();
  });
});
