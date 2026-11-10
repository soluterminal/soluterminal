import { Component, type ErrorInfo, type ReactNode } from 'react';
import { openFeedback } from '../lib/feedbackReport';

// ErrorBoundary keeps a render-time crash inside the pane tree from
// blanking the entire app. The realistic failure modes:
//   - xterm.js / canvas init throwing on a browser without WebGL
//     support (or after the WebGL context limit is hit, which has
//     bitten this codebase before — see feedback_webgl_context_limit)
//   - a Zustand selector returning a fresh array each call,
//     triggering useSyncExternalStore's "infinite render" error
//     (#185, also previously bitten)
//   - any unhandled exception from a deep child during render
//
// React error boundaries only catch render-phase errors, NOT async
// errors from event handlers, timers, or promises — those still
// reach window.onerror. This boundary is a backstop, not a safety
// net for everything.

type Props = {
  children: ReactNode;
  // label is only used in the fallback UI + console message so we can
  // tell which boundary fired when there are several in the tree.
  label?: string;
  title?: string;
  // reportAs names the pane type ("Codex", "Terminal") on a per-pane
  // boundary; it adds "Report this", which opens the feedback form filled in
  // with the pane type and the error.
  reportAs?: string;
};

type State = {
  error: Error | null;
};

export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    // eslint-disable-next-line no-console
    console.error(`[ErrorBoundary${this.props.label ? ' ' + this.props.label : ''}]`, error, info.componentStack);
  }

  reset = () => {
    this.setState({ error: null });
  };

  report = () => {
    const error = this.state.error;
    const message = error ? error.message || String(error) : '';
    openFeedback({ type: 'bug', body: `The ${this.props.reportAs} pane stopped with an error: ${message}` });
  };

  render() {
    if (this.state.error) {
      return (
        <div role="alert" className="error-boundary-state">
          <div className="error-boundary-title">{this.props.title ?? 'Panel failed to render'}</div>
          <div className="error-boundary-copy">
            This usually clears after a retry. If it comes back, reload the app so the pane tree can remount cleanly.
          </div>
          <div className="error-boundary-detail">
            {this.state.error.message || String(this.state.error)}
          </div>
          <div className="error-boundary-actions">
            <button type="button" className="secondary" onClick={this.reset}>
              Try again
            </button>
            {this.props.reportAs && (
              <button type="button" className="secondary" onClick={this.report}>
                Report this
              </button>
            )}
          </div>
        </div>
      );
    }
    return this.props.children;
  }
}
