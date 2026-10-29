import { useEffect, useState } from 'react';
import { useStore } from '../store';
import { submitToPane } from '../ptySubmit';
import { api } from '../api';
import { effectivePaneAgent } from '../lib/agentKind';
import { useUnsentInput } from '../lib/unsentInput';

// Send one prompt to many panes at once. Targets concurrent-Claude
// workflows ("run /status in all three worktrees", "say continue to
// every waiting agent") where the alternative today is N tab-switches
// + N pastes.
//
// Scope decisions:
//   * Only running panes are listed; exited ones can't receive input.
//   * Selection state is ephemeral (resets on close). A persisted
//     "broadcast group" feature can be layered on later if the raw
//     pattern proves itself.
//   * "Append Enter" submits after the prompt text; off = insert-only,
//     the user can review and hit Enter on each pane manually.
//   * Terminal Claude/Codex submissions use daemon receipts. Raw insert-only
//     writes keep their existing behavior; failures never fall back and retype.

export function BroadcastDialog({ onClose }: { onClose: () => void }) {
  const panes = useStore((s) => s.panes);
  const senders = useStore((s) => s.senders);
  const workspaces = useStore((s) => s.workspaces);
  const toast = useStore((s) => s.toast);

  const running = Object.values(panes)
    .filter((p) => p.status === 'running')
    .sort((a, b) => (b.lastActivityMs || 0) - (a.lastActivityMs || 0));

  // Map paneId → owning workspace for the subtitle.
  const ownerByPane = new Map<string, string>();
  for (const w of workspaces) {
    if (w.name === '_current') continue;
    for (const id of w.paneIds ?? []) {
      if (!ownerByPane.has(id)) ownerByPane.set(id, w.name);
    }
  }

  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [prompt, setPrompt] = useState('');
  useUnsentInput(prompt);
  const [appendEnter, setAppendEnter] = useState(true);
  const [sending, setSending] = useState(false);

  // Escape to cancel.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);

  const toggle = (id: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  const selectAll = () => setSelected(new Set(running.map((p) => p.id)));
  const clearAll = () => setSelected(new Set());

  const canSend = prompt.length > 0 && selected.size > 0 && !sending;

  const send = async () => {
    if (!canSend) return;
    setSending(true);
    const results = await Promise.allSettled([...selected].map(async (id) => {
      const pane = panes[id];
      const agent = pane && effectivePaneAgent(pane);
      const verified = pane && pane.kind !== 'native' && (agent === 'codex' || agent === 'claude' || pane.hasCodex || pane.hasClaude);
      if (appendEnter && verified) {
        return (await api.submitPaneMessage(id, prompt)).outcome;
      }
      const sender = senders[id];
      if (!sender || !submitToPane(sender, prompt, { appendEnter })) throw new Error('pane could not receive');
      return appendEnter ? 'sent_unconfirmed' : 'inserted';
    }));
    const counts = { submitted: 0, sent_unconfirmed: 0, left_in_composer: 0, inserted: 0, failed: 0 };
    for (const result of results) {
      if (result.status === 'fulfilled') counts[result.value]++;
      else counts.failed++;
    }
    const summary = [
      counts.submitted && `${counts.submitted} submitted`,
      counts.sent_unconfirmed && `${counts.sent_unconfirmed} sent without confirmation`,
      counts.left_in_composer && `${counts.left_in_composer} left unsent`,
      counts.inserted && `${counts.inserted} inserted`,
      counts.failed && `${counts.failed} could not receive`,
    ].filter(Boolean).join(' · ');
    toast(`Broadcast: ${summary}`, { kind: counts.failed || counts.left_in_composer || counts.sent_unconfirmed ? 'warn' : 'info', icon: '📢', ttlMs: 6000 });
    void useStore.getState().refreshPanes();
    onClose();
  };

  return (
    <div className="dialog-backdrop broadcast-scrim" onClick={onClose}>
      <div
        className="dialog broadcast-dialog"
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-label="Broadcast prompt"
      >
        <div className="dialog-header broadcast-header">
          <h2 className="dialog-title">Broadcast prompt</h2>
          <button className="iconbtn" onClick={onClose} title="Close (Esc)">×</button>
        </div>
        <div className="dialog-help broadcast-subtitle">
          Send one prompt to multiple panes at once.
        </div>

        <div className="broadcast-pane-list-header">
          <span>Target panes ({selected.size} / {running.length} selected)</span>
          <div>
            <button type="button" onClick={selectAll} disabled={running.length === 0}>All</button>
            <button type="button" onClick={clearAll} disabled={selected.size === 0}>None</button>
          </div>
        </div>
        <div className="broadcast-pane-list">
          {running.length === 0 && (
            <div className="broadcast-empty">No running panes to broadcast to.</div>
          )}
          {running.map((p) => {
            const title = p.title || p.cmd.join(' ') || p.id.slice(-6);
            const ws = ownerByPane.get(p.id) ?? 'background';
            const checked = selected.has(p.id);
            return (
              <label key={p.id} className={`broadcast-pane-row ${checked ? 'checked' : ''}`}>
                <input type="checkbox" checked={checked} onChange={() => toggle(p.id)} />
                <span className="broadcast-pane-title">{title}</span>
                <span className="broadcast-pane-ws">{ws}</span>
              </label>
            );
          })}
        </div>

        <label className="broadcast-prompt-label" htmlFor="broadcast-prompt">Prompt</label>
        <textarea
          id="broadcast-prompt"
          className="broadcast-prompt"
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
          placeholder="Type the text to send to every selected pane…"
          rows={4}
          autoFocus
        />

        <div className="dialog-footer broadcast-footer">
          <label className="broadcast-enter-toggle">
            <input
              type="checkbox"
              checked={appendEnter}
              onChange={(e) => setAppendEnter(e.target.checked)}
            />
            Submit with Enter
          </label>
          <div className="dialog-actions broadcast-actions">
            <button type="button" onClick={onClose}>Cancel</button>
            <button
              type="button"
              className="primary"
              disabled={!canSend}
              onClick={send}
            >
              {sending ? 'Sending…' : `Send to ${selected.size}`}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
