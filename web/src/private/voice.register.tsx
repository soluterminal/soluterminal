// Voice dictation (soluvoice): the mic in the top bar and the phone bar, the
// global recorder and its status, the Alt+S / Alt+C / Esc shortcuts, dictation
// in the composer and into terminal panes, and its Settings › Notifications & voice section.
// Public since 2026-10-27 (soluvoice is Apache-2.0).
import type { WebExtension } from '../extensions';
import { useAccountFeatures } from '../entitlements';
import { useStore } from '../store';
import { VoiceButton, VoiceAutoSendToggle, VoiceLanguageSelect } from '../components/VoiceButton';
import { VoiceConfigMenu } from '../components/VoiceConfigMenu';
import { VoiceSessionStatus } from '../components/VoiceSessionStatus';
import { VOICE_SETTINGS_TAB, VoiceCleanupSetting } from '../components/settings/VoiceCleanupSetting';
import { useComposerVoice } from '../components/composer/useComposerVoice';
import { notePaneTyped } from '../lib/voiceGuards';
import { ComposerVoiceRecording } from './ComposerVoiceRecording';
import { PaneDictationChip } from './PaneDictationChip';
import { createVoiceKeys } from './voice.keys';

// Dictation needs a free account (cloud voice); without one none of this shows.

// The headless root recorder (Alt+S target when no visible mic is mounted)
// and the global session status.
function VoiceRoot() {
  const voiceAllowed = useAccountFeatures();
  if (!voiceAllowed) return null;
  return (
    <>
      <VoiceButton headless />
      <div className="voice-global-status"><VoiceSessionStatus global /></div>
    </>
  );
}

// Input — voice split-button: mic body (hold-to-talk) + attached options
// caret, one shared shell. The only "produce input" control, so it's its own
// cluster and the natural mid-row boundary.
function TopbarVoice() {
  const voiceAllowed = useAccountFeatures();
  if (!voiceAllowed) return null;
  return (
    <>
      <span className="topbar-divider" aria-hidden="true" />
      <div className="topbar-group">
        <div className="chrome-split">
          <VoiceButton extraClass="chrome-split-main" />
          <VoiceConfigMenu />
        </div>
      </div>
    </>
  );
}

function MobileBarVoice() {
  const voiceAllowed = useAccountFeatures();
  if (!voiceAllowed) return null;
  return (
    <div className="bottombar-voice">
      <VoiceButton />
      <VoiceAutoSendToggle />
      <VoiceLanguageSelect />
    </div>
  );
}

// Three narrow selectors (boolean + primitives) so each pane only re-renders
// when its OWN dictation status flips, not on every phase change of someone
// else's session.
function usePaneDictation(paneId: string) {
  const dictating = useStore((s) => s.voiceSession?.paneId === paneId);
  const phase = useStore((s) => (s.voiceSession?.paneId === paneId ? s.voiceSession.phase : null));
  // Slide-to-cancel armed flag. Flips the chip into a red "Release to
  // cancel" state so the user gets an obvious commitment cue before
  // lifting their finger/pointer. Only meaningful during 'recording'.
  const aboutToCancel = useStore((s) => (s.voiceSession?.paneId === paneId ? !!s.voiceSession.aboutToCancel : false));
  return dictating ? { phase: phase ?? '', aboutToCancel } : null;
}

const voice: WebExtension = {
  globalKeys: [createVoiceKeys],
  rootOverlays: [VoiceRoot],
  topbarItems: [TopbarVoice],
  mobileBarItems: [MobileBarVoice],
  // The Settings voice group, like the mic, needs an account.
  settingsSections: [{ tab: VOICE_SETTINGS_TAB, useVisible: useAccountFeatures, Component: VoiceCleanupSetting }],
  shortcutGroups: [{
    title: 'Voice',
    items: [
      { keys: ['Alt', 'S'], label: 'Hold to talk, release to type what you said. Or tap once to start and again to stop.' },
      { keys: ['Esc'], label: 'Stop recording and keep the words' },
      { keys: ['Alt', 'C'], label: 'Stop recording and throw the words away' },
    ],
    useVisible: useAccountFeatures,
  }],
  composerVoice: { useComposerVoice, Recording: ComposerVoiceRecording },
  paneDictation: {
    usePaneDictation,
    isDictating: (paneId) => useStore.getState().voiceSession?.paneId === paneId,
    noteTyped: notePaneTyped,
    Chip: PaneDictationChip,
  },
};

export default voice;
