import { useEffect, useState } from 'react';
import { api } from '../../api';
import { useStore } from '../../store';
import { useIsMobileDevice } from '../useIsMobileView';
import { SettingsGroup, SettingsCheck, SettingsRow } from './SettingsRow';
import { ClaudeTab } from './ClaudeTab';

// Layout is saved with the workspace. Applying a preset selects custom mode.

export function PanesTab() {
  const isMobile = useIsMobileDevice();
  const paneLayoutMode = useStore((s) => s.paneLayoutMode);
  const setPaneLayoutMode = useStore((s) => s.setPaneLayoutMode);
  const applyLayoutPreset = useStore((s) => s.applyLayoutPreset);
  const copyOnSelect = useStore((s) => s.copyOnSelect);
  const setCopyOnSelect = useStore((s) => s.setCopyOnSelect);
  const middleClickPaste = useStore((s) => s.middleClickPaste);
  const setMiddleClickPaste = useStore((s) => s.setMiddleClickPaste);
  const leafCountInLayout = useStore((s) => {
    const l = s.layout;
    if (!l) return 0;
    const walk = (n: typeof l): number => {
      if (!n) return 0;
      if (n.type !== 'split') return 1;
      return walk(n.a) + walk(n.b);
    };
    return walk(l);
  });

  return (
    <>
      <SettingsGroup title="Layout">
        <SettingsRow
          label="Pane layout"
          hint="Saved with this workspace. Auto balances the grid; dragging a divider or choosing a preset switches to Custom."
        >
          <select
            className="settings-select"
            value={paneLayoutMode}
            onChange={(e) => setPaneLayoutMode(e.target.value as 'auto' | 'free')}
          >
            <option value="auto">Auto grid</option>
            <option value="free">Custom</option>
          </select>
        </SettingsRow>
        {leafCountInLayout >= 2 && (
          <SettingsRow label="Arrange" hint="Rebuild the current workspace's pane arrangement.">
            <div className="settings-btn-row">
              <button
                type="button"
                className="settings-btn"
                onClick={() => applyLayoutPreset('cols')}
                title="All panes side-by-side as columns"
              >Cols</button>
              <button
                type="button"
                className="settings-btn"
                onClick={() => applyLayoutPreset('rows')}
                title="All panes stacked vertically as rows"
              >Rows</button>
              <button
                type="button"
                className="settings-btn"
                onClick={() => applyLayoutPreset('grid')}
                title="Closest-to-square grid"
              >Grid</button>
              {leafCountInLayout >= 3 && (
                <button
                  type="button"
                  className="settings-btn"
                  onClick={() => applyLayoutPreset('left-big')}
                  title="First pane on the left, others stacked on the right"
                >Left+stack</button>
              )}
            </div>
          </SettingsRow>
        )}
      </SettingsGroup>
      {/* Mouse copy/paste parity — X11 primary-selection convention. Mouse-only,
          so hidden on touch where there's no middle button and selection
          already copies via the long-press menu. */}
      {!isMobile && (
        <SettingsGroup title="Mouse">
          <SettingsCheck
            label="Copy on select"
            checked={copyOnSelect}
            onChange={setCopyOnSelect}
            hint="Selecting text in a terminal copies it immediately."
          />
          <SettingsCheck
            label="Middle-click paste"
            checked={middleClickPaste}
            onChange={setMiddleClickPaste}
            hint="Middle mouse button pastes the current selection."
          />
        </SettingsGroup>
      )}
      <PaneNamingGroup />
      {/* Session behaviour used to be its own two-row tab. */}
      <ClaudeTab />
    </>
  );
}

/** Opt-in AI names for agent panes still called "claude" or "codex". */
function PaneNamingGroup() {
  const [autoName, setAutoName] = useState<boolean | null>(null);
  const [error, setError] = useState('');
  useEffect(() => {
    let live = true;
    api.getPaneNaming().then((s) => { if (live) setAutoName(s.autoName); }, () => { if (live) setAutoName(null); });
    return () => { live = false; };
  }, []);
  if (autoName === null) return null;
  return (
    <SettingsGroup title="Naming">
      <SettingsCheck
        label="Name new agent panes with AI"
        checked={autoName}
        onChange={(next) => {
          setError('');
          setAutoName(next);
          api.setPaneNaming(next).catch((e: Error) => {
            setAutoName(!next);
            setError(e.message || 'Could not save the setting.');
          });
        }}
        hint={error || 'A Claude or Codex pane you open after turning this on, still called "claude" or "codex", is named once after its first prompt by your own Claude Code (Haiku), from its own conversation. Panes already open keep their names, and a name you or an agent gave is never replaced.'}
      />
    </SettingsGroup>
  );
}
