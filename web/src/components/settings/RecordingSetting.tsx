import { useEffect, useRef, useState } from 'react';
import { api, ApiError, currentRoutingScope } from '../../api';
import { isRelayedSession, useCloud } from '../../cloud';
import { confirmDialog } from '../../lib/dialog';
import { daemonSupports, PANE_RECORDING_TIMELINE, PANE_RECORDING_V2 } from '../../lib/daemonFeatures';
import type { RecordingSettings } from '../../types';
import { SettingsGroup, SettingsRow } from './SettingsRow';

export const RECORDING_DAYS_LABEL = 'Delete pane recordings after';
export const RECORDING_CAP_LABEL = 'Space for pane recordings';
// Cleared by the security owner verbatim (Addendum D d).
export const RECORDING_LOWERING_NOTE =
  'Lowering applies to recordings made from now on; to delete older ones sooner, lower it here at this computer.';

// The confirm before a lowering at this computer deletes older recordings at
// once. Cleared by the security owner verbatim (20 Nov).
export const RECORDING_LOWER_TITLE = 'Delete older recordings now?';
export const RECORDING_LOWER_CONFIRM = 'Delete and save';
export const recordingLowerDaysText = (n: number) =>
  `Lowering this to ${n} days deletes every recording last written more than ${n} days ago, now. This can't be undone.`;
export const recordingLowerCapText = (n: number) =>
  `Lowering this to ${n} MB deletes the oldest recordings until the rest fit, now. This can't be undone.`;

// How often a save re-reads and asks again when the values changed under it.
const RECORDING_SAVE_ATTEMPTS = 3;

// A whole number within bounds, committed on blur or Enter. A value outside
// them is refused with the bounds shown and nothing is sent: never pulled into
// bounds (0 days must not become 1 day and delete).
function BoundedField({ label, unit, value, min, max, disabled, onCommit }: {
  label: string;
  unit: string;
  value: number;
  min: number;
  max: number;
  disabled: boolean;
  onCommit: (value: number) => void;
}) {
  const [draft, setDraft] = useState(String(value));
  const [synced, setSynced] = useState(value);
  const [refused, setRefused] = useState<string | null>(null);
  if (value !== synced) {
    setSynced(value);
    setDraft(String(value));
  }
  const commit = () => {
    const text = draft.trim();
    const n = Number(text);
    if (!/^[0-9]+$/.test(text) || n < min || n > max) {
      setRefused(`Enter a whole number from ${min} to ${max} ${unit}. Nothing was saved.`);
      setDraft(String(value));
      return;
    }
    setRefused(null);
    if (n !== value) onCommit(n);
  };
  return (
    <>
      <input
        className="settings-input"
        aria-label={unit === 'days' ? 'Days' : 'MB'}
        inputMode="numeric"
        disabled={disabled}
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={commit}
        onKeyDown={(e) => {
          if (e.key === 'Enter') { e.preventDefault(); commit(); }
          if (e.key === 'Escape') { setDraft(String(value)); setRefused(null); }
        }}
      />
      {' '}{unit}
      {refused && <p className="settings-row-hint settings-error" role="alert" aria-label={`${label}: refused`}>{refused}</p>}
    </>
  );
}

// Pane output recordings (docs/pane-recorder.md): how long they are kept and
// how much space they may use, both on the daemon. Raising either needs the
// person at this computer and asks nothing; a lowering here deletes older
// recordings at once, so it asks first. Reached through the cloud, a lowering
// applies only to later recordings and deletes nothing now. A daemon without
// the setting (older than pane-recording.v2) hides the group.
export function RecordingSetting() {
  const [settings, setSettings] = useState<RecordingSettings | null>(null);
  const [error, setError] = useState<string | null>(null);
  // One save at a time; the fields are disabled until it settles, so a
  // second edit is never dropped behind the first.
  const [saving, setSaving] = useState(false);
  // Bumped after a refused or cancelled save so the fields show the values in
  // force again.
  const [epoch, setEpoch] = useState(0);
  const pending = useRef(false);
  // The daemon whose values are shown: every read and save of this group goes
  // to it, even if the view switches daemon while Settings is open.
  const scope = useRef(currentRoutingScope()).current;
  // The daemon checks the values read before a save (it ships with the
  // timeline, pane-recording-timeline.v1); an older one refuses the fields.
  const expectServed = useRef(false);

  useEffect(() => {
    let mounted = true;
    void api
      .healthScoped(scope)
      .then((h) => {
        expectServed.current = daemonSupports(h, PANE_RECORDING_TIMELINE);
        return daemonSupports(h, PANE_RECORDING_V2) ? api.getRecordingSettings(scope) : null;
      })
      .then((s) => {
        if (mounted && s) setSettings(s);
      })
      .catch(() => {
        /* older or unreachable daemon: keep the group hidden */
      });
    return () => {
      mounted = false;
    };
  }, [scope]);

  if (!settings) return null;
  const b = settings.bounds;

  const save = async (body: { deleteAfterDays?: number; capMB?: number }) => {
    if (pending.current) return;
    pending.current = true;
    setSaving(true);
    setError(null);
    try {
      // Judge a lowering against the daemon's values now, not the ones read
      // when Settings opened, and send those values: if they changed again
      // before the save (another window), the daemon answers 409 and this
      // reads them again and asks again.
      for (let attempt = 0; ; attempt++) {
        const now = await api.getRecordingSettings(scope);
        setSettings(now);
        const lowering = (body.deleteAfterDays ?? Infinity) < now.deleteAfterDays || (body.capMB ?? Infinity) < now.capMB;
        if (lowering && !isRelayedSession(useCloud.getState())) {
          const ok = await confirmDialog({
            title: RECORDING_LOWER_TITLE,
            message: body.deleteAfterDays !== undefined ? recordingLowerDaysText(body.deleteAfterDays) : recordingLowerCapText(body.capMB!),
            confirmLabel: RECORDING_LOWER_CONFIRM,
            cancelLabel: 'Cancel',
            danger: true,
            focusCancel: true,
          });
          if (!ok) {
            setEpoch((n) => n + 1);
            return;
          }
        }
        try {
          const expect = expectServed.current ? { expectDeleteAfterDays: now.deleteAfterDays, expectCapMB: now.capMB } : {};
          setSettings(await api.putRecordingSettings({ ...body, ...expect }, scope));
          return;
        } catch (e) {
          if (!(e instanceof ApiError && e.status === 409) || attempt >= RECORDING_SAVE_ATTEMPTS - 1) throw e;
        }
      }
    } catch (e) {
      setError(`Couldn't save: ${e instanceof Error ? e.message : String(e)}`);
      try {
        setSettings(await api.getRecordingSettings(scope));
      } catch { /* keep the last values read */ }
      setEpoch((n) => n + 1);
    } finally {
      pending.current = false;
      setSaving(false);
    }
  };

  return (
    <SettingsGroup title="Pane recordings">
      <SettingsRow
        label={RECORDING_DAYS_LABEL}
        hint={`${b.deleteAfterDaysMin} to ${b.deleteAfterDaysMax} days after a recording's last write. Recordings are saved on this computer only and never sent to the cloud.`}
      >
        <BoundedField key={`days-${epoch}`} label={RECORDING_DAYS_LABEL} unit="days" disabled={saving}
          value={settings.deleteAfterDays} min={b.deleteAfterDaysMin} max={b.deleteAfterDaysMax}
          onCommit={(deleteAfterDays) => void save({ deleteAfterDays })} />
      </SettingsRow>
      <SettingsRow
        label={RECORDING_CAP_LABEL}
        hint={`${b.capMBMin} to ${b.capMBMax} MB (${b.capMBMin / 1000} to ${b.capMBMax / 1000} GB) for all recordings together. Over it, the oldest recordings are deleted first.`}
      >
        <BoundedField key={`cap-${epoch}`} label={RECORDING_CAP_LABEL} unit="MB" disabled={saving}
          value={settings.capMB} min={b.capMBMin} max={b.capMBMax}
          onCommit={(capMB) => void save({ capMB })} />
      </SettingsRow>
      <p className="settings-row-hint">{RECORDING_LOWERING_NOTE}</p>
      {error && <p className="settings-row-hint settings-error" role="alert">{error}</p>}
    </SettingsGroup>
  );
}
