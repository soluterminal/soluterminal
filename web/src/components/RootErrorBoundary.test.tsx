import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { ReactElement } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../api';
import { RootErrorBoundary, crashReportBody } from './RootErrorBoundary';

function Crash(): ReactElement {
  throw new Error('topbar exploded');
}

let consoleSpy: ReturnType<typeof vi.spyOn>;
beforeEach(() => { consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {}); });
afterEach(() => { consoleSpy.mockRestore(); vi.restoreAllMocks(); });

function renderCrash() {
  render(<RootErrorBoundary><Crash /></RootErrorBoundary>);
}

describe('RootErrorBoundary', () => {
  it('renders children when nothing throws', () => {
    render(<RootErrorBoundary><div>App body</div></RootErrorBoundary>);
    expect(screen.getByText('App body')).toBeInTheDocument();
  });

  it('replaces a blank window with "Something went wrong", the error and Reload', () => {
    renderCrash();
    expect(screen.getByRole('alert')).toBeInTheDocument();
    expect(screen.getByText('Something went wrong')).toBeInTheDocument();
    expect(screen.getByText('topbar exploded')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Reload' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Send report' })).toBeInTheDocument();
  });

  it('Send report files the crash through the feedback route', async () => {
    vi.spyOn(api, 'feedbackStatus').mockResolvedValue({ available: true });
    const submit = vi.spyOn(api, 'submitFeedback').mockResolvedValue({ ok: true, number: 812, url: '' });
    renderCrash();
    fireEvent.click(screen.getByRole('button', { name: 'Send report' }));
    expect(await screen.findByText('Report sent (#812). Thank you.')).toBeInTheDocument();
    expect(submit).toHaveBeenCalledWith(expect.objectContaining({
      type: 'bug',
      title: 'App crashed: topbar exploded',
      includeSnapshot: false,
      body: expect.stringContaining('Error: topbar exploded'),
    }));
    expect(screen.getByRole('button', { name: 'Send report' })).toBeDisabled();
  });

  it('says why when reports cannot be sent, and offers Copy details', async () => {
    vi.spyOn(api, 'feedbackStatus').mockResolvedValue({ available: false, reason: 'signed_out' });
    const submit = vi.spyOn(api, 'submitFeedback');
    renderCrash();
    fireEvent.click(screen.getByRole('button', { name: 'Send report' }));
    expect(await screen.findByText(/Sending a report needs a free account/)).toBeInTheDocument();
    expect(submit).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Copy details' })).toBeInTheDocument();
  });

  it('shows a send failure and lets the user retry', async () => {
    vi.spyOn(api, 'feedbackStatus').mockResolvedValue(null);
    vi.spyOn(api, 'submitFeedback').mockRejectedValue(new Error('daemon unreachable'));
    renderCrash();
    fireEvent.click(screen.getByRole('button', { name: 'Send report' }));
    expect(await screen.findByText(/Couldn't send the report: daemon unreachable/)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Send report' })).not.toBeDisabled());
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
