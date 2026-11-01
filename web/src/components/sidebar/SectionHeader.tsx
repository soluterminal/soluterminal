import type { ReactNode } from 'react';
import { IconChevronDown } from '../ui/icons';

interface SectionHeaderProps {
  title: string;
  // Plain text, or a compact inline settings link.
  // Rendered inside the toggle button, so an interactive
  // subtitle must be a span[role=button] (nested <button> is invalid HTML)
  // that stopPropagation()s its own clicks.
  subtitle?: ReactNode;
  count?: number | string;
  collapsed: boolean;
  onToggle: () => void;
  extra?: ReactNode;
  // Leading identity glyph (an SVG from ui/icons). Anchors the section so the
  // sidebar scans as icons + labels, not a wall of uppercase text.
  icon?: ReactNode;
  // Extra modifier class on the header strip.
  className?: string;
}

export function SectionHeader({
  title,
  subtitle,
  count,
  collapsed,
  onToggle,
  extra,
  icon,
  className,
}: SectionHeaderProps) {
  return (
    <div className={className ? `section-header ${className}` : 'section-header'}>
      <button
        type="button"
        className="section-header-toggle"
        aria-expanded={!collapsed}
        onClick={onToggle}
      >
        <IconChevronDown size={13} className={`section-chevron ${collapsed ? 'collapsed' : ''}`} />
        {icon && <span className="section-icon" aria-hidden="true">{icon}</span>}
        <span className="section-header-text">
          <span className="section-title">
            {title}
            {count !== undefined && count !== '' ? <span className="section-count">{count}</span> : null}
          </span>
          {subtitle && !collapsed ? <span className="section-subtitle">{subtitle}</span> : null}
        </span>
      </button>
      {extra && <div className="section-extra">{extra}</div>}
    </div>
  );
}
