// NativeEventViews.tsx — the per-event-type render pieces of the native pane
// transcript. NativePane folds the NativeEvent stream and dispatches each row
// here. Sub-components are deliberately small + presentational; the only side
// effect is the approval card's resolve buttons, wired to a callback from the
// hook (idempotent server-side, so disable-on-click is safe).

import { memo, useContext, useEffect, useRef, useState, type MouseEvent as ReactMouseEvent } from 'react';
import type { NativeEvent } from '../../native/nativeEvents';
import { NativeFindContext } from '../../native/nativeFindContext';
import type { NativeMetaBarFields } from '../../native/metaBarFields';
import { fmtDuration, isUserDenial, isErroredResult, isTransientOverload } from '../../native/nativeTranscript';
import type { SlashCommand, Shortcut } from '../../native/nativeComposer';
import { modeLabel, fromKindLabel, pendingSwitchLabel } from '../../native/modeSwitch';
import { sameModelId } from '../../native/modelInfo';
import { formatRateLimitReset } from '../../native/footerVitals';
import type { ModelOption } from '../../native/modelInfo';
import type { PendingSwitch } from '../../native/modeSwitch';
import { Markdown } from './Markdown';
import { detectLang, tokenizeLine } from '../../lib/highlight';
import { RESTART_PAUSED_BODY, RESTART_PAUSED_TITLE } from '../../lib/restartPaused';
import { useRenderCount } from '../../native/nativePerf';
import { copyToClipboard } from '../../lib/clipboard';

// How many leading lines of tool output to show before the "show N more" fold.
const OUTPUT_PREVIEW_LINES = 15;

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// Best-effort one-line summary of a tool call's args. Bash → the command;
// file tools → the path; everything else → a compact JSON snippet. When
// `collapse` is set (the compact tool-call ROW, which is always a single
// ellipsized line), internal whitespace/newlines are flattened to single spaces
// so a multi-line BATCHED command (`a && \n  b && \n  c`) reads as one tidy line
// instead of forcing the row to several rows. The approval card leaves it false
// so the user reviews the command's real structure before approving.
function summarizeArgs(args: unknown, collapse = false): string {
  const out = (s: string) => (collapse ? s.replace(/\s+/g, ' ').trim() : s);
  if (args == null) return '';
  if (typeof args === 'string') return out(args);
  if (typeof args !== 'object') return String(args);
  const a = args as Record<string, unknown>;
  const pick = (k: string) => (typeof a[k] === 'string' ? (a[k] as string) : undefined);
  const command = pick('command');
  if (command) return out(command);
  const path = pick('file_path') ?? pick('path') ?? pick('filePath') ?? pick('notebook_path');
  const pattern = pick('pattern') ?? pick('query') ?? pick('url');
  const parts = [path, pattern].filter(Boolean) as string[];
  if (parts.length) return out(parts.join('  '));
  try {
    const json = JSON.stringify(a);
    return json.length > 80 ? json.slice(0, 80) + '…' : json;
  } catch {
    return '';
  }
}

// isBackgroundBash — a Bash tool_call launched with run_in_background:true. It
// returns IMMEDIATELY (the shell runs detached) and the agent must poll BashOutput
// on a LATER turn to read the result — so a "✓" on this row means "launched", NOT
// "finished". The row otherwise looks identical to a blocking $ bash, which hid the
// fire-and-forget nature (Term mode shows Claude's own background-shell chrome; the
// native renderer abstracted it away). Flag it so "I kicked X off in the background"
// is legible.
function isBackgroundBash(ev: NativeEvent): boolean {
  if ((ev.name || ev.toolName) !== 'Bash') return false;
  const a = ev.args;
  return !!(a && typeof a === 'object' && (a as Record<string, unknown>).run_in_background);
}

// BgBadge — the "background" pill on run_in_background bash(es), carrying live status:
// "running" by default (launched + detached, no completion seen yet), flipping to
// "exited <code>" once buildTranscript correlates a BashOutput/task-output result for
// the shell id (the `bgShell` annotation on the launch result). For a GROUP header that
// folds SEVERAL background shells it AGGREGATES — surfacing a failure ("N background · K
// failed", red) over a done/running summary, so a failed detached build is never hidden
// behind a sibling shell's exit 0. A non-zero exit tints danger. `lead` prefixes " · ".
function BgBadge({ shells, lead }: { shells: NativeEvent['bgShell'][]; lead?: boolean }) {
  const n = shells.length;
  if (n === 0) return null;
  const done = shells.filter(Boolean) as NonNullable<NativeEvent['bgShell']>[];
  const running = n - done.length;
  const failed = done.filter((s) => s.exitCode != null && s.exitCode !== 0);
  let label: string;
  if (n === 1) {
    const s = shells[0];
    label = s ? `background · exited${s.exitCode != null ? ` ${s.exitCode}` : ''}` : 'background · running';
  } else {
    // Failures take priority on the collapsed header (never hide a non-zero exit behind
    // a sibling's success); else "running" while any is still going; else "done".
    const suffix = failed.length ? `${failed.length} failed` : running > 0 ? 'running' : 'done';
    label = `${n} background · ${suffix}`;
  }
  return (
    <span
      className={`native-tool-bg${failed.length ? ' native-tool-bg-failed' : ''}`}
      title="Launched with run_in_background — runs detached; status is derived from its BashOutput/TaskOutput reads. A ✓ on launch means started, not finished."
    >
      {lead ? ' · ' : ''}
      {label}
    </span>
  );
}

// Pull a short error summary out of a tool_result: an exit code from the
// structured output, else the first non-empty line of the content/output.
function errorSummary(result?: NativeEvent): string {
  if (!result) return '';
  const out = result.output as Record<string, unknown> | undefined;
  if (out && typeof out === 'object') {
    const code = out.exitCode ?? out.exit_code;
    if (typeof code === 'number' && code !== 0) return `exit ${code}`;
  }
  const body = result.content || stringifyOutput(result.output);
  const first = body.split('\n').map((l) => l.trim()).find(Boolean);
  if (!first) return '';
  return first.length > 80 ? first.slice(0, 80) + '…' : first;
}

function toolKindGlyph(kind?: string): string {
  switch (kind) {
    case 'exec':
      return '$';
    case 'file':
      return '⎘';
    case 'search':
      return '⌕';
    case 'mcp':
      return '⚙';
    case 'todo':
      return '☑';
    default:
      return '›';
  }
}

// classifyApprovalRisk buckets a tool approval by how dangerous it is, so the UI
// can style + gate it. read-only tools (Read/Grep/…) get a calm accent;
// destructive tools (writes, shell mutations / redirects) keep the ⚠ danger
// styling AND are excluded from the Enter-to-approve keyboard shortcut (a
// keystroke must never auto-approve an `rm`/`Write`). Shared by ApprovalView and
// NativePane's keyboard handler so the two never disagree on what's destructive.
export function classifyApprovalRisk(
  toolName?: string,
  args?: unknown,
): { readOnly: boolean; destructive: boolean } {
  const tn = toolName || '';
  const summary = summarizeArgs(args);
  const readOnly = /^(Read|Grep|Glob|NotebookRead|WebFetch|WebSearch|LS)$/i.test(tn);
  const destructive =
    !readOnly &&
    (/^(Write|Edit|MultiEdit|NotebookEdit)$/i.test(tn) ||
      (/^Bash$/i.test(tn) && /\b(rm|mv|sudo|dd|mkfs)\b|>|>>/.test(summary)));
  return { readOnly, destructive };
}

// ---------------------------------------------------------------------------
// Diff preview for the file-edit tools (Write / Edit / MultiEdit) — the killer
// affordance: instead of dumping the raw JSON args, parse the tool input into a
// file path + a set of (old → new) replacements and render a real red/green
// line diff. Shared by the approval card AND the folded tool-call card so the
// preview is consistent whether you're about to approve or reviewing afterward.
// ---------------------------------------------------------------------------

// One change-hunk: a contiguous before/after pair. Write → a single hunk whose
// `before` is empty (pure addition). Edit → one hunk. MultiEdit → one per edit.
interface DiffHunk {
  before: string;
  after: string;
}
interface ParsedDiff {
  filePath: string;
  hunks: DiffHunk[];
}

function isFileDiffTool(toolName?: string): boolean {
  return /^(Write|Edit|MultiEdit)$/i.test(toolName || '');
}

// Parse a Write/Edit/MultiEdit tool input into a file path + change hunks.
// Returns null for any shape we don't recognize (caller falls back to the raw
// summary), so a malformed payload never throws or hides the approval.
// Exported for unit tests.
export function parseFileDiff(toolName: string | undefined, args: unknown): ParsedDiff | null {
  if (!args || typeof args !== 'object') return null;
  const a = args as Record<string, unknown>;
  const filePath =
    (typeof a.file_path === 'string' && a.file_path) ||
    (typeof a.filePath === 'string' && a.filePath) ||
    (typeof a.path === 'string' && a.path) ||
    '';
  const str = (v: unknown) => (typeof v === 'string' ? v : '');
  if (/^Write$/i.test(toolName || '')) {
    // Write replaces the whole file — render as all-additions (empty `before`).
    if (typeof a.content !== 'string') return null;
    return { filePath, hunks: [{ before: '', after: a.content }] };
  }
  if (/^Edit$/i.test(toolName || '')) {
    if (typeof a.old_string !== 'string' && typeof a.new_string !== 'string') return null;
    return { filePath, hunks: [{ before: str(a.old_string), after: str(a.new_string) }] };
  }
  if (/^MultiEdit$/i.test(toolName || '')) {
    if (!Array.isArray(a.edits)) return null;
    const hunks: DiffHunk[] = [];
    for (const e of a.edits) {
      if (!e || typeof e !== 'object') continue;
      const eo = e as Record<string, unknown>;
      if (typeof eo.old_string !== 'string' && typeof eo.new_string !== 'string') continue;
      hunks.push({ before: str(eo.old_string), after: str(eo.new_string) });
    }
    if (!hunks.length) return null;
    return { filePath, hunks };
  }
  return null;
}

type DiffLine = { kind: 'add' | 'del' | 'ctx'; text: string };

// --- diffLines memo cache + cost cap ----------------------------------------
// WHY a cache: diffLines builds an O(n·m) LCS matrix, and during a STREAMING turn
// the whole transcript re-renders. DiffView / DiffBlock / diffStats / EditGroupView
// all call diffLines in their render BODY, so the same hunk re-diffs even though its
// inputs (an edit's before/after, parsed from a tool input that already arrived)
// never change. A transparent memo on (before, after) collapses repeats to a Map
// lookup; identical inputs → identical outputs as before. Bounded: past ~256
// distinct hunks we clear it (cheap; worst case the live hunk re-diffs once).
const DIFF_CACHE_MAX = 256;
const diffCache = new Map<string, DiffLine[]>();

// WHY a cost cap: a giant MultiEdit/Edit hunk would otherwise allocate an n·m number
// matrix and walk it synchronously on the render thread. The cache means a hunk is
// diffed at most ONCE, so the cap only guards a single enormous edit. Past these
// thresholds we DON'T build the matrix: a COARSE del-then-add fallback (O(n+m), no
// shared-context detection — so the +/− header OVER-reports for a mostly-unchanged
// giant hunk, a tolerable degradation for a >2000-line wholesale rewrite).
const DIFF_MAX_CELLS = 4_000_000;
const DIFF_MAX_LINES = 8000;

// Line-level diff of two strings via a classic LCS, then emit del/add/ctx rows.
// Lossless on the line set: deletions then additions where they diverge, shared
// lines as context. Good enough + readable for the small hunks tool edits carry.
// Exported for unit tests. Memoized + cost-capped (see above): output is IDENTICAL
// to the raw LCS for any non-pathological input — the cache is transparent.
export function diffLines(before: string, after: string): DiffLine[] {
  // Pure addition (Write, or an Edit with empty old_string) — skip the LCS.
  if (before === '') {
    if (after === '') return [];
    return after.split('\n').map((text) => ({ kind: 'add' as const, text }));
  }
  if (after === '') {
    return before.split('\n').map((text) => ({ kind: 'del' as const, text }));
  }
  // Transparent memo. The key MUST be injective — a naive `before + sep + after` is
  // NOT safe for ANY separator that can appear in content: two DISTINCT pairs can
  // join to the same string (('a','b c') and ('a b','c') both → 'a b c') → the
  // second returns the WRONG cached diff. Prefixing with before.length pins the
  // boundary unambiguously (the leading digits end at the first space). Plain ASCII
  // space separator — never a NUL (a stray NUL makes the file read as binary to grep).
  const key = before.length + ' ' + before + after;
  const cached = diffCache.get(key);
  if (cached) return cached;

  const a = before.split('\n');
  const b = after.split('\n');
  const n = a.length;
  const m = b.length;

  let out: DiffLine[];
  if (n * m > DIFF_MAX_CELLS || n + m > DIFF_MAX_LINES) {
    // Pathological size — skip the LCS matrix entirely. Coarse del-then-add diff:
    // O(n+m), no shared-context detection (counts every line as changed, so the +/−
    // header over-reports for a mostly-unchanged giant hunk — tolerable for a
    // >2000-line rewrite that would otherwise stall the render thread).
    out = [];
    for (const text of a) out.push({ kind: 'del', text });
    for (const text of b) out.push({ kind: 'add', text });
  } else {
    // LCS length table.
    const lcs: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0));
    for (let i = n - 1; i >= 0; i--) {
      for (let j = m - 1; j >= 0; j--) {
        lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
      }
    }
    out = [];
    let i = 0;
    let j = 0;
    while (i < n && j < m) {
      if (a[i] === b[j]) {
        out.push({ kind: 'ctx', text: a[i] });
        i++;
        j++;
      } else if (lcs[i + 1][j] >= lcs[i][j + 1]) {
        out.push({ kind: 'del', text: a[i] });
        i++;
      } else {
        out.push({ kind: 'add', text: b[j] });
        j++;
      }
    }
    while (i < n) out.push({ kind: 'del', text: a[i++] });
    while (j < m) out.push({ kind: 'add', text: b[j++] });
  }

  // Bound the cache (clear-on-overflow is safe + cheap; no LRU bookkeeping needed).
  if (diffCache.size >= DIFF_CACHE_MAX) diffCache.clear();
  diffCache.set(key, out);
  return out;
}

const SHORT_PATH = (p: string): string => {
  if (!p) return '';
  const parts = p.split(/[\\/]/).filter(Boolean);
  return parts.length > 3 ? '…/' + parts.slice(-3).join('/') : p;
};

// DiffView — the red/green line-diff card body. Renders a header (file path +
// "+N −M" summary), then the colored diff lines with a ─/+/- gutter, capped
// height with internal scroll. Token-colors each line via the FileTile
// highlighter (lang inferred from the path); falls back to plain text for
// unknown extensions, so there is never a regression.
// Added/removed line counts for a parsed diff, without rendering it. Shared by
// the DiffBlock collapsed summary and DiffView's own header so the two agree.
function diffStats(parsed: ParsedDiff): { added: number; removed: number } {
  let added = 0;
  let removed = 0;
  for (const h of parsed.hunks) {
    for (const l of diffLines(h.before, h.after)) {
      if (l.kind === 'add') added++;
      else if (l.kind === 'del') removed++;
    }
  }
  return { added, removed };
}

function DiffView({
  parsed,
  compact,
  hideHead,
}: {
  parsed: ParsedDiff;
  compact?: boolean;
  hideHead?: boolean;
}) {
  const lang = detectLang(parsed.filePath || '');
  const lines: DiffLine[] = [];
  for (let h = 0; h < parsed.hunks.length; h++) {
    if (h > 0) lines.push({ kind: 'ctx', text: '' }); // blank separator between hunks
    for (const l of diffLines(parsed.hunks[h].before, parsed.hunks[h].after)) lines.push(l);
  }
  let added = 0;
  let removed = 0;
  for (const l of lines) {
    if (l.kind === 'add') added++;
    else if (l.kind === 'del') removed++;
  }
  return (
    <div className={`native-diff${compact ? ' native-diff-compact' : ''}`}>
      {!hideHead && (
        <div className="native-diff-head">
          <span className="native-diff-path" title={parsed.filePath}>
            {SHORT_PATH(parsed.filePath) || '(no path)'}
          </span>
          <span className="native-diff-stat">
            {added > 0 && <span className="native-diff-stat-add">+{added}</span>}
            {removed > 0 && <span className="native-diff-stat-del">−{removed}</span>}
          </span>
        </div>
      )}
      <pre className="native-diff-body">
        {lines.map((l, i) => (
          <span key={i} className={`native-diff-line ${l.kind}`}>
            <span className="native-diff-gutter" aria-hidden>
              {l.kind === 'add' ? '+' : l.kind === 'del' ? '-' : ' '}
            </span>
            <span className="native-diff-text">
              {l.text === '' ? (
                ' '
              ) : (
                tokenizeLine(l.text, lang).map((t, ti) =>
                  t.cls ? (
                    <span key={ti} className={`tok-${t.cls}`}>
                      {t.text}
                    </span>
                  ) : (
                    <span key={ti}>{t.text}</span>
                  ),
                )
              )}
            </span>
          </span>
        ))}
      </pre>
    </div>
  );
}

// DiffBlock — a diff with its own expand toggle, so it collapses to a one-line
// "path +N −M" summary by default without ever losing it (a user click expands
// it). The toggle IS the persistent header (path + stat + chevron); the diff
// body renders below when open with its own head suppressed to avoid a duplicate
// path line.
function DiffBlock({ parsed }: { parsed: ParsedDiff }) {
  const [open, setOpen] = useState<boolean | null>(null);
  const findActive = useContext(NativeFindContext).active;
  const isOpen = findActive || (open ?? false);
  // `parsed` is rebuilt each render upstream, so memoizing on it wouldn't cache;
  // diffStats is a cheap line walk and ToolCallView is memo'd, so just compute it.
  const { added, removed } = diffStats(parsed);
  const shortPath = SHORT_PATH(parsed.filePath) || '(no path)';
  return (
    <div className="native-diff-block">
      <button
        type="button"
        className="native-diff-toggle"
        onClick={() => setOpen(!isOpen)}
        aria-expanded={isOpen}
        aria-label={isOpen ? `Hide diff for ${shortPath}` : `Show diff for ${shortPath} (+${added} −${removed})`}
      >
        <span className="native-diff-toggle-chev" aria-hidden>{isOpen ? '▾' : '▸'}</span>
        <span className="native-diff-path" title={parsed.filePath}>{shortPath}</span>
        <span className="native-diff-stat">
          {added > 0 && <span className="native-diff-stat-add">+{added}</span>}
          {removed > 0 && <span className="native-diff-stat-del">−{removed}</span>}
        </span>
      </button>
      {isOpen && <DiffView parsed={parsed} compact hideHead />}
    </div>
  );
}

// ---------------------------------------------------------------------------
// AskUserQuestion — render the question + options readably instead of dumping
// the raw args JSON. Handles both the live-stream shape ({questions:[…]}) and a
// single-question object; tolerant of string-or-{label,description} options.
// ---------------------------------------------------------------------------

export type AskOption = { label: string; description?: string };
export type AskQuestion = { header?: string; question: string; multiSelect?: boolean; options: AskOption[] };

function isAskUserQuestion(name?: string): boolean {
  return name === 'AskUserQuestion';
}

// Exported so NativePane's keyboard handler can parse the same question shape the
// card renders (digits 1–9 select options / Enter confirms) without re-deriving
// it — the single source of truth for the wire-shape tolerance.
export function parseAskQuestions(args: unknown): AskQuestion[] | null {
  if (!args || typeof args !== 'object') return null;
  const a = args as Record<string, unknown>;
  let raw: unknown[] | null = null;
  if (Array.isArray(a.questions)) raw = a.questions as unknown[];
  else if (typeof a.question === 'string') raw = [a];
  if (!raw) return null;
  const out: AskQuestion[] = [];
  for (const q of raw) {
    if (!q || typeof q !== 'object') continue;
    const qo = q as Record<string, unknown>;
    const question = typeof qo.question === 'string' ? qo.question : '';
    if (!question) continue;
    const options: AskOption[] = [];
    if (Array.isArray(qo.options)) {
      for (const o of qo.options) {
        if (typeof o === 'string') options.push({ label: o });
        else if (o && typeof o === 'object') {
          const oo = o as Record<string, unknown>;
          if (typeof oo.label === 'string') {
            options.push({
              label: oo.label,
              description: typeof oo.description === 'string' ? oo.description : undefined,
            });
          }
        }
      }
    }
    out.push({
      header: typeof qo.header === 'string' ? qo.header : undefined,
      question,
      multiSelect: qo.multiSelect === true,
      options,
    });
  }
  return out.length ? out : null;
}

function QuestionCard({ questions, answer }: { questions: AskQuestion[]; answer?: string }) {
  // `answer` is the parsed selection string ("Apple" / "Apple, Banana"); used to
  // mark the chosen option(s) once the question is answered.
  const chosen = new Set(
    (answer || '')
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean),
  );
  const hasAnswer = chosen.size > 0;
  return (
    <div className="native-question">
      {questions.map((q, i) => (
        <div key={i} className="native-question-block">
          {q.header && <div className="native-question-header">{q.header}</div>}
          <div className="native-question-text">{q.question}</div>
          {q.options.length > 0 && (
            <ul className="native-question-options">
              {q.options.map((o, j) => {
                const isChosen = chosen.has(o.label);
                const muted = hasAnswer && !isChosen;
                return (
                  <li
                    key={j}
                    className={`native-question-option${isChosen ? ' chosen' : ''}${muted ? ' muted' : ''}`}
                  >
                    <span className="native-question-option-label">
                      {isChosen && <span aria-hidden>✓ </span>}
                      {o.label}
                      {/* Non-color cue for the chosen option so a SR (and a
                          color-blind user, via the ✓) knows it was selected. */}
                      {isChosen && <span className="native-sr-only"> (selected)</span>}
                    </span>
                    {o.description && (
                      <span className="native-question-option-desc">{o.description}</span>
                    )}
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      ))}
    </div>
  );
}

// Build the deny-reason string that carries the user's selection back to Claude.
// Empirically (probe, 2026-06-20) the headless CLI has NO slot to convey an
// AskUserQuestion answer through an `allow` — it self-resolves to "did not
// answer". A `deny` WITH a reason, however, surfaces that reason verbatim as the
// tool_result content, which Claude reads as the answer. So "answering" a native
// question = denying the permission with the selection encoded in the reason.
export function buildAnswerReason(questions: AskQuestion[], selections: string[][]): string {
  if (questions.length === 1) {
    return `The user selected: ${selections[0].join(', ')}`;
  }
  const parts = questions.map((q, i) => {
    const head = q.header || q.question;
    return `${head}: ${selections[i].join(', ') || '(no selection)'}`;
  });
  return `The user answered — ${parts.join('; ')}`;
}

// Strip the carrier prefixes off a tool_result content so the answered question
// card can show just the choice ("Apple") rather than the wire string. Returns
// '' for the skip/no-answer outcomes so the card renders a muted "— no answer —".
function parseAnswer(content?: string): string {
  if (!content) return '';
  if (/did not answer|declined/i.test(content)) return '';
  const m = content.match(/^The user (?:selected|answered)\s*[:—-]?\s*(.+)$/s);
  return (m ? m[1] : content).trim();
}

// (isUserDenial lives in nativeTranscript.ts — shared with the grouping pass so
// the card styling and the collapsed "N failed" pip use one denial predicate.)

// QuestionAnswer — the INTERACTIVE control shown on the live approval card.
// Single question + single-select → each option answers immediately on tap.
// multiSelect / multiple questions → options toggle, then "Send answer" confirms
// once every question has at least one pick.
//
// Selection is CONTROLLED when `sel`/`onSelChange` are passed (NativePane owns it
// so the keyboard handler — digits 1–9 toggle, Enter confirms — and the on-card
// clicks share one source of truth). Falls back to internal state otherwise.
// Each option shows a small digit badge (1–9) so the keyboard shortcut is
// discoverable; digits past 9 simply have no badge (still clickable).
export function QuestionAnswer({
  questions,
  busy,
  onAnswer,
  onSkip,
  sel: selProp,
  onSelChange,
}: {
  questions: AskQuestion[];
  busy: boolean;
  onAnswer: (reason: string) => void;
  onSkip: () => void;
  sel?: string[][];
  onSelChange?: (sel: string[][]) => void;
}) {
  const [selState, setSelState] = useState<string[][]>(() => questions.map(() => []));
  const sel = selProp ?? selState;
  const setSel = (updater: (prev: string[][]) => string[][]) => {
    const next = updater(sel);
    if (onSelChange) onSelChange(next);
    else setSelState(next);
  };
  const single = questions.length === 1 && !questions[0].multiSelect;

  const toggle = (qi: number, label: string, multi: boolean) => {
    setSel((prev) => {
      const next = prev.map((a) => a.slice());
      const cur = next[qi] ?? (next[qi] = []);
      if (multi) {
        const idx = cur.indexOf(label);
        if (idx >= 0) cur.splice(idx, 1);
        else cur.push(label);
      } else {
        next[qi] = cur[0] === label ? [] : [label];
      }
      return next;
    });
  };

  const answerNow = (qi: number, label: string) => {
    if (busy) return;
    onAnswer(buildAnswerReason(questions, questions.map((_, i) => (i === qi ? [label] : []))));
  };

  const ready = questions.length > 0 && questions.every((_, i) => (sel[i]?.length ?? 0) > 0);

  return (
    <div className="native-question">
      {questions.map((q, qi) => (
        <div key={qi} className="native-question-block">
          {q.header && <div className="native-question-header">{q.header}</div>}
          <div className="native-question-text">{q.question}</div>
          <div className="native-question-choices">
            {q.options.map((o, oi) => {
              const chosen = (sel[qi] ?? []).includes(o.label);
              return (
                <button
                  key={oi}
                  type="button"
                  className={`native-question-choice ${chosen ? 'chosen' : ''}`}
                  disabled={busy}
                  // single-select answers immediately (no toggle state to track);
                  // multi/multiSelect options toggle, so expose the pressed state.
                  aria-pressed={single ? undefined : chosen}
                  aria-label={o.description ? `${o.label} — ${o.description}` : o.label}
                  onClick={() =>
                    single ? answerNow(qi, o.label) : toggle(qi, o.label, q.multiSelect === true)
                  }
                >
                  {oi < 9 && (
                    <span className="native-question-digit" aria-hidden>
                      {oi + 1}
                    </span>
                  )}
                  <span className="native-question-choice-body">
                    <span className="native-question-option-label">{o.label}</span>
                    {o.description && (
                      <span className="native-question-option-desc">{o.description}</span>
                    )}
                  </span>
                </button>
              );
            })}
          </div>
        </div>
      ))}
      <div className="native-approval-actions">
        {!single && (
          <button
            type="button"
            className="native-btn native-btn-approve"
            disabled={busy || !ready}
            onClick={() => onAnswer(buildAnswerReason(questions, sel))}
          >
            Send answer
          </button>
        )}
        <button type="button" className="native-btn native-btn-deny" disabled={busy} onClick={onSkip}>
          Skip
        </button>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Message actions toolbar — a low-noise hover/focus action row attached to a
// message (assistant text, user message, error). Hidden by default; revealed on
// hover OR keyboard focus within the message (CSS :hover / :focus-within), so the
// transcript stays calm but every action is reachable by mouse AND keyboard.
// Each action is a real <button> (tab-focusable, focus-visible ring) with a copy
// confirmation that flips the label to "copied" for ~1.2s.
// ---------------------------------------------------------------------------

export interface MsgAction {
  key: string;
  label: string; // resting label
  title: string; // tooltip / aria-label
  onClick: () => void;
  // When set, the button label flips to `doneLabel` for ~1.2s after a click
  // (used by copy). Purely cosmetic confirmation.
  confirm?: boolean;
  doneLabel?: string;
}

export function MessageActions({ actions }: { actions: MsgAction[] }) {
  // Track which action keys are showing their transient "done" confirmation.
  const [done, setDone] = useState<Record<string, boolean>>({});
  if (!actions.length) return null;
  const fire = (a: MsgAction) => {
    a.onClick();
    if (a.confirm) {
      setDone((d) => ({ ...d, [a.key]: true }));
      setTimeout(() => setDone((d) => ({ ...d, [a.key]: false })), 1200);
    }
  };
  return (
    <div className="native-msg-actions" role="group" aria-label="Message actions">
      {actions.map((a) => (
        <button
          key={a.key}
          type="button"
          className="native-msg-action"
          title={a.title}
          aria-label={a.title}
          onClick={() => fire(a)}
        >
          {a.confirm && done[a.key] ? a.doneLabel ?? 'copied' : a.label}
        </button>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Assistant text / thinking
// ---------------------------------------------------------------------------

// React.memo on every row view: settled rows have identity-stable `ev` (the hook
// only allocates a fresh object for the actively-streaming block), so a streamed
// token re-renders ONLY the live block — every prior row bails out. Default
// shallow prop compare is correct here: all props are primitives or the stable
// `ev` reference (plus callbacks the call sites keep stable; see NativePane).
// SlashResultView — output of a forwarded slash command (/context, /cost, /config).
// The CLI returns the whole thing as one markdown blob in the turn's result; render
// it as a calm, distinct "report" card (not an assistant bubble) so it reads as a
// command readout rather than Claude talking.
export const SlashResultView = memo(function SlashResultView({ ev }: { ev: NativeEvent }) {
  const text = ev.text ?? '';
  if (!text.trim()) return null;
  return (
    <div className="native-slash-result" role="group" aria-label="command output">
      <Markdown source={text} />
    </div>
  );
});

export const TextView = memo(function TextView({ ev }: { ev: NativeEvent }) {
  const text = ev.text ?? '';
  if (!text) return null;
  return (
    <div className="native-msg native-assistant">
      <Markdown source={text} streaming={ev.streaming} />
      {ev.streaming && <span className="native-caret" aria-hidden />}
    </div>
  );
});

// UserMessageView renders a user turn (user_text). The SAME view is reused for
// the OPTIMISTIC echo: when the user hits Enter the hook pushes a pending row and
// NativePane renders it through here with `optimistic` (and `failed` if the POST
// rejected). The real `user_text` replay echo then reconciles the pending row
// away (foldEvent), and this view re-renders the authoritative row in the same
// logical position — no double bubble. Transcript-SEEDED history (no composer
// echo) flows through here too with neither flag.
export const UserMessageView = memo(function UserMessageView({
  ev,
  optimistic,
  failed,
  onRetry,
}: {
  ev: NativeEvent;
  optimistic?: boolean;
  failed?: boolean;
  onRetry?: () => void;
}) {
  const text = ev.text ?? '';
  if (!text) return null;
  return (
    <div className={`native-msg native-user${optimistic ? ' optimistic' : ''}${failed ? ' failed' : ''}`}>
      <Markdown source={text} />
      {failed && (
        <div className="native-user-failed" role="alert">
          <span className="native-user-failed-glyph" aria-hidden>
            ✕
          </span>
          <span className="native-user-failed-text">Failed to send</span>
          {onRetry && (
            <button
              type="button"
              className="native-user-retry"
              onClick={onRetry}
              aria-label="Retry sending this message"
            >
              Retry
            </button>
          )}
        </div>
      )}
    </div>
  );
});

export const ThinkingView = memo(function ThinkingView({
  ev,
  isStreaming,
}: {
  ev: NativeEvent;
  isStreaming?: boolean;
}) {
  // Default COLLAPSED, even while streaming (the "· thinking…" cue below signals
  // live reasoning) — NOT force-open, which expanded then snapped shut on settle
  // (the flashing). The user toggle (open) always wins; deriving (not seeding)
  // means the collapsed default still applies to untouched blocks.
  const [open, setOpen] = useState<boolean | null>(null);
  const findActive = useContext(NativeFindContext).active;
  const text = ev.text ?? '';
  // Force-open while a find query is present so folded reasoning is searchable.
  const effectiveOpen = findActive || (open ?? false);
  // Collapsed summary: "thought for Xs" when both endpoints were live-stamped,
  // else a word-count fallback ("· 142 words") so a settled block still reads as
  // a real chunk of reasoning rather than a bare toggle.
  const dur = ev.clientTs != null && ev.clientTsEnd != null ? fmtDuration(ev.clientTsEnd - ev.clientTs) : '';
  const words = text.trim() ? text.trim().split(/\s+/).length : 0;
  const summary = dur ? `thought for ${dur}` : words ? `thought · ${words.toLocaleString()} words` : 'thinking';
  return (
    <div className="native-msg native-thinking">
      <button
        type="button"
        className="native-thinking-toggle"
        onClick={() => setOpen(!effectiveOpen)}
        aria-expanded={effectiveOpen}
        aria-label={effectiveOpen ? 'Hide reasoning' : `Show reasoning (${summary})`}
      >
        <span aria-hidden>{effectiveOpen ? '▾' : '▸'} </span>
        {effectiveOpen ? 'thinking' : summary}
        {/* Live cue while collapsed + streaming (replaces the old force-open). */}
        {isStreaming && !effectiveOpen && <span className="native-tool-group-running"> · thinking…</span>}
      </button>
      {effectiveOpen && (
        <div className="native-thinking-body">
          <Markdown source={text} streaming={ev.streaming} />
          {ev.streaming && <span className="native-caret" aria-hidden />}
        </div>
      )}
    </div>
  );
});

// ---------------------------------------------------------------------------
// Tool call (+ its paired result) card
// ---------------------------------------------------------------------------

// ToolOutput — the long-output fold under a tool card. Shows a short preview
// (first OUTPUT_PREVIEW_LINES lines) with a "▸ output · N lines" toggle carrying
// the line/byte count, a copy-output button, and a "show N more lines" expander.
// Errors render the count bar but stay collapsed-to-nothing by default (consistent
// with every other foldable: collapse by default, user expands). The error is still
// loud — red card, `!` dot, "error" status pill, "error output · N lines" bar — so
// nothing is hidden, just not auto-dumped. No inner scroll-trap — the preview is
// short and the body flows in the transcript.
function ToolOutput({ text, isError }: { text: string; isError?: boolean }) {
  // Body visibility: the bar (with the line/byte count) is always shown; the body
  // <pre> below it folds. `open` is the SECOND level — preview vs full — within a
  // shown body (the existing "show N more lines"). bodyOpen is the per-item toggle
  // for the body itself; it overrides the collapsed default, so a collapsed output
  // is one click from view and is never lost.
  const [bodyOpen, setBodyOpen] = useState<boolean | null>(null);
  const [open, setOpen] = useState(false);
  const [copied, setCopied] = useState(false);
  const [copyFailed, setCopyFailed] = useState(false);
  const findActive = useContext(NativeFindContext).active;
  // A SUCCESSFUL tool's output is pure supporting detail — hide it whole (no count
  // bar, no body) so the row collapses to a single line, which is the "stop showing
  // me so much bash" ask. Errors keep the count bar (so a failure is discoverable),
  // but the body stays collapsed until the user expands it — same as every other
  // foldable. The guard sits AFTER all hooks so the hook order never changes.
  if (!isError) return null;
  const lines = text.split('\n');
  const total = lines.length;
  const overflow = total > OUTPUT_PREVIEW_LINES;
  const shown = open || findActive || !overflow ? lines : lines.slice(0, OUTPUT_PREVIEW_LINES);
  const bytes = text.length;
  const countLabel =
    `${total.toLocaleString()} ${total === 1 ? 'line' : 'lines'}` +
    (bytes >= 1024 ? ` · ${(bytes / 1024).toFixed(1)} KB` : '');
  // Body collapses to the count bar by default; a user toggle (bodyOpen) expands it.
  const bodyShown = findActive || (bodyOpen ?? false);

  const copy = () => {
    void copyToClipboard(text).then((ok) => {
      setCopied(ok);
      setCopyFailed(!ok);
      if (ok) setTimeout(() => setCopied(false), 1200);
    });
  };

  return (
    <div className="native-tool-result">
      <div className="native-tool-result-bar">
        <button
          type="button"
          className="native-tool-result-fold"
          onClick={() => setBodyOpen(!bodyShown)}
          aria-expanded={bodyShown}
          aria-label={bodyShown ? 'Hide output' : `Show output (${countLabel})`}
        >
          <span className="native-tool-result-chev" aria-hidden>{bodyShown ? '▾' : '▸'} </span>
          {isError ? 'error output' : 'output'} · {countLabel}
        </button>
        <button
          type="button"
          className="native-tool-copy"
          onClick={copy}
          title={copyFailed ? 'Copy failed — select the output and copy manually' : 'Copy output'}
          aria-label={copyFailed ? 'Copy failed — retry copying output' : copied ? 'Output copied' : 'Copy output'}
        >
          {copyFailed ? 'copy failed' : copied ? 'copied' : 'copy'}
        </button>
      </div>
      {bodyShown && (
        <>
          <pre className={`native-tool-result-body ${isError ? 'error' : ''}`}>
            {shown.join('\n')}
          </pre>
          {overflow && (
            <button
              type="button"
              className="native-tool-result-toggle"
              onClick={() => setOpen((v) => !v)}
              aria-expanded={open}
              aria-label={open ? 'Show less output' : `Show ${(total - OUTPUT_PREVIEW_LINES).toLocaleString()} more lines of output`}
            >
              <span aria-hidden>
                {open ? '▾ show less' : `▸ show ${(total - OUTPUT_PREVIEW_LINES).toLocaleString()} more lines`}
              </span>
            </button>
          )}
        </>
      )}
    </div>
  );
}

export const ToolCallView = memo(function ToolCallView({
  ev,
  result,
}: {
  ev: NativeEvent;
  result?: NativeEvent;
}) {
  const name = ev.name || ev.toolName;
  const questions = isAskUserQuestion(name) ? parseAskQuestions(ev.args) : null;
  // File-edit tools (Write/Edit/MultiEdit) get a real diff preview instead of a
  // raw-path summary; everything else keeps the one-line arg summary.
  const diff = !questions && isFileDiffTool(name) ? parseFileDiff(name, ev.args) : null;
  // collapse=true: the compact row is one ellipsized line, so flatten a batched
  // multi-line command to a single tidy line (the approval card keeps the full
  // structure for review).
  const summary = questions || diff ? '' : summarizeArgs(ev.args, true);
  // `interrupted` is a synthetic result buildTranscript fabricates for a call
  // that never got its real result once the daemon reported the turn settled
  // (the ghost-spinner fix). It carries a result, so `running` is false — the row
  // shows a quiet "interrupted" instead of spinning forever.
  const interrupted = result?.interrupted === true;
  const running = !result;
  // A native question is "answered" via deny-with-reason, so its tool_result
  // carries isError:true even on success — never paint it as an error.
  // A user DENY also comes back as isError:true; render it neutrally ("denied")
  // rather than as a failure (the user chose to reject it — it's not an error).
  const denied = !questions && result?.isError === true && isUserDenial(result?.content);
  const isError = !questions && result?.isError === true && !denied;
  const answer = questions && result ? parseAnswer(result.content) : '';
  const errSummary = isError ? errorSummary(result) : '';
  const resultText = result ? result.content || stringifyOutput(result.output) : '';
  const hasOutput = !questions && !denied && !!resultText;
  // Best-effort duration: only when BOTH endpoints were live-stamped (a replayed
  // call/result has no clientTs, so we don't fabricate a time for history).
  const duration =
    ev.clientTs != null && result?.clientTs != null
      ? fmtDuration(result.clientTs - ev.clientTs)
      : '';

  // Hierarchy: a SUCCESSFUL tool call is a quiet compact row (no heavy border, no
  // green "done"). Reserve the bordered-card treatment for errors, denials, the
  // interactive question, and diffs. Status is exception-driven: show
  // running/error/denied; stay silent on plain success.
  const heavy = isError || denied || !!questions || !!diff;
  return (
    <div
      className={
        `native-tool${heavy ? ' native-tool-card' : ' native-tool-compact'}` +
        `${isError ? ' native-tool-error' : ''}${denied ? ' native-tool-denied' : ''}`
      }
    >
      <div className="native-tool-head">
        {/* Status dot carries a non-color shape cue (a glyph for the settled
            states) so running/done/error/denied are distinguishable without
            relying on color. Decorative to the SR — the status word below (and
            on plain success the lack of one) conveys the same to assistive tech. */}
        <span
          className={`native-tool-dot ${running ? 'running' : interrupted ? 'interrupted' : isError ? 'error' : denied ? 'denied' : 'ok'}`}
          aria-hidden
        >
          {running ? '' : interrupted ? '⊘' : isError ? '!' : denied ? '–' : '✓'}
        </span>
        <span className="native-tool-glyph" aria-hidden title={ev.toolKind || undefined}>
          {questions ? '?' : toolKindGlyph(ev.toolKind)}
        </span>
        <span className="native-tool-name">{name || 'tool'}</span>
        {isBackgroundBash(ev) && <BgBadge shells={[result?.bgShell]} />}
        {summary && <span className="native-tool-head-arg" title={summary}>{summary}</span>}
        {/* Exception-driven status: silent on plain success. The word itself is
            the non-color cue (color merely reinforces it). */}
        {(running || interrupted || isError || denied || questions) && (
          <span
            className={`native-tool-status ${
              running ? 'running' : interrupted ? 'interrupted' : denied ? 'denied' : isError ? 'error' : 'done'
            }`}
          >
            {interrupted
              ? 'interrupted'
              : questions
                ? running
                  ? 'awaiting answer…'
                  : 'answered'
                : running
                  ? 'running…'
                  : denied
                    ? 'denied'
                    : 'error'}
          </span>
        )}
        {!running && !questions && duration && (
          <span className="native-tool-dur" title="duration">
            {duration}
          </span>
        )}
      </div>
      {errSummary && <div className="native-tool-errline">{errSummary}</div>}
      {denied && <div className="native-tool-denyline">You denied this — Claude was told, and it didn’t run.</div>}
      {interrupted && (
        <div className="native-tool-interruptline">
          Interrupted before it finished — the turn was cut off (e.g. an update restarted the agent). Ask again to re-run.
        </div>
      )}
      {diff && <DiffBlock parsed={diff} />}
      {questions && <QuestionCard questions={questions} answer={answer} />}
      {questions && result && (
        <div className="native-question-answer">
          {answer ? (
            <>
              <span className="native-question-answer-check" aria-hidden>
                ✓
              </span>
              {answer}
            </>
          ) : (
            <span className="native-question-answer-none">— no answer —</span>
          )}
        </div>
      )}
      {hasOutput && <ToolOutput text={resultText} isError={isError} />}
    </div>
  );
});

// --- memo comparators for the group views -----------------------------------
// WHY (the "fresh wrapper, stable contents" problem): during a streaming turn the
// hook bumps `events` (a new array identity) ~60×/sec, and buildTranscript then
// rebuilds a FRESH `resultByCallId` Map and groupRows slices FRESH `row.events`
// arrays every token. Those new Map/array IDENTITIES flow as props into these
// memo'd group views, so React's DEFAULT shallow compare re-renders every group on
// every token — even a settled group that is visually unchanged (worst offender:
// EditGroupView re-runs an LCS diff in its body).
//
// KEY FACT that makes an identity comparator correct + cheap: buildTranscript /
// groupRows do NOT clone event objects — they filter/slice, passing the ORIGINAL
// event refs through. The fold only allocates a fresh object for the ACTIVELY
// STREAMING block. So a SETTLED group has identity-stable event refs; only a
// streaming group has a changing element ref. Likewise resultByCallId is a fresh
// Map each render but its VALUES (result events) are stable for settled calls.
//
// INVERTED SEMANTICS: a React.memo comparator returns TRUE to SKIP the re-render,
// FALSE to re-render — the OPPOSITE of shouldComponentUpdate. Reference identity
// only; never deep/JSON compare.
function eventsEqual(prev: NativeEvent[], next: NativeEvent[]): boolean {
  if (prev.length !== next.length) return false;
  for (let i = 0; i < prev.length; i++) {
    if (prev[i] !== next[i]) return false;
  }
  return true;
}
// true (skip) iff, for every event in `events` carrying a callId, the paired result
// is the same ref in both Maps. A result arriving/changing for one of THIS group's
// calls flips a value ref → false → re-render.
function resultsEqual(
  events: NativeEvent[],
  prevById: Map<string, NativeEvent>,
  nextById: Map<string, NativeEvent>,
): boolean {
  for (const e of events) {
    if (!e.callId) continue;
    if (prevById.get(e.callId) !== nextById.get(e.callId)) return false;
  }
  return true;
}
// Shared comparator for ToolGroupView / StepGroupView / EditGroupView. Returns true
// to SKIP (memo's inverted contract).
function eventsAndResultsEqual(
  prev: { events: NativeEvent[]; resultByCallId: Map<string, NativeEvent> },
  next: { events: NativeEvent[]; resultByCallId: Map<string, NativeEvent> },
): boolean {
  return (
    eventsEqual(prev.events, next.events) &&
    resultsEqual(next.events, prev.resultByCallId, next.resultByCallId)
  );
}
// TaskView comparator: the Task row (ev + its result) PLUS the nested childCalls and
// their results from the shared resultByCallId.
function taskEqual(
  prev: { ev: NativeEvent; result?: NativeEvent; childCalls: NativeEvent[]; resultByCallId: Map<string, NativeEvent> },
  next: { ev: NativeEvent; result?: NativeEvent; childCalls: NativeEvent[]; resultByCallId: Map<string, NativeEvent> },
): boolean {
  return (
    prev.ev === next.ev &&
    prev.result === next.result &&
    eventsEqual(prev.childCalls, next.childCalls) &&
    resultsEqual(next.childCalls, prev.resultByCallId, next.resultByCallId)
  );
}

// ToolGroupView — folds a run of consecutive tool calls into ONE compact,
// expandable line ("$ bash ×5" / "5 tool calls", plus a "· N failed" pip when any
// errored) so a burst of shell/edits doesn't stack into a wall of cards.
// Expanding reuses ToolCallView verbatim, so every call keeps its own output
// fold, diff, duration and error styling — nothing is lost. Collapsed by default;
// a lone failed call force-opens so its error card stays visible. Derives (never
// seeds) the open state, so a freshly-arrived result re-derives any group the
// user hasn't touched.
export const ToolGroupView = memo(function ToolGroupView({
  events,
  resultByCallId,
}: {
  events: NativeEvent[];
  resultByCallId: Map<string, NativeEvent>;
}) {
  useRenderCount('ToolGroupView');
  const [open, setOpen] = useState<boolean | null>(null);
  const findActive = useContext(NativeFindContext).active;
  const last = events[events.length - 1];
  const streaming = !(last.callId && resultByCallId.get(last.callId));
  const failed = events.reduce(
    (n, e) => n + (isErroredResult(e.callId ? resultByCallId.get(e.callId) : undefined) ? 1 : 0),
    0,
  );
  const single = events.length === 1;
  // Calls left in flight when the turn was cut off (the ghost-spinner fix) — shown
  // as a neutral "· N interrupted" pip so a collapsed group still surfaces it.
  const interrupted = events.reduce(
    (n, e) => n + (e.callId && resultByCallId.get(e.callId)?.interrupted ? 1 : 0),
    0,
  );
  // Default COLLAPSED while streaming (not force-open) — the "· running…" pip on the
  // header signals live work without expanding then snapping shut on settle (that
  // show-then-collapse was the "flashing" on fast calls). The user can expand to watch
  // live and their toggle sticks. A lone FAILED call still force-opens so its error
  // card is visible (an error isn't a flash — it stays). Else the user toggle / default.
  const isOpen = findActive || (open ?? (single && failed > 0));
  // "$ bash ×N" only when every call is literally the Bash tool. Grep/Glob classify
  // as `exec` on the daemon (no dedicated kind) but aren't bash, so key off the tool
  // NAME — a mixed Bash+Grep run reads as a neutral "N tool calls". A single call
  // shows its kind glyph + name (command/args stay hidden until expand).
  const allBash = events.every((e) => (e.name || e.toolName) === 'Bash');
  const label = single
    ? allBash
      ? '$ bash'
      : `${toolKindGlyph(events[0].toolKind)} ${events[0].name || events[0].toolName || 'tool'}`
    : allBash
      ? `$ bash ×${events.length}`
      : `${events.length} tool calls`;
  return (
    <div className="native-tool-group">
      <button
        type="button"
        className="native-tool-group-toggle"
        onClick={() => setOpen(!isOpen)}
        aria-expanded={isOpen}
        aria-label={
          isOpen
            ? 'Collapse tool calls'
            : `Expand ${events.length} tool call${single ? '' : 's'}${failed ? `, ${failed} failed` : ''}${
                interrupted ? `, ${interrupted} interrupted` : ''
              }`
        }
      >
        {/* Aggregate status of the folded children, so the collapsed header alone
            tells the state: spinner if any still running, else error/interrupted, else ✓. */}
        <span
          className={`native-tool-dot ${streaming ? 'running' : failed > 0 ? 'error' : interrupted > 0 ? 'interrupted' : 'ok'}`}
          style={{ marginRight: '4px' }}
          aria-hidden
        >
          {streaming ? '' : failed > 0 ? '!' : interrupted > 0 ? '⊘' : '✓'}
        </span>
        <span className="native-tool-group-chev" aria-hidden>{isOpen ? '▾' : '▸'} </span>
        <span className="native-tool-group-label">{label}</span>
        {/* A backgrounded bash folds to "$ bash" like any other tool — surface the
            fire-and-forget nature on the collapsed header too, not just on expand. */}
        {(() => {
          const sh = events.filter(isBackgroundBash).map((e) => (e.callId ? resultByCallId.get(e.callId)?.bgShell : undefined));
          return sh.length ? <BgBadge shells={sh} lead /> : null;
        })()}
        {/* A pulsing "running…" pip so the group header itself signals live work — even
            at a glance, and even in the rare collapsed-while-streaming frame. */}
        {streaming && <span className="native-tool-group-running"> · running…</span>}
        {failed > 0 && <span className="native-tool-group-failed"> · {failed} failed</span>}
        {interrupted > 0 && (
          <span className="native-tool-group-interrupted"> · {interrupted} interrupted</span>
        )}
      </button>
      {isOpen && (
        <div className="native-tool-group-body">
          {events.map((e) => (
            <ToolCallView
              key={e.seq}
              ev={e}
              result={e.callId ? resultByCallId.get(e.callId) : undefined}
            />
          ))}
        </div>
      )}
    </div>
  );
}, eventsAndResultsEqual);

// StepGroupView — folds an INTERLEAVED run of thinking + tool calls (the
// think→tool→think→tool ladder) into ONE compact, expandable group instead of a tall
// stack of alternating collapsed rows. The interstitial thinking is the agent reasoning
// about its NEXT action (the MEANS → folded here); a thinking that precedes the prose
// reply (the conclusion's reasoning) was already peeled out by groupRows and stays a
// visible row. The collapsed header reads by the TOOLS (the action) plus a "· reasoning"
// hint so it's distinguishable from a plain tool-group; expanding reuses ThinkingView +
// ToolCallView per child IN ORDER, so the reasoning is one click away.
export const StepGroupView = memo(function StepGroupView({
  events,
  resultByCallId,
}: {
  events: NativeEvent[];
  resultByCallId: Map<string, NativeEvent>;
}) {
  useRenderCount('StepGroupView');
  const [open, setOpen] = useState<boolean | null>(null);
  const findActive = useContext(NativeFindContext).active;
  const tools = events.filter((e) => e.type === 'tool_call');
  const last = events[events.length - 1];
  // Streaming if the last child is a tool awaiting its result, or a thinking still streaming.
  const streaming =
    last.type === 'tool_call' ? !(last.callId && resultByCallId.get(last.callId)) : !!last.streaming;
  const failed = tools.reduce(
    (n, e) => n + (isErroredResult(e.callId ? resultByCallId.get(e.callId) : undefined) ? 1 : 0),
    0,
  );
  const interrupted = tools.reduce(
    (n, e) => n + (e.callId && resultByCallId.get(e.callId)?.interrupted ? 1 : 0),
    0,
  );
  const isOpen = findActive || (open ?? (tools.length === 1 && failed > 0));
  const allBash = tools.length > 0 && tools.every((e) => (e.name || e.toolName) === 'Bash');
  const label =
    tools.length === 1
      ? allBash
        ? '$ bash'
        : `${toolKindGlyph(tools[0].toolKind)} ${tools[0].name || tools[0].toolName || 'tool'}`
      : allBash
        ? `$ bash ×${tools.length}`
        : `${tools.length} tool calls`;
  return (
    <div className="native-tool-group native-step-group">
      <button
        type="button"
        className="native-tool-group-toggle"
        onClick={() => setOpen(!isOpen)}
        aria-expanded={isOpen}
        aria-label={
          isOpen
            ? 'Collapse steps'
            : `Expand ${tools.length} tool call${tools.length === 1 ? '' : 's'} with reasoning${
                failed ? `, ${failed} failed` : ''
              }${interrupted ? `, ${interrupted} interrupted` : ''}`
        }
      >
        <span
          className={`native-tool-dot ${streaming ? 'running' : failed > 0 ? 'error' : interrupted > 0 ? 'interrupted' : 'ok'}`}
          style={{ marginRight: '4px' }}
          aria-hidden
        >
          {streaming ? '' : failed > 0 ? '!' : interrupted > 0 ? '⊘' : '✓'}
        </span>
        <span className="native-tool-group-chev" aria-hidden>{isOpen ? '▾' : '▸'} </span>
        <span className="native-tool-group-label">{label}</span>
        <span className="native-step-group-think"> · reasoning</span>
        {(() => {
          const sh = events.filter(isBackgroundBash).map((e) => (e.callId ? resultByCallId.get(e.callId)?.bgShell : undefined));
          return sh.length ? <BgBadge shells={sh} lead /> : null;
        })()}
        {streaming && <span className="native-tool-group-running"> · running…</span>}
        {failed > 0 && <span className="native-tool-group-failed"> · {failed} failed</span>}
        {interrupted > 0 && (
          <span className="native-tool-group-interrupted"> · {interrupted} interrupted</span>
        )}
      </button>
      {isOpen && (
        <div className="native-tool-group-body">
          {events.map((e) =>
            e.type === 'thinking' || e.type === 'thinking_delta' ? (
              <ThinkingView key={e.seq} ev={e} isStreaming={e.streaming} />
            ) : (
              <ToolCallView key={e.seq} ev={e} result={e.callId ? resultByCallId.get(e.callId) : undefined} />
            ),
          )}
        </div>
      )}
    </div>
  );
}, eventsAndResultsEqual);

// EditGroupView — consolidates a run of ≥2 CONSECUTIVE file edits into ONE
// compact, expandable line ("✎ N edits · files · +X −Y") so a sequence of edits no
// longer stacks into a wall of repeated "Edit" cards (Benjamin's report). A LONE edit
// is never grouped — it renders inline with its diff (the v0.0.603 surface-the-result
// principle). Collapsed by default: the header carries the high-signal summary (count,
// files touched, aggregate +/−) parsed straight from each edit's args; expanding reuses
// ToolCallView per edit so every diff is one click away, nothing lost. Force-opens when
// an edit failed so its error card stays visible.
export const EditGroupView = memo(function EditGroupView({
  events,
  resultByCallId,
}: {
  events: NativeEvent[];
  resultByCallId: Map<string, NativeEvent>;
}) {
  useRenderCount('EditGroupView');
  const [open, setOpen] = useState<boolean | null>(null);
  const findActive = useContext(NativeFindContext).active;
  const last = events[events.length - 1];
  const streaming = !(last.callId && resultByCallId.get(last.callId));
  const failed = events.reduce(
    (n, e) => n + (isErroredResult(e.callId ? resultByCallId.get(e.callId) : undefined) ? 1 : 0),
    0,
  );
  // Aggregate diff stats + distinct files from each edit's ARGS (the diff is parsed
  // from the tool input, not the result), so the collapsed header is informative
  // without expanding. Cheap line walks; the view is memo'd.
  let added = 0;
  let removed = 0;
  const files = new Set<string>();
  for (const e of events) {
    const d = parseFileDiff(e.name || e.toolName, e.args);
    if (!d) continue;
    const s = diffStats(d);
    added += s.added;
    removed += s.removed;
    if (d.filePath) files.add(d.filePath);
  }
  const onlyFile = files.size === 1 ? [...files][0] : '';
  const fileLabel = onlyFile ? onlyFile.split('/').pop() || onlyFile : files.size > 1 ? `${files.size} files` : '';
  // An all-Write run is file CREATION, so read it as "writes" rather than "edits";
  // any Edit/MultiEdit/NotebookEdit in the run makes it "edits".
  const noun = events.every((e) => (e.name || e.toolName) === 'Write') ? 'writes' : 'edits';
  // Force-open on a failure so the error card shows; else collapsed by default.
  const isOpen = findActive || (open ?? failed > 0);
  return (
    <div className="native-tool-group native-edit-group">
      <button
        type="button"
        className="native-tool-group-toggle"
        onClick={() => setOpen(!isOpen)}
        aria-expanded={isOpen}
        aria-label={
          isOpen
            ? `Collapse ${noun}`
            : `Expand ${events.length} ${noun}${fileLabel ? ` to ${fileLabel}` : ''} (+${added} −${removed})${
                failed ? `, ${failed} failed` : ''
              }`
        }
      >
        <span
          className={`native-tool-dot ${streaming ? 'running' : failed > 0 ? 'error' : 'ok'}`}
          style={{ marginRight: '4px' }}
          aria-hidden
        >
          {streaming ? '' : failed > 0 ? '!' : '✓'}
        </span>
        <span className="native-tool-group-chev" aria-hidden>{isOpen ? '▾' : '▸'} </span>
        <span className="native-tool-group-label">✎ {events.length} {noun}</span>
        {fileLabel && <span className="native-edit-group-files" title={onlyFile || undefined}> · {fileLabel}</span>}
        {(added > 0 || removed > 0) && (
          <span className="native-edit-group-stat">
            {' · '}
            {added > 0 && <span className="native-diff-stat-add">+{added}</span>}
            {removed > 0 && <span className="native-diff-stat-del"> −{removed}</span>}
          </span>
        )}
        {/* Pulsing pip so the header signals live work even while collapsed-streaming. */}
        {streaming && <span className="native-tool-group-running"> · running…</span>}
        {failed > 0 && <span className="native-tool-group-failed"> · {failed} failed</span>}
      </button>
      {isOpen && (
        <div className="native-tool-group-body">
          {events.map((e) => (
            <ToolCallView key={e.seq} ev={e} result={e.callId ? resultByCallId.get(e.callId) : undefined} />
          ))}
        </div>
      )}
    </div>
  );
}, eventsAndResultsEqual);

// TaskView — a Task/Agent tool call that spawned a sub-agent, with the sub-agent's
// captured tool calls rendered INDENTED beneath the Task row. The Task row itself is
// a normal ToolCallView; the children render via ToolGroupView → one "$ bash ×N"
// sub-summary that the user can expand, pulling their results from the shared
// resultByCallId. NativePane only routes here when ≥1 child was captured, so a
// childless Task degrades to a plain ToolCallView (no empty affordance).
export const TaskView = memo(function TaskView({
  ev,
  result,
  childCalls,
  resultByCallId,
}: {
  ev: NativeEvent;
  result?: NativeEvent;
  childCalls: NativeEvent[];
  resultByCallId: Map<string, NativeEvent>;
}) {
  useRenderCount('TaskView');
  return (
    <div className="native-task">
      <ToolCallView ev={ev} result={result} />
      <div className="native-task-body">
        <ToolGroupView events={childCalls} resultByCallId={resultByCallId} />
      </div>
    </div>
  );
}, taskEqual);

// ThinkingGroupView — folds a run of consecutive thinking blocks into one
// collapsible "reasoning" line ("thought · 420 words (3 blocks)"). Expanding
// renders all blocks' markdown inline (a hairline divider between them) instead
// of nested per-block toggles, so a single click reveals the whole reasoning
// chain. Default COLLAPSED while streaming (the "thinking (N blocks)" label already
// signals live reasoning) — not force-open, which expanded then snapped shut on
// settle (the flashing). Expand to watch live; the toggle sticks.
export const ThinkingGroupView = memo(function ThinkingGroupView({
  events,
}: {
  events: NativeEvent[];
}) {
  useRenderCount('ThinkingGroupView');
  const [open, setOpen] = useState<boolean | null>(null);
  const findActive = useContext(NativeFindContext).active;
  const streaming = events.some((e) => e.streaming);
  const isOpen = findActive || (open ?? false);
  const words = events.reduce((n, e) => {
    const t = (e.text ?? '').trim();
    return n + (t ? t.split(/\s+/).length : 0);
  }, 0);
  const summary = words
    ? `thought · ${words.toLocaleString()} words (${events.length} blocks)`
    : `thinking (${events.length} blocks)`;
  return (
    <div className="native-msg native-thinking native-thinking-group">
      <button
        type="button"
        className="native-thinking-toggle"
        onClick={() => setOpen(!isOpen)}
        aria-expanded={isOpen}
        aria-label={isOpen ? 'Collapse reasoning' : `Show reasoning (${summary})`}
      >
        <span aria-hidden>{isOpen ? '▾' : '▸'} </span>
        {isOpen ? `reasoning · ${events.length} blocks` : summary}
        {/* Live cue while collapsed + streaming (replaces the old force-open). */}
        {streaming && !isOpen && <span className="native-tool-group-running"> · thinking…</span>}
      </button>
      {isOpen && (
        <div className="native-thinking-body">
          {events.map((e, i) => (
            <div key={e.seq} className="native-thinking-group-block">
              {i > 0 && <div className="native-thinking-group-div" aria-hidden />}
              <Markdown source={e.text ?? ''} streaming={e.streaming} />
              {e.streaming && <span className="native-caret" aria-hidden />}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}, (prev, next) => eventsEqual(prev.events, next.events));

function stringifyOutput(out: unknown): string {
  if (out == null) return '';
  if (typeof out === 'string') return out;
  try {
    return JSON.stringify(out, null, 2);
  } catch {
    return String(out);
  }
}

// ---------------------------------------------------------------------------
// Todo list
// ---------------------------------------------------------------------------

export const TodoView = memo(function TodoView({ ev }: { ev: NativeEvent }) {
  const items = ev.items ?? [];
  if (!items.length) return null;
  const isDone = (it: { completed?: boolean; status?: string }) =>
    it.completed === true || it.status === 'completed';
  const isActive = (it: { status?: string }) => it.status === 'in_progress';
  const done = items.filter(isDone).length;
  const total = items.length;
  const pct = total ? Math.round((done / total) * 100) : 0;
  return (
    <div className="native-todo" role="group" aria-label={`Plan, ${done} of ${total} done`}>
      <div className="native-todo-header">
        <span className="native-todo-title">Plan · {done}/{total}</span>
        <span
          className="native-todo-bar"
          role="progressbar"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={pct}
          aria-label={`${pct}% complete`}
        >
          <span className="native-todo-bar-fill" style={{ width: `${pct}%` }} />
        </span>
      </div>
      {items.map((it, i) => {
        const active = isActive(it);
        const finished = isDone(it);
        // Non-color status: a per-item word (done / in progress) read by the SR,
        // alongside the visible shape glyph (☑ / ▸ / ☐).
        const status = finished ? 'done' : active ? 'in progress' : 'to do';
        return (
          <div
            key={i}
            className={`native-todo-item ${finished ? 'done' : ''} ${active ? 'active' : ''}`}
          >
            <span className="native-todo-box" aria-hidden>
              {finished ? '☑' : active ? '▸' : '☐'}
            </span>
            <span className="native-todo-text">{it.text}</span>
            <span className="native-sr-only">{` — ${status}`}</span>
          </div>
        );
      })}
    </div>
  );
});

// ---------------------------------------------------------------------------
// Approval card
// ---------------------------------------------------------------------------

// Commands where "cmd subcommand" is the meaningful allow-scope; anything else
// suggests just the first token. This heuristic only decides what rule we
// OFFER — the daemon's matching (allowlistPrefixMatch) is sound regardless:
// it parses the whole script and every command must match the rule.
const PREFIX_TWO_TOKEN = new Set([
  'git', 'npm', 'pnpm', 'yarn', 'docker', 'kubectl', 'cargo', 'go', 'pip',
  'pip3', 'terraform', 'gh', 'aws', 'az', 'gcloud', 'make', 'apt', 'apt-get', 'brew',
]);

// bashPrefixSuggestion derives the scoped-allow suggestion ("git status") from
// a Bash approval's args, or null when the command's first token(s) don't read
// as a sensible rule (paths, expansions, metacharacters).
export function bashPrefixSuggestion(args: unknown): string | null {
  const cmd = (args as { command?: unknown } | null | undefined)?.command;
  if (typeof cmd !== 'string') return null;
  const tokens = cmd.trim().split(/\s+/);
  const first = tokens[0];
  if (!first || !/^[A-Za-z][\w.-]*$/.test(first)) return null;
  const second = tokens[1];
  if (PREFIX_TWO_TOKEN.has(first) && second && /^[A-Za-z][\w:.@/-]*$/.test(second)) {
    return `${first} ${second}`;
  }
  return first;
}

export const ApprovalView = memo(function ApprovalView({
  ev,
  onResolve,
  onAlwaysAllow,
  alreadyAllowed,
  allowlist,
  keyHint,
  batch,
  questionSel,
  onQuestionSelChange,
}: {
  ev: NativeEvent;
  onResolve: (reqId: string, allow: boolean, reason?: string) => Promise<unknown>;
  // onAlwaysAllow: add this tool to the pane's auto-approve allowlist (NativePane
  // wires it to the hook's addToAllowlist). The card then approves THIS request
  // via onResolve, so "Always allow" both allowlists the tool and approves the
  // current one-shot. Absent → the "Always allow" control is hidden (e.g. for
  // AskUserQuestion, which has no tool-name semantics). Returns a promise so the
  // card can sequence the resolve after the add and show busy state. Also used
  // for scoped Bash prefix rules — the entry string is `Bash(git status)`.
  onAlwaysAllow?: (tool: string) => Promise<unknown>;
  // alreadyAllowed: this tool is already on the allowlist. In practice the daemon
  // auto-approves so the card won't render, but if a card does race in we hide
  // the redundant "Always allow" control.
  alreadyAllowed?: boolean;
  // Full allowlist entries, to hide a scoped-prefix suggestion that's already
  // been added (the boolean above only covers the whole-tool entry).
  allowlist?: string[];
  // keyHint: this is the keyboard-active approval (the first open non-question
  // one), so show the Enter/Esc affordance. NativePane wires the actual key
  // handling; this only renders the hint so it appears on exactly one card.
  keyHint?: boolean;
  // batch: when ≥2 tool approvals are pending, the FIRST card surfaces the batch
  // affordances ("Approve all (N)" / "Approve all reads (M)") near the card. Wired
  // by NativePane to the same resolve() loop the docked bar uses.
  batch?: { count: number; readCount: number; onApproveAllReads: () => void };
  // questionSel / onQuestionSelChange: CONTROLLED multi-select state for an
  // AskUserQuestion card, owned by NativePane so the keyboard handler (digits
  // 1–9 / Enter) and the on-card clicks mutate one source of truth. Absent for a
  // non-question approval (or when keyboard control isn't wired).
  questionSel?: string[][];
  onQuestionSelChange?: (sel: string[][]) => void;
}) {
  const [busy, setBusy] = useState(false);
  const reqId = ev.requestId;
  const questions = isAskUserQuestion(ev.toolName) ? parseAskQuestions(ev.args) : null;
  // File-edit approvals (Write/Edit/MultiEdit) render a red/green diff preview
  // instead of the raw JSON args — the single most useful thing to see before
  // approving a write.
  const diff = !questions && isFileDiffTool(ev.toolName) ? parseFileDiff(ev.toolName, ev.args) : null;
  const summary = questions || diff ? '' : summarizeArgs(ev.args);
  // Risk classification (shared with NativePane's keyboard handler): destructive
  // tools (writes, shell mutations) keep the ⚠/danger styling; read-only tools
  // (Read/Grep/Glob) get a calmer accent.
  const { readOnly, destructive } = classifyApprovalRisk(ev.toolName, ev.args);

  const act = async (allow: boolean, reason?: string) => {
    if (!reqId || busy) return;
    setBusy(true);
    try {
      await onResolve(reqId, allow, reason);
    } catch {
      setBusy(false); // allow retry on failure
    }
  };

  // "Always allow <Tool>": add the tool to the pane's allowlist (onAlwaysAllow)
  // THEN approve this current request too (onResolve allow). Order matters — the
  // allowlist add lands first so any sibling/next call for the same tool is
  // auto-approved, then this one-shot is approved. On failure re-enable so the
  // user can retry (or just Approve the one-shot).
  const alwaysAllow = async () => {
    const tool = ev.toolName;
    if (!tool || !onAlwaysAllow || !reqId || busy) return;
    setBusy(true);
    try {
      await onAlwaysAllow(tool);
      await onResolve(reqId, true);
    } catch {
      setBusy(false);
    }
  };

  // Scoped Bash rule ("Always allow `git status …`") — the safer opt-in: the
  // daemon parses each future command and auto-approves only when EVERY
  // command in it matches the prefix. Offered alongside the whole-tool button.
  const prefixSuggestion = ev.toolName === 'Bash' ? bashPrefixSuggestion(ev.args) : null;
  const prefixEntry = prefixSuggestion ? `Bash(${prefixSuggestion})` : null;
  const prefixAlreadyAllowed = !!prefixEntry && !!allowlist?.includes(prefixEntry);
  const alwaysAllowPrefix = async () => {
    if (!prefixEntry || !onAlwaysAllow || !reqId || busy) return;
    setBusy(true);
    try {
      await onAlwaysAllow(prefixEntry);
      await onResolve(reqId, true);
    } catch {
      setBusy(false);
    }
  };

  // A non-color label for the risk glyph so a screen reader announces the
  // severity ("destructive" / "read-only" / "question") rather than a bare glyph.
  const riskLabel = questions
    ? 'Question'
    : destructive
      ? 'Destructive action — review carefully'
      : readOnly
        ? 'Read-only action'
        : 'Action requires approval';
  // Group label so the SR announces e.g. "Approval required: Write, destructive".
  const approvalLabel = questions
    ? 'Claude is asking a question'
    : `Approval required: ${ev.toolName || 'tool'}${destructive ? ', destructive' : readOnly ? ', read-only' : ''}`;
  return (
    <div
      className={`native-approval ${questions ? 'native-approval-question' : ''}${
        !questions && readOnly ? ' native-approval-readonly' : ''
      }`}
      role="group"
      aria-label={approvalLabel}
    >
      <div className="native-approval-head">
        <span className="native-approval-glyph" role="img" aria-label={riskLabel}>
          {questions ? '?' : readOnly ? '○' : '⚠'}
        </span>
        <span className="native-approval-title">
          {questions ? (
            <>
              Claude is asking <strong>a question</strong>
            </>
          ) : (
            <>
              {destructive ? 'Approve' : 'Allow'} <strong>{ev.toolName || 'tool'}</strong>?
            </>
          )}
        </span>
      </div>
      {questions ? (
        // Interactive answer: tapping an option DENIES the permission with the
        // selection encoded in the reason — the only channel that round-trips an
        // AskUserQuestion answer to the headless CLI (allow → "did not answer").
        <QuestionAnswer
          questions={questions}
          busy={busy}
          onAnswer={(reason) => void act(false, reason)}
          onSkip={() => void act(false, 'The user declined to answer the question.')}
          sel={questionSel}
          onSelChange={onQuestionSelChange}
        />
      ) : (
        <>
          {diff && <DiffView parsed={diff} />}
          {summary && <pre className="native-approval-args">{summary}</pre>}
          <div className="native-approval-actions">
            <button
              type="button"
              className="native-btn native-btn-approve"
              disabled={busy || !reqId}
              onClick={() => void act(true)}
              title="Allow this one action"
              aria-label={`Approve ${ev.toolName || 'tool'} once`}
            >
              Approve
            </button>
            {/* Scoped Bash rule — offered BEFORE the whole-tool opt-in so the
                narrower, safer choice is the first "always" the eye lands on.
                The daemon parses every future command and the rule only covers
                scripts it can fully see (no expansions/substitution/redirects),
                with every command matching the prefix. */}
            {onAlwaysAllow && prefixSuggestion && !alreadyAllowed && !prefixAlreadyAllowed && (
              <button
                type="button"
                className="native-btn native-btn-always"
                disabled={busy || !reqId}
                onClick={() => void alwaysAllowPrefix()}
                title={`Auto-approve future Bash commands starting with "${prefixSuggestion}" in this pane (compound/opaque commands still prompt)`}
                aria-label={`Always allow commands starting with ${prefixSuggestion}`}
              >
                Always allow {prefixSuggestion} …
              </button>
            )}
            {/* "Always allow <Tool>": the third action — adds the tool to the
                pane's auto-approve allowlist AND approves this request, closing the
                "every tool re-prompts forever" gap (mirrors Claude Code's TUI
                "don't ask again for this tool"). DESTRUCTIVE tools (Write/Edit/rm)
                get a cautious ⚠ treatment + warning class so opting in is explicit,
                not the easy default. Hidden once the tool is already allowed. */}
            {onAlwaysAllow && ev.toolName && !alreadyAllowed && (
              <button
                type="button"
                className={`native-btn native-btn-always${destructive ? ' native-btn-always-danger' : ''}`}
                disabled={busy || !reqId}
                onClick={() => void alwaysAllow()}
                title={
                  destructive
                    ? `Auto-approve ALL future ${ev.toolName} calls in this pane — including destructive ones`
                    : `Auto-approve all future ${ev.toolName} calls in this pane`
                }
                aria-label={
                  destructive
                    ? `Always allow ${ev.toolName} — auto-approve all future calls, including destructive ones`
                    : `Always allow ${ev.toolName} — auto-approve all future calls in this pane`
                }
              >
                Always allow {ev.toolName}
                {destructive ? <span aria-hidden> ⚠</span> : ''}
              </button>
            )}
            <button
              type="button"
              className="native-btn native-btn-deny"
              disabled={busy || !reqId}
              onClick={() => void act(false)}
              aria-label={`Deny ${ev.toolName || 'tool'}`}
            >
              Deny
            </button>
            {keyHint && (
              // Enter approves only NON-destructive tools (gated in NativePane's
              // keyboard handler); destructive ones require an explicit click, so
              // their hint drops the ↵-approve affordance and shows click-to-approve.
              <span className="native-approval-keyhint" aria-hidden>
                {destructive ? (
                  <>
                    <kbd>Esc</kbd> deny · click to approve
                  </>
                ) : (
                  <>
                    <kbd>↵</kbd> approve · <kbd>Esc</kbd> deny
                  </>
                )}
              </span>
            )}
          </div>
          {batch && batch.count >= 2 && (
            // Batch affordance on the first card when several approvals are open.
            // "reads" approves only the read-like ones (Read/Grep/Glob/LS/WebFetch).
            <div className="native-approval-batch" role="group" aria-label="Batch approval">
              <span className="native-approval-batch-label">{batch.count} pending</span>
              {batch.readCount > 0 && (
                <button
                  type="button"
                  className="native-btn native-btn-batch"
                  disabled={busy}
                  onClick={batch.onApproveAllReads}
                  title="Approve only the read-only requests (Read/Grep/Glob/LS/WebFetch)"
                  aria-label={`Approve all ${batch.readCount} read-only requests`}
                >
                  Approve all reads ({batch.readCount})
                </button>
              )}
            </div>
          )}
        </>
      )}
    </div>
  );
});

// ---------------------------------------------------------------------------
// Allowlist strip — a thin, low-noise row surfacing which tools are currently
// auto-approved in this pane (set via "Always allow <Tool>"). Without it the
// auto-approval would be invisible/magical: a tool just stops prompting with no
// trace. Each tool is a removable chip ("Read ✕") that revokes via
// removeFromAllowlist, so the user can always see + undo what they opted into.
// Destructive tools get a ⚠ + warning tint so an auto-approved Write/Edit reads
// as deliberately risky, not benign. Renders nothing when the list is empty
// (zero footprint in the common case). Lives just above the composer (NativePane).
// ---------------------------------------------------------------------------

export function AllowlistStrip({
  tools,
  onRemove,
}: {
  tools: string[];
  onRemove: (tool: string) => void;
}) {
  if (!tools.length) return null;
  return (
    <div className="native-allowlist-strip" role="status" aria-label="Auto-approved tools">
      <span className="native-allowlist-label" aria-hidden>
        auto-approving:
      </span>
      <span className="native-allowlist-chips">
        {tools.map((tool) => {
          // Scoped Bash prefix rules ("Bash(git status)") render as the command
          // prefix with a trailing ellipsis — the parens form is a wire format,
          // not a label. Removal still passes the exact entry string.
          const prefixInner = /^Bash\((.+)\)$/.exec(tool)?.[1];
          const label = prefixInner ? `${prefixInner} …` : tool;
          const { destructive } = classifyApprovalRisk(tool);
          return (
            <span
              key={tool}
              className={`native-allowlist-chip${destructive ? ' danger' : ''}`}
              title={prefixInner ? `Bash commands starting with "${prefixInner}"` : undefined}
            >
              <span className="native-allowlist-chip-name">
                {destructive && (
                  <span className="native-allowlist-chip-warn" aria-hidden>
                    ⚠
                  </span>
                )}
                {label}
              </span>
              <button
                type="button"
                className="native-allowlist-chip-x"
                title={`Stop auto-approving ${label}`}
                aria-label={`Stop auto-approving ${label}`}
                onClick={() => onRemove(tool)}
              >
                ✕
              </button>
            </span>
          );
        })}
      </span>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Docked approval bar — a compact, unmissable strip just above the composer,
// shown ONLY when a pending approval is scrolled out of view (NativePane decides
// visibility and passes the off-screen target). Surfaces the tool name + risk
// glyph + Approve/Deny + a "jump to" affordance that scrolls the card into view.
// When ≥2 approvals are pending it also surfaces batch affordances: "Approve all
// (N)" and "Approve all reads" (only the read-like ones). Deny is visually the
// default for a destructive target.
// ---------------------------------------------------------------------------

// read-like = the calm, non-mutating tools (Read/Grep/Glob/LS/WebFetch/…). The
// batch "Approve all reads" only ever touches these — never a Write/Bash/Edit.
export function isReadLikeApproval(ev: NativeEvent): boolean {
  if (ev.toolName === 'AskUserQuestion') return false;
  return classifyApprovalRisk(ev.toolName, ev.args).readOnly;
}

export function DockedApprovalBar({
  target,
  pendingApprovals,
  onResolve,
  onJump,
  onApproveAllReads,
  onAlwaysAllow,
  alreadyAllowed,
  allowlist,
}: {
  // The off-screen approval to surface (already filtered to a tool approval, not
  // an AskUserQuestion — those are answered by tapping options, not a bar).
  target: NativeEvent;
  // Full pending list, to drive the batch affordances + counts.
  pendingApprovals: NativeEvent[];
  onResolve: (reqId: string, allow: boolean, reason?: string) => Promise<unknown>;
  onJump: () => void;
  onApproveAllReads: () => void;
  // Add target.toolName to the pane's persistent "always allow" set, then resolve
  // every currently-parked request for that SAME tool (the allowlist only
  // auto-approves FUTURE requests, including after a restart/resume — already
  // pending siblings must be approved explicitly). Absent → the control is hidden
  // (e.g. AskUserQuestion). This is the surface most users actually reach when the
  // inline card is scrolled off, so without it the allowlist never gets populated.
  onAlwaysAllow?: (tool: string) => Promise<unknown>;
  // This tool is already allowlisted → hide the redundant control.
  alreadyAllowed?: boolean;
  // Full allowlist entries (to hide an already-added scoped Bash rule).
  allowlist?: string[];
}) {
  const [busy, setBusy] = useState(false);
  const reqId = target.requestId;
  const { readOnly, destructive } = classifyApprovalRisk(target.toolName, target.args);
  const count = pendingApprovals.length;
  const readCount = pendingApprovals.filter(isReadLikeApproval).length;
  // Scoped Bash rule — see ApprovalView. From the docked bar we add the rule
  // and approve the CURRENT target only (we can't soundly match siblings
  // client-side; the daemon auto-approves matching future requests).
  const prefixSuggestion = target.toolName === 'Bash' ? bashPrefixSuggestion(target.args) : null;
  const prefixEntry = prefixSuggestion ? `Bash(${prefixSuggestion})` : null;
  const prefixAlreadyAllowed = !!prefixEntry && !!allowlist?.includes(prefixEntry);
  const alwaysAllowPrefix = async () => {
    if (!prefixEntry || !onAlwaysAllow || !reqId || busy) return;
    setBusy(true);
    try {
      await onAlwaysAllow(prefixEntry);
      await onResolve(reqId, true);
    } catch {
      setBusy(false);
    }
  };

  const act = async (allow: boolean) => {
    if (!reqId || busy) return;
    setBusy(true);
    try {
      await onResolve(reqId, allow);
    } catch {
      setBusy(false);
    }
  };

  // "Always allow <Tool>": persist the opt-in (so it survives restart/resume),
  // then approve every already-parked request for the same tool. Future requests
  // auto-approve on the daemon — which is exactly what stops Bash re-prompting
  // after each app restart.
  const alwaysAllow = async () => {
    const tool = target.toolName;
    if (!tool || !onAlwaysAllow || busy) return;
    setBusy(true);
    try {
      await onAlwaysAllow(tool);
      for (const e of pendingApprovals) {
        if (e.toolName === tool && e.requestId) await onResolve(e.requestId, true);
      }
    } catch {
      setBusy(false);
    }
  };

  return (
    <div
      className={`native-docked-approval${readOnly ? ' readonly' : ''}`}
      role="group"
      aria-label={`Pending approval: ${target.toolName || 'tool'}${destructive ? ', destructive' : readOnly ? ', read-only' : ''}`}
    >
      <button
        type="button"
        className="native-docked-main"
        onClick={onJump}
        title="Scroll the approval into view"
        aria-label={`Scroll the ${target.toolName || 'tool'} approval into view`}
      >
        <span
          className="native-docked-glyph"
          role="img"
          aria-label={destructive ? 'Destructive action' : readOnly ? 'Read-only action' : 'Requires approval'}
        >
          {readOnly ? '○' : '⚠'}
        </span>
        <span className="native-docked-label">
          {destructive ? 'Approve' : 'Allow'} <strong>{target.toolName || 'tool'}</strong>
        </span>
        {count > 1 && <span className="native-docked-count">+{count - 1} more</span>}
        <span className="native-docked-jump" aria-hidden>
          jump ↧
        </span>
      </button>
      <div className="native-docked-actions">
        {count >= 2 && (
          <>
            {readCount > 0 && (
              <button
                type="button"
                className="native-btn native-btn-batch"
                disabled={busy}
                onClick={onApproveAllReads}
                title="Approve only the read-only requests (Read/Grep/Glob/LS/WebFetch)"
                aria-label={`Approve all ${readCount} read-only requests`}
              >
                Approve all reads ({readCount})
              </button>
            )}
          </>
        )}
        {onAlwaysAllow && prefixSuggestion && !alreadyAllowed && !prefixAlreadyAllowed && (
          <button
            type="button"
            className="native-btn native-btn-batch"
            disabled={busy}
            onClick={() => void alwaysAllowPrefix()}
            title={`Auto-approve future Bash commands starting with "${prefixSuggestion}" in this pane (compound/opaque commands still prompt)`}
            aria-label={`Always allow commands starting with ${prefixSuggestion}`}
          >
            Allow {prefixSuggestion} …
          </button>
        )}
        {onAlwaysAllow && target.toolName && !alreadyAllowed && (
          <button
            type="button"
            className="native-btn native-btn-batch"
            disabled={busy}
            onClick={() => void alwaysAllow()}
            title={`Always allow ${target.toolName} in this pane (persists across restarts) and approve any pending ${target.toolName} requests`}
            aria-label={`Always allow ${target.toolName}`}
          >
            Always allow {target.toolName}
          </button>
        )}
        <button
          type="button"
          className={`native-btn native-btn-approve${destructive ? ' native-btn-secondary' : ''}`}
          disabled={busy || !reqId}
          onClick={() => void act(true)}
          aria-label={`Approve ${target.toolName || 'tool'}`}
        >
          Approve
        </button>
        <button
          type="button"
          className={`native-btn native-btn-deny${destructive ? ' native-btn-default' : ''}`}
          disabled={busy || !reqId}
          onClick={() => void act(false)}
          aria-label={`Deny ${target.toolName || 'tool'}`}
        >
          Deny
        </button>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Status / error
// ---------------------------------------------------------------------------

export const StatusView = memo(function StatusView({
  ev,
  override,
}: {
  ev: NativeEvent;
  override?: string;
}) {
  const label =
    override ||
    ev.detail ||
    (ev.subtype === 'requesting' ? 'Claude is thinking…' : ev.subtype) ||
    'Claude is working…';
  return (
    <div className="native-status">
      <span className="native-spinner" aria-hidden />
      <span className="native-status-text">{label}</span>
    </div>
  );
});

export const ErrorView = memo(function ErrorView({
  ev,
  onRetry,
}: {
  ev: NativeEvent;
  onRetry?: () => void;
}) {
  // onRetry — resend the last user prompt as a new turn (NativePane derives the
  // text). Absent (e.g. no prior prompt to replay) → no retry action.
  //
  // A transient overload (Anthropic 529 / rate-limit / 5xx) is a server-side hiccup,
  // not a dead pane — render it as a CALM, retryable banner (amber, ⟳, "your prompt
  // is intact", role=status not alert) instead of the hard red ✕ failure.
  const overload = isTransientOverload(ev.message, ev.code);
  if (overload) {
    return (
      <div className="native-error native-error-transient" role="status">
        <span className="native-error-glyph" aria-hidden>
          ⟳
        </span>
        <span className="native-error-text">
          The model API is briefly overloaded — a server-side hiccup (usually HTTP 529) that
          normally clears in a few seconds. Your prompt is intact; retry when ready.
        </span>
        {onRetry && (
          <MessageActions
            actions={[{ key: 'retry', label: 'retry', title: 'Resend the last prompt', onClick: onRetry }]}
          />
        )}
      </div>
    );
  }
  return (
    <div className="native-error" role="alert">
      <span className="native-error-glyph" aria-hidden>
        ✕
      </span>
      <span className="native-error-text">
        {ev.message || 'error'}
        {ev.code ? ` (${ev.code})` : ''}
      </span>
      {onRetry && (
        <MessageActions
          actions={[{ key: 'retry', label: 'retry', title: 'Resend the last prompt', onClick: onRetry }]}
        />
      )}
    </div>
  );
});

// TurnSeparator — a subtle divider + relative-time chip at each user-turn
// boundary so long scrollback is scannable. `label` is the pre-formatted
// relative time ("2m ago" / "earlier"); NativePane computes it so the time
// re-renders live as the transcript grows.
export function TurnSeparator({ label }: { label: string }) {
  return (
    <div className="native-turn-sep" role="separator" aria-label={`new turn · ${label}`}>
      <span className="native-turn-sep-line" aria-hidden />
      <span className="native-turn-sep-time">{label}</span>
      <span className="native-turn-sep-line" aria-hidden />
    </div>
  );
}

// SessionEndedCard — terminal state when the agent process exits. Mirrors the
// dignity of the terminal pane's "Session closed" overlay (PaneBodyOverlays):
// a calm card stating the process has exited (with its code when known). The
// composer is disabled separately by NativePane.
export function SessionEndedCard({ exitCode, resumable, restartPaused, agentLabel = 'Claude' }: { exitCode?: number; resumable?: boolean; restartPaused?: boolean; agentLabel?: string }) {
  if (resumable && restartPaused) {
    return (
      <div className="native-session-ended" role="status" aria-live="polite">
        <div className="native-session-ended-title">{RESTART_PAUSED_TITLE}</div>
        <div className="native-session-ended-body">{RESTART_PAUSED_BODY}</div>
      </div>
    );
  }
  // When the session is resumable, the continuation path is the "Resume session"
  // button on the composer below — not a fresh pane. Keep the copy consistent.
  const tail = resumable ? 'Resume to continue, or close the pane.' : 'Start a new session to continue.';
  return (
    <div className="native-session-ended" role="status" aria-live="polite">
      <div className="native-session-ended-title">Session ended</div>
      <div className="native-session-ended-body">
        {exitCode == null
          ? `The ${agentLabel} process has exited. ${tail}`
          : exitCode === 0
            ? `The ${agentLabel} process exited cleanly. ${tail}`
            : `The ${agentLabel} process exited with code ${exitCode}. ${tail}`}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Cost / context footer (usage + rate_limit)
// ---------------------------------------------------------------------------

export const UsageFooter = memo(function UsageFooter({
  usage,
  rateLimit,
}: {
  usage?: NativeEvent;
  rateLimit?: NativeEvent;
}) {
  if (!usage && !rateLimit) return null;
  const fmt = (n?: number) => (n != null ? n.toLocaleString() : '0');
  return (
    <div className="native-usage-footer">
      {usage && (
        <>
          <span title="input tokens">in {fmt(usage.inputTokens)}</span>
          {usage.cachedInputTokens ? (
            <span title="cached input tokens">cache {fmt(usage.cachedInputTokens)}</span>
          ) : null}
          <span title="output tokens">out {fmt(usage.outputTokens)}</span>
          {usage.totalCostUsd != null && (
            <span title="session cumulative cost (USD)">session ${usage.totalCostUsd.toFixed(4)}</span>
          )}
        </>
      )}
      {rateLimit?.resetsAt ? (
        <span className="native-rate-limit" title={rateLimit.rateLimitKind || 'rate limit'}>
          resets {formatRateLimitReset(rateLimit.resetsAt)}
        </span>
      ) : null}
    </div>
  );
});

// compactNum — 132648 → "132k", 2_100_000 → "2.1M", <1000 verbatim. Keeps the
// metabar's token counts glanceable instead of a wall of digits.
function compactNum(n?: number): string {
  if (n == null) return '0';
  if (n < 1000) return String(n);
  if (n < 1_000_000) {
    const k = n / 1000;
    return `${k < 10 ? k.toFixed(1).replace(/\.0$/, '') : Math.round(k)}k`;
  }
  const m = n / 1_000_000;
  return `${m < 10 ? m.toFixed(1).replace(/\.0$/, '') : Math.round(m)}M`;
}

// fmtCost — a compact USD label for the cumulative session cost. Sub-cent rounds up
// to "<$0.01" (so a tiny non-zero cost never reads as free); everything else is 2dp
// ("$0.18", "$1.20", "$12.40"). Empty for a missing/negative value.
function fmtCost(usd?: number): string {
  if (usd == null || !Number.isFinite(usd) || usd < 0) return '';
  if (usd > 0 && usd < 0.01) return '<$0.01';
  return `$${usd.toFixed(2)}`;
}

// NativeMetaBar — ONE slim reserved row docked under the composer that replaces
// the old fat usage-footer + the verbose keyboard-hint line, and now ALSO hosts the
// read-only session "vitals" that used to crowd the pane header (model, cwd).
// Two clusters, left + right:
//   • LEFT — the turn status (the dedicated, always-present "is Claude working / is
//     Claude done" line): a spinner + label + live elapsed while in flight; a quiet
//     "✓ worked for Xs" once the turn settles; blank when idle with no completed
//     turn.
//   • RIGHT — the configurable vitals, in fixed order (model · ctx% · ↑in ↓out ·
//     ⚡cache · $cost · resets · cwd). Each is gated by BOTH the user's pref (the
//     `fields` blob from the Preferences gear) AND whether its data is present, so
//     an enabled-but-dataless vital simply doesn't paint. Dim + monospace.
// Always rendered (fixed min-height) so the layout never jumps as the status flips.
// Owns its own 1s ticker — scoped here so the per-second elapsed re-render doesn't
// re-render the whole pane.
export const NativeMetaBar = memo(function NativeMetaBar({
  phase,
  label,
  startedAt,
  doneDurationMs,
  usage,
  rateLimit,
  fields,
  model,
  contextPct,
  contextMeasuring,
  cost,
  cwd,
  cwdFull,
  idleBgShells,
  idleBgAgents,
  workflow,
}: {
  phase: 'working' | 'done' | 'idle';
  label: string;
  startedAt: number | null;
  doneDurationMs?: number;
  usage?: NativeEvent;
  rateLimit?: NativeEvent;
  fields: NativeMetaBarFields;
  model?: string;
  contextPct?: number; // 0..1 fraction of the context window used
  // A /compact (or /clear) freed the window and no post-compact usage was supplied
  // (older CLI boundaries omit postTokens), so the true occupancy is UNKNOWABLE
  // until the next turn re-measures it. Keep the chip in place showing "—"
  // rather than hiding it (the reading is pending, not absent)
  // or showing 0% (the summary itself occupies real tokens — that would be a
  // confident lie for one turn, then visibly jump).
  contextMeasuring?: boolean;
  cost?: number; // cumulative session cost (USD)
  cwd?: string; // "…/<project folder>" for display
  cwdFull?: string; // full absolute path for the hover title
  idleBgShells?: number; // settled-turn count of run_in_background shells not yet exited
  idleBgAgents?: number; // settled-turn count of background Agent subagents not yet completed
  // A background Workflow still running after the turn settled (Workflow tool
  // launches subagents that outlive the launching turn). lastActivityMs is the
  // freshest workflow file mtime (unix-ms; 0 = nothing written yet); the bar
  // derives "active Xs ago" vs "stalled" from it. null when none is running.
  workflow?: { running: boolean; agentsTotal: number; agentsDone: number; lastActivityMs: number } | null;
}) {
  const [now, setNow] = useState(() => Date.now());
  // A live workflow needs the 1s tick too (turn is settled, so phase!=='working')
  // so its "active Xs ago" counter advances and the stall flip is timely.
  const workflowTicking = phase !== 'working' && !!workflow?.running;
  useEffect(() => {
    if (phase !== 'working' && !workflowTicking) return;
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, [phase, workflowTicking]);

  const elapsed = startedAt != null ? now - startedAt : 0;

  let statusEl: React.ReactNode = null;
  if (phase === 'working') {
    statusEl = (
      <>
        <span className="native-spinner" aria-hidden />
        <span className="native-metabar-label">{label}</span>
        {elapsed > 1500 && <span className="native-metabar-elapsed">{fmtDuration(elapsed)}</span>}
      </>
    );
  } else if (phase === 'done' && doneDurationMs != null) {
    statusEl = (
      <>
        <span className="native-metabar-check" aria-hidden>
          ✓
        </span>
        <span className="native-metabar-done">worked for {fmtDuration(doneDurationMs)}</span>
      </>
    );
  }

  // Context % → integer + a severity class so a filling window reads at a glance
  // (amber past 75%, red past 90% — the "you're about to auto-compact" warning).
  const ctxPct = contextPct != null ? Math.round(contextPct * 100) : null;
  const ctxClass = ctxPct == null ? '' : ctxPct >= 90 ? ' is-crit' : ctxPct >= 75 ? ' is-warn' : '';

  // Per-vital gates: user pref AND data present. Order matches NATIVE_METABAR_ORDER.
  const showModel = fields.model && !!model;
  // The measuring state counts as "data present": the chip holds its slot with a
  // dash so a /compact reads as "freed, re-measuring" instead of the vital
  // silently vanishing from the bar.
  const showCtx = fields.context && (ctxPct != null || !!contextMeasuring);
  const showTokens = fields.tokens && !!usage && ((usage.inputTokens || 0) > 0 || (usage.outputTokens || 0) > 0);
  const showCache = fields.cache && !!usage?.cachedInputTokens;
  const showCost = fields.cost && cost != null;
  const showResets = fields.resets && !!rateLimit?.resetsAt;
  const showCwd = fields.cwd && !!cwd;
  const hasVitals = showModel || showCtx || showTokens || showCache || showCost || showResets || showCwd;

  return (
    <div className="native-metabar" role="status" aria-live="polite">
      <div className="native-metabar-status">
        {/* While a background workflow is shown, it stands in for the "✓ worked
            for Xs" done line (the launching turn settled but work continues) —
            matches the chosen footer design. The done line returns once the
            workflow clears. */}
        {!workflowTicking && statusEl}
        {phase !== 'working' && (idleBgShells ?? 0) > 0 && (
          <span
            className="native-metabar-waiting"
            title={`The turn finished, but ${idleBgShells} run_in_background shell${
              idleBgShells! > 1 ? 's' : ''
            } haven't reported completion. The agent resumes on its own when one finishes (Claude Code re-prompts it) — or message it to check now. A shell sitting at 0% CPU may be hung.`}
          >
            ⏳ waiting on {idleBgShells} background shell{idleBgShells! > 1 ? 's' : ''}
          </span>
        )}
        {phase !== 'working' && (idleBgAgents ?? 0) > 0 && (
          <span
            className="native-metabar-waiting"
            title={`The turn finished, but ${idleBgAgents} background agent${
              idleBgAgents! > 1 ? 's are' : ' is'
            } still running. Claude resumes on its own when one completes (a task-notification re-prompts it) — or message the pane to check on it now.`}
          >
            ⏳ waiting on {idleBgAgents} background agent{idleBgAgents! > 1 ? 's' : ''}
          </span>
        )}
        {phase !== 'working' && workflow?.running && (() => {
          // Liveness from the freshest workflow file mtime. Generous stall window
          // (90s): a single long-thinking subagent (e.g. a synthesis pass) can go
          // ~a minute between block writes while genuinely working — mirrors the
          // daemon's own settle backstop, so we don't false-flag "stalled".
          const ageMs = workflow.lastActivityMs > 0 ? Math.max(0, now - workflow.lastActivityMs) : null;
          const stalled = ageMs != null && ageMs > 90_000;
          const counts = workflow.agentsTotal > 0
            ? `${workflow.agentsDone}/${workflow.agentsTotal} agents done`
            : 'starting…';
          const text = ageMs == null
            ? '⚙ workflow starting…'
            : stalled
              ? `⚠ workflow stalled · ${counts} · no activity ${fmtDuration(ageMs)}`
              : `⚙ workflow · ${counts} · active ${fmtDuration(ageMs)} ago`;
          return (
            <span
              className={`native-metabar-workflow${stalled ? ' is-stalled' : ''}`}
              title={
                stalled
                  ? `A background Workflow this pane launched hasn't written to its journal for ${fmtDuration(ageMs!)}. It may have finished without re-prompting the agent, or an agent may be hung — message the pane to check, or watch /workflows.`
                  : 'A background Workflow is still running. Its subagents outlive the launching turn (the Workflow tool returns immediately and re-prompts the agent on completion), so the turn shows settled while work continues. Watch live with /workflows.'
              }
            >
              {text}
            </span>
          );
        })()}
      </div>
      {hasVitals && (
        <div className="native-metabar-usage">
          {showModel && (
            <span className="native-metabar-model" title="model">
              {model}
            </span>
          )}
          {showCtx && (
            <span
              className={`native-metabar-ctx${ctxClass}${ctxPct == null ? ' is-measuring' : ''}`}
              title={ctxPct == null
                ? 'Context freed by a compaction — the new size is measured on the next turn'
                : 'context window used'}
            >
              ctx {ctxPct == null ? '—' : `${ctxPct}%`}
            </span>
          )}
          {showTokens && (
            <>
              <span title="input tokens (latest usage update)">↑{compactNum(usage!.inputTokens)}</span>
              <span title="output tokens (latest usage update)">↓{compactNum(usage!.outputTokens)}</span>
            </>
          )}
          {showCache && <span title="cached input tokens">⚡{compactNum(usage!.cachedInputTokens)}</span>}
          {showCost && (
            <span className="native-metabar-cost" title="session cost (USD)">
              {fmtCost(cost)}
            </span>
          )}
          {showResets && (
            <span className="native-metabar-reset" title={rateLimit!.rateLimitKind || 'rate limit resets'}>
              resets {formatRateLimitReset(rateLimit!.resetsAt)}
            </span>
          )}
          {showCwd && (
            <span className="native-metabar-cwd" title={cwdFull || 'working directory'}>
              {cwd}
            </span>
          )}
        </div>
      )}
    </div>
  );
});

// ---------------------------------------------------------------------------
// Client slash-command menu — a small popover above the composer listing the
// purely-client commands (/clear, /help) matching the typed "/query". The
// keyboard navigation (↑/↓ to move, Enter to run, Esc to close) lives in
// NativePane (it shares the composer's keydown); this is the presentational
// list + the highlighted-row + click-to-run surface. `index` is the highlighted
// row; selecting (click) runs `onPick`. Renders nothing for an empty list.
// ---------------------------------------------------------------------------

export function SlashMenu({
  commands,
  index,
  onPick,
  onHover,
}: {
  commands: SlashCommand[];
  index: number;
  onPick: (cmd: SlashCommand) => void;
  onHover: (i: number) => void;
}) {
  // Keep the highlighted row scrolled into view as ↑/↓ moves through a long list.
  const activeRef = useRef<HTMLButtonElement | null>(null);
  useEffect(() => {
    activeRef.current?.scrollIntoView({ block: 'nearest' });
  }, [index]);
  if (commands.length === 0) return null;
  return (
    <div className="native-slash-menu" role="listbox" aria-label="Slash commands">
      {commands.map((c, i) => (
        <button
          key={c.name}
          ref={i === index ? activeRef : null}
          type="button"
          role="option"
          aria-selected={i === index}
          className={`native-slash-item${i === index ? ' active' : ''}`}
          // Use mousedown so the pick fires BEFORE the textarea blur (a click on
          // the menu would otherwise steal focus + re-fold the composer state).
          onMouseDown={(e) => {
            e.preventDefault();
            onPick(c);
          }}
          onMouseEnter={() => onHover(i)}
        >
          <span className="native-slash-name">{c.name}</span>
          <span className="native-slash-desc">{c.description}</span>
        </button>
      ))}
      <div className="native-slash-foot" aria-hidden="true">
        <kbd>Tab</kbd> add args · <kbd>↵</kbd> run · <kbd>Esc</kbd> close
      </div>
    </div>
  );
}

// A file hit in the @-mention picker (api.searchFiles' hit shape, minus the
// absolute path the menu doesn't need).
export interface AtMentionHit {
  rel: string;
  name: string;
  isDir?: boolean;
}

// AtMentionMenu — the "@" fuzzy file picker floating above the composer.
// Same visual system as SlashMenu (list, active row, mousedown-pick so the
// textarea keeps focus); rows show a dir/file glyph + the cwd-relative path.
export function AtMentionMenu({
  hits,
  index,
  onPick,
  onHover,
}: {
  hits: AtMentionHit[];
  index: number;
  onPick: (hit: AtMentionHit) => void;
  onHover: (i: number) => void;
}) {
  const activeRef = useRef<HTMLButtonElement | null>(null);
  useEffect(() => {
    activeRef.current?.scrollIntoView({ block: 'nearest' });
  }, [index]);
  if (hits.length === 0) return null;
  return (
    <div className="native-slash-menu" role="listbox" aria-label="Files">
      {hits.map((h, i) => (
        <button
          key={h.rel}
          ref={i === index ? activeRef : null}
          type="button"
          role="option"
          aria-selected={i === index}
          className={`native-slash-item${i === index ? ' active' : ''}`}
          onMouseDown={(e) => {
            e.preventDefault();
            onPick(h);
          }}
          onMouseEnter={() => onHover(i)}
        >
          <span className="native-slash-name">
            <span aria-hidden>{h.isDir ? '▸ ' : ''}</span>
            {h.rel}
          </span>
        </button>
      ))}
      <div className="native-slash-foot" aria-hidden="true">
        <kbd>Tab</kbd>/<kbd>↵</kbd> insert · <kbd>Esc</kbd> close
      </div>
    </div>
  );
}

// ModelMenu — the /model picker floating above the composer. Same visual
// system as SlashMenu (list, active row, mousedown-pick so the textarea keeps
// focus). The row matching `currentId` (session_init's reported model,
// normalized — a "[1m]" Fable is still Fable) is marked "current"; picking any
// row calls onPick, which respawns the pane with --model.
export function ModelMenu({
  options,
  currentId,
  index,
  onPick,
  onHover,
}: {
  options: ModelOption[];
  currentId?: string;
  index: number;
  onPick: (opt: ModelOption) => void;
  onHover: (i: number) => void;
}) {
  const activeRef = useRef<HTMLButtonElement | null>(null);
  useEffect(() => {
    activeRef.current?.scrollIntoView({ block: 'nearest' });
  }, [index]);
  if (options.length === 0) return null;
  return (
    <div className="native-slash-menu" role="listbox" aria-label="Claude model">
      {options.map((o, i) => {
        const current = sameModelId(currentId, o.id);
        return (
          <button
            key={o.id}
            ref={i === index ? activeRef : null}
            type="button"
            role="option"
            aria-selected={i === index}
            className={`native-slash-item${i === index ? ' active' : ''}`}
            onMouseDown={(e) => {
              e.preventDefault();
              onPick(o);
            }}
            onMouseEnter={() => onHover(i)}
          >
            <span className="native-slash-name">
              {o.label}
              {current ? ' ✓' : ''}
            </span>
            <span className="native-slash-desc">
              {current ? 'current — ' : ''}
              {o.hint}
            </span>
          </button>
        );
      })}
      <div className="native-slash-foot" aria-hidden="true">
        <kbd>↵</kbd> switch model (conversation kept) · <kbd>Esc</kbd> close
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Keyboard cheatsheet — the /help overlay. A compact card of <kbd>key</kbd> →
// description rows from KEYBOARD_SHORTCUTS, dismissable with the ✕ or Esc (wired
// by NativePane). Floats above the composer like the slash menu.
// ---------------------------------------------------------------------------

export function Cheatsheet({
  shortcuts,
  onClose,
}: {
  shortcuts: Shortcut[];
  onClose: () => void;
}) {
  return (
    <div className="native-cheatsheet" role="dialog" aria-label="Keyboard shortcuts">
      <div className="native-cheatsheet-head">
        <span className="native-cheatsheet-title">Keyboard shortcuts</span>
        <button
          type="button"
          className="native-cheatsheet-close"
          aria-label="Close shortcuts"
          onClick={onClose}
        >
          ✕
        </button>
      </div>
      <div className="native-cheatsheet-body">
        {shortcuts.map((s, i) => (
          <div key={i} className="native-cheatsheet-row">
            <kbd className="native-cheatsheet-key">{s.keys}</kbd>
            <span className="native-cheatsheet-desc">{s.desc}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

// ModeSwitchConfirm — the inline three-way confirm popover shown when the user
// clicks the renderer toggle WHILE a turn is in flight (busy / recent /
// choice-prompt). It does NOT switch on its own: it surfaces the choice and
// calls back. Anchored absolutely under the toggle by its CSS; the caller
// positions the wrapping element.
//
// Three actions:
//   • "Switch when done" — PRIMARY, full-width on its own row, AUTOFOCUSED. The
//     lossless default: arm a deferred switch that fires when the turn finishes.
//     Enter triggers it (window-level, so it works regardless of focus).
//   • "Switch now" — secondary, warn-tinted OUTLINE (de-emphasized). Interrupts
//     the current turn immediately. CLICK-ONLY: no Enter/key accelerator, so a
//     stray keystroke can never abort live work.
//   • "Cancel" — quiet. Esc.
//
// If the turn SETTLES while this is open (`working` flips false — the caller
// recomputes shouldConfirmModeSwitch live and passes it down), collapse to a
// simple two-button "switch (lossless) / cancel": there's nothing to interrupt,
// so the now/deferred distinction disappears. Does NOT auto-fire.
//   • `target` is the renderer being switched TO ('' = terminal, 'native').
//   • onConfirm('deferred') = lossless; onConfirm('now') = interrupt now.
export function ModeSwitchConfirm({
  target,
  working,
  agentLabel = 'Claude',
  onConfirm,
  onCancel,
}: {
  target: '' | 'native';
  // Whether the pane is STILL working (recomputed live by the caller). When this
  // flips false mid-confirm, the popover collapses to the two-button form.
  working: boolean;
  agentLabel?: string;
  onConfirm: (choice: 'deferred' | 'now') => void;
  onCancel: () => void;
}) {
  const confirmRef = useRef<HTMLButtonElement | null>(null);
  const toLabel = modeLabel(target);

  // Autofocus the primary (lossless) action so Enter/Space lands on it
  // immediately and the popover is keyboard-operable the instant it opens.
  useEffect(() => {
    confirmRef.current?.focus();
  }, []);

  // Window-level Esc/Enter so the keys work even before focus settles (and even
  // if the user tabs away). Esc cancels; Enter triggers the LOSSLESS default
  // ('deferred' while working, plain switch once settled). "Switch now" has NO
  // key accelerator — interrupting live work must be an explicit click. Stop
  // here so the pane's own Esc/Enter handlers don't also fire.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented) return;
      if (e.key === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        onCancel();
      } else if (e.key === 'Enter' && !e.shiftKey && !e.metaKey && !e.ctrlKey && !e.altKey) {
        e.preventDefault();
        e.stopPropagation();
        onConfirm('deferred');
      }
    };
    window.addEventListener('keydown', onKey, true);
    return () => window.removeEventListener('keydown', onKey, true);
  }, [onConfirm, onCancel]);

  return (
    <div
      className="native-switch-confirm"
      role="alertdialog"
      aria-label={
        working
          ? `${agentLabel} is working — switch to ${toLabel} when the turn finishes, or interrupt now?`
          : `Switch to ${toLabel}?`
      }
      // Swallow header drag / focus churn while the popover is open.
      onMouseDownCapture={(e) => e.stopPropagation()}
      onDoubleClick={(e) => e.stopPropagation()}
    >
      <p className="native-switch-confirm-msg">
        {working ? (
          <>
            {agentLabel} is working — switch to <strong>{toLabel}</strong> when the turn finishes, or
            interrupt now?
          </>
        ) : (
          <>
            The turn finished — switch to <strong>{toLabel}</strong>?
          </>
        )}
      </p>
      {working ? (
        <div className="native-switch-confirm-actions native-switch-confirm-actions-deferred">
          {/* PRIMARY, full-width on its own row, autofocused, Enter-triggered. */}
          <button
            type="button"
            ref={confirmRef}
            className="native-switch-confirm-btn native-switch-confirm-defer"
            onClick={(e) => {
              e.stopPropagation();
              onConfirm('deferred');
            }}
          >
            Switch when done
          </button>
          <div className="native-switch-confirm-row">
            <button
              type="button"
              className="native-switch-confirm-btn native-switch-confirm-cancel"
              onClick={(e) => {
                e.stopPropagation();
                onCancel();
              }}
            >
              Cancel
            </button>
            {/* Secondary, warn-tinted OUTLINE, de-emphasized. CLICK-ONLY: no
                Enter/key accelerator (a keystroke must never interrupt). */}
            <button
              type="button"
              className="native-switch-confirm-btn native-switch-confirm-now"
              onClick={(e) => {
                e.stopPropagation();
                onConfirm('now');
              }}
            >
              Switch now
            </button>
          </div>
        </div>
      ) : (
        // Settled mid-confirm: nothing to interrupt, so collapse to lossless
        // switch / cancel. Enter still triggers the (now plain) switch.
        <div className="native-switch-confirm-actions">
          <button
            type="button"
            className="native-switch-confirm-btn native-switch-confirm-cancel"
            onClick={(e) => {
              e.stopPropagation();
              onCancel();
            }}
          >
            Cancel
          </button>
          <button
            type="button"
            ref={confirmRef}
            className="native-switch-confirm-btn native-switch-confirm-defer"
            onClick={(e) => {
              e.stopPropagation();
              onConfirm('deferred');
            }}
          >
            Switch to {toLabel}
          </button>
        </div>
      )}
    </div>
  );
}

// PaneModeToggle — the Terminal/Native segmented control + its pending indicator,
// shared by the terminal-pane header (PaneHeader) and the native-pane header
// (NativePane) so the four states render identically in both. Pure presentation:
// the caller owns the switch/confirm/cancel callbacks and the in-flight flag.
//
// Four visual states (toggleVisual precedence: flipping > pending > idle):
//   • idle      — plain segments, current one active+pressed, ENABLED.
//   • flipping  — round-8 in-progress (paneModeSwitching): neutral "…", DISABLED,
//                 aria-busy. The active "now-switch" round-trip.
//   • pending   — a DEFERRED switch is armed + waiting (pane.pendingSwitch): the
//                 TARGET segment shows "{Native|Terminal} ⏳" with a dashed slow
//                 opacity PULSE in the magenta/violet accent, segments ENABLED
//                 (clickable). Distinct from flipping by design.
// Gestures while pending (wired by the caller via onCancelPending / onSwitchNow):
//   • plain-click any segment → cancel the pending switch.
//   • Shift-click the pending (target) segment → override → switch now.
//   • the small ✕ on the pending segment → explicit cancel.
export function PaneModeToggle({
  isNative,
  visual,
  pending,
  onSeg,
  onCancelPending,
  onSwitchNow,
  termTitle,
  nativeTitle,
  idleTitle,
  agentLabel = 'Claude',
  disabledReason,
}: {
  // Whether the pane is CURRENTLY native (drives which segment is active). For a
  // pending switch the active segment is still the CURRENT renderer — the target
  // segment is the one that pulses.
  isNative: boolean;
  visual: 'flipping' | 'pending' | 'idle';
  // The armed pending switch (only meaningful when visual==='pending').
  pending: PendingSwitch | null | undefined;
  // Click a segment to switch TO that kind ('' = terminal). Caller decides
  // whether that opens the confirm (working) or switches immediately (idle).
  onSeg: (to: '' | 'native') => void;
  // Cancel the armed pending switch (plain-click while pending, or the ✕).
  onCancelPending: () => void;
  // Override the armed pending switch → switch now (Shift-click the target).
  onSwitchNow: () => void;
  termTitle: string;
  nativeTitle: string;
  // The group tooltip when idle (the renderer explainer). pending/flipping
  // supply their own.
  idleTitle: string;
  agentLabel?: string;
  // A visible-but-locked discovery state (currently fresh shell-launched
  // Codex): there is no resumable thread to hand to Focus yet.
  disabledReason?: string;
}) {
  const switching = visual === 'flipping';
  const disabled = !!disabledReason;
  const isPending = visual === 'pending';
  const targetLabel = pendingSwitchLabel(pending); // 'Focus' | 'Terminal' | ''
  const pendingIsNative = pending?.target === 'native';

  const groupTitle = disabledReason || (switching
    ? 'Switching renderer… (the pane is restarting)'
    : isPending && targetLabel
      ? `Will switch to ${targetLabel} when ${agentLabel} finishes · click to cancel · Shift-click to switch now`
      : idleTitle);

  // A click on the pending TARGET segment: Shift = override→now, plain = cancel.
  const onPendingTargetClick = (e: ReactMouseEvent) => {
    e.stopPropagation();
    if (e.shiftKey) onSwitchNow();
    else onCancelPending();
  };
  // A click on the non-target segment while pending = cancel too.
  const onPendingOtherClick = (e: ReactMouseEvent) => {
    e.stopPropagation();
    onCancelPending();
  };

  // Per-segment helpers: is THIS segment the pending target?
  const termIsTarget = isPending && !pendingIsNative;
  const nativeIsTarget = isPending && pendingIsNative;

  return (
    <div
      className={`pane-mode-toggle ${switching ? 'switching' : ''}${isPending ? ' pending' : ''}${disabled ? ' identity-pending' : ''}`}
      role="group"
      aria-label={
        isPending && targetLabel
          ? `Renderer mode — switching to ${targetLabel} when ${agentLabel} finishes`
          : 'Renderer mode'
      }
      aria-busy={switching || undefined}
      aria-disabled={disabled || undefined}
      title={groupTitle}
      onMouseDownCapture={(e) => e.stopPropagation()}
      onDoubleClick={(e) => e.stopPropagation()}
    >
      <button
        type="button"
        className={`pane-mode-seg ${!isNative ? 'active' : ''}${termIsTarget ? ' pane-mode-seg-pending' : ''}`}
        aria-pressed={!isNative}
        aria-label={switching && !isNative ? 'Terminal renderer — switching…' : 'Terminal renderer'}
        disabled={switching || disabled}
        title={termIsTarget ? groupTitle : termTitle}
        onClick={(e) => {
          if (isPending) {
            if (termIsTarget) onPendingTargetClick(e);
            else onPendingOtherClick(e);
            return;
          }
          e.stopPropagation();
          if (isNative) onSeg('');
        }}
      >
        {'Term'}
        {switching && !isNative && <span className="pane-mode-spin" aria-hidden />}
        {termIsTarget && (
          <>
            <span className="pane-mode-pending-when" aria-hidden>
              {' '}when done
            </span>
            <span
              className="pane-mode-pending-x"
              role="button"
              aria-label={`Cancel pending switch to ${targetLabel}`}
              title="Cancel pending switch"
              onClick={(e) => {
                e.stopPropagation();
                onCancelPending();
              }}
            >
              ✕
            </span>
          </>
        )}
      </button>
      <button
        type="button"
        className={`pane-mode-seg pane-mode-seg-native ${isNative ? 'active' : ''}${nativeIsTarget ? ' pane-mode-seg-pending' : ''}`}
        aria-pressed={isNative}
        aria-label={switching && isNative ? 'Focus renderer — switching…' : 'Focus renderer'}
        disabled={switching || disabled}
        title={nativeIsTarget ? groupTitle : nativeTitle}
        onClick={(e) => {
          if (isPending) {
            if (nativeIsTarget) onPendingTargetClick(e);
            else onPendingOtherClick(e);
            return;
          }
          e.stopPropagation();
          if (!isNative) onSeg('native');
        }}
      >
        {'Focus'}
        {switching && isNative && <span className="pane-mode-spin" aria-hidden />}
        {nativeIsTarget && (
          <>
            <span className="pane-mode-pending-when" aria-hidden>
              {' '}when done
            </span>
            <span
              className="pane-mode-pending-x"
              role="button"
              aria-label={`Cancel pending switch to ${targetLabel}`}
              title="Cancel pending switch"
              onClick={(e) => {
                e.stopPropagation();
                onCancelPending();
              }}
            >
              ✕
            </span>
          </>
        )}
      </button>
    </div>
  );
}

// SwitchInterruptedBanner — the conversation-discontinuity marker shown in a
// native pane when pane.switchNotice is set. Pinned above the composer. The
// `reason` picks the copy:
//   • absent/'' — a renderer switch aborted whatever Claude was doing.
//   • 'resume-repair' — a daemon restart (usually an update) cut the previous
//     response off mid-tool; the resume repaired the transcript.
//   • 'fresh-start' — the requested resume couldn't continue the previous
//     conversation; this pane started a NEW one. Informational: no Continue
//     button (there is nothing to continue in the fresh session).
//   • Continue → sends a short "continue where you left off" turn (which clears
//     the daemon's notice on the next input). The caller wires the send.
//   • ✕ → dismisses locally (the caller keys the dismissed-set by switchNotice.at
//     so it doesn't reappear for the SAME notice).
export function SwitchInterruptedBanner({
  fromKind,
  reason,
  onContinue,
  onDismiss,
}: {
  fromKind: 'native' | 'terminal';
  reason?: 'resume-repair' | 'fresh-start';
  onContinue: () => void;
  onDismiss: () => void;
}) {
  const fromLabel = fromKindLabel(fromKind);
  const text =
    reason === 'resume-repair' ? (
      <>A restart interrupted the previous response.</>
    ) : reason === 'fresh-start' ? (
      <>Couldn&rsquo;t resume the previous conversation — started a fresh session.</>
    ) : (
      <>
        Switched from <strong>{fromLabel}</strong> mid-turn — the previous response was interrupted.
      </>
    );
  return (
    <div className="native-interrupted-banner" role="status">
      <span className="native-interrupted-glyph" aria-hidden>
        ↩
      </span>
      <span className="native-interrupted-text">{text}</span>
      {reason !== 'fresh-start' && (
        <button
          type="button"
          className="native-interrupted-continue"
          title="Send “Continue where you left off.” to resume"
          onClick={(e) => {
            e.stopPropagation();
            onContinue();
          }}
        >
          Continue
        </button>
      )}
      <button
        type="button"
        className="native-interrupted-dismiss"
        aria-label="Dismiss interrupted notice"
        title="Dismiss"
        onClick={(e) => {
          e.stopPropagation();
          onDismiss();
        }}
      >
        ✕
      </button>
    </div>
  );
}
