import { useState } from 'react';
import { isDevLikeVersion, isVersionNewer } from '../lib/bundleParity';
import { currentRoutingScope } from '../api';
import { useDaemonHealth } from '../lib/useDaemonHealth';
import { useStore } from '../store';

// ST-315: a new desktop setup can update this window (the web UI) while the
// background service it talks to (the daemon, e.g. in WSL) stays a version
// behind until its own update applies. Say so plainly, and why some new
// actions are missing, instead of letting them fail with a raw 404.

const bundleVersion = typeof __SOLU_WEB_VERSION__ === 'string' ? __SOLU_WEB_VERSION__ : 'dev';
const DISMISSED_KEY = 'soluterminal.daemonBehindDismissed';

/** The daemon version when it is older than this window, else null (dev builds never count). */
export function daemonBehind(daemonVersion: string | undefined, uiVersion = bundleVersion): string | null {
  const daemon = (daemonVersion ?? '').trim();
  if (isDevLikeVersion(daemon) || isDevLikeVersion(uiVersion)) return null;
  return isVersionNewer(uiVersion, daemon) ? daemon : null;
}

export function DaemonBehindNotice({ uiVersion = bundleVersion }: { uiVersion?: string }) {
  const { health, healthScopeKey, bundleState } = useDaemonHealth();
  const autoApply = useStore((s) => s.updateSettings?.autoApply);
  const status = useStore((s) => s.updateStatus);
  // Only a health answer for the scope this window is on now counts (not a
  // scope we've left; the router may still have answered from its fallback
  // backend under the same key, so this is advice, never a gate: recording is
  // gated by usePaneContextMenu's own scoped probe). The existing version
  // banners speak first so the two never contradict each other.
  const inScope = !!health && healthScopeKey === currentRoutingScope().key;
  const bannersQuiet = !bundleState || bundleState.kind === 'ok';
  const behind = inScope && bannersQuiet ? daemonBehind(health.version, uiVersion) : null;
  const pair = behind ? `${uiVersion}>${behind}` : '';
  const [dismissed, setDismissed] = useState<string | null>(() => {
    try {
      return localStorage.getItem(DISMISSED_KEY);
    } catch {
      return null;
    }
  });
  if (!behind || dismissed === pair) return null;
  // Only promise an update when the daemon reports one it can apply.
  const pending = !!status?.available && !!status.latest && !isVersionNewer(uiVersion, status.latest);
  const applies = !pending
    ? "it hasn't seen the update yet and picks it up on its next update check"
    : autoApply === 'when-idle' || status?.required
      ? 'it updates itself the next time your agents are idle'
      : 'choose Update now when SoluTerminal offers the update';
  const dismiss = () => {
    setDismissed(pair);
    try {
      localStorage.setItem(DISMISSED_KEY, pair);
    } catch {
      /* not fatal: the notice returns next session */
    }
  };
  return (
    <div className="daemon-banner-stack">
      <div className="daemon-banner is-info" role="status">
        <span className="daemon-banner-dot" aria-hidden />
        <span className="daemon-banner-text">
          <strong>{pending ? 'The background service update is still pending' : 'The background service is older than this window'}</strong>
          <span className="daemon-banner-sub">
            {` · this window is ${uiVersion}, the service is ${behind}. Some newer actions are hidden until the service catches up; ${applies}.`}
          </span>
        </span>
        <button type="button" className="daemon-banner-dismiss" onClick={dismiss} title="Dismiss for this update" aria-label="Dismiss">
          ×
        </button>
      </div>
    </div>
  );
}
