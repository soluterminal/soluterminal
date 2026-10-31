import { Component, type ErrorInfo, type ReactNode } from 'react';
import { api } from '../api';
import { copyToClipboard } from '../lib/clipboard';

// RootErrorBoundary is the last line of defence around the whole window
// (main.tsx). The pane grid has its own ErrorBoundary; anything else that
// throws while rendering (top bar, sidebar, a dialog, CloudGate, a lazy chunk
// that fails to load after an update) used to unmount the whole tree and
// leave a blank window. Now it shows "Something went wrong" with Reload, and
// "Send report" files the crash through the same feedback route as
// Help › Send feedback (POST /api/feedback, daemon/feedback.go).
//
// The fallback deliberately uses nothing but this component and the API
// client: the store, toasts and dialogs belong to the tree that just crashed.

const webVersion = typeof __SOLU_WEB_VERSION__ === 'string' ? __SOLU_WEB_VERSION__ : 'dev';
const MAX_TRACE = 4000;

type SendState =
  | { kind: 'idle' }
  | { kind: 'sending' }
  | { kind: 'sent'; text: string }
  | { kind: 'failed'; text: string };

type Props = { children: ReactNode };
type State = { error: Error | null; componentStack: string; send: SendState; copied: boolean };

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

export class RootErrorBoundary extends Component<Props, State> {
  state: State = { error: null, componentStack: '', send: { kind: 'idle' }, copied: false };

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    this.setState({ componentStack: info.componentStack ?? '' });
    // eslint-disable-next-line no-console
    console.error('[RootErrorBoundary]', error, info.componentStack);
  }

  reload = () => {
    window.location.reload();
  };

  sendReport = async () => {
    const { error, componentStack, send } = this.state;
    if (!error || send.kind === 'sending' || send.kind === 'sent') return;
    this.setState({ send: { kind: 'sending' } });
    try {
      const status = await api.feedbackStatus().catch(() => null);
      if (status && !status.available) {
        this.setState({
          send: {
            kind: 'failed',
            text: status.reason === 'unpaired'
              ? "Reports can't be sent from this computer yet: it isn't connected to your account. Copy the details and send them to support instead."
              : 'Sending a report needs a free account. Copy the details and send them to support instead.',
          },
        });
        return;
      }
      const message = (error.message || String(error)).replace(/\s+/g, ' ').trim();
      const res = await api.submitFeedback({
        type: 'bug',
        title: `App crashed: ${message.slice(0, 80) || 'unknown error'}`,
        body: crashReportBody(error, componentStack),
        includeSnapshot: false,
        clientVersion: webVersion,
      });
      this.setState({
        send: { kind: 'sent', text: res.number ? `Report sent (#${res.number}). Thank you.` : 'Report sent. Thank you.' },
      });
    } catch (e) {
      this.setState({
        send: { kind: 'failed', text: `Couldn't send the report: ${(e as Error).message || 'unknown error'}. Copy the details instead.` },
      });
    }
  };

  copyDetails = async () => {
    const { error, componentStack } = this.state;
    if (!error) return;
    if (await copyToClipboard(crashReportBody(error, componentStack))) {
      this.setState({ copied: true });
    }
  };

  render() {
    const { error, send, copied } = this.state;
    if (!error) return this.props.children;
    return (
      <div className="root-error-shell">
        <div role="alert" className="error-boundary-state root-error-state">
          <div className="error-boundary-title">Something went wrong</div>
          <div className="error-boundary-copy">
            SoluTerminal hit an error and couldn't show this window. Your panes and agents keep running in the background. Reload to get back to them.
          </div>
          <div className="error-boundary-detail">{error.message || String(error)}</div>
          <div className="root-error-actions">
            <button type="button" className="primary" onClick={this.reload}>Reload</button>
            <button
              type="button"
              className="secondary"
              onClick={() => { void this.sendReport(); }}
              disabled={send.kind === 'sending' || send.kind === 'sent'}
              title="Sends the error message and where it happened to the SoluTerminal team, through Send feedback. No pane contents are included."
            >{send.kind === 'sending' ? 'Sending…' : 'Send report'}</button>
            {send.kind === 'failed' && (
              <button type="button" className="secondary" onClick={() => { void this.copyDetails(); }}>
                {copied ? 'Copied' : 'Copy details'}
              </button>
            )}
          </div>
          {(send.kind === 'sent' || send.kind === 'failed') && (
            <div className="error-boundary-copy" role="status">{send.text}</div>
          )}
        </div>
      </div>
    );
  }
}
