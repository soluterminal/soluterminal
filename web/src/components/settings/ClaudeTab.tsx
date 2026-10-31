import { useStore } from '../../store';
import { NATIVE_METABAR_ORDER } from '../../native/metaBarFields';
import { SettingsGroup, SettingsCheck, SettingsRow } from './SettingsRow';
import type { RecoveryResumeMode } from '../../types';
import { useFocusEnabled } from '../../lib/focusAvailability';

// Agent-session behavior: default Claude renderer, shared Focus features and
// vitals, and what panes do after a WSL restart.

export function ClaudeTab() {
  const recoverySettings = useStore((s) => s.recoverySettings);
  const setRecoveryResumeMode = useStore((s) => s.setRecoveryResumeMode);
  const nativeSettings = useStore((s) => s.nativeSettings);
  const setNativePromptSuggestions = useStore((s) => s.setNativePromptSuggestions);
  const setNativeDefaultClaudeMode = useStore((s) => s.setNativeDefaultClaudeMode);
  const nativeMetaBarFields = useStore((s) => s.nativeMetaBarFields);
  const setNativeMetaBarField = useStore((s) => s.setNativeMetaBarField);
  const showPromptHistory = useStore((s) => s.showPromptHistory);
  const setShowPromptHistory = useStore((s) => s.setShowPromptHistory);
  const focusEnabled = useFocusEnabled();

  return (
    <>
      <SettingsGroup title="Sessions">
        {/* Default renderer for NEW Claude sessions. Daemon-stored so every
            device agrees; the new-session dialog preselects it and the per-
            session choice (chips / mode toggle) always wins. */}
        {focusEnabled && <SettingsRow
          label="New Claude sessions"
          hint="Which renderer new sessions open in — Terminal (raw TUI) or Focus (structured chat). Any session can switch at any time."
        >
          <select
            className="settings-select"
            value={nativeSettings?.defaultClaudeMode ?? ''}
            onChange={(e) => void setNativeDefaultClaudeMode(e.target.value as '' | 'native')}
          >
            <option value="">Open in Terminal</option>
            <option value="native">Open in Focus</option>
          </select>
        </SettingsRow>}
        <SettingsRow
          label="After a restart"
          hint="What Claude panes do after your computer (or WSL) restarts. Ask me offers Resume on each pane. Don't resume leaves them paused until you choose Resume session in a pane's ⋯ menu."
        >
          <select
            className="settings-select"
            value={recoverySettings?.resumeAfterRestart ?? 'manual'}
            onChange={(e) => void setRecoveryResumeMode(e.target.value as RecoveryResumeMode)}
          >
            <option value="manual">Ask me</option>
            <option value="auto">Resume automatically</option>
            <option value="paused">Don't resume</option>
          </select>
        </SettingsRow>
      </SettingsGroup>
      <SettingsGroup title="Prompts">
        {focusEnabled && <SettingsCheck
          label="Suggest next prompt (Focus)"
          checked={nativeSettings?.promptSuggestions ?? false}
          onChange={(v) => void setNativePromptSuggestions(v)}
          hint="Ghost-text suggestion in the Focus composer (Tab to accept). Adds one cached background model call per turn. Applies to new and restarted Focus panes."
        />}
        {/* The prompt-history strip at the top of Claude panes (both Term and
            Focus). Presentational client toggle, applied live to every pane. */}
        <SettingsCheck
          label="Show prompt history strip"
          checked={showPromptHistory}
          onChange={setShowPromptHistory}
          hint="Your recent prompts in a collapsible strip at the top of each Claude pane. Hover a row to copy it."
        />
      </SettingsGroup>
      {/* Native bottom-bar vitals: which read-only session indicators show in
          the NativeMetaBar's right cluster. Purely presentational client
          toggles (they gate already-available data), applied live. */}
      {focusEnabled && <SettingsGroup title="Focus vitals">
        <div className="settings-row settings-row-check">
          <div className="native-vitals-toggles">
            {NATIVE_METABAR_ORDER.map(([key, label]) => (
              <label
                key={key}
                className="native-vitals-toggle"
                title={`Show ${label.toLowerCase()} in the Focus pane's bottom status bar`}
              >
                <input
                  type="checkbox"
                  checked={nativeMetaBarFields[key]}
                  onChange={(e) => setNativeMetaBarField(key, e.target.checked)}
                />
                {label}
              </label>
            ))}
          </div>
          <p className="settings-row-hint">
            Applies live to Claude and Codex Focus panes. Metrics a provider does not supply stay hidden.
          </p>
        </div>
      </SettingsGroup>}
    </>
  );
}
