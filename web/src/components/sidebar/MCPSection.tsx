import { useEffect } from 'react';
import { useStore } from '../../store';
import { SectionHeader } from './SectionHeader';
import { IconPlug } from '../ui/icons';

interface MCPSectionProps {
  collapsed: boolean;
  onToggle: () => void;
}

export function MCPSection({ collapsed, onToggle }: MCPSectionProps) {
  const focusedPaneId = useStore((s) => s.focusedPaneId);
  const panes = useStore((s) => s.panes);
  const mcpByCwd = useStore((s) => s.mcpServersByCwd);
  const refreshMcpServers = useStore((s) => s.refreshMcpServers);
  const toast = useStore((s) => s.toast);

  const cwd = focusedPaneId
    ? (panes[focusedPaneId]?.liveCwd || panes[focusedPaneId]?.cwd || '')
    : '';

  // Refetch when the focused pane's cwd changes — covers both pane-focus
  // changes and live `cd`s within a pane (liveCwd updates on /api/panes
  // polls every 3s). Skipped while collapsed to save the round-trip;
  // the next expand will fetch on mount. While expanded we also re-probe
  // every 15 s so silent disconnects flip the status dot to red without
  // requiring the user to toggle the section — the whole reason this
  // feature exists.
  useEffect(() => {
    if (collapsed || !cwd) return;
    void refreshMcpServers(cwd);
    const h = setInterval(() => { void refreshMcpServers(cwd); }, 15_000);
    return () => clearInterval(h);
  }, [cwd, collapsed, refreshMcpServers]);

  const servers = (cwd && mcpByCwd[cwd]) || [];

  // The daemon sends url, command and args in a short display form. The row
  // shows that text and the click copies the same string, so nothing is
  // copied that isn't on screen.
  function copyDetail(text: string) {
    if (!text) return;
    navigator.clipboard?.writeText(text)
      .then(() => toast('Copied', { kind: 'success', icon: '✓', ttlMs: 1500 }))
      .catch(() => { /* clipboard denied — silent */ });
  }

  return (
    <div className="sidebar-section">
      <SectionHeader
        icon={<IconPlug size={14} />}
        title="MCP servers"
        count={servers.length || undefined}
        collapsed={collapsed}
        onToggle={onToggle}
      />
      {!collapsed && (
        <div>
          {!cwd && (
            <div style={{ fontSize: 11, color: 'var(--fg-dim)', padding: '2px 4px' }}>
              Focus a pane to see its MCP servers.
            </div>
          )}
          {cwd && servers.length === 0 && (
            <div style={{ fontSize: 11, color: 'var(--fg-dim)', padding: '2px 4px' }}>
              No MCP servers configured for this project.
            </div>
          )}
          {servers.map((s) => {
            const detail = s.url || [s.command, ...(s.args || [])].filter(Boolean).join(' ');
            const statusLabel = s.status === 'alive' ? 'reachable'
              : s.status === 'dead' ? 'not reachable — check the server process'
              : s.type === 'stdio' ? 'stdio — liveness unknown (can\'t probe without spawning)'
              : '';
            const tooltip = [
              `Name: ${s.name}`,
              `Type: ${s.type || 'unknown'}`,
              `Source: ${s.source}`,
              statusLabel ? `Status: ${statusLabel}` : null,
              detail ? `→ ${detail}` : null,
              detail ? 'Click to copy (short form)' : null,
            ].filter(Boolean).join('\n');
            return (
              <div
                key={`${s.source}-${s.name}`}
                className="port-row"
                title={tooltip}
                onClick={() => copyDetail(detail)}
                style={{ cursor: detail ? 'pointer' : undefined }}
              >
                <div className="port-row-main">
                  {s.status && (
                    <span className={`mcp-dot mcp-dot-${s.status}`} aria-hidden="true" />
                  )}
                  <span className={`mcp-type mcp-type-${s.type || 'unknown'}`}>
                    {(s.type || '?').toUpperCase()}
                  </span>
                  <span className="port-label">{s.name}</span>
                  <span className={`mcp-source mcp-source-${s.source.replace('.', '')}`}>
                    {s.source}
                  </span>
                </div>
                {detail && (
                  <div className="port-row-sub" title={detail}>{detail}</div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
