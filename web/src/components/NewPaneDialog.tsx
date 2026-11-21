import { useEffect, useRef, useState } from 'react';
import { PastSessions, pastSessionTitle } from './PastSessions';
import type { BrowsedSession } from '../types';
import { api } from '../api';
import { useStore } from '../store';
import { useFocusTrap } from './useFocusTrap';
import { DirPicker } from './DirPicker';
import type { PaneEffort } from '../types';
import { EFFORTS, effortChipLabel, effortDescription } from '../lib/effort';
import { agentFromCommand, agentLabel } from '../lib/agentKind';
import { paneRolesAvailable, roleHostSupport } from '../extensions';
import { useOperatorFeatures, useTeamFeatures } from '../entitlements';

interface Props {
  open: boolean;
  onClose: () => void;
  splitDirection: 'horizontal' | 'vertical' | null;
  // Optional overrides — used by "Claude here" shortcut to skip typing.
  presetCwd?: string;
  presetCmd?: string;
}

const DEFAULT_CWD_HINT = '~/projects';
const SHELL_DEFAULT = '$SHELL';

export function parseCommandLine(input: string): string[] {
  const parts: string[] = [];
  let current = '';
  let quote: '"' | "'" | null = null;
  let escaping = false;
  let hasToken = false;

  for (const ch of input) {
    if (escaping) {
      current += ch;
      escaping = false;
      hasToken = true;
      continue;
    }

    if (ch === '\\' && quote !== "'") {
      escaping = true;
      hasToken = true;
      continue;
    }

    if (quote) {
      if (ch === quote) {
        quote = null;
      } else {
        current += ch;
      }
      hasToken = true;
      continue;
    }

    if (/\s/.test(ch)) {
      if (hasToken) {
        parts.push(current);
        current = '';
        hasToken = false;
      }
      continue;
    }

    if (ch === '"' || ch === "'") {
      quote = ch;
      hasToken = true;
      continue;
    }

    current += ch;
    hasToken = true;
  }

  if (quote) {
    throw new Error('Close the quoted command argument before creating the session.');
  }

  if (escaping) {
    current += '\\';
  }

  if (hasToken) {
    parts.push(current);
  }

  return parts;
}

export function NewPaneDialog({ open, onClose, splitDirection, presetCwd, presetCmd }: Props) {
  const createPane = useStore((s) => s.createPane);
  const splitFocused = useStore((s) => s.splitFocused);
  const focusedPaneId = useStore((s) => s.focusedPaneId);
  const workspaceDefaultContextPack = useStore((s) => {
    const workspace = s.workspaces.find((item) => item.name === s.activeWorkspace && (!s.activeBackend || !item.backend || item.backend === s.activeBackend));
    return workspace?.contextPack ?? '';
  });
  const hosts = useStore((s) => s.orchestratorHosts);
  // The Orchestrator coordinating role and Muse Code
  // are team-only on the public beta.
  const operatorTools = useOperatorFeatures();
  const orchestratorAllowed = operatorTools && paneRolesAvailable();
  // GLM (Z.ai) runs on an operator-managed key: operator installs only.
  const glmAllowed = operatorTools;
  // The raw `claude --resume` chip is a team-plan power shortcut; everyone
  // else resumes from "Continue a past conversation" below the folder.
  const resumeChipAllowed = useTeamFeatures();
  const loadOrchestratorHosts = useStore((s) => s.loadOrchestratorHosts);

  const [cwd, setCwd] = useState('');
  const [cwdLoading, setCwdLoading] = useState(false);
  const [cmd, setCmd] = useState(SHELL_DEFAULT);
  // A Linux pane's WSL distro ('' = a pane on this machine). The chips show
  // only when this machine opens Linux panes (GET /api/linux/distros).
  const [distro, setDistro] = useState('');
  const [distros, setDistros] = useState<{ name: string; default?: boolean }[]>([]);
  // Which LLM backend a claude pane runs against. 'anthropic' is the default;
  // 'glm' routes to Z.ai (the daemon resolves the key from ~/.config/glm/env —
  // we only send the name). Only meaningful for claude commands; ignored for
  // shells/runtimes. See daemon/api.go createPane provider handling.
  const [provider, setProvider] = useState<'anthropic' | 'glm'>('anthropic');
  // Reasoning-effort level for a claude pane (--effort). '' = claude's default.
  // Provider-agnostic; picking GLM auto-selects 'max' (Z.ai's coding default)
  // unless the user already chose a level. Only sent for claude commands.
  const [effort, setEffort] = useState<PaneEffort>('');
  // True while effort holds the 'max' GLM picked for the user, so switching
  // back to Claude returns to Claude's default instead of leaving max stuck.
  const [effortFromGlm, setEffortFromGlm] = useState(false);
  // Host-neutral product role. Capability metadata controls which agents can
  // receive the chief-of-staff brief and delegation tools.
  const [role, setRole] = useState<'' | 'orchestrator'>('');
  const [contextPack, setContextPack] = useState('');
  const [inheritDefault, setInheritDefault] = useState(true);
  const [title, setTitle] = useState('');
  const [err, setErr] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const cwdRequestId = useRef(0);
  const cwdEditedDuringLoad = useRef(false);
  const trapRef = useRef<HTMLFormElement>(null);
  useFocusTrap(trapRef, open);

  // Esc-to-close. Uses bubble-phase so nested components (e.g. DirPicker
  // closing its dropdown first) can stopPropagation and eat the Escape
  // before it reaches us.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !e.defaultPrevented) {
        onClose();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open, onClose]);

  useEffect(() => {
    if (!open) return;
    // Reset state each open.
    setCmd(presetCmd || SHELL_DEFAULT);
    setDistro('');
    api.linuxDistros()
      .then((r) => setDistros(r.enabled ? r.distros : []))
      .catch(() => setDistros([]));
    setProvider('anthropic');
    setEffort('');
    setEffortFromGlm(false);
    setRole('');
    setContextPack(workspaceDefaultContextPack);
    setInheritDefault(true);
    setTitle('');
    setErr(null);
    setSubmitting(false);
    void loadOrchestratorHosts();
    setCwdLoading(false);
    cwdEditedDuringLoad.current = false;
    const requestId = cwdRequestId.current + 1;
    cwdRequestId.current = requestId;

    // Populate cwd: explicit preset takes precedence; otherwise prefill the
    // focused pane's KNOWN (cached, spawn-time) cwd immediately, then fetch its
    // LIVE cwd (where the shell actually is, after cd'ing) as a best-effort
    // upgrade so splits feel natural.
    //
    // The live fetch must never be able to FREEZE the dialog. The desktop
    // reaches the daemon over the cloud relay, where a /cwd request can stall
    // with no response at all; with no bound, cwdLoading stayed true forever —
    // "Loading current directory…" that never cleared and a permanently
    // disabled Create button. So: (1) seed the cached cwd up front so the field
    // is usable at once, (2) only show the loading state when we have nothing to
    // prefill, and (3) cap that loading state with a timeout so it can't hang.
    if (presetCwd) {
      setCwd(presetCwd);
    } else if (focusedPaneId) {
      const cachedCwd = useStore.getState().panes[focusedPaneId]?.cwd ?? '';
      setCwd(cachedCwd);
      if (cachedCwd) {
        setCwdLoading(false);
      } else {
        setCwdLoading(true);
        // Fall back to a usable (empty) field if the live cwd never arrives.
        window.setTimeout(() => {
          if (cwdRequestId.current === requestId) setCwdLoading(false);
        }, 2500);
      }
      api.paneLiveCwd(focusedPaneId).then((live) => {
        if (cwdRequestId.current === requestId && !cwdEditedDuringLoad.current && live) {
          setCwd(live);
        }
      }).catch(() => {}).finally(() => {
        if (cwdRequestId.current === requestId) {
          setCwdLoading(false);
        }
      });
    } else {
      setCwd('');
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  if (!open) return null;

  function handleCwdChange(nextCwd: string) {
    cwdEditedDuringLoad.current = true;
    setCwd(nextCwd);
    setCwdLoading(false);
  }

  async function submit(e?: React.FormEvent) {
    e?.preventDefault();
    if (cwdLoading) {
      setErr('Still loading the current working directory.');
      return;
    }
    const resolvedCwd = cwd.trim();
    // Resolve $SHELL on the daemon side (empty cmd → daemon uses defaultShell).
    let parts: string[];
    if (cmd.trim() === SHELL_DEFAULT || cmd.trim() === '') {
      parts = []; // daemon picks $SHELL
    } else {
      try {
        parts = parseCommandLine(cmd);
      } catch (e) {
        setErr((e as Error).message);
        return;
      }
      if (parts.length === 0) {
        setErr('Enter a command.');
        return;
      }
    }
    // Agent identity is explicit for Codex so a request can never fall
    // through the legacy default and accidentally spawn Claude. Claude remains
    // the omitted/default wire value to preserve existing request behaviour.
    const commandAgent = agentFromCommand(parts);
    const isClaudeCmd = commandAgent === 'claude';
    const roleSupport = roleHostSupport(commandAgent, hosts);
    if (role === 'orchestrator' && (!roleSupport.supported || !roleSupport.available)) {
      setErr(roleSupport.reason || `${roleSupport.label} cannot host the Orchestrator role.`);
      return;
    }
    const opts = {
      cwd: resolvedCwd,
      cmd: parts,
      // A Linux pane with no title of its own gets the daemon's "Linux (<distro>)".
      title: title.trim() || (distro ? '' : role === 'orchestrator' && roleSupport.supported ? 'Orchestrator' : parts[0] ? parts[0] : 'shell'),
      ...(commandAgent === 'codex' ? { agent: 'codex' as const } : {}),
      ...(provider === 'glm' && isClaudeCmd && glmAllowed ? { provider: 'glm' as const } : {}),
      // Effort rides only on a claude command. '' is omitted so the daemon
      // keeps its default (and still applies GLM's max default server-side).
      ...(effort && isClaudeCmd ? { effort } : {}),
      ...(distro ? { distro } : {}),
      ...(role === 'orchestrator' && roleSupport.supported ? { role: 'orchestrator' as const } : {}),
      // Packs aren't available in Linux panes yet: send none for them, so a
      // workspace's default pack doesn't refuse the launch.
      ...(!distro && commandAgent && (commandAgent === 'claude' || commandAgent === 'codex') && contextPack.trim()
        ? { contextPack: contextPack.trim() }
        : {}),
      ...(!distro && commandAgent && (commandAgent === 'claude' || commandAgent === 'codex') && workspaceDefaultContextPack && !contextPack.trim() && !inheritDefault
        ? { contextPack: 'none' }
        : {}),
    };
    setErr(null);
    setSubmitting(true);
    try {
      if (splitDirection && focusedPaneId) {
        await splitFocused(splitDirection, opts);
      } else {
        await createPane(opts);
      }
      onClose();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setSubmitting(false);
    }
  }

  // One click on a past session: spawn the agent resuming it, in this folder.
  async function resumeSession(sess: BrowsedSession) {
    const opts = {
      cwd: cwd.trim(),
      cmd: [sess.agent === 'codex' ? 'codex' : 'claude'],
      title: pastSessionTitle(sess),
      ...(sess.agent === 'codex'
        ? { agent: 'codex' as const, codexThreadId: sess.id }
        : { claudeSessionId: sess.id }),
    };
    setErr(null);
    setSubmitting(true);
    try {
      if (splitDirection && focusedPaneId) await splitFocused(splitDirection, opts);
      else await createPane(opts);
      onClose();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setSubmitting(false);
    }
  }

  // Provider + effort controls currently describe Claude CLI flags. Keep them
  // hidden for Codex; its app-server capabilities are configured separately.
  const selectedAgent = agentFromCommand(cmd);
  const isClaude = selectedAgent === 'claude';
  // The shell chip is selected only while the command is the default shell.
  const isDefaultShell = cmd.trim() === SHELL_DEFAULT || cmd.trim() === '';
  const isPackAgent = selectedAgent === 'claude' || selectedAgent === 'codex';
  const selectedOrchestratorSupport = roleHostSupport(selectedAgent, hosts);
  const codexOrchestratorSupport = roleHostSupport('codex', hosts);

  // What the pane will actually be titled if the user leaves Title blank —
  // mirrors submit()'s fallback ($SHELL/empty → 'shell', else first token) so
  // the placeholder never promises a title the pane won't get.
  const defaultTitle = role === 'orchestrator' && selectedOrchestratorSupport.supported
    ? 'Orchestrator'
    : cmd.trim() === SHELL_DEFAULT || cmd.trim() === ''
      ? 'shell'
      : cmd.trim().split(/\s+/)[0];

  return (
    <div className="dialog-backdrop" onMouseDown={onClose}>
      <form ref={trapRef} className="dialog" onSubmit={submit} onMouseDown={(e) => e.stopPropagation()}>
        <div className="dialog-header">
          <h2 className="dialog-title">
            {splitDirection
              ? `Split ${splitDirection === 'horizontal' ? 'right' : 'down'} with new session`
              : 'New session'}
          </h2>
        </div>

        {isPackAgent && (
          <div>
            <label htmlFor="new-pane-context-pack">Context pack</label>
            <DirPicker
              id="new-pane-context-pack"
              value={contextPack}
              onChange={(value) => { setContextPack(value); setInheritDefault(false); }}
              placeholder="~/projects/context"
            />
            <div className="dialog-chip-row" style={{ marginTop: 6 }}>
              {workspaceDefaultContextPack && (
                <button
                  type="button"
                  className={`dialog-chip${inheritDefault && contextPack === workspaceDefaultContextPack ? ' dialog-chip-strong' : ''}`}
                  onClick={() => { setContextPack(workspaceDefaultContextPack); setInheritDefault(true); }}
                >Workspace default</button>
              )}
              <button
                type="button"
                className={`dialog-chip${!contextPack && !inheritDefault ? ' dialog-chip-strong' : ''}`}
                onClick={() => { setContextPack(''); setInheritDefault(false); }}
              >None</button>
            </div>
            <div className="dialog-help">A local folder containing CLAUDE.md or AGENTS.md. Only its path is stored.</div>
          </div>
        )}

        <div>
          <label htmlFor="cwd">Working directory</label>
          <DirPicker
            id="cwd"
            value={cwd}
            onChange={handleCwdChange}
            placeholder={cwdLoading ? 'Loading current directory…' : DEFAULT_CWD_HINT}
            autoFocus
          />
          <div className="dialog-help">
            {cwdLoading
              ? 'Loading the focused pane’s current directory before creating a session.'
              : 'Type to filter, ↑↓ to navigate, Tab/Enter to select. Or paste a path.'}
          </div>
        </div>

        {/* Resuming isn't available in Linux panes yet. */}
        {!cwdLoading && !distro && (
          <PastSessions cwd={cwd} disabled={submitting} onResume={(sess) => void resumeSession(sess)} />
        )}

        <div>
          <label htmlFor="cmd">Command</label>
          <input
            id="cmd"
            value={cmd}
            onChange={(e) => {
              const next = e.target.value;
              if (agentFromCommand(next) !== selectedAgent) setRole('');
              setCmd(next);
             
            }}
            placeholder="$SHELL"
            className="dialog-control"
          />
          <div className="dialog-chip-row">
            {/* shells (default + explicit alternatives) */}
            <button
              type="button"
              onClick={() => { setCmd(SHELL_DEFAULT); setRole(''); setDistro(''); }}
              className={`dialog-chip${isDefaultShell && !distro ? ' dialog-chip-strong' : ''}`}
              title="Use $SHELL (your default shell)"
            >shell</button>
            {['bash', 'zsh', 'pwsh.exe', 'powershell.exe'].map((c) => (
              <button
                type="button"
                key={c}
                onClick={() => { setCmd(c); setRole(''); setDistro(''); }}
                className={`dialog-chip${cmd.trim() === c && !distro ? ' dialog-chip-strong' : ''}`}
                title={
                  c === 'pwsh.exe' ? 'PowerShell 7+ on Windows (via WSL interop)'
                    : c === 'powershell.exe' ? 'Windows PowerShell 5.1 (via WSL interop)'
                    : undefined
                }
              >{c}</button>
            ))}
            {distros.length > 0 && <span className="dialog-chip-separator">·</span>}
            {/* Linux panes: a shell (or the command above) in a WSL distro */}
            {distros.map((d) => (
              <button
                type="button"
                key={`linux:${d.name}`}
                onClick={() => {
                  setDistro(d.name);
                  setRole('');
                  // A Windows path means nothing there: clear it as an edit,
                  // so a late live-cwd lookup can't put it back.
                  if (distro === '') handleCwdChange('');
                }}
                className={`dialog-chip${distro === d.name ? ' dialog-chip-strong' : ''}`}
                title={`Open in the ${d.name} WSL distro${d.default ? ' (your default)' : ''}. Leave the folder empty for your Linux home.`}
              >{`Linux (${d.name})`}</button>
            ))}
            <span className="dialog-chip-separator">·</span>
            {/* claude quick options */}
            {(resumeChipAllowed ? ['claude', 'claude --resume'] : ['claude']).map((c) => (
              <button
                type="button"
                key={c}
                onClick={() => { if (selectedAgent !== 'claude') setRole(''); setCmd(c); }}
                className={`dialog-chip${cmd.trim() === c && !role ? ' dialog-chip-strong' : ''}`}
              >{c}</button>
            ))}
            {/* The chief-of-staff preset: a claude pane spawned with the
                orchestrator brief, so it coordinates direct work / schedules
                while keeping the user informed. */}
            {orchestratorAllowed && (
              <button
                type="button"
                onClick={() => {
                  const active = role === 'orchestrator' && selectedAgent === 'claude';
                  setCmd('claude');
                 
                  setRole(active ? '' : 'orchestrator');
                }}
                className={`dialog-chip${role === 'orchestrator' && selectedAgent === 'claude' ? ' dialog-chip-strong' : ''}`}
                title="One Claude pane that investigates, builds, and coordinates other panes; you decide what merges. Click again for a plain claude"
              >claude · Orchestrator</button>
            )}
            <button
              type="button"
              onClick={() => { if (selectedAgent !== 'codex') setRole(''); setCmd('codex'); }}
              className={`dialog-chip${cmd.trim() === 'codex' && !role ? ' dialog-chip-strong' : ''}`}
              title="Open Codex in Terminal mode"
            >codex</button>
            {orchestratorAllowed && (
              <button
                type="button"
                disabled={!codexOrchestratorSupport.available}
                onClick={() => {
                  const active = role === 'orchestrator' && selectedAgent === 'codex';
                  setCmd('codex');
                 
                  setRole(active ? '' : 'orchestrator');
                }}
                className={`dialog-chip${role === 'orchestrator' && selectedAgent === 'codex' ? ' dialog-chip-strong' : ''}`}
                title={codexOrchestratorSupport.available
                  ? 'Run Codex as your Orchestrator in Terminal mode'
                  : codexOrchestratorSupport.reason}
              >codex · Orchestrator</button>
            )}
            {/* Muse Code is an internal runtime: team installs only. */}
            {orchestratorAllowed && (
              <button
                type="button"
                onClick={() => { setCmd('muse'); setRole(''); }}
                className={`dialog-chip${cmd.trim() === 'muse' ? ' dialog-chip-strong' : ''}`}
                title="Open Muse Code (Meta) in Terminal mode"
              >muse</button>
            )}
            <span className="dialog-chip-separator">·</span>
            {/* runtimes */}
            {['node', 'python3'].map((c) => (
              <button
                type="button"
                key={c}
                onClick={() => { setCmd(c); setRole(''); }}
                className={`dialog-chip${cmd.trim() === c ? ' dialog-chip-strong' : ''}`}
              >{c}</button>
            ))}
          </div>
          {orchestratorAllowed && hosts !== null && !codexOrchestratorSupport.available && (
            <div className="dialog-help">Codex Orchestrator unavailable: {codexOrchestratorSupport.reason}</div>
          )}
          {role === 'orchestrator' && selectedOrchestratorSupport.supported && (
            <div className="dialog-help" style={{ color: 'var(--accent)' }}>
              → This {agentLabel(selectedAgent)} session runs as your <strong>Orchestrator</strong>: it investigates, builds,
              coordinates other panes, and reports what needs your decision.
            </div>
          )}
          {isClaude && glmAllowed && (
            <div className="dialog-chip-row" style={{ marginTop: 6 }}>
              <span className="dialog-chip-separator" style={{ paddingRight: 2 }}>model</span>
              <button
                type="button"
                onClick={() => {
                  setProvider('anthropic');
                  if (effortFromGlm) setEffort('');
                  setEffortFromGlm(false);
                }}
                className={`dialog-chip${provider === 'anthropic' ? ' dialog-chip-strong' : ''}`}
                title="Anthropic (default) — your Claude subscription"
              >Claude</button>
              <button
                type="button"
                onClick={() => {
                  setProvider('glm');
                  if (effort === '') {
                    setEffort('max');
                    setEffortFromGlm(true);
                  }
                }}
                className={`dialog-chip${provider === 'glm' ? ' dialog-chip-strong' : ''}`}
                title="GLM-5.2 via Z.ai — uses your key in ~/.config/glm/env"
              >GLM</button>
            </div>
          )}
          {isClaude && glmAllowed && provider === 'glm' && (
            <div className="dialog-help" style={{ color: 'var(--accent)' }}>
              → Routes this Claude session to <strong>GLM-5.2</strong> (Z.ai). Needs
              your Z.ai key in <code>~/.config/glm/env</code>; otherwise the daemon
              rejects it with a clear error.
            </div>
          )}
          {isClaude && (
            <div className="dialog-chip-row" style={{ marginTop: 6 }}>
              <span className="dialog-chip-separator" style={{ paddingRight: 2 }}>effort</span>
              {EFFORTS.map((e) => (
                <button
                  key={e || 'default'}
                  type="button"
                  onClick={() => { setEffort(e); setEffortFromGlm(false); }}
                  className={`dialog-chip${effort === e ? ' dialog-chip-strong' : ''}`}
                  title={effortDescription(e)}
                >{effortChipLabel(e)}</button>
              ))}
            </div>
          )}
          {isClaude && (
            <div className="dialog-help">
              Reasoning effort (claude’s <code>/effort</code>).{' '}
              {provider === 'glm' && glmAllowed
                ? 'GLM maps high-and-below → High, xhigh/max → Max; max is recommended for coding.'
                : 'Leave “default” to use Claude’s built-in level.'}
            </div>
          )}
        </div>

        <div>
          <label htmlFor="title">Title (optional)</label>
          <input
            id="title"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            placeholder={defaultTitle}
            className="dialog-control"
          />
        </div>

        {err && (
          <div className="dialog-error">{err}</div>
        )}

        <div className="dialog-actions">
          <button type="button" onClick={onClose} disabled={submitting}>Cancel</button>
          <button type="submit" className="primary" disabled={submitting || cwdLoading}>
            {submitting ? 'Creating…' : 'Create'}
          </button>
        </div>
      </form>
    </div>
  );
}
