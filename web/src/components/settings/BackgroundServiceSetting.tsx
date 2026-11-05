import { useEffect, useState } from 'react';
import { backgroundServiceCapability, restartBackgroundService, useBackgroundService } from '../../lib/backgroundService';
import { SettingsGroup, SettingsRow } from './SettingsRow';

export function BackgroundServiceSetting() {
  const [supported, setSupported] = useState(false);
  const { pending, message, error } = useBackgroundService();
  useEffect(() => {
    let active = true;
    void backgroundServiceCapability().then((s) => { if (active) setSupported(s.supported); }).catch(() => {});
    return () => { active = false; };
  }, []);
  if (!supported && !message) return null;
  return <SettingsGroup title="Background service">
    <SettingsRow label="Restart background service" hint="Reload the service env file. Panes keep running and reconnect when the service returns.">
      <button type="button" className="settings-btn" disabled={pending || !supported} onClick={() => void restartBackgroundService()}>{pending ? 'Restarting…' : 'Restart background service'}</button>
    </SettingsRow>
    {message && <p className={error ? 'settings-row-hint settings-error' : 'settings-row-hint'} role={error ? 'alert' : 'status'}>{message}</p>}
  </SettingsGroup>;
}
