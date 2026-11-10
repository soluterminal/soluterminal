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

/** The first non-empty line of what the user wrote, at most 200 characters:
 *  the title when none is typed. */
export function feedbackFirstLine(text: string): string {
  const line = text.split('\n').map((l) => l.trim()).find(Boolean) ?? '';
  return Array.from(line).slice(0, 200).join('');
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
    ['CPU type', ctx.arch],
    ['An ID for this computer (not its name)', ctx.machineId],
  ];
  if (ctx.paneId) rows.push(["Pane's ID", ctx.paneId]);
  return rows.filter(([, v]) => !!v);
}

/** The plain-text report for Copy report and Email it: exactly the redacted
 *  preview, nothing the form doesn't show. */
export function feedbackReportText(type: FeedbackType, p: FeedbackPreview): string {
  const parts = [`SoluTerminal feedback · ${FEEDBACK_TYPE_LABEL[type]}`];
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

/** A mailto link to the team with the report as its body, cut to fit. */
export function feedbackMailto(text: string): { href: string; cut: boolean } {
  const subject = encodeURIComponent('SoluTerminal feedback');
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
 *  link itself. */
export async function openMailDraft(href: string) {
  if (tauriInvoke()) await openInSystemBrowser(href);
  else window.location.href = href;
}

/** The report behind CloudGate's "Account not ready" Copy report / Email it:
 *  the app isn't loaded there, so it carries only what the page knows. */
export function accountNotReadyReportText(webVersion: string): string {
  return [
    `SoluTerminal feedback · ${FEEDBACK_TYPE_LABEL.bug}`,
    "Account not ready: this account has no SoluTerminal profile.",
    ['App and system details', `Web version: ${webVersion}`].join('\n'),
  ].join('\n\n');
}

export const FEEDBACK_SEND_ERROR =
  "We couldn't send this just now. Your report is still here. Try again in a minute, or copy it and email us.";
export const FEEDBACK_PREVIEW_ERROR =
  "We couldn't show this preview, so it won't be sent. Tick it again to retry.";
export const FEEDBACK_LIMIT_ERROR =
  "You've reached today's limit for reports. Try again tomorrow, or copy this report and email it to us.";

/** A failed send in plain words: never an HTTP status line. */
export function feedbackSendError(e: unknown): string {
  // ApiError carries the status; read it structurally so this module stays
  // free of the API client.
  return (e as { status?: unknown } | null)?.status === 429 ? FEEDBACK_LIMIT_ERROR : FEEDBACK_SEND_ERROR;
}
