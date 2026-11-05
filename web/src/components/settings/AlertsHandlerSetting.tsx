import { useCallback, useEffect, useState } from 'react';
import { api } from '../../api';
import type { NotificationsHandlerState } from '../../api';
import { SettingsGroup, SettingsRow } from './SettingsRow';

export const TAKE_ALERTS_BACK = 'Take alerts back';

// The heading names who holds the alerts: the app's own label, else its
// approved device's name.
export function alertsHandlerLabel(s: NotificationsHandlerState): string {
  const who = s.client || s.deviceName || 'another app';
  return `Alerts handled by ${who}`;
}

export function alertsHandlerHint(s: NotificationsHandlerState): string {
  const device = s.deviceName ? `the approved device “${s.deviceName}”` : 'an approved device';
  const until = s.expiresAtMs ? ` until ${new Date(s.expiresAtMs).toLocaleTimeString()} unless it renews` : '';
  return `Code alerts are delivered by an app on ${device}${until}, so this computer doesn't send its own phone push for them. Device and security notices always come from this computer.`;
}

// While another app on an approved device delivers Code alerts (daemon
// notifications_handler.go), say who and until when, and let the person take
// them back. Hidden when no app holds them or the daemon is older.
export function AlertsHandlerSetting() {
  const [state, setState] = useState<NotificationsHandlerState | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(() => {
    void api.getNotificationsHandler().then(setState).catch(() => {
      /* older or unreachable daemon, or a remote viewer: keep it hidden */
    });
  }, []);

  useEffect(() => {
    load();
    const timer = window.setInterval(load, 5000);
    return () => window.clearInterval(timer);
  }, [load]);

  if (!state?.active) return null;

  const takeBack = async () => {
    if (pending) return;
    setPending(true);
    setError(null);
    try {
      await api.releaseNotificationsHandler();
    } catch (e) {
      setError(`Couldn't take alerts back: ${String(e)}`);
    } finally {
      setPending(false);
      load();
    }
  };

  return (
    <SettingsGroup title="Alerts">
      <SettingsRow label={alertsHandlerLabel(state)} hint={alertsHandlerHint(state)}>
        <button type="button" className="settings-btn" disabled={pending} onClick={() => void takeBack()}>
          {TAKE_ALERTS_BACK}
        </button>
      </SettingsRow>
      {error && <p className="settings-row-hint settings-error">{error}</p>}
    </SettingsGroup>
  );
}
