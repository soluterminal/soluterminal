import { useState } from 'react';
import type { DropZone } from '../store';
import { slotValue } from '../extensions';
import { CustomScrollbar } from './CustomScrollbar';
import { ScrollToLatestButton } from './ScrollToLatestButton';
import { RESTART_PAUSED_BODY, RESTART_PAUSED_TITLE } from '../lib/restartPaused';

interface PaneBodyOverlaysProps {
  paneId: string;
  connStatus: 'connecting' | 'open' | 'reconnecting' | 'closed';
  paneStatus: 'running' | 'exited';
  exitCode?: number;
  // resumable + onResume drive the one closed state that has a real action:
  // a pane whose durable conversation can be resumed in place — Claude
  // (`claude --resume`), Codex, Muse, or a managed DeepSeek/Pi harness
  // (`SOLU_HARNESS_RESUME_SESSION`). This happens after a daemon restart that
  // suspended the pane instead of auto-respawning it (see daemon
  // SuspendExisting), or after a live client exit.
  resumable?: boolean;
  // A pane paused across a restart under "Don't resume": shown as paused,
  // with no Resume offer (the pane menu still has Resume session).
  restartPaused?: boolean;
  onResume?: () => Promise<unknown>;
  inputLocked: boolean;
  // True when the lock is because NOBODY owns the pane (daemon restart /
  // owner disconnected) rather than a peer device holding input — the
  // overlay copy must not claim "in use somewhere else" in that state.
  inputLockUnowned?: boolean;
  dropZone: DropZone | null;
  beingDragged: boolean;
  dictating: boolean;
  dictationPhase: string | null;
  dictationAboutToCancel: boolean;
  showsReaderChip: boolean;
  scrolledAway: boolean;
  newLinesCount: number;
  lastOutputMs: number;
  isMobileView: boolean;
  settling: boolean;
  getViewport: () => HTMLElement | null;
  jumpToLive: () => void;
}

function paneBodyState(
  connStatus: PaneBodyOverlaysProps['connStatus'],
  paneStatus: PaneBodyOverlaysProps['paneStatus'],
  exitCode: number | undefined,
  inputLocked: boolean,
  inputLockUnowned: boolean,
): { kind: 'connecting' | 'reconnecting' | 'closed' | 'input-locked'; title: string; body: string } | null {
  if (inputLocked) {
    // Two locked worlds: a peer device really holds input, vs nobody does
    // (the daemon restarted or the controlling device disconnected). The
    // previous owner re-adopts an unowned pane automatically, so this copy
    // mostly shows on a device that was already a passive viewer.
    return inputLockUnowned
      ? {
          kind: 'input-locked',
          title: 'Typing is paused on this screen',
          body: 'Use Type here in the banner to type from this screen.',
        }
      : {
          kind: 'input-locked',
          title: 'Typing is on another device',
          body: 'Use Type here in the banner to type from this screen.',
        };
  }
  if (paneStatus === 'exited' || connStatus === 'closed') {
    return {
      kind: 'closed',
      title: 'Session closed',
      body: exitCode == null
        ? 'The terminal process has exited.'
        : `The terminal process exited with code ${exitCode}.`,
    };
  }
  if (connStatus === 'reconnecting') {
    return {
      kind: 'reconnecting',
      title: 'Reconnecting to pane',
      body: 'Output may be stale and input is paused until the connection returns.',
    };
  }
  if (connStatus === 'connecting') {
    return {
      kind: 'connecting',
      title: 'Connecting to pane',
      body: 'Attaching to the terminal session.',
    };
  }
  return null;
}

export function PaneBodyOverlays({
  paneId,
  connStatus,
  paneStatus,
  exitCode,
  resumable,
  restartPaused,
  onResume,
  inputLocked,
  inputLockUnowned,
  dropZone,
  beingDragged,
  dictating,
  dictationPhase,
  dictationAboutToCancel,
  showsReaderChip,
  scrolledAway,
  newLinesCount,
  lastOutputMs,
  isMobileView,
  settling,
  getViewport,
  jumpToLive,
}: PaneBodyOverlaysProps) {
  const DictationChip = slotValue('paneDictation')?.Chip;
  const [resuming, setResuming] = useState(false);
  const state = paneBodyState(connStatus, paneStatus, exitCode, inputLocked, !!inputLockUnowned);
  // A suspended Claude pane (session resumable in place) is the one closed
  // state with a real action — show Resume. The optimistic store flip clears
  // `resumable` on click, so the button unmounts immediately; `resuming` only
  // covers the brief click→flip window.
  const paused = state?.kind === 'closed' && !!resumable && !!restartPaused;
  const showResume = state?.kind === 'closed' && !!resumable && !!onResume && !paused;
  return (
    <>
      {state && (
        <div
          className={`pane-state-overlay pane-state-${state.kind}`}
          role="status"
          aria-live="polite"
          aria-label={paused ? RESTART_PAUSED_TITLE : state.title}
        >
          {/* A locked pane keeps the overlay (it blocks input) but not the card:
              the single grid banner (SessionClaimOverlay) explains it once,
              instead of one card per pane plus the banner. */}
          {state.kind !== 'input-locked' && <div className="pane-state-card">
            <div className="pane-state-title">{paused ? RESTART_PAUSED_TITLE : state.title}</div>
            {/* On a normal exit a Claude pane drops back to an interactive
                shell in place (daemon spawn: `…; claude; exec bash -i`), so
                this overlay is purely informational — resume via the
                pane-header session-id badge, close via the pane's X. The ONE
                exception is a pane the daemon SUSPENDED across a restart (no
                shell to drop to): it carries `resumable`, so we offer a real
                Resume button here (matches the native Session-ended card).
                The copy is deliberately provider-agnostic: this overlay also
                serves the managed DeepSeek/Pi terminal harnesses, whose
                conversation is not a Claude session. */}
            <div className="pane-state-body">
              {paused
                ? RESTART_PAUSED_BODY
                : showResume
                  ? 'Resume to continue this session, or close the pane.'
                  : state.body}
            </div>
            {showResume && (
              <div className="pane-state-actions">
                <button
                  type="button"
                  className="pane-state-btn pane-state-btn-primary"
                  title="Resume this session"
                  disabled={resuming}
                  onClick={async (e) => {
                    e.stopPropagation();
                    setResuming(true);
                    try {
                      await onResume!();
                    } catch {
                      // store.resumePane already toasts + restores the card.
                    } finally {
                      setResuming(false);
                    }
                  }}
                >
                  {resuming ? 'Resuming…' : 'Resume session'}
                </button>
              </div>
            )}
          </div>}
        </div>
      )}
      {dropZone && !beingDragged && (
        <div className={`drop-zone-overlay zone-${dropZone}`} aria-hidden>
          <span className="drop-zone-hint">
            {dropZone === 'center' ? 'swap' : `insert ${dropZone}`}
          </span>
        </div>
      )}
      {dictating && DictationChip && (
        <DictationChip phase={dictationPhase ?? ''} aboutToCancel={dictationAboutToCancel} />
      )}
      <ScrollToLatestButton
        visible={showsReaderChip && scrolledAway}
        count={newLinesCount}
        lastOutputMs={lastOutputMs}
        onClick={jumpToLive}
        // Surface the keyboard shortcut on desktop only — mobile has no
        // physical key and no hover to reveal a title. The binding is
        // Shift+End (Pane.tsx scrollback-nav handler); bare End goes to
        // the shell, so naming just "End" would mislead.
        title={isMobileView ? 'Return to latest output' : 'Return to latest output (Shift+End)'}
      />
      {isMobileView && (
        <CustomScrollbar
          key={paneId}
          getViewport={getViewport}
          hidden={settling}
        />
      )}
      {isMobileView && settling && (
        <div className="pane-settling" aria-hidden />
      )}
    </>
  );
}
