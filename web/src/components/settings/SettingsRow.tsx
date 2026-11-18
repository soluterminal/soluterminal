import { useContext } from 'react';
import type { ReactNode } from 'react';
import { SettingsGroupTransformContext } from './SettingsGroupTransformContext';

// Form primitives for the Settings dialog tabs: a titled group, a
// label-left/control-right row, and a full-width checkbox row. All controls
// are live (they write straight to the stores), so there is no Save button
// anywhere in Settings — the hint line is where a row explains its scope.

export function SettingsGroup({ title, children }: { title: string; children: ReactNode }) {
  const groupTransform = useContext(SettingsGroupTransformContext);
  const content = groupTransform?.title === title ? groupTransform.transform(children) : children;
  return (
    <section className="settings-group">
      <h3 className="settings-group-title">{title}</h3>
      {content}
    </section>
  );
}

export function SettingsRow({
  label,
  hint,
  title,
  id,
  searchKey,
  children,
}: {
  label: string;
  hint?: ReactNode;
  title?: string;
  /** Scroll target — the Crew panel's Change links jump to a role's editor row. */
  id?: string;
  /** The registry label Settings search lands on, when the shown label varies. */
  searchKey?: string;
  children: ReactNode;
}) {
  return (
    <div className="settings-row" id={id} title={title} data-setting={searchKey ?? label}>
      <span className="settings-row-label">{label}</span>
      <div className="settings-row-control">{children}</div>
      {hint && <p className="settings-row-hint">{hint}</p>}
    </div>
  );
}

export function SettingsCheck({
  label,
  checked,
  onChange,
  hint,
  title,
  disabled,
}: {
  label: string;
  checked: boolean;
  onChange: (v: boolean) => void;
  hint?: ReactNode;
  title?: string;
  disabled?: boolean;
}) {
  return (
    <div className="settings-row settings-row-check" title={title} data-setting={label}>
      <label className="settings-check">
        <input type="checkbox" checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
        {label}
      </label>
      {hint && <p className="settings-row-hint">{hint}</p>}
    </div>
  );
}
