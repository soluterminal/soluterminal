import { useMemo } from 'react';
import { api } from '../../api';
import { useStore } from '../../store';
import { Tooltip } from '../Tooltip';

// Compact auto-approver status pill for the account footer. Carries the two
// signals that matter at a glance — paused state and how many auto-approvals
// fired in the last hour — plus the one action users actually reach for
// (releasing backed-off panes). Reads the store snapshot App.tsx keeps fresh
// via SSE + boot fetch. There is no hourly cap (see aria.go); the count is
// informational.
export function AriaHeaderStatus() {
  const paused = useStore((s) => s.ariaPaused);
  const autoFires = useStore((s) => s.ariaAutoFiresHr);
  const setStatusSnapshot = useStore((s) => s.setAriaStatusSnapshot);
  const panesMap = useStore((s) => s.panes);
  // Panes the sweeper gave up on need a one-click release (ResetAutoFireCap
  // clears their backoff). Derived from the live panes map — stable selector
  // + useMemo, an inline fresh-array selector would trip zustand's Object.is
  // compare.
  const anyBackedOff = useMemo(() => Object.values(panesMap).some((p) =>
    p.status === 'running'
    && (p.autoMode === 'always' || p.autoMode === 'cautious')
    && (p.ariaBackoffStreak ?? 0) >= 3,
  ), [panesMap]);

  return (
    <>
      {paused && (
        <button
          type="button"
          className="aria-pause aria-pause-on"
          title="Autoresponder paused — click to resume"
          onClick={() => {
            void api.ariaPause(false).then(setStatusSnapshot).catch(() => {});
          }}
        >
          resume
        </button>
      )}
      {anyBackedOff && (
        <button
          type="button"
          className="aria-cap-reset"
          title="Release panes the sweeper gave up on (backed-off badge)."
          onClick={() => {
            void api.ariaResetCap().then(setStatusSnapshot).catch(() => {});
          }}
        >
          reset
        </button>
      )}
      {/* Focusable so the explanation shows for keyboard users too; the
          bare "219/hr" never said what was being counted. */}
      <Tooltip
        label={paused
          ? 'Autoresponder paused — no auto-sends fire while paused'
          : `${autoFires} auto-approval${autoFires === 1 ? '' : 's'} in the last hour`}
        side="top"
      >
        <span className="aria-cap" tabIndex={0}>
          {paused ? 'paused' : `${autoFires} auto-approved/hr`}
        </span>
      </Tooltip>
    </>
  );
}
