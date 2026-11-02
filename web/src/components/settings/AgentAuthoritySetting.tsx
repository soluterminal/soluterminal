import { useEffect, useRef, useState } from 'react';
import { api } from '../../api';
import type { AgentAuthoritySettings } from '../../api';
import { SettingsGroup, SettingsRow } from './SettingsRow';

export const AGENT_AUTHORITY_LABEL = 'Let agents in panes approve for me';
export const AGENT_AUTHORITY_HINT =
  'Off: only you can allow permission prompts and turn on auto-approve. ' +
  'On: agents running in your panes can do these for you. Agents can never turn this on ' +
  'or widen autopilot and YOLO settings. Moving your panes and renaming or deleting workspaces still need your Allow in the app.';

type Choice = 'off' | '1' | '8' | 'always';

function choiceOf(s: AgentAuthoritySettings): Choice {
  if (!s.active) return 'off';
  if (!s.until) return 'always';
  const hours = (new Date(s.until).getTime() - Date.now()) / 3_600_000;
  return hours > 1.5 ? '8' : '1';
}

// The daemon decides who may act for the person (daemon/caller_authority.go);
// this row only shows and changes the grant. A daemon without the endpoint
// answers 404 and the row stays hidden.
export function AgentAuthoritySetting() {
  const [settings, setSettings] = useState<AgentAuthoritySettings | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const pendingRef = useRef(false);

  useEffect(() => {
    let mounted = true;
    void api
      .getAgentAuthority()
      .then((s) => {
        if (mounted && s) setSettings(s);
      })
      .catch(() => {
        /* older or unreachable daemon: keep the row hidden */
      });
    return () => {
      mounted = false;
    };
  }, []);

  if (!settings) return null;

  const change = async (next: Choice) => {
    if (pendingRef.current) return;
    pendingRef.current = true;
    setPending(true);
    setError(null);
    try {
      setSettings(
        await api.setAgentAuthority(
          next === 'off' ? { panes: false } : { panes: true, hours: next === 'always' ? 0 : Number(next) },
        ),
      );
    } catch (e) {
      setError(`Couldn't save agent permissions: ${String(e)}`);
    } finally {
      pendingRef.current = false;
      setPending(false);
    }
  };

  return (
    <SettingsGroup title="Agent permissions">
      <SettingsRow
        label={AGENT_AUTHORITY_LABEL}
        hint={
          <>
            {AGENT_AUTHORITY_HINT}
            {settings.active && settings.until && (
              <> Ends {new Date(settings.until).toLocaleString()}.</>
            )}
          </>
        }
      >
        <select
          className="settings-select"
          value={choiceOf(settings)}
          disabled={pending}
          onChange={(e) => void change(e.target.value as Choice)}
        >
          <option value="off">Off</option>
          <option value="1">On for 1 hour</option>
          <option value="8">On for 8 hours</option>
          <option value="always">On until I turn it off</option>
        </select>
      </SettingsRow>
      <p className="settings-row-hint">This can only be turned on at this computer.</p>
      {error && <p className="settings-row-hint settings-error">{error}</p>}
    </SettingsGroup>
  );
}
