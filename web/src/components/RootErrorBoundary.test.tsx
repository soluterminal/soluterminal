import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { ReactElement } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError, type FeedbackPreview, type FeedbackReq } from '../api';
import { FEEDBACK_MAILTO_CUT_NOTE } from '../lib/feedbackReport';
import { RootErrorBoundary, crashReportBody } from './RootErrorBoundary';

const mail = vi.hoisted(() => ({ openMailDraft: vi.fn<(href: string) => Promise<void>>(async () => {}) }));
vi.mock('../lib/feedbackReport', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../lib/feedbackReport')>()),
  openMailDraft: mail.openMailDraft,
}));

function Crash(): ReactElement {
  throw new Error('topbar exploded');
}

const CTX = {
  appVersion: '0.0.1242', daemonVersion: '0.0.1242', clientVersion: '0.0.1242', shellVersion: '', shellPlatform: 'windows',
  backendID: 'wsl-ubuntu', platform: 'windows', os: 'linux', arch: 'amd64', machineId: 'machine-7', paneId: '',
  captureKind: 'none',
};

// The daemon's preview: the same title and body, as it would redact them.
function daemonPreview(req: Omit<FeedbackReq, 'type'>): Promise<FeedbackPreview> {
  return Promise.resolve({ title: req.title, body: `${req.body}\n(redacted by the daemon)`, snapshot: '', logs: '', context: CTX });
}

let consoleSpy: ReturnType<typeof vi.spyOn>;
let preview: ReturnType<typeof vi.spyOn>;
let status: ReturnType<typeof vi.spyOn>;
let submit: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
  preview = vi.spyOn(api, 'feedbackPreview').mockImplementation(daemonPreview);
  status = vi.spyOn(api, 'feedbackStatus').mockResolvedValue({ available: true });
  submit = vi.spyOn(api, 'submitFeedback').mockResolvedValue({ ok: true, number: 812, url: '' });
  mail.openMailDraft.mockClear();
});
afterEach(() => { consoleSpy.mockRestore(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function renderCrash() {
  render(<RootErrorBoundary><Crash /></RootErrorBoundary>);
}

function shownReport(): string {
  return document.querySelector('.root-error-report')?.textContent ?? '';
}

function stubClipboard() {
  const writeText = vi.fn<(text: string) => Promise<void>>(async () => {});
  vi.stubGlobal('navigator', { ...navigator, userAgent: navigator.userAgent, clipboard: { writeText } });
  return writeText;
}

describe('RootErrorBoundary', () => {
  it('renders children when nothing throws', () => {
    render(<RootErrorBoundary><div>App body</div></RootErrorBoundary>);
    expect(screen.getByText('App body')).toBeInTheDocument();
    expect(preview).not.toHaveBeenCalled();
  });

  it('replaces a blank window with "Something went wrong", the error and Reload', () => {
    renderCrash();
    expect(screen.getByRole('alert')).toBeInTheDocument();
    expect(screen.getByText('Something went wrong')).toBeInTheDocument();
    expect(screen.getByText('topbar exploded')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Reload' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Send report' })).toBeInTheDocument();
  });

  it('lists what a report carries before anything is sent, and sends nothing by itself', async () => {
    renderCrash();
    const rows = within(screen.getByRole('list', { name: "What we'll send" })).getAllByRole('listitem');
    expect(rows.map((li) => li.firstChild?.textContent)).toEqual([
      'The error message',
      'Where in the app it happened',
      'Your browser and app version',
      'Technical details of the error (stack trace)',
      'App and system details',
    ]);
    expect(rows[4]).toHaveTextContent(
      'Versions, operating system, platform, backend, CPU type and an ID for this computer (not its name).',
    );
    expect(screen.getByText(/Only what's listed above is sent\./)).toBeInTheDocument();
    await waitFor(() => expect(preview).toHaveBeenCalledTimes(1));
    // Asking what would be sent sends nothing, and no button takes the focus.
    expect(submit).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(document.body);
  });

  it('Show opens exactly the text the daemon would send', async () => {
    renderCrash();
    expect(document.querySelector('.root-error-report')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Show the report that would be sent' }));
    await waitFor(() => expect(shownReport()).toContain('(redacted by the daemon)'));
    const text = shownReport();
    expect(text).toContain('SoluTerminal feedback · Bug');
    expect(text).toContain('App crashed: topbar exploded');
    expect(text).toContain('Operating system: linux');
    expect(text).toContain('An ID for this computer (not its name): machine-7');
    expect(screen.queryByText(/Check this text before you send it/)).not.toBeInTheDocument();
    expect(preview).toHaveBeenCalledWith(expect.objectContaining({ includeSnapshot: false, includeLogs: false }));
    fireEvent.click(screen.getByRole('button', { name: 'Hide the report that would be sent' }));
    expect(document.querySelector('.root-error-report')).toBeNull();
  });

  it('Send report files the crash through the feedback route, with no pane text and no logs', async () => {
    renderCrash();
    fireEvent.click(screen.getByRole('button', { name: 'Send report' }));
    expect(await screen.findByText('Report sent (#812). Thank you.')).toBeInTheDocument();
    expect(submit).toHaveBeenCalledTimes(1);
    const sent = submit.mock.calls[0][0] as FeedbackReq;
    expect(sent).toMatchObject({ type: 'bug', title: 'App crashed: topbar exploded', includeSnapshot: false, includeLogs: false });
    expect(sent.body).toContain('Error: topbar exploded');
    expect(sent.paneId).toBeUndefined();
    expect(sent.snapshot).toBeUndefined();
    expect(sent.logs).toBeUndefined();
    expect(screen.getByRole('button', { name: 'Send report' })).toBeDisabled();
  });

  it.each([
    ['signed_out', "You're not signed in, so this can't be sent from here. Copy the report or email it to us."],
    ['unpaired', "This computer isn't connected to your account, so this can't be sent from here. Copy the report or email it to us."],
  ] as const)('offers Copy report and Email it instead of Send when %s', async (reason, sentence) => {
    status.mockResolvedValue({ available: false, reason });
    const writeText = stubClipboard();
    renderCrash();
    expect(await screen.findByText(sentence)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Send report' })).not.toBeInTheDocument();
    expect(screen.getByRole('alert')).not.toHaveTextContent(/support/i);

    fireEvent.click(screen.getByRole('button', { name: 'Show the report that would be sent' }));
    await waitFor(() => expect(shownReport()).toContain('(redacted by the daemon)'));
    fireEvent.click(screen.getByRole('button', { name: 'Copy report' }));
    expect(await screen.findByText(/^Copied\. Paste it into an email to hello@soluterminal\.com/)).toBeInTheDocument();
    expect(writeText).toHaveBeenCalledWith(shownReport());
    expect(submit).not.toHaveBeenCalled();
  });

  it('Email it opens a draft to the team and says when the text was cut', async () => {
    status.mockResolvedValue({ available: false, reason: 'signed_out' });
    preview.mockImplementation((req: Omit<FeedbackReq, 'type'>) => daemonPreview({ ...req, body: 'stack line\n'.repeat(400) }));
    renderCrash();
    fireEvent.click(await screen.findByRole('button', { name: 'Email it' }));
    await waitFor(() => expect(mail.openMailDraft).toHaveBeenCalledTimes(1));
    const href = mail.openMailDraft.mock.calls[0][0];
    expect(href.startsWith('mailto:hello@soluterminal.com?')).toBe(true);
    expect(decodeURIComponent(href)).toContain(FEEDBACK_MAILTO_CUT_NOTE);
    expect(await screen.findByText(/This report was too long for an email link/)).toBeInTheDocument();
  });

  it('finds out at Send that this computer cannot send, and sends nothing', async () => {
    status.mockRejectedValueOnce(new Error('offline')).mockResolvedValue({ available: false, reason: 'unpaired' });
    renderCrash();
    await waitFor(() => expect(status).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByRole('button', { name: 'Send report' }));
    expect(await screen.findByText(/isn't connected to your account/)).toBeInTheDocument();
    expect(submit).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Copy report' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Send report' })).not.toBeInTheDocument();
  });

  it('shows a send failure in plain words, never the HTTP status, with Copy report, Email it and a retry', async () => {
    status.mockResolvedValue(null);
    submit.mockRejectedValue(new ApiError(502, '502 Bad Gateway: could not file feedback right now — please try again'));
    renderCrash();
    fireEvent.click(screen.getByRole('button', { name: 'Send report' }));
    expect(await screen.findByText("Couldn't send the report. Use Copy report or Email it instead.")).toBeInTheDocument();
    expect(screen.getByRole('alert')).not.toHaveTextContent(/502|Bad Gateway/);
    expect(screen.getByRole('button', { name: 'Copy report' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Email it' })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Send report' })).not.toBeDisabled());
  });

  // No daemon preview, no redaction: the page builds the text, runs its own
  // small pass, opens it without being asked and says it was not checked.
  // Send would carry the daemon's text, not this one, so it is not offered,
  // even if the daemon would have accepted the send.
  it('when the daemon gives no preview, shows page-built text to check and offers only Copy report and Email it', async () => {
    preview.mockRejectedValue(new Error('Failed to fetch'));
    const writeText = stubClipboard();
    renderCrash();
    expect(await screen.findByText(
      "Check this text before you send it: we couldn't run our usual check for private details.",
    )).toBeInTheDocument();
    const text = shownReport();
    expect(text).toContain('App crashed: topbar exploded');
    expect(text).toContain('App and system details\nWeb version: ');
    expect(text).not.toMatch(/\/home\/(?!\[redacted: user\])/);
    expect(screen.getByRole('alert')).not.toHaveTextContent(/We remove tokens/);

    expect(screen.queryByRole('button', { name: 'Send report' })).not.toBeInTheDocument();
    expect(screen.getByText("This can't be sent from here right now. Copy the report or email it to us.")).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Email it' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Copy report' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(text));
    expect(submit).not.toHaveBeenCalled();
  });

  it('a Send clicked before the preview fails sends nothing', async () => {
    let fail: (e: Error) => void = () => {};
    preview.mockReturnValue(new Promise<FeedbackPreview>((_, reject) => { fail = reject; }));
    renderCrash();
    fireEvent.click(screen.getByRole('button', { name: 'Send report' }));
    fail(new Error('Failed to fetch'));
    expect(await screen.findByRole('button', { name: 'Copy report' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Send report' })).not.toBeInTheDocument();
    expect(submit).not.toHaveBeenCalled();
  });
});

describe('crashReportBody', () => {
  it('carries the error, version and stacks', () => {
    const err = new Error('boom');
    err.stack = 'Error: boom\n    at Topbar';
    const body = crashReportBody(err, '\n    in Topbar\n    in App');
    expect(body).toContain('Error: boom');
    expect(body).toContain('Version: ');
    expect(body).toContain('at Topbar');
    expect(body).toContain('in App');
  });
});
