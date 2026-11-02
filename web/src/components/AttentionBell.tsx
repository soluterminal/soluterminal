import { AttentionInbox, useAttentionBadge, useAttentionItems } from './AttentionInbox';
import { TopbarPopover } from './TopbarPopover';
import { IconBell } from './ui/icons';

// Topbar attention bell — the always-present "which panes need ME right
// now?" surface. Same single source of truth as the statusbar "N needs
// you" pill and the mobile sheet (useAttentionBadge / useAttentionItems),
// so every surface shows the same count by construction. Distinct from
// the AlertsMenu next door: that is alert-channel settings + a history of
// past toasts; this is live prompts waiting for a human, with the
// LLM-graded risk chip + summary on each row.

// List body is a separate component so useAttentionItems only mounts
// while the panel is open (TopbarPopover renders children only when
// open) — closed, the bell costs one badge selector, nothing more.
function AttentionBellList() {
  const items = useAttentionItems();
  // Row clicks reveal the pane; close the panel the same way AlertsMenu
  // does — a synthetic body click that the popover's outside-close sees.
  const close = () =>
    document.body.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
  if (items.length === 0) {
    return <div className="notif-center-empty">Nothing is waiting on you</div>;
  }
  return <AttentionInbox items={items} onSelect={close} />;
}

export function AttentionBell() {
  const { count, hasChoice } = useAttentionBadge();
  const title =
    count === 0 ? 'Nothing is waiting on you'
    : count === 1 ? '1 item is waiting on you'
    : `${count} items are waiting on you`;
  return (
    <TopbarPopover
      minWidth={300}
      align="right"
      trigger={({ toggle, open }) => (
        <button
          type="button"
          className={`chrome-btn attention-bell${count > 0 ? ' attention-bell-active' : ''}${hasChoice ? ' attention-bell-choice' : ''}`}
          onClick={toggle}
          title={title}
          aria-label={title}
          aria-expanded={open}
        >
          <IconBell />
          {count > 0 && (
            <span className="attention-bell-count" aria-hidden="true">
              {count > 9 ? '9+' : count}
            </span>
          )}
        </button>
      )}
    >
      <div className="topbar-menu-row">
        <span className="topbar-menu-label">Waiting on you</span>
      </div>
      <AttentionBellList />
    </TopbarPopover>
  );
}
