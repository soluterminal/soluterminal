import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { Session } from '@supabase/supabase-js';
import { useCloud } from '../cloud';
import { CloudGate } from './CloudGate';

const mail = vi.hoisted(() => ({ openMailDraft: vi.fn<(href: string) => Promise<void>>(async () => {}) }));
vi.mock('../lib/feedbackReport', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../lib/feedbackReport')>()),
  openMailDraft: mail.openMailDraft,
}));

function seedAccountNotReady() {
  const signOut = vi.fn(async () => {});
  useCloud.setState({
    mode: 'cloud',
    isLocal: false,
    session: { user: { id: 'u1' } } as unknown as Session,
    authorised: false,
    selectedMachineId: null,
    error: null,
    recoveryPending: false,
    bootstrap: vi.fn(async () => {}),
    signOut,
  });
  return { signOut };
}

afterEach(() => {
  vi.unstubAllGlobals();
  mail.openMailDraft.mockClear();
});

describe('CloudGate: Account not ready', () => {
  it('offers Copy report and Email it instead of pointing at in-app feedback', async () => {
    const { signOut } = seedAccountNotReady();
    const writeText = vi.fn<(text: string) => Promise<void>>(async () => {});
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } });
    render(<CloudGate><div>app</div></CloudGate>);

    expect(screen.getByText(/Your account isn.t ready yet/)).toBeInTheDocument();
    expect(screen.queryByText(/Ctrl\+K/)).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Copy report' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText.mock.calls[0][0]).toMatch(/Account not ready[\s\S]*Web version: /);
    expect(await screen.findByRole('status')).toHaveTextContent(/^Copied\. Paste it into an email to hello@soluterminal\.com/);

    fireEvent.click(screen.getByRole('button', { name: 'Email it' }));
    expect(mail.openMailDraft).toHaveBeenCalledWith(expect.stringMatching(/^mailto:hello@soluterminal\.com\?subject=SoluTerminal%20feedback&body=/));

    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    expect(signOut).toHaveBeenCalled();
  });
});
