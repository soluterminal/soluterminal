import { useStore, terminalSessionKey } from '../store';
import { currentRoutingScope } from '../api';
import { TopbarPopover } from './TopbarPopover';
import { IconRefresh, IconChevronDown } from './ui/icons';

// "Refresh view" — the merged realign + reflow control (was two loose buttons).
//
// The MAIN button is the idempotent primitive: realign every visible pane to
// re-assert this device's width and repaint. It fixes the garble you get after
// an app update or a takeover from another device, and it's safe to click any
// number of times — that's the "something looks off, just hit refresh" gesture.
//
// The CARET holds Reflow, kept deliberately SEPARATE because it isn't idempotent:
// it sends Ctrl+O, which TOGGLES Claude's transcript view, and nothing tracks
// whether that view is currently on. Folding it into the plain click could
// toggle you OUT of a good view or strand you in read-only transcript — so it
// stays a labelled, opt-in menu item, gated on a focused Claude pane.

export function RefreshViewButton() {
  const realignAllPanes = useStore((s) => s.realignAllPanes);
  // Enable-gate for reflow: the focused pane must be running Claude — Ctrl+O
  // means other things to bash/vim. Primitive selector so this never hands
  // React a fresh reference per store tick.
  const reflowEnabled = useStore(
    (s) => (s.focusedPaneId ? s.panes[s.focusedPaneId]?.hasClaude === true : false),
  );

  const reflowFocusedPane = () => {
    const st = useStore.getState();
    const id = st.focusedPaneId;
    if (!id) return;
    // Order matters: Ctrl+O reprints at whatever width the PTY has when it
    // fires, so first win the sizing election and force this view's width onto
    // the PTY (realign), give the SIGWINCH + repaint a beat to land, then toggle
    // Claude's transcript view.
    // Terminal handlers are registered under the view-session key, not the
    // bare pane id.
    const key = terminalSessionKey(currentRoutingScope(), id);
    st.realignPane(key);
    window.setTimeout(() => {
      useStore.getState().senders[key]?.('\x0f');
    }, 800);
  };

  // Close the popover the same way the other topbar menus do — a synthetic body
  // click that TopbarPopover's outside-close listener sees.
  const closeFromInside = () =>
    document.body.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));

  return (
    <div className="chrome-split">
      <button
        type="button"
        className="chrome-btn chrome-split-main"
        onClick={() => realignAllPanes()}
        title="Refresh view — realign panes to fix garbled / misaligned text after an app update or a takeover from another device (Alt+R)"
        aria-label="Refresh view (realign panes)"
      >
        <IconRefresh />
      </button>
      <TopbarPopover
        minWidth={248}
        align="right"
        trigger={({ toggle, open }) => (
          <button
            type="button"
            className="chrome-btn chrome-split-caret"
            onClick={toggle}
            title="More view fixes — reflow the focused conversation"
            aria-label="More view fixes"
            aria-expanded={open}
          >
            <IconChevronDown size={13} />
          </button>
        )}
      >
        <div className="topbar-menu-row">
          <span className="topbar-menu-label">Reflow conversation</span>
          <button
            type="button"
            className="topbar-menu-btn"
            disabled={!reflowEnabled}
            onClick={() => { reflowFocusedPane(); closeFromInside(); }}
          >
            Rewrap
          </button>
        </div>
        <div className="topbar-menu-hint">
          {reflowEnabled
            ? 'Re-renders the focused Claude pane’s history at this width. Opens the transcript view; run again to return to typing.'
            : 'Focus a Claude pane to enable this.'}
        </div>
      </TopbarPopover>
    </div>
  );
}
