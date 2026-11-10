import { useState } from 'react';
import { useStore } from '../store';
import { api, currentPrefix, currentRoutingScope } from '../api';
import type { Port } from '../types';
import { openInSystemBrowser } from '../tauri-browser';
import { serviceTarget, systemBrowserUrl } from '../lib/portBrowser';
import {
  isWebService,
  portKey,
  portUrl,
  visiblePorts,
  workspacePorts,
  type PortScope,
} from '../lib/ports';
import { usePortPreferences } from '../lib/portPreferences';

export function primaryLabel(port: Port, paneTitle?: string): string {
  if (paneTitle) return paneTitle;
  const cmdline = port.cmdline.trim();
  if (cmdline) {
    // Reduce common verbose forms to their interesting part.
    // /usr/bin/node /path/to/npm-cli.js run dev  →  npm run dev
    const parts = cmdline.split(/\s+/);
    // If argv[0] is an interpreter and argv[1] is a script, use the script's name.
    const interpreters = /^(node|python3?|ruby|deno|bun|java)$/i;
    if (parts.length >= 2 && interpreters.test(parts[0].split('/').pop() || '')) {
      const script = parts[1].split('/').pop() || parts[1];
      // Trim common extensions, drop full paths.
      const scriptName = script.replace(/\.(js|mjs|cjs|ts|py|rb)$/, '');
      return [scriptName, ...parts.slice(2)].join(' ').slice(0, 40);
    }
    // Otherwise: basename of argv[0] plus remaining args.
    const head = parts[0].split('/').pop() || parts[0];
    return [head, ...parts.slice(1)].join(' ').slice(0, 40);
  }
  return port.process || `pid ${port.pid}`;
}

export function PortPanel() {
  const ports = useStore((s) => s.ports);
  const error = useStore((s) => s.portsError);
  const loaded = useStore((s) => s.portsUpdatedAt !== null);
  const panes = useStore((s) => s.panes);
  const workspaces = useStore((s) => s.workspaces);
  const activeWorkspace = useStore((s) => s.activeWorkspace);
  const activeBackend = useStore((s) => s.activeBackend);
  const { scope, includeOther, setView } = usePortPreferences();
  const [checking, setChecking] = useState<string | null>(null);
  const current = workspacePorts(ports, activeWorkspace, activeBackend, workspaces);
  const sorted = visiblePorts(ports, scope, includeOther, current);
  const otherCount = ports.filter((p) => !isWebService(p)).length;
  const remote = !!currentPrefix();
  const st = useStore.getState();
  async function check(p: Port) {
    const scope = currentRoutingScope().key;
    setChecking(portKey(p));
    try {
      const checked = await api.probePort(p);
      if (scope !== currentRoutingScope().key) return;
      if (!isWebService(checked))
        st.toast('This listener did not confirm an HTTP or HTTPS service.', { kind: 'info' });
      await st.refreshPorts();
    } catch (error) {
      if (scope === currentRoutingScope().key)
        st.toast(error instanceof Error ? error.message : 'Could not check this listener.', {
          kind: 'warn',
        });
    } finally {
      setChecking(null);
    }
  }
  async function goToPane(p: Port) {
    const machine = currentPrefix();
    if (p.backend && p.backend !== activeBackend) await st.switchBackend(p.backend, p.workspace);
    if (machine === currentPrefix() && p.paneId) await useStore.getState().revealPane(p.paneId);
  }
  async function copy(p: Port) {
    try {
      if (!navigator.clipboard?.writeText) throw new Error('Clipboard is unavailable');
      // Copy the friendly address, never a bearer token or expiring proxy URL.
      await navigator.clipboard.writeText(portUrl(p));
      st.toast('Service address copied', { kind: 'success' });
    } catch {
      st.toast('Could not copy the address. Clipboard access is unavailable.', { kind: 'warn' });
    }
  }
  return (
    <div className="services-panel">
      <div className="port-toolbar">
        <select
          aria-label="Service scope"
          value={scope}
          onChange={(e) => setView({ scope: e.target.value as PortScope })}
        >
          <option value="workspace">This workspace</option>
          <option value="panes">All my panes</option>
          <option value="all">All services</option>
        </select>
        <button
          type="button"
          className="port-action"
          onClick={() => void st.refreshPorts()}
          aria-label="Refresh services"
        >
          Refresh
        </button>
      </div>
      {error && (
        <div role="status" className="port-notice">
          {error}
          {loaded ? ' Showing the last available services.' : ''}
        </div>
      )}
      {sorted.length === 0 && (
        <p className="port-notice">
          {!loaded && !error
            ? 'Looking for services…'
            : scope === 'workspace'
              ? 'No web services in this workspace. Start your app in a pane; it will appear here.'
              : 'No confirmed web services found.'}
        </p>
      )}
      <div className="port-list-scroll">
        {sorted.map((p) => {
          const pane =
            p.paneId && (!p.backend || !activeBackend || p.backend === activeBackend)
              ? panes[p.paneId]
              : undefined;
          const label = primaryLabel(p, p.paneTitle || pane?.title);
          const target = serviceTarget(p);
          const url = portUrl(p);
          const web = isWebService(p);
          const routed = remote || !!p.backend;
          // The preview tile shows only services the person's panes run; any
          // web service can still open in their own browser.
          const previewable = web && !!p.paneId;
          const external = web && (!routed || !!p.paneId);
          const reason = !web
            ? p.webStatus === 'internal'
              ? 'Internal tool or TCP service; no web preview'
              : 'Listening; HTTP readiness has not been confirmed'
            : !p.paneId
              ? routed
                ? `${remote ? 'Remote' : 'Backend'} preview requires a service started in a soluterminal pane`
                : 'Preview shows services started in a soluterminal pane; use Open in browser'
              : undefined;
          const cwd = (p.cwd || pane?.cwd || '').replace(/^\/home\/[^/]+/, '~');
          return (
            <div
              key={portKey(p)}
              className="port-row"
              title={[p.cmdline, p.cwd, `PID: ${p.pid}`, `Address: ${p.addr}:${p.port}`]
                .filter(Boolean)
                .join('\n')}
            >
              <div className="port-row-main">
                <span className="port-num">:{p.port}</span>
                <span className="port-label">{label}</span>
                <span className="port-badge">
                  {p.backend === 'win'
                    ? 'Windows'
                    : p.backend === 'wsl'
                      ? 'WSL'
                      : p.proto === 'tcp6'
                        ? 'IPv6'
                        : ''}
                </span>
                <span className="port-badge">{web ? p.scheme?.toUpperCase() : 'TCP'}</span>
              </div>
              {(p.workspace || cwd) && (
                <div className="port-row-sub">
                  {p.workspace ? `${p.workspace} · ` : ''}
                  {cwd}
                </div>
              )}
              <div className="port-actions">
                {!web && p.webStatus !== 'internal' && (
                  <button
                    type="button"
                    className="port-action"
                    disabled={checking !== null}
                    onClick={() => void check(p)}
                  >
                    {checking === portKey(p) ? 'Checking…' : 'Check web service'}
                  </button>
                )}
                <button
                  type="button"
                  className="port-action"
                  disabled={!previewable}
                  title={reason}
                  onClick={() => st.openBrowserHere(target)}
                >
                  Open preview
                </button>
                <button
                  type="button"
                  className="port-action"
                  disabled={!external}
                  title={external ? 'Open in your browser' : reason}
                  onClick={(e) => {
                    const click = e.nativeEvent;
                    void systemBrowserUrl(url, target, ports)
                      .then((address) => {
                        if (address) return openInSystemBrowser(address, click);
                        st.toast('This service can’t be opened from here.', { kind: 'warn' });
                      })
                      .catch((error) =>
                        st.toast(
                          error instanceof Error ? error.message : 'Could not prepare the address.',
                          { kind: 'warn' },
                        ),
                      );
                  }}
                >
                  Open in browser
                </button>
                <button
                  type="button"
                  className="port-action"
                  title="Copy address on the service's computer"
                  onClick={() => void copy(p)}
                >
                  Copy address
                </button>
                {p.paneId && (
                  <button
                    type="button"
                    className="port-action"
                    disabled={!pane && !p.backend}
                    title={
                      !pane ? 'Switch to this service’s backend and go to its pane' : undefined
                    }
                    onClick={() => void goToPane(p)}
                  >
                    Go to pane
                  </button>
                )}
              </div>
              {reason && <div className="port-reason">{reason}</div>}
            </div>
          );
        })}
      </div>
      {(otherCount > 0 || includeOther) && (
        <label className="port-filter-toggle port-other-toggle">
          <input
            type="checkbox"
            checked={includeOther}
            onChange={(e) => setView({ includeOther: e.target.checked })}
          />
          Other TCP listeners ({otherCount})
        </label>
      )}
    </div>
  );
}
