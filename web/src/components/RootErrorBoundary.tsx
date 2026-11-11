import { Component, type ErrorInfo, type ReactNode } from 'react';
import { api, type FeedbackReq } from '../api';
import { copyToClipboard } from '../lib/clipboard';
import {
  FEEDBACK_COPIED_NOTE,
  FEEDBACK_COPY_ERROR,
  FEEDBACK_EMAIL_CUT_NOTE,
  crashReportFallbackText,
  feedbackMailto,
  feedbackReportText,
  openMailDraft,
} from '../lib/feedbackReport';

// RootErrorBoundary is the last line of defence around the whole window
// (main.tsx). The pane grid has its own ErrorBoundary; anything else that
// throws while rendering (top bar, sidebar, a dialog, CloudGate, a lazy chunk
// that fails to load after an update) used to unmount the whole tree and
// leave a blank window. Now it shows "Something went wrong" with Reload, and
// "Send report" files the crash through the same feedback route as
// Help › Send feedback (POST /api/feedback, daemon/feedback.go).
//
// The screen lists what a report carries before anything is sent, and Show
// opens the exact text: the daemon's redacted preview (local only, the same
// one the feedback form shows), or, when the daemon can't be reached, text
// built here that the person is asked to check. Copy report and Email it use
// that same text. Strings: ST-167 in the in-app feedback wording file.
//
// The fallback deliberately uses nothing but this component, the API client
// and pure helpers: the store, toasts and dialogs belong to the tree that
// just crashed.

const webVersion = typeof __SOLU_WEB_VERSION__ === 'string' ? __SOLU_WEB_VERSION__ : 'dev';
const MAX_TRACE = 4000;

type SendState =
  | { kind: 'idle' }
  | { kind: 'sending' }
  | { kind: 'sent'; text: string }
  | { kind: 'failed'; text: string };

/** The text behind Show, Copy report and Email it. `checked` is false when
 *  the daemon could not redact it and the page built it instead. */
type Report = { text: string; checked: boolean };

type Props = { children: ReactNode };
type State = {
  error: Error | null;
  componentStack: string;
  send: SendState;
  /** Why this computer can't send; null while it can, or isn't known yet. */
  unavailable: 'signed_out' | 'unpaired' | null;
  report: Report | null;
  showReport: boolean;
  /** How the last Copy report or Email it went. */
  note: string;
};

// What a crash report carries, in the order the body lists it. The last row
// is what the daemon adds to every report (daemon/feedback.go feedbackContext).
const SEND_ROWS = [
  'The error message',
  'Where in the app it happened',
  'Your browser and app version',
  'Technical details of the error (stack trace)',
];

function clip(text: string): string {
  return text.length > MAX_TRACE ? text.slice(0, MAX_TRACE) + '\n…' : text;
}

/** The crash report body: what broke and where, never pane contents. */
export function crashReportBody(error: Error, componentStack: string): string {
  return [
    'The app window crashed and showed the "Something went wrong" screen.',
    '',
    `Error: ${error.message || String(error)}`,
    `Version: ${webVersion}`,
    `Browser: ${typeof navigator !== 'undefined' ? navigator.userAgent : 'unknown'}`,
    '',
    'Stack:',
    clip(error.stack || '(none)'),
    '',
    'Component stack:',
    clip(componentStack.trim() || '(none)'),
  ].join('\n');
}

/** The crash report as sent: no pane text, no logs. */
export function crashReportRequest(error: Error, componentStack: string): FeedbackReq {
  const message = (error.message || String(error)).replace(/\s+/g, ' ').trim();
  return {
    type: 'bug',
    title: `App crashed: ${message.slice(0, 80) || 'unknown error'}`,
    body: crashReportBody(error, componentStack),
    includeSnapshot: false,
    includeLogs: false,
    clientVersion: webVersion,
  };
}

export class RootErrorBoundary extends Component<Props, State> {
  state: State = {
    error: null, componentStack: '', send: { kind: 'idle' }, unavailable: null, report: null, showReport: false, note: '',
  };

  private reportReady: Promise<Report> | null = null;

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    const componentStack = info.componentStack ?? '';
    this.setState({ componentStack });
    // eslint-disable-next-line no-console
    console.error('[RootErrorBoundary]', error, componentStack);
    this.prepare(error, componentStack);
  }

  // Asks the local daemon, and nothing else, what a send would carry and
  // whether this computer can send. Neither call sends the report.
  private prepare(error: Error, componentStack: string) {
    const { type: _type, ...req } = crashReportRequest(error, componentStack);
    this.reportReady = Promise.resolve()
      .then(() => api.feedbackPreview(req))
      .then((preview): Report => ({ text: feedbackReportText('bug', preview), checked: true }))
      .catch((): Report => ({ text: crashReportFallbackText(req.title, req.body, webVersion), checked: false }));
    // Unchecked text opens by itself: it is read before it is copied or emailed.
    void this.reportReady.then((report) => this.setState((prev) => ({ report, showReport: prev.showReport || !report.checked })));
    void Promise.resolve()
      .then(() => api.feedbackStatus())
      .then((status) => { if (status && !status.available) this.setUnavailable(status.reason); })
      .catch(() => { /* unknown: Send stays, and a failure offers Copy report and Email it */ });
  }

  private setUnavailable(reason: string | undefined) {
    this.setState({ unavailable: reason === 'unpaired' ? 'unpaired' : 'signed_out' });
  }

  reload = () => {
    window.location.reload();
  };

  sendReport = async () => {
    const { error, componentStack, send } = this.state;
    if (!error || send.kind === 'sending' || send.kind === 'sent') return;
    this.setState({ send: { kind: 'sending' }, note: '' });
    try {
      // Send carries what the daemon builds, so it runs only when Show holds
      // the daemon's own text; page-built text goes by Copy report or Email it.
      if (!(await this.reportReady)?.checked) {
        this.setState({ send: { kind: 'idle' } });
        return;
      }
      const status = await api.feedbackStatus().catch(() => null);
      if (status && !status.available) {
        this.setState({ send: { kind: 'idle' } });
        this.setUnavailable(status.reason);
        return;
      }
      const res = await api.submitFeedback(crashReportRequest(error, componentStack));
      this.setState({
        send: { kind: 'sent', text: res.number ? `Report sent (#${res.number}). Thank you.` : 'Report sent. Thank you.' },
      });
    } catch {
      this.setState({
        // A plain sentence only: the cause is an HTTP status line or a
        // network error, which tells the person nothing they can act on.
        send: { kind: 'failed', text: "Couldn't send the report. Use Copy report or Email it instead." },
      });
    }
  };

  copyReport = async () => {
    if (!this.reportReady) return;
    const { text } = await this.reportReady;
    this.setState({
      note: (await copyToClipboard(text)) ? FEEDBACK_COPIED_NOTE : FEEDBACK_COPY_ERROR,
    });
  };

  emailReport = async () => {
    if (!this.reportReady) return;
    const { href, cut } = feedbackMailto((await this.reportReady).text, 'bug', webVersion);
    await openMailDraft(href);
    this.setState({ note: cut ? FEEDBACK_EMAIL_CUT_NOTE : '' });
  };

  render() {
    const { error, send, unavailable, report, showReport, note } = this.state;
    if (!error) return this.props.children;
    // The daemon gave no preview: the text shown was built by this page and is
    // not what a send would carry, so Send is not offered beside it.
    const pageBuilt = !!report && !report.checked;
    return (
      <div className="root-error-shell">
        <div role="alert" className="error-boundary-state root-error-state">
          <div className="error-boundary-title">Something went wrong</div>
          <div className="error-boundary-copy">
            SoluTerminal hit an error and couldn't show this window. Your panes and agents keep running in the background. Reload to get back to them.
          </div>
          <div className="error-boundary-detail">{error.message || String(error)}</div>
          <div className="root-error-send">
            <div className="error-boundary-title" id="root-error-send-title">What we'll send</div>
            <ul aria-labelledby="root-error-send-title">
              {SEND_ROWS.map((row) => <li key={row}>{row}</li>)}
              <li>
                App and system details
                <div className="error-boundary-copy">
                  Versions, operating system, platform, backend, CPU type and an ID for this computer (not its name).
                </div>
              </li>
            </ul>
            <div className="error-boundary-copy">
              Only what's listed above is sent.{' '}
              <button
                type="button"
                className="vp-link"
                aria-expanded={showReport}
                aria-controls="root-error-report"
                aria-label={`${showReport ? 'Hide' : 'Show'} the report that would be sent`}
                onClick={() => this.setState({ showReport: !showReport })}
              >{showReport ? 'Hide' : 'Show'}</button>
            </div>
            {showReport && (
              <div id="root-error-report">
                {report && !report.checked && (
                  <div className="error-boundary-copy">
                    Check this text before you send it: we couldn't run our usual check for private details.
                  </div>
                )}
                <pre className="error-boundary-detail root-error-report">{report ? report.text : 'Loading…'}</pre>
              </div>
            )}
          </div>
          <div className="root-error-actions">
            <button type="button" className="primary" onClick={this.reload}>Reload</button>
            {!unavailable && !pageBuilt && (
              <button
                type="button"
                className="secondary"
                onClick={() => { void this.sendReport(); }}
                disabled={send.kind === 'sending' || send.kind === 'sent'}
              >{send.kind === 'sending' ? 'Sending…' : 'Send report'}</button>
            )}
            {(unavailable || pageBuilt || send.kind === 'failed') && (
              <>
                <button type="button" className="secondary" onClick={() => { void this.copyReport(); }}>Copy report</button>
                <button type="button" className="secondary" onClick={() => { void this.emailReport(); }}>Email it</button>
              </>
            )}
          </div>
          {unavailable && (
            <div className="error-boundary-copy" role="status">
              {unavailable === 'unpaired'
                ? "This computer isn't connected to your account, so this can't be sent from here. Copy the report or email it to us."
                : "You're not signed in, so this can't be sent from here. Copy the report or email it to us."}
            </div>
          )}
          {pageBuilt && !unavailable && (
            <div className="error-boundary-copy" role="status">
              This can't be sent from here right now. Copy the report or email it to us.
            </div>
          )}
          {(send.kind === 'sent' || send.kind === 'failed') && (
            <div className="error-boundary-copy" role="status">{send.text}</div>
          )}
          {note && <div className="error-boundary-copy" role="status">{note}</div>}
        </div>
      </div>
    );
  }
}
