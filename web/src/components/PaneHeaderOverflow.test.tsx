import { useRef } from 'react';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { PaneDetail, PaneHeaderOverflow, overflowDecision } from './PaneHeaderOverflow';
import { titleShowsHandle, messageSharesOrigin } from '../lib/paneHeaderDetails';
import { PaneIdentityChip } from './PaneIdentityChip';
import type { PaneInfo } from '../types';

const pane: PaneInfo = { id: 'worker', title: '@api · Fix login', identity: { handle: 'api', workingOn: 'Fix login' }, cmd: ['bash'], cwd: '/', status: 'running', createdAt: '', pid: 1, cols: 80, rows: 24, lastActivityMs: 0, state: 'busy', hasClaude: false };

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('pane header details', () => {
  it('keeps fitting details and folds all present details when the title reserve is exceeded', () => {
    expect(overflowDecision(600, 600, 8)).toBe(0);
    expect(overflowDecision(600, 620, 8)).toBe(8);
    expect(overflowDecision(256, 900, 0)).toBe(0);
  });

  it('deduplicates a complete handle token in the title', () => {
    expect(titleShowsHandle(pane)).toBe(true);
    expect(titleShowsHandle({ ...pane, title: 'Fix @API: login' })).toBe(true);
    expect(titleShowsHandle({ ...pane, title: '@api-worker · Fix login' })).toBe(false);
    expect(titleShowsHandle({ ...pane, title: 'Fix login' })).toBe(false);
    render(<PaneIdentityChip pane={pane} deduplicateTitle />);
    expect(screen.queryByRole('button')).toBeNull();
  });

  it('combines only messages belonging to the same origin or squad', () => {
    const worker = { ...pane, origin: { source: 'agent' as const, leadPaneId: 'lead', byPaneId: 'starter', at: 1 }, lastAgentMessage: { fromPaneId: 'lead', fromLabel: '@lead', at: Date.now(), outcome: 'sent' as const } };
    expect(messageSharesOrigin(worker, {})).toBe(true);
    expect(messageSharesOrigin({ ...worker, lastAgentMessage: { ...worker.lastAgentMessage, fromPaneId: 'unrelated' } }, {})).toBe(false);
    expect(messageSharesOrigin({ ...worker, lastAgentMessage: undefined }, {})).toBe(false);
  });

  it('counts actual chips, keeps actions usable, restores focus on Escape, and unfolds after resize', async () => {
    let resize = () => {};
    let available = 320;
    vi.stubGlobal('ResizeObserver', class { constructor(cb: () => void) { resize = cb; } observe() {} disconnect() {} });
    vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockImplementation(() => available);
    vi.spyOn(HTMLElement.prototype, 'scrollWidth', 'get').mockReturnValue(800);
    const measure = vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({ width: 80, height: 20, top: 0, left: 0, right: 80, bottom: 20, x: 0, y: 0, toJSON() {} });
    const action = vi.fn();
    const zoom = vi.fn();
    function Header() {
      const ref = useRef<HTMLDivElement>(null);
      return <div ref={ref} onDoubleClick={zoom}><div data-testid="terminal" /><PaneHeaderOverflow headerRef={ref}>
        <span>Title</span><span className="pane-autoapproving-chip" data-testid="countdown"> approving in 60s</span><PaneDetail><button onClick={action}>Resume</button></PaneDetail>
        <PaneDetail>{null}</PaneDetail><PaneDetail><span>111 MB<input aria-label="Detail draft" defaultValue="draft" /></span></PaneDetail>
      </PaneHeaderOverflow></div>;
    }
    render(<Header />);
    const more = screen.getByRole('button', { name: '2 more pane details' });
    await act(async () => { await new Promise(resolve => setTimeout(resolve, 150)); });
    const measurements = measure.mock.calls.length;
    await act(async () => {
      screen.getByTestId('countdown').firstChild!.nodeValue = ' approving in 59s';
      await new Promise(resolve => setTimeout(resolve, 150));
    });
    expect(measure.mock.calls.length).toBe(measurements);
    fireEvent.click(more);
    const dialog = await screen.findByRole('dialog', { name: 'More pane details' });
    expect(dialog).toHaveFocus();
    expect(dialog.querySelectorAll('.pane-detail-row')).toHaveLength(2);
    const draft = within(dialog).getByRole('textbox', { name: 'Detail draft' });
    fireEvent.change(draft, { target: { value: 'keep my edit' } });
    act(() => resize()); // Height-only resize: the header width is unchanged.
    expect(draft).toHaveValue('keep my edit');
    expect(dialog).toBeVisible();
    fireEvent.click(screen.getAllByText('Resume')[1]);
    expect(action).toHaveBeenCalledOnce();
    fireEvent.scroll(screen.getByTestId('terminal'));
    expect(dialog).toBeVisible();
    fireEvent.doubleClick(screen.getAllByText('111 MB')[1]);
    expect(zoom).not.toHaveBeenCalled();
    const resume = within(dialog).getByRole('button', { name: 'Resume' });
    resume.focus();
    fireEvent.keyDown(resume, { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(more).toHaveFocus();
    available = 1200;
    resize();
    await waitFor(() => expect(screen.queryByRole('button', { name: /more pane details/ })).toBeNull());
    expect(screen.getByRole('button', { name: 'Resume' })).toBeVisible();
  });
});
