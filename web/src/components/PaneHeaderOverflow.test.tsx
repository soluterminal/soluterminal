import { useRef } from 'react';
import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { DETAIL_PRIORITY, MEASURE_SENTINEL, MORE_RESERVE, PaneDetail, PaneHeaderOverflow, chooseCollapsed, type DetailMetric } from './PaneHeaderOverflow';
import { titleShowsHandle, messageSharesOrigin } from '../lib/paneHeaderDetails';
import { PaneIdentityChip } from './PaneIdentityChip';
import type { PaneInfo } from '../types';

const pane: PaneInfo = { id: 'worker', title: '@api · Fix login', identity: { handle: 'api', workingOn: 'Fix login' }, cmd: ['bash'], cwd: '/', status: 'running', createdAt: '', pid: 1, cols: 80, rows: 24, lastActivityMs: 0, state: 'busy', hasClaude: false };

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('pane header details', () => {
  it('ranks details in the order the product owner approved', () => {
    const order = Object.entries(DETAIL_PRIORITY).sort((a, b) => a[1] - b[1]).map(([name]) => name);
    expect(order).toEqual(['autoMode', 'role', 'credential', 'schedule', 'review', 'message', 'identity', 'origin', 'mode', 'pack', 'resume', 'memory', 'maximise', 'workerNotice']);
    expect(new Set(Object.values(DETAIL_PRIORITY)).size).toBe(order.length);
  });

  describe('chooseCollapsed', () => {
    // Deliberately shuffled: DOM order must not decide who collapses.
    const items: DetailMetric[] = [
      { id: 'resume', priority: 11, width: 60 }, { id: 'auto', priority: 1, width: 70 }, { id: 'maximise', priority: 13, width: 30 },
      { id: 'credential', priority: 3, width: 80 }, { id: 'identity', priority: 7, width: 90 },
    ];
    const total = 60 + 70 + 30 + 80 + 90;
    const fixed = 200;
    it('keeps everything while it fits and drops nothing for a header with no details', () => {
      expect(chooseCollapsed(fixed + total, fixed, items)).toEqual({ hidden: [], wrap: false });
      expect(chooseCollapsed(fixed + total - 1, fixed, items).hidden).toEqual([]); // the one pixel of tolerance
      expect(chooseCollapsed(100, 200, [])).toEqual({ hidden: [], wrap: true });
    });
    it('moves the least important detail first, one at a time, listing the hidden ones most important first', () => {
      const priorityOrder = ['auto', 'credential', 'identity', 'resume', 'maximise'];
      const hiddenAt = (available: number, prev = 0) => chooseCollapsed(available, fixed, items, prev).hidden;
      // Hiding the 30px maximise button alone cannot pay for the +N button, so resume goes with it.
      expect(hiddenAt(fixed + total - 2)).toEqual(['resume', 'maximise']);
      // Walking the width down: the hidden set is always the lowest-priority tail, growing by one.
      let prev: string[] = [];
      const sizes: number[] = [];
      for (let width = fixed + total + 10; width >= 0; width--) {
        const hidden = hiddenAt(width, prev.length);
        expect(hidden).toEqual(priorityOrder.slice(priorityOrder.length - hidden.length));
        expect(hidden.length).toBeGreaterThanOrEqual(prev.length);
        if (hidden.length < priorityOrder.length) expect(fixed + items.filter(i => !hidden.includes(i.id)).reduce((n, i) => n + i.width, 0) + (hidden.length ? MORE_RESERVE : 0)).toBeLessThanOrEqual(width + 1);
        if (hidden.length !== prev.length) sizes.push(hidden.length);
        prev = hidden;
      }
      expect(sizes[0]).toBeGreaterThan(0);
      expect(sizes.at(-1)).toBe(5);
      expect(sizes.every((n, i) => i === 0 || n > sizes[i - 1])).toBe(true);
    });
    it('only wraps the attention group when no detail is left to give up', () => {
      expect(chooseCollapsed(fixed + MORE_RESERVE, fixed, items)).toEqual({ hidden: ['auto', 'credential', 'identity', 'resume', 'maximise'], wrap: false });
      expect(chooseCollapsed(fixed - 1, fixed, items).wrap).toBe(true);
    });
    it('does not oscillate around a boundary: coming back needs extra room', () => {
      const edge = fixed + total; // everything fits exactly here
      let hidden = chooseCollapsed(edge - 40, fixed, items).hidden.length;
      expect(hidden).toBeGreaterThan(0);
      // Jitter of a few pixels around the point where the last item would fit again must not toggle.
      for (const width of [edge - 4, edge - 2, edge, edge - 3, edge - 1, edge]) {
        const next = chooseCollapsed(width, fixed, items, hidden).hidden.length;
        expect(next).toBe(hidden);
      }
      // A clear margin brings it back.
      expect(chooseCollapsed(edge + 8, fixed, items, hidden).hidden).toEqual([]);
    });
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

  describe('in the header', () => {
    let resize = () => {};
    let available = 1200;
    let scrollReads = 0;
    // jsdom has no layout: chips declare their width in data-w and the header
    // reports the same overflow arithmetic the browser does (sentinel included).
    function mockLayout() {
      vi.useFakeTimers();
      vi.stubGlobal('ResizeObserver', class { constructor(cb: () => void) { resize = cb; } observe() {} disconnect() {} });
      vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockImplementation(() => available);
      vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockImplementation(function (this: HTMLElement) { return Number(this.dataset.w ?? 0); });
      vi.spyOn(HTMLElement.prototype, 'scrollWidth', 'get').mockImplementation(function (this: HTMLElement) {
        scrollReads++;
        return MEASURE_SENTINEL + [...this.querySelectorAll<HTMLElement>('[data-w]')].reduce((sum, el) => sum + Number(el.dataset.w), 0);
      });
    }
    const settle = () => act(() => { vi.advanceTimersByTime(150); });
    const setWidth = (width: number) => { available = width; act(() => resize()); settle(); };
    const hiddenNames = (root: ParentNode = document) => [...root.querySelectorAll<HTMLElement>('[data-pane-detail][hidden]')].map(el => el.textContent).sort();
    const rowNames = () => [...screen.getByRole('dialog', { name: 'More pane details' }).querySelectorAll('.pane-detail-row')].map(row => row.textContent);
    afterEach(() => { vi.useRealTimers(); available = 1200; scrollReads = 0; });

    // 100px title + 40px actions + attention 120px => 260px never collapses.
    // Details (deliberately out of DOM order): maximise 30, auto 70, credential 80, resume 60.
    function PriorityHeader() {
      const ref = useRef<HTMLDivElement>(null);
      return <div ref={ref}><PaneHeaderOverflow headerRef={ref}>
        <span data-w="100">Title</span>
        <PaneDetail priority={DETAIL_PRIORITY.maximise}><button data-w="30">Maximise</button></PaneDetail>
        <PaneDetail priority={DETAIL_PRIORITY.resume}><button data-w="60">Resume</button></PaneDetail>
        <div className="pane-header-attention"><span data-w="120">needs you</span></div>
        <PaneDetail priority={DETAIL_PRIORITY.credential}><span data-w="80">Credential</span></PaneDetail>
        <PaneDetail priority={DETAIL_PRIORITY.autoMode}><span data-w="70">Auto</span></PaneDetail>
        <span data-w="40">Menu</span>
      </PaneHeaderOverflow></div>;
    }
    const moreButton = () => screen.queryByRole('button', { name: /more pane details/ });

    it('hides details in priority order as the header narrows, with an accurate count and popover', () => {
      mockLayout();
      render(<PriorityHeader />);
      expect(moreButton()).toBeNull();
      // 260 fixed + 240 details = 500 fits exactly.
      setWidth(500);
      expect(moreButton()).toBeNull();
      const steps: [number, string[]][] = [
        [498, ['Maximise', 'Resume']],             // Maximise alone (30) cannot pay for +N (48)
        [460, ['Maximise', 'Resume']],             // 260 + Auto 70 + Credential 80 + 48 = 458
        [440, ['Credential', 'Maximise', 'Resume']],
        [400, ['Credential', 'Maximise', 'Resume']],
        [330, ['Auto', 'Credential', 'Maximise', 'Resume']],
      ];
      for (const [width, names] of steps) {
        setWidth(width);
        expect(hiddenNames()).toEqual(names);
        expect(moreButton()).toHaveAccessibleName(`${names.length} more pane details`);
        expect(moreButton()).toHaveTextContent(`+${names.length}`);
      }
      // The popover lists exactly the hidden ones, most important first.
      fireEvent.click(moreButton()!);
      expect(rowNames()).toEqual(['Auto', 'Credential', 'Resume', 'Maximise']);
      fireEvent.keyDown(within(screen.getByRole('dialog')).getByRole('button', { name: 'Resume' }), { key: 'Escape' });
      // Widening brings them back in reverse: the most important one first.
      setWidth(470);
      expect(hiddenNames()).toEqual(['Maximise', 'Resume']);
      setWidth(1200);
      expect(moreButton()).toBeNull();
      expect(hiddenNames()).toEqual([]);
    });

    it('collapses on the resize itself, not after the debounce, so actions never overflow a painted frame', () => {
      mockLayout();
      render(<PriorityHeader />);
      available = 330;
      act(() => resize()); // no timers advanced: the ResizeObserver callback runs before paint
      expect(hiddenNames()).toEqual(['Auto', 'Credential', 'Maximise', 'Resume']);
      expect(moreButton()).toHaveTextContent('+4');
      // Widening restores them in the same synchronous pass.
      available = 1200;
      act(() => resize());
      expect(moreButton()).toBeNull();
    });

    it('never collapses the attention group, even with every detail hidden', () => {
      mockLayout();
      render(<PriorityHeader />);
      setWidth(120);
      expect(moreButton()).toHaveAccessibleName('4 more pane details');
      expect(hiddenNames()).toHaveLength(4);
      expect(screen.getByText('needs you').closest('[hidden]')).toBeNull();
      expect(screen.getByText('Title')).toBeVisible();
      expect(screen.getByText('Menu')).toBeVisible();
    });

    it('does not oscillate at a boundary width', () => {
      mockLayout();
      render(<PriorityHeader />);
      setWidth(430);
      const settled = hiddenNames();
      expect(settled.length).toBeGreaterThan(0);
      const states = new Set<string>();
      // Re-measure the same width many times, then wobble by a few pixels.
      for (const width of [430, 430, 431, 429, 432, 428, 430, 433, 430, 434, 430]) { setWidth(width); states.add(hiddenNames().join()); }
      expect(states.size).toBe(1);
      expect(scrollReads).toBeLessThan(40);
    });

    it('measures nothing and keeps all details inline when disabled (phones)', () => {
      mockLayout();
      available = 100;
      function Phone() {
        const ref = useRef<HTMLDivElement>(null);
        return <div ref={ref}><PaneHeaderOverflow headerRef={ref} enabled={false}><PaneDetail priority={1}><span data-w="300">Wide</span></PaneDetail></PaneHeaderOverflow></div>;
      }
      render(<Phone />);
      expect(scrollReads).toBe(0);
      expect(moreButton()).toBeNull();
      expect(screen.getByText('Wide')).toBeVisible();
    });

    it('counts actual chips, keeps actions usable, restores focus on Escape, and unfolds after resize', () => {
      mockLayout();
      available = 200;
      const action = vi.fn();
      const zoom = vi.fn();
      function Header() {
        const ref = useRef<HTMLDivElement>(null);
        return <div ref={ref} onDoubleClick={zoom}><div data-testid="terminal" /><PaneHeaderOverflow headerRef={ref}>
          <span data-w="100">Title</span><span className="pane-autoapproving-chip" data-testid="countdown"> approving in 60s</span><PaneDetail priority={1}><button data-w="80" onClick={action}>Resume</button></PaneDetail>
          <PaneDetail priority={2}>{null}</PaneDetail><PaneDetail priority={3}><span data-w="80">111 MB<input aria-label="Detail draft" defaultValue="draft" /></span></PaneDetail>
        </PaneHeaderOverflow></div>;
      }
      render(<Header />);
      const more = screen.getByRole('button', { name: '2 more pane details' });
      settle();
      const reads = scrollReads;
      act(() => { screen.getByTestId('countdown').firstChild!.nodeValue = ' approving in 59s'; });
      settle();
      expect(scrollReads).toBe(reads);
      fireEvent.click(more);
      const dialog = screen.getByRole('dialog', { name: 'More pane details' });
      expect(dialog).toHaveFocus();
      expect(dialog.querySelectorAll('.pane-detail-row')).toHaveLength(2);
      const draft = within(dialog).getByRole('textbox', { name: 'Detail draft' });
      fireEvent.change(draft, { target: { value: 'keep my edit' } });
      act(() => resize()); // Height-only resize: the header width is unchanged.
      expect(draft).toHaveValue('keep my edit');
      expect(dialog).toBeVisible();
      fireEvent.scroll(screen.getByTestId('terminal'));
      expect(dialog).toBeVisible();
      fireEvent.doubleClick(screen.getAllByText('111 MB')[1]);
      expect(zoom).not.toHaveBeenCalled();
      fireEvent.click(screen.getByRole('button', { name: 'Resume' }));
      expect(action).toHaveBeenCalledOnce();
      fireEvent.keyDown(draft, { key: 'Escape' });
      expect(screen.queryByRole('dialog')).toBeNull();
      expect(more).toHaveFocus();
      setWidth(1200);
      expect(screen.queryByRole('button', { name: /more pane details/ })).toBeNull();
      expect(screen.getByRole('button', { name: 'Resume' })).toBeVisible();
    });
  });
});
