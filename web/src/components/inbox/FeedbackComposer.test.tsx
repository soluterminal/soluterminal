import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../../store';
import { api, ApiError, type FeedbackContext, type FeedbackReq } from '../../api';
import { useCloud } from '../../cloud';
import { FeedbackComposer } from './FeedbackComposer';
import {
  FEEDBACK_CONNECTED_TOAST,
  FEEDBACK_UNPAIRED_LINE,
  FEEDBACK_COPIED_NOTE,
  FEEDBACK_COPY_ERROR,
  FEEDBACK_EMAIL_CUT_NOTE,
  FEEDBACK_LIMIT_ERROR,
  FEEDBACK_MAIL_DRAFT_NOTE,
  FEEDBACK_NO_MAIL_APP_ERROR,
  FEEDBACK_NOT_SENT_YET,
  FEEDBACK_PREVIEW_ERROR,
  FEEDBACK_ROW_UNAVAILABLE,
  FEEDBACK_SEND_ERROR,
  FEEDBACK_UNCHECKED_NOTE,
} from '../../lib/feedbackReport';

vi.mock('../../api', async (importOriginal) => ({ ...(await importOriginal<typeof import('../../api')>()),
  api: { submitFeedback: vi.fn(), feedbackStatus: vi.fn(), feedbackPreview: vi.fn() },
}));

// null is a browser's answer: the page can't tell whether a draft opened.
const mail = vi.hoisted(() => ({ openMailDraft: vi.fn<(href: string) => Promise<boolean | null>>(async () => null) }));
const connect = vi.hoisted(() => ({ connectThisComputerAfterWarning: vi.fn<(msg?: string) => Promise<boolean>>() }));
vi.mock('../../accountConnect', () => connect);

vi.mock('../../lib/feedbackReport', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../lib/feedbackReport')>()),
  openMailDraft: mail.openMailDraft,
}));

const submitFeedback = vi.mocked(api.submitFeedback);
const feedbackStatus = vi.mocked(api.feedbackStatus);
const feedbackPreview = vi.mocked(api.feedbackPreview);

const CTX: FeedbackContext = {
  appVersion: '0.0.1241', daemonVersion: '0.0.1241', clientVersion: '0.0.1241', shellVersion: '0.0.1240',
  shellPlatform: 'windows', backendID: 'wsl-ubuntu', platform: 'windows', os: 'linux', arch: 'amd64',
  machineId: 'machine-7', paneId: '', captureKind: 'none',
};
const PANE_TEXT = 'npm run dev\nkey [REDACTED]\nprivate line\nready on :5173';

// The daemon stand-in: echoes the previewed text back as a submit would send it.
function previewImpl(req: Omit<FeedbackReq, 'type'>) {
  return Promise.resolve({
    title: (req.title || req.body.split('\n')[0]).replace('hunter2', '[REDACTED]'),
    body: req.body.replace('hunter2', '[REDACTED]'),
    snapshot: req.includeSnapshot ? req.snapshot ?? PANE_TEXT : '',
    logs: req.includeLogs ? req.logs ?? 'daemon started\nlistening' : '',
    context: req.includeSnapshot
      ? { ...CTX, paneId: req.paneId ?? '', paneKind: 'Focus', paneAgent: 'Codex' }
      : { ...CTX, paneId: '' },
  });
}

function seed() {
  const toast = vi.fn();
  useStore.setState({
    toast,
    focusedPaneId: 'pane_42',
    panes: { pane_42: { id: 'pane_42', title: 'Dev server' } },
  } as never);
  return { toast };
}

function renderComposer(onDone = vi.fn()) {
  render(<FeedbackComposer onDone={onDone} onCancel={() => {}} />);
  return { onDone };
}

const mainField = () => screen.getByRole('textbox', { name: 'What happened?' });

beforeEach(() => {
  submitFeedback.mockReset();
  feedbackStatus.mockReset().mockResolvedValue({ available: true });
  feedbackPreview.mockReset().mockImplementation(previewImpl);
  mail.openMailDraft.mockReset().mockResolvedValue(null);
  connect.connectThisComputerAfterWarning.mockReset().mockResolvedValue(false);
  localStorage.clear();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('FeedbackComposer', () => {
  it('sends the type, the trying-to-do line and the main field; title optional; nothing else ticked', async () => {
    const { toast } = seed();
    submitFeedback.mockResolvedValue({ ok: true, number: 99, url: 'https://x/issues/99' });
    const { onDone } = renderComposer();

    const idea = screen.getByRole('button', { name: 'Idea' });
    expect(screen.getByRole('button', { name: 'Bug' })).toHaveAttribute('aria-pressed', 'true');
    fireEvent.click(idea);
    expect(idea).toHaveAttribute('aria-pressed', 'true');
    fireEvent.change(screen.getByLabelText('What were you trying to do?'), { target: { value: 'split a pane' } });
    fireEvent.change(screen.getByLabelText('What would you like it to do?'), { target: { value: 'Add dark mode' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));

    await waitFor(() => expect(submitFeedback).toHaveBeenCalledTimes(1));
    const sent = submitFeedback.mock.calls[0][0];
    expect(sent).toMatchObject({
      type: 'idea',
      // No title typed: the main field's first line, not the trying-to-do line.
      title: 'Add dark mode',
      body: 'What I was trying to do: split a pane\n\nAdd dark mode',
      includeSnapshot: false,
      includeLogs: false,
    });
    expect(sent).not.toHaveProperty('paneId');
    expect(sent).not.toHaveProperty('snapshot');
    expect(sent).not.toHaveProperty('logs');
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(toast).toHaveBeenCalledWith(
      "Thanks, we've got it. Report #99 is with the team.",
      expect.objectContaining({ kind: 'success' }),
    );
  });

  it('hands a filed report to the host instead of closing with a toast', async () => {
    const { toast } = seed();
    submitFeedback.mockResolvedValue({ ok: true, number: 99, url: 'https://x/issues/99' });
    const onDone = vi.fn();
    const onSent = vi.fn();
    render(<FeedbackComposer onDone={onDone} onSent={onSent} onCancel={() => {}} />);
    fireEvent.change(mainField(), { target: { value: 'Tabs vanish' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(onSent).toHaveBeenCalledWith(99));
    expect(onDone).not.toHaveBeenCalled();
    expect(toast).not.toHaveBeenCalled();
  });

  it('a queued report has no number yet: thanks and close, as before', async () => {
    const { toast } = seed();
    submitFeedback.mockResolvedValue({ ok: true, number: 0, url: '', queued: true });
    const onDone = vi.fn();
    const onSent = vi.fn();
    render(<FeedbackComposer onDone={onDone} onSent={onSent} onCancel={() => {}} />);
    fireEvent.change(mainField(), { target: { value: 'Tabs vanish' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(onSent).not.toHaveBeenCalled();
    expect(toast).toHaveBeenCalledWith("Thanks, we've got it.", expect.objectContaining({ kind: 'success' }));
  });

  it('labels the main field by type and announces an empty one', async () => {
    seed();
    renderComposer();
    fireEvent.click(screen.getByRole('button', { name: 'Question' }));
    expect(screen.getByLabelText("What's your question?")).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Bug' }));
    expect(mainField()).toHaveAttribute('placeholder', 'What happened, and what did you expect instead?');
    expect(screen.getByLabelText('What were you trying to do?')).toHaveAttribute(
      'placeholder', 'For example: start Codex in a WSL pane');

    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Tell us what happened.');
    expect(mainField()).toHaveAttribute('aria-invalid', 'true');
    expect(mainField()).toHaveAccessibleDescription('Tell us what happened.');
    expect(submitFeedback).not.toHaveBeenCalled();
  });

  it('says a failure in plain words and keeps the report', async () => {
    seed();
    submitFeedback.mockRejectedValue(new ApiError(503, '503 Service Unavailable: could not file feedback'));
    renderComposer();
    fireEvent.change(mainField(), { target: { value: 'it broke' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(FEEDBACK_SEND_ERROR);
    expect(alert.textContent).not.toMatch(/503|Service Unavailable/);
    expect(mainField()).toHaveValue('it broke');

    submitFeedback.mockRejectedValue(new ApiError(429, 'daily feedback limit reached'));
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(FEEDBACK_LIMIT_ERROR));
  });

  it('after a failed send, offers Copy report and Email it as the error says', async () => {
    seed();
    const writeText = vi.fn<(text: string) => Promise<void>>(async () => {});
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    submitFeedback.mockRejectedValue(new ApiError(503, '503 Service Unavailable'));
    renderComposer();
    expect(screen.queryByRole('button', { name: 'Copy report' })).toBeNull();
    fireEvent.change(mainField(), { target: { value: 'it broke' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(FEEDBACK_SEND_ERROR);

    fireEvent.click(screen.getByRole('button', { name: 'Copy report' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText.mock.calls[0][0]).toContain('it broke');
    fireEvent.click(screen.getByRole('button', { name: 'Email it' }));
    await waitFor(() => expect(mail.openMailDraft).toHaveBeenCalledTimes(1));
    expect(screen.getByRole('button', { name: 'Send' })).toBeInTheDocument();
    expect(submitFeedback).toHaveBeenCalledTimes(1);
  });

  it('opens "What we\'ll send" the first time only, and lists the details exactly', async () => {
    seed();
    const { unmount } = render(<FeedbackComposer onDone={() => {}} onCancel={() => {}} />);
    const toggle = screen.getByRole('button', { name: "What we'll send" });
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByText('App and system details · always sent')).toBeInTheDocument();
    expect(screen.getByText(
      'Versions, operating system, platform, backend, CPU type and an ID for this computer (not its name).',
    )).toBeInTheDocument();
    expect(screen.getByText(
      "We remove tokens, keys, email addresses and home-folder paths we recognise. Check the preview and delete anything else you don't want to send.",
    )).toBeInTheDocument();

    await waitFor(() => expect(feedbackPreview).toHaveBeenCalled());
    fireEvent.click(within(screen.getByText('App and system details · always sent').closest('.inbox-send-row') as HTMLElement)
      .getByRole('button', { name: 'Show app and system details' }));
    expect(await screen.findByText('An ID for this computer (not its name)')).toBeInTheDocument();
    expect(screen.getByText('machine-7')).toBeInTheDocument();
    expect(screen.getByText('0.0.1240')).toBeInTheDocument();
    expect(screen.queryByText(/computer name|email address:/i)).toBeNull();
    unmount();

    render(<FeedbackComposer onDone={() => {}} onCancel={() => {}} />);
    expect(screen.getByRole('button', { name: "What we'll send" })).toHaveAttribute('aria-expanded', 'false');
  });

  // Wording item 9: each Show says what it opens; once open, it reads Hide.
  it('names the three Show buttons for what they open', async () => {
    seed();
    renderComposer();
    await waitFor(() => expect(feedbackPreview).toHaveBeenCalled());
    fireEvent.click(screen.getByRole('checkbox', { name: /Recent text from a pane/ }));
    fireEvent.click(screen.getByRole('checkbox', { name: 'Recent app logs' }));
    // Ticked rows open with their text shown; their buttons read Hide.
    await screen.findByRole('textbox', { name: 'Recent app logs' });
    for (const hide of screen.getAllByRole('button', { name: 'Hide' })) fireEvent.click(hide);
    for (const name of ['Show app and system details', 'Show the pane text that would be sent', 'Show the app logs that would be sent']) {
      expect(await screen.findByRole('button', { name })).toHaveTextContent('Show');
    }
    fireEvent.click(screen.getByRole('button', { name: 'Show app and system details' }));
    expect(screen.queryByRole('button', { name: 'Show app and system details' })).toBeNull();
  });

  // ST-165: the pane's type and agent go with its text, so the details list
  // shows them while pane text is ticked, and not otherwise.
  it('lists the pane\'s ID, type and agent in the details only while its text is ticked', async () => {
    seed();
    renderComposer();
    await waitFor(() => expect(feedbackPreview).toHaveBeenCalled());
    fireEvent.click(screen.getByRole('button', { name: 'Show app and system details' }));
    expect(await screen.findByText('An ID for this computer (not its name)')).toBeInTheDocument();
    expect(screen.queryByText('Pane type')).toBeNull();

    const paneBox = screen.getByRole('checkbox', { name: "Recent text from a pane (last 40 lines), with the pane's ID, type and agent" });
    fireEvent.click(paneBox);
    await screen.findByRole('textbox', { name: 'Recent text from a pane' });
    expect(await screen.findByText('Pane type')).toBeInTheDocument();
    expect(screen.getByText('Focus')).toBeInTheDocument();
    expect(screen.getByText('Agent')).toBeInTheDocument();
    expect(screen.getByText('Codex')).toBeInTheDocument();

    fireEvent.click(paneBox);
    expect(screen.queryByText('Pane type')).toBeNull();
    expect(screen.queryByText('Agent')).toBeNull();
  });

  it('sends the pane text as previewed, without the line the user deleted', async () => {
    seed();
    submitFeedback.mockResolvedValue({ ok: true, number: 1, url: 'u' });
    renderComposer();
    fireEvent.change(mainField(), { target: { value: 'dev server hangs' } });
    const paneBox = screen.getByRole('checkbox', { name: "Recent text from a pane (last 40 lines), with the pane's ID, type and agent" });
    expect(paneBox).not.toBeChecked();
    fireEvent.click(paneBox);
    expect(screen.getByLabelText('Pane')).toHaveValue('pane_42');
    expect(screen.getByRole('option', { name: 'Dev server' })).toBeInTheDocument();

    const preview = await screen.findByRole('textbox', { name: 'Recent text from a pane' });
    expect(preview).toHaveValue(PANE_TEXT);
    const edited = PANE_TEXT.replace('private line\n', '');
    fireEvent.change(preview, { target: { value: edited } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));

    await waitFor(() => expect(submitFeedback).toHaveBeenCalled());
    expect(submitFeedback.mock.calls[0][0]).toMatchObject({
      includeSnapshot: true, paneId: 'pane_42', snapshot: edited, includeLogs: false,
    });
    expect(JSON.stringify(submitFeedback.mock.calls[0][0])).not.toContain('private line');
  });

  it('unticks a row whose preview fails, so nothing unseen is sent', async () => {
    seed();
    submitFeedback.mockResolvedValue({ ok: true, number: 1, url: 'u' });
    feedbackPreview.mockImplementation((req) =>
      req.includeSnapshot ? Promise.reject(new Error('404')) : previewImpl(req));
    renderComposer();
    fireEvent.change(mainField(), { target: { value: 'x' } });
    const box = screen.getByRole('checkbox', { name: "Recent text from a pane (last 40 lines), with the pane's ID, type and agent" });
    fireEvent.click(box);
    expect(await screen.findByRole('alert')).toHaveTextContent(FEEDBACK_PREVIEW_ERROR);
    expect(box).not.toBeChecked();
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(submitFeedback).toHaveBeenCalled());
    expect(submitFeedback.mock.calls[0][0]).toMatchObject({ includeSnapshot: false });
    expect(submitFeedback.mock.calls[0][0]).not.toHaveProperty('snapshot');
  });

  it('keeps the preview box when every line is deleted', async () => {
    seed();
    renderComposer();
    fireEvent.click(screen.getByRole('checkbox', { name: "Recent text from a pane (last 40 lines), with the pane's ID, type and agent" }));
    const preview = await screen.findByRole('textbox', { name: 'Recent text from a pane' });
    fireEvent.change(preview, { target: { value: '' } });
    expect(screen.getByRole('textbox', { name: 'Recent text from a pane' })).toHaveValue('');
    expect(screen.queryByText('This pane has no recent text.')).toBeNull();
  });

  it('ticks logs on their own', async () => {
    seed();
    submitFeedback.mockResolvedValue({ ok: true, number: 1, url: 'u' });
    renderComposer();
    fireEvent.change(mainField(), { target: { value: 'x' } });
    fireEvent.click(screen.getByRole('checkbox', { name: 'Recent app logs' }));
    expect(await screen.findByRole('textbox', { name: 'Recent app logs' })).toHaveValue('daemon started\nlistening');
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(submitFeedback).toHaveBeenCalled());
    const sent = submitFeedback.mock.calls[0][0];
    expect(sent).toMatchObject({ includeSnapshot: false, includeLogs: true, logs: 'daemon started\nlistening' });
    expect(sent).not.toHaveProperty('snapshot');
  });

  it('shows the approved send-time line with the privacy link', () => {
    seed();
    renderComposer();
    const line = screen.getByText(/^Only what's shown above is sent\./);
    expect(line).toHaveTextContent(
      "Only what's shown above is sent. It goes privately to the SoluTerminal team, linked to your account so we can reply here, and an AI model helps us sort it. How we handle feedback",
    );
    expect(within(line).getByRole('link', { name: 'How we handle feedback' }))
      .toHaveAttribute('href', 'https://soluterminal.com/privacy/#feedback');
  });

  it('has no screenshot control', () => {
    seed();
    renderComposer();
    expect(screen.queryByText(/screenshot/i)).toBeNull();
    expect(document.querySelector('input[type="file"]')).toBeNull();
  });

  it('keeps a draft across unmount and clears it on send', async () => {
    seed();
    submitFeedback.mockResolvedValue({ ok: true, number: 5, url: 'u' });
    const { unmount } = render(<FeedbackComposer onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByLabelText('Title (optional)'), { target: { value: 'half-written' } });
    fireEvent.change(mainField(), { target: { value: 'came back later' } });
    unmount();

    render(<FeedbackComposer onDone={() => {}} onCancel={() => {}} />);
    expect(screen.getByLabelText('Title (optional)')).toHaveValue('half-written');
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(submitFeedback).toHaveBeenCalled());
    expect(localStorage.getItem('solu.feedbackDraft.v2')).toBeNull();
  });

  it('reopens with pane text and logs unticked, so a trimmed preview is never resent unseen', async () => {
    seed();
    const { unmount } = render(<FeedbackComposer onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(mainField(), { target: { value: 'half-written' } });
    fireEvent.click(screen.getByRole('checkbox', { name: "Recent text from a pane (last 40 lines), with the pane's ID, type and agent" }));
    fireEvent.click(screen.getByRole('checkbox', { name: 'Recent app logs' }));
    const preview = await screen.findByRole('textbox', { name: 'Recent text from a pane' });
    fireEvent.change(preview, { target: { value: PANE_TEXT.replace('private line\n', '') } });
    unmount();

    render(<FeedbackComposer onDone={() => {}} onCancel={() => {}} />);
    expect(mainField()).toHaveValue('half-written');
    fireEvent.click(screen.getByRole('button', { name: "What we'll send" }));
    expect(screen.getByRole('checkbox', { name: "Recent text from a pane (last 40 lines), with the pane's ID, type and agent" })).not.toBeChecked();
    expect(screen.getByRole('checkbox', { name: 'Recent app logs' })).not.toBeChecked();
    expect(localStorage.getItem('solu.feedbackDraft.v2')).not.toMatch(/include/);
  });

  it('does not add the same error twice when Report this is pressed again', () => {
    seed();
    const prefill = { type: 'bug' as const, body: 'The Terminal pane stopped with an error: boom' };
    const { unmount } = render(<FeedbackComposer onDone={() => {}} onCancel={() => {}} prefill={prefill} />);
    unmount();
    render(<FeedbackComposer onDone={() => {}} onCancel={() => {}} prefill={prefill} />);
    expect(mainField()).toHaveValue('The Terminal pane stopped with an error: boom');
  });

  it('adds an error screen\'s details to the form', () => {
    seed();
    render(<FeedbackComposer onDone={() => {}} onCancel={() => {}} prefill={{ type: 'bug', body: 'The Terminal pane stopped with an error: boom' }} />);
    expect(mainField()).toHaveValue('The Terminal pane stopped with an error: boom');
  });
});

// ST-166: a follow-up is a clean form linked by a field, and the person's
// saved draft is neither shown in it, overwritten nor cleared by it.
describe('FeedbackComposer as a follow-up', () => {
  const saved = JSON.stringify({ type: 'idea', trying: '', title: 'half-written idea', body: 'tabs could…' });

  it('opens clean with the follow-up line, sends the link, and leaves the saved draft alone', async () => {
    seed();
    localStorage.setItem('solu.feedbackDraft.v2', saved);
    submitFeedback.mockResolvedValue({ ok: true, number: 9, url: 'u' });
    render(<FeedbackComposer onDone={() => {}} onCancel={() => {}} prefill={{ followUpOf: 38, body: 'Still broken' }} />);

    expect(screen.getByText('This is a follow-up to report #38.')).toBeInTheDocument();
    expect(mainField()).toHaveValue('Still broken');
    expect(screen.getByLabelText('Title (optional)')).toHaveValue('');
    fireEvent.change(mainField(), { target: { value: 'Still broken in 1242' } });
    expect(localStorage.getItem('solu.feedbackDraft.v2')).toBe(saved);

    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(submitFeedback).toHaveBeenCalledTimes(1));
    const req = submitFeedback.mock.calls[0][0];
    expect(req.followUpOf).toBe(38);
    expect(req.body).toBe('Still broken in 1242');
    expect(localStorage.getItem('solu.feedbackDraft.v2')).toBe(saved);
  });

  it('the normal form still opens on the saved draft, and sends no link', async () => {
    seed();
    localStorage.setItem('solu.feedbackDraft.v2', saved);
    submitFeedback.mockResolvedValue({ ok: true, number: 9, url: 'u' });
    renderComposer();
    expect(screen.queryByText(/This is a follow-up/)).toBeNull();
    expect(screen.getByLabelText('Title (optional)')).toHaveValue('half-written idea');
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(submitFeedback).toHaveBeenCalledTimes(1));
    expect(submitFeedback.mock.calls[0][0]).not.toHaveProperty('followUpOf');
  });

  it('Copy report names the report it follows up', async () => {
    seed();
    feedbackStatus.mockResolvedValue({ available: false, reason: 'signed_out' });
    const writeText = vi.fn<(text: string) => Promise<void>>(async () => {});
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    render(<FeedbackComposer onDone={() => {}} onCancel={() => {}} prefill={{ followUpOf: 38 }} />);
    fireEvent.change(mainField(), { target: { value: 'it broke again' } });
    fireEvent.click(await screen.findByRole('button', { name: 'Copy report' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText.mock.calls[0][0]).toMatch(/^SoluTerminal feedback · Bug\n\nFollow-up to #38\n\n/);
  });
});

describe('FeedbackComposer without a connected account', () => {
  it('signed out: keeps the form, offers Copy report with the redacted report, sends nothing to the cloud', async () => {
    seed();
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    feedbackStatus.mockResolvedValue({ available: false, reason: 'signed_out' });
    renderComposer();

    expect(await screen.findByText(
      "You're not signed in, so this can't be sent from here. Copy the report or email it to us. Sign in to send it and get replies in the app.",
    )).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Send' })).toBeNull();
    expect(screen.queryByText(/^Only what's shown above is sent/)).toBeNull();

    fireEvent.change(mainField(), { target: { value: 'password hunter2 shows' } });
    fireEvent.click(screen.getByRole('checkbox', { name: "Recent text from a pane (last 40 lines), with the pane's ID, type and agent" }));
    await screen.findByRole('textbox', { name: 'Recent text from a pane' });
    fireEvent.click(screen.getByRole('button', { name: 'Copy report' }));

    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    const text = writeText.mock.calls[0][0] as string;
    expect(text).toContain('SoluTerminal feedback · Bug');
    expect(text).toContain('App version: 0.0.1241');
    expect(text).toContain('An ID for this computer (not its name): machine-7');
    expect(text).toContain(PANE_TEXT);
    expect(text).not.toContain('hunter2');
    expect(await screen.findByText(
      'Copied. Paste it into an email to hello@soluterminal.com, or into a GitHub issue (those are public, so check it first).',
    )).toBeInTheDocument();
    expect(submitFeedback).not.toHaveBeenCalled();
  });

  it('unpaired: Email it opens a mail draft with the report and sends nothing to the cloud', async () => {
    seed();
    feedbackStatus.mockResolvedValue({ available: false, reason: 'unpaired' });
    renderComposer();
    expect(await screen.findByText(FEEDBACK_UNPAIRED_LINE)).toBeInTheDocument();
    fireEvent.change(mainField(), { target: { value: 'it broke' } });
    fireEvent.click(screen.getByRole('button', { name: 'Email it' }));
    await waitFor(() => expect(mail.openMailDraft).toHaveBeenCalledTimes(1));
    const href = mail.openMailDraft.mock.calls[0][0] as string;
    expect(href).toMatch(/^mailto:hello@soluterminal\.com\?subject=SoluTerminal%20feedback%20%C2%B7%20Bug%20%C2%B7%20web%20[^&]+&body=/);
    expect(decodeURIComponent(href.split('&body=')[1])).toContain('it broke');
    expect(screen.queryByRole('button', { name: 'Sign in' })).toBeNull();
    expect(submitFeedback).not.toHaveBeenCalled();
  });

  // Wording item 4 and 18: the same on-demand connect as the mic's, then Send.
  it('unpaired: Connect this computer connects it, says so, and then offers Send', async () => {
    seed();
    // Pairing finishes late: two more checks still say unpaired, one fails.
    feedbackStatus
      .mockResolvedValueOnce({ available: false, reason: 'unpaired' })
      .mockResolvedValueOnce({ available: false, reason: 'unpaired' })
      .mockRejectedValueOnce(new Error('offline'))
      .mockResolvedValueOnce({ available: false, reason: 'unpaired' })
      .mockResolvedValue({ available: true });
    connect.connectThisComputerAfterWarning.mockResolvedValue(true);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      renderComposer();
      fireEvent.click(await screen.findByRole('button', { name: 'Connect this computer' }));
      await waitFor(() => expect(connect.connectThisComputerAfterWarning).toHaveBeenCalledWith(FEEDBACK_CONNECTED_TOAST));
      expect(screen.queryByRole('button', { name: 'Send' })).toBeNull();
      await vi.advanceTimersByTimeAsync(3000 * 4);
      expect(await screen.findByRole('button', { name: 'Send' })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Connect this computer' })).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it('unpaired: stops asking after a minute if the computer never connects', async () => {
    seed();
    feedbackStatus.mockResolvedValue({ available: false, reason: 'unpaired' });
    connect.connectThisComputerAfterWarning.mockResolvedValue(true);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      renderComposer();
      fireEvent.click(await screen.findByRole('button', { name: 'Connect this computer' }));
      await waitFor(() => expect(connect.connectThisComputerAfterWarning).toHaveBeenCalledTimes(1));
      await vi.advanceTimersByTimeAsync(120_000);
      const asked = feedbackStatus.mock.calls.length;
      expect(asked).toBeLessThanOrEqual(1 + 21);
      await vi.advanceTimersByTimeAsync(60_000);
      expect(feedbackStatus.mock.calls.length).toBe(asked);
      expect(screen.getByRole('button', { name: 'Connect this computer' })).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it('unpaired: a connect that did not happen leaves the form as it was', async () => {
    seed();
    feedbackStatus.mockResolvedValue({ available: false, reason: 'unpaired' });
    connect.connectThisComputerAfterWarning.mockResolvedValue(false);
    renderComposer();
    fireEvent.click(await screen.findByRole('button', { name: 'Connect this computer' }));
    await waitFor(() => expect(connect.connectThisComputerAfterWarning).toHaveBeenCalledTimes(1));
    expect(feedbackStatus).toHaveBeenCalledTimes(1);
    expect(screen.getByRole('button', { name: 'Email it' })).toBeInTheDocument();
  });

  it('offers Sign in on a local install with a sign-in screen', async () => {
    seed();
    const requestSignIn = vi.fn();
    const initial = useCloud.getState();
    useCloud.setState({ mode: 'cloud', isLocal: true, accountOptional: true, withoutAccount: true, session: null, requestSignIn });
    feedbackStatus.mockResolvedValue({ available: false, reason: 'signed_out' });
    const onDone = vi.fn();
    try {
      renderComposer(onDone);
      fireEvent.click(await screen.findByRole('button', { name: 'Sign in' }));
      expect(onDone).toHaveBeenCalled();
      expect(requestSignIn).toHaveBeenCalled();
    } finally {
      useCloud.setState(initial, true);
    }
  });
});

describe('FeedbackComposer when the background service cannot be reached', () => {
  const PANE_ROW = "Recent text from a pane (last 40 lines), with the pane's ID, type and agent";
  const down = () => feedbackPreview.mockRejectedValue(new ApiError(502, 'bad gateway'));

  function stubClipboard() {
    const writeText = vi.fn<(text: string) => Promise<void>>(async () => {});
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    return writeText;
  }

  it('says the check did not run before Copy report is used, and copies what was typed, scrubbed', async () => {
    seed();
    const writeText = stubClipboard();
    feedbackStatus.mockResolvedValue({ available: false, reason: 'signed_out' });
    down();
    renderComposer();

    expect(await screen.findByText(FEEDBACK_UNCHECKED_NOTE)).toBeInTheDocument();
    expect(screen.queryByText(/^We remove tokens, keys/)).toBeNull();

    fireEvent.change(mainField(), { target: { value: 'it broke for jo@example.com in C:\\Users\\jo\\app' } });
    fireEvent.click(screen.getByRole('button', { name: 'Copy report' }));

    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    const text = writeText.mock.calls[0][0];
    expect(text).toContain('SoluTerminal feedback · Bug');
    expect(text).toContain('it broke for [redacted: email] in C:\\Users\\[redacted: user]\\app');
    expect(text).toMatch(/Web version: .+\nDevice: .+\nOther app details weren't available\.$/);
    expect(text).not.toContain('App version');
    expect(text).not.toContain('jo@example.com');
    expect(await screen.findByText(FEEDBACK_COPIED_NOTE)).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(submitFeedback).not.toHaveBeenCalled();
  });

  it('after a failed send, leaves a ticked pane row out and says so on the row', async () => {
    seed();
    const writeText = stubClipboard();
    submitFeedback.mockRejectedValue(new ApiError(502, 'bad gateway'));
    renderComposer();
    fireEvent.change(mainField(), { target: { value: 'it broke' } });
    fireEvent.click(screen.getByRole('checkbox', { name: PANE_ROW }));
    await screen.findByRole('textbox', { name: 'Recent text from a pane' });
    expect(screen.queryByText(FEEDBACK_UNCHECKED_NOTE)).toBeNull();

    down();
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));

    expect(await screen.findByText(FEEDBACK_UNCHECKED_NOTE)).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: PANE_ROW })).not.toBeChecked();
    expect(screen.getByText(FEEDBACK_ROW_UNAVAILABLE)).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Copy report' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText.mock.calls[0][0]).toContain('it broke');
    expect(writeText.mock.calls[0][0]).not.toContain(PANE_TEXT);
    expect(writeText.mock.calls[0][0]).not.toContain('Recent text from a pane');
  });

  it('a row ticked while it is unreachable is unticked with the same line, not a retry', async () => {
    seed();
    feedbackStatus.mockResolvedValue({ available: false, reason: 'unpaired' });
    down();
    renderComposer();
    await screen.findByText(FEEDBACK_UNCHECKED_NOTE);

    fireEvent.click(screen.getByRole('checkbox', { name: 'Recent app logs' }));

    expect(await screen.findByText(FEEDBACK_ROW_UNAVAILABLE)).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'Recent app logs' })).not.toBeChecked();
    expect(screen.queryByText(FEEDBACK_PREVIEW_ERROR)).toBeNull();
  });

  it('copies or emails nothing on the click that finds the service gone; the next click goes on', async () => {
    seed();
    const writeText = stubClipboard();
    feedbackStatus.mockResolvedValue({ available: false, reason: 'signed_out' });
    renderComposer();
    fireEvent.change(mainField(), { target: { value: 'it broke' } });
    fireEvent.click(await screen.findByRole('checkbox', { name: 'Recent app logs' }));
    await screen.findByRole('textbox', { name: 'Recent app logs' });
    expect(screen.queryByText(FEEDBACK_UNCHECKED_NOTE)).toBeNull();

    down();
    fireEvent.click(screen.getByRole('button', { name: 'Email it' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(FEEDBACK_NOT_SENT_YET);
    expect(screen.getByText(FEEDBACK_UNCHECKED_NOTE)).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'Recent app logs' })).not.toBeChecked();
    expect(screen.getByText(FEEDBACK_ROW_UNAVAILABLE)).toBeInTheDocument();
    expect(mail.openMailDraft).not.toHaveBeenCalled();
    expect(writeText).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: 'Copy report' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText.mock.calls[0][0]).toContain('it broke');
    expect(writeText.mock.calls[0][0]).not.toContain('Recent app logs');
    expect(screen.queryByText(FEEDBACK_NOT_SENT_YET)).toBeNull();
  });

  it('uses the checked report again once the background service answers', async () => {
    seed();
    const writeText = stubClipboard();
    feedbackStatus.mockResolvedValue({ available: false, reason: 'signed_out' });
    down();
    renderComposer();
    await screen.findByText(FEEDBACK_UNCHECKED_NOTE);

    feedbackPreview.mockImplementation(previewImpl);
    fireEvent.change(mainField(), { target: { value: 'password hunter2 shows' } });
    fireEvent.click(screen.getByRole('button', { name: 'Copy report' }));

    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText.mock.calls[0][0]).toContain('App version: 0.0.1241');
    expect(writeText.mock.calls[0][0]).not.toContain('hunter2');
    await waitFor(() => expect(screen.queryByText(FEEDBACK_UNCHECKED_NOTE)).toBeNull());
  });
});

describe('FeedbackComposer Copy report and Email it outcomes', () => {
  function signedOut() {
    seed();
    feedbackStatus.mockResolvedValue({ available: false, reason: 'signed_out' });
    renderComposer();
  }

  it('says so when the report could not be copied', async () => {
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText: vi.fn().mockRejectedValue(new Error('denied')) } });
    signedOut();
    fireEvent.change(mainField(), { target: { value: 'it broke' } });
    fireEvent.click(await screen.findByRole('button', { name: 'Copy report' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(FEEDBACK_COPY_ERROR);
    expect(screen.queryByText(FEEDBACK_COPIED_NOTE)).toBeNull();
  });

  it('in a browser, says what to do if no draft opened, and when the email was cut', async () => {
    signedOut();
    fireEvent.change(mainField(), { target: { value: 'it broke' } });
    fireEvent.click(await screen.findByRole('button', { name: 'Email it' }));
    expect(await screen.findByText(FEEDBACK_MAIL_DRAFT_NOTE)).toBeInTheDocument();
    expect(screen.queryByText(FEEDBACK_EMAIL_CUT_NOTE)).toBeNull();

    fireEvent.change(mainField(), { target: { value: 'long line. '.repeat(400) } });
    fireEvent.click(screen.getByRole('button', { name: 'Email it' }));
    expect(await screen.findByText(FEEDBACK_EMAIL_CUT_NOTE)).toBeInTheDocument();
    expect(screen.getByText(FEEDBACK_MAIL_DRAFT_NOTE)).toBeInTheDocument();
  });

  it('in the desktop app, says no email app opened only when the open failed', async () => {
    signedOut();
    fireEvent.change(mainField(), { target: { value: 'it broke' } });
    mail.openMailDraft.mockResolvedValue(true);
    fireEvent.click(await screen.findByRole('button', { name: 'Email it' }));
    await waitFor(() => expect(mail.openMailDraft).toHaveBeenCalledTimes(1));
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByText(FEEDBACK_MAIL_DRAFT_NOTE)).toBeNull();

    mail.openMailDraft.mockResolvedValue(false);
    fireEvent.click(screen.getByRole('button', { name: 'Email it' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(FEEDBACK_NO_MAIL_APP_ERROR);
  });
});
