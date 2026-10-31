import { useState } from 'react';

// Commit-on-blur numeric input for Settings rows: typing an intermediate value
// must never fire a save per keystroke, because every control in Settings
// writes straight through to the daemon (there is no Save button anywhere).
// Enter commits, Escape resets, and the value is clamped into [min, max] on
// the way out so a hand-typed 9999 becomes the cap rather than an error.
//
// `step` sets the granularity the committed value is rounded to (default 1,
// i.e. whole numbers). `live` is for cheap local preferences (font size,
// sidebar zoom) that should apply while the user types or clicks the spinner:
// a draft that is already a valid in-range number is committed immediately,
// but an intermediate one ("1" on the way to "18", an empty box) is left alone
// until it becomes valid or the field loses focus.
export function NumberInput({ label, value, min, max, step = 1, placeholder, disabled, live = false, onCommit }: {
  label: string;
  value: number;
  min: number;
  max: number;
  step?: number;
  placeholder?: string;
  disabled?: boolean;
  live?: boolean;
  onCommit: (value: number) => void;
}) {
  const [draft, setDraft] = useState(String(value));
  const [syncedValue, setSyncedValue] = useState(value);
  // Settings sections mount after their async settings fetch resolves. A
  // passive mount effect can therefore still be queued when the user types;
  // React's controlled-input restore then applies both updates and reverts the
  // fresh draft to the prop. Derive prop changes during render so mount never
  // enqueues that stale reset.
  if (value !== syncedValue) {
    setSyncedValue(value);
    setDraft(String(value));
  }
  const normalize = (parsed: number) => {
    const decimals = (String(step).split('.')[1] ?? '').length;
    const stepped = Math.round(parsed / step) * step;
    return Math.max(min, Math.min(max, Number(stepped.toFixed(decimals))));
  };
  const commit = () => {
    if (draft.trim() === '') {
      setDraft(String(value));
      return;
    }
    const parsed = Number(draft);
    if (!Number.isFinite(parsed)) {
      setDraft(String(value));
      return;
    }
    const next = normalize(parsed);
    setDraft(String(next));
    if (next !== value) onCommit(next);
  };
  const onDraftChange = (text: string) => {
    setDraft(text);
    if (!live || text.trim() === '') return;
    const parsed = Number(text);
    if (!Number.isFinite(parsed) || parsed < min || parsed > max) return;
    const next = normalize(parsed);
    if (next === value) return;
    // Mark the value as already synced so the prop update this commit causes
    // doesn't overwrite what the user is still typing (e.g. "1.2" → "1.25").
    setSyncedValue(next);
    onCommit(next);
  };
  return <input
    className="settings-input"
    aria-label={label}
    type="number"
    min={min}
    max={max}
    step={step}
    placeholder={placeholder}
    disabled={disabled}
    value={draft}
    onChange={(event) => onDraftChange(event.target.value)}
    onBlur={commit}
    onKeyDown={(event) => {
      if (event.key === 'Enter') { event.preventDefault(); commit(); }
      if (event.key === 'Escape') setDraft(String(value));
    }}
  />;
}
