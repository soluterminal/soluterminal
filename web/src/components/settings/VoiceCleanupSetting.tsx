import { useEffect, useRef, useState } from 'react';
import { api } from '../../api';
import type { VoiceLocalSettings } from '../../private/voice.api';
import { SettingsCheck, SettingsGroup, SettingsRow } from './SettingsRow';
import { useStore } from '../../store';
import { VoiceWordsSetting } from './VoiceWordsSetting';
import { VOICE_LANGS } from '../VoiceButton';
import type { SettingsTab } from '../../settings';

// The Settings tab holding the Voice group. The mic menu's "Voice settings…"
// link opens it, so the two can't drift apart.
export const VOICE_SETTINGS_TAB: SettingsTab = 'notifications';

export const VOICE_AUTO_SEND_LABEL = 'Send dictation automatically';
export const VOICE_AUTO_SEND_HINT =
  'On: in Claude Code and Codex panes, what you say is typed and Enter is pressed for you. ' +
  'In a shell it is only typed, so a command never runs unchecked. Off: it is typed for you to ' +
  'check, and you press Enter.';
export const VOICE_LANGUAGE_LABEL = 'Language';
export const VOICE_LANGUAGE_HINT =
  'The language you usually speak, so short clips aren’t mistaken for another one. ' +
  'Auto-detect guesses each time.';

export const VOICE_LOCAL_LABEL = 'Clean up dictation with my Claude plan';
export const VOICE_KEEP_MIC_LABEL =
  'Keep the microphone ready for 2 minutes after dictating (faster start; your system’s mic light stays on)';
export const VOICE_KEEP_MIC_HINT =
  'Off: the mic opens when you press and closes when you finish. On: it stays open between ' +
  'dictations so your first word isn’t cut off. Nothing is recorded or sent while it waits, but ' +
  'each dictation then starts with a split second from just before you press.';
export const VOICE_READY_TICK_LABEL = 'Tick when the mic is listening';
export const VOICE_READY_TICK_HINT =
  'A soft tick the moment recording starts. After a pause the mic takes a moment to open; ' +
  'start speaking at the tick so your first word is kept.';
// Why the row is on but not in use right now. Each says what happens instead,
// so the user knows dictation still works.
const REASONS: Record<string, string> = {
  not_installed: 'Needs Claude Code on this computer. Until then SoluTerminal cleans your dictation.',
  not_signed_in: 'Sign in to Claude Code in a pane to use this. Until then SoluTerminal cleans your dictation.',
  paused: "Paused for a few minutes after Claude Code didn't answer. SoluTerminal is cleaning your dictation meanwhile.",
};

export function voiceLocalHint(s: VoiceLocalSettings): string {
  if (s.enabled && !s.available && s.reason && REASONS[s.reason]) return REASONS[s.reason];
  if (!s.enabled) {
    return 'Off: SoluTerminal cleans your dictation, which counts toward your daily voice allowance.';
  }
  return (
    'Claude Haiku tidies your dictation on your own Claude Code login, so the terminal text it ' +
    'reads goes only to Anthropic, and it doesn’t count toward your daily voice allowance.'
  );
}

// Settings › Voice: every voice preference in one group — auto-send and
// language (also quick toggles in the mic menu), keeping the mic warm, the
// ready tick, cleanup and words to recognise. The voice module registers it
// only while voice is available (an account), like the mic itself.
// The cleanup choice is the daemon's (it runs the cleanup); a daemon without
// the endpoint answers 404 and that row stays hidden.
export function VoiceCleanupSetting() {
  const [settings, setSettings] = useState<VoiceLocalSettings | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const pendingRef = useRef(false);
  const keepWarmMs = useStore((s) => s.voiceKeepWarmMs);
  const setKeepWarm = useStore((s) => s.setVoiceKeepWarm);
  const readyTick = useStore((s) => s.voiceReadyTick);
  const setReadyTick = useStore((s) => s.setVoiceReadyTick);
  const autoSend = useStore((s) => s.voiceAutoSend);
  const setAutoSend = useStore((s) => s.setVoiceAutoSend);
  const language = useStore((s) => s.voiceLanguage);
  const setLanguage = useStore((s) => s.setVoiceLanguage);
  // A language set outside this list (by editing local storage) stays
  // selectable rather than showing as Auto-detect.
  const languages = VOICE_LANGS.some((l) => l.code === language)
    ? VOICE_LANGS
    : [...VOICE_LANGS, { code: language, label: language, name: language }];

  useEffect(() => {
    let mounted = true;
    void api
      .getVoiceLocal()
      .then((s) => {
        if (mounted && s) setSettings(s);
      })
      .catch(() => {
        /* older or unreachable daemon: keep the row hidden */
      });
    return () => {
      mounted = false;
    };
  }, []);

  const change = async (next: boolean) => {
    if (!settings || pendingRef.current) return;
    pendingRef.current = true;
    setPending(true);
    setError(null);
    const previous = settings;
    setSettings({ ...previous, enabled: next });
    try {
      setSettings(await api.setVoiceLocal(next));
    } catch (e) {
      setSettings(previous);
      setError(`Couldn't save the voice setting: ${String(e)}`);
    } finally {
      pendingRef.current = false;
      setPending(false);
    }
  };

  return (
    <SettingsGroup title="Voice">
      <SettingsCheck
        label={VOICE_AUTO_SEND_LABEL}
        checked={autoSend}
        onChange={setAutoSend}
        hint={VOICE_AUTO_SEND_HINT}
      />
      <SettingsRow label={VOICE_LANGUAGE_LABEL} hint={VOICE_LANGUAGE_HINT}>
        <select
          className="settings-select"
          value={language}
          onChange={(e) => setLanguage(e.target.value)}
          aria-label="Voice language"
        >
          {languages.map((l) => (
            <option key={l.code || 'auto'} value={l.code}>{l.name}</option>
          ))}
        </select>
      </SettingsRow>
      <SettingsCheck
        label={VOICE_KEEP_MIC_LABEL}
        checked={keepWarmMs > 0}
        onChange={setKeepWarm}
        hint={VOICE_KEEP_MIC_HINT}
      />
      <SettingsCheck
        label={VOICE_READY_TICK_LABEL}
        checked={readyTick}
        onChange={setReadyTick}
        hint={VOICE_READY_TICK_HINT}
      />
      {settings && (
        <SettingsCheck
          label={VOICE_LOCAL_LABEL}
          checked={settings.enabled}
          disabled={pending}
          onChange={(next) => void change(next)}
          hint={voiceLocalHint(settings)}
        />
      )}
      <VoiceWordsSetting />
      {error && <p className="settings-row-hint settings-error">{error}</p>}
    </SettingsGroup>
  );
}
