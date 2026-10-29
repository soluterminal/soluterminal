import { useStore } from '../store';
import type { ToastPosition } from '../store';
import { TopbarPopover } from './TopbarPopover';
import { isTauri } from '../voice';
import { alertDialog } from '../lib/dialog';
import { IconBell, IconBellOff, IconVolume, IconShare } from './ui/icons';
import { useAccountFeatures } from '../entitlements';
import { IOS_BLOCKED_NOTIFICATIONS_HELP, IOS_ENABLE_NOTIFICATIONS_HELP, IOS_PUSH_HINT, isIosDevice, needsHomeScreenForPush } from '../lib/iosPush';

// The alerts bell — now a notification center as well as the alert-channel
// controls. All three original controls ("🔔/🔕" permission toggle, push "test"
// send, and the voice-alert-mode select) answered "how does this app get my
// attention?"; the bell now also lets you REVIEW what already got it — a session
// log of every toast, so an alert you missed isn't lost when it auto-dismisses.

async function requestToggle(next: boolean): Promise<boolean> {
  const setNotifyEnabled = useStore.getState().setNotifyEnabled;
  const wasOn = useStore.getState().notifyEnabled;
  const ok = await setNotifyEnabled(next);
  if (wasOn || ok) return true;
  // Turning on failed — explain why (copy lifted from pre-consolidation
  // inline handler in Topbar.tsx).
  if (isTauri()) {
    await alertDialog(
      'Windows blocked notifications for SoluTerminal, or the prompt was dismissed.\n\n' +
        'To re-enable:\n' +
        '• Open Windows Settings → System → Notifications\n' +
        '• Scroll to "SoluTerminal" in the app list and turn it on\n' +
        '• If you don\'t see SoluTerminal there yet, make sure Focus Assist / Do Not Disturb is off, then click Alerts again\n\n' +
        'In-app toasts still work without this.',
    );
    return false;
  }
  if (typeof Notification === 'undefined') {
    // iOS browser tabs have no Notification API; a Home Screen launch does.
    await alertDialog(needsHomeScreenForPush()
      ? IOS_ENABLE_NOTIFICATIONS_HELP
      : 'This browser does not support notifications.\n\nIn-app toasts still work.');
    return false;
  }
  if (Notification.permission === 'denied' && isIosDevice()) {
    // Only a Home Screen launch reaches here on iOS (tabs have no API above).
    await alertDialog(`Notifications are turned off for SoluTerminal.\n\n${IOS_BLOCKED_NOTIFICATIONS_HELP}\n\nIn-app toasts still work.`);
    return false;
  }
  if (Notification.permission === 'denied') {
    await alertDialog(
      'Notifications are blocked by your browser for this site.\n\n' +
        '• Chrome / Edge: click the 🔒 / ⓘ icon in the address bar → Site settings → Notifications → Allow\n' +
        '• Firefox: click the 🔒 icon → Clear notification permission → try again\n' +
        '• Safari on Mac: Safari → Settings → Websites → Notifications → set this site to Allow\n' +
        `• ${IOS_BLOCKED_NOTIFICATIONS_HELP}`,
    );
    return false;
  }
  await alertDialog('Notification prompt was dismissed. Try again.');
  return false;
}

function ago(ts: number): string {
  const secs = Math.max(0, Math.floor((Date.now() - ts) / 1000));
  if (secs < 60) return `${secs}s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h`;
  return `${Math.floor(hrs / 24)}d`;
}

const POSITION_LABELS: Record<ToastPosition, string> = {
  'bottom-right': 'Bottom right',
  'bottom-left': 'Bottom left',
  'top-right': 'Top right',
  'top-center': 'Top center',
};

export function AlertsMenu() {
  const notifyEnabled = useStore((s) => s.notifyEnabled);
  const voiceAlertMode = useStore((s) => s.voiceAlertMode);
  const setVoiceAlertMode = useStore((s) => s.setVoiceAlertMode);
  const setBannerSetupOpen = useStore((s) => s.setBannerSetupOpen);
  const notificationLog = useStore((s) => s.notificationLog);
  const unread = useStore((s) => s.unreadNotifications);
  const markSeen = useStore((s) => s.markNotificationsSeen);
  const clearLog = useStore((s) => s.clearNotificationLog);
  const toastPosition = useStore((s) => s.toastPosition);
  const setToastPosition = useStore((s) => s.setToastPosition);
  const revealPane = useStore((s) => s.revealPane);
  const tauri = isTauri();
  // Phone push rides the cloud, so its test tool needs a free account.
  // Without one the bell still drives in-page and desktop notifications.
  const push = useAccountFeatures();
  const iosHint = push && !tauri && needsHomeScreenForPush();

  // Trigger icon reflects the strongest alerting channel currently
  // active: voice mode on the desktop (audible) ranks above the silent
  // notify toggle; disabled everywhere → bell-off.
  // Chime plays everywhere; spoken clips only in the desktop app.
  const voiceOn = voiceAlertMode === 'chime' || (tauri && voiceAlertMode !== 'off');
  const alertsOn = voiceOn || notifyEnabled;
  const TriggerIcon = voiceOn ? IconVolume : notifyEnabled ? IconBell : IconBellOff;
  const triggerTitle =
    voiceOn ? `Alerts & sounds — on (${voiceAlertMode === 'chime' ? 'chime' : 'voice'})`
    : notifyEnabled ? 'Alerts & sounds — notifications on'
    : 'Alerts & sounds — off';

  const closeFromInside = () => {
    document.body.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
  };

  return (
    <TopbarPopover
      minWidth={264}
      align="right"
      onOpenChange={(open) => { if (open) markSeen(); }}
      trigger={({ toggle, open }) => (
        <button
          type="button"
          className={`chrome-btn${alertsOn ? ' chrome-btn-on' : ''}`}
          onClick={toggle}
          title={unread > 0 ? `${triggerTitle} — ${unread} new` : triggerTitle}
          aria-label={unread > 0 ? `Alerts, ${unread} new` : 'Alerts'}
          aria-expanded={open}
        >
          <TriggerIcon />
          {unread > 0 && (
            <span className="chrome-btn-badge" aria-hidden="true">
              {unread > 9 ? '9+' : unread}
            </span>
          )}
        </button>
      )}
    >
      <div className="topbar-menu-row">
        <span className="topbar-menu-label">Notifications</span>
        <button
          type="button"
          onClick={() => void requestToggle(!notifyEnabled)}
        >
          {notifyEnabled ? 'On' : 'Off'}
        </button>
      </div>
      <div className="topbar-menu-row">
        <span className="topbar-menu-label">{tauri ? 'Voice alerts' : 'Sound alerts'}</span>
        <select
          className="topbar-select"
          // Spoken clips are desktop-only; in a browser tab a voice setting
          // plays nothing, so it reads as Off here.
          value={!tauri && voiceAlertMode !== 'chime' ? 'off' : voiceAlertMode}
          onChange={(e) => setVoiceAlertMode(e.target.value as 'off' | 'sonia' | 'ryan' | 'chime')}
          title="Chime: short tones — a rising double for 'needs approval', a soft falling pair for 'reply ready'"
        >
          <option value="off">Off</option>
          <option value="chime">Chime</option>
          {tauri && <option value="sonia">Sonia (UK f)</option>}
          {tauri && <option value="ryan">Ryan (UK m)</option>}
        </select>
      </div>
      {iosHint && (
        <div className="topbar-menu-hint" role="note">
          {IOS_PUSH_HINT}
        </div>
      )}
      {push && notifyEnabled && !tauri && (
        <div className="topbar-menu-row">
          <span className="topbar-menu-label">Test & setup</span>
          <button
            type="button"
            onClick={() => setBannerSetupOpen(true)}
            title="Test notification delivery and set up banners on Android"
          >
            <IconShare size={12} style={{ verticalAlign: '-2px' }} /> Test
          </button>
        </div>
      )}
      <div className="topbar-menu-row">
        <span className="topbar-menu-label">Toast position</span>
        <select
          className="topbar-select"
          value={toastPosition}
          onChange={(e) => setToastPosition(e.target.value as ToastPosition)}
          title="Where transient notifications appear"
        >
          {(Object.keys(POSITION_LABELS) as ToastPosition[]).map((p) => (
            <option key={p} value={p}>{POSITION_LABELS[p]}</option>
          ))}
        </select>
      </div>

      <div className="topbar-menu-sep" />
      <div className="notif-center">
        <div className="notif-center-head">
          <span className="topbar-menu-label">Recent</span>
          {notificationLog.length > 0 && (
            <button
              type="button"
              className="notif-center-clear"
              onClick={() => clearLog()}
              title="Clear the notification history on this device"
            >
              Clear
            </button>
          )}
        </div>
        {notificationLog.length === 0 ? (
          <div className="notif-center-empty">No notifications yet</div>
        ) : (
          <ul className="notif-center-list">
            {notificationLog.map((n) => {
              const clickable = !!n.paneId;
              return (
                <li key={n.id} className={`notif-center-row notif-${n.kind}`}>
                  <button
                    type="button"
                    className="notif-center-item"
                    disabled={!clickable}
                    onClick={() => {
                      if (n.paneId) { void revealPane(n.paneId); closeFromInside(); }
                    }}
                    title={n.paneId ? 'Go to the pane' : n.message}
                  >
                    <span className="notif-center-dot" aria-hidden="true" />
                    <span className="notif-center-msg" style={{ whiteSpace: 'pre-line' }}>{n.message}</span>
                    <span className="notif-center-when">{ago(n.ts)}</span>
                  </button>
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </TopbarPopover>
  );
}
