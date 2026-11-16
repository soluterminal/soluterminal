import { useEffect, useRef, useState } from 'react';
import { api } from '../../api';
import type { AgentIntegrationSettings } from '../../api';
import { SettingsCheck, SettingsGroup } from './SettingsRow';

export const AGENT_INTEGRATION_LABEL = 'Let Claude Code and Codex use SoluTerminal';
export const AGENT_INTEGRATION_HINT =
  'Agents you start in SoluTerminal always work together. When on, Claude Code and Codex started outside ' +
  'SoluTerminal (another terminal, or typed in a Windows PowerShell pane) can see and message your panes too. ' +
  'It adds three entries to their settings files, including Claude Code hooks; turning it off removes them and ' +
  'keeps a backup of each file.';

// The daemon owns this choice (it writes and removes the entries), so the
// switch reads and writes it there. An older daemon answers 404 and the group
// stays hidden rather than offering a switch that does nothing.
export function AgentIntegrationSetting() {
  const [settings, setSettings] = useState<AgentIntegrationSettings | null>(null);
  const [pending, setPending] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const pendingRef = useRef(false);

  useEffect(() => {
    let mounted = true;
    void api
      .getAgentIntegration()
      .then((s) => {
        if (mounted && s) setSettings(s);
      })
      .catch(() => {
        /* older or unreachable daemon: keep the group hidden */
      });
    return () => {
      mounted = false;
    };
  }, []);

  if (!settings) return null;
  const on = settings.effective === 'on';

  const change = async (next: boolean) => {
    if (pendingRef.current) return;
    pendingRef.current = true;
    setPending(true);
    setError(null);
    setMessage(null);
    const previous = settings;
    setSettings({ ...previous, effective: next ? 'on' : 'off' });
    try {
      const saved = await api.setAgentIntegration(next ? 'on' : 'off');
      setSettings(saved);
      const failed = (saved.cleanup ?? []).filter((r) => r.error);
      if (failed.length > 0) {
        setError(`Some entries couldn't be removed: ${failed.map((r) => `${r.what} (${r.error})`).join('; ')}`);
      } else if (next) {
        setMessage('Added. New Claude Code and Codex sessions get SoluTerminal\'s tools.');
      } else {
        setMessage('Removed. A copy of each changed file was saved in SoluTerminal\'s data folder.');
      }
    } catch (e) {
      setSettings(previous);
      setError(`Couldn't save the setting: ${String(e)}`);
    } finally {
      pendingRef.current = false;
      setPending(false);
    }
  };

  return (
    <SettingsGroup title="Claude Code and Codex">
      <SettingsCheck
        label={AGENT_INTEGRATION_LABEL}
        checked={on}
        disabled={pending}
        onChange={(next) => void change(next)}
        hint={AGENT_INTEGRATION_HINT}
      />
      <details className="settings-row-hint">
        <summary>What this changes</summary>
        <ul>
          {settings.changes.map((c) => (
            <li key={c.path + c.what}>
              {c.what}: <code>{c.path}</code>
            </li>
          ))}
        </ul>
        <p>
          Turning it off removes only SoluTerminal's own entries. Uninstalling SoluTerminal removes them too.
          This can only be changed on this computer.
        </p>
      </details>
      {message && <p className="settings-row-hint">{message}</p>}
      {error && <p className="settings-row-hint settings-error">{error}</p>}
    </SettingsGroup>
  );
}
