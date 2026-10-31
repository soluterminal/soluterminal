import { VoiceAutoSendToggle, VoiceLanguageSelect } from './VoiceButton';
import { VoiceHistoryInline } from './VoiceHistoryButton';
import { TopbarPopover } from './TopbarPopover';
import { IconChevronDown } from './ui/icons';
import { useSettings } from '../settings';
import { VOICE_SETTINGS_TAB } from './settings/VoiceCleanupSetting';

// Voice settings popover — the options half of the mic split-button. A caret
// attached to the mic (shared shell, hairline divider) opens auto-send toggle,
// language select, and history. Daily use is hold-to-talk; these three are
// "configure once" or "review occasionally" controls. Every voice setting,
// these two included, lives in Settings › Voice; the last row opens it.

export function VoiceConfigMenu() {
  return (
    <TopbarPopover
      minWidth={300}
      align="right"
      trigger={({ toggle, open }) => (
        <button
          type="button"
          className="chrome-btn chrome-split-caret"
          onClick={toggle}
          title="Voice settings (auto-send, language, history)"
          aria-label="Voice settings"
          aria-expanded={open}
        >
          <IconChevronDown size={13} />
        </button>
      )}
    >
      {({ close }) => (
        <>
          <div className="topbar-menu-row">
            <span className="topbar-menu-label">Auto-send</span>
            <VoiceAutoSendToggle />
          </div>
          <div className="topbar-menu-row">
            <span className="topbar-menu-label">Language</span>
            <VoiceLanguageSelect />
          </div>
          <div className="topbar-menu-sep" />
          <VoiceHistoryInline />
          <div className="topbar-menu-sep" />
          <button
            type="button"
            className="help-menu-item"
            onClick={() => {
              close();
              useSettings.getState().openSettings(VOICE_SETTINGS_TAB);
            }}
          >
            Voice settings…
          </button>
        </>
      )}
    </TopbarPopover>
  );
}
