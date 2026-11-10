import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../../store';
import { api, ApiError, type FeedbackContext, type FeedbackReq } from '../../api';
import { useCloud } from '../../cloud';
import { FeedbackComposer } from './FeedbackComposer';
import { FEEDBACK_LIMIT_ERROR, FEEDBACK_PREVIEW_ERROR, FEEDBACK_SEND_ERROR } from '../../lib/feedbackReport';

vi.mock('../../api', async (importOriginal) => ({ ...(await importOriginal<typeof import('../../api')>()),
  api: { submitFeedback: vi.fn(), feedbackStatus: vi.fn(), feedbackPreview: vi.fn() },
}));

const mail = vi.hoisted(() => ({ openMailDraft: vi.fn<(href: string) => Promise<void>>(async () => {}) }));
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
    context: { ...CTX, paneId: req.includeSnapshot ? req.paneId ?? '' : '' },
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
  mail.openMailDraft.mockClear();
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
      .getByRole('button', { name: 'Show' }));
    expect(await screen.findByText('An ID for this computer (not its name)')).toBeInTheDocument();
    expect(screen.getByText('machine-7')).toBeInTheDocument();
    expect(screen.getByText('0.0.1240')).toBeInTheDocument();
    expect(screen.queryByText(/computer name|email address:/i)).toBeNull();
    unmount();

    render(<FeedbackComposer onDone={() => {}} onCancel={() => {}} />);
    expect(screen.getByRole('button', { name: "What we'll send" })).toHaveAttribute('aria-expanded', 'false');
  });

  it('sends the pane text as previewed, without the line the user deleted', async () => {
    seed();
    submitFeedback.mockResolvedValue({ ok: true, number: 1, url: 'u' });
    renderComposer();
    fireEvent.change(mainField(), { target: { value: 'dev server hangs' } });
    const paneBox = screen.getByRole('checkbox', { name: "Recent text from a pane (last 40 lines), with the pane's ID" });
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
    const box = screen.getByRole('checkbox', { name: "Recent text from a pane (last 40 lines), with the pane's ID" });
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
    fireEvent.click(screen.getByRole('checkbox', { name: "Recent text from a pane (last 40 lines), with the pane's ID" }));
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
    fireEvent.click(screen.getByRole('checkbox', { name: "Recent text from a pane (last 40 lines), with the pane's ID" }));
    fireEvent.click(screen.getByRole('checkbox', { name: 'Recent app logs' }));
    const preview = await screen.findByRole('textbox', { name: 'Recent text from a pane' });
    fireEvent.change(preview, { target: { value: PANE_TEXT.replace('private line\n', '') } });
    unmount();

    render(<FeedbackComposer onDone={() => {}} onCancel={() => {}} />);
    expect(mainField()).toHaveValue('half-written');
    fireEvent.click(screen.getByRole('button', { name: "What we'll send" }));
    expect(screen.getByRole('checkbox', { name: "Recent text from a pane (last 40 lines), with the pane's ID" })).not.toBeChecked();
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
    fireEvent.click(screen.getByRole('checkbox', { name: "Recent text from a pane (last 40 lines), with the pane's ID" }));
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
    expect(await screen.findByText(
      "This computer isn't connected to your account, so this can't be sent from here. Copy the report or email it to us.",
    )).toBeInTheDocument();
    fireEvent.change(mainField(), { target: { value: 'it broke' } });
    fireEvent.click(screen.getByRole('button', { name: 'Email it' }));
    await waitFor(() => expect(mail.openMailDraft).toHaveBeenCalledTimes(1));
    const href = mail.openMailDraft.mock.calls[0][0] as string;
    expect(href).toMatch(/^mailto:hello@soluterminal\.com\?subject=SoluTerminal%20feedback&body=/);
    expect(decodeURIComponent(href.split('&body=')[1])).toContain('it broke');
    expect(screen.queryByRole('button', { name: 'Sign in' })).toBeNull();
    expect(submitFeedback).not.toHaveBeenCalled();
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
