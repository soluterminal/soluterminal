import { useEffect, useMemo, useState } from 'react';
import type { RecordingRule } from '../types';

// The body of the recording start dialog (DialogHost kind 'recording'): the
// time limit, and for each pane the defaults that would refuse it, each with
// a "Record anyway" box naming the default in plain words. A pane with a
// default left unticked is skipped; a pane a hard rule refuses is listed as
// skipped with its reason. Nothing chosen here is remembered for next time.

export interface RecordingDialogPane {
  id: string;
  /** What the person calls the pane (its title or handle); shown here only. */
  name: string;
  defaults: RecordingRule[];
  /** Set when the pane is skipped whatever is chosen, with the reason. */
  skip?: string;
}

export interface RecordingDialogSpec {
  panes: RecordingDialogPane[];
  hours: number[];
  defaultHours: number;
  deleteAfterDays: number;
  /** "Record every pane in this workspace" rather than one pane. */
  workspace: boolean;
}

export interface RecordingChoice {
  hours: number;
  panes: { id: string; override: RecordingRule[] }[];
}

/** The defaults in plain words: what the dialog says it is overriding. */
export const RECORDING_RULE_TEXT: Record<RecordingRule, string> = {
  handle: 'Its handle is on the do-not-record list (secure, ops, release, publish and gate panes).',
  'folder:launch_security': 'It works in launch/roles/security, a folder on the do-not-record list.',
  'folder:launch_deploy': 'It works in launch/deploy, a folder on the do-not-record list.',
  'folder:config': 'It works in ~/.config, where other tools keep their tokens; the folder is on the do-not-record list. Moving to another listed folder stops the recording.',
  'folder:etc': 'It works in /etc, a folder on the do-not-record list.',
  shell: 'Shells print secrets (tokens, env, cat). Switch recording off before a login or sudo.',
};

export const RECORDING_RULE_TITLE: Record<RecordingRule, string> = {
  handle: 'Do-not-record handle',
  'folder:launch_security': 'Do-not-record folder',
  'folder:launch_deploy': 'Do-not-record folder',
  'folder:config': 'Do-not-record folder',
  'folder:etc': 'Do-not-record folder',
  shell: 'Plain shell',
};

/** The choice the boxes add up to, or null when nothing would be recorded. */
export function recordingChoiceOf(spec: RecordingDialogSpec, hours: number, ticked: Record<string, RecordingRule[]>): RecordingChoice | null {
  const panes = spec.panes
    .filter((p) => !p.skip && p.defaults.every((d) => ticked[p.id]?.includes(d)))
    .map((p) => ({ id: p.id, override: p.defaults.filter((d) => ticked[p.id]?.includes(d)) }));
  return panes.length > 0 ? { hours, panes } : null;
}

export function RecordingDialogBody({ spec, onChange }: { spec: RecordingDialogSpec; onChange: (choice: RecordingChoice | null) => void }) {
  const [hours, setHours] = useState(spec.defaultHours);
  const [ticked, setTicked] = useState<Record<string, RecordingRule[]>>({});
  const choice = useMemo(() => recordingChoiceOf(spec, hours, ticked), [spec, hours, ticked]);
  useEffect(() => onChange(choice), [choice, onChange]);

  const toggle = (paneId: string, rule: RecordingRule, on: boolean) =>
    setTicked((t) => {
      const cur = t[paneId] ?? [];
      return { ...t, [paneId]: on ? [...cur.filter((r) => r !== rule), rule] : cur.filter((r) => r !== rule) };
    });

  return (
    <div className="recording-dialog">
      <label className="recording-dialog-hours">
        Stop recording after{' '}
        <select aria-label="Time limit" value={hours} onChange={(e) => setHours(Number(e.target.value))}>
          {spec.hours.map((h) => <option key={h} value={h}>{h === 1 ? '1 hour' : `${h} hours`}</option>)}
        </select>
      </label>
      <ul className="recording-dialog-panes" aria-label={spec.workspace ? 'Panes in this workspace' : 'Pane'}>
        {spec.panes.map((p) => {
          const included = !p.skip && p.defaults.every((d) => ticked[p.id]?.includes(d));
          return (
            <li key={p.id} className={`recording-dialog-pane${included ? '' : ' skipped'}`} data-pane-id={p.id}>
              <div className="recording-dialog-pane-name">
                {p.name}
                {spec.workspace && <span className="recording-dialog-pane-state">{included ? ' — will be recorded' : ' — skipped'}</span>}
              </div>
              {p.skip ? (
                <div className="recording-dialog-skip">Skipped: {p.skip}</div>
              ) : (
                p.defaults.map((rule) => (
                  <label key={rule} className="recording-dialog-override">
                    <input type="checkbox" checked={ticked[p.id]?.includes(rule) ?? false}
                      onChange={(e) => toggle(p.id, rule, e.target.checked)} />
                    <span><strong>{RECORDING_RULE_TITLE[rule]}.</strong> {RECORDING_RULE_TEXT[rule]} Record anyway.</span>
                  </label>
                ))
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
