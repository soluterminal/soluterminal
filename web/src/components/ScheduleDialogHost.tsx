import { useEffect, useState } from 'react';
import { useStore } from '../store';
import { ScheduleDialog } from './ScheduleDialog';

// Always-mounted host for the `solu:open-schedule-dialog` event (pane
// context menu "Schedule this pane…", mobile footer, anything else). The
// listener used to live inside SchedulesSection, which only mounts when
// the sidebar section is visible — a user who hid the section (or a phone
// whose drawer prefs hide it) dispatched the event into the void. Same
// lesson as ScheduleHistoryDeepLink: deep-link style events need a host
// that is mounted unconditionally in App.
export function ScheduleDialogHost() {
  const [state, setState] = useState<{ open: boolean; paneId?: string; fromPaneMenu: boolean }>({
    open: false,
    fromPaneMenu: false,
  });

  useEffect(() => {
    const handler = (e: Event) => {
      const detail = (e as CustomEvent).detail as { paneId?: string; fromPaneMenu?: boolean } | undefined;
      // The pane to seed from is fixed here, at the moment of opening. Following
      // the focused pane while the form is open would re-seed it on background
      // focus changes, under what the person is typing.
      setState({
        open: true,
        paneId: detail?.paneId ?? useStore.getState().focusedPaneId ?? undefined,
        fromPaneMenu: detail?.fromPaneMenu === true,
      });
    };
    window.addEventListener('solu:open-schedule-dialog', handler);
    return () => window.removeEventListener('solu:open-schedule-dialog', handler);
  }, []);

  return (
    <ScheduleDialog
      open={state.open}
      editing={null}
      initialPaneId={state.paneId}
      fromPaneMenu={state.fromPaneMenu}
      onClose={() => setState({ open: false, fromPaneMenu: false })}
    />
  );
}
