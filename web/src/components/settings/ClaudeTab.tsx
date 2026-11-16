import { useStore } from '../../store';
import { SettingsGroup, SettingsCheck, SettingsRow } from './SettingsRow';
import type { RecoveryResumeMode } from '../../types';

// Agent-session behavior: the prompt-history strip and what panes do after a
// WSL restart.

export function ClaudeTab() {
  const recoverySettings = useStore((s) => s.recoverySettings);
  const setRecoveryResumeMode = useStore((s) => s.setRecoveryResumeMode);
  const showPromptHistory = useStore((s) => s.showPromptHistory);
  const setShowPromptHistory = useStore((s) => s.setShowPromptHistory);

  return (
    <>
      <SettingsGroup title="Sessions">
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
        {/* The prompt-history strip at the top of Claude panes. Presentational
            client toggle, applied live to every pane. */}
        <SettingsCheck
          label="Show prompt history strip"
          checked={showPromptHistory}
          onChange={setShowPromptHistory}
          hint="Your recent prompts in a collapsible strip at the top of each Claude pane. Hover a row to copy it."
        />
      </SettingsGroup>
    </>
  );
}
