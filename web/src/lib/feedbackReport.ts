import type { FeedbackContext, FeedbackPreview } from '../api';
import { SITE_CONTACT_EMAIL } from './siteLinks';
import { openInSystemBrowser, tauriInvoke } from '../tauri-browser';

// Pure helpers for the feedback form (components/inbox/FeedbackComposer.tsx):
// the body it sends, the plain-text report behind Copy report and Email it,
// the details rows of "What we'll send", and the error sentences. All strings
// come from launch/marketing/site-copy/soluterminal-inapp-feedback-wording.md.

export type FeedbackType = 'bug' | 'idea' | 'question';

export const FEEDBACK_TYPE_LABEL: Record<FeedbackType, string> = {
  bug: 'Bug',
  idea: 'Idea',
  question: 'Question',
};

/** The main field's label, placeholder and empty-field error, by type. */
export const FEEDBACK_MAIN_FIELD: Record<FeedbackType, { label: string; placeholder: string; empty: string }> = {
  bug: {
    label: 'What happened?',
    placeholder: 'What happened, and what did you expect instead?',
    empty: 'Tell us what happened.',
  },
  idea: { label: 'What would you like it to do?', placeholder: '', empty: "Tell us what you'd like it to do." },
  question: { label: "What's your question?", placeholder: '', empty: 'Type your question.' },
};

/** What the form fills in when it is opened from an error screen. */
export interface FeedbackPrefill {
  type?: FeedbackType;
  trying?: string;
  body?: string;
  /** A follow-up to this report of the person's own: a clean form that
   *  leaves any saved draft alone, linked to #N by a field, not by text. */
  followUpOf?: number;
}

/** Above the follow-up form's type picker (wording item 17). */
export function feedbackFollowUpLine(n: number): string {
  return `This is a follow-up to report #${n}.`;
}

/** Open the feedback form in the inbox, optionally filled in (an error
 *  screen's "Report this"). Every entry point lands here: the status-bar
 *  button, Help › Send feedback, Ctrl+K and the mobile menu. The inbox host
 *  (components/inbox/InboxPanel.tsx) listens for the event. */
export function openFeedback(prefill?: FeedbackPrefill) {
  window.dispatchEvent(new CustomEvent('solu:open-inbox', { detail: { view: 'compose', prefill } }));
}

/** The report body: the optional "trying to do" line, then the main field. */
export function composeFeedbackBody(trying: string, main: string): string {
  const t = trying.trim();
  const m = main.trim();
  return t ? `What I was trying to do: ${t}\n\n${m}` : m;
}

/** The first non-empty line of what the user wrote: the title when none is
 *  typed. Keep it whole so the daemon can redact it before applying its limit. */
export function feedbackFirstLine(text: string): string {
  return text.split('\n').map((l) => l.trim()).find(Boolean) ?? '';
}

/** The "App and system details" rows, in the order the form shows them.
 *  Every field the daemon sends that a person can read is listed; empty ones
 *  are left out because they aren't sent with a value. */
export function feedbackDetailRows(ctx: FeedbackContext): [string, string][] {
  const rows: [string, string][] = [
    ['App version', ctx.appVersion],
    ['Daemon version', ctx.daemonVersion],
    ['Web version', ctx.clientVersion],
    ['Desktop app version', ctx.shellVersion],
    ['Desktop app platform', ctx.shellPlatform],
    ['Platform', ctx.platform],
    ['Backend', ctx.backendID],
    ['Operating system', ctx.os],
    ['Windows build', ctx.windowsBuild ?? ''],
    ['CPU type', ctx.arch],
    ['An ID for this computer (not its name)', ctx.machineId],
  ];
  if (ctx.paneId) {
    rows.push(["Pane's ID", ctx.paneId], ['Pane type', ctx.paneKind ?? ''], ['Agent', ctx.paneAgent ?? '']);
  }
  return rows.filter(([, v]) => !!v);
}

/** The plain-text report for Copy report and Email it: exactly the redacted
 *  preview, nothing the form doesn't show. */
export function feedbackReportText(type: FeedbackType, p: FeedbackPreview, followUpOf?: number): string {
  const parts = [`SoluTerminal feedback · ${FEEDBACK_TYPE_LABEL[type]}`];
  if (followUpOf) parts.push(`Follow-up to #${followUpOf}`);
  if (p.title) parts.push(p.title);
  if (p.body) parts.push(p.body);
  parts.push(['App and system details', ...feedbackDetailRows(p.context).map(([k, v]) => `${k}: ${v}`)].join('\n'));
  if (p.snapshot) parts.push(`Recent text from a pane (last 40 lines)\n${p.snapshot}`);
  if (p.logs) parts.push(`Recent app logs\n${p.logs}`);
  return parts.join('\n\n');
}

/** Windows hands a mailto link to the mail app only up to about 2,000
 *  characters, so the encoded body stays within this budget (@secure). */
export const FEEDBACK_MAILTO_BODY_MAX = 1800;
export const FEEDBACK_MAILTO_CUT_NOTE = '(cut to fit; use Copy report for the full text)';

/** A mailto link to the team with the report as its body, cut to fit. The
 *  subject carries the type and web version, so the mailbox can be sorted. */
export function feedbackMailto(text: string, type: FeedbackType, webVersion: string): { href: string; cut: boolean } {
  const subject = encodeURIComponent(`SoluTerminal feedback · ${FEEDBACK_TYPE_LABEL[type]} · web ${webVersion}`);
  const href = (body: string) => `mailto:${SITE_CONTACT_EMAIL}?subject=${subject}&body=${encodeURIComponent(body)}`;
  if (encodeURIComponent(text).length <= FEEDBACK_MAILTO_BODY_MAX) return { href: href(text), cut: false };
  // Longest prefix (whole code points) whose encoded form, with the note, fits.
  const chars = Array.from(text);
  const fits = (n: number) =>
    encodeURIComponent(`${chars.slice(0, n).join('').trimEnd()}\n\n${FEEDBACK_MAILTO_CUT_NOTE}`).length <= FEEDBACK_MAILTO_BODY_MAX;
  let lo = 0;
  let hi = chars.length;
  while (lo < hi) {
    const mid = Math.ceil((lo + hi) / 2);
    if (fits(mid)) lo = mid;
    else hi = mid - 1;
  }
  return { href: href(`${chars.slice(0, lo).join('').trimEnd()}\n\n${FEEDBACK_MAILTO_CUT_NOTE}`), cut: true };
}

/** Opens a mail draft in the person's own mail app: the desktop shell needs
 *  the opener plugin (a plain link does nothing there); a browser follows the
 *  link itself. Resolves false when the desktop shell could not hand the link
 *  over, and null in a browser, where a page can't tell what happened. */
export async function openMailDraft(href: string): Promise<boolean | null> {
  if (tauriInvoke()) return openInSystemBrowser(href);
  window.location.href = href;
  return null;
}

export const FEEDBACK_COPIED_NOTE =
  'Copied. Paste it into an email to hello@soluterminal.com, or into a GitHub issue (those are public, so check it first).';
export const FEEDBACK_COPY_ERROR = "Couldn't copy the report. Try again, or use Email it.";
export const FEEDBACK_EMAIL_CUT_NOTE =
  'This report was too long for an email link, so the email has only the first part. Use Copy report to get all of it and paste it into the email.';
/** The desktop shell said the link could not be opened. */
export const FEEDBACK_NO_MAIL_APP_ERROR = 'No email app opened. Copy the report and send it to hello@soluterminal.com.';
/** A browser can't tell whether a draft opened, so this is said every time. */
export const FEEDBACK_MAIL_DRAFT_NOTE = 'If no email draft opened, copy the report and send it to hello@soluterminal.com.';
/** Beside text this page built, which the background service never checked. */
export const FEEDBACK_UNCHECKED_NOTE =
  "Check this text before you send it: we couldn't run our usual check for private details.";
/** After "Connect this computer" on the form (wording item 18). */
export const FEEDBACK_CONNECTED_TOAST = 'This computer is connected to your account. You can send your report in a few seconds.';
/** The form's line when this computer isn't connected (wording item 4). */
export const FEEDBACK_UNPAIRED_LINE =
  "This computer isn't connected to your account, so this can't be sent from here. Connect it to send from the app, or copy the report or email it to us.";
/** On a pane-text or logs row while the background service can't be reached. */
export const FEEDBACK_ROW_UNAVAILABLE = "This can't be included right now, so it won't be sent.";
/** Copy report or Email it found the background service gone: nothing went
 *  out, and the person goes on once the check line is in front of them. */
export const FEEDBACK_NOT_SENT_YET = 'Nothing was copied or emailed yet. Check the report, then copy or email it.';

/** The report behind CloudGate's "Account not ready" Copy report / Email it:
 *  the app isn't loaded there, so it carries only what the page knows. */
export function accountNotReadyReportText(webVersion: string): string {
  return [
    `SoluTerminal feedback · ${FEEDBACK_TYPE_LABEL.bug}`,
    "Account not ready: this account has no SoluTerminal profile.",
    ['App and system details', `Web version: ${webVersion}`].join('\n'),
  ].join('\n\n');
}

/** The kind of device this page is open on, as one coarse word. The browser's
 *  own description of itself is never carried. */
export function feedbackDevice(): string {
  if (typeof navigator === 'undefined') return 'Other';
  const ua = navigator.userAgent || '';
  if (/Android/i.test(ua)) return 'Android';
  // An iPad asks for desktop sites as a Mac, with a touch screen.
  if (/iPad|iPhone|iPod/.test(ua) || (/Macintosh/.test(ua) && (navigator.maxTouchPoints ?? 0) > 1)) return 'iPhone or iPad';
  if (/Windows/i.test(ua)) return 'Windows';
  if (/Macintosh|Mac OS X/i.test(ua)) return 'Mac';
  if (/Linux|X11/i.test(ua) && !/CrOS/.test(ua)) return 'Linux';
  return 'Other';
}

/** The report when the background service can't be reached for the redacted
 *  preview: what was typed and what the page itself knows. No pane text or
 *  logs, which only the background service can capture and check. */
export function feedbackFallbackReportText(
  type: FeedbackType, title: string, body: string, webVersion: string, followUpOf?: number,
): string {
  return scrubLocalReport([
    `SoluTerminal feedback · ${FEEDBACK_TYPE_LABEL[type]}`,
    ...(followUpOf ? [`Follow-up to #${followUpOf}`] : []),
    title,
    body,
    [
      'App and system details',
      `Web version: ${webVersion}`,
      `Device: ${feedbackDevice()}`,
      "Other app details weren't available.",
    ].join('\n'),
  ].join('\n\n'));
}

/** The crash screen's report in the same case. */
export function crashReportFallbackText(title: string, body: string, webVersion: string): string {
  return feedbackFallbackReportText('bug', title, body, webVersion);
}

// The last label must be letters, so "react-dom@18.3.1" in a stack stays.
const LOCAL_EMAIL = /[A-Za-z0-9._%+-]+@(?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,}/g;
// /home/<x>, /Users/<x> and C:\Users\<x> (either slash, so file:// URLs too).
// A Windows or Mac name may hold spaces, so it runs to the next slash, quote,
// colon or bracket, or to the line end, as in shared/feedbackredact.
const LOCAL_HOME = /(\/home\/)[^\\/\s:"'()<>]+/gi;
const LOCAL_USERS_HOME = /([\\/]Users[\\/])[^\\/\s:"'()<>](?:[^\\/\r\n:"'()<>]*[^\\/\s:"'()<>])?/gi;
const LOCAL_TOKEN_PREFIX = /\b(?:sk|pk|rk|ghp|gho|ghs|ghu|ghr|github_pat|glpat|xox[abeprs])[-_][A-Za-z0-9_-]{10,}/g;
const LOCAL_LONG_RUN = /[A-Za-z0-9_-]{32,}/g;
const LOCAL_SECRET_NAME = String.raw`(?:password|passphrase|passwd|secret|token|api[_-]?key|access[_-]?key|auth[_-]?token|access_token|refresh_token|id_token|account_id|openai_api_key|codex_access_token|codex_api_key|expires_at|\b[A-Z][A-Z0-9_]*_KEY)`;
const LOCAL_CODE_NAME = String.raw`(?:pairing[ _-]?code|pair[ _-]?code|device[ _-]?code|verification[ _-]?code|user[ _-]?code|claim[ _-]?code)`;

function localQuotedValue(name: string, escaped = true): RegExp {
  const double = escaped ? String.raw`(?:\\[^\r\n]|[^"\\\r\n])*(?:\\)?` : String.raw`[^"\r\n]+`;
  const single = escaped ? String.raw`(?:\\[^\r\n]|[^'\\\r\n])*(?:\\)?` : String.raw`[^'\r\n]+`;
  const end = escaped ? String.raw`|(?=\r?$)` : '';
  // Bound separator matching even when an unfinished draft has no value.
  return new RegExp(
    String.raw`("${name}"[ \t]{0,64}:[ \t]{0,64}|${name}\b(?:[ \t]{1,64}is)?(?:[ \t]{0,64}[=:][ \t]{0,64}|[ \t]{1,64}))(?:"(${double})("${end})|'(${single})('${end}))`,
    'gim',
  );
}

const LOCAL_QUOTED_RULES = [
  { pattern: localQuotedValue(LOCAL_CODE_NAME), kind: 'code' },
  { pattern: localQuotedValue(LOCAL_CODE_NAME, false), kind: 'code' },
  { pattern: localQuotedValue(LOCAL_SECRET_NAME), kind: 'token' },
  { pattern: localQuotedValue(LOCAL_SECRET_NAME, false), kind: 'token' },
];
const LOCAL_MARKER = /(\[(?:REDACTED|redacted: (?:token|email|ip|host|user|code|private key|password|header))\])/g;

interface LocalQuotedSpan { start: number; end: number; kind: string }

// Each rule supplies an ordered stream; merge on the original text so either
// quote interpretation can find a following field. Overlaps are removed once.
function mergeLocalQuoted(a: LocalQuotedSpan[], b: LocalQuotedSpan[]): LocalQuotedSpan[] {
  const out: LocalQuotedSpan[] = [];
  let i = 0, j = 0;
  while (i < a.length || j < b.length) {
    const next = j === b.length || (i < a.length && a[i].start <= b[j].start) ? a[i++] : b[j++];
    const last = out[out.length - 1];
    if (last && next.start < last.end) last.end = Math.max(last.end, next.end);
    else out.push({ ...next });
  }
  return out;
}

function scrubLocalQuoted(text: string): string {
  let spans: LocalQuotedSpan[] = [];
  for (const { pattern, kind } of LOCAL_QUOTED_RULES) {
    const matches: LocalQuotedSpan[] = [];
    for (const match of text.matchAll(pattern)) {
      const value = match[2] ?? match[4];
      if (!value) continue;
      const start = match.index + match[1].length + 1;
      matches.push({ start, end: start + value.length, kind });
    }
    spans = mergeLocalQuoted(spans, matches);
  }
  const out: string[] = [];
  let start = 0;
  for (const span of spans) {
    out.push(text.slice(start, span.start));
    out.push(text.slice(span.start, span.end).split(LOCAL_MARKER)
      .map((part, i) => i % 2 || !part ? part : `[redacted: ${span.kind}]`).join(''));
    start = span.end;
  }
  out.push(text.slice(start));
  return out.join('');
}

/** A small pass over text the daemon could not redact: quoted values,
 *  email addresses, home-folder names, and long token-like strings. Much
 *  less than the daemon's check, so the screen asks the person to read the
 *  text before sending it (@secure). Markers match shared/feedbackredact. */
export function scrubLocalReport(text: string): string {
  text = scrubLocalQuoted(text);
  return text
    .replace(LOCAL_EMAIL, '[redacted: email]')
    .replace(LOCAL_HOME, '$1[redacted: user]')
    .replace(LOCAL_USERS_HOME, '$1[redacted: user]')
    .replace(LOCAL_TOKEN_PREFIX, '[redacted: token]')
    // A long unbroken run mixing letters and digits; plain long words stay.
    .replace(LOCAL_LONG_RUN, (run) => (/\d/.test(run) && /[A-Za-z]/.test(run) ? '[redacted: token]' : run));
}

export const FEEDBACK_SEND_ERROR =
  "We couldn't send this just now. Your report is still here. Try again in a minute, or copy it and email us.";
export const FEEDBACK_PREVIEW_ERROR =
  "We couldn't show this preview, so it won't be sent. Tick it again to retry.";
export const FEEDBACK_LIMIT_ERROR =
  "You've reached today's limit for reports. Try again tomorrow, or copy this report and email it to us.";

/** The report could not be read, or its live state is unknown. */
export const FEEDBACK_THREAD_ERROR = "We can't load this report right now. Try again in a minute.";
/** The cloud's answer to a reply on a closed report (cloud/feedback_status.go). */
export const FEEDBACK_CLOSED_REPLY_ERROR =
  "This report is closed, so it can't take replies. Send a follow-up instead.";
/** Said only where the typed reply stays in the box after the failure. */
export const FEEDBACK_REPLY_ERROR =
  "Couldn't add your reply. It's still in the box, so try again in a minute.";

// ApiError carries the status; read it structurally so this module stays
// free of the API client.
function errorStatus(e: unknown): unknown {
  return (e as { status?: unknown } | null)?.status;
}

/** A failed send in plain words: never an HTTP status line. */
export function feedbackSendError(e: unknown): string {
  return errorStatus(e) === 429 ? FEEDBACK_LIMIT_ERROR : FEEDBACK_SEND_ERROR;
}

/** True when a reply was refused because the report has been closed. */
export function isFeedbackClosedReply(e: unknown): boolean {
  return errorStatus(e) === 409;
}

/** A failed reply in plain words: never an HTTP status line. */
export function feedbackReplyError(e: unknown): string {
  return isFeedbackClosedReply(e) ? FEEDBACK_CLOSED_REPLY_ERROR : FEEDBACK_REPLY_ERROR;
}
