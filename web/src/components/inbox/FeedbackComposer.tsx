import { useEffect, useRef, useState } from 'react';
import { api, type FeedbackContext, type FeedbackReq, type FeedbackStatus } from '../../api';
import { SITE_PRIVACY_FEEDBACK_URL } from '../../lib/siteLinks';
import {
  FEEDBACK_MAIN_FIELD,
  composeFeedbackBody,
  feedbackDetailRows,
  feedbackMailto,
  feedbackReportText,
  feedbackFirstLine,
  feedbackSendError,
  openMailDraft,
  FEEDBACK_PREVIEW_ERROR,
  type FeedbackPrefill,
  type FeedbackType,
} from '../../lib/feedbackReport';
import { openInSystemBrowser } from '../../tauri-browser';
import { useStore } from '../../store';
import { useCloud } from '../../cloud';
import { useUnsentInput } from '../../lib/unsentInput';

// The feedback form, hosted by the inbox panel's Compose view (the old modal
// is retired; compose lives where the replies land). The client collects
// intent; the daemon redacts and the cloud files the issue.
//
// "What we'll send" shows exactly what leaves the computer: the details row
// and, when ticked, the pane text and logs as the daemon redacted them
// (POST /api/feedback/preview). Those previews are editable, and Send carries
// the edited text, so a line the user deleted is never sent. Signed out or
// unpaired, the same form offers Copy report and Email it instead of Send,
// built from the same redacted preview; nothing goes to our cloud then.
//
// Screenshots are hidden for everyone until private storage exists (ST-103
// D3); the daemon drops any a client sends.
//
// Drafts: the typed fields persist to localStorage on every change and
// restore on mount, so closing the panel mid-thought loses nothing. The ticks
// and previews are not drafted: a reopened form starts with pane text and logs
// off, so nothing is sent that the user hasn't seen (and trimmed) again.

const TYPE_OPTIONS: { value: FeedbackType; label: string; hint: string }[] = [
  { value: 'bug', label: 'Bug', hint: 'Something is broken or behaving wrong' },
  { value: 'idea', label: 'Idea', hint: 'A feature or improvement you want' },
  { value: 'question', label: 'Question', hint: 'You need help or clarification' },
];

const DRAFT_KEY = 'solu.feedbackDraft.v2';
// The v1 draft's fields carry over; its tick never did.
const DRAFT_KEY_V1 = 'solu.feedbackDraft.v1';
// "What we'll send" opens by itself the first time the form is used.
const PREVIEW_SEEN_KEY = 'solu.feedbackPreviewSeen.v1';

const webVersion = typeof __SOLU_WEB_VERSION__ === 'string' ? __SOLU_WEB_VERSION__ : 'dev';

interface Draft {
  type: FeedbackType;
  trying: string;
  title: string;
  body: string;
}

function loadDraft(): Draft | null {
  try {
    const raw = localStorage.getItem(DRAFT_KEY);
    if (raw) {
      const d = JSON.parse(raw) as Partial<Draft>;
      if (typeof d.title !== 'string' || typeof d.body !== 'string') return null;
      return { type: d.type ?? 'bug', trying: typeof d.trying === 'string' ? d.trying : '', title: d.title, body: d.body };
    }
    const old = localStorage.getItem(DRAFT_KEY_V1);
    if (!old) return null;
    const d = JSON.parse(old) as { type?: FeedbackType; title?: string; body?: string };
    localStorage.removeItem(DRAFT_KEY_V1);
    if (typeof d.title !== 'string' || typeof d.body !== 'string') return null;
    return { type: d.type ?? 'bug', trying: '', title: d.title, body: d.body };
  } catch {
    return null;
  }
}

function initialState(prefill?: FeedbackPrefill): Draft {
  const d: Draft = loadDraft() ?? { type: 'bug', trying: '', title: '', body: '' };
  if (!prefill) return d;
  // An error screen's details join whatever the user had already started.
  return {
    ...d,
    type: prefill.type ?? d.type,
    trying: prefill.trying ?? d.trying,
    body: !prefill.body || d.body.includes(prefill.body) ? d.body
      : d.body.trim() ? `${d.body.trim()}\n\n${prefill.body}` : prefill.body,
  };
}

function readFlag(key: string): boolean {
  try { return localStorage.getItem(key) === '1'; } catch { return false; }
}

function paneLabel(p: { id: string; title: string; titleOverride?: string }): string {
  return p.titleOverride || p.title || p.id;
}

export function FeedbackComposer({
  onDone,
  onCancel,
  prefill,
}: {
  onDone: () => void;
  onCancel: () => void;
  prefill?: FeedbackPrefill;
}) {
  const toast = useStore((s) => s.toast);
  const focusedPaneId = useStore((s) => s.focusedPaneId);
  const panes = useStore((s) => s.panes);

  const start = useRef<Draft | null>(null);
  if (start.current === null) start.current = initialState(prefill);
  const [type, setType] = useState<FeedbackType>(start.current.type);
  const [trying, setTrying] = useState(start.current.trying);
  const [title, setTitle] = useState(start.current.title);
  const [body, setBody] = useState(start.current.body);
  useUnsentInput(body);
  // Off on every open: pane text and logs go only when the user ticks them
  // and the preview shows what they captured.
  const [includePane, setIncludePane] = useState(false);
  const [includeLogs, setIncludeLogs] = useState(false);
  const [paneId, setPaneId] = useState<string>(() => focusedPaneId ?? Object.keys(panes)[0] ?? '');

  const [sectionOpen, setSectionOpen] = useState(() => !readFlag(PREVIEW_SEEN_KEY));
  const [showDetails, setShowDetails] = useState(false);
  const [showPane, setShowPane] = useState(true);
  const [showLogs, setShowLogs] = useState(true);
  const [context, setContext] = useState<FeedbackContext | null>(null);
  // The previewed text: undefined while it loads, then what will be sent.
  const [paneText, setPaneText] = useState<string | undefined>(undefined);
  const [logsText, setLogsText] = useState<string | undefined>(undefined);
  // Whether the capture itself was empty: deleting every line of a preview
  // keeps the box, so the user can still see that nothing will be sent.
  const [paneEmpty, setPaneEmpty] = useState(false);
  const [logsEmpty, setLogsEmpty] = useState(false);
  const paneReq = useRef(0);
  const logsReq = useRef(0);

  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  // A failed send says "copy it and email us", so the form then offers the
  // same Copy report / Email it as a computer that can't send at all.
  const [sendFailed, setSendFailed] = useState(false);
  // Checked before the user types, so a computer that can't send feedback
  // offers Copy report / Email it from the start instead of failing on Send.
  const [status, setStatus] = useState<FeedbackStatus | null>(null);
  const canSignIn = useCloud((s) => (
    s.mode === 'cloud' && s.isLocal && s.accountOptional && (s.withoutAccount || !s.session)
  ));

  useEffect(() => {
    let mounted = true;
    void api
      .feedbackStatus()
      .then((s) => { if (mounted) setStatus(s); })
      .catch(() => { /* unknown: keep Send; a failure says so in plain words */ });
    void api
      .feedbackPreview({ title: '', body: '', clientVersion: webVersion })
      .then((p) => { if (mounted) setContext(p.context); })
      .catch(() => { /* the summary line still says what is sent */ });
    try { localStorage.setItem(PREVIEW_SEEN_KEY, '1'); } catch { /* best effort */ }
    return () => { mounted = false; };
  }, []);

  // Capture the pane text when it is ticked or another pane is picked.
  useEffect(() => {
    if (!includePane || !paneId) { setPaneText(undefined); return; }
    const req = ++paneReq.current;
    setPaneText(undefined);
    void api
      .feedbackPreview({ title: '', body: '', paneId, includeSnapshot: true, includeLogs: false })
      .then((p) => { if (paneReq.current === req) { setPaneEmpty(!p.snapshot); setPaneText(p.snapshot); } })
      // Fail closed: a row whose preview can't be shown is unticked, so nothing
      // the user hasn't seen can go (an older daemon would capture it anew).
      .catch(() => { if (paneReq.current === req) { setIncludePane(false); setErr(FEEDBACK_PREVIEW_ERROR); } });
  }, [includePane, paneId]);

  useEffect(() => {
    if (!includeLogs) { setLogsText(undefined); return; }
    const req = ++logsReq.current;
    setLogsText(undefined);
    void api
      .feedbackPreview({ title: '', body: '', includeSnapshot: false, includeLogs: true })
      .then((p) => { if (logsReq.current === req) { setLogsEmpty(!p.logs); setLogsText(p.logs); } })
      .catch(() => { if (logsReq.current === req) { setIncludeLogs(false); setErr(FEEDBACK_PREVIEW_ERROR); } });
  }, [includeLogs]);

  useEffect(() => {
    const d: Draft = { type, trying, title, body };
    try {
      if (!title && !body && !trying) localStorage.removeItem(DRAFT_KEY);
      else localStorage.setItem(DRAFT_KEY, JSON.stringify(d));
    } catch { /* quota — drafts are best-effort */ }
  }, [type, trying, title, body]);

  const paneOn = includePane && !!paneId;
  const loading = (paneOn && paneText === undefined) || (includeLogs && logsText === undefined);
  const unavailable = status && !status.available ? status.reason ?? 'signed_out' : null;
  const main = FEEDBACK_MAIN_FIELD[type];

  // The request a submit (or Copy / Email) makes: the previewed text, never
  // a fresh capture the user hasn't seen.
  function request(): FeedbackReq {
    return {
      type,
      // Empty: the first line of the main field (not of the trying-to-do line).
      title: title.trim() || feedbackFirstLine(body),
      body: composeFeedbackBody(trying, body),
      includeSnapshot: paneOn,
      includeLogs,
      clientVersion: webVersion,
      ...(paneOn ? { paneId, snapshot: paneText ?? '' } : {}),
      ...(includeLogs ? { logs: logsText ?? '' } : {}),
    };
  }

  function validate(): boolean {
    if (!body.trim()) { setErr(main.empty); return false; }
    if (loading) return false;
    return true;
  }

  async function submit(e?: React.FormEvent) {
    e?.preventDefault();
    if (unavailable || !validate()) return;
    setErr(null);
    setNotice(null);
    setSubmitting(true);
    try {
      const res = await api.submitFeedback(request());
      toast(
        res.number ? `Thanks, we've got it. Report #${res.number} is with the team.` : "Thanks, we've got it.",
        { kind: 'success', icon: '✓' },
      );
      try { localStorage.removeItem(DRAFT_KEY); } catch { /* best effort */ }
      onDone();
    } catch (e) {
      setErr(feedbackSendError(e));
      setSendFailed(true);
    } finally {
      setSubmitting(false);
    }
  }

  // The plain-text report for Copy report / Email it, redacted by the daemon
  // exactly as a send would be.
  async function reportText(): Promise<string | null> {
    if (!validate()) return null;
    setErr(null);
    try {
      const req = request();
      return feedbackReportText(type, await api.feedbackPreview(req));
    } catch {
      setErr(feedbackSendError(null));
      return null;
    }
  }

  async function copyReport() {
    const text = await reportText();
    if (text == null) return;
    try {
      await navigator.clipboard.writeText(text);
      setNotice(
        'Copied. Paste it into an email to hello@soluterminal.com, or into a GitHub issue (those are public, so check it first).',
      );
    } catch {
      setErr("Couldn't copy. Select the text in the preview and copy it, or use Email it.");
    }
  }

  async function emailReport() {
    const text = await reportText();
    if (text == null) return;
    await openMailDraft(feedbackMailto(text).href);
  }

  const signIn = () => {
    onDone();
    useCloud.getState().requestSignIn();
  };

  const paneIds = Object.keys(panes);
  if (paneId && !panes[paneId] && paneIds.length) paneIds.unshift(paneId);
  const detailRows = context ? feedbackDetailRows({ ...context, paneId: paneOn ? paneId : '' }) : [];

  return (
    <form className="inbox-compose" onSubmit={submit} noValidate>
      <div>
        <span id="fb-type-label" className="inbox-compose-label">Type</span>
        <div className="dialog-chip-row" role="group" aria-labelledby="fb-type-label">
          {TYPE_OPTIONS.map((o) => (
            <button
              key={o.value}
              type="button"
              onClick={() => setType(o.value)}
              className={`inbox-filter${type === o.value ? ' is-active' : ''}`}
              aria-pressed={type === o.value}
              title={o.hint}
            >{o.label}</button>
          ))}
        </div>
      </div>

      <div>
        <label htmlFor="fb-trying">What were you trying to do?</label>
        <input
          id="fb-trying"
          value={trying}
          onChange={(e) => setTrying(e.target.value)}
          placeholder="For example: start Codex in a WSL pane"
          className="dialog-control"
          maxLength={300}
        />
      </div>

      <div>
        <label htmlFor="fb-body">{main.label}</label>
        <textarea
          id="fb-body"
          value={body}
          onChange={(e) => { setBody(e.target.value); if (err === main.empty) setErr(null); }}
          placeholder={main.placeholder}
          className="dialog-control"
          rows={6}
          required
          aria-required="true"
          aria-invalid={err === main.empty || undefined}
          aria-describedby={err === main.empty ? 'fb-error' : undefined}
          autoFocus
        />
      </div>

      <div>
        <label htmlFor="fb-title">Title (optional)</label>
        <input
          id="fb-title"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          placeholder="If you leave this empty, we'll use the first line of what you wrote."
          className="dialog-control"
          maxLength={200}
        />
      </div>

      <section className="inbox-send-preview" aria-labelledby="fb-send-heading">
        <button
          type="button"
          id="fb-send-heading"
          className="inbox-send-preview-toggle"
          aria-expanded={sectionOpen}
          aria-controls="fb-send-body"
          onClick={() => setSectionOpen((v) => !v)}
        >What we'll send</button>
        {sectionOpen && (
          <div id="fb-send-body" className="inbox-send-preview-body">
            <div className="inbox-send-row">
              <div className="inbox-send-row-head">
                <span className="inbox-send-row-title">App and system details · always sent</span>
                <button type="button" className="vp-link" aria-expanded={showDetails} onClick={() => setShowDetails((v) => !v)}>
                  {showDetails ? 'Hide' : 'Show'}
                </button>
              </div>
              <div className="dialog-help">
                Versions, operating system, platform, backend, CPU type and an ID for this computer (not its name).
              </div>
              {showDetails && detailRows.length > 0 && (
                <dl className="inbox-send-details">
                  {detailRows.map(([k, v]) => (
                    <div key={k}><dt>{k}</dt><dd>{v}</dd></div>
                  ))}
                </dl>
              )}
            </div>

            <div className="inbox-send-row">
              <div className="inbox-send-row-head">
                <label className="inbox-send-check">
                  <input type="checkbox" checked={includePane} onChange={(e) => { setIncludePane(e.target.checked); setShowPane(true); }} />
                  <span>Recent text from a pane (last 40 lines), with the pane&apos;s ID</span>
                </label>
                {paneOn && (
                  <button type="button" className="vp-link" aria-expanded={showPane} onClick={() => setShowPane((v) => !v)}>
                    {showPane ? 'Hide' : 'Show'}
                  </button>
                )}
              </div>
              {includePane && paneIds.length > 0 && (
                <div className="inbox-send-pane">
                  <label htmlFor="fb-pane">Pane</label>
                  <select id="fb-pane" className="dialog-control" value={paneId} onChange={(e) => setPaneId(e.target.value)}>
                    {paneIds.map((id) => (
                      <option key={id} value={id}>{panes[id] ? paneLabel(panes[id]) : id}</option>
                    ))}
                  </select>
                </div>
              )}
              {paneOn && showPane && (
                paneText === undefined ? (
                  <div className="dialog-help" role="status">Loading…</div>
                ) : paneEmpty ? (
                  <div className="dialog-help">This pane has no recent text.</div>
                ) : (
                  <textarea
                    className="dialog-control inbox-send-text"
                    aria-label="Recent text from a pane"
                    value={paneText}
                    onChange={(e) => setPaneText(e.target.value)}
                    rows={8}
                    spellCheck={false}
                  />
                )
              )}
            </div>

            <div className="inbox-send-row">
              <div className="inbox-send-row-head">
                <label className="inbox-send-check">
                  <input type="checkbox" checked={includeLogs} onChange={(e) => { setIncludeLogs(e.target.checked); setShowLogs(true); }} />
                  <span>Recent app logs</span>
                </label>
                {includeLogs && (
                  <button type="button" className="vp-link" aria-expanded={showLogs} onClick={() => setShowLogs((v) => !v)}>
                    {showLogs ? 'Hide' : 'Show'}
                  </button>
                )}
              </div>
              {includeLogs && showLogs && (
                logsText === undefined ? (
                  <div className="dialog-help" role="status">Loading…</div>
                ) : logsEmpty ? (
                  <div className="dialog-help">No recent app logs.</div>
                ) : (
                  <textarea
                    className="dialog-control inbox-send-text"
                    aria-label="Recent app logs"
                    value={logsText}
                    onChange={(e) => setLogsText(e.target.value)}
                    rows={8}
                    spellCheck={false}
                  />
                )
              )}
            </div>

            <div className="dialog-help">
              We remove tokens, keys, email addresses and home-folder paths we recognise. Check the preview and delete
              anything else you don&apos;t want to send.
            </div>
          </div>
        )}
      </section>

      {unavailable ? (
        <p className="dialog-help" role="status">
          {unavailable === 'unpaired'
            ? "This computer isn't connected to your account, so this can't be sent from here. Copy the report or email it to us."
            : "You're not signed in, so this can't be sent from here. Copy the report or email it to us. Sign in to send it and get replies in the app."}
        </p>
      ) : (
        <p className="dialog-help">
          Only what&apos;s shown above is sent. It goes privately to the SoluTerminal team, linked to your account so we
          can reply here, and an AI model helps us sort it.{' '}
          <a
            href={SITE_PRIVACY_FEEDBACK_URL}
            onClick={(e) => { e.preventDefault(); void openInSystemBrowser(SITE_PRIVACY_FEEDBACK_URL, e.nativeEvent); }}
          >How we handle feedback</a>
        </p>
      )}

      {notice && <div className="dialog-help" role="status">{notice}</div>}
      {err && <div id="fb-error" className="dialog-error" role="alert">{err}</div>}

      <div className="dialog-actions">
        <button type="button" onClick={onCancel} disabled={submitting}>Cancel</button>
        {unavailable ? (
          <>
            {unavailable === 'signed_out' && canSignIn && (
              <button type="button" onClick={signIn}>Sign in</button>
            )}
            <button type="button" onClick={() => void copyReport()} disabled={loading}>Copy report</button>
            <button type="button" className="primary" onClick={() => void emailReport()} disabled={loading}>Email it</button>
          </>
        ) : (
          <>
            {sendFailed && (
              <>
                <button type="button" onClick={() => void copyReport()} disabled={submitting || loading}>Copy report</button>
                <button type="button" onClick={() => void emailReport()} disabled={submitting || loading}>Email it</button>
              </>
            )}
            <button type="submit" className="primary" disabled={submitting || loading}>
              {submitting ? 'Sending…' : 'Send'}
            </button>
          </>
        )}
      </div>
    </form>
  );
}
