import { render, screen, fireEvent } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { PaneBodyOverlays } from './PaneBodyOverlays';

const baseProps = {
  paneId: 'pane-1',
  connStatus: 'open' as const,
  paneStatus: 'running' as const,
  exitCode: undefined,
  inputLocked: false,
  dropZone: null,
  beingDragged: false,
  dictating: false,
  dictationPhase: null,
  dictationAboutToCancel: false,
  showsReaderChip: false,
  scrolledAway: false,
  newLinesCount: 0,
  lastOutputMs: 0,
  isMobileView: false,
  settling: false,
  getViewport: vi.fn(),
  jumpToLive: vi.fn(),
};

describe('PaneBodyOverlays pane state messaging', () => {
  it('shows reconnecting state in the pane body', () => {
    render(<PaneBodyOverlays {...baseProps} connStatus="reconnecting" />);

    expect(screen.getByRole('status', { name: /reconnecting to pane/i })).toBeInTheDocument();
    expect(screen.getByText(/input is paused until the connection returns/i)).toBeInTheDocument();
  });

  it('shows closed state with the exit code', () => {
    render(
      <PaneBodyOverlays
        {...baseProps}
        connStatus="closed"
        paneStatus="exited"
        exitCode={130}
      />,
    );

    expect(screen.getByRole('status', { name: /session closed/i })).toBeInTheDocument();
    expect(screen.getByText(/exited with code 130/i)).toBeInTheDocument();
  });

  it('marks a locked pane without a per-pane card (the grid banner explains it)', () => {
    render(<PaneBodyOverlays {...baseProps} inputLocked />);

    expect(screen.getByRole('status', { name: /typing is on another device/i })).toBeInTheDocument();
    expect(screen.queryByText(/type here in the banner/i)).toBeNull();
  });

  it('offers a Resume button on a suspended (resumable) closed pane', async () => {
    const onResume = vi.fn().mockResolvedValue(undefined);
    render(
      <PaneBodyOverlays
        {...baseProps}
        connStatus="closed"
        paneStatus="exited"
        resumable
        onResume={onResume}
      />,
    );

    const btn = screen.getByRole('button', { name: /resume session/i });
    expect(screen.getByText(/resume to continue this session/i)).toBeInTheDocument();
    fireEvent.click(btn);
    expect(onResume).toHaveBeenCalledTimes(1);
  });

  it("makes no Resume offer on a pane paused by Don't resume", () => {
    const onResume = vi.fn().mockResolvedValue(undefined);
    render(
      <PaneBodyOverlays
        {...baseProps}
        connStatus="closed"
        paneStatus="exited"
        resumable
        restartPaused
        onResume={onResume}
      />,
    );

    expect(screen.getByRole('status', { name: /paused after a restart/i })).toBeInTheDocument();
    expect(screen.getByText(/not resumed, as set in settings › terminal & panes › sessions › after a restart/i)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /resume/i })).toBeNull();
    expect(screen.queryByText(/resume to continue this session/i)).toBeNull();
    expect(onResume).not.toHaveBeenCalled();
  });

  it('stays informational (no Resume button) on a closed pane that is not resumable', () => {
    render(
      <PaneBodyOverlays
        {...baseProps}
        connStatus="closed"
        paneStatus="exited"
        exitCode={0}
      />,
    );

    expect(screen.queryByRole('button', { name: /resume session/i })).toBeNull();
  });
});

describe('PaneBodyOverlays jump-to-latest chip', () => {
  it('shows the chip only when enabled and scrolled away, and forwards the click', () => {
    const jumpToLive = vi.fn();
    const { rerender } = render(
      <PaneBodyOverlays {...baseProps} showsReaderChip scrolledAway={false} jumpToLive={jumpToLive} />,
    );
    expect(screen.queryByRole('button', { name: /scroll to latest/i })).toBeNull();

    rerender(
      <PaneBodyOverlays
        {...baseProps}
        showsReaderChip
        scrolledAway
        newLinesCount={7}
        lastOutputMs={Date.now() - 3000}
        jumpToLive={jumpToLive}
      />,
    );
    const chip = screen.getByRole('button', { name: /scroll to latest/i });
    expect(chip).toHaveTextContent('7 new');
    fireEvent.click(chip);
    expect(jumpToLive).toHaveBeenCalledTimes(1);
  });
});
