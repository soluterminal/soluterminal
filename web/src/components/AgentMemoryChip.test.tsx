import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { PaneInfo } from '../types';
import { useStore } from '../store';
import { AgentMemoryChip } from './AgentMemoryChip';
import { DialogHost } from './DialogHost';

const codexPane = {
  id: 'codex-focus',
  kind: 'native',
  agent: 'codex',
  codexThreadId: 'thread-1',
  agentRssMB: 384,
} as PaneInfo;

describe('AgentMemoryChip', () => {
  afterEach(() => useStore.setState({ dialog: null }));

  it('shows the agent RAM as a read-only readout: no button, no restart on click', () => {
    const refreshPane = vi.fn();
    useStore.setState({ refreshPane } as never);
    render(
      <>
        <AgentMemoryChip pane={codexPane} />
        <DialogHost />
      </>,
    );

    expect(screen.queryByRole('button', { name: /Agent memory/ })).toBeNull();
    const chip = screen.getByLabelText('Agent memory 384 MB');
    expect(chip.tagName).toBe('SPAN');
    expect(chip).toHaveClass('readonly');
    fireEvent.click(chip);
    expect(screen.queryByText(/Restart & reclaim/)).toBeNull();
    expect(refreshPane).not.toHaveBeenCalled();
  });

  it('marks heavy use', () => {
    render(<AgentMemoryChip pane={{ ...codexPane, agentRssMB: 1536 }} />);
    expect(screen.getByLabelText('Agent memory 1.5 GB')).toHaveClass('heavy');
  });

  it('stays hidden until the daemon has sampled memory', () => {
    const { container } = render(<AgentMemoryChip pane={{ ...codexPane, agentRssMB: 0 }} />);
    expect(container).toBeEmptyDOMElement();
  });
});
