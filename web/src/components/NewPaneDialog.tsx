import { useEffect, useRef, useState } from 'react';
import { useFocusEnabled } from '../lib/focusAvailability';
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
  // The user's persisted default renderer for NEW Claude sessions ('' terminal
  // | 'native' Focus, Preferences → "New Claude sessions"). Feeds
  // defaultKindFor below; the explicit chips still always win.
  const focusEnabled = useFocusEnabled();
  const storedDefaultClaudeMode = useStore((s) => s.nativeSettings?.defaultClaudeMode ?? '');
  // Focus retired → every new session is a terminal, whatever was stored.
  const defaultClaudeMode = focusEnabled ? storedDefaultClaudeMode : '';
  // The kind a given command DEFAULTS to: claude commands follow the user's
  // preference; everything else is a terminal pane. Applied whenever the
  // command changes without an explicit Focus choice.
  const defaultKindFor = (c: string): '' | 'native' =>
    agentFromCommand(c) === 'claude' ? defaultClaudeMode : '';
  // '' = terminal pane; 'native' = open Claude directly in Focus (rendered)
  // mode, no terminal→toggle step. Re-derived via defaultKindFor whenever the
  // command changes, so Focus sticks when explicitly chosen OR preferred.
  const [kind, setKind] = useState<'' | 'native'>('');
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
    // Reset state each open. kind follows the command's default (a preset
    // claude cmd honors the user's default-renderer preference).
    setCmd(presetCmd || SHELL_DEFAULT);
    setKind(defaultKindFor(presetCmd || SHELL_DEFAULT));
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
    // Agent identity is explicit for Codex so a native request can never fall
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
      title: title.trim() || (role === 'orchestrator' && roleSupport.supported ? 'Orchestrator' : parts[0] ? parts[0] : 'shell'),
      // Focus pane: the daemon ignores cmd for kind:'native' and builds the
      // headless Claude argv itself, so cmd:['claude'] is just a sensible label.
      ...(kind === 'native' && focusEnabled ? { kind: 'native' as const } : {}),
      ...(commandAgent === 'codex' ? { agent: 'codex' as const } : {}),
      ...(provider === 'glm' && isClaudeCmd && glmAllowed ? { provider: 'glm' as const } : {}),
      // Effort rides only on a claude command. '' is omitted so the daemon
      // keeps its default (and still applies GLM's max default server-side).
      ...(effort && isClaudeCmd ? { effort } : {}),
      ...(role === 'orchestrator' && roleSupport.supported ? { role: 'orchestrator' as const } : {}),
      ...(commandAgent && (commandAgent === 'claude' || commandAgent === 'codex') && contextPack.trim()
        ? { contextPack: contextPack.trim() }
        : {}),
      ...(commandAgent && (commandAgent === 'claude' || commandAgent === 'codex') && workspaceDefaultContextPack && !contextPack.trim() && !inheritDefault
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
  const selectedAgentLabel = agentLabel(selectedAgent);
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

        {!cwdLoading && (
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
              setKind(defaultKindFor(next));
            }}
            placeholder="$SHELL"
            className="dialog-control"
          />
          <div className="dialog-chip-row">
            {/* shells (default + explicit alternatives) */}
            <button
              type="button"
              onClick={() => { setCmd(SHELL_DEFAULT); setKind(''); setRole(''); }}
              className={`dialog-chip${isDefaultShell ? ' dialog-chip-strong' : ''}`}
              title="Use $SHELL (your default shell)"
            >shell</button>
            {['bash', 'zsh', 'pwsh.exe', 'powershell.exe'].map((c) => (
              <button
                type="button"
                key={c}
                onClick={() => { setCmd(c); setKind(''); setRole(''); }}
                className={`dialog-chip${cmd.trim() === c ? ' dialog-chip-strong' : ''}`}
                title={
                  c === 'pwsh.exe' ? 'PowerShell 7+ on Windows (via WSL interop)'
                    : c === 'powershell.exe' ? 'Windows PowerShell 5.1 (via WSL interop)'
                    : undefined
                }
              >{c}</button>
            ))}
            <span className="dialog-chip-separator">·</span>
            {/* claude quick options — follow the user's default-renderer
                preference (Preferences → "New Claude sessions"). */}
            {(resumeChipAllowed ? ['claude', 'claude --resume'] : ['claude']).map((c) => (
              <button
                type="button"
                key={c}
                onClick={() => { if (selectedAgent !== 'claude') setRole(''); setCmd(c); setKind(defaultKindFor(c)); }}
                className={`dialog-chip${cmd.trim() === c && kind !== 'native' && !role ? ' dialog-chip-strong' : ''}`}
              >{c}</button>
            ))}
            {/* Explicit Focus toggle for the claude command — no terminal→toggle
                step. Toggles kind:'native' (click again for a Terminal claude,
                which also lets a Focus-by-default user opt out per-session). */}
            {focusEnabled && (
            <button
              type="button"
              onClick={() => {
                const active = kind === 'native' && selectedAgent === 'claude';
                if (selectedAgent !== 'claude') setRole('');
                setCmd('claude');
                setKind(active ? '' : 'native');
              }}
              className={`dialog-chip${kind === 'native' && selectedAgent === 'claude' ? ' dialog-chip-strong' : ''}`}
              title="Open Claude directly in Focus (rendered) mode — click again for a Terminal claude"
            >claude · Focus</button>
            )}
            {/* The chief-of-staff preset: a claude pane spawned with the
                orchestrator brief, so it coordinates direct work / schedules
                while keeping the user informed. Renderer follows the same default
                as the plain claude chips. */}
            {orchestratorAllowed && (
              <button
                type="button"
                onClick={() => {
                  const active = role === 'orchestrator' && selectedAgent === 'claude';
                  setCmd('claude');
                  setKind(defaultKindFor('claude'));
                  setRole(active ? '' : 'orchestrator');
                }}
                className={`dialog-chip${role === 'orchestrator' && selectedAgent === 'claude' ? ' dialog-chip-strong' : ''}`}
                title="One Claude pane that investigates, builds, and coordinates other panes; you decide what merges. Click again for a plain claude"
              >claude · Orchestrator</button>
            )}
            <button
              type="button"
              onClick={() => { if (selectedAgent !== 'codex') setRole(''); setCmd('codex'); setKind(''); }}
              className={`dialog-chip${cmd.trim() === 'codex' && kind !== 'native' && !role ? ' dialog-chip-strong' : ''}`}
              title="Open Codex in Terminal mode"
            >codex</button>
            {focusEnabled && (
            <button
              type="button"
              onClick={() => {
                const active = kind === 'native' && selectedAgent === 'codex';
                if (selectedAgent !== 'codex') setRole('');
                setCmd('codex');
                setKind(active ? '' : 'native');
              }}
              className={`dialog-chip${kind === 'native' && selectedAgent === 'codex' ? ' dialog-chip-strong' : ''}`}
              title="Open Codex directly in Focus (rendered) mode — click again for Terminal mode"
            >codex · Focus</button>
            )}
            {orchestratorAllowed && (
              <button
                type="button"
                disabled={!codexOrchestratorSupport.available}
                onClick={() => {
                  const active = role === 'orchestrator' && selectedAgent === 'codex';
                  setCmd('codex');
                  setKind('');
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
                onClick={() => { setCmd('muse'); setKind(''); setRole(''); }}
                className={`dialog-chip${cmd.trim() === 'muse' && kind !== 'native' ? ' dialog-chip-strong' : ''}`}
                title="Open Muse Code (Meta) in Terminal mode"
              >muse</button>
            )}
            {orchestratorAllowed && focusEnabled && (
            <button
              type="button"
              onClick={() => {
                const active = kind === 'native' && selectedAgent === 'muse';
                setCmd('muse');
                setKind(active ? '' : 'native');
                setRole('');
              }}
              className={`dialog-chip${kind === 'native' && selectedAgent === 'muse' ? ' dialog-chip-strong' : ''}`}
              title="Open Muse directly in Focus (rendered) mode — click again for Terminal mode"
            >muse · Focus</button>
            )}
            <span className="dialog-chip-separator">·</span>
            {/* runtimes */}
            {['node', 'python3'].map((c) => (
              <button
                type="button"
                key={c}
                onClick={() => { setCmd(c); setKind(''); setRole(''); }}
                className={`dialog-chip${cmd.trim() === c ? ' dialog-chip-strong' : ''}`}
              >{c}</button>
            ))}
          </div>
          {orchestratorAllowed && hosts !== null && !codexOrchestratorSupport.available && (
            <div className="dialog-help">Codex Orchestrator unavailable: {codexOrchestratorSupport.reason}</div>
          )}
          {focusEnabled && kind === 'native' && (
            <div className="dialog-help" style={{ color: 'var(--accent)' }}>
              → Opens {selectedAgentLabel} in <strong>Focus</strong> (rendered) mode. The command stays
              “{selectedAgent ?? 'agent'}” — the session is shown as formatted prose
              instead of a raw terminal.
            </div>
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
            {submitting ? 'Creating…' : kind === 'native' && focusEnabled ? 'Create Focus session' : 'Create'}
          </button>
        </div>
      </form>
    </div>
  );
}
