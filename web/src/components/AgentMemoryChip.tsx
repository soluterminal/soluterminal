import type { PaneInfo } from '../types';
import { paneAgentRssMB } from '../lib/agentMemory';
import { Tooltip } from './Tooltip';

function memoryLabel(mb: number) {
  return mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb} MB`;
}

// Display-only, like the Terminal header's readout (PaneHeader.tsx): a chip
// that restarted the agent on click looked like any other status chip, so a
// curious click cost a restart. Reclaiming stays in the pane menu.
export function AgentMemoryChip({ pane }: { pane: PaneInfo }) {
  const mb = paneAgentRssMB(pane);
  if (!(mb > 0)) return null;

  const label = memoryLabel(mb);
  const heavy = mb >= 500;
  return (
    <Tooltip label={`Agent memory ${label}${heavy ? ' — heavy' : ''}. To reclaim it, use Restart pane in the ⋯ menu; your conversation is kept.`}>
      <span
        className={`iconbtn pane-mem-chip readonly${heavy ? ' heavy' : ''}`}
        aria-label={`Agent memory ${label}`}
      >
        {label}
      </span>
    </Tooltip>
  );
}
