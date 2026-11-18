
import { useEffect, useRef, useState } from 'react';
import { useSettings, type SettingsTab } from '../settings';
import { useStore } from '../store';
import { useOperatorFeatures } from '../entitlements';
import { slotList } from '../extensions';
import { useDraggableDialog } from './useDraggableDialog';
import { useFocusTrap } from './useFocusTrap';
import { useIsMobileDevice } from './useIsMobileView';
import { SidebarCustomizer } from './sidebar/SidebarCustomizer';
import { AppearanceTab } from './settings/AppearanceTab';
import { PanesTab } from './settings/PanesTab';
import { DevicesTab } from './settings/DevicesTab';
import { AccountTab } from './settings/AccountTab';
import { NotificationsVoiceTab, useNotificationsVoiceVisible } from './settings/NotificationsVoiceTab';
import { PrivacySecurityTab } from './settings/PrivacySecurityTab';
import { PhoneEncryptionSetting, privateRelayCandidateInView } from './settings/RemoteDevicesSetting';
import { ServicesTab } from './settings/ServicesTab';
import { CORE_SETTINGS_TABS } from './settings/settingsTabs';
import { SettingsSearchBox, landOnSetting, useSettingsSearch, type SettingsSearchResult } from './settings/SettingsSearch';
import { ErrorBoundary } from './ErrorBoundary';

// THE Settings dialog — every settings surface lives here as a tab on the
// left nav rail, and every entry point (topbar gear, sidebar right-click
// "Customize sidebar…", section settings links) deep-links to its
// tab via useSettings.openSettings(tab). All controls apply live; the window
// drags by its header and resizes from the corner grip (useDraggableDialog
// pins it to fixed coordinates so both track the cursor 1:1).
// Voice and Alerts stay as small anchored topbar popovers — quick toggles,
// not settings pages. HotkeyHelp stays a reference overlay.

const TABS = CORE_SETTINGS_TABS;

// Team-only service settings.
// Feature modules add their own tabs (extensions.ts).
const TEAM_ONLY_TABS = new Set<SettingsTab>(['tasks']);

export function SettingsModal() {
  const open = useSettings((s) => s.open);
  const dialogOpen = useStore((s) => !!s.dialog);
  const tab = useSettings((s) => s.tab);
  const setTab = useSettings((s) => s.setTab);
  const close = useSettings((s) => s.closeSettings);
  const isMobile = useIsMobileDevice();
  const team = useOperatorFeatures();
  const notificationsVisible = useNotificationsVoiceVisible();
  // Tabs registered by optional features (extensions.ts), each placed after
  // a core tab. The list is fixed for the page's lifetime, so the hook order
  // is stable.
  const featureTabs = slotList('settingsTabs');
  const featureVisible = featureTabs.map((t) => t.useVisible());

  const query = useSettings((s) => s.query);
  const setQuery = useSettings((s) => s.setQuery);
  const searchRef = useRef<HTMLInputElement>(null);
  const bodyRef = useRef<HTMLDivElement>(null);
  const [pendingLand, setPendingLand] = useState<{ label?: string } | null>(null);

  const { cardRef, cardStyle, dragHandleProps } = useDraggableDialog(open);
  useFocusTrap(cardRef, open, dialogOpen);

  useEffect(() => {
    if (!open) {
      setQuery('');
      return;
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        close();
        return;
      }
      // "/" (outside a text field) or Ctrl/Cmd+F focuses search (ST-281).
      const el = document.activeElement as HTMLElement | null;
      const typing = !!el && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName));
      const slash = e.key === '/' && !e.ctrlKey && !e.metaKey && !e.altKey && !typing;
      const find = (e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'f';
      if (slash || find) {
        e.preventDefault();
        searchRef.current?.focus();
        searchRef.current?.select();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open, close, setQuery]);

  // Account, Notifications & voice and Devices & updates hold desktop rows
  // (sign-in, voice, installer, phone pairing, updates) that don't apply on a
  // phone, so a phone sees only Privacy & security, holding just this
  // browser's end-to-end encryption switch, and only when that applies.
  const phoneEncryption = isMobile && privateRelayCandidateInView() !== null;
  const shown = (key: SettingsTab): boolean => {
    switch (key) {
      // Presentation mode applies to every desktop install, so Account always shows.
      case 'account': return !isMobile;
      case 'notifications': return !isMobile && notificationsVisible;
      case 'devices': return !isMobile;
      case 'privacy': return !isMobile || phoneEncryption;
      default: return true;
    }
  };
  const coreTabs = TABS.filter((t) => shown(t.key) && (team || !TEAM_ONLY_TABS.has(t.key)));
  const tabs: { key: SettingsTab; label: string }[] = [];
  const placed = new Set<number>();
  // A feature tab follows its anchor, which may be another feature's tab;
  // one whose anchor isn't shown goes at the end.
  const placeAfter = (key: SettingsTab) => {
    featureTabs.forEach((f, i) => {
      if (f.after !== key || !featureVisible[i] || placed.has(i)) return;
      placed.add(i);
      tabs.push(f);
      placeAfter(f.key);
    });
  };
  for (const t of TABS) {
    if (!coreTabs.includes(t)) continue;
    tabs.push(t);
    placeAfter(t.key);
  }
  featureTabs.forEach((f, i) => { if (featureVisible[i] && !placed.has(i)) { placed.add(i); tabs.push(f); } });
  // A deep link to a hidden tab lands on the first visible one instead of an
  // empty panel.
  // The old two-row "Claude sessions" tab now lives at the bottom of
  // Terminal & panes, so its deep links land there.
  // Settings › Devices was split into four tabs; on a phone the encryption
  // switch it used to hold now lives in Privacy & security.
  const wanted = tab === 'claude' ? 'panes' : tab === 'devices' && isMobile ? 'privacy' : tab;
  const activeTab = tabs.some((t) => t.key === wanted) ? wanted : tabs[0].key;
  // Search hides what Devices & updates hides (DevicesTab.tsx).
  const shellAutoApplyPaused = useStore((s) => s.updateStatus?.shellAutoApplyPaused === true);
  const results = useSettingsSearch(query, tabs, isMobile, shellAutoApplyPaused);

  // Land on the chosen row once its tab has rendered.
  useEffect(() => {
    if (!pendingLand) return;
    landOnSetting(bodyRef.current, pendingLand.label);
    setPendingLand(null);
  }, [pendingLand, activeTab]);

  const choose = (result: SettingsSearchResult) => {
    setQuery('');
    useSettings.getState().openSettings(result.target);
    setPendingLand({ label: result.landOn });
  };

  if (!open) return null;

  return (
    <div className="dialog-backdrop" onMouseDown={close}>
      <div
        ref={cardRef}
        style={cardStyle}
        className="dialog settings-modal"
        role="dialog"
        aria-label="Settings"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="dialog-header settings-drag" {...dragHandleProps}>
          <h2 className="dialog-title">Settings</h2>
          <button onClick={close} title="Close (Esc)" aria-label="Close">×</button>
        </div>
        <SettingsSearchBox ref={searchRef} query={query} onQuery={setQuery} results={results} onChoose={choose} />
        <div className="settings-layout">
          <nav className="settings-nav" role="tablist" aria-orientation="vertical">
            {tabs.map((t) => (
              <button
                key={t.key}
                type="button"
                role="tab"
                aria-selected={activeTab === t.key}
                className={`settings-nav-item${activeTab === t.key ? ' active' : ''}`}
                onClick={() => { setQuery(''); setTab(t.key); }}
              >
                {t.label}
              </button>
            ))}
          </nav>
          <div className="settings-body" role="tabpanel" ref={bodyRef}>
            <ErrorBoundary key={activeTab} label="settings" title="This settings section failed to load">
              {activeTab === 'appearance' && <AppearanceTab />}
              {activeTab === 'panes' && <PanesTab />}
              {activeTab === 'sidebar' && <SidebarCustomizer />}
              {activeTab === 'account' && <AccountTab />}
              {activeTab === 'notifications' && <NotificationsVoiceTab />}
              {activeTab === 'privacy' && !isMobile && <PrivacySecurityTab />}
              {activeTab === 'privacy' && isMobile && <PhoneEncryptionSetting />}
              {activeTab === 'devices' && <DevicesTab />}
              {activeTab === 'tasks' && <ServicesTab />}
              {featureTabs.map((f, i) => activeTab === f.key && featureVisible[i] && <f.Component key={f.key} />)}
            </ErrorBoundary>
          </div>
        </div>
      </div>
    </div>
  );
}
