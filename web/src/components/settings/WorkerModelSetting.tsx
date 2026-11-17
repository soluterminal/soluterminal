import { useEffect, useState } from 'react';
import { api } from '../../api';
import { SettingsGroup, SettingsRow } from './SettingsRow';

export const WORKER_MODEL_LABEL = 'Worker model';

const CHOICES: [string, string][] = [
  ['', "Claude Code's default"],
  ['opus', 'Opus'],
  ['sonnet', 'Sonnet'],
  ['haiku', 'Haiku'],
];

function saveError(e: unknown): string {
  const body = (e as { body?: { message?: unknown } } | null)?.body;
  return typeof body?.message === 'string' ? body.message : `Couldn't save the worker model: ${String(e)}`;
}

// ST-283: the model a Claude worker gets when the agent that starts it
// (spawn_pane, no lead) names none. Without it, a worker takes the model key
// in Claude Code's own settings.json, which any /model pick in any pane
// rewrites. Only the person can change it (the daemon refuses agents). A
// daemon that doesn't know the setting leaves the row hidden.
export function WorkerModelSetting() {
  const [model, setModel] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let mounted = true;
    void api
      .workerSettings()
      .then((s) => {
        if (mounted && typeof s?.workerModel === 'string') setModel(s.workerModel);
      })
      .catch(() => {
        /* older or unreachable daemon: keep the row hidden */
      });
    return () => {
      mounted = false;
    };
  }, []);

  const save = async (next: string) => {
    if (model === null || next === model) return;
    const prev = model;
    setModel(next);
    setError(null);
    try {
      const saved = await api.updateWorkerSettings({ workerModel: next });
      setModel(saved.workerModel ?? next);
    } catch (e) {
      setModel(prev);
      setError(saveError(e));
    }
  };

  if (model === null) return null;
  const choices = CHOICES.some(([v]) => v === model) ? CHOICES : [...CHOICES, [model, model] as [string, string]];
  return (
    <SettingsGroup title="Workers">
      <SettingsRow
        label={WORKER_MODEL_LABEL}
        hint={
          error ??
          "The model for Claude workers an agent starts without naming one. Claude Code's default is the model key in ~/.claude/settings.json, which a /model pick in any pane changes. An agent that names a model still gets it; leads and Codex panes aren't affected."
        }
      >
        <select
          className="settings-select"
          aria-label={WORKER_MODEL_LABEL}
          value={model}
          onChange={(e) => void save(e.target.value)}
        >
          {choices.map(([value, label]) => (
            <option key={value} value={value}>
              {label}
            </option>
          ))}
        </select>
      </SettingsRow>
    </SettingsGroup>
  );
}
