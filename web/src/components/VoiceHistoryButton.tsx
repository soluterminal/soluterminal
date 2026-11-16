import { useStore } from '../store';
import type { VoiceTranscription } from '../private/voice.slice';
import type { VoiceCleanedBy } from '../private/voice.api';
import { confirmDialog } from '../lib/dialog';

// Voice-history — the last N transcriptions, so a user whose text went to the
// wrong pane (or was lost to a killed pane) can recover it via copy or re-insert.
//
// Rendered INLINE inside the voice options popover. Previously this was a nested
// popover-in-popover (footer ⋮ → 🕘 opened its own panel); lifting it inline
// removes that nesting so History is one click from the mic's options caret.
//
// Privacy: the list lives only in this browser's localStorage — never
// transmitted.

export function VoiceHistoryInline() {
  const history = useStore((s) => s.voiceHistory);
  const clear = useStore((s) => s.clearVoiceHistory);
  const reinsert = useStore((s) => s.reinsertVoiceHistory);
  const toast = useStore((s) => s.toast);
  const focusedPaneId = useStore((s) => s.focusedPaneId);

  async function onCopy(text: string) {
    try {
      await navigator.clipboard?.writeText?.(text);
      toast('Transcript copied to clipboard', { kind: 'success', ttlMs: 2500 });
    } catch {
      toast('Clipboard unavailable — select the text manually', { kind: 'warn' });
    }
  }

  function onReinsert(id: number, close: () => void) {
    if (!focusedPaneId) {
      toast('Focus a terminal pane first', { kind: 'warn' });
      return;
    }
    const ok = reinsert(id);
    if (ok) {
      close();
    } else {
      toast('Could not send to focused pane (no open WebSocket)', { kind: 'warn' });
    }
  }

  return (
    <VoiceHistoryPanel
      history={history}
      onCopy={onCopy}
      onReinsert={onReinsert}
      onClear={async () => {
        if (await confirmDialog({
          message: 'Clear voice-dictation history on this device?',
          confirmLabel: 'Clear',
          danger: true,
        })) {
          clear();
        }
      }}
    />
  );
}

// Taking an explicit `close` callback via onReinsert keeps the "close-after-
// insert" behavior intact; the enclosing TopbarPopover closes on outside-click,
// so we approximate an explicit close by dispatching a body click it catches.
function VoiceHistoryPanel({
  history,
  onCopy,
  onReinsert,
  onClear,
}: {
  history: VoiceTranscription[];
  onCopy: (text: string) => void;
  onReinsert: (id: number, close: () => void) => void;
  onClear: () => void;
}) {
  const closeFromInside = () => {
    // Must match the event TopbarPopover's outside-click detector listens on
    // (`click`). A synthetic click on body — outside both rootRef and panelRef —
    // triggers the popover's contains() check → "not in panel" → close.
    const ev = new MouseEvent('click', { bubbles: true, cancelable: true });
    document.body.dispatchEvent(ev);
  };
  return (
    <div className="voice-history-panel inline" role="group" aria-label="Voice history">
      <div className="voice-history-header">
        <span>Recent dictations</span>
        {history.length > 0 && (
          <button
            type="button"
            className="voice-history-clear"
            onClick={() => {
              onClear();
              closeFromInside();
            }}
            title="Wipe all saved transcriptions from this device"
          >
            Clear
          </button>
        )}
      </div>
      {history.length === 0 ? (
        <div className="voice-history-empty">
          Your last 20 voice transcriptions will appear here.
          Stored only on this device.
        </div>
      ) : (
        <ul className="voice-history-list">
          {history.map((entry) => (
            <li key={entry.id} className={`voice-history-row ${entry.delivered ? '' : 'undelivered'}`}>
              <div className="voice-history-meta">
                <span className="voice-history-when" title={new Date(entry.id).toLocaleString()}>
                  {formatAgo(entry.id)}
                </span>
                {entry.paneTitle && (
                  <span className="voice-history-pane" title={`Went to pane: ${entry.paneTitle}`}>
                    → {entry.paneTitle}
                  </span>
                )}
                {historyStatusLabel(entry) && (
                  <span className="voice-history-by">{historyStatusLabel(entry)}</span>
                )}
                {slowStartLabel(entry) && (
                  <span
                    className="voice-history-badge"
                    title="Words spoken before the mic started may be missing. Start speaking at the tick, or turn on Keep the microphone ready in Settings."
                  >
                    {slowStartLabel(entry)}
                  </span>
                )}
                {!entry.delivered && (
                  <span className="voice-history-badge" title="Target pane closed before delivery — text was not sent anywhere">
                    not delivered
                  </span>
                )}
              </div>
              <div className="voice-history-text" title={entry.text}>{entry.text}</div>
              {entry.raw && (
                <div className="voice-history-raw" title="What the speech recogniser heard, before cleanup">
                  Heard: {entry.raw}
                </div>
              )}
              {entry.sessionId && <details>
                <summary>Dictation details{entry.partial ? ' · partial' : ''}</summary>
                <p>{Math.round((entry.durationMs ?? 0) / 1000)} seconds · {entry.completedSegments ?? 0}/{entry.totalSegments ?? 0} parts · {entry.stopReason ?? 'unavailable'}</p>
                <p>Requested language: {entry.requestedLanguage || 'unavailable'} · Detected language: {entry.detectedLanguage || 'unavailable'}</p>
                <p>Language differences are diagnostic evidence; they do not establish why recognition or cleanup changed words.</p>
                <ul>{entry.segments?.map(segment => <li key={segment.index}>Part {segment.index + 1}: {segment.transcriptionProvider || 'provider unavailable'} · cleanup {segment.cleanup || 'unavailable'} ({segment.cleanupProvider || 'provider unavailable'}) · detected {segment.detectedLanguage || 'unavailable'}</li>)}</ul>
              </details>}
              <div className="voice-history-actions">
                {entry.raw && <button type="button" onClick={() => onCopy(entry.raw!)}>Copy heard</button>}
                <button
                  type="button"
                  onClick={() => onCopy(entry.text)}
                  title="Copy to clipboard"
                >
                  Copy
                </button>
                <button
                  type="button"
                  onClick={() => onReinsert(entry.id, closeFromInside)}
                  title="Send to the currently-focused pane"
                >
                  Insert
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// Whose login or service cleaned it (ST-181 wording, @cmo 14 Nov). History
// from before the API-key choice existed says 'local' only for the plan.
const CLEANED_WITH: Record<VoiceCleanedBy, string> = {
  local: 'with your Claude plan',
  api_key: 'with your API key',
  cloud: 'by SoluTerminal',
};

export function cleanedByLabel(by: VoiceCleanedBy): string {
  return `cleaned ${CLEANED_WITH[by] ?? ''}`.trim();
}

/** One short line saying what happened to the cleanup pass. */
export function historyStatusLabel(e: VoiceTranscription): string | null {
  const where = e.cleanedBy && CLEANED_WITH[e.cleanedBy] ? ` ${CLEANED_WITH[e.cleanedBy]}` : '';
  switch (e.polish) {
    case 'cleaned': return e.cleanedBy ? cleanedByLabel(e.cleanedBy) : 'cleaned';
    case 'unchanged': return `checked${where}, nothing to fix`;
    case 'short': return 'too short to clean';
    case 'kept-raw': return 'you typed first, kept as heard';
    case 'failed': return 'cleanup failed, kept as heard';
    case 'off': return 'not cleaned';
    default: return e.cleanedBy ? cleanedByLabel(e.cleanedBy) : null;
  }
}

/** Flags a recording whose mic took long enough to start to cut a word. */
export function slowStartLabel(e: VoiceTranscription): string | null {
  if (e.warmMic || e.startDelayMs === undefined || e.startDelayMs < 250) return null;
  return `mic took ${(e.startDelayMs / 1000).toFixed(1)} s to start`;
}

// Compact relative-time string. Good enough for a dropdown where we
// don't want to drag in a whole date-fns dep just to render "2m ago".
function formatAgo(ts: number): string {
  const secs = Math.max(0, Math.floor((Date.now() - ts) / 1000));
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h ago`;
  const days = Math.floor(hrs / 24);
  return `${days}d ago`;
}
