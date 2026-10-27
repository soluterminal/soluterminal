import type { PaneInfo } from '../types';
import { useStore } from '../store';
import { Tooltip } from './Tooltip';
import { formatAgo, formatDuration } from '../lib/turnTime';
import { turnChipAriaLabel, turnChipModel } from '../lib/turnChip';
import { useTurnClock } from '../lib/useTurnClock';

// Pane-header readout of the agent's last finished turn (daemon/turns.go):
//   "Δ 4 files · +120 −30 · 2m ago"   (click: that turn's diff)
//   "done 2m ago"                      (finished, no file changes)
// For agent panes it is ALSO the unread signal: while the turn finished after
// you last looked at the pane (`unseen`), the chip is highlighted with a
// "new" tag — instead of a separate dot next to the title. Hidden while a
// turn runs and once the last turn is a few hours old.
export function PaneTurnChip({ pane, unseen }: { pane: PaneInfo; unseen: boolean }) {
  const openTurnDiff = useStore((s) => s.openTurnDiff);
  const now = useTurnClock(pane.turn?.last?.endedMs);

  const model = turnChipModel(pane, now, unseen);
  if (!model) return null;

  const t = model.turn;
  const outcomeLabel = t.outcome === 'interrupted' || t.outcome === 'failed' ? `${t.outcome} · ` : '';
  const lines = [
    `Turn ${t.n} · ${new Date(t.endedMs).toLocaleString()} · elapsed ${formatDuration(t.endedMs - t.startedMs)} · finished ${formatAgo(t.endedMs, now)}`,
  ];
  if (t.files > 0) lines.push(`${t.files} file${t.files > 1 ? 's' : ''} changed (+${t.added} −${t.removed}) — click to review`);
  else if (t.note) lines.push(`No diff: ${t.note}`);
  else if (t.skippedUntracked) lines.push('No observed changes; capture incomplete');
  else lines.push('No changes observed between snapshots');
  if (t.overlapPanes?.length) lines.push(`Also working in this repo: ${t.overlapPanes.join(', ')}`);
  if (unseen) lines.push('New — finished while you were looking elsewhere');

  const cls = `pane-turn-chip ${model.kind}${unseen ? ' unseen' : ''}${t.overlapPanes?.length ? ' overlap' : ''}`;
  const newTag = unseen ? <span className="pane-turn-new">new</span> : null;

  if (model.kind === 'changed') {
    return (
      <Tooltip label={lines.join('\n')}>
        <button
          type="button"
          className={cls}
          aria-label={turnChipAriaLabel(t.files, unseen)}
          onClick={(e) => {
            e.stopPropagation();
            openTurnDiff(pane.id, t.n);
          }}
          onDoubleClick={(e) => e.stopPropagation()}
        >
          {newTag}{outcomeLabel}Δ {t.files} file{t.files === 1 ? '' : 's'} · <span className="diff-add">+{t.added}</span> <span className="diff-del">−{t.removed}</span> · {formatAgo(t.endedMs, now)}
        </button>
      </Tooltip>
    );
  }
  return (
    <Tooltip label={lines.join('\n')}>
      <span className={cls}>{newTag}{model.kind} {formatAgo(t.endedMs, now)}</span>
    </Tooltip>
  );
}
