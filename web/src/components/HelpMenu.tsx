import { useEffect, useRef } from 'react';
import type { KeyboardEvent as ReactKeyboardEvent } from 'react';
import { TopbarPopover } from './TopbarPopover';
import { IconHelp } from './ui/icons';
import { useStore } from '../store';
import { openInSystemBrowser } from '../tauri-browser';
import { SITE_HELP_URL } from '../lib/siteLinks';
import { openFeedback } from '../lib/feedbackReport';

// The top-bar "?" — a small help menu rather than straight into the full
// shortcut sheet, which is a lot to land on for someone new. The "?" key still
// opens the sheet directly (App.tsx, help.toggle).

interface Props {
  onOpenGettingStarted?: () => void;
  onOpenShortcuts?: () => void;
}

interface Item {
  id: string;
  label: string;
  hint?: string;
  shortcut?: string;
  quiet?: boolean;
  run: (event?: Event) => void;
}

function MenuList({ items, close }: { items: Item[]; close: (refocus?: boolean) => void }) {
  const listRef = useRef<HTMLDivElement | null>(null);

  const buttons = () =>
    Array.from(listRef.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]') ?? []);

  // Menu-button pattern: opening the menu moves focus to its first item.
  useEffect(() => {
    buttons()[0]?.focus();
  }, []);

  const onKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const all = buttons();
    const at = all.indexOf(document.activeElement as HTMLButtonElement);
    let next = -1;
    if (e.key === 'ArrowDown') next = at < 0 ? 0 : (at + 1) % all.length;
    else if (e.key === 'ArrowUp') next = at < 0 ? all.length - 1 : (at - 1 + all.length) % all.length;
    else if (e.key === 'Home') next = 0;
    else if (e.key === 'End') next = all.length - 1;
    else if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      close(true);
      return;
    } else if (e.key === 'Tab') {
      close(false);
      return;
    }
    if (next >= 0) {
      e.preventDefault();
      all[next]?.focus();
    }
  };

  return (
    <div ref={listRef} className="help-menu" role="menu" aria-label="Help" onKeyDown={onKeyDown}>
      {items.map((it) => (
        <div key={it.id} className="help-menu-group">
          {it.quiet && <div className="topbar-menu-sep" role="separator" />}
          <button
            type="button"
            role="menuitem"
            tabIndex={-1}
            className={`help-menu-item${it.quiet ? ' is-quiet' : ''}`}
            title={it.hint}
            onClick={(e) => {
              close(false);
              it.run(e.nativeEvent);
            }}
          >
            <span>{it.label}</span>
            {it.shortcut && <kbd>{it.shortcut}</kbd>}
          </button>
        </div>
      ))}
    </div>
  );
}

export function HelpMenu({ onOpenGettingStarted, onOpenShortcuts }: Props) {
  const triggerRef = useRef<HTMLButtonElement | null>(null);

  const items: Item[] = [
    ...(onOpenGettingStarted
      ? [{
          id: 'start',
          label: 'Getting started',
          hint: 'Show the welcome tour again',
          run: onOpenGettingStarted,
        }]
      : []),
    {
      id: 'help',
      label: 'Help ↗',
      hint: 'Open the help pages in your browser',
      run: (event) => { void openInSystemBrowser(SITE_HELP_URL, event); },
    },
    ...(onOpenShortcuts
      ? [{
          id: 'shortcuts',
          label: 'Keyboard shortcuts',
          shortcut: '?',
          run: onOpenShortcuts,
        }]
      : []),
    {
      id: 'feedback',
      label: 'Send feedback',
      hint: 'Report a problem or share an idea',
      run: () => openFeedback(),
    },
    {
      id: 'bundle',
      label: 'Create support bundle…',
      hint: 'Save a file of logs and app details to send with a problem report',
      quiet: true,
      run: () => useStore.getState().setSupportBundleOpen(true),
    },
  ];

  return (
    <TopbarPopover
      align="right"
      minWidth={200}
      trigger={({ open, toggle }) => (
        <button
          ref={triggerRef}
          type="button"
          className={`chrome-btn${open ? ' active' : ''}`}
          onClick={toggle}
          title="Help"
          aria-label="Help"
          aria-haspopup="menu"
          aria-expanded={open}
        >
          <IconHelp />
        </button>
      )}
    >
      {({ close }) => (
        <MenuList
          items={items}
          close={(refocus) => {
            close();
            if (refocus) triggerRef.current?.focus();
          }}
        />
      )}
    </TopbarPopover>
  );
}
