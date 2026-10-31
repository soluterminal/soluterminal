import { slotList } from '../../extensions';

// Settings › Notifications & voice. Voice settings come from the voice feature
// module (extensions.ts settingsSections, tab 'notifications'). Notification
// settings (on/off, sounds, toast position) still live in the top-bar Alerts
// menu; when they move into Settings they go first in this tab.

function useVisibleSections() {
  // The slot list is fixed for the page's lifetime, so the hook order is
  // stable.
  const sections = slotList('settingsSections').filter((section) => section.tab === 'notifications');
  const visible = sections.map((section) => section.useVisible?.() ?? true);
  return sections.filter((_, i) => visible[i]);
}

/** Whether the tab has anything to show. The modal hides it otherwise. */
export function useNotificationsVoiceVisible(): boolean {
  return useVisibleSections().length > 0;
}

export function NotificationsVoiceTab() {
  const sections = useVisibleSections();
  return (
    <>
      {sections.map((section, i) => <section.Component key={i} />)}
    </>
  );
}
