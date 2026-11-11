import { useStore } from '../store';
import { openInbox, openInboxAt } from '../components/inbox/InboxPanel';
import { markFeedbackEventRead } from './useFeedbackInbox';

// Push notifications carry their target in the paneId slot (pusher.go):
// a real pane id, "sched:<scheduleId>" for an automation,
// "agent:<agentId>" for a managed agent, or "feedback:<reportNumber>" for an
// update on a report the person sent (cloud/feedback_loop.go). Pane ids are
// revealed by the caller; this routes the others, which used to wait 10 s for
// a pane named "agent:…" that never exists.
//
// - A feedback tap opens the Inbox on that report's thread and marks it read.
// - An agent tap opens the Inbox on that agent's pending decision.
// - A task automation's tap opens the Inbox on its newest report.
// - Any other automation (a prompt typed into a pane, a built-in agent) has
//   no Inbox row, so its tap keeps opening the schedule's run history.

const SCHEDULE_WAIT_MS = 10_000;

function openHistory(id: string) {
  window.dispatchEvent(new CustomEvent('solu:open-schedule-history', { detail: { id } }));
}

function routeSchedule(id: string) {
  const decide = (): boolean => {
    const sched = useStore.getState().schedules.find((s) => s.id === id);
    if (!sched) return false;
    if (sched.kind === 'task') openInboxAt({ schedule: id });
    else openHistory(id);
    return true;
  };
  if (decide()) return;
  // Cold start: the schedule list has not loaded yet. Decide the moment it
  // does; if it never names this schedule, fall back to the history dialog,
  // which explains a missing schedule on its own.
  let done = false;
  const finish = () => { done = true; unsub(); window.clearTimeout(timer); };
  const unsub = useStore.subscribe((state, prev) => {
    if (done || state.schedules === prev.schedules) return;
    if (decide()) finish();
  });
  const timer = window.setTimeout(() => {
    if (done) return;
    finish();
    openHistory(id);
  }, SCHEDULE_WAIT_MS);
}

const INBOX_TAP_PREFIXES = ['sched:', 'agent:', 'feedback:'];

/** True for a target routeNotificationTap handles (not a pane id). */
export function isInboxTapTarget(target: string): boolean {
  return INBOX_TAP_PREFIXES.some((p) => target.startsWith(p));
}

/** Routes a non-pane notification target. Returns false for a pane id. */
export function routeNotificationTap(target: string): boolean {
  if (target.startsWith('feedback:')) {
    const raw = target.slice('feedback:'.length);
    const number = /^[1-9]\d*$/.test(raw) ? Number(raw) : 0;
    if (number > 0) {
      openInbox('feed', number);
      // Opening the thread is reading it, the same as picking its row.
      markFeedbackEventRead(number);
    } else {
      // No report to show (the admin alert carries 0): the feed itself.
      openInbox();
    }
    return true;
  }
  if (target.startsWith('sched:')) {
    routeSchedule(target.slice('sched:'.length));
    return true;
  }
  if (target.startsWith('agent:')) {
    openInboxAt({ agent: target.slice('agent:'.length) });
    return true;
  }
  return false;
}

/**
 * The service worker's messages about a tapped notification, for a window
 * where the app is mounted. Before it hands over a target that any window can
 * show, the worker asks which windows are listening (sw.js tapReady); the
 * answer on the port is what tells it this one can.
 */
export function onServiceWorkerTapMessage(e: MessageEvent): void {
  const d = e.data as { type?: string; paneId?: string } | null;
  if (!d) return;
  if (d.type === 'solu:notification-tap-ready') {
    e.ports[0]?.postMessage('ready');
    return;
  }
  if (d.type !== 'solu:notification-tap' || !d.paneId) return;
  if (routeNotificationTap(d.paneId)) return;
  const st = useStore.getState();
  if (st.panes[d.paneId]) void st.revealPane(d.paneId);
}
