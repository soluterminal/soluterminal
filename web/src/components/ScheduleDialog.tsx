import { useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { api, currentRoutingScope } from '../api';
import { useStore } from '../store';
import { useAccountFeatures, useOperatorFeatures } from '../entitlements';
import type { Schedule, ScheduleAgent, ScheduleChatRef, ScheduleInput, SchedulePermission } from '../types';
import { scheduleChatDelivery } from '../extensions';
import { SCHEDULE_TEMPLATES, templatePrompt, type ScheduleTemplate } from '../lib/scheduleTemplates';
import {
  HOUR_STEP_CHOICES,
  MINUTE_STEP_CHOICES,
  compileSpec,
  cronsEquivalent,
  defaultBuilderSpec,
  describeCron,
  looksLikeCron,
  parseCron,
  type BuilderSpec,
} from '../lib/cron';
import { parseNaturalSchedule } from '../lib/scheduleNL';
import { useUnsentInput } from '../lib/unsentInput';

// Add / edit modal for Schedules. A schedule is a trigger plus one of
// two job kinds:
//   • Prompt — inject text into a target pane's PTY on fire.
//   • Agent — run a built-in local agent (fix agent, PR babysitter) as
//     a child process on fire. Only offered when the daemon reports an
//     installed agent (operator machines) — everyone else never sees
//     the toggle.
//
// Trigger kinds:
//   • Recurring cron — "every hour", "weekdays at 9". Preset dropdown,
//     or "Custom…" which opens a structured recurrence builder (every N
//     minutes/hours, daily/weekly/monthly at a time) that compiles to a
//     5-field cron expression. Raw cron survives as a collapsed
//     "Advanced" editor for expressions the builder can't represent.
//   • One-time — fire once at a specific instant, then auto-disable.
//     "Remind me in 30 min" / "at 3pm tomorrow".
//
// UX choices:
//   • "Runs" picker collapses the trigger-kind choice into the same
//     dropdown the user's already looking at, so "run this once" is as
//     one-click as "every hour".
//   • A typed natural-language box above the picker ("every weekday at
//     9am", "in 30 min") pre-fills the picker — deterministic regex
//     parsing, so a wrong guess is always visible and correctable.
//   • Recurring schedules gain an optional end condition (after N runs
//     / until date) — without it, recurring jobs linger forever until
//     the user remembers to delete them.
//   • Target pane picker lists only RUNNING panes — scheduling against
//     a dead pane is almost always a mistake.

const NEEDS_CLAUDE = 'Needs Claude Code';
const NEEDS_CLAUDE_TITLE = 'Claude Code isn\'t installed on this computer. Install it to run Claude tasks, or use "Prompt into a pane" with a pane running another agent such as Codex.';
const CLAUDE_GONE_HINT = 'Claude Code isn\'t installed on this computer any more, so this task can\'t run until it is.';

interface Props {
  open: boolean;
  onClose: () => void;
  /** If set, we're editing this schedule rather than creating a new one. */
  editing?: Schedule | null;
  /** Pre-fill the target pane when creating (optional). */
  initialPaneId?: string | null;
  /** True only when the dialog was opened from a pane context menu. */
  fromPaneMenu?: boolean;
}

interface Preset {
  label: string;
  cron: string;
}

// Ordered from fine-grained to coarse. "Custom" and "Once" are handled
// separately — see the cronMode state machine.
const PRESETS: Preset[] = [
  { label: 'Every 5 minutes', cron: '*/5 * * * *' },
  { label: 'Every 15 minutes', cron: '*/15 * * * *' },
  { label: 'Every 30 minutes', cron: '*/30 * * * *' },
  { label: 'Every hour (on the hour)', cron: '0 * * * *' },
  { label: 'Every 2 hours', cron: '0 */2 * * *' },
  { label: 'Every 4 hours', cron: '0 */4 * * *' },
  { label: 'Every day at 9am', cron: '0 9 * * *' },
  { label: 'Every weekday at 9am', cron: '0 9 * * 1-5' },
  { label: 'Every Monday at 9am', cron: '0 9 * * 1' },
  { label: 'Every day at noon', cron: '0 12 * * *' },
  { label: 'Every day at midnight', cron: '0 0 * * *' },
];

// Preset lookup by MEANING, not string identity: the builder and the NL
// parser emit `0 9 * * 1,2,3,4,5` where PRESETS spells the same job
// `0 9 * * 1-5`. Without canonical comparison, typing "every weekday at 9am"
// would drop into the custom builder instead of selecting the shipped preset.
function findPreset(cron: string | undefined): Preset | undefined {
  if (!cron) return undefined;
  return PRESETS.find((p) => cronsEquivalent(p.cron, cron));
}

type CronMode = 'preset' | 'custom' | 'once';
type EndMode = 'forever' | 'max' | 'until';
type JobKind = 'task' | 'prompt' | 'agent';

// datetime-local input uses "YYYY-MM-DDTHH:MM" interpreted as the
// browser's local timezone. Round-trip helpers below go local-string ↔
// server ISO (always UTC on the wire).
function pad2(n: number) { return String(n).padStart(2, '0'); }

function dateToLocalInput(d: Date): string {
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

function localInputToISO(local: string): string {
  if (!local) return '';
  const d = new Date(local);
  if (isNaN(d.getTime())) return '';
  return d.toISOString();
}

function isoToLocalInput(iso: string | undefined): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '';
  return dateToLocalInput(d);
}

// Default suggestion for a fresh one-time pick: 1 hour from now,
// rounded down to the previous 5-minute mark so the input doesn't show
// awkward :23 / :47 timestamps on first open.
function defaultRunAt(): string {
  const d = new Date(Date.now() + 60 * 60 * 1000);
  d.setMinutes(d.getMinutes() - (d.getMinutes() % 5), 0, 0);
  return dateToLocalInput(d);
}

function defaultUntil(): string {
  const d = new Date();
  d.setDate(d.getDate() + 7);
  d.setHours(9, 0, 0, 0);
  return dateToLocalInput(d);
}

// "UTC+8" / "UTC+5:30" for the machine-timezone note.
function fmtUtcOffset(minutes: number): string {
  const sign = minutes < 0 ? '-' : '+';
  const abs = Math.abs(minutes);
  const h = Math.floor(abs / 60);
  const m = abs % 60;
  return `UTC${sign}${h}${m ? ':' + pad2(m) : ''}`;
}

// Monday-first: matches how this audience thinks about work weeks.
const WEEKDAY_CHIPS: Array<{ label: string; day: number }> = [
  { label: 'Mon', day: 1 },
  { label: 'Tue', day: 2 },
  { label: 'Wed', day: 3 },
  { label: 'Thu', day: 4 },
  { label: 'Fri', day: 5 },
  { label: 'Sat', day: 6 },
  { label: 'Sun', day: 0 },
];

export function ScheduleDialog({ open, onClose, editing, initialPaneId, fromPaneMenu = false }: Props) {
  const panes = useStore((s) => s.panes);
  const createSchedule = useStore((s) => s.createSchedule);
  const updateSchedule = useStore((s) => s.updateSchedule);
  const toast = useStore((s) => s.toast);

  const [name, setName] = useState('');
  const [cronMode, setCronMode] = useState<CronMode>('preset');
  const [presetCron, setPresetCron] = useState(PRESETS[3].cron); // every hour default
  const [customCron, setCustomCron] = useState('0 9 * * *');
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const [nlText, setNlText] = useState('');
  const [runAt, setRunAt] = useState(defaultRunAt());
  const [endMode, setEndMode] = useState<EndMode>('forever');
  const [maxFires, setMaxFires] = useState(10);
  const [until, setUntil] = useState(defaultUntil());
  const [paneId, setPaneId] = useState('');
  const [prompt, setPrompt] = useState('');
  useUnsentInput(open ? prompt : '');
  const [notifyOnRun, setNotifyOnRun] = useState(false);
  // Phone push needs a free account; the cloud fallback and chat delivery
  // are team-only on the public beta.
  const team = useOperatorFeatures();
  const phonePush = useAccountFeatures();
  // Once the user touches the notify checkbox (or we're editing a saved
  // schedule), stop auto-flipping it on mode changes.
  const notifyTouched = useRef(false);
  // Report delivery to a chat (task kind, an optional feature): null = off.
  const chatDelivery = scheduleChatDelivery();
  const [deliverChat, setDeliverChat] = useState<ScheduleChatRef | null>(null);
  const [enabled, setEnabled] = useState(true);
  const [saving, setSaving] = useState(false);
  const [jobKind, setJobKind] = useState<JobKind>('task');
  // A Claude task needs the Claude Code CLI on this computer. Only a finished
  // probe that found none counts (claudeChecked): an older daemon or the first
  // seconds after boot keep the task kind on offer, and the daemon refuses the
  // save anyway if Claude really is missing. Asked of the selected machine and
  // backend each time the dialog opens: the shared health poll can still hold
  // another machine's answer after a switch, or fall back to the other backend.
  const [claudeMissing, setClaudeMissing] = useState(false);
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setClaudeMissing(false);
    api.healthScoped(currentRoutingScope())
      .then((h) => { if (!cancelled) setClaudeMissing(h.claudeChecked === true && h.claudeInstalled === false); })
      .catch(() => {});
    return () => { cancelled = true; };
  }, [open]);
  // An existing task stays editable (Claude may come back); new ones can't be made.
  const taskUnavailable = claudeMissing && editing?.kind !== 'task';
  const taskUnavailableRef = useRef(taskUnavailable);
  taskUnavailableRef.current = taskUnavailable;
  // The health answer can land after the dialog opened on the task default.
  useEffect(() => {
    if (open && taskUnavailable) setJobKind((k) => (k === 'task' ? 'prompt' : k));
  }, [open, taskUnavailable]);
  const [agentId, setAgentId] = useState('');
  const [applyMode, setApplyMode] = useState(false);
  const [agents, setAgents] = useState<ScheduleAgent[]>([]);
  // kind=task
  const [cwd, setCwd] = useState('');
  const [permission, setPermission] = useState<SchedulePermission>('plan');
  const [timeoutMin, setTimeoutMin] = useState(30);
  const [catchUp, setCatchUp] = useState(false);
  const [showTemplates, setShowTemplates] = useState(false);

  // Built-in agents this daemon can run. Any failure (old daemon, no
  // endpoint) = empty list = the Agent kind is simply never offered.
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    api.listScheduleAgents()
      .then((a) => { if (!cancelled) setAgents(Array.isArray(a) ? a : []); })
      .catch(() => { if (!cancelled) setAgents([]); });
    return () => { cancelled = true; };
  }, [open]);

  const presentAgents = useMemo(() => agents.filter((a) => a.present), [agents]);

  // Non-gating "Connect GitHub" tip for agent schedules: the agent itself
  // runs on local gh auth, but the cockpit's reviewed-PRs / merge-ready view
  // needs the GitHub App connection. Only shown when the cloud actually has
  // an App configured and this user hasn't connected yet.
  const [ghAppConnectable, setGhAppConnectable] = useState(false);
  useEffect(() => {
    if (jobKind !== 'agent') return;
    let cancelled = false;
    api.githubAppStatus()
      .then((s) => { if (!cancelled) setGhAppConnectable(!!s.configured && !s.connected); })
      .catch(() => {});
    return () => { cancelled = true; };
  }, [jobKind]);
  // Offer the toggle only where an agent is installed; keep an editing
  // agent-schedule editable even if the fetch failed.
  const showKindToggle = presentAgents.length > 0 || editing?.kind === 'agent';

  // Reset when opening.
  useEffect(() => {
    if (!open) return;
    if (editing) {
      setName(editing.name);
      if (editing.runAt) {
        setCronMode('once');
        setRunAt(isoToLocalInput(editing.runAt) || defaultRunAt());
        setEndMode('forever'); // N/A for one-time
      } else {
        const matched = findPreset(editing.cron);
        if (matched) {
          setCronMode('preset');
          setPresetCron(matched.cron);
        } else {
          setCronMode('custom');
          setCustomCron(editing.cron || '0 9 * * *');
          // Builder-representable crons open in the builder; anything odder
          // (comma minute lists, dom steps) opens straight into the raw editor.
          setAdvancedOpen(parseCron(editing.cron || '') === null);
        }
        if (editing.maxFires && editing.maxFires > 0) {
          setEndMode('max');
          setMaxFires(editing.maxFires);
        } else if (editing.until) {
          setEndMode('until');
          setUntil(isoToLocalInput(editing.until) || defaultUntil());
        } else {
          setEndMode('forever');
        }
      }
      setPaneId(editing.paneId || '');
      setPrompt(editing.prompt || '');
      setNotifyOnRun(!!editing.notifyOnRun);
      notifyTouched.current = true; // saved value sticks across mode changes
      setEnabled(editing.enabled);
      setJobKind(editing.kind === 'agent' ? 'agent' : editing.kind === 'task' ? 'task' : 'prompt');
      setAgentId(editing.agentId || '');
      setApplyMode(!!editing.apply);
      setCwd(editing.cwd || '');
      setPermission(editing.permission === 'edit' ? 'edit' : 'plan');
      setTimeoutMin(editing.timeoutMin || 30);
      setCatchUp(!!editing.catchUp);
      setDeliverChat(editing.deliverChat ?? null);
      setShowTemplates(false);
    } else {
      setName('');
      setCronMode('preset');
      setPresetCron(PRESETS[3].cron);
      setCustomCron('0 9 * * *');
      setAdvancedOpen(false);
      setRunAt(defaultRunAt());
      setEndMode('forever');
      setMaxFires(10);
      setUntil(defaultUntil());
      setPaneId(initialPaneId || '');
      setPrompt('');
      setNotifyOnRun(false);
      notifyTouched.current = false;
      setEnabled(true);
      // A pane context-menu entry means the user already chose both the prompt
      // job and its destination. The global New schedule entry still defaults
      // to a fresh Claude task because it works unattended without a live pane,
      // unless this computer has no Claude Code to run it.
      setJobKind(fromPaneMenu || taskUnavailableRef.current ? 'prompt' : 'task');
      setAgentId('');
      setApplyMode(false);
      // Seed the working directory from the pane the user came from — the
      // overwhelmingly likely answer, and it saves a paste. Read imperatively:
      // `panes` re-identifies on every daemon poll, and depending on it here
      // would re-run this whole reset and wipe what the user is typing.
      const seedPane = (initialPaneId && useStore.getState().panes[initialPaneId]) || undefined;
      setCwd(seedPane ? (seedPane.liveCwd || seedPane.cwd || '') : '');
      setPermission('plan');
      setTimeoutMin(30);
      setCatchUp(false);
      setDeliverChat(null);
      setShowTemplates(!fromPaneMenu); // pane context menu opens the seeded prompt form directly
    }
    setNlText('');
  }, [open, editing, initialPaneId, fromPaneMenu]);

  // Recipe → dialog state. Leaves cwd/paneId alone (already seeded) so a
  // template never yanks the user out of the directory they came from.
  function applyTemplate(t: ScheduleTemplate) {
    const p = t.preset;
    setShowTemplates(false);
    setJobKind(p.kind);
    setName(p.name);
    setPrompt(templatePrompt(t, team));
    if (p.permission) setPermission(p.permission);
    if (p.notifyOnRun !== undefined) {
      notifyTouched.current = true;
      setNotifyOnRun(p.notifyOnRun);
    }
    if (p.cron) {
      const preset = findPreset(p.cron);
      if (preset) {
        setCronMode('preset');
        setPresetCron(preset.cron);
      } else {
        setCronMode('custom');
        setCustomCron(p.cron);
        setAdvancedOpen(parseCron(p.cron) === null);
      }
    } else {
      // No cron on the recipe (the "Remind me…" one-shot) → one-time mode.
      setCronMode('once');
      setRunAt(defaultRunAt());
    }
  }

  // Default the run-notification ON for one-time reminders — a reminder
  // that can't reach your phone isn't a reminder. Recurring defaults OFF
  // (errors push regardless). Stops the instant the user touches it.
  useEffect(() => {
    if (!open || notifyTouched.current) return;
    setNotifyOnRun(cronMode === 'once');
  }, [open, cronMode]);

  // Selecting an agent pre-fills its shipped cadence (the same one the
  // legacy systemd timer used) so the sensible default is one click.
  function chooseAgent(id: string) {
    setAgentId(id);
    const def = agents.find((a) => a.id === id);
    if (!def || editing) return;
    const preset = findPreset(def.defaultCron);
    if (preset) {
      setCronMode('preset');
      setPresetCron(preset.cron);
    } else if (def.defaultCron) {
      setCronMode('custom');
      setCustomCron(def.defaultCron);
      setAdvancedOpen(parseCron(def.defaultCron) === null);
    }
  }

  // ----- typed natural language ("every weekday at 9am", "in 30 min") -----
  // Each successful parse applies once; further manual tweaks to the picker
  // survive until the text MEANINGFULLY changes. The dedup key is normalized
  // the same way the parser normalizes, so adding a trailing space doesn't
  // re-apply the parse and clobber a picker change the user made since.
  const lastAppliedNL = useRef('');
  const nlKey = useMemo(() => nlText.trim().toLowerCase().replace(/\s+/g, ' '), [nlText]);
  const nlParsed = useMemo(() => parseNaturalSchedule(nlText), [nlText]);
  useEffect(() => {
    if (!nlParsed || lastAppliedNL.current === nlKey) return;
    lastAppliedNL.current = nlKey;
    if (nlParsed.kind === 'once') {
      setCronMode('once');
      setRunAt(dateToLocalInput(nlParsed.at));
      return;
    }
    const preset = findPreset(nlParsed.cron);
    if (preset) {
      setCronMode('preset');
      setPresetCron(preset.cron);
    } else {
      setCronMode('custom');
      setCustomCron(nlParsed.cron);
      setAdvancedOpen(false);
    }
  }, [nlParsed, nlKey]);

  // ----- recurrence builder -----
  // Single source of truth is the cron STRING; the builder is a live view
  // over parseCron(customCron). Builder edits recompile into the string, so
  // the two can never drift. A null spec = expression the builder can't
  // represent → the raw editor takes over.
  const builderSpec = useMemo(() => parseCron(customCron), [customCron]);

  function updateSpec(patch: Partial<BuilderSpec>) {
    const cur = builderSpec ?? defaultBuilderSpec();
    let next: BuilderSpec = { ...cur, ...patch };
    // Unit switches keep the time-of-day but need a sane N for the new unit.
    if (patch.unit && patch.unit !== cur.unit) {
      const choices = patch.unit === 'minutes' ? MINUTE_STEP_CHOICES
        : patch.unit === 'hours' ? HOUR_STEP_CHOICES : [1];
      if (!choices.includes(next.n)) {
        next = { ...next, n: patch.unit === 'minutes' ? 5 : 1 };
      }
    }
    if (next.weekdays.length === 0) next = { ...next, weekdays: cur.weekdays.length ? cur.weekdays : [1] };
    setCustomCron(compileSpec(next));
  }

  function toggleWeekday(day: number) {
    const cur = builderSpec ?? defaultBuilderSpec();
    if (cur.weekdays.includes(day)) {
      // Never allow zero days — a weekly schedule that can't fire is a trap.
      if (cur.weekdays.length === 1) return;
      updateSpec({ weekdays: cur.weekdays.filter((d) => d !== day) });
    } else {
      updateSpec({ weekdays: [...cur.weekdays, day] });
    }
  }

  const effectiveCron = cronMode === 'preset' ? presetCron : customCron.trim();
  const cronValid = cronMode === 'once' ? true : looksLikeCron(effectiveCron);

  // runAt must be in the future; server would reject otherwise but we
  // want the "Create" button to gate on it inline.
  const runAtValid = useMemo(() => {
    if (cronMode !== 'once') return true;
    const iso = localInputToISO(runAt);
    if (!iso) return false;
    return new Date(iso).getTime() > Date.now();
  }, [cronMode, runAt]);

  // Live preview of the next 5 fire times for cron modes — catches
  // classic cron footguns (Sunday vs Monday numbering, `*/5 9 * * *`
  // vs `*/5 * * * *`) at creation time instead of 3 days later when
  // the schedule didn't fire when you expected. For one-time mode
  // "preview" is just the single RunAt we already render in the input.
  const [preview, setPreview] = useState<string[]>([]);
  const [previewErr, setPreviewErr] = useState<string | null>(null);
  // The daemon's timezone (cron runs in the MACHINE's wall clock). Only
  // surfaced when it differs from the browser's — the common same-machine
  // case stays quiet.
  const [previewTz, setPreviewTz] = useState<{ name: string; offsetMinutes: number } | null>(null);
  useEffect(() => {
    if (!open || cronMode === 'once') {
      setPreview([]);
      setPreviewErr(null);
      return;
    }
    if (!cronValid) {
      setPreview([]);
      setPreviewErr(null);
      return;
    }
    let cancelled = false;
    const h = setTimeout(() => {
      api.previewCron(effectiveCron, 5)
        .then((r) => {
          if (cancelled) return;
          setPreview(r.fires);
          setPreviewErr(null);
          setPreviewTz(
            typeof r.tzOffsetMinutes === 'number'
              ? { name: r.tzName || '', offsetMinutes: r.tzOffsetMinutes }
              : null,
          );
        })
        .catch((e) => {
          if (cancelled) return;
          setPreview([]);
          setPreviewErr((e as Error).message);
        });
    }, 200);
    return () => { cancelled = true; clearTimeout(h); };
  }, [open, cronMode, effectiveCron, cronValid]);

  const tzMismatch = previewTz !== null && previewTz.offsetMinutes !== -new Date().getTimezoneOffset();

  // Offer only running panes — scheduling against a dead pane just
  // fails at fire time with no useful behaviour. Sorted stably: the store
  // re-keys panes on every daemon event, so unsorted Object.values()
  // shuffles the dropdown under the user's cursor mid-selection.
  const runningPanes = useMemo(
    () =>
      Object.values(panes)
        .filter((p) => p.status === 'running')
        .sort(
          (a, b) =>
            (a.title || a.cmd.join(' ') || a.id).localeCompare(
              b.title || b.cmd.join(' ') || b.id,
            ) || a.id.localeCompare(b.id),
        ),
    [panes],
  );

  // Distinct cwds of live panes: the directories the user actually works in,
  // offered as one-click choices for a task's working directory.
  const cwdSuggestions = useMemo(() => {
    const seen = new Set<string>();
    for (const p of Object.values(panes)) {
      const dir = p.liveCwd || p.cwd;
      if (dir) seen.add(dir);
    }
    return [...seen].sort();
  }, [panes]);

  function setRunAtOffset(ms: number) {
    const d = new Date(Date.now() + ms);
    d.setSeconds(0, 0);
    setRunAt(dateToLocalInput(d));
  }

  function setRunAtTomorrow9am() {
    const d = new Date();
    d.setDate(d.getDate() + 1);
    d.setHours(9, 0, 0, 0);
    setRunAt(dateToLocalInput(d));
  }

  async function save() {
    if (jobKind === 'agent') {
      if (!agentId) { toast('Pick an agent', { kind: 'warn', icon: '⚠' }); return; }
    } else if (jobKind === 'task') {
      if (!cwd.trim()) { toast('Pick a working directory', { kind: 'warn', icon: '⚠' }); return; }
      if (!prompt.trim()) { toast('Task is empty', { kind: 'warn', icon: '⚠' }); return; }
      if (deliverChat && !deliverChat.chatId) { toast(chatDelivery?.missingChoice ?? 'Pick the chat for the report', { kind: 'warn', icon: '⚠' }); return; }
    } else {
      if (!paneId) { toast('Pick a target pane', { kind: 'warn', icon: '⚠' }); return; }
      if (!prompt.trim()) { toast('Prompt is empty', { kind: 'warn', icon: '⚠' }); return; }
    }
    if (cronMode !== 'once' && !cronValid) { toast('Schedule looks wrong — check the repeat settings', { kind: 'warn', icon: '⚠' }); return; }
    if (cronMode === 'once' && !runAtValid) { toast('Pick a time in the future', { kind: 'warn', icon: '⚠' }); return; }

    const input: ScheduleInput =
      jobKind === 'agent'
        ? {
            name: name.trim() || undefined,
            kind: 'agent',
            agentId,
            apply: applyMode,
            notifyOnRun,
            enabled,
          }
        : jobKind === 'task'
          ? {
              name: name.trim() || undefined,
              kind: 'task',
              cwd: cwd.trim(),
              prompt,
              permission,
              timeoutMin: timeoutMin === 30 ? undefined : timeoutMin,
              notifyOnRun,
              deliverChat: deliverChat ?? undefined,
              // catchUp is recurring-only server-side; a once-mode save must
              // not carry a stale true from an earlier mode switch.
              catchUp: cronMode !== 'once' && catchUp,
              enabled,
            }
          : {
              name: name.trim() || undefined,
              paneId,
              prompt,
              notifyOnRun,
              enabled,
            };

    if (cronMode === 'once') {
      input.runAt = localInputToISO(runAt);
    } else {
      input.cron = effectiveCron;
      if (endMode === 'max') {
        if (maxFires < 1) { toast('"Stop after" needs at least 1 run', { kind: 'warn', icon: '⚠' }); return; }
        input.maxFires = maxFires;
      } else if (endMode === 'until') {
        const iso = localInputToISO(until);
        if (!iso || new Date(iso).getTime() <= Date.now()) {
          toast('"Until" must be in the future', { kind: 'warn', icon: '⚠' });
          return;
        }
        input.until = iso;
      }
    }

    setSaving(true);
    try {
      if (editing) {
        await updateSchedule(editing.id, input);
        toast('Schedule updated', { kind: 'success', icon: '✓', ttlMs: 1500 });
      } else {
        await createSchedule(input);
        toast('Schedule created', { kind: 'success', icon: '✓', ttlMs: 1500 });
      }
      onClose();
    } catch (e) {
      toast(`Save failed: ${(e as Error).message}`, { kind: 'error', icon: '⚠' });
    } finally {
      setSaving(false);
    }
  }

  if (!open) return null;

  const runsValue =
    cronMode === 'once' ? '__once__'
    : cronMode === 'custom' ? '__custom__'
    : presetCron;

  const spec = builderSpec;
  const nlHint = nlParsed
    ? (nlParsed.kind === 'once'
        ? `once · ${new Date(nlParsed.at).toLocaleString(undefined, { weekday: 'short', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}`
        : findPreset(nlParsed.cron)?.label.toLowerCase() || describeCron(nlParsed.cron) || nlParsed.cron)
    : null;

  // Portaled to <body>: opened from the sidebar section, which on mobile sits
  // inside the transformed drawer — a transform makes the drawer the containing
  // block for position:fixed, so without the portal the dialog rendered
  // squeezed inside the drawer box on phones.
  return createPortal(
    <div className="dialog-backdrop" onMouseDown={onClose}>
      <div
        className="dialog schedule-dialog"
        onMouseDown={(e) => e.stopPropagation()}
        style={{ width: 560, maxWidth: '92vw' }}
      >
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <h2>{editing ? 'Edit schedule' : 'New schedule'}</h2>
          <button
            onClick={onClose}
            title="Close (Esc)"
            aria-label="Close"
            style={{ padding: '1px 8px' }}
          >×</button>
        </div>

        {/* Recipe shelf — the answer to "what would I even schedule?".
            REPLACES the form on a fresh dialog; picking one (or "start from
            scratch") reveals the form prefilled. Never shown when editing. */}
        {showTemplates && (
          <>
            <div>
              <label>Start with a recipe</label>
              <div style={{ display: 'flex', flexDirection: 'column', gap: 3, marginTop: 3 }}>
                {SCHEDULE_TEMPLATES.map((t) => {
                  const needsClaude = taskUnavailable && t.preset.kind === 'task';
                  return (
                  <button
                    key={t.id}
                    type="button"
                    onClick={() => applyTemplate(t)}
                    disabled={needsClaude}
                    title={needsClaude ? NEEDS_CLAUDE_TITLE : undefined}
                    style={{
                      display: 'flex',
                      alignItems: 'baseline',
                      gap: 8,
                      textAlign: 'left',
                      padding: '6px 8px',
                      fontSize: 12,
                    }}
                  >
                    <span style={{ fontSize: 14 }}>{t.emoji}</span>
                    <span style={{ fontWeight: 600, whiteSpace: 'nowrap' }}>{t.title}</span>
                    <span style={{ color: 'var(--fg-dim)', fontSize: 11, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                      {needsClaude ? NEEDS_CLAUDE : t.blurb}
                    </span>
                  </button>
                  );
                })}
              </div>
            </div>
            <div className="row" style={{ marginTop: 10 }}>
              <button onClick={onClose}>Cancel</button>
              <button className="primary" onClick={() => setShowTemplates(false)}>
                Start from scratch →
              </button>
            </div>
          </>
        )}

        {!showTemplates && (<>
          <div>
            <label>Job</label>
            <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
              <label
                style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12, opacity: taskUnavailable ? 0.55 : undefined }}
                title={taskUnavailable ? NEEDS_CLAUDE_TITLE : undefined}
              >
                <input type="radio" name="jobKind" checked={jobKind === 'task'} disabled={taskUnavailable} onChange={() => setJobKind('task')} />
                <span>Run a Claude task</span>
                {taskUnavailable && <span style={{ color: 'var(--fg-dim)', fontSize: 10 }}>· {NEEDS_CLAUDE}</span>}
              </label>
              <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12 }}>
                <input type="radio" name="jobKind" checked={jobKind === 'prompt'} onChange={() => setJobKind('prompt')} />
                <span>Prompt into a pane</span>
              </label>
              {showKindToggle && (
                <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12 }}>
                  <input type="radio" name="jobKind" checked={jobKind === 'agent'} onChange={() => { setJobKind('agent'); if (!agentId && presentAgents[0]) chooseAgent(presentAgents[0].id); }} />
                  <span>Run an agent</span>
                </label>
              )}
            </div>
            <div style={{ fontSize: 10, color: 'var(--fg-dim)', marginTop: 3 }}>
              {jobKind === 'task'
                ? (claudeMissing
                    ? CLAUDE_GONE_HINT
                    : 'A fresh Claude session each run — no pane needed, works while you\'re away.')
                : jobKind === 'prompt'
                  ? (claudeMissing
                      ? 'Types into a pane you already have open, such as a Codex pane. Skipped if that pane is busy.'
                      : 'Types into a pane you already have open. Skipped if that pane is busy.')
                  : 'Runs a built-in local agent as a background process.'}
            </div>
          </div>

        <div>
          <label>Name <span style={{ color: 'var(--fg-dim)' }}>(optional)</span></label>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={jobKind === 'agent' ? 'Defaults to the agent name' : 'Auto-derived from prompt if blank'}
            style={{ width: '100%' }}
          />
        </div>

        <div>
          <label>Runs</label>
          <input
            value={nlText}
            onChange={(e) => setNlText(e.target.value)}
            placeholder='Type it — "every weekday at 9am", "in 30 min", "tomorrow 3pm"'
            style={{ width: '100%', marginBottom: 6 }}
          />
          {nlText.trim().length >= 4 && (
            <div style={{ fontSize: 10, marginTop: -3, marginBottom: 6, color: 'var(--fg-dim)' }}>
              {nlHint ? <>↳ {nlHint} — applied below</> : <>not recognized — try “every weekday at 9am” or “in 30 min”</>}
            </div>
          )}
          <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
            <select
              aria-label="Runs"
              value={runsValue}
              onChange={(e) => {
                const v = e.target.value;
                if (v === '__custom__') {
                  // Carry the selected preset into the builder so "Every day
                  // at 9am → Custom…" starts from 9am, not from scratch.
                  if (cronMode === 'preset' && parseCron(presetCron)) setCustomCron(presetCron);
                  setCronMode('custom');
                  setAdvancedOpen(false);
                } else if (v === '__once__') setCronMode('once');
                else {
                  setCronMode('preset');
                  setPresetCron(v);
                }
              }}
              style={{ flex: 1 }}
            >
              <option value="__once__">Once — pick a time…</option>
              {PRESETS.map((p) => (
                <option key={p.cron} value={p.cron}>{p.label}</option>
              ))}
              <option value="__custom__">Custom…</option>
            </select>
          </div>

          {cronMode === 'custom' && (
            <div style={{ marginTop: 6, display: 'flex', flexDirection: 'column', gap: 6 }}>
              {spec ? (
                <>
                  <div style={{ display: 'flex', gap: 6, alignItems: 'center', flexWrap: 'wrap', fontSize: 12 }}>
                    <span>Every</span>
                    {(spec.unit === 'minutes' || spec.unit === 'hours') && (
                      <select
                        value={spec.n}
                        onChange={(e) => updateSpec({ n: parseInt(e.target.value, 10) })}
                      >
                        {(spec.unit === 'minutes' ? MINUTE_STEP_CHOICES : HOUR_STEP_CHOICES).map((n) => (
                          <option key={n} value={n}>{n}</option>
                        ))}
                      </select>
                    )}
                    <select
                      value={spec.unit}
                      onChange={(e) => updateSpec({ unit: e.target.value as BuilderSpec['unit'] })}
                    >
                      <option value="minutes">{spec.n === 1 ? 'minute' : 'minutes'}</option>
                      <option value="hours">{spec.n === 1 ? 'hour' : 'hours'}</option>
                      <option value="days">day</option>
                      <option value="weeks">week</option>
                      <option value="months">month</option>
                    </select>
                    {spec.unit === 'hours' && (
                      <>
                        <span>at minute</span>
                        <input
                          type="number"
                          min={0}
                          max={59}
                          value={spec.minute}
                          onChange={(e) => updateSpec({ minute: Math.min(59, Math.max(0, parseInt(e.target.value, 10) || 0)) })}
                          style={{ width: 56 }}
                        />
                      </>
                    )}
                    {spec.unit === 'months' && (
                      <>
                        <span>on day</span>
                        <select
                          value={spec.monthday}
                          onChange={(e) => updateSpec({ monthday: parseInt(e.target.value, 10) })}
                        >
                          {Array.from({ length: 31 }, (_, i) => i + 1).map((d) => (
                            <option key={d} value={d}>{d}</option>
                          ))}
                        </select>
                      </>
                    )}
                    {(spec.unit === 'days' || spec.unit === 'weeks' || spec.unit === 'months') && (
                      <>
                        <span>at</span>
                        <input
                          type="time"
                          value={`${pad2(spec.hour)}:${pad2(spec.minute)}`}
                          onChange={(e) => {
                            const [h, m] = e.target.value.split(':').map((x) => parseInt(x, 10));
                            if (!isNaN(h) && !isNaN(m)) updateSpec({ hour: h, minute: m });
                          }}
                        />
                      </>
                    )}
                  </div>
                  {spec.unit === 'weeks' && (
                    <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                      {WEEKDAY_CHIPS.map((c) => (
                        <button
                          key={c.day}
                          type="button"
                          className={spec.weekdays.includes(c.day) ? 'primary' : ''}
                          onClick={() => toggleWeekday(c.day)}
                          style={{ fontSize: 11, padding: '2px 8px' }}
                        >{c.label}</button>
                      ))}
                    </div>
                  )}
                  {spec.unit === 'months' && spec.monthday > 28 && (
                    <div style={{ fontSize: 10, color: 'var(--fg-dim)' }}>
                      Months without a day {spec.monthday} are skipped.
                    </div>
                  )}
                  <button
                    type="button"
                    onClick={() => setAdvancedOpen((v) => !v)}
                    style={{ alignSelf: 'flex-start', fontSize: 10, padding: '1px 6px', color: 'var(--fg-dim)', background: 'none', border: 'none', cursor: 'pointer' }}
                  >{advancedOpen ? '▾' : '▸'} Advanced: edit as cron</button>
                </>
              ) : (
                <div style={{ fontSize: 10, color: 'var(--fg-dim)' }}>
                  This expression is beyond the simple builder — edit the cron directly.
                </div>
              )}
              {(advancedOpen || !spec) && (
                <div>
                  <input
                    value={customCron}
                    onChange={(e) => setCustomCron(e.target.value)}
                    placeholder="minute hour dom month dow  (e.g. 0 9 * * 1-5)"
                    style={{
                      width: '100%',
                      fontFamily: "'SF Mono','Menlo',monospace",
                      borderColor: cronValid ? undefined : 'var(--danger)',
                    }}
                  />
                  <div style={{ fontSize: 10, color: 'var(--fg-dim)', marginTop: 3 }}>
                    5 fields: minute · hour · day-of-month · month · day-of-week.
                    Use <code>*/N</code> for every N, <code>A-B</code> for range,
                    <code>A,B</code> for list. Runs in this machine&apos;s timezone.
                    Note: <code>dow</code> is 0=Sunday, 1=Monday, …, 6=Saturday.
                  </div>
                </div>
              )}
            </div>
          )}

          {cronMode === 'once' && (
            <div style={{ marginTop: 6 }}>
              <input
                type="datetime-local"
                value={runAt}
                onChange={(e) => setRunAt(e.target.value)}
                style={{
                  fontFamily: "'SF Mono','Menlo',monospace",
                  borderColor: runAtValid ? undefined : 'var(--danger)',
                }}
              />
              <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap', marginTop: 6 }}>
                <button type="button" onClick={() => setRunAtOffset(15 * 60 * 1000)} style={{ fontSize: 11, padding: '2px 8px' }}>+15 min</button>
                <button type="button" onClick={() => setRunAtOffset(30 * 60 * 1000)} style={{ fontSize: 11, padding: '2px 8px' }}>+30 min</button>
                <button type="button" onClick={() => setRunAtOffset(60 * 60 * 1000)} style={{ fontSize: 11, padding: '2px 8px' }}>+1 hour</button>
                <button type="button" onClick={() => setRunAtOffset(4 * 60 * 60 * 1000)} style={{ fontSize: 11, padding: '2px 8px' }}>+4 hours</button>
                <button type="button" onClick={setRunAtTomorrow9am} style={{ fontSize: 11, padding: '2px 8px' }}>Tomorrow 9am</button>
              </div>
              <div style={{ fontSize: 10, color: 'var(--fg-dim)', marginTop: 3 }}>
                Runs once at this time, then turns itself off. Times are in your local timezone.
                {team && notifyOnRun && ' If this machine is off at that moment, the reminder is delivered to your phone from the cloud instead.'}
              </div>
            </div>
          )}

          {/* Live fire-time preview for cron modes. One-time just shows
              the instant the user already typed, so we skip it. */}
          {cronMode !== 'once' && (
            <div className="schedule-preview">
              {previewErr && (
                <div className="schedule-preview-err">Invalid cron: {previewErr}</div>
              )}
              {!previewErr && preview.length > 0 && (
                <>
                  <div className="schedule-preview-label">Next {preview.length} runs:</div>
                  <ul className="schedule-preview-list">
                    {preview.map((iso, i) => (
                      <li key={i}>
                        <span className="schedule-preview-rel">{formatFireRel(iso)}</span>
                        <span className="schedule-preview-sep">·</span>
                        <span className="schedule-preview-abs">{formatFireAbs(iso)}</span>
                      </li>
                    ))}
                  </ul>
                  {tzMismatch && previewTz && (
                    <div style={{ fontSize: 10, color: 'var(--fg-dim)', marginTop: 3 }}>
                      Runs in this machine&apos;s timezone
                      ({previewTz.name ? `${previewTz.name}, ` : ''}{fmtUtcOffset(previewTz.offsetMinutes)});
                      times above are shown in yours.
                    </div>
                  )}
                </>
              )}
            </div>
          )}
        </div>

        {/* End-condition section — recurring only. Without a limit,
            recurring schedules linger forever; 80% of "every 30 min"
            jobs really mean "every 30 min for the next few hours". */}
        {cronMode !== 'once' && (
          <div>
            <label>End condition</label>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
              <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12 }}>
                <input type="radio" name="endMode" checked={endMode === 'forever'} onChange={() => setEndMode('forever')} />
                <span>Run forever (until I disable it)</span>
              </label>
              <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12 }}>
                <input type="radio" name="endMode" checked={endMode === 'max'} onChange={() => setEndMode('max')} />
                <span>Stop after</span>
                <input
                  type="number"
                  min={1}
                  value={maxFires}
                  onChange={(e) => setMaxFires(Math.max(1, parseInt(e.target.value, 10) || 1))}
                  disabled={endMode !== 'max'}
                  style={{ width: 60 }}
                />
                <span>runs</span>
              </label>
              <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12 }}>
                <input type="radio" name="endMode" checked={endMode === 'until'} onChange={() => setEndMode('until')} />
                <span>Stop at</span>
                <input
                  type="datetime-local"
                  value={until}
                  onChange={(e) => setUntil(e.target.value)}
                  disabled={endMode !== 'until'}
                  style={{ fontFamily: "'SF Mono','Menlo',monospace" }}
                />
              </label>
            </div>
          </div>
        )}

        {jobKind === 'task' && (
          <div>
            <label>Working directory</label>
            <input
              aria-label="Working directory"
              value={cwd}
              onChange={(e) => setCwd(e.target.value)}
              placeholder="/home/you/project"
              list="sched-cwd-suggestions"
              style={{ width: '100%', fontFamily: "'SF Mono','Menlo',monospace", fontSize: 12 }}
            />
            <datalist id="sched-cwd-suggestions">
              {cwdSuggestions.map((d) => <option key={d} value={d} />)}
            </datalist>
            {cwdSuggestions.length > 0 && (
              <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap', marginTop: 4 }}>
                {cwdSuggestions.slice(0, 4).map((d) => (
                  <button
                    key={d}
                    type="button"
                    onClick={() => setCwd(d)}
                    title={d}
                    style={{ fontSize: 10, padding: '2px 6px', maxWidth: 160, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                  >{d.replace(/^\/home\/[^/]+/, '~')}</button>
                ))}
              </div>
            )}

            <label style={{ marginTop: 8 }}>Permission</label>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
              <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12 }}>
                <input type="radio" name="perm" checked={permission === 'plan'} onChange={() => setPermission('plan')} />
                <span>Read &amp; plan only <span style={{ color: 'var(--fg-dim)' }}>— changes nothing</span></span>
              </label>
              <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12 }}>
                <input type="radio" name="perm" checked={permission === 'edit'} onChange={() => setPermission('edit')} />
                <span>Allow edits <span style={{ color: 'var(--fg-dim)' }}>— Claude may change files in this directory</span></span>
              </label>
            </div>
            {permission === 'edit' && (
              <div style={{ fontSize: 10, color: 'var(--danger)', marginTop: 3 }}>
                This runs unattended and can modify your working tree. Commit or stash first.
              </div>
            )}

            <label style={{ marginTop: 8, display: 'flex', alignItems: 'center', gap: 6, fontSize: 12 }}>
              <span>Give up after</span>
              <input
                type="number"
                min={1}
                max={240}
                value={timeoutMin}
                onChange={(e) => setTimeoutMin(Math.min(240, Math.max(1, parseInt(e.target.value, 10) || 30)))}
                style={{ width: 64 }}
              />
              <span>minutes</span>
            </label>

            {cronMode !== 'once' && (
              <label style={{ marginTop: 6, display: 'flex', alignItems: 'center', gap: 6, fontSize: 12 }}>
                <input
                  type="checkbox"
                  checked={catchUp}
                  onChange={(e) => setCatchUp(e.target.checked)}
                />
                <span>
                  If runs were missed while the app was off, run once at startup
                  <span style={{ color: 'var(--fg-dim)' }}> (one run, not one per miss)</span>
                </span>
              </label>
            )}
          </div>
        )}

        {jobKind === 'task' && (
          <div>
            <label>Task</label>
            <textarea
              value={prompt}
              onChange={(e) => setPrompt(e.target.value)}
              placeholder="What should Claude do each run? e.g. &quot;Run the tests and report only what failed.&quot;"
              rows={6}
              style={{
                width: '100%',
                fontFamily: "'SF Mono','Menlo',monospace",
                fontSize: 12,
                resize: 'vertical',
              }}
            />
            <div style={{ fontSize: 10, color: 'var(--fg-dim)', marginTop: 3 }}>
              Each run starts a fresh session with no memory of the last one — say
              everything it needs to know. Output lands in this schedule&apos;s run history.
            </div>
          </div>
        )}

        {jobKind === 'agent' && (
          <div>
            <label>Agent</label>
            <select
              value={agentId}
              onChange={(e) => chooseAgent(e.target.value)}
              style={{ width: '100%' }}
            >
              <option value="">— pick an agent —</option>
              {presentAgents.map((a) => (
                <option key={a.id} value={a.id}>{a.title}</option>
              ))}
              {/* Editing a schedule whose agent isn't in the fetched list
                  (fetch failed / agent uninstalled): keep it selectable. */}
              {agentId && !presentAgents.some((a) => a.id === agentId) && (
                <option value={agentId}>{agentId}</option>
              )}
            </select>
            <label style={{ display: 'flex', alignItems: 'center', gap: 6, marginTop: 6, fontSize: 12 }}>
              <input
                type="checkbox"
                checked={applyMode}
                onChange={(e) => setApplyMode(e.target.checked)}
              />
              <span>Apply mode — the agent posts comments / opens PRs</span>
            </label>
            <div style={{ fontSize: 10, color: 'var(--fg-dim)', marginTop: 3 }}>
              {applyMode
                ? 'The agent writes to GitHub on each run.'
                : 'Dry-run: the agent plans and writes previews but posts nothing.'}
              {' '}The agent keeps its own single-flight lock and kill switch; runs land in the row's history.
            </div>
            {ghAppConnectable && (
              <div style={{ fontSize: 10, color: 'var(--fg-dim)', marginTop: 3 }}>
                Tip: Connect GitHub (◇ agents popover in the status bar) so this
                agent&apos;s reviewed PRs and merge-ready status show in the cockpit.
              </div>
            )}
          </div>
        )}

        {jobKind === 'prompt' && (
        <div>
          <label>Target pane</label>
          <select
            aria-label="Target pane"
            value={paneId}
            onChange={(e) => setPaneId(e.target.value)}
            style={{ width: '100%' }}
          >
            <option value="">— pick a running pane —</option>
            {runningPanes.map((p) => (
              <option key={p.id} value={p.id}>
                {p.title || p.cmd.join(' ')} · {(p.liveCwd || p.cwd).replace(/^\/home\/[^/]+/, '~')}
                {p.kind === 'native' ? ' · Focus' : p.hasClaude ? ' · claude' : ''}
              </option>
            ))}
          </select>
          {runningPanes.length === 0 && (
            <div style={{ fontSize: 10, color: 'var(--fg-dim)', marginTop: 3 }}>
              No running panes — create one first, then schedule.
            </div>
          )}
        </div>
        )}

        {jobKind === 'prompt' && (
        <div>
          <label>Prompt</label>
          <textarea
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            placeholder="Text to inject at each run — e.g. &quot;Summarize open PRs and post the TL;DR.&quot;"
            rows={6}
            style={{
              width: '100%',
              fontFamily: "'SF Mono','Menlo',monospace",
              fontSize: 12,
              resize: 'vertical',
            }}
          />
          <div style={{ fontSize: 10, color: 'var(--fg-dim)', marginTop: 3 }}>
            A trailing Enter (<code>\r</code>) is added automatically so Claude submits the input.
          </div>
        </div>
        )}

        {/* The push also reaches desktop notifications, so public installs
            keep the option without the phone wording. */}
        <label style={{ display: 'flex', alignItems: 'center', gap: 6, marginTop: 4 }}>
          <input
            type="checkbox"
            checked={notifyOnRun}
            onChange={(e) => { notifyTouched.current = true; setNotifyOnRun(e.target.checked); }}
          />
          <span style={{ fontSize: 12 }}>
            {phonePush ? 'Notify my phone when this runs' : 'Notify me when this runs'}
            <span style={{ color: 'var(--fg-dim)' }}> (failures always notify)</span>
          </span>
        </label>

        {team && jobKind === 'task' && chatDelivery && (
          <>
            <label style={{ display: 'flex', alignItems: 'center', gap: 6, marginTop: 4 }}>
              <input
                type="checkbox"
                checked={deliverChat !== null}
                onChange={(e) => setDeliverChat(e.target.checked ? { source: '', chatId: '' } : null)}
              />
              <span style={{ fontSize: 12 }}>
                {chatDelivery.label}
                {chatDelivery.hint && <span style={{ color: 'var(--fg-dim)' }}> {chatDelivery.hint}</span>}
              </span>
            </label>
            {deliverChat !== null && (
              <div style={{ marginLeft: 22, marginTop: 4 }}>
                <chatDelivery.Picker value={deliverChat} onChange={setDeliverChat} />
              </div>
            )}
          </>
        )}

        <label style={{ display: 'flex', alignItems: 'center', gap: 6, marginTop: 4 }}>
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          <span style={{ fontSize: 12 }}>Enabled</span>
        </label>

        <div className="row" style={{ marginTop: 10 }}>
          <button onClick={onClose} disabled={saving}>Cancel</button>
          <button
            className="primary"
            onClick={save}
            disabled={saving || (cronMode === 'once' ? !runAtValid : !cronValid)}
          >
            {saving ? 'Saving…' : editing ? 'Save' : 'Create'}
          </button>
        </div>
        </>)}
      </div>
    </div>,
    document.body,
  );
}

// Humanized "in N" for a fire-time preview row. Shorter than
// relativeTime() elsewhere — we expect fires in the near future, not
// "2 days ago", so we lean into "in 23m" / "in 3h" / "in 4d".
function formatFireRel(iso: string): string {
  const t = new Date(iso).getTime();
  if (isNaN(t)) return '—';
  const diff = t - Date.now();
  if (diff <= 0) return 'imminent';
  const sec = Math.round(diff / 1000);
  if (sec < 60) return `in ${sec}s`;
  if (sec < 3600) return `in ${Math.round(sec / 60)}m`;
  if (sec < 86400) return `in ${Math.round(sec / 3600)}h`;
  return `in ${Math.round(sec / 86400)}d`;
}

// Short weekday + HH:MM for same-week fires; full date for further out.
// Matches the kind of cognitive question a user actually has at this
// stage: "is that Monday?" not "is that October 17 2026?".
function formatFireAbs(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '—';
  const now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  const hhmm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
  if (sameDay) return `today ${hhmm}`;
  const weekday = d.toLocaleDateString(undefined, { weekday: 'short' });
  const diffDays = Math.round((d.getTime() - now.getTime()) / 86400000);
  if (diffDays >= 0 && diffDays < 7) return `${weekday} ${hhmm}`;
  return d.toLocaleString(undefined, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  });
}
