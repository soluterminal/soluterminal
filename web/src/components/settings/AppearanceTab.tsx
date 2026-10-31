import { useStore, type AppThemePref } from '../../store';
import { THEMES } from '../../themes';
import { SettingsGroup, SettingsRow } from './SettingsRow';
import { NumberInput } from './NumberInput';
import { useFocusEnabled } from '../../lib/focusAvailability';

// Theme + type-size controls. Everything applies live to mounted panes, so
// users can drag the Settings window aside and watch the effect as they tweak.

export function AppearanceTab() {
  const appTheme = useStore((s) => s.appTheme);
  const setAppTheme = useStore((s) => s.setAppTheme);
  const terminalThemeName = useStore((s) => s.terminalThemeName);
  const terminalThemeExplicit = useStore((s) => s.terminalThemeExplicit);
  const setTerminalThemeName = useStore((s) => s.setTerminalThemeName);
  const setTerminalThemeAuto = useStore((s) => s.setTerminalThemeAuto);
  const terminalFontSize = useStore((s) => s.terminalFontSize);
  const setTerminalFontSize = useStore((s) => s.setTerminalFontSize);
  const terminalLineHeight = useStore((s) => s.terminalLineHeight);
  const setTerminalLineHeight = useStore((s) => s.setTerminalLineHeight);
  const focusFontOffset = useStore((s) => s.focusFontOffset);
  const setFocusFontOffset = useStore((s) => s.setFocusFontOffset);
  const uiScale = useStore((s) => s.uiScale);
  const setUiScale = useStore((s) => s.setUiScale);
  // Focus (the structured chat renderer) is retired for most installs; its
  // text offset row only shows where Focus can still open.
  const focusEnabled = useFocusEnabled();

  return (
    <>
      <SettingsGroup title="Appearance">
        <SettingsRow
          label="App theme"
          hint="Sidebar, top bar and dialogs. System follows your computer's setting. Terminal colours are set below."
        >
          <select
            className="settings-select"
            value={appTheme}
            onChange={(e) => setAppTheme(e.target.value as AppThemePref)}
          >
            <option value="dark">Dark</option>
            <option value="light">Light</option>
            <option value="system">System</option>
          </select>
        </SettingsRow>
      </SettingsGroup>
      <SettingsGroup title="Terminal theme">
        <SettingsRow
          label="Color theme"
          hint="Auto matches the app theme. Pick a scheme to always use it."
        >
          <select
            className="settings-select"
            value={terminalThemeExplicit ? terminalThemeName : 'auto'}
            onChange={(e) => {
              if (e.target.value === 'auto') setTerminalThemeAuto();
              else setTerminalThemeName(e.target.value);
            }}
          >
            <option value="auto">Auto — match app theme</option>
            {THEMES.map((t) => (
              <option key={t.name} value={t.name}>{t.name}</option>
            ))}
          </select>
        </SettingsRow>
      </SettingsGroup>
      <SettingsGroup title="Text size">
        {/* Font size: NumberInput keeps the typed draft and applies it once it
            is a valid 10-22px value (or clamps it on blur/Enter), so typing
            "18" over "13" works instead of clamping each keystroke. Applies to BOTH renderers: xterm gets
            the px directly; the Focus pane scales its prose ramp proportionally
            (px / 13 default) so the two modes stay consistent. */}
        <SettingsRow
          label="Font size"
          hint="Terminal text, 10–22 px."
        >
          <NumberInput
            label="Font size"
            min={10}
            max={22}
            step={1}
            live
            value={terminalFontSize}
            onCommit={setTerminalFontSize}
          />
          <span className="settings-unit">px</span>
        </SettingsRow>
        {/* Line height: clamped 1.0-1.8 by the store. Tighter is better for
            code review (more lines on screen); looser for long reading. */}
        <SettingsRow
          label="Line height"
          hint="Ratio, 1.0–1.8. Tighter fits more lines; looser reads easier."
        >
          <NumberInput
            label="Line height"
            min={1.0}
            max={1.8}
            step={0.1}
            live
            value={terminalLineHeight}
            onCommit={setTerminalLineHeight}
          />
        </SettingsRow>
        {/* Focus text offset: Focus is a proportional font (reads smaller than
            mono at the same px), so the default +0.4 keeps the two modes
            visually balanced; 0 makes Focus prose the same px as the Terminal.
            Store clamps 0–8. */}
        {focusEnabled && <SettingsRow
          label="Focus text offset"
          hint="How much larger Focus prose renders than the Terminal font (0 = same size, default +0.4 px)."
        >
          <input
            type="number"
            className="settings-input"
            min={0}
            max={8}
            step={0.1}
            value={focusFontOffset}
            onChange={(e) => {
              const n = parseFloat(e.target.value);
              if (!Number.isNaN(n)) setFocusFontOffset(n);
            }}
          />
          <span className="settings-unit">px</span>
        </SettingsRow>}
        {/* Sidebar size: zoom factor on the sidebar SECTIONS only — topbar,
            account footer, statusbar and panes stay at true size. Store clamps
            0.7–1.4. */}
        <SettingsRow
          label="Sidebar text size"
          hint="Scales the sidebar lists only, 0.7–1.4×. Panes and the top bar keep their size."
        >
          <NumberInput
            label="Sidebar text size"
            min={0.7}
            max={1.4}
            step={0.05}
            live
            value={uiScale}
            onCommit={setUiScale}
          />
          <span className="settings-unit">×</span>
        </SettingsRow>
      </SettingsGroup>
    </>
  );
}
