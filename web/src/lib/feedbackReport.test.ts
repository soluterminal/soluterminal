import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  FEEDBACK_MAILTO_BODY_MAX,
  FEEDBACK_MAILTO_CUT_NOTE,
  composeFeedbackBody,
  crashReportFallbackText,
  feedbackDevice,
  feedbackFallbackReportText,
  openMailDraft,
  scrubLocalReport,
  feedbackDetailRows,
  feedbackFirstLine,
  feedbackMailto,
  feedbackReportText,
  feedbackSendError,
  FEEDBACK_LIMIT_ERROR,
  FEEDBACK_SEND_ERROR,
} from './feedbackReport';
import type { FeedbackContext } from '../api';
import { readFileSync } from 'node:fs';

// The shared corpus lives outside web/, so it is read when the tests run, not
// imported: the web image type-checks web/ on its own.
interface CorpusCase { name: string; input: string; want: string; web?: boolean }
const redactionCorpus = JSON.parse(
  readFileSync('../shared/feedbackredact/redacttest/testdata/corpus.json', 'utf8'),
) as CorpusCase[];

const CTX: FeedbackContext = {
  appVersion: '0.0.1241', daemonVersion: '0.0.1241', clientVersion: '0.0.1241', shellVersion: '',
  shellPlatform: 'windows', backendID: 'wsl-ubuntu', platform: 'windows', os: 'linux', arch: 'amd64',
  machineId: 'machine-7', paneId: '', captureKind: 'none',
};

const bodyOf = (href: string) => decodeURIComponent(href.split('&body=')[1]);

describe('feedbackMailto', () => {
  it('keeps a short report whole', () => {
    const { href, cut } = feedbackMailto('short report', 'bug', '0.0.1242');
    expect(cut).toBe(false);
    expect(href).toBe(
      `mailto:hello@soluterminal.com?subject=${encodeURIComponent('SoluTerminal feedback · Bug · web 0.0.1242')}&body=short%20report`,
    );
  });

  it('names the type and web version in the subject', () => {
    const subject = (href: string) => decodeURIComponent(href.split('?subject=')[1].split('&body=')[0]);
    expect(subject(feedbackMailto('x', 'idea', '0.0.1242').href)).toBe('SoluTerminal feedback · Idea · web 0.0.1242');
    expect(subject(feedbackMailto('x', 'question', 'dev').href)).toBe('SoluTerminal feedback · Question · web dev');
  });

  it('cuts a long report to fit and says so', () => {
    const text = `${'line of pane text — ünïcode 🙂\n'.repeat(200)}END`;
    const { href, cut } = feedbackMailto(text, 'bug', '0.0.1242');
    expect(cut).toBe(true);
    expect(href.split('&body=')[1].length).toBeLessThanOrEqual(FEEDBACK_MAILTO_BODY_MAX);
    const body = bodyOf(href);
    expect(body.endsWith(`\n\n${FEEDBACK_MAILTO_CUT_NOTE}`)).toBe(true);
    expect(text.startsWith(body.slice(0, body.length - FEEDBACK_MAILTO_CUT_NOTE.length - 2))).toBe(true);
  });
});

describe('feedback report text', () => {
  // The fallback has a smaller rule set; these cases cover its quoted values.
  for (const c of redactionCorpus.filter((c) => c.web)) {
    it(`shares corpus: ${c.name}`, () => {
      const got = scrubLocalReport(c.input);
      expect(got).toBe(c.want);
      expect(scrubLocalReport(got)).toBe(got);
    });
  }

  it('handles quoted assignments at the whitespace bound through the line end', () => {
    const prefix = `password${' '.repeat(64)}`;
    expect(scrubLocalReport(`${prefix}"fake\\" tail\nnext`))
      .toBe(`${prefix}"[redacted: token]\nnext`);
  });

  it('finishes quickly for a secret name followed by 8 KB of spaces and no value', () => {
    const text = `password${' '.repeat(8 * 1024)}`;
    const started = performance.now();
    const got = scrubLocalReport(text);
    const elapsed = performance.now() - started;
    expect(got).toBe(text);
    expect(elapsed).toBeLessThan(50);
  });

  it('finishes quickly for a secret name followed by 64 KB of spaces and an invalid value', () => {
    const text = `password${' '.repeat(64 * 1024)}z`;
    const started = performance.now();
    const got = scrubLocalReport(text);
    const elapsed = performance.now() - started;
    expect(got).toBe(text);
    expect(elapsed).toBeLessThan(100);
  });

  it('joins the trying-to-do line and the main field', () => {
    expect(composeFeedbackBody('  ', ' main ')).toBe('main');
    expect(composeFeedbackBody('open a pane', 'it hung')).toBe('What I was trying to do: open a pane\n\nit hung');
  });

  it('takes the title from the first written line', () => {
    expect(feedbackFirstLine('\n  first line  \nsecond')).toBe('first line');
    expect(feedbackFirstLine('')).toBe('');
    expect(feedbackFirstLine('🙂'.repeat(300))).toBe('🙂'.repeat(300));
    const quoted = `password="${'fake value '.repeat(30)}"`;
    expect(feedbackFirstLine(`\n${quoted}\nnext`)).toBe(quoted);
  });

  it('lists every sent detail, the pane ID only with pane text', () => {
    const rows = feedbackDetailRows(CTX);
    expect(rows).toContainEqual(['An ID for this computer (not its name)', 'machine-7']);
    expect(rows.map(([k]) => k)).not.toContain("Pane's ID");
    expect(rows.map(([k]) => k)).not.toContain('Desktop app version'); // empty: not sent with a value
    expect(feedbackDetailRows({ ...CTX, paneId: 'pane_1' })).toContainEqual(["Pane's ID", 'pane_1']);
  });

  // ST-165: what a newer daemon sends, in the order the form shows it.
  it('shows the Windows build, and the pane type and agent only with the pane', () => {
    const ctx: FeedbackContext = {
      ...CTX, appVersion: '', platform: '', os: 'Windows (WSL)', windowsBuild: '10.0.26100.4652',
      paneKind: 'Focus', paneAgent: 'Codex',
    };
    expect(feedbackDetailRows(ctx).map(([k]) => k)).toEqual([
      'Daemon version', 'Web version', 'Desktop app platform', 'Backend', 'Operating system', 'Windows build',
      'CPU type', 'An ID for this computer (not its name)',
    ]);
    expect(feedbackDetailRows({ ...ctx, paneId: 'pane_1' }).slice(-3)).toEqual([
      ["Pane's ID", 'pane_1'], ['Pane type', 'Focus'], ['Agent', 'Codex'],
    ]);
    expect(feedbackDetailRows({ ...ctx, paneId: 'pane_1', paneAgent: '' }).map(([k]) => k)).not.toContain('Agent');
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

  it('takes emails, home-folder names and token-like strings out of text the daemon could not check', () => {
    expect(scrubLocalReport('mail dana@example.co.uk now')).toBe('mail [redacted: email] now');
    expect(scrubLocalReport('at /home/dana/app/main.js:3:1')).toBe('at /home/[redacted: user]/app/main.js:3:1');
    expect(scrubLocalReport('at /Users/dana/app.js')).toBe('at /Users/[redacted: user]/app.js');
    expect(scrubLocalReport('C:\\Users\\Dana\\AppData\\x.js')).toBe('C:\\Users\\[redacted: user]\\AppData\\x.js');
    expect(scrubLocalReport('file:///C:/Users/Dana%20Lee/x.js')).toBe('file:///C:/Users/[redacted: user]/x.js');
    // A Windows or Mac name with a space goes whole, as in shared/feedbackredact.
    expect(scrubLocalReport('C:\\Users\\Dana Lee\\AppData\\x.js')).toBe('C:\\Users\\[redacted: user]\\AppData\\x.js');
    expect(scrubLocalReport('at /Users/Dana Lee/app.js')).toBe('at /Users/[redacted: user]/app.js');
    expect(scrubLocalReport('saved in C:\\Users\\Dana Lee \nnext')).toBe('saved in C:\\Users\\[redacted: user] \nnext');
    expect(scrubLocalReport('"C:\\Users\\Dana Lee": not found')).toBe('"C:\\Users\\[redacted: user]": not found');
    // A Linux name can't hold one, so the line goes on.
    expect(scrubLocalReport('/home/dana is the folder')).toBe('/home/[redacted: user] is the folder');
    expect(scrubLocalReport('file:///home/dana/x.js')).toBe('file:///home/[redacted: user]/x.js');
    expect(scrubLocalReport('key sk-live_abc123DEF456 end')).toBe('key [redacted: token] end');
    expect(scrubLocalReport(`bearer ${'a1B2'.repeat(10)} end`)).toBe('bearer [redacted: token] end');
  });

  it('leaves the stack itself readable', () => {
    const stack = [
      'Error: boom',
      '    at Topbar (http://localhost:8787/assets/index-D3kq9xZ_.js:1234:5678)',
      '    at node_modules/react-dom@18.3.1/index.js:10:2',
      '    at aVeryLongComponentNameWithoutAnyDigitsInItAtAll',
    ].join('\n');
    expect(scrubLocalReport(stack)).toBe(stack);
  });

  it('builds the crash report from what the page knows, scrubbed', () => {
    const text = crashReportFallbackText('App crashed: boom', 'Error: boom\nat /home/dana/x.js', '0.0.1242');
    expect(text).toBe([
      'SoluTerminal feedback · Bug', 'App crashed: boom', 'Error: boom\nat /home/[redacted: user]/x.js',
      `App and system details\nWeb version: 0.0.1242\nDevice: ${feedbackDevice()}\nOther app details weren't available.`,
    ].join('\n\n'));
  });

  it('builds the form\'s report the same way, under its own type', () => {
    const text = feedbackFallbackReportText('idea', 'Tabs', 'Tabs for dana@example.com', '0.0.1242');
    expect(text).toBe([
      'SoluTerminal feedback · Idea', 'Tabs', 'Tabs for [redacted: email]',
      `App and system details\nWeb version: 0.0.1242\nDevice: ${feedbackDevice()}\nOther app details weren't available.`,
    ].join('\n\n'));
  });

  it('names the device as one coarse word, never the browser\'s own description', () => {
    const on = (userAgent: string, maxTouchPoints = 0) => {
      vi.stubGlobal('navigator', { userAgent, maxTouchPoints });
      return feedbackDevice();
    };
    try {
      expect(on('Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0 Safari/537.36')).toBe('Windows');
      expect(on('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/18.0 Safari/605.1.15')).toBe('Mac');
      expect(on('Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/130.0 Safari/537.36')).toBe('Linux');
      expect(on('Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/130.0 Mobile Safari/537.36')).toBe('Android');
      expect(on('Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148')).toBe('iPhone or iPad');
      // An iPad asking for desktop sites.
      expect(on('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/18.0 Safari/605.1.15', 5)).toBe('iPhone or iPad');
      expect(on('Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 Chrome/130.0 Safari/537.36')).toBe('Other');
      expect(on('')).toBe('Other');
      const text = feedbackFallbackReportText('bug', 't', 'b', '0.0.1242');
      expect(text).not.toContain('Mozilla');
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('never shows an HTTP status', () => {
    expect(feedbackSendError({ status: 503, message: '503 Service Unavailable' })).toBe(FEEDBACK_SEND_ERROR);
    expect(feedbackSendError(new Error('Failed to fetch'))).toBe(FEEDBACK_SEND_ERROR);
    expect(feedbackSendError({ status: 429 })).toBe(FEEDBACK_LIMIT_ERROR);
  });
});

describe('openMailDraft in the desktop app', () => {
  const w = window as unknown as { __SOLU_TAURI__?: boolean; __TAURI_INTERNALS__?: { invoke: (cmd: string, args?: unknown) => Promise<unknown> } };

  afterEach(() => {
    delete w.__SOLU_TAURI__;
    delete w.__TAURI_INTERNALS__;
    vi.restoreAllMocks();
  });

  it('answers true when the shell opened the link', async () => {
    const invoke = vi.fn(async () => undefined);
    w.__SOLU_TAURI__ = true;
    w.__TAURI_INTERNALS__ = { invoke };
    await expect(openMailDraft('mailto:hello@soluterminal.com')).resolves.toBe(true);
    expect(invoke).toHaveBeenCalledWith('plugin:opener|open_url', { url: 'mailto:hello@soluterminal.com' });
  });

  it('answers false when the shell could not open it', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(window, 'open').mockImplementation(() => null);
    w.__SOLU_TAURI__ = true;
    w.__TAURI_INTERNALS__ = { invoke: vi.fn(async () => { throw new Error('no handler'); }) };
    await expect(openMailDraft('mailto:hello@soluterminal.com')).resolves.toBe(false);
  });
});
