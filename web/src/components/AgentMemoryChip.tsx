import type { PaneInfo } from '../types';
import { confirmDialog } from '../lib/dialog';
import { useAgentMemoryPill, type AgentMemoryPillState } from '../lib/useAgentMemoryPill';
import { Tooltip } from './Tooltip';
import { useIsMobileView } from './useIsMobileView';

// The agent's memory in a pane header. Hidden until it is unusual; then it
// reads "1.8 GB · Restart?" and asks before restarting the agent in place.
// "Always show agent memory" adds a plain readout that does nothing on click:
// a chip that restarted on any click cost a curious user a restart.
export function AgentMemoryPill({ memory, onRestart }: { memory: AgentMemoryPillState; onRestart?: () => void }) {
  const { label, unusual } = memory;
  if (unusual && onRestart) {
    const restart = async () => {
      const ok = await confirmDialog({
        title: 'Restart this agent?',
        message: `It is using ${label} of memory. Restarting it frees that memory. The conversation is kept.`,
        confirmLabel: 'Restart',
      });
      if (ok) onRestart();
    };
    return (
      <Tooltip label={`Agent memory ${label}, more than usual. Restart the agent to free it; the conversation is kept.`}>
        <button
          type="button"
          className="iconbtn pane-mem-chip heavy"
          aria-label={`Agent memory ${label}. Restart?`}
          onClick={(e) => { e.stopPropagation(); void restart(); }}
          onDoubleClick={(e) => e.stopPropagation()}
        >
          {label} · Restart?
        </button>
      </Tooltip>
    );
  }
  return (
    <Tooltip label={`Agent memory ${label}${unusual ? ', more than usual' : ''}. To free it, use Restart pane in the ⋯ menu; the conversation is kept.`}>
      <span
        className={`iconbtn pane-mem-chip readonly${unusual ? ' heavy' : ''}`}
        aria-label={`Agent memory ${label}`}
      >
        {label}
      </span>
    </Tooltip>
  );
}

// The Focus header's pill. The Terminal header (PaneHeader.tsx) renders
// AgentMemoryPill itself, inside its overflow-managed details.
export function AgentMemoryChip({ pane, onRestart }: { pane: PaneInfo; onRestart?: () => void }) {
  const memory = useAgentMemoryPill(pane, useIsMobileView());
  return memory.show ? <AgentMemoryPill memory={memory} onRestart={onRestart} /> : null;
}
