import { useStore } from '../store';
import { openInboxAt } from '../components/inbox/InboxPanel';

// Push notifications carry their target in the paneId slot (pusher.go):
// a real pane id, "sched:<scheduleId>" for an automation, or
// "agent:<agentId>" for a managed agent. Pane ids are revealed by the caller;
// this routes the other two, which used to wait 10 s for a pane named
// "agent:…" that never exists.
//
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

/** Routes a non-pane notification target. Returns false for a pane id. */
export function routeNotificationTap(target: string): boolean {
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
