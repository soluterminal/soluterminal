import { describe, expect, it } from 'vitest';
import {
  FEEDBACK_MAILTO_BODY_MAX,
  FEEDBACK_MAILTO_CUT_NOTE,
  composeFeedbackBody,
  feedbackDetailRows,
  feedbackFirstLine,
  feedbackMailto,
  feedbackReportText,
  feedbackSendError,
  FEEDBACK_LIMIT_ERROR,
  FEEDBACK_SEND_ERROR,
} from './feedbackReport';
import type { FeedbackContext } from '../api';

const CTX: FeedbackContext = {
  appVersion: '0.0.1241', daemonVersion: '0.0.1241', clientVersion: '0.0.1241', shellVersion: '',
  shellPlatform: 'windows', backendID: 'wsl-ubuntu', platform: 'windows', os: 'linux', arch: 'amd64',
  machineId: 'machine-7', paneId: '', captureKind: 'none',
};

const bodyOf = (href: string) => decodeURIComponent(href.split('&body=')[1]);

describe('feedbackMailto', () => {
  it('keeps a short report whole', () => {
    const { href, cut } = feedbackMailto('short report');
    expect(cut).toBe(false);
    expect(href).toBe('mailto:hello@soluterminal.com?subject=SoluTerminal%20feedback&body=short%20report');
  });

  it('cuts a long report to fit and says so', () => {
    const text = `${'line of pane text — ünïcode 🙂\n'.repeat(200)}END`;
    const { href, cut } = feedbackMailto(text);
    expect(cut).toBe(true);
    expect(href.split('&body=')[1].length).toBeLessThanOrEqual(FEEDBACK_MAILTO_BODY_MAX);
    const body = bodyOf(href);
    expect(body.endsWith(`\n\n${FEEDBACK_MAILTO_CUT_NOTE}`)).toBe(true);
    expect(text.startsWith(body.slice(0, body.length - FEEDBACK_MAILTO_CUT_NOTE.length - 2))).toBe(true);
  });
});

describe('feedback report text', () => {
  it('joins the trying-to-do line and the main field', () => {
    expect(composeFeedbackBody('  ', ' main ')).toBe('main');
    expect(composeFeedbackBody('open a pane', 'it hung')).toBe('What I was trying to do: open a pane\n\nit hung');
  });

  it('takes the title from the first written line', () => {
    expect(feedbackFirstLine('\n  first line  \nsecond')).toBe('first line');
    expect(feedbackFirstLine('')).toBe('');
    expect(Array.from(feedbackFirstLine('🙂'.repeat(300)))).toHaveLength(200);
  });

  it('lists every sent detail, the pane ID only with pane text', () => {
    const rows = feedbackDetailRows(CTX);
    expect(rows).toContainEqual(['An ID for this computer (not its name)', 'machine-7']);
    expect(rows.map(([k]) => k)).not.toContain("Pane's ID");
    expect(rows.map(([k]) => k)).not.toContain('Desktop app version'); // empty: not sent with a value
    expect(feedbackDetailRows({ ...CTX, paneId: 'pane_1' })).toContainEqual(["Pane's ID", 'pane_1']);
  });

  it('carries only the preview', () => {
    const text = feedbackReportText('idea', { title: 'T', body: 'B', snapshot: '', logs: 'L', context: CTX });
    expect(text).toBe([
      'SoluTerminal feedback · Idea', 'T', 'B',
      'App and system details\nApp version: 0.0.1241\nDaemon version: 0.0.1241\nWeb version: 0.0.1241\n'
        + 'Desktop app platform: windows\nPlatform: windows\nBackend: wsl-ubuntu\nOperating system: linux\n'
        + 'CPU type: amd64\nAn ID for this computer (not its name): machine-7',
      'Recent app logs\nL',
    ].join('\n\n'));
  });

  it('never shows an HTTP status', () => {
    expect(feedbackSendError({ status: 503, message: '503 Service Unavailable' })).toBe(FEEDBACK_SEND_ERROR);
    expect(feedbackSendError(new Error('Failed to fetch'))).toBe(FEEDBACK_SEND_ERROR);
    expect(feedbackSendError({ status: 429 })).toBe(FEEDBACK_LIMIT_ERROR);
  });
});
