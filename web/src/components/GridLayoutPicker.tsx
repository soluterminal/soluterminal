import { useState } from 'react';
import { useStore } from '../store';
import { collectLeaves } from '../lib/layout';
import { workspaceViewLayout } from '../lib/workspaceView';
import { TopbarPopover } from './TopbarPopover';
import { IconGrid } from './ui/icons';

// Excel-style hover-grid picker for the bottom-left toolbar. User
// hovers over a 5×5 grid; cells whose total < current pane count are
// disabled (we never destroy panes from this control). Click commits:
// equal count = pure rearrange, greater = spawn shell panes for the
// delta, then reflow column-major into rows×cols.
//
// The grid is column-major (matches buildNxMFromLeaves in store.ts):
// hovering the (r=2, c=3) cell selects 2 rows × 3 columns. The caption
// underneath shows the delta so the user knows what they're committing
// to before the click.

const MAX_DIM = 5;

export function GridLayoutPicker() {
  const applyGridLayout = useStore((s) => s.applyGridLayout);
  // Count the tiles the grid shows, as applyGridLayout does: in the overview
  // hidden workers keep their saved place and are not part of the grid.
  const currentLeafCount = useStore((s) => collectLeaves(workspaceViewLayout(s)).length);

  const [hoverR, setHoverR] = useState(0);
  const [hoverC, setHoverC] = useState(0);
  const [busy, setBusy] = useState(false);

  return (
    <TopbarPopover
      align="right"
      minWidth={200}
      trigger={({ toggle, open }) => (
        <button
          type="button"
          className="chrome-btn"
          onClick={toggle}
          title="Open multiple panes (grid)"
          aria-label="Open multiple panes"
          aria-expanded={open}
        >
          <IconGrid />
        </button>
      )}
    >
      <GridPickerBody
        currentLeafCount={currentLeafCount}
        hoverR={hoverR}
        hoverC={hoverC}
        busy={busy}
        onHover={(r, c) => { setHoverR(r); setHoverC(c); }}
        onLeave={() => { setHoverR(0); setHoverC(0); }}
        onPick={async (r, c) => {
          if (busy) return;
          if (r * c < currentLeafCount) return;
          setBusy(true);
          try {
            await applyGridLayout(r, c);
          } finally {
            setBusy(false);
            setHoverR(0);
            setHoverC(0);
          }
        }}
      />
    </TopbarPopover>
  );
}

function GridPickerBody({
  currentLeafCount,
  hoverR,
  hoverC,
  busy,
  onHover,
  onLeave,
  onPick,
}: {
  currentLeafCount: number;
  hoverR: number;
  hoverC: number;
  busy: boolean;
  onHover: (r: number, c: number) => void;
  onLeave: () => void;
  onPick: (r: number, c: number) => void;
}) {
  const selected = hoverR * hoverC;
  const delta = selected - currentLeafCount;
  // Caption: lead with the dimensions the user is about to commit so
  // they don't have to count cells. "rearrange" wording matches the
  // language we used during design (delta = 0 = pure layout reflow).
  let caption: string;
  if (selected === 0) {
    caption = currentLeafCount === 0
      ? 'Hover to pick a grid'
      : `${currentLeafCount} pane${currentLeafCount === 1 ? '' : 's'} now — hover to add or rearrange`;
  } else if (delta === 0) {
    caption = `${hoverR}×${hoverC} — rearrange (no new panes)`;
  } else if (delta > 0) {
    caption = `${hoverR}×${hoverC} — +${delta} pane${delta === 1 ? '' : 's'}`;
  } else {
    // Cell is disabled; this branch shouldn't fire from clicks but the
    // hover may still hit a disabled cell on its way to a valid one.
    caption = `${hoverR}×${hoverC} — too small (need ≥ ${currentLeafCount})`;
  }

  return (
    <div className="grid-picker">
      <div
        className="grid-picker-grid"
        onMouseLeave={onLeave}
        role="grid"
      >
        {Array.from({ length: MAX_DIM }).map((_, rIdx) =>
          Array.from({ length: MAX_DIM }).map((__, cIdx) => {
            const r = rIdx + 1;
            const c = cIdx + 1;
            const total = r * c;
            const disabled = total < currentLeafCount;
            const active = !disabled && r <= hoverR && c <= hoverC;
            return (
              <button
                key={`${r}-${c}`}
                type="button"
                role="gridcell"
                className={
                  'grid-picker-cell'
                  + (active ? ' active' : '')
                  + (disabled ? ' disabled' : '')
                }
                disabled={disabled || busy}
                onMouseEnter={() => { if (!disabled) onHover(r, c); }}
                onFocus={() => { if (!disabled) onHover(r, c); }}
                onClick={() => onPick(r, c)}
                aria-label={`${r} by ${c}`}
                title={disabled ? `Need at least ${currentLeafCount} cells` : `${r}×${c}`}
              />
            );
          }),
        )}
      </div>
      <div className="grid-picker-caption">
        {busy ? 'Spawning panes…' : caption}
      </div>
    </div>
  );
}
