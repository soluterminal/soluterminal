import type { PaneInfo } from '../types';
import { useStore } from '../store';
import { dismissPaneRecordingNotice, RECORDING_TOOLTIP, recordingStoppedText } from '../lib/paneRecording';

// The recording mark, from daemon state only (pane.recording): a red dot and
// "REC" while the pane's output is being recorded, and "recording stopped:
// <reason>" after a stop the person should see, until they dismiss it. Shown
// on the pane header (always visible, never folded into +N) and on the pane's
// sidebar row, for every viewer.
export function PaneRecordingMark({ pane, place }: { pane: PaneInfo; place: 'header' | 'row' }) {
  const rec = pane.recording;
  if (rec?.on) {
    return (
      <span
        className={`pane-rec-mark is-${place}`}
        role="status"
        aria-label={RECORDING_TOOLTIP}
        title={RECORDING_TOOLTIP}
        data-testid={`pane-rec-${place}`}
        onMouseDownCapture={(e) => e.stopPropagation()}
        onDoubleClick={(e) => e.stopPropagation()}
      >
        <span className="pane-rec-dot" aria-hidden />REC
      </span>
    );
  }
  if (!rec?.stoppedReason) return null;
  const text = recordingStoppedText(rec.stoppedReason);
  if (place === 'row') {
    return <span className="pane-rec-stopped is-row" title={text} data-testid="pane-rec-stopped-row">REC stopped</span>;
  }
  return (
    <span
      className="pane-rec-stopped is-header"
      role="status"
      title={text}
      data-testid="pane-rec-stopped-header"
      onMouseDownCapture={(e) => e.stopPropagation()}
      onDoubleClick={(e) => e.stopPropagation()}
      onClick={(e) => e.stopPropagation()}
    >
      {text}
      <button type="button" className="pane-rec-dismiss" aria-label="Dismiss" title="Dismiss"
        onClick={() => void dismissPaneRecordingNotice(pane)}>×</button>
    </span>
  );
}

// The status bar's "N recording": every recording pane on the machine,
// including those in other workspaces.
export function RecordingStatusChip() {
  const titles = useStore((s) => {
    const out: string[] = [];
    for (const id in s.panes) {
      if (s.panes[id].recording?.on) out.push(s.panes[id].title || id);
    }
    return out.sort().join('\n');
  });
  if (!titles) return null;
  const names = titles.split('\n');
  const label = `${names.length} recording`;
  const title = `Recording the output of: ${names.join(', ')}. Saved on this computer only, deleted after 7 days.`;
  return (
    <span className="statusbar-recording" role="status" aria-label={title} title={title} data-testid="statusbar-recording">
      <span className="pane-rec-dot" aria-hidden />{label}
    </span>
  );
}
