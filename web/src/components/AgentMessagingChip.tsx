import { useEffect, useState } from 'react';
import { api } from '../api';
import { useStore } from '../store';
import { refreshDaemonHealth } from '../lib/useDaemonHealth';
import { Tooltip } from './Tooltip';

// The person's one-click stop for agent-to-agent messaging
// (daemon/agent_messaging_pause.go). While paused, agents' messages to each
// other wait on each pane with the sender and the reason; spawning panes and
// answering other panes' prompts are refused with a notice. Resume delivers
// the held messages in order. Only the person can turn it on or off.
//
// It shows once two panes run (one agent cannot message another), and always
// while paused, so a pause is never out of sight. Renders its own leading
// separator.

export const AGENT_MESSAGING_COPY = {
  on: 'Agent messages on',
  paused: 'Agent messages paused',
  onTitle: 'Agents can message each other and start panes. Click to pause: their messages then wait on each pane until you resume.',
  // Short label suffixes; the full warning is in the tooltip and a toast.
  unsavedSuffix: 'not saved',
  unreadableSuffix: 'setting reset',
  dismiss: 'Dismiss',
  dismissFailed: "Couldn't save the setting. Try again.",
  dismissTitle: 'Keep agent messages on and save the setting. Automations that agents set up, held while the setting couldn\'t be read, then run.',
  pausedTitle: (held: number) =>
    `Paused: agents' messages to each other wait on each pane${held > 0 ? ` (${held} now)` : ''}, and agents can't start panes or answer other panes' prompts. Click to resume and deliver held messages in order.`,
} as const;

// The version popover's line while an update waits on the pause.
export const AGENT_MESSAGING_UPDATE_WAIT = '⏳ Waiting: agent messaging is paused with held messages. Resume, or send or discard them, to update';

export function heldMessageCount(panes: Record<string, { heldMessages?: unknown[] }>): number {
  let n = 0;
  for (const id in panes) n += panes[id].heldMessages?.length ?? 0;
  return n;
}

// The PUT's answer shows until health reports anything newer, at most this
// long (longer than the 10 s health poll).
const OVERRIDE_MS = 15_000;

// Each warning is toasted once per page load; the chip keeps showing it.
const toastedWarnings = new Set<string>();

type WarningKind = 'unreadable' | 'unsaved';

export function AgentMessagingChip({ paused: healthPaused, warning: healthWarning, warningKind: healthWarningKind }: {
  paused: boolean | undefined;
  warning?: string;
  warningKind?: WarningKind;
}) {
  // Primitive selectors: a fresh object here would re-render on every store change.
  const held = useStore((s) => heldMessageCount(s.panes));
  const running = useStore((s) => {
    let n = 0;
    for (const id in s.panes) if (s.panes[id].status === 'running') n++;
    return n;
  });
  // The PUT's answer until the next health poll agrees with it.
  const [override, setOverride] = useState<{ paused: boolean; warning: string; warningKind?: WarningKind } | null>(null);
  const [pending, setPending] = useState(false);
  // saveFailed: a Dismiss the daemon answered but could not save (it keeps
  // the unreadable notice); other errors are a refused or failed request.
  const [error, setError] = useState<{ message: string; dismiss: boolean; saveFailed?: boolean } | null>(null);
  // Any change in what health reports is newer than the PUT (another
  // device may have toggled it too); and the answer never outlives a poll.
  useEffect(() => { setOverride(null); }, [healthPaused, healthWarning]);
  useEffect(() => {
    if (override === null) return;
    const timer = setTimeout(() => setOverride(null), OVERRIDE_MS);
    return () => clearTimeout(timer);
  }, [override]);

  const paused = override ? override.paused : healthPaused ?? false;
  const warning = override ? override.warning : healthWarning ?? '';
  const warningKind = override ? override.warningKind : healthWarningKind;
  // A stop never fails silently: a setting that couldn't be read or saved is
  // shown, even while the chip itself would be hidden.
  useEffect(() => {
    if (!warning || toastedWarnings.has(warning)) return;
    toastedWarnings.add(warning);
    useStore.getState().toast(warning, { kind: 'warn' });
  }, [warning]);
  if (!paused && running < 2 && !warning) return null;

  // dismiss answers the unreadable-setting notice: the state stays as it is
  // (on), the daemon saves it and releases what it kept held at startup.
  const send = async (nextPaused: boolean, dismiss = false) => {
    if (pending) return;
    setPending(true);
    setError(null);
    try {
      const next = await api.setAgentMessagingPaused(nextPaused);
      setOverride({ paused: next.paused, warning: next.warning ?? '', warningKind: next.warningKind });
      // A Dismiss whose save failed comes back 200 with the notice kept.
      if (dismiss && next.warningKind === 'unreadable') setError({ message: '', dismiss, saveFailed: true });
      refreshDaemonHealth();
      void useStore.getState().refreshPanes();
    } catch (err) {
      setError({ message: err instanceof Error ? err.message : String(err), dismiss });
    } finally {
      setPending(false);
    }
  };

  const label = (paused
    ? `${AGENT_MESSAGING_COPY.paused}${held > 0 ? ` · ${held} held` : ''}`
    : AGENT_MESSAGING_COPY.on)
    + (warning ? ` · ${warningKind === 'unreadable' ? AGENT_MESSAGING_COPY.unreadableSuffix : AGENT_MESSAGING_COPY.unsavedSuffix}` : '');
  const title = error
    ? error.saveFailed
      ? AGENT_MESSAGING_COPY.dismissFailed
      : `Couldn't ${error.dismiss ? 'dismiss' : paused ? 'resume' : 'pause'}: ${error.message}`
    : `${warning ? `${warning} ` : ''}${paused ? AGENT_MESSAGING_COPY.pausedTitle(held) : AGENT_MESSAGING_COPY.onTitle}`;
  return (
    <>
      <span className="statusbar-sep">·</span>
      <Tooltip label={title} side="top">
        <button
          type="button"
          className={`statusbar-agent-msgs${paused ? ' paused' : ''}${warning ? ' warn' : ''}${error ? ' error' : ''}`}
          aria-label={title}
          aria-pressed={paused}
          disabled={pending}
          onClick={() => void send(!paused)}
          data-tauri-drag-region={false}
        >
          <span aria-hidden className="statusbar-agent-msgs-dot" />
          <span role="status" aria-live="polite">{label}</span>
        </button>
      </Tooltip>
      {warningKind === 'unreadable' && !paused && (
        <Tooltip label={AGENT_MESSAGING_COPY.dismissTitle} side="top">
          <button
            type="button"
            className="statusbar-agent-msgs-dismiss"
            aria-label={`${AGENT_MESSAGING_COPY.dismiss}: ${AGENT_MESSAGING_COPY.dismissTitle}`}
            disabled={pending}
            onClick={() => void send(false, true)}
            data-tauri-drag-region={false}
          >
            {AGENT_MESSAGING_COPY.dismiss}
          </button>
        </Tooltip>
      )}
    </>
  );
}
