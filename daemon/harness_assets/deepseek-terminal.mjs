#!/usr/bin/env node
// A terminal client for the official DSH ACP profile, not a replacement agent.
import { spawn } from 'node:child_process';
import { EventEmitter } from 'node:events';
import readline from 'node:readline';
import fs from 'node:fs/promises';
import fsSync from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { format } from 'node:util';
import { canonicalDeepseekModel, dshModel, validateDeepseekSelector, dshPatchEntries, rotateDshAnonymousId, decodeDshModelValue, encodeDshModelValue } from './deepseek-model.mjs';

const [runtime, rawModel = '', ...args] = process.argv.slice(2);
if (!runtime || args.some(arg => arg !== '--help')) {
  console.error('Usage: deepseek (then /help for commands)'); process.exit(1);
}
if (args.includes('--help')) {
  console.log('DeepSeek Harness in SoluTerminal. Commands: /new, /sessions, /resume, /model, /effort, /status, /context, /thinking, /allow, /clear, /help, /exit (also /quit). Type / for suggestions; Ctrl+C cancels a turn.'); process.exit(0);
}
let model;
try { model = dshModel(rawModel); }
catch (error) { console.error(error.message); process.exit(1); }
// A managed launch always sets an absolute DSH_HOME (launch.mjs). Running the
// client by hand without one would otherwise crash on path.join(undefined, …)
// with a stack trace; fail with a usable message instead.
const dshHome = process.env.DSH_HOME;
if (!dshHome || !path.isAbsolute(dshHome)) {
  console.error('DSH_HOME must be set to an absolute path. Launch DeepSeek from SoluTerminal → Credentials, or set DSH_HOME.');
  process.exit(1);
}
// Build identity. The launcher resolves it (extracted VERSION, else the repo's
// Tauri version) and passes SOLU_CLIENT_BUILD; the local VERSION file is the
// fallback for a client started by hand. Restarting a pane is the only way to
// pick up a new build, and a resumed session keeps the build it started with.
const clientDir = path.dirname(fileURLToPath(import.meta.url));
const clientVersion = (process.env.SOLU_CLIENT_BUILD || '').trim()
  || await fs.readFile(path.join(clientDir, 'VERSION'), 'utf8').then(value => value.trim()).catch(() => 'dev');
const dshVersion = await fs.readFile(path.join(path.dirname(runtime), '..', 'package.json'), 'utf8')
  .then(raw => JSON.parse(raw).version).catch(() => '');
// A SIGKILL cannot run the graceful cleanup, so a crashed session can leave its
// `terminal-XXXXXX` scratch directory (and the route.yaml inside it) under
// DSH_HOME. Sweep ones that are clearly abandoned. Match the mkdtemp shape
// exactly and require the route.yaml payload, so the long-lived
// terminal-transcripts / terminal-history siblings are never eligible.
const PATCH_DIR_TTL_MS = 6 * 60 * 60 * 1000;
function sweepStalePatchDirs() {
  try {
    const now = Date.now();
    for (const name of fsSync.readdirSync(dshHome)) {
      if (!/^terminal-[A-Za-z0-9]{6}$/.test(name)) continue;
      const dir = path.join(dshHome, name);
      try {
        const stat = fsSync.statSync(dir);
        if (!stat.isDirectory() || now - stat.mtimeMs < PATCH_DIR_TTL_MS) continue;
        if (!fsSync.existsSync(path.join(dir, 'route.yaml'))) continue;
        fsSync.rmSync(dir, { recursive: true, force: true });
      } catch {}
    }
  } catch {}
}
sweepStalePatchDirs();
const patchDir = await fs.mkdtemp(path.join(dshHome, 'terminal-'));
const patch = path.join(patchDir, 'route.yaml');
await fs.writeFile(patch, JSON.stringify(dshPatchEntries(model)), { mode: 0o600 });
// dsh sends a per-home anonymous id (x-deepseek-harness-user-id) with every
// request and offers no way to omit it. A fresh one per launch keeps it from
// linking this pane's sessions to earlier ones.
await rotateDshAnonymousId(dshHome);
const child = spawn(process.execPath, [runtime, '--profile', 'acp', '--patch', patch], { env: { ...process.env, DSH_TELEMETRY_DISABLED: '1', DSH_TELEMETRY_MODE: 'DISABLED' }, stdio: ['pipe', 'pipe', 'pipe'] });
// Local transcript + input history live under DSH_HOME so each managed
// machine keeps its own. The transcript is ours, not DSH's: it lets /resume
// replay recent context the ACP server never sends back.
const transcriptDir = path.join(dshHome, 'terminal-transcripts');
await fs.mkdir(transcriptDir, { recursive: true, mode: 0o700 });
const historyFile = path.join(dshHome, 'terminal-history');
// Per-tool "always allow". DSH's ACP profile only offers allow_once/reject_once
// (checked against @deepseek-ai/dsh@0.1.5-rc.1 and 0.2.2-rc.1), so a remembered allowlist has
// to live here: keyed on the tool kind, persisted under DSH_HOME, consulted
// before the approval menu is drawn. /allow lists or clears it.
const alwaysAllowFile = path.join(dshHome, 'terminal-allow.json');
// ACP tool kinds DSH uses, mapped to the label shown in the menu.
const TOOL_KIND_LABELS = { execute: 'Bash', read: 'Read', edit: 'Edit', write: 'Write', search: 'Search', fetch: 'Web', move: 'Move', delete: 'Delete', think: 'Think', switch_mode: 'Mode' };
// The renderer classifies tool calls from their title because the wire does not
// always carry `kind`, so permission requests use the same heuristics.
const TOOL_GLYPH_LABELS = { '$': 'Bash', read: 'Read', write: 'Write', edit: 'Edit', grep: 'Search', list: 'List', web: 'Web', agent: 'Agent', todo: 'Todo' };
// "Always allow" is not offered for any DeepSeek tool. DSH's permission
// request names only the call id (dsh-acp, 0.1.5-rc.1 and 0.2.2-rc.1), so the
// tool can't be identified when it is answered, and even DSH's "read" kind
// covers tools that reach the network or another process (Cordis inspect
// queries, terminal reads, the workspace runtime install). Every request asks.
function canAlwaysAllow() { return false; }
// Map a permission request to a stable, user-facing tool category. An
// unclassifiable tool returns an empty key: we do not offer "always allow" for
// it rather than lumping every unknown tool into one blanket allowance.
function permissionTool(request) {
  const call = request?.params?.toolCall ?? {};
  const kind = String(call.kind ?? '').trim().toLowerCase();
  if (TOOL_KIND_LABELS[kind]) return { key: kind, label: TOOL_KIND_LABELS[kind] };
  const label = TOOL_GLYPH_LABELS[toolGlyph(call)];
  return label ? { key: `title:${label}`, label } : { key: '', label: 'tool' };
}
// The answerable rows of a permission prompt, in menu order. Kept in lockstep
// with renderPermission's printed menu and submit()'s answer mapping: the
// selector, the printed menu, and the wire option kinds are three views of the
// same list, so adding a row here without the other two would desync them.
function permissionChoices(request) {
  const choices = [
    { answer: '1', label: 'Yes, allow once' },
    { answer: '2', label: 'No, reject' },
  ];
  if (canAlwaysAllow(permissionTool(request).key)) choices.push({ answer: '3', label: `Always allow ${permissionTool(request).label}` });
  return choices;
}
// A single keypress that answers the prompt without Enter, matching the TUI
// harnesses: the digit selects its row, y/n/a answer directly. Returns the
// answer string, or null for a key that is not a hotkey.
function permissionHotkey(ch) {
  if (!permission) return null;
  const key = String(ch).toLowerCase();
  if (permissionChoices(permission).some(choice => choice.answer === key)) return key;
  if (key === 'y') return '1';
  if (key === 'n') return '2';
  if (key === 'a') return '3';
  return null;
}
let alwaysAllow = new Map(); // key -> label
// Entries an older client saved for kinds that may no longer be remembered
// (canAlwaysAllow): dropped on load, saved away below, and named once.
let droppedAlwaysAllow = [];
try {
  const saved = JSON.parse(await fs.readFile(alwaysAllowFile, 'utf8'));
  if (saved && typeof saved === 'object' && !Array.isArray(saved)) {
    for (const [key, label] of Object.entries(saved)) {
      if (canAlwaysAllow(String(key))) alwaysAllow.set(String(key), String(label));
      else droppedAlwaysAllow.push(String(label));
    }
  }
} catch {}
// Small state files are replaced atomically so a crash cannot truncate them.
async function writePrivateAtomic(file, text) {
  const temp = `${file}.${process.pid}.tmp`;
  await fs.writeFile(temp, text, { mode: 0o600 });
  await fs.rename(temp, file);
}
async function saveAlwaysAllow() {
  try { await writePrivateAtomic(alwaysAllowFile, JSON.stringify(Object.fromEntries(alwaysAllow))); } catch {}
}
if (droppedAlwaysAllow.length) {
  await saveAlwaysAllow();
  console.log(`Always allow is no longer available for DeepSeek tools; ${droppedAlwaysAllow.map(label => label.replace(/[\x00-\x1f\x7f-\x9f]/g, '')).join(', ')} will ask each time now.`);
}
// Single source of truth for the REPL surface. Powers /help, the live "/"
// suggestion list, and Tab completion so the three can never drift apart.
const COMMANDS = [
  { name: '/new',      args: '',       desc: 'start a fresh session' },
  { name: '/sessions', args: '',       desc: 'list saved sessions (alias: /resume)' },
  { name: '/resume',   args: '[N|ID]', desc: 'resume a session by number or id' },
  { name: '/model',    args: '[N]',    desc: 'show or choose the model' },
  { name: '/effort',   args: '[N]',    desc: 'show or choose reasoning effort' },
  { name: '/status',   args: '',       desc: 'model, session, cwd and context usage' },
  { name: '/tasks',    args: '',       desc: 'list running background jobs' },
  { name: '/tools',    args: '[mode]', desc: 'collapse tool result bodies (full/collapse)' },
  { name: '/hotkeys',  args: '',       desc: 'show key bindings' },
  { name: '/export',   args: '[file]', desc: 'copy this session transcript to a file' },
  { name: '/import',   args: '<file>', desc: 'replay an exported session transcript' },
  { name: '/context',  args: '',       desc: 'context-window usage' },
  { name: '/thinking', args: '',       desc: 'cycle reasoning display (summary/full/off)' },
  { name: '/allow',    args: '[clear]', desc: 'list or clear always-allow tools' },
  { name: '/clear',    args: '',       desc: 'clear the screen and start a fresh session' },
  { name: '/help',     args: '',       desc: 'show this help' },
  { name: '/exit',     args: '',       desc: 'quit (also /quit)' },
];
const commandHits = token => {
  const head = String(token || '').split(/\s+/)[0];
  return head.startsWith('/') ? COMMANDS.filter(row => row.name.startsWith(head)) : [];
};
const completer = line => {
  const hits = commandHits(line);
  const names = hits.length ? hits : (line.startsWith('/') ? COMMANDS : []);
  return [names.map(row => row.name + (row.args ? ' ' : '')), line];
};
// On a real terminal we run our own raw-mode composer (pinned status line,
// multi-line input, history, completion). Pipes are not raw: tests and
// redirected input keep readline's deterministic line behaviour.
// SOLU_HARNESS_FORCE_RAW drives the raw composer over pipes. It is the only way
// to cover the bottom-anchored composer/redraw path in the test suite, which
// otherwise runs with non-TTY stdio and takes the readline branch.
const RAW = process.env.SOLU_HARNESS_FORCE_RAW === '1'
  || (!!process.stdin.isTTY && !!process.stdout.isTTY && typeof process.stdin.setRawMode === 'function');
const HISTORY_LIMIT = 200;
const history = [];
let rl = null;
if (!RAW) rl = readline.createInterface({ input: process.stdin, output: process.stdout, terminal: false, historySize: HISTORY_LIMIT, completer });
try {
  const saved = (await fs.readFile(historyFile, 'utf8')).split('\n').map(entry => entry.trimEnd()).filter(Boolean).slice(-HISTORY_LIMIT);
  if (saved.length > 0) { if (RAW) history.push(...saved.reverse()); else rl.history = saved.reverse(); }
} catch {}
async function saveHistory() {
  // Redact defensively at the write site too, in case a non-raw readline
  // history entry was never passed through safe().
  const entries = (RAW ? history : (rl?.history ?? [])).slice(0, HISTORY_LIMIT).map(entry => safe(entry)).reverse();
  try { await writePrivateAtomic(historyFile, entries.join('\n') + '\n'); } catch {}
}
let transcriptPath = null;
const transcriptName = id => `${String(id).replace(/[^A-Za-z0-9_-]/g, '_').slice(0, 80) || 'session'}.jsonl`;
async function transcript(entry) {
  if (!transcriptPath) return;
  try { await fs.appendFile(transcriptPath, JSON.stringify({ t: Date.now(), ...entry }) + '\n', { mode: 0o600 }); } catch {}
}
async function pruneTranscript(file) {
  try {
    const stat = await fs.stat(file).catch(() => null);
    if (!stat || stat.size <= 512 * 1024) return;
    const lines = (await fs.readFile(file, 'utf8')).split('\n').filter(Boolean);
    await writePrivateAtomic(file, lines.slice(Math.floor(lines.length / 2)).join('\n') + '\n');
  } catch {}
}
const REPLAY_LIMIT = 20;
function replayLine(entry) {
  if (entry.role === 'user') return renderUserTurn(String(entry.text ?? '').slice(0, 500));
  if (entry.role === 'agent') {
    // An interrupted turn can leave an empty agent entry behind; skip it rather
    // than replay a bare "— end_turn" with no text.
    const text = safe(String(entry.text ?? '').slice(0, 4000));
    if (!text.trim()) return null;
    return `${renderMarkdown(text)}\n${dim(`— ${entry.stop || 'end_turn'}`)}`;
  }
  if (entry.role === 'approval') {
    const allowed = String(entry.decision ?? '').startsWith('allow');
    return `${paint(allowed ? '✔' : '✖', allowed ? 'green' : 'red')} ${dim('approval')} ${safe(entry.title ?? '')}`;
  }
  return null;
}
// Resume history is selected by conversation, not by raw transcript line. Every
// tool call is its own entry (a single heavy turn writes dozens), so slicing the
// last N lines replayed a wall of `● bash` and could push every user/agent
// message out of the window. Collapse each consecutive run of tool calls into one
// summary line first, then keep the last REPLAY_LIMIT conversational blocks.
function replayBlocks(entries) {
  const blocks = [];
  let tools = 0;
  const flush = () => {
    if (tools === 0) return;
    blocks.push(`${paint('●', 'yellow')} ${dim(tools === 1 ? '1 tool call' : `${tools} tool calls`)}`);
    tools = 0;
  };
  for (const entry of entries) {
    if (entry.role === 'tool') { tools += 1; continue; }
    flush();
    const line = replayLine(entry);
    if (line) blocks.push(line);
  }
  flush();
  return blocks;
}
async function replayTranscript(file) {
  let lines = [];
  try { lines = (await fs.readFile(file, 'utf8')).split('\n').filter(Boolean); } catch { return; }
  const entries = [];
  for (const line of lines) { try { entries.push(JSON.parse(line)); } catch {} }
  const shown = replayBlocks(entries).slice(-REPLAY_LIMIT);
  if (shown.length === 0) return;
  process.stdout.write(dim(`\n── replaying last ${shown.length} saved message${shown.length === 1 ? '' : 's'} (history, not re-sent) ──\n`));
  for (const row of shown) process.stdout.write(row + '\n');
  process.stdout.write(dim('── end replay ──\n'));
}
// Pasted-image handling. SoluTerminal panes inject image pastes as
// `@/absolute/path.png` (a Claude Code convention). DSH understands no `@`
// prefix, so resolve such tokens here: attach the bytes as ACP image blocks
// when the bridge advertises image prompts, otherwise rewrite to the plain
// path and say so instead of letting the agent fail on a bogus `@/...` file.
const IMAGE_MIME = { '.png': 'image/png', '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg', '.gif': 'image/gif', '.webp': 'image/webp' };
const MAX_IMAGE_BYTES = 10 * 1024 * 1024;
const MAX_IMAGE_BLOCKS = 20;
async function resolveImageRefs(text) {
  const pattern = /@(\/[^\s'"]+?\.(?:png|jpe?g|gif|webp))/gi;
  const blocks = [];
  let out = '', last = 0, noticed = false;
  for (const match of text.matchAll(pattern)) {
    const file = match[1];
    let ok = false;
    try {
      const stat = await fs.stat(file);
      ok = stat.isFile() && stat.size > 0 && stat.size <= MAX_IMAGE_BYTES;
    } catch { ok = false; }
    if (!ok) continue;
    const base = path.basename(file);
    const attachable = imageCapableNow();
    if (attachable && blocks.length < MAX_IMAGE_BLOCKS) {
      try {
        const mime = IMAGE_MIME[path.extname(file).toLowerCase()] || 'image/png';
        blocks.push({ type: 'image', data: (await fs.readFile(file)).toString('base64'), mimeType: mime });
        out += text.slice(last, match.index) + `[image: ${base}]`;
        last = match.index + match[0].length;
      } catch { continue; }
    } else {
      if (!noticed) {
        noticed = true;
        emitOutput(dim(attachable
          ? `Only ${MAX_IMAGE_BLOCKS} images can be attached to one message; sent the remaining paths as text: ${file}`
          : `Images can't be attached here: this model/harness does not accept image input. Sent the file path as text instead: ${file}`) + '\n');
      }
      out += text.slice(last, match.index) + file;
      last = match.index + match[0].length;
    }
  }
  return { text: out + text.slice(last), blocks };
}
// Pi-style `@file` references: `@path` to an existing text file is inlined
// (bounded) so the model reads it without a tool round-trip. Images are handled
// above; a resolved image token is already gone by the time this runs.
const INLINE_MAX_BYTES = 32 * 1024;
const INLINE_MAX_FILES = 3;
async function resolvePromptRefs(text) {
  const images = await resolveImageRefs(text);
  const inlined = [];
  let additions = '';
  for (const match of images.text.matchAll(/(^|\s)@([^\s@'"]+)/g)) {
    if (inlined.length >= INLINE_MAX_FILES) break;
    const token = match[2];
    if (IMAGE_MIME[path.extname(token).toLowerCase()]) continue;
    const candidate = path.isAbsolute(token) ? token : path.resolve(process.cwd(), token);
    let stat;
    try { stat = await fs.stat(candidate); } catch { continue; }
    if (!stat.isFile() || stat.size === 0 || stat.size > INLINE_MAX_BYTES) continue;
    let content;
    try { content = await fs.readFile(candidate, 'utf8'); } catch { continue; }
    if (content.includes('\0')) continue; // binary, not text
    const rel = path.relative(process.cwd(), candidate) || candidate;
    inlined.push(rel);
    additions += `\n\n--- ${rel} ---\n\`\`\`\n${content.replace(/\r\n?/g, '\n')}\n\`\`\``;
  }
  return { text: additions ? `${images.text}${additions}` : images.text, blocks: images.blocks, inlined };
}
// dsh is local, but a runaway or corrupt frame must not grow memory without
// limit: read its stdout in bounded lines instead of an unbounded readline.
const WIRE_MAX_LINE = Number(process.env.SOLU_WIRE_MAX_LINE) || 8 * 1024 * 1024;
const wire = new EventEmitter();
// The line handler must never take the pane down via the stream's data event:
// report a thrown message handler and keep reading.
const emitWireLine = line => { try { wire.emit('line', line); } catch (error) { console.error(safe(String(error?.message ?? error))); } };
{
  let buffer = '';
  child.stdout.setEncoding('utf8');
  child.stdout.on('data', chunk => {
    buffer += chunk;
    for (let index = buffer.indexOf('\n'); index >= 0; index = buffer.indexOf('\n')) {
      const line = buffer.slice(0, index).replace(/\r$/, '');
      buffer = buffer.slice(index + 1);
      emitWireLine(line);
    }
    if (buffer.length > WIRE_MAX_LINE) {
      buffer = '';
      wire.emit('error', new Error('DeepSeek Harness sent an oversized frame'));
    }
  });
  child.stdout.on('end', () => { if (buffer) { emitWireLine(buffer); buffer = ''; } });
  child.stdout.on('error', error => wire.emit('error', error));
}
const pending = new Map();
// A turn may legitimately run for minutes, so session/prompt is sent with no
// timeout. But if DSH ignores session/cancel, the busy lock would keep the pane
// dead forever — arm a bounded grace after a cancel and unlock if it never
// answers. Grace is overridable so tests need not wait 15s.
let promptRequest = null, cancelWatchdog = null;
const CANCEL_GRACE_MS = Number(process.env.SOLU_CANCEL_GRACE_MS) || 15000;
function clearCancelWatchdog() { if (cancelWatchdog) { clearTimeout(cancelWatchdog); cancelWatchdog = null; } }
function armCancelWatchdog() {
  if (cancelWatchdog || promptRequest == null || !pending.has(promptRequest)) return;
  cancelWatchdog = setTimeout(() => {
    cancelWatchdog = null;
    const id = promptRequest; promptRequest = null;
    const call = id != null ? pending.get(id) : null;
    if (!call) return;
    pending.delete(id); clearTimeout(call.timer);
    call.reject(new Error('Cancel was not acknowledged; input unlocked (the harness may still be working).'));
  }, CANCEL_GRACE_MS);
  cancelWatchdog.unref?.();
}
let nextId = 0, session, configOptions = [], busy = false, permission, permissionQueue = [], closing = false, ready = false, imageCapableBoot = false, bootModelValue = model;
// Pi-style steering: a message typed while a turn runs is queued and sent as
// the next prompt when that turn settles, instead of being rejected or dropped.
const steeringQueue = [];
// Set for a few seconds after a close attempt warns about in-flight work, so a
// second Ctrl+C / /exit confirms instead of warning again.
let exitArmed = false;
const safe = value => String(value ?? '').split(process.env.DEEPSEEK_API_KEY || '\0').join('[redacted]').replace(/\x1b(?:\][^\x07]*(?:\x07|$)|\[[0-?]*[ -/]*[@-~])/g, '').replace(/[\x00-\x08\x0b-\x1f\x7f]/g, '');
// ── Presentation layer ─────────────────────────────────────────────────
// Turns ACP blocks into a readable terminal conversation: coloured/structured
// markdown, syntax-highlighted code and diffs, reasoning, tool lifecycles and
// live context usage. Colour is strictly opt-out (NO_COLOR / non-TTY) because
// the same bytes are asserted by tests and replayed from transcripts; every
// renderer must degrade to clean plain text.
// The NO_COLOR convention is presence-based: an empty value still disables
// colour, so only an undefined variable allows it.
const useColor = () => !!process.stdout.isTTY && process.env.NO_COLOR === undefined;
const ANSI = { reset: '\x1b[0m', dim: '\x1b[2m', bold: '\x1b[1m', italic: '\x1b[3m', underline: '\x1b[4m',
  red: '\x1b[31m', green: '\x1b[32m', yellow: '\x1b[33m', blue: '\x1b[34m', magenta: '\x1b[35m', cyan: '\x1b[36m', gray: '\x1b[90m' };
const paint = (text, ...styles) => {
  const value = String(text ?? '');
  if (!useColor() || styles.length === 0) return value;
  return styles.map(style => ANSI[style] || '').join('') + value + ANSI.reset;
};
const dim = text => paint(text, 'dim');
const bold = text => paint(text, 'bold');
// A submitted user turn must never read as just another assistant/tool line in
// the transcript. Render it as a labeled block: a bold-cyan `▸ you` header, then
// each line of the message behind a cyan rail. The plain-text shape
// (`▸ you` / `│ text`) is deliberate: NO_COLOR runs and the byte-asserting tests
// still get a stable, unmistakable marker, and the leading `▸ ` keeps every
// existing "prompt is ready" sentinel in the suite matching.
function renderUserTurn(text) {
  const body = safe(text).replace(/\r\n?/g, '\n').split('\n');
  const rail = paint('│', 'cyan');
  return [paint('▸ you', 'bold', 'cyan'), ...body.map(line => `${rail} ${line}`)].join('\n');
}
const stripAnsi = text => String(text ?? '').replace(/\x1b\[[0-9;]*m/g, '');
const visibleLen = text => stripAnsi(text).length;
// The pane's PTY width can disagree with xterm's real width (only the owner's
// resize drives the PTY), and drawing wider than the terminal wraps the rule and
// the status into rows rawErase does not account for — stale rules then pile up
// as a "staircase" above the input. So ask the terminal for its real width and
// prefer that over the PTY's claim.
let measuredCols = 0;
// Non-moving width probe state. `winSizeAnswered` latches once the terminal has
// answered a CSI 18 t report; `winSizeFallbackUsed` latches the one-time legacy
// caret-moving probe for terminals that never answer.
let winSizeAnswered = false;
let winSizeFallbackUsed = false;
let winSizeFallbackTimer = null;
// The width probe uses CSI 18 t ("report text-area size in characters"), which
// xterm.js answers with `ESC[8;<rows>;<cols>t` WITHOUT moving the caret. The old
// probe walked the cursor to the right margin (`ESC[9999C`) and read it back with
// a cursor-position report: on every keystroke and every mid-turn tick that
// painted the caret in the right margin, which users saw as a random flashing
// typing indicator. A non-moving report removes the flash entirely.
//
// A terminal that does not implement 18 t never answers. xterm.js gates it on
// `windowOptions.getWinSizeChars`, which the pane's Terminal now enables;
// anything older (a stale cached bundle during a rolling update, a bare pipe)
// gets one legacy probe so width correction still works, then stops probing.
function probeWidth() {
  if (!RAW || closing) return;
  try { process.stdout.write('\x1b[18t'); } catch {}
  if (winSizeAnswered || winSizeFallbackUsed) return;
  // The legacy probe moves the caret, so only ever use it on a real TTY and
  // only once. A pipe/headless run (tests, CI) has no terminal to answer, so
  // skip it there rather than emit a caret move no one asked for.
  if (!process.stdout.isTTY) return;
  if (winSizeFallbackTimer) clearTimeout(winSizeFallbackTimer);
  winSizeFallbackTimer = setTimeout(() => {
    winSizeFallbackTimer = null;
    if (winSizeAnswered || winSizeFallbackUsed || closing || !RAW) return;
    winSizeFallbackUsed = true;
    try {
      process.stdout.write('\x1b[9999C\x1b[6n');
      if (raw.drawn) process.stdout.write(`\r\x1b[${raw.cursorCol}C`);
    } catch {}
  }, 350);
  winSizeFallbackTimer.unref?.();
}
// Never draw wider than the PTY either: a probe taken before a resize is stale,
// and drawing at the old, wider size wraps every rule and status line into rows
// rawErase does not account for — the "staircase" of leftover rules. The smaller
// of the probe and the PTY is always safe, and a missing/zero PTY falls back to
// a conservative 80 rather than a width most panes are narrower than.
const termWidth = () => {
  const pty = Number(process.stdout.columns) || 0;
  const measured = measuredCols > 0 ? measuredCols : 0;
  const width = measured && pty ? Math.min(measured, pty) : (measured || pty || 80);
  return Math.max(20, width);
};
// The terminal reflowed the drawn frame to `width`, so the stored row distance
// is stale: re-wrap each drawn row's length to find how many rows now sit above
// the input cell. Shared by SIGWINCH and a measured-width correction.
function recomputeEraseUp(width) {
  if (!raw.drawn || !raw.width || raw.width === width) return;
  let up = Math.floor(raw.cursorCol / width);
  for (const len of raw.above) up += Math.max(1, Math.ceil(len / width));
  raw.up = up;
}
// Tell the pane's web client when the shared PTY is a different width than this
// xterm (only the visible/active device drives the PTY, so a passive or stale
// view can disagree). It answers by re-asserting its measured width — the same
// self-heal a reconnect runs — which is the only way to fix the PTY from here.
let lastWidthNotice = '';
function reportWidthMismatch() {
  if (!RAW || !measuredCols) return;
  const pty = Number(process.stdout.columns) || 0;
  const key = `${pty};${measuredCols}`;
  if (key === lastWidthNotice || !pty || pty === measuredCols) return;
  lastWidthNotice = key;
  try { process.stdout.write(`\x1b]7658;SoluWidth=${pty};${measuredCols}\x07`); } catch {}
}
// Report running work to the pane so its close dialog can warn before killing
// the process out from under a run. Edge-triggered: the count, not a heartbeat.
let lastWorkNotice = -1;
function reportWork() {
  const count = runningWork();
  if (count === lastWorkNotice) return;
  lastWorkNotice = count;
  if (!RAW) return;
  try { process.stdout.write(`\x1b]7658;SoluWork=${count}\x07`); } catch {}
}
const fmtTokens = n => {
  const value = Number(n) || 0;
  if (value >= 1e6) return `${(value / 1e6).toFixed(1)}M`;
  if (value >= 1e4) return `${Math.round(value / 1e3)}k`;
  if (value >= 1e3) return `${(value / 1e3).toFixed(1)}k`;
  return String(value);
};
const indent = (text, prefix) => String(text ?? '').split('\n').map(line => prefix + line).join('\n');

// Wrap plain text (markers included — consumers inline after wrapping) to a
// visible width. Long tokens are hard-split so a 400-char path can't wrap the
// terminal into a wall.
function wrapText(text, width) {
  const limit = Math.max(20, width);
  const out = [];
  for (const paragraph of String(text ?? '').split('\n')) {
    if (!paragraph) { out.push(''); continue; }
    let line = '';
    for (const word of paragraph.split(/(\s+)/)) {
      if (!word) continue;
      if (line && visibleLen(line) + visibleLen(word) > limit) { out.push(line); line = ''; }
      if (visibleLen(word) > limit) {
        let rest = word;
        while (visibleLen(rest) > limit) { out.push(rest.slice(0, limit)); rest = rest.slice(limit); }
        line = rest;
      } else line += word;
    }
    out.push(line);
  }
  return out;
}

const KEYWORDS = {
  js: 'const let var function return if else for while of in new class extends import export from default async await try catch throw typeof instanceof this null undefined true false static get set yield delete void switch case break continue do',
  ts: 'const let var function return if else for while of in new import export from default async await try catch throw typeof instanceof this null undefined true false interface type enum implements public private protected readonly as satisfies keyof infer never unknown any string number boolean void object declare namespace',
  go: 'package import func var const type struct interface map chan go defer return if else for range switch case default break continue select nil true false make new len cap append copy delete panic recover string int int64 float64 bool byte rune error',
  py: 'def class return if elif else for while in import from as try except finally raise with lambda yield global nonlocal pass break continue and or not is None True False async await self',
  sh: 'if then else elif fi for while do done case esac function return export local readonly in set unset echo cd pwd exit',
  rust: 'fn let mut const struct enum impl trait pub use mod return if else match for while loop in as where self Self crate super unsafe async await move ref dyn true false Some None Ok Err',
  json: 'true false null',
  yaml: 'true false null',
  sql: 'select from where insert into values update set delete create table drop alter join left right inner outer on group by order limit offset and or not null as',
};
const LANG_ALIAS = { javascript: 'js', jsx: 'js', mjs: 'js', cjs: 'js', typescript: 'ts', tsx: 'ts', golang: 'go', python: 'py', python3: 'py', bash: 'sh', shell: 'sh', zsh: 'sh', console: 'sh', rs: 'rust', yml: 'yaml' };

function highlightDiff(text) {
  return String(text ?? '').split('\n').map(line => {
    if (/^(diff --git|index |new file|deleted file)/.test(line)) return paint(line, 'bold');
    if (/^(---|\+\+\+) /.test(line)) return paint(line, 'bold');
    if (line.startsWith('@@')) return paint(line, 'cyan');
    if (line.startsWith('+')) return paint(line, 'green');
    if (line.startsWith('-')) return paint(line, 'red');
    return line;
  }).join('\n');
}

// Tiny zero-dependency highlighter. Good enough for the languages an agent
// actually emits; unknown languages pass through untouched rather than risk a
// wrong/mangled render.
function highlightCode(code, lang) {
  const text = String(code ?? '');
  const raw = String(lang || '').toLowerCase().trim();
  const language = LANG_ALIAS[raw] || raw;
  if (language === 'diff' || language === 'patch' || /^(diff --git|@@ )/m.test(text)) return highlightDiff(text);
  if (!useColor()) return text;
  const keywords = new Set(String(KEYWORDS[language] || '').split(/\s+/).filter(Boolean));
  const marker = (language === 'py' || language === 'sh' || language === 'yaml' || language === 'rb') ? '#' : (language === 'sql' ? '--' : '//');
  const comment = (marker === '#' ? '#' : marker === '--' ? '--' : '\\/\\/') + '[^\\n]*';
  const tokenRE = new RegExp(
    `(${comment}|\\/\\*[\\s\\S]*?\\*\\/)` +                                              // 1 comment
    `|("(?:\\\\.|[^"\\\\])*"|'(?:\\\\.|[^'\\\\])*'|\`(?:\\\\.|[^\`\\\\])*\`)` +           // 2 string
    `|(\\b0x[0-9a-fA-F]+\\b|\\b\\d+(?:\\.\\d+)?\\b)` +                                    // 3 number
    `|([A-Za-z_$][\\w$]*)` +                                                              // 4 identifier
    `|([{}()\\[\\];,.:=+\\-*/%<>!&|^~?]+)`,                                               // 5 punctuation
    'g');
  let out = '', last = 0, match;
  while ((match = tokenRE.exec(text))) {
    if (match.index > last) out += text.slice(last, match.index);
    const [token, , str, num, ident] = match;
    if (match[1]) out += paint(token, 'gray', 'italic');
    else if (str) out += paint(token, 'green');
    else if (num) out += paint(token, 'yellow');
    else if (ident && keywords.has(ident)) out += paint(token, 'magenta');
    else if (ident && /^[A-Z]/.test(token)) out += paint(token, 'blue');
    else if (ident && text[match.index + token.length] === '(') out += paint(token, 'cyan');
    else if (match[5]) out += paint(token, 'dim');
    else out += token;
    last = match.index + token.length;
  }
  return out + text.slice(last);
}

function inlineMarkdown(text) {
  let out = String(text ?? '');
  out = out.replace(/`([^`\n]+)`/g, (_, code) => paint(code, 'cyan'));
  out = out.replace(/\*\*([^*\n]+)\*\*/g, (_, body) => paint(body, 'bold'));
  out = out.replace(/(^|[^*])\*([^*\n]+)\*(?!\*)/g, (_, pre, body) => pre + paint(body, 'italic'));
  out = out.replace(/~~([^~\n]+)~~/g, (_, body) => paint(body, 'dim'));
  out = out.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (_, label, url) => `${label} ${paint(url, 'dim', 'underline')}`);
  return out;
}

function renderBlockLine(line, width) {
  if (!line.trim()) return [''];
  const heading = line.match(/^(#{1,6})\s+(.*)$/);
  if (heading) return wrapText(heading[2], width).map(l => paint(l, 'bold', 'cyan'));
  if (/^\s*([-*_])\1{2,}\s*$/.test(line)) return [paint('─'.repeat(Math.min(width, 60)), 'dim')];
  const quote = line.match(/^\s*>\s?(.*)$/);
  if (quote) return wrapText(quote[1], width - 2).map(l => `${paint('│', 'gray')} ${paint(l, 'italic', 'gray')}`);
  const item = line.match(/^(\s*)([-*+]|\d+[.)])\s+(.*)$/);
  if (item) {
    const bullet = /\d/.test(item[2]) ? `${item[2].replace(/[.)]$/, '')}.` : '•';
    const lead = '  '.repeat(Math.floor(item[1].length / 2)) + bullet + ' ';
    return wrapText(item[3], width - lead.length).map((l, i) => (i === 0 ? lead : ' '.repeat(lead.length)) + inlineMarkdown(l));
  }
  if (/^\s*\|.*\|\s*$/.test(line)) {
    const cells = line.trim().replace(/^\||\|$/g, '').split('|').map(cell => cell.trim());
    if (cells.every(cell => /^:?-{2,}:?$/.test(cell))) return [];
    return [inlineMarkdown(cells.join('  │  '))];
  }
  return wrapText(line, width).map(l => inlineMarkdown(l));
}

// Full-text markdown renderer. ACP delivers whole committed blocks (no token
// deltas), so block-level state can be resolved before anything is printed.
function renderMarkdown(text, width = termWidth()) {
  const out = [];
  let fence = null, fenceLang = '', buffer = [];
  for (const raw of String(text ?? '').replace(/\r\n?/g, '\n').split('\n')) {
    const fenceMatch = raw.match(/^\s*```(\S*)\s*$/);
    if (fenceMatch) {
      if (fence === null) { fence = true; fenceLang = fenceMatch[1] || ''; buffer = []; }
      else { out.push(...highlightCode(buffer.join('\n'), fenceLang).split('\n').map(l => '  ' + l)); fence = null; buffer = []; }
      continue;
    }
    if (fence !== null) { buffer.push(raw); continue; }
    out.push(...renderBlockLine(raw, width));
  }
  if (fence !== null) out.push(...highlightCode(buffer.join('\n'), fenceLang).split('\n').map(l => '  ' + l));
  return out.join('\n');
}

function miniDiff(before, after) {
  const lines = [];
  for (const line of String(before ?? '').split('\n')) if (line !== '') lines.push(paint(`- ${line}`, 'red'));
  for (const line of String(after ?? '').split('\n')) if (line !== '') lines.push(paint(`+ ${line}`, 'green'));
  return lines.slice(0, 20).join('\n');
}

// ── Tool lifecycle rendering ───────────────────────────────────────────
const toolCalls = new Map();
// Tool calls DSH has started but not settled. ACP deliberately omits DSH's own
// job/subagent cards, so these in-progress calls are the only running-work
// signal the client has; they drive the live count and the close guard.
const activeTools = new Map();
const TOOL_SETTLED = new Set(['completed', 'failed', 'done', 'cancelled']);
// Detached DSH background jobs. ACP never reports them and the spawning bash
// tool call completes immediately, so activeTools cannot see them — but the
// result text does: "started background job <id>" to start, and job_output /
// job_kill "[status: …]" or a job_list ("<id> [<kind>] <status> — <label>") to
// settle. job_list is authoritative, so a job that finishes without being polled
// cannot leave the badge stuck.
const backgroundJobs = new Map(); // jobId -> { at } (start time for /tasks)
const runningWork = () => activeTools.size + backgroundJobs.size;
function trackJobs(update, text) {
  if (!text) return;
  const started = text.match(/started background job\s+(\S+)/);
  if (started) { backgroundJobs.set(started[1], { at: Date.now() }); return; }
  if (/\(no background jobs\)/.test(text)) { backgroundJobs.clear(); return; }
  const listed = new Map();
  for (const line of text.split('\n')) {
    const row = line.match(/^(\S+)\s+\[[^\]]*\]\s+(\S+)/);
    if (row) listed.set(row[1], row[2]);
  }
  if (listed.size) {
    for (const [id, status] of listed) {
      if (/^(running|pending|starting|queued)$/i.test(status)) { if (!backgroundJobs.has(id)) backgroundJobs.set(id, { at: Date.now() }); }
      else backgroundJobs.delete(id);
    }
    for (const id of [...backgroundJobs.keys()]) if (!listed.has(id)) backgroundJobs.delete(id);
    return;
  }
  const status = text.match(/\[status:\s*([a-z]+)/i);
  if (!status || /^running$/i.test(status[1])) return;
  const args = update?.rawInput ?? toolCalls.get(update.toolCallId)?.args ?? {};
  for (const key of ['job_id', 'jobId', 'id']) {
    if (args[key] && !/^t\d*$/.test(String(args[key]))) backgroundJobs.delete(String(args[key]));
  }
  for (const id of [...backgroundJobs.keys()]) if (text.includes(id)) backgroundJobs.delete(id);
}
function toolGlyph(update) {
  const title = String(update?.title || '').toLowerCase();
  if (/bash|shell|exec|command|terminal/.test(title)) return '$';
  if (/read|cat|view/.test(title)) return 'read';
  if (/write|create|save/.test(title)) return 'write';
  if (/edit|patch|replace|apply/.test(title)) return 'edit';
  if (/grep|search|find/.test(title)) return 'grep';
  if (/glob|list|ls|dir/.test(title)) return 'list';
  if (/web|fetch|http|browse/.test(title)) return 'web';
  if (/task|agent|subagent/.test(title)) return 'agent';
  if (/todo|plan/.test(title)) return 'todo';
  return 'tool';
}
function toolArgsPreview(raw) {
  if (raw == null) return '';
  if (typeof raw === 'string') return safe(raw).slice(0, 400);
  const value = raw.command || raw.cmd || raw.path || raw.file_path || raw.filePath || raw.pattern || raw.query || '';
  if (value !== '') return safe(String(value)).slice(0, 400);
  return safe(JSON.stringify(raw)).slice(0, 400);
}
function renderToolCall(update) {
  const title = safe(update.title || 'tool');
  // Keep rawInput: the matching tool_call_update carries the result text but
  // not the args, and the args are what tie a job_output back to its job id.
  toolCalls.set(update.toolCallId, { title, at: Date.now(), args: update.rawInput });
  // Bound the map: a long session can run thousands of tool calls and nothing
  // else ever removes an entry.
  if (toolCalls.size > 500) toolCalls.delete(toolCalls.keys().next().value);
  emitOutput(`\n${paint('●', 'yellow')} ${paint(toolGlyph(update), 'cyan')} ${bold(title)}\n`);
  const preview = toolArgsPreview(update.rawInput);
  if (preview) emitOutput(indent(dim(preview), '  ') + '\n');
  const args = update.rawInput;
  if (args && typeof args === 'object') {
    if (typeof args.old_str === 'string' || typeof args.new_str === 'string') {
      const diff = miniDiff(args.old_str || '', args.new_str || '');
      if (diff) emitOutput(indent(diff, '  ') + '\n');
    } else if (typeof args.patch === 'string' || typeof args.diff === 'string') {
      emitOutput(indent(highlightDiff(String(args.patch || args.diff)).split('\n').slice(0, 20).join('\n'), '  ') + '\n');
    }
  }
}
// A terminal cannot paint an image, so an image content block is written to
// disk and its path printed instead — the user can open it, and nothing is
// silently reduced to "[image: png]". Best-effort: an unwritable DSH_HOME
// falls back to the old placeholder.
let toolImageSeq = 0;
const TOOL_IMAGE_KEEP = 50;
// Saved tool images are useful only while you might open them; without a bound
// they accumulate for the life of the DSH home. Keep the newest few, drop the
// rest. Best-effort and cheap (a handful of entries after the first prune).
function pruneToolImages() {
  try {
    const dir = path.join(dshHome, 'attachments', 'tool-images');
    const names = fsSync.readdirSync(dir).filter(name => name.startsWith('image-'));
    if (names.length <= TOOL_IMAGE_KEEP) return;
    const byAge = names
      .map(name => ({ name, at: fsSync.statSync(path.join(dir, name)).mtimeMs }))
      .sort((a, b) => b.at - a.at);
    for (const stale of byAge.slice(TOOL_IMAGE_KEEP)) {
      try { fsSync.unlinkSync(path.join(dir, stale.name)); } catch {}
    }
  } catch {}
}
const MAX_TOOL_IMAGE_B64 = 20 * 1024 * 1024;
function saveToolImage(block) {
  const data = typeof block?.data === 'string' ? block.data : '';
  if (!data || data.length > MAX_TOOL_IMAGE_B64) return '';
  const mime = String(block.mimeType || '').toLowerCase();
  const ext = { 'image/png': 'png', 'image/jpeg': 'jpg', 'image/jpg': 'jpg', 'image/webp': 'webp', 'image/gif': 'gif' }[mime] || 'bin';
  try {
    const dir = path.join(dshHome, 'attachments', 'tool-images');
    fsSync.mkdirSync(dir, { recursive: true, mode: 0o700 });
    const file = path.join(dir, `image-${process.pid}-${Date.now()}-${toolImageSeq++}.${ext}`);
    fsSync.writeFileSync(file, Buffer.from(data, 'base64'), { mode: 0o600 });
    pruneToolImages();
    return file;
  } catch { return ''; }
}
function imageRef(block) {
  const saved = saveToolImage(block);
  return saved ? `image saved: ${saved}` : `[image: ${block?.mimeType || 'image'}]`;
}
function toolResultText(update) {
  const parts = [];
  for (const item of update.content || []) {
    const block = item?.content ?? item;
    if (block?.type === 'text' && block.text) parts.push(block.text);
    else if (block?.type === 'image') parts.push(imageRef(block));
  }
  return safe(parts.join('\n')).trim();
}
function renderToolOutput(text, limit = 14) {
  const all = String(text ?? '').split('\n');
  const shown = all.slice(0, limit);
  const looksDiff = /^(diff --git|@@ |\+\+\+ |--- )/m.test(text) || /^[+-][^+-]/m.test(text);
  let body = looksDiff
    ? highlightDiff(shown.join('\n'))
    : shown.map(line => (line.length > 240 ? `${line.slice(0, 240)}…` : line)).join('\n');
  if (all.length > limit) body += `\n${paint(`… ${all.length - limit} more line${all.length - limit === 1 ? '' : 's'}`, 'dim')}`;
  return body;
}
// A terminal cannot un-print output, so "collapse" is a display mode for the
// bodies of tools that finish from here on: the full text still reaches the
// model and the local transcript, and /tools full brings bodies back. The first
// non-empty line is the useful summary in practice; the rest is counted.
let toolOutputCollapsed = false;
function renderToolOutputCollapsed(text) {
  const lines = String(text ?? '').split('\n');
  const first = (lines.find(line => line.trim() !== '') || '').trim();
  const hidden = lines.filter(line => line.trim() !== '').length - 1;
  const head = first.length > 160 ? `${first.slice(0, 160)}…` : first;
  if (!head) return '';
  return head + (hidden > 0 ? dim(` … ${hidden} more line${hidden === 1 ? '' : 's'} · /tools full`) : '');
}
function renderToolResult(update) {
  const meta = toolCalls.get(update.toolCallId) || {};
  const failed = update.status === 'failed';
  const mark = failed ? paint('✖', 'red') : paint('✔', 'green');
  const secs = meta.at ? ` · ${((Date.now() - meta.at) / 1000).toFixed(1)}s` : '';
  emitOutput(`${mark} ${dim(meta.title || 'tool')}${dim(secs)}\n`);
  const text = toolResultText(update);
  trackJobs(update, text);
  if (text) emitOutput(indent(toolOutputCollapsed ? renderToolOutputCollapsed(text) : renderToolOutput(text), '  ') + '\n');
}

// ── Context usage + prompt bar ─────────────────────────────────────────
const usage = { used: 0, size: 0 };
// Context tokens when the current turn started, so the footer can show how much
// this turn added. usage_update carries a cumulative `used`, not a per-turn
// count; the delta is the honest per-turn figure the wire supports.
let turnStartUsed = 0;
// Sum of the positive per-turn context growth across THIS session. `usage_update`
// reports a cumulative context size that can also shrink (compaction), so this is
// honestly "tokens added", not a billed total, which the wire does not carry.
let sessionTokens = 0;
function contextLabel() {
  if (!usage.size) return '';
  const pct = Math.min(999, Math.round((usage.used / usage.size) * 100));
  const tone = pct >= 85 ? 'red' : pct >= 60 ? 'yellow' : 'green';
  return `ctx ${paint(`${pct}%`, tone)} ${dim(`(${fmtTokens(usage.used)}/${fmtTokens(usage.size)})`)}`;
}
function configLabel(category) {
  const option = configOptions.find(row => row.category === category || row.id === category);
  if (!option) return '';
  const values = (option.options ?? []).flatMap(row => row.options ?? [row]);
  const current = values.find(row => row.value === option.currentValue);
  return safe(current?.name || option.currentValue || '');
}
// ACP advertises image support once, for the model the session booted with. A
// `/model` switch to any other route is treated as text-only — pasted images
// degrade to paths — until per-model capability is observable on the wire.
const currentModelValue = () => {
  const option = configOptions.find(row => row.category === 'model' || row.id === 'model');
  return option?.currentValue || model;
};
// The model id inside the wire value (see decodeDshModelValue).
const currentModelId = () => decodeDshModelValue(currentModelValue()).model;
// DSH owns the live session after boot and can advertise model switching. The
// boot patch alone is therefore not a sufficient spend boundary: recover a
// stale live selection to Flash before every prompt, and fail locally if the
// server reports an unknown model or refuses the recovery update.
async function ensureCurrentDeepseekModel() {
  const wire = decodeDshModelValue(currentModelValue());
  const canonical = canonicalDeepseekModel(wire.model);
  if (wire.model !== canonical) {
    const option = configOptions.find(row => row.category === 'model' || row.id === 'model');
    if (!option) throw new Error(`Unsupported live DeepSeek model: ${wire.model}`);
    const value = encodeDshModelValue(canonical, wire.pair);
    const result = await request('session/set_config_option', { sessionId: session, configId: option.id, value });
    configOptions = result.configOptions ?? configOptions.map(row => row === option ? { ...row, currentValue: value } : row);
  }
  return validateDeepseekSelector(currentModelId());
}
const imageCapableNow = () => imageCapableBoot && currentModelValue() === bootModelValue;
function promptText() {
  const parts = [configLabel('model') || model, configLabel('reasoning_effort'), contextLabel(), runningWork() ? `⚙ ${runningWork()} task${runningWork() === 1 ? '' : 's'}` : '', steeringQueue.length ? `⇢ ${steeringQueue.length} queued` : ''].filter(Boolean);
  const head = parts.length ? dim(parts.join(' · ')) + ' ' : '';
  return `${head}${paint('▸', 'cyan')} `;
}
function turnHeader() {
  const parts = [configLabel('model') || model, configLabel('reasoning_effort')].filter(Boolean);
  return dim(`── ${parts.join(' · ')} ──`);
}
function turnFooter(stopReason, secs) {
  const parts = [paint(String(stopReason || 'done'), 'green'), secs];
  if (usage.size) {
    const grew = usage.used - turnStartUsed;
    if (grew > 0) sessionTokens += grew;
    parts.push(stripAnsi(contextLabel()) + (grew > 0 ? ` ${dim(`+${fmtTokens(grew)}`)}` : ''));
  }
  return `${paint('✓', 'green')} ${parts.join(' · ')}`;
}
// Claude-Code-style task view: the composer only shows a count, so /tasks lists
// each detached background job with its age, which is what makes a long-running
// publish/test visible instead of a bare "⚙ 1 task".
function tasksReport() {
  const now = Date.now();
  const rows = [...backgroundJobs.entries()].map(([id, meta]) => {
    const secs = Math.max(0, Math.round((now - (meta?.at || now)) / 1000));
    return `  ${paint('●', 'yellow')} ${bold(id)} ${dim(`· running · ${secs}s`)}`;
  });
  const inFlight = activeTools.size;
  const head = `${bold('Background tasks')} ${dim(`(${backgroundJobs.size})`)}`;
  if (rows.length === 0) {
    return inFlight
      ? `No detached background jobs; ${inFlight} tool call${inFlight === 1 ? '' : 's'} in flight.`
      : 'No background tasks running.';
  }
  return `${head}\n${rows.join('\n')}\n${dim('Read one with job_output, or stop it with job_kill.')}`;
}
// The daemon stores imported per-key spend beside the accounts
// (dataDir/harness-accounts/<provider>-usage.json). Read this account's total
// directly so /status can show it without a daemon round-trip. Read-only and
// best-effort: a missing or malformed ledger simply shows nothing.
function localSpendLabel() {
  const account = process.env.SOLU_HARNESS_ACCOUNT;
  if (!account) return '';
  try {
    const dataDir = process.env.SOLU_HARNESS_DATA_DIR || path.dirname(path.dirname(dshHome));
    const raw = fsSync.readFileSync(path.join(dataDir, 'harness-accounts', 'deepseek-usage.json'), 'utf8');
    if (raw.length > (4 << 20)) return '';
    const state = JSON.parse(raw);
    let cost = 0, seen = false, currency = 'CNY';
    for (const entry of Object.values(state.imports ?? {})) {
      if (entry && typeof entry.currency === 'string' && entry.currency) currency = entry.currency;
      const usage = entry && entry.keys ? entry.keys[account] : undefined;
      if (usage && Number.isFinite(usage.cost)) { cost += usage.cost; seen = true; }
    }
    if (!seen) return '';
    const digits = Math.abs(cost) > 0 && Math.abs(cost) < 1 ? 4 : 2;
    return `${currency} ${cost.toFixed(digits)} imported · /clear keeps the ledger`;
  } catch { return ''; }
}
function statusReport() {
  return [
    `${bold('model')}     ${configLabel('model') || model}`,
    `${bold('effort')}    ${configLabel('reasoning_effort') || dim('default')}`,
    `${bold('client')}    ${clientVersion}${dshVersion ? ` ${dim(`· DSH ${dshVersion}`)}` : ''}`,
    `${bold('session')}   ${safe(session)}`,
    `${bold('cwd')}       ${process.cwd()}`,
    `${bold('context')}   ${usage.size ? contextLabel() : dim('unknown (harness has not reported usage)')}`,
    `${bold('tokens')}    ${usage.size ? `session +${fmtTokens(sessionTokens)} added · last turn +${fmtTokens(Math.max(0, usage.used - turnStartUsed))}` : dim('not reported yet')}`,
    `${bold('spend')}     ${localSpendLabel() || dim('no imported usage for this account')}`,
    `${bold('images')}    ${imageCapableNow() ? 'accepted' : dim('path-only — this model/harness does not accept image input')}`,
    `${bold('thinking')}  ${thinkingMode}`,
    `${bold('tasks')}     ${runningWork() ? `${runningWork()} running${backgroundJobs.size ? ` — bg: ${[...backgroundJobs.keys()].join(', ')} · /tasks for detail` : ''} — Ctrl+C or /exit warns before closing` : dim('none')}`,
    `${bold('queued')}    ${steeringQueue.length ? `${steeringQueue.length} message${steeringQueue.length === 1 ? '' : 's'} — sent after the running turn` : dim('none')}`,
  ].join('\n');
}
// Reasoning display. A terminal cannot re-collapse already-printed text, so
// this is a forward-looking mode: summary (a few lines), full, or off. The
// live status line still shows a "thinking" spinner regardless of the mode.
const THINKING_MODES = ['summary', 'full', 'off'];
let thinkingMode = 'summary';
const thinkingOn = () => thinkingMode !== 'off';
function renderThought(text) {
  const clean = safe(text).trim();
  if (!clean || !thinkingOn()) return;
  const lines = clean.split('\n');
  const limit = thinkingMode === 'summary' ? 6 : lines.length;
  emitOutput(`\n${paint('∴', 'gray')} ${paint('thinking', 'gray', 'italic')}\n`);
  emitOutput(indent(lines.slice(0, limit).map(line => paint(line, 'gray', 'italic')).join('\n'), '  ') + '\n');
  if (lines.length > limit) emitOutput(indent(paint(`… ${lines.length - limit} more · /thinking for full`, 'dim'), '  ') + '\n');
}
const write = message => { if (!child.stdin.destroyed) child.stdin.write(JSON.stringify({ jsonrpc: '2.0', ...message }) + '\n'); };
// A dead harness makes later writes EPIPE. Without this listener the stream
// error is unhandled and the client crashes instead of exiting cleanly.
child.stdin.on('error', error => { if (error?.code !== 'EPIPE') console.error(safe(error?.message)); void shutdown(1); });
function request(method, params, timeout = 30000) {
  const id = ++nextId;
  if (method === 'session/prompt') promptRequest = id;
  return new Promise((resolve, reject) => {
    const timer = timeout ? setTimeout(() => { pending.delete(id); if (method === 'session/prompt') promptRequest = null; reject(new Error(`${method} timed out`)); }, timeout) : null;
    pending.set(id, { resolve, reject, timer }); write({ id, method, params });
  });
}
// The ACP SDK collapses an unknown server failure to the literal message
// "Internal error" and parks the real cause in `data`. Surface it: DSH answers
// exactly that when a session is already owned by another live client (the
// session's flock is held), which is the difference between "retry after the
// old client exits" and a permanent-looking "Internal error". Prefer the SDK's
// own `data.details`, then a data string, then the raw data; never duplicate a
// detail already present in the message.
function requestErrorMessage(error) {
  const base = safe(error?.message || 'request failed');
  const data = error?.data;
  let detail = '';
  if (typeof data === 'string') detail = data;
  else if (data && typeof data === 'object') {
    if (typeof data.details === 'string') detail = data.details;
    else if (typeof data.message === 'string') detail = data.message;
    else { try { detail = JSON.stringify(data); } catch { /* non-serializable */ } }
  }
  detail = safe(String(detail || '')).trim();
  if (!detail || detail === base || base.includes(detail)) return base;
  return `${base}: ${detail}`;
}
// Working indicator: quiet heartbeat lines on stderr (dimmed on TTY), so the
// conversation stream on stdout is never corrupted. A carriage-return spinner
// was tried here and removed: SoluTerminal panes do not rewrite \r lines, so
// every spinner frame leaked into the transcript as a permanent line.
const status = { active: false, t0: 0, timer: null, phase: '', tool: '', chars: 0, firstChunk: false, slowNotified: false };
let turnText = '';
const statusElapsed = () => (Date.now() - status.t0) / 1000;
const statusSecs = () => `${statusElapsed().toFixed(1)}s`;
function statusLine() {
  const base = status.phase === 'approval' ? 'waiting for approval'
    : !status.firstChunk ? `working${status.tool ? ` · ${status.tool}` : ''}`
    : `receiving${status.tool ? ` · ${status.tool}` : ''} · ${status.chars} chars`;
  return `… ${base} · ${statusSecs()} · Ctrl+C cancels`;
}
// Live status line (real terminal only): one line pinned to the bottom. It is
// clipped to the pane width and erased with a relative `\r\x1b[J`, never
// DECSC/DECRC, so a wrap at the bottom edge cannot scroll the saved row out
// from under it.
const SPINNER = ['⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'];
// The composer is the single bottom UI while RAW — idle and mid-turn. `live`
// only animates the spinner in the composer's status row; there is no separate
// live line any more, so there is exactly one writer for the bottom rows.
const live = { active: false, frame: 0, timer: null };
// Busy status for the composer's status row. The draft itself lives on the
// input row (always visible), so this is only the spinner and turn progress.
function liveText() {
  const frame = SPINNER[live.frame % SPINNER.length];
  const running = activeTools.size;
  const base = status.phase === 'approval' ? 'waiting for approval'
    : running > 1 ? `running ${running} tools`
    : status.tool ? `running ${status.tool}`
    : status.firstChunk ? `receiving · ${status.chars} chars`
    : 'thinking';
  const width = Math.max(20, termWidth());
  const bg = backgroundJobs.size ? ` · ${backgroundJobs.size} bg` : '';
  let head = `${frame} ${base}`;
  let tail = ` · ${statusSecs()} · type to queue · Ctrl+C cancels${bg}`;
  if (head.length + tail.length > width) {
    if (head.length > width - 1) {
      head = head.slice(0, Math.max(0, width - 2)) + '…';
      tail = '';
    } else {
      tail = tail.slice(0, Math.max(0, width - head.length - 1)) + '…';
    }
  }
  // Highlighted, not dim: during a long tool call this is the only sign the
  // turn is alive, and a dim line reads like finished output.
  return `${paint(head, 'yellow')}${dim(tail)}`;
}
// A spinner tick repaints the composer. rawDraw emits the erase + whole frame
// in one write, so the 120 ms cadence cannot flash an intermediate frame.
function liveRender() {
  if (!RAW || !live.active || permission) return;
  rawDraw();
}
function liveStart() {
  if (!RAW) return;
  live.active = true; live.frame = 0;
  if (live.timer) clearInterval(live.timer);
  live.timer = setInterval(() => { live.frame += 1; liveRender(); }, 120); live.timer.unref?.();
  rawDraw();
}
function liveStop() {
  if (live.timer) clearInterval(live.timer);
  live.timer = null; live.active = false;
}
// Every stdout/stderr write goes through here so the bottom composer is erased
// first, the output lands, and the composer is repainted underneath.
//
// This is the fix for the "after a long reply the text input area is cut off or
// gone" report. The composer is drawn against the bottom of the pane with the
// cursor parked on its input row, so any output that bypasses this — a late
// usage/tool update, a queued-message notice, command output — is written
// *into* the input row and pushes the rules and status away, leaving a lone
// cursor where the composer was. The composer is the ONLY bottom writer now
// (idle or mid-turn), so erasing + repainting it here covers every case.
function emitOutput(text, stream = process.stdout) {
  const hadComposer = RAW && raw.drawn;
  if (hadComposer) rawErase();
  stream.write(text);
  if (hadComposer) rawDraw();
}
function statusHeartbeat() {
  // The approval bar is drawn while we wait for a decision; a stderr heartbeat
  // would be written straight through it and corrupt the composer.
  if (!status.active || permission) return;
  status.slowNotified = true;
  try { process.stderr.write(dim(statusLine()) + '\n'); } catch {}
}
function statusStopSilent() {
  if (status.timer) { clearTimeout(status.timer); clearInterval(status.timer); }
  status.timer = null;
}
function statusStart() {
  statusStopSilent();
  Object.assign(status, { active: true, t0: Date.now(), phase: 'working', tool: '', chars: 0, firstChunk: false, slowNotified: false });
  if (RAW) { liveStart(); return; }
  // First heartbeat after 1s for fast feedback, then one every 15s. Streamed
  // content itself shows progress in between; silence is intentional.
  status.timer = setTimeout(function tick() {
    if (!status.active) return;
    statusHeartbeat();
    status.timer = setInterval(statusHeartbeat, 15000); status.timer.unref?.();
  }, 1000);
  status.timer.unref?.();
}
function statusChunk(length) {
  if (!status.active) return;
  status.firstChunk = true; status.chars += length;
}
function statusTool(title) {
  if (!status.active) return;
  status.tool = safe(title).slice(0, 40);
}
function statusApproval(waiting) {
  if (!status.active) return;
  status.phase = waiting ? 'approval' : (status.firstChunk ? 'receiving' : 'working');
}
function statusStop(outcome) {
  const secs = status.active ? statusSecs() : '0.0s';
  const wasSlow = status.slowNotified;
  statusStopSilent();
  status.active = false;
  if (RAW) { liveStop(); return secs; }
  try {
    if (wasSlow) process.stderr.write(dim(`… ${outcome} in ${secs}`) + '\n');
  } catch {}
  return secs;
}
// Approvals render as a focused numbered menu — the exact shape SoluTerminal's
// per-pane auto-approval (ARIA AutoMode) already detects for Claude and Codex.
// Bare Enter selects the focused option 1 (allow), matching those harnesses;
// 2/n/no and every unrecognized answer reject. The RAW composer adds the TUI
// feel on top: ↑/↓ move the highlight and Enter confirms it, while 1/2/3 (or
// y/n/a) answer in one keypress. Do not add an "enter to select" footer or an
// "↑/↓ to navigate" hint: either string marks a generic non-approval menu and
// makes the daemon deliberately refuse to auto-answer.
function renderPermission(request) {
  const title = safe(request?.params?.toolCall?.title ?? 'tool');
  console.log(`\n${bold('Tool approval:')} ${title}`);
  const raw = request?.params?.toolCall?.rawInput;
  if (raw != null) console.log(dim(safe(typeof raw === 'string' ? raw : JSON.stringify(raw, null, 2)).slice(0, 2000)));
  console.log('Do you want to proceed?');
  permissionChoices(request).forEach((choice, index) => {
    console.log(`${index === 0 ? bold(`❯ ${choice.answer}.`) : `  ${choice.answer}.`} ${safe(choice.label)}`);
  });
}
// DSH can have more than one tool waiting for approval. Show one menu at a
// time and queue the rest: answering a second request cancelled would deny a
// tool the user never saw.
function showPermission(message) {
  permission = message;
  // Every menu opens with option 1 (allow) focused, which is also what the
  // daemon's auto-approval assumes when it writes a bare Enter.
  raw.permSel = 0;
  statusApproval(true);
  liveStop();
  renderPermission(message);
  prompt();
}
// After one approval is answered, present the next queued one, or resume the
// turn's live line when the queue is empty.
function advancePermission() {
  const next = permissionQueue.shift();
  if (next) { showPermission(next); return; }
  if (status.active) liveStart();
  else rawDraw();
}
// ── Raw-mode composer ──────────────────────────────────────────────────
// A minimal pinned input bar: one or more input lines with a status line
// beneath, redrawn in place. It owns stdin only when RAW, so the readline path
// (tests, pipes) is untouched.
const raw = { text: '', cursor: 0, index: -1, draft: '', drawn: false, up: 0, pasting: false, width: 0, above: [], cursorCol: 0, sel: 0, permSel: 0 };
let rawBound = false;
function rawPrefix() {
  if (!permission) return '▸ ';
  return canAlwaysAllow(permissionTool(permission).key) ? 'Select [1/2/3]: ' : 'Select [1/2]: ';
}
function rawStatus() {
  if (permission) {
    // Same one-row invariant as the idle status: the composer's cursor math
    // breaks if this wraps, so shrink it rather than let it. Never render the
    // "enter to select" / "↑/↓ to navigate" strings here: the daemon treats
    // those as a generic (non-approval) menu and refuses to auto-answer.
    const width = Math.max(20, termWidth());
    const choices = permissionChoices(permission);
    const pick = choices[Math.min(Math.max(raw.permSel, 0), choices.length - 1)];
    const hotkeys = choices.length > 2 ? '1/2/3' : '1/2';
    let text = `[${pick.answer}] ${pick.label} · ↑/↓ choose · Enter · ${hotkeys}`;
    if (text.length > width) text = `[${pick.answer}] ${pick.label} · ↑/↓ · Enter`;
    if (text.length > width) text = `[${pick.answer}] ${pick.label}`;
    if (text.length > width) text = text.slice(0, Math.max(0, width - 1)) + '…';
    return dim(text);
  }
  if (status.active) {
    // Mid-turn: the spinner/progress replaces the idle hints. The input row
    // above stays visible, so the draft (and the "type to queue" hint) is always
    // on screen instead of hidden behind a live line.
    return liveText();
  }
  const width = Math.max(20, termWidth());
  const parts = [configLabel('model') || model, configLabel('reasoning_effort'), usage.size ? stripAnsi(contextLabel()) : '', runningWork() ? `⚙ ${runningWork()} task${runningWork() === 1 ? '' : 's'}` : '', steeringQueue.length ? `⇢ ${steeringQueue.length} queued` : ''].filter(Boolean);
  const keys = 'Enter send · Ctrl+J newline · Tab complete · /help';
  // Exactly one row: the composer's cursor math assumes the status cannot wrap,
  // so drop the key hints first and hard-clip anything still too long.
  let text = [...parts, keys].join(' · ');
  if (text.length > width) text = parts.join(' · ');
  if (text.length > width) text = text.slice(0, Math.max(0, width - 1)) + '…';
  return dim(text);
}
function rawSuggestions() {
  if (!RAW || permission) return [];
  if (!raw.text.startsWith('/') || /\s/.test(raw.text)) return [];
  return commandHits(raw.text).slice(0, 5);
}
// Move from the parked input cell back to the composer's first row and clear
// down. Deliberately NOT DECSC/DECRC: the composer is drawn against the bottom
// of the pane, so drawing it scrolls the screen, and a saved absolute row is
// not adjusted for that scroll. Restoring it landed below the real top, so
// every keystroke left the previous frame behind — the duplicated input row.
// The cursor is always parked on the input row after a draw, so a relative
// move is scroll-proof.
function rawErase() {
  if (!raw.drawn) return;
  process.stdout.write(`\r${raw.up > 0 ? `\x1b[${raw.up}A` : ''}\x1b[J`);
  raw.drawn = false;
  raw.up = 0;
}
// A pane narrowed past the width its last frame was drawn at: every row wider
// than the new terminal hard-wrapped, and each repaint at the stale width left
// another wrapped rule behind, so the stored row distance only describes the
// last frame. Clear the viewport (scrollback keeps the transcript) and repaint
// the composer from home, which is exact regardless of how much residue had
// piled up. Called only on a shrink, so a normal SIGWINCH resize keeps its
// in-place repaint.
function resetComposerAfterShrink() {
  rawErase();
  process.stdout.write('\x1b[2J\x1b[H');
  rawDraw();
}
// Wrap-aware, bounded layout: long lines and multi-line input both render
// correctly, and the window follows the cursor so a big paste cannot scroll
// the terminal out from under the saved composer origin.
const RAW_MAX_ROWS = 8;
function rawLayout() {
  const width = Math.max(30, termWidth());
  const prefix = rawPrefix();
  const cont = ' '.repeat(prefix.length);
  const inputWidth = Math.max(10, width - prefix.length);
  const lines = raw.text.split('\n');
  const before = raw.text.slice(0, raw.cursor).split('\n');
  const cursorLine = before.length - 1;
  const wrappedBefore = wrapText(before[before.length - 1], inputWidth);
  const rows = [];
  let cursorRow = 0, cursorCol = prefix.length;
  lines.forEach((line, index) => {
    const wrapped = wrapText(line, inputWidth);
    wrapped.forEach((row, rowIndex) => {
      rows.push((index === 0 && rowIndex === 0 ? prefix : cont) + row);
      if (index === cursorLine && rowIndex === Math.min(wrappedBefore.length - 1, wrapped.length - 1)) {
        cursorRow = rows.length - 1;
        cursorCol = prefix.length + visibleLen(wrappedBefore[wrappedBefore.length - 1] || '');
      }
    });
  });
  if (rows.length === 0) rows.push(prefix);
  let offset = 0;
  if (rows.length > RAW_MAX_ROWS) {
    offset = Math.min(Math.max(0, cursorRow - Math.floor(RAW_MAX_ROWS / 2)), rows.length - RAW_MAX_ROWS);
    cursorRow -= offset;
  }
  const shown = rows.slice(offset, offset + RAW_MAX_ROWS);
  const hiddenAbove = offset > 0;
  const hiddenBelow = offset + shown.length < rows.length;
  if (hiddenAbove) shown.unshift(paint(`↑ ${offset} more`, 'dim'));
  if (hiddenBelow) shown.push(paint(`↓ ${rows.length - offset - shown.length} more`, 'dim'));
  return { shown, cursorRow: cursorRow + (hiddenAbove ? 1 : 0), cursorCol };
}
function rawDraw() {
  if (closing || !RAW) return;
  // Erase the previous frame and repaint in ONE write. Several writes let the
  // pane's xterm render an intermediate (erased/partial) frame, which reads as
  // a flicker on every keystroke. Inline rawErase's escape so the whole frame
  // is a single chunk.
  const erase = raw.drawn ? `\r${raw.up > 0 ? `\x1b[${raw.up}A` : ''}\x1b[J` : '';
  raw.drawn = false;
  raw.up = 0;
  // rawErase only rewinds when a frame was already on screen. When it was not
  // (the first draw, or the first after a turn), a width probe may have left
  // the cursor in the right margin, so start the frame at column 0.
  const out = [erase, '\r'];
  const suggestions = rawSuggestions();
  if (raw.sel >= suggestions.length) raw.sel = Math.max(0, suggestions.length - 1);
  const width = Math.max(20, termWidth());
  const suggestionRows = suggestions.map((row, index) => {
    const lead = index === raw.sel ? '❯ ' : '  ';
    const pad = ' '.repeat(Math.max(1, 20 - row.name.length - row.args.length));
    const plain = `${lead}${row.name}${row.args ? ` ${row.args}` : ''}${pad}${row.desc}`;
    const clipped = plain.length > width ? plain.slice(0, Math.max(0, width - 1)) + '…' : plain;
    return clipped.replace(`${lead}${row.name}`, `${lead}${paint(row.name, 'cyan')}`);
  });
  const layout = rawLayout();
  const ruleWidth = Math.max(10, termWidth());
  // suggestions + top rule + input rows + bottom rule + status. The two rules
  // bracket the input into an always-on frame so the text area reads as a
  // distinct control, not transcript output.
  const total = suggestionRows.length + 2 + layout.shown.length + 1;
  // Ghost placeholder on the empty input row, so the text area is unmistakable
  // at a glance (Claude Code and Codex both draw a prompt with a hint). It is
  // rendered outside raw.text — it can never be submitted — and the caret still
  // parks on the real prompt column.
  const inputRows = layout.shown.slice();
  if (!permission && raw.text === '' && layout.cursorRow >= 0 && layout.cursorRow < inputRows.length) {
    const room = Math.max(1, width - rawPrefix().length);
    const hint = 'Type a message…  / for commands';
    inputRows[layout.cursorRow] = rawPrefix() + dim(hint.length > room ? hint.slice(0, Math.max(0, room - 1)) + '…' : hint);
  }
  if (suggestionRows.length) out.push(suggestionRows.join('\n') + '\n');
  out.push(paint('─'.repeat(ruleWidth), 'dim') + '\n');
  out.push(inputRows.join('\n') + '\n');
  out.push(paint('─'.repeat(ruleWidth), 'dim') + '\n');
  out.push(rawStatus());
  const targetRow = suggestionRows.length + 1 + layout.cursorRow;
  const up = (total - 1) - targetRow;
  out.push(`\r${up > 0 ? `\x1b[${up}A` : ''}${layout.cursorCol > 0 ? `\x1b[${layout.cursorCol}C` : ''}`);
  process.stdout.write(out.join(''));
  // Park the input cell and remember how far below the top it sits, so the
  // next rawErase() can walk back with a relative move.
  raw.drawn = true;
  raw.up = targetRow;
  // Also remember the frame's shape. On a resize the terminal reflows these
  // hard lines at the new width, so the row distance above the input cell
  // changes; SIGWINCH recomputes raw.up from these lengths before erasing.
  raw.width = termWidth();
  raw.cursorCol = layout.cursorCol;
  raw.above = [
    ...suggestionRows.map(visibleLen),
    ruleWidth,
    ...layout.shown.slice(0, layout.cursorRow).map(visibleLen),
  ];
}
const RAW_MAX_INPUT = 1 << 20; // 1 MiB: a paste without its terminator cannot grow forever.
function rawInsert(value) {
  let insert = String(value);
  const room = RAW_MAX_INPUT - raw.text.length;
  if (room <= 0) return;
  if (insert.length > room) insert = insert.slice(0, room);
  raw.text = raw.text.slice(0, raw.cursor) + insert + raw.text.slice(raw.cursor);
  raw.cursor += insert.length;
  raw.sel = 0;
}
function rawBackspace() {
  if (raw.cursor === 0) return;
  raw.text = raw.text.slice(0, raw.cursor - 1) + raw.text.slice(raw.cursor);
  raw.cursor -= 1;
  raw.sel = 0;
}
function rawDelete() {
  if (raw.cursor >= raw.text.length) return;
  raw.text = raw.text.slice(0, raw.cursor) + raw.text.slice(raw.cursor + 1);
  raw.sel = 0;
}
function rawDeleteWord() {
  const before = raw.text.slice(0, raw.cursor);
  const kept = before.replace(/\S+\s*$/, '');
  raw.text = kept + raw.text.slice(before.length);
  raw.cursor = kept.length;
  raw.sel = 0;
}
function rawHistory(direction) {
  if (raw.text.includes('\n')) {
    const before = raw.text.slice(0, raw.cursor).split('\n');
    if (direction < 0 && before.length > 1) raw.cursor -= before[before.length - 1].length + 1;
    else if (direction > 0) { const end = raw.text.slice(raw.cursor).indexOf('\n'); if (end >= 0) raw.cursor += end + 1; }
    return;
  }
  if (history.length === 0) return;
  if (raw.index === -1) raw.draft = raw.text;
  let next = raw.index + (direction < 0 ? 1 : -1);
  if (next < -1) next = -1;
  if (next > history.length - 1) next = history.length - 1;
  raw.index = next;
  raw.text = next === -1 ? raw.draft : history[next];
  raw.cursor = raw.text.length;
}
function clearScreen() {
  if (RAW) rawErase();
  process.stdout.write('\x1b[2J\x1b[3J\x1b[H');
}
function rawComplete() {
  const before = raw.text.slice(0, raw.cursor);
  const tokenStart = Math.max(before.lastIndexOf(' '), before.lastIndexOf('\n'), before.lastIndexOf('\t')) + 1;
  const token = before.slice(tokenStart);
  // `@`-prefixed tokens complete against the filesystem (Pi's @file flow).
  if (token.startsWith('@') && token.length > 1) {
    const partial = token.slice(1);
    const dir = path.dirname(partial);
    const base = path.basename(partial);
    let entries;
    try { entries = fsSync.readdirSync(path.resolve(process.cwd(), dir === '.' ? '' : dir), { withFileTypes: true }); }
    catch { return; }
    const matches = entries.map(entry => entry.name + (entry.isDirectory() ? '/' : '')).filter(name => name.startsWith(base));
    if (matches.length === 0) return;
    let common = matches[0];
    for (const name of matches) { while (common && !name.startsWith(common)) common = common.slice(0, -1); }
    const next = '@' + (dir === '.' ? '' : dir + '/') + common;
    raw.text = raw.text.slice(0, tokenStart) + next + raw.text.slice(raw.cursor);
    raw.cursor = tokenStart + next.length;
    raw.sel = 0;
    rawDraw();
    return;
  }
  if (/\s/.test(before)) return; // otherwise only complete the command token
  const hits = commandHits(before);
  if (hits.length === 0) return;
  const pick = hits[Math.min(Math.max(raw.sel, 0), hits.length - 1)];
  const next = pick.name + (pick.args ? ' ' : '');
  raw.text = next + raw.text.slice(before.length);
  raw.cursor = next.length;
  raw.sel = 0;
}
// Answer the active permission prompt and advance to the next queued one. The
// echoed line mirrors what typed input used to print so the transcript keeps a
// record of the answer; rawErase first stops emitOutput from repainting the
// old menu frame before submit() swaps in the next prompt / live line.
function permissionFire(answer) {
  if (!permission) return;
  const prefix = rawPrefix();
  raw.text = ''; raw.cursor = 0; raw.index = -1; raw.draft = '';
  rawErase();
  emitOutput(`${prefix}${answer}\n`);
  void submit(answer);
}
function rawSubmit() {
  if (permission) {
    // Enter confirms the highlighted row (default option 1), which is what the
    // daemon's auto-approval writes: a bare \r must still allow option 1.
    const choices = permissionChoices(permission);
    const pick = choices[Math.min(Math.max(raw.permSel, 0), choices.length - 1)];
    permissionFire(pick.answer);
    return;
  }
  if (busy && !permission) {
    // Pi-style steering: Enter during a turn queues the message, and it is sent
    // as the next prompt when this one settles. Ctrl+C still cancels.
    const queued = raw.text.trim();
    if (queued) {
      raw.text = ''; raw.cursor = 0; raw.index = -1; raw.draft = '';
      steeringQueue.push({ text: queued, resolveRefs: true });
      // During a turn the composer is intentionally hidden behind the live line;
      // emitOutput repaints that line. Drawing the composer here parked the
      // cursor mid-turn, so the rest of the stream overwrote it and the
      // turn-end erase used a stale row distance.
      emitOutput(`\n${dim(`⇢ queued (${steeringQueue.length}): ${safe(queued).slice(0, 80)}`)}\n`);
    } else {
      emitOutput(`\n${dim('⏳ turn still running — type a message to queue it, or Ctrl+C to cancel')}\n`);
    }
    return;
  }
  // With the "/" suggestion list up, Enter accepts the highlighted command
  // rather than submitting a half-typed one ("/ses" -> "Unknown command").
  const hits = rawSuggestions();
  if (hits.length) {
    const pick = hits[Math.min(Math.max(raw.sel, 0), hits.length - 1)];
    if (raw.text.trim() !== pick.name) { rawComplete(); rawDraw(); return; }
  }
  const text = raw.text;
  raw.text = ''; raw.cursor = 0; raw.index = -1; raw.draft = '';
  rawErase();
  if (text.trim() !== '') {
    // Keep secrets out of the on-disk history and the echo, matching the
    // transcript: a key pasted into a prompt must not return on up-arrow.
    const safeText = safe(text);
    history.unshift(safeText); if (history.length > HISTORY_LIMIT) history.pop();
    // A blank line sets the user's turn apart from the preceding reply; the
    // labeled `▸ you` / `│ …` block marks it as the human's input at a glance.
    emitOutput(`\n${renderUserTurn(safeText)}\n`);
  }
  void submit(text);
}
function rawInterrupt() {
  if (activeShell) { // a `!command` is running: Ctrl+C kills the local command
    try { activeShell.kill('SIGKILL'); } catch {}
    emitOutput(`\n${dim('⚠ cancelled local command')}\n`);
    return;
  }
  if (busy && session) {
    const label = status.tool || (activeTools.size > 1 ? `${activeTools.size} tasks` : 'turn');
    emitOutput(`\n${dim(`⚠ cancelling ${safe(label)}…`)}\n`);
    rejectPermission();
    write({ method: 'session/cancel', params: { sessionId: session } });
    armCancelWatchdog();
    return;
  }
  if (raw.text !== '') { raw.text = ''; raw.cursor = 0; rawDraw(); return; }
  // Ctrl+C closes only after warning about work still in flight; a second
  // press within the window confirms, matching the "don't lose my task" ask.
  if (runningWork() > 0 && !exitArmed) {
    exitArmed = true;
    const timer = setTimeout(() => { exitArmed = false; }, 5000); timer.unref?.();
    rawErase();
    process.stdout.write(`\n${paint(`⚠ ${runningWork()} task${runningWork() === 1 ? '' : 's'} still running — Ctrl+C again to close.`, 'yellow')}\n`);
    rawDraw();
    return;
  }
  void shutdown(0);
}
// Cursor-position reports (ESC[row;colR) and text-area size reports
// (ESC[8;rows;cols t) are answers to probeWidth, not user input. Strip them
// (buffering a split tail) so their bytes never get typed into the composer.
let cprTail = '';
// Apply one measured terminal width. `from18t` records that the non-moving
// report actually works, which cancels the one-shot legacy fallback.
function applyMeasuredWidth(cols, from18t) {
  if (!(cols > 0)) return;
  if (from18t) {
    winSizeAnswered = true;
    if (winSizeFallbackTimer) { clearTimeout(winSizeFallbackTimer); winSizeFallbackTimer = null; }
  }
  if (cols === measuredCols) { reportWidthMismatch(); return; }
  measuredCols = cols;
  // Recompute against the effective width (termWidth may clamp to the PTY).
  recomputeEraseUp(termWidth());
  if (!busy) {
    // Compare against the width the live frame was actually drawn at, not
    // the previous measurement: a cold start can draw at the PTY width
    // before the first report lands. Any shrink means those rows wrapped.
    if (raw.drawn && raw.width > cols) resetComposerAfterShrink();
    else rawDraw();
  }
  reportWidthMismatch();
}
function consumeCPR(text) {
  let s = cprTail + text;
  cprTail = '';
  // Buffer a trailing ESC or partial CSI too, not only ESC[…: a response split
  // exactly after the ESC byte would otherwise leak "[r;cR" into the composer.
  // The second alternative covers the 18t report's optional space before `t`.
  const partial = s.match(/\x1b(?:\[[0-9;]*)?$/) || s.match(/\x1b\[8;[0-9;]*\s?$/);
  if (partial) { cprTail = partial[0]; s = s.slice(0, partial.index); }
  if (!s.includes('\x1b[')) return s;
  s = s.replace(/\x1b\[8;(\d+);(\d+)\s?t/g, (_, _rows, cols) => {
    applyMeasuredWidth(Number(cols) || 0, true);
    return '';
  });
  return s.replace(/\x1b\[(\d+);(\d+)R/g, (_, _row, col) => {
    applyMeasuredWidth(Number(col) || 0, false);
    return '';
  });
}
function rawData(chunk) {
  const input = consumeCPR(String(chunk));
  if (!input) return;
  let i = 0;
  while (i < input.length) {
    if (raw.pasting) {
      // The terminal sends pasted newlines as \r (xterm normalizes \r?\n to
      // \r before bracketing); fold them to the composer's \n so a pasted
      // chunk renders and submits as multi-line text, not CR bytes.
      const end = input.indexOf('\x1b[201~', i);
      if (end < 0) { rawInsert(input.slice(i).replace(/\r\n?/g, '\n')); break; }
      rawInsert(input.slice(i, end).replace(/\r\n?/g, '\n')); raw.pasting = false; i = end + 6; continue;
    }
    if (input.startsWith('\x1b[200~', i)) { raw.pasting = true; i += 6; continue; }
    const ch = input[i];
    if (ch === '\x1b') {
      // While a permission prompt is up, ↑/↓ move the highlighted row; the
      // printed menu is static, so the composer's status line reflects it.
      const permChoices = permission ? permissionChoices(permission) : null;
      if (permChoices && input.startsWith('\x1b[A', i)) {
        raw.permSel = Math.max(0, raw.permSel - 1);
        i += 3;
      }
      else if (permChoices && input.startsWith('\x1b[B', i)) {
        raw.permSel = Math.min(permChoices.length - 1, raw.permSel + 1);
        i += 3;
      }
      // While the "/" suggestion list is up, ↑/↓ move through it; otherwise
      // they walk input history as before.
      else if (input.startsWith('\x1b[A', i)) {
        const hits = rawSuggestions();
        if (hits.length) raw.sel = Math.max(0, raw.sel - 1); else rawHistory(-1);
        i += 3;
      }
      else if (input.startsWith('\x1b[B', i)) {
        const hits = rawSuggestions();
        if (hits.length) raw.sel = Math.min(hits.length - 1, raw.sel + 1); else rawHistory(1);
        i += 3;
      }
      else if (input.startsWith('\x1b[C', i)) { if (raw.cursor < raw.text.length) raw.cursor += 1; i += 3; }
      else if (input.startsWith('\x1b[D', i)) { if (raw.cursor > 0) raw.cursor -= 1; i += 3; }
      else if (input.startsWith('\x1b[3~', i)) { rawDelete(); i += 4; }
      else if (input.startsWith('\x1b[H', i) || input.startsWith('\x1b[1~', i)) { raw.cursor = 0; i += 3; }
      else if (input.startsWith('\x1b[F', i) || input.startsWith('\x1b[4~', i)) { raw.cursor = raw.text.length; i += 3; }
      else if (input.startsWith('\x1b\r', i) || input.startsWith('\x1b\n', i)) { rawInsert('\n'); i += 2; }
      else i += 1;
      continue;
    }
    // Keep consuming after a submit: bytes trailing the \r in the same read
    // are type-ahead (a paste burst coalesced with the keystroke), not
    // garbage — returning here dropped them, so a paste lost everything
    // after its first newline and submitted early.
    if (ch === '\r') { rawSubmit(); i += 1; continue; }
    if (ch === '\n') { rawInsert('\n'); i += 1; continue; }
    if (ch === '\x7f' || ch === '\x08') { rawBackspace(); i += 1; continue; }
    if (ch === '\x03') { rawInterrupt(); i += 1; continue; }
    if (ch === '\x04') { if (raw.text === '') { void shutdown(0); return; } i += 1; continue; }
    if (ch === '\t') { rawComplete(); i += 1; continue; }
    if (ch === '\x01') { raw.cursor = 0; i += 1; continue; }
    if (ch === '\x05') { raw.cursor = raw.text.length; i += 1; continue; }
    if (ch === '\x0b') { raw.text = raw.text.slice(0, raw.cursor); i += 1; continue; }
    if (ch === '\x15') { raw.text = raw.text.slice(raw.cursor); raw.cursor = 0; i += 1; continue; }
    if (ch === '\x17') { rawDeleteWord(); i += 1; continue; }
    if (ch === '\x0c') { clearScreen(); i += 1; continue; }
    if (ch < ' ') { i += 1; continue; }
    if (permission) {
      // Number/y/n/a answer in a single keypress, like the TUI harnesses.
      const answer = permissionHotkey(ch);
      if (answer) { permissionFire(answer); return; }
    }
    rawInsert(ch); i += 1;
  }
  // The composer owns the input in every state, and it is always on screen —
  // idle, mid-turn (with the spinner in its status row) and during a permission
  // prompt. Every input update repaints it, so the draft is always visible.
  // Re-probe first: the pane may have narrowed since the last probe without a
  // PTY resize, and drawing at the stale width is exactly what wraps into a
  // staircase.
  probeWidth();
  rawDraw();
}
function rawInit() {
  if (rawBound) return;
  rawBound = true;
  try { process.stdin.setRawMode?.(true); } catch {}
  // Opt into bracketed paste (DEC 2004) so the terminal wraps Ctrl+Shift+V
  // pastes in ESC[200~ … ESC[201~. Without this, xterm sends pasted newlines
  // as bare \r, which the composer below reads as submit keys: the first
  // pasted line fired immediately and the rest of the read was dropped.
  try { process.stdout.write('\x1b[?2004h'); } catch {}
  process.stdin.resume();
  process.stdin.setEncoding('utf8');
  process.stdin.on('data', rawData);
  probeWidth();
  reportWork(); // clear any stale count the pane kept from a previous client
  // A single early probe can be lost while the pane attaches; retry until one
  // lands. Re-measure on a timer ONLY while a turn is in flight: a long turn
  // can outlive a resize that never reached the PTY (a non-owner viewer sends
  // no SIGWINCH), and the live line repaints every 120 ms, so a stale width
  // would wrap every frame into permanent lines.
  //
  // While IDLE we deliberately stop probing. The probe moves the caret, and
  // xterm keeps the viewport on the caret, so an idle tick yanked a reader who
  // had scrolled up back to the bottom a second or two later. Idle width drift
  // is still absorbed by the probes that already run before every idle repaint
  // (input, SIGWINCH and prompt()), so a stale idle frame is corrected as soon
  // as anything is drawn again. (UX review 2026-09-15, item F.)
  const widthWatch = setInterval(() => { if (busy && !permission) probeWidth(); }, 1500); widthWatch.unref?.();
  process.on('SIGWINCH', () => {
    // The pane just resized the PTY, so its width is authoritative until a fresh
    // probe lands. Drop a stale measurement so the redraw below cannot draw at
    // the previous, wider size while the probe is in flight.
    measuredCols = 0;
    // Probe even mid-turn so the next draw already has the real width.
    probeWidth();
    // The composer is the single bottom writer (idle or mid-turn), so a resize is
    // just a re-measured repaint; recomputeEraseUp re-wraps the old frame's rows.
    const width = Math.max(20, termWidth());
    recomputeEraseUp(width);
    rawDraw();
  });
}
function rawTeardown() {
  if (!rawBound) return;
  rawBound = false;
  try { process.stdin.setRawMode(false); } catch {}
  try { process.stdout.write('\x1b[?2004l'); } catch {}
  process.stdin.pause();
}
function prompt() {
  if (closing) return;
  if (RAW) {
    raw.index = -1;
    // A long turn can outlive a pane resize that never reached the PTY, so
    // force a fresh measurement before the first idle repaint.
    probeWidth();
    rawDraw();
    return;
  }
  rl.setPrompt(permission ? 'Select [1/2]: ' : promptText());
  rl.prompt();
}
function rejectPermission() {
  if (permission) write({ id: permission.id, result: { outcome: { outcome: 'cancelled' } } });
  permission = undefined;
  // Cancel every queued request too: a shutdown or cancel must not leave the
  // harness waiting on approvals that will never be shown.
  for (const queued of permissionQueue) write({ id: queued.id, result: { outcome: { outcome: 'cancelled' } } });
  permissionQueue = [];
}
wire.on('error', error => { console.error(safe(error.message)); void shutdown(1); });
wire.on('line', line => {
  let message;
  try { message = JSON.parse(line); } catch { console.error('\nInvalid response from DeepSeek Harness.'); void shutdown(1); return; }
  if (message.method === 'session/request_permission' && message.id != null) {
    const tool = permissionTool(message);
    if (canAlwaysAllow(tool.key) && alwaysAllow.has(tool.key)) {
      // Remembered approval: answer without drawing the menu. DSH has no
      // allow_always option, so this is an allow_once plus our own memory.
      // Only an explicit allow_once is used: falling back to options[0] could
      // select a reject entry when a harness orders its menu differently.
      const option = message.params?.options?.find(value => value.kind === 'allow_once');
      write({ id: message.id, result: { outcome: option ? { outcome: 'selected', optionId: option.optionId } : { outcome: 'cancelled' } } });
      emitOutput(`\n${dim(`✔ ${tool.label} allowed automatically (always allow)`)}\n`);
      void transcript({ role: 'approval', title: safe(message.params?.toolCall?.title ?? ''), decision: 'allow_always' });
      return;
    }
    // One menu at a time; queue any additional outstanding requests rather
    // than silently denying them.
    if (permission) { permissionQueue.push(message); return; }
    showPermission(message);
    return;
  }
  if (message.method && message.id != null) { write({ id: message.id, error: { code: -32601, message: 'Unsupported client method' } }); return; }
  if (message.method === 'session/update') {
    const update = message.params?.update;
    switch (update?.sessionUpdate) {
      case 'agent_message_chunk': {
        const content = update.content;
        if (content?.type === 'text') {
          const text = String(content.text);
          statusChunk(text.length); if (turnText.length < 8000) turnText = (turnText + text).slice(0, 8000);
          emitOutput(`\n${renderMarkdown(safe(text))}\n`);
        } else if (content?.type === 'image') {
          emitOutput(`\n${dim(safe(imageRef(content)))}\n`);
        }
        break;
      }
      case 'agent_thought_chunk':
        if (update.content?.type === 'text') renderThought(String(update.content.text));
        break;
      case 'tool_call':
        statusTool(update.title ?? update.kind ?? 'tool');
        if (update.toolCallId) {
          // A tool_call can already carry a settled status (fast/instant tools
          // that never get a follow-up update). Track only genuinely pending
          // work, or the count sticks forever and the close guard misfires.
          if (TOOL_SETTLED.has(update.status)) activeTools.delete(update.toolCallId);
          else activeTools.set(update.toolCallId, safe(update.title ?? update.kind ?? 'tool'));
        }
        renderToolCall(update);
        // A settled tool_call carries its result inline (no follow-up update).
        // That is the only payload that can announce a detached background job,
        // so process it here too — otherwise the task badge never appears.
        if (TOOL_SETTLED.has(update.status)) { statusTool(''); renderToolResult(update); }
        void transcript({ role: 'tool', kind: update.kind ?? 'tool', title: safe(update.title ?? '') });
        break;
      case 'tool_call_update':
        if (update.toolCallId) {
          if (TOOL_SETTLED.has(update.status)) { activeTools.delete(update.toolCallId); if (runningWork() === 0) exitArmed = false; }
          else if (update.title) activeTools.set(update.toolCallId, safe(update.title));
        }
        if (TOOL_SETTLED.has(update.status)) { statusTool(''); renderToolResult(update); }
        break;
      case 'usage_update': {
        // A legitimate 0 must reset the readout; `Number(x) || old` kept the
        // previous value forever after a new session or compaction.
        const used = Number(update.used), size = Number(update.size);
        if (Number.isFinite(used)) usage.used = used;
        if (Number.isFinite(size)) usage.size = size;
        break;
      }
      case 'config_option_update':
        configOptions = update.configOptions ?? configOptions;
        break;
    }
    reportWork();
    return;
  }
  const call = pending.get(message.id);
  if (call) {
    pending.delete(message.id); clearTimeout(call.timer);
    if (message.id === promptRequest) { promptRequest = null; clearCancelWatchdog(); }
    message.error ? call.reject(new Error(requestErrorMessage(message.error))) : call.resolve(message.result);
  }
});
// Preserve errors, but remove the known credential and terminal control codes.
child.stderr.on('data', chunk => process.stderr.write(safe(String(chunk))));
function ago(ts) {
  const secs = Math.max(0, (Date.now() - ts) / 1000);
  if (secs < 60) return 'just now';
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`;
  return `${Math.floor(secs / 86400)}d ago`;
}
// DSH titles are often blank, so enrich each session from our local
// transcript: message count, recency, and the opening user message.
async function sessionMeta(id) {
  try {
    const lines = (await fs.readFile(path.join(transcriptDir, transcriptName(id)), 'utf8')).split('\n').filter(Boolean);
    if (lines.length === 0) return null;
    let first = '', count = 0, when = 0;
    for (const line of lines) {
      try {
        const entry = JSON.parse(line);
        if (entry.role === 'user' || entry.role === 'agent' || entry.role === 'tool') count++;
        if (!first && entry.role === 'user' && entry.text) first = String(entry.text).slice(0, 80);
        if (entry.t) when = entry.t;
      } catch {}
    }
    return { count, first, when };
  } catch { return null; }
}
let lastSessionOrder = [];
async function listSessions() {
  const result = await request('session/list', { cwd: process.cwd() });
  const rows = Array.isArray(result?.sessions) ? result.sessions : [];
  if (rows.length === 0) { console.log('No sessions yet.'); return; }
  const metas = await Promise.all(rows.map(row => sessionMeta(row.sessionId)));
  const order = rows.map((row, index) => ({ row, meta: metas[index] }))
    .sort((a, b) => (b.meta?.when || 0) - (a.meta?.when || 0));
  lastSessionOrder = order.map(entry => entry.row.sessionId);
  console.log('Sessions (most recent first):');
  order.forEach((entry, index) => {
    const id = String(entry.row.sessionId);
    const title = entry.row.title && String(entry.row.title).trim() ? ` · ${safe(entry.row.title).slice(0, 60)}` : '';
    const meta = entry.meta ? dim(` · ${entry.meta.count} msgs · ${ago(entry.meta.when)}${entry.meta.first ? ` · "${safe(entry.meta.first)}"` : ''}`) : dim(' · no local history');
    console.log(`${bold(`${index + 1}.`)} ${id.slice(0, 8)}…${title}${meta}`);
    console.log(dim(`   ${id}`));
  });
  console.log('Use /resume NUMBER or ID to resume a session.');
}
// Record which harness account launched this session. The daemon's spend meter
// reads these sidecars to attribute DSH's durable per-turn usage to the right
// API key (and therefore user); the key itself never leaves the environment.
async function recordSessionAccount(sessionId) {
  const account = process.env.SOLU_HARNESS_ACCOUNT;
  if (!account || !sessionId) return;
  try {
    const dir = path.join(dshHome, 'session-accounts');
    await fs.mkdir(dir, { recursive: true, mode: 0o700 });
    await fs.writeFile(
      path.join(dir, `${sessionId}.json`),
      JSON.stringify({ account, model: process.env.SOLU_HARNESS_ACCOUNT_MODEL || model }),
      { mode: 0o600 },
    );
  } catch { /* attribution is best-effort; never block a session */ }
}
// Per-pane durable session pointer. The daemon exports SOLU_PANE_ID into every
// pane, so the client can record which DSH session belongs to this pane and
// resume it after the pane agent is replaced (fleet upgrade, daemon restart,
// reboot). This mirrors the daemon's Claude/Codex resume contract — a durable,
// per-pane conversation id replayed on the next launch — without adding a DSH
// session-id field to the daemon: the file IS the handshake. The daemon reads
// it back and passes SOLU_HARNESS_RESUME_SESSION on a managed respawn.
function paneSessionFile() {
  const paneId = String(process.env.SOLU_PANE_ID || '').trim();
  if (!paneId) return null;
  const safe = paneId.replace(/[^A-Za-z0-9_-]/g, '_').slice(0, 80);
  return safe ? path.join(dshHome, 'pane-sessions', `${safe}.json`) : null;
}
async function recordPaneSession(sessionId) {
  const file = paneSessionFile();
  if (!file || !sessionId) return;
  try {
    await fs.mkdir(path.dirname(file), { recursive: true, mode: 0o700 });
    await writePrivateAtomic(file, JSON.stringify({
      provider: 'deepseek', sessionId, cwd: process.cwd(), model: (() => { try { return currentModelId(); } catch { return model; } })(), updatedAt: Date.now(),
    }));
  } catch { /* best-effort; never block a session */ }
}
async function readPaneSession() {
  const file = paneSessionFile();
  if (!file) return '';
  try {
    const saved = JSON.parse(await fs.readFile(file, 'utf8'));
    return saved && saved.provider === 'deepseek' && typeof saved.sessionId === 'string' ? saved.sessionId.trim() : '';
  } catch { return ''; }
}
// Switch sessions transactionally. The old code closed the current session
// before attempting the new/resume call, so a failed resume (e.g. the session
// is already open in another pane and DSH answers "Internal error") left the
// pane with no session at all and the next prompt died on `Invalid params`.
// Open first, close the previous one only once the switch succeeded, so a
// failure keeps the working session.
async function newSession(id) {
  const previous = session;
  const result = await request(id ? 'session/resume' : 'session/new', { cwd: process.cwd(), mcpServers: [], ...(id ? { sessionId: id } : {}) });
  const next = result.sessionId ?? id;
  if (previous && previous !== next) {
    try { await request('session/close', { sessionId: previous }, 3000); } catch {}
  }
  session = next; configOptions = result.configOptions ?? [];
  void recordSessionAccount(session);
  void recordPaneSession(session);
  // Each session has its own context window, so drop the previous readout until
  // DSH reports this one — otherwise /new, /resume and /clear keep showing the
  // old session's ctx in the composer.
  usage.used = 0; usage.size = 0; turnStartUsed = 0; sessionTokens = 0;
  // A new session has no tasks. Stale counts would keep /status claiming
  // background work and /exit warning about jobs that belong to the old one.
  activeTools.clear(); backgroundJobs.clear(); toolCalls.clear();
  exitArmed = false;
  reportWork();
  transcriptPath = path.join(transcriptDir, transcriptName(session));
  await pruneTranscript(transcriptPath);
  console.log(`\nSession ${safe(session)}${id ? ' resumed' : ''}`);
  if (id) await replayTranscript(transcriptPath);
}
// Pi-style `!command`: run a local shell command in the pane's cwd. `!cmd` also
// hands the output to the model as the next prompt; `!!cmd` only prints it.
// The running child is tracked so Ctrl+C and shutdown can stop it instead of
// leaving the pane blocked until the 120s timeout.
let activeShell = null;
function runShell(cmd, timeoutMs = 120000) {
  return new Promise(resolve => {
    let child;
    // A Windows backend has no /bin/sh; PowerShell is the shell its panes run.
    const [shell, ...shellArgs] = process.platform === 'win32' ? ['powershell.exe', '-NoProfile', '-Command', cmd] : ['/bin/sh', '-c', cmd];
    try { child = spawn(shell, shellArgs, { cwd: process.cwd(), stdio: ['ignore', 'pipe', 'pipe'] }); }
    catch (error) { resolve({ code: 1, output: String(error?.message ?? error) }); return; }
    activeShell = child;
    const cap = 64 * 1024;
    let out = '', err = '';
    const finish = result => { if (activeShell === child) activeShell = null; resolve(result); };
    const timer = setTimeout(() => child.kill('SIGKILL'), timeoutMs); timer.unref?.();
    child.stdout.on('data', data => { if (out.length < cap) out += data; });
    child.stderr.on('data', data => { if (err.length < cap) err += data; });
    child.on('error', error => { clearTimeout(timer); finish({ code: 1, output: String(error?.message ?? error) }); });
    child.on('exit', code => {
      clearTimeout(timer);
      finish({ code: code ?? 0, output: (out + (err ? (out ? '\n' : '') + err : '')).trim() });
    });
  });
}
async function chooseConfig(category, choice) {
  const option = configOptions.find(row => row.category === category || row.id === category);
  if (!option) throw new Error('The harness does not advertise this option for the current model.');
  let values = (option.options ?? []).flatMap(row => row.options ?? [row]);
  if (category === 'model') {
    values = values.filter(row => {
      try { validateDeepseekSelector(decodeDshModelValue(row.value).model); return true; }
      catch { return false; }
    });
  }
  if (!choice) { values.forEach((row, index) => console.log(`${index + 1}. ${safe(row.name)}${row.value === option.currentValue ? ' (selected)' : ''}`)); console.log(`Use /${category === 'reasoning_effort' ? 'effort' : 'model'} NUMBER`); return; }
  const selected = values[Number(choice) - 1];
  if (!selected) throw new Error('Choose a listed number.');
  if (category === 'model') validateDeepseekSelector(decodeDshModelValue(selected.value).model);
  const result = await request('session/set_config_option', { sessionId: session, configId: option.id, value: selected.value });
  configOptions = result.configOptions ?? configOptions;
  console.log(`Selected ${safe(selected.name)}`);
}
async function submit(line, options = {}) {
  if (permission) {
    const current = permission; permission = undefined;
    statusApproval(false);
    const tool = permissionTool(current);
    // Focused option 1 is allow-once, so bare Enter (what SoluTerminal's
    // auto-approval writes) allows one invocation; 1/y/yes do the same.
    // 3/a/always would remember the tool, but no DeepSeek tool is offered it
    // (canAlwaysAllow), so like any unlisted answer it rejects. 2/n/no and every unrecognized answer reject.
    const answer = line.trim().toLowerCase();
    const always = answer === '3' || answer === 'a' || answer === 'always';
    const remember = always && canAlwaysAllow(tool.key);
    // Option 3 isn't on the menu for this tool: like any other unlisted
    // answer, it rejects.
    const kind = remember || answer === '' || answer === '1' || answer === 'y' || answer === 'yes' ? 'allow_once' : 'reject_once';
    if (remember) { alwaysAllow.set(tool.key, tool.label); void saveAlwaysAllow(); }
    const option = current?.params?.options?.find(value => value.kind === kind);
    write({ id: current.id, result: { outcome: option ? { outcome: 'selected', optionId: option.optionId } : { outcome: 'cancelled' } } });
    void transcript({ role: 'approval', title: safe(current?.params?.toolCall?.title ?? ''), decision: remember ? 'allow_always' : kind });
    advancePermission();
    return;
  }
  if (busy) {
    const queued = line.trim();
    if (queued) { steeringQueue.push({ text: queued, resolveRefs: options.resolveRefs !== false }); console.log(dim(`⇢ queued (${steeringQueue.length}): ${safe(queued).slice(0, 80)}`)); }
    else console.log('⏳ turn still running — type a message to queue it, or Ctrl+C to cancel.');
    return;
  }
  busy = true;
  try {
    const text = line.trim();
    if (!text) return;
    if (text === '/exit' || text === '/quit') {
      if (runningWork() > 0 && !exitArmed) {
        exitArmed = true;
        const timer = setTimeout(() => { exitArmed = false; }, 5000); timer.unref?.();
        console.log(paint(`⚠ ${runningWork()} task${runningWork() === 1 ? '' : 's'} still running — run /exit again to close anyway.`, 'yellow'));
        return;
      }
      await shutdown(0); return;
    }
    if (text === '/help') console.log([
      `${bold('DeepSeek Harness')} ${dim('· SoluTerminal')}`,
      '',
      ...COMMANDS.map(row => `  ${row.name.padEnd(11)}${(row.args || '').padEnd(8)}${row.desc}`),
      '',
      'Type / to see suggestions (↑/↓ picks, else history) · Tab completes · Ctrl+J newline.',
      'Enter while a turn runs queues a steering message. !command runs a shell command and',
      'sends its output; !!command runs it only. @path inlines a text file (Tab completes @paths).',
      'Chat, tools and per-tool approvals work here; Ctrl+C cancels a running turn; turns are saved',
      'locally and /resume replays recent history. /tools collapse hides a finished tool\'s body,',
      'which still reaches the model. DSH web-only cards and elicitation are not exposed.',
    ].join('\n'));
    else if (text === '/allow') console.log(alwaysAllow.size ? `Always allow: ${[...alwaysAllow.values()].join(', ')} ${dim('· /allow clear to revoke')}` : 'No always-allow tools: DeepSeek tools ask each time.');
    else if (text === '/allow clear') { alwaysAllow.clear(); void saveAlwaysAllow(); console.log('Cleared always-allow tools.'); }
    // Claude-parity /clear: wipe the screen AND start a fresh session, so the
    // context readout actually goes back to zero. The previous conversation is
    // still saved and resumable via /resume; Ctrl+L clears the screen alone.
    else if (text === '/clear') { clearScreen(); await newSession(); }
    else if (text === '/status') console.log(statusReport());
    else if (text === '/tasks') console.log(tasksReport());
    else if (text === '/tools') console.log(`Tool output: ${toolOutputCollapsed ? 'collapsed' : 'full'}${toolOutputCollapsed ? ' · /tools full to show bodies' : ' · /tools collapse to hide bodies'}`);
    else if (text === '/tools full') { toolOutputCollapsed = false; console.log('Tool output: full.'); }
    else if (text === '/tools collapse') { toolOutputCollapsed = true; console.log('Tool output: collapsed. Bodies still reach the model; /tools full restores display.'); }
    else if (text.startsWith('/tools ')) console.log('Usage: /tools [full|collapse]');
    else if (text === '/hotkeys') console.log([
      bold('Key bindings'),
      '  Enter              send · while a turn runs: queue a steering message',
      '  Ctrl+J / Alt+Enter insert a newline',
      '  ↑ / ↓              move through / suggestions, else input history',
      '  Tab                complete the highlighted command',
      '  Ctrl+L             clear the screen',
      '  Ctrl+C             cancel the running turn · clear input · close when idle',
      '  Ctrl+D             exit when the input is empty',
    ].join('\n'));
    else if (text === '/export' || text.startsWith('/export ')) {
      const target = text.slice(7).trim() || path.join(process.cwd(), `deepseek-session-${String(session || 'current').slice(0, 8)}.jsonl`);
      try {
        if (!transcriptPath) throw new Error('this session has no local transcript yet');
        await fs.copyFile(transcriptPath, target);
        console.log(`Exported this session to ${target}`);
      } catch (error) { console.log(`Export failed: ${safe(error.message)}`); }
    }
    else if (text === '/import') console.log('Usage: /import <file.jsonl> — replay an exported session transcript (read-only history, not re-sent).');
    else if (text.startsWith('/import ')) {
      const raw = text.slice(8).trim();
      const target = raw.startsWith('~/') ? path.join(process.env.HOME || '', raw.slice(2)) : path.resolve(process.cwd(), raw);
      try {
        const info = await fs.stat(target);
        if (!info.isFile()) throw new Error('not a file');
        if (info.size > (16 << 20)) throw new Error('transcript is larger than 16 MiB');
        const lines = (await fs.readFile(target, 'utf8')).split('\n').filter(Boolean);
        // Every line must be a transcript entry. A random JSONL or text file is
        // not a session, and replaying one would print nonsense as history.
        if (lines.length === 0 || lines.some(line => { try { return typeof JSON.parse(line)?.role !== 'string'; } catch { return true; } })) {
          throw new Error('not a SoluTerminal session transcript');
        }
        await replayTranscript(target);
      } catch (error) { console.log(`Import failed: ${safe(error.message)}`); }
    }
    else if (text === '/context') console.log(usage.size ? `${contextLabel()} ${dim(`· ${fmtTokens(Math.max(0, usage.size - usage.used))} free · session +${fmtTokens(sessionTokens)} added`)}` : 'Context usage is not advertised by the harness yet.');
    else if (text === '/thinking') { thinkingMode = THINKING_MODES[(THINKING_MODES.indexOf(thinkingMode) + 1) % THINKING_MODES.length]; console.log(`Reasoning display: ${thinkingMode}.`); }
    else if (text === '/new') await newSession();
    else if (text === '/sessions') await listSessions();
    else if (text === '/resume') await listSessions();
    else if (text.startsWith('/resume ')) {
      const arg = text.slice(8).trim();
      const pick = Number(arg);
      let target;
      if (arg && Number.isInteger(pick) && pick >= 1 && pick <= lastSessionOrder.length) target = lastSessionOrder[pick - 1];
      else if (arg && /^\d+$/.test(arg) && arg.length <= 3) console.log(`Choose 1–${lastSessionOrder.length || 'a listed number'} or paste a session ID. Run /resume to see the list.`);
      else if (arg) target = arg;
      if (target) {
        // A resume can fail server-side (a session already open in another
        // pane answers "Internal error"). That must not take the pane down
        // with it: newSession() keeps the current session on failure, and if
        // there was none we start a fresh one rather than prompt sessionless.
        try { await newSession(target); }
        catch (error) {
          console.error(`\n${paint(`Could not resume ${safe(String(target).slice(0, 8))}…: ${safe(error.message)}`, 'red')}`);
          if (session) console.error(dim(`Staying on session ${safe(String(session).slice(0, 8))}….`));
          else { console.error(dim('Starting a new session instead.')); await newSession(); }
        }
      }
    }
    else if (/^\/(model|effort)( |$)/.test(text)) { const [command, choice] = text.split(/\s+/); await chooseConfig(command === '/model' ? 'model' : 'reasoning_effort', choice); }
    else if (text.startsWith('/')) console.log('Unknown command. Use /help.');
    else if (text.startsWith('!')) {
      const silent = text.startsWith('!!');
      const cmd = text.slice(silent ? 2 : 1).trim();
      if (!cmd) console.log('Usage: !command runs it and sends the output to the model; !!command runs it only.');
      else {
        console.log(dim(`$ ${cmd}`));
        const result = await runShell(cmd);
        if (result.output) console.log(safe(result.output).slice(0, 8000));
        console.log(dim(`[exit ${result.code}]`));
        await transcript({ role: 'user', text: safe(`!${cmd}`).slice(0, 8000) });
        // `!` hands the output to the model as the next prompt; `!!` does not.
        if (!silent && result.output) await submit(`Command \`${cmd}\` output (exit ${result.code}):\n\`\`\`\n${safe(result.output).slice(0, 8000)}\n\`\`\``, { resolveRefs: false });
      }
    }
    else {
      if (!session) { console.log(dim('No active session — starting a new one.')); await newSession(); }
      await ensureCurrentDeepseekModel();
      turnStartUsed = usage.used;
      exitArmed = false;
      statusStart();
      turnText = '';
      // Must go through emitOutput: the live spinner line is already on screen,
      // and a raw console.log here left a stale "⠋ thinking" line behind it.
      emitOutput(`\n${turnHeader()}\n`);
      // Resolve @ references only for text the user typed. A prompt synthesized
      // from `!command` output is untrusted: auto-inlining a path it happens to
      // mention would read local files the user never asked for.
      const resolved = options.resolveRefs === false ? { text, blocks: [], inlined: [] } : await resolvePromptRefs(text);
      if (resolved.inlined.length) emitOutput(dim(`Inlined ${resolved.inlined.join(', ')}`) + '\n');
      await transcript({ role: 'user', text: safe(resolved.text).slice(0, 8000), ...(resolved.blocks.length > 0 ? { images: resolved.blocks.length } : {}) });
      try {
        const result = await request('session/prompt', { sessionId: session, prompt: [{ type: 'text', text: resolved.text }, ...resolved.blocks] }, 0);
        // DSH settles a prompt only after its owned tools have settled, so any
        // entry still marked in-progress is a lost update. Clear it here rather
        // than let a stale count keep the close guard warning forever. Detached
        // background jobs live in backgroundJobs and are untouched.
        if (activeTools.size) { activeTools.clear(); reportWork(); }
        const secs = statusStop(result.stopReason || 'done');
        console.log(turnFooter(result.stopReason, secs));
        await transcript({ role: 'agent', text: safe(turnText).slice(0, 8000), stop: result.stopReason });
      } catch (error) {
        const secs = statusStop('failed');
        console.error(`\n${paint(safe(error.message), 'red')} ${dim(`(${secs})`)}`);
      } finally {
        promptRequest = null; clearCancelWatchdog();
      }
    }
  } catch (error) { if (status.active) statusStop('failed'); console.error(`\n${safe(error.message)}`); }
  finally {
    busy = false;
    // A queued steering message becomes the next prompt; otherwise go idle.
    // Carry its ref-resolution intent so `!command` output is never inlined.
    if (!closing && steeringQueue.length) { const next = steeringQueue.shift(); void submit(next.text, { resolveRefs: next.resolveRefs }); }
    else prompt();
  }
}
// While the raw composer owns the screen, a bare console.log/error lands at the
// parked input cell and erases the composer (the same overwrite emitOutput now
// guards). Route them through the composer-safe writer. Only in RAW: the
// piped/readline path (tests, scripts) keeps plain console semantics.
if (RAW) {
  console.log = (...values) => emitOutput(format(...values) + '\n');
  console.error = (...values) => emitOutput(format(...values) + '\n', process.stderr);
}
if (RAW) rawInit(); else rl.on('line', submit);
async function shutdown(code) {
  if (closing) return; closing = true; steeringQueue.length = 0; clearCancelWatchdog(); statusStopSilent(); liveStop(); if (activeShell) { try { activeShell.kill('SIGKILL'); } catch {} } rejectPermission(); if (rl) rl.close(); rawTeardown(); await saveHistory(); process.stdin.destroy();
  if (session) { try { await request('session/close', { sessionId: session }, 3000); } catch {} }
  child.stdin.end();
  const timer = setTimeout(() => child.kill('SIGKILL'), 5000); timer.unref();
  process.exitCode = code;
}
process.on('SIGINT', () => {
  if (activeShell) { try { activeShell.kill('SIGKILL'); } catch {} return; }
  if (busy && session) { rejectPermission(); write({ method: 'session/cancel', params: { sessionId: session } }); armCancelWatchdog(); return; }
  // Same close guard as raw mode: warn once before dropping in-flight work.
  if (runningWork() > 0 && !exitArmed) {
    exitArmed = true;
    const timer = setTimeout(() => { exitArmed = false; }, 5000); timer.unref?.();
    console.error(`\n⚠ ${runningWork()} task${runningWork() === 1 ? '' : 's'} still running — Ctrl+C again to close.`);
    return;
  }
  void shutdown(0);
});
if (rl) rl.on('close', () => { if (!closing) void shutdown(0); });
process.on('SIGTERM', () => void shutdown(0));
// A pane or terminal hang-up must stop the DSH child too, not orphan it.
process.on('SIGHUP', () => void shutdown(0));
// Last-resort sync cleanup: if the process exits without a graceful shutdown,
// still take the child down and drop the scratch directory. SIGKILL cannot be
// caught, so sweepStalePatchDirs covers that case on the next start.
process.on('exit', () => { try { child.kill('SIGKILL'); } catch {} try { fsSync.rmSync(patchDir, { recursive: true, force: true }); } catch {} });
child.on('error', error => { console.error(safe(error.message)); void shutdown(1); });
child.on('exit', async code => {
  for (const call of pending.values()) { clearTimeout(call.timer); call.reject(new Error('DeepSeek Harness stopped.')); }
  pending.clear(); closing = true; rl?.close(); rawTeardown(); process.stdin.destroy(); await fs.rm(patchDir, { recursive: true, force: true }); process.exitCode = ready ? (code ?? 1) : 1;
});
busy = true;
try {
  console.log(`${bold('DeepSeek Harness')} ${dim(`· SoluTerminal ${clientVersion}`)}`);
  console.log(dim(`Starting official DSH${dshVersion ? ` ${dshVersion}` : ''}… · ${process.cwd()}`));
  const init = await request('initialize', { protocolVersion: 1, clientCapabilities: {}, clientInfo: { name: 'soluterminal', version: clientVersion } });
  imageCapableBoot = !!init?.agentCapabilities?.promptCapabilities?.image;
  // Resume the pane's session when the daemon re-launched us for a restart
  // (SOLU_HARNESS_RESUME_SESSION), or when this pane already has a recorded
  // session (a hand relaunch after the daemon was torn down). A stale id must
  // never brick the client: fall back to a fresh session.
  const resumeId = String(process.env.SOLU_HARNESS_RESUME_SESSION || '').trim() || await readPaneSession();
  try {
    await newSession(resumeId || undefined);
  } catch (error) {
    if (!resumeId) throw error;
    console.error(dim(`Could not resume session ${safe(resumeId)} — starting a new one.`));
    await newSession();
  }
  bootModelValue = currentModelValue(); ready = true; busy = false; prompt();
} catch (error) { console.error(safe(error.message)); await shutdown(1); }
