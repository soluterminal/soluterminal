import type { Session } from '@supabase/supabase-js';
import { renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  askToConnectAfterSignIn,
  connectPromptPending,
  connectThisComputer,
  CONNECT_DECLINED_TOAST,
  CONNECT_LATER_TOAST,
  CONNECT_PROMPT_BODY,
  CONNECT_PROMPT_TITLE,
  resetAutoConnectForTests,
  useAskToConnectOnSignIn,
} from './accountConnect';
import { useStore } from './store';
import { useCloud } from './cloud';

afterEach(() => {
  vi.restoreAllMocks();
  useCloud.setState({ session: null, isLocal: false });
  resetAutoConnectForTests();
});

describe('connectThisComputer', () => {
  it('needs a signed-in local session', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    useCloud.setState({ session: null, isLocal: true });
    expect((await connectThisComputer()).status).toBe('signin');
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('sends the account token to the daemon and returns its answer', async () => {
    useCloud.setState({ isLocal: true, session: { access_token: 'acct', user: { id: 'u1' } } as unknown as Session });
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ status: 'machine_limit', message: 'up to 3 computers' }), { status: 409 }),
    );
    const res = await connectThisComputer({ force: true });
    expect(res).toEqual({ status: 'machine_limit', message: 'up to 3 computers' });
    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/cloud/pair/account');
    expect((init.headers as Record<string, string>)['X-Solu-Account-Token']).toBe('acct');
    expect(JSON.parse(String(init.body))).toEqual({ force: true });
  });

  it('never sends an anonymous session', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    useCloud.setState({ isLocal: true, session: { access_token: 'anon', user: { id: 'u2', is_anonymous: true } } as unknown as Session });
    expect((await connectThisComputer()).status).toBe('signin');
    expect(fetchSpy).not.toHaveBeenCalled();
  });
});

describe('askToConnectAfterSignIn', () => {
  function signedIn() {
    useCloud.setState({ isLocal: true, session: { access_token: 'acct', user: { id: 'u3' } } as unknown as Session });
  }
  function daemon(answers: Array<{ status: string }>) {
    const bodies: Array<Record<string, boolean>> = [];
    const spy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (_url, init) => {
      bodies.push(JSON.parse(String((init as RequestInit).body)));
      const next = answers.shift() ?? { status: 'error' };
      return new Response(JSON.stringify(next), { status: 200 });
    });
    return { spy, bodies };
  }
  function answerDialog(choice: boolean | null) {
    const opened: Array<{ title?: string; message: string; confirmLabel?: string; cancelLabel?: string }> = [];
    const prev = useStore.getState().openDialog;
    useStore.setState({
      openDialog: ((req: { title?: string; message: string; confirmLabel?: string; cancelLabel?: string; resolve: (v: unknown) => void }) => {
        opened.push(req);
        req.resolve(choice);
      }) as never,
    });
    return { opened, restore: () => useStore.setState({ openDialog: prev }) };
  }

  it('asks once with Connect / Not now, and connects on Connect', async () => {
    signedIn();
    const { bodies } = daemon([{ status: 'available' }, { status: 'connecting' }]);
    const dialog = answerDialog(true);
    await askToConnectAfterSignIn();
    dialog.restore();
    expect(dialog.opened).toHaveLength(1);
    expect(dialog.opened[0]).toMatchObject({
      title: CONNECT_PROMPT_TITLE,
      message: CONNECT_PROMPT_BODY,
      confirmLabel: 'Connect',
      cancelLabel: 'Not now',
    });
    expect(bodies).toEqual([{ force: false, check: true }, { force: true }]);
  });

  it('remembers the Not now button on the daemon', async () => {
    signedIn();
    const { bodies } = daemon([{ status: 'available' }, { status: 'declined' }]);
    const toast = vi.spyOn(useStore.getState(), 'toast');
    const dialog = answerDialog(false);
    await askToConnectAfterSignIn();
    dialog.restore();
    expect(bodies).toEqual([{ force: false, check: true }, { force: false, decline: true }]);
    expect(toast).toHaveBeenCalledWith(CONNECT_DECLINED_TOAST, expect.anything());
  });

  it('treats Escape or a click outside as "later", not as Not now', async () => {
    signedIn();
    const { bodies } = daemon([{ status: 'available' }, { status: 'connecting' }]);
    const toast = vi.fn();
    const prevToast = useStore.getState().toast;
    useStore.setState({ toast: toast as never });
    const dialog = answerDialog(null);
    await askToConnectAfterSignIn();
    dialog.restore();
    // Nothing is sent to the daemon, so the next start asks again.
    expect(bodies).toEqual([{ force: false, check: true }]);
    expect(toast).toHaveBeenCalledWith(CONNECT_LATER_TOAST, expect.objectContaining({
      action: expect.objectContaining({ label: 'Connect now' }),
    }));
    // The toast's "Connect now" connects straight away.
    const action = (toast.mock.calls[0][1] as { action: { run: () => void } }).action;
    action.run();
    await new Promise((r) => setTimeout(r, 0));
    useStore.setState({ toast: prevToast });
    expect(bodies).toEqual([{ force: false, check: true }, { force: true }]);
  });

  it('is pending while it asks, so other first-run questions wait', async () => {
    signedIn();
    daemon([{ status: 'available' }, { status: 'declined' }]);
    let resolveDialog: ((v: unknown) => void) | null = null;
    const prev = useStore.getState().openDialog;
    useStore.setState({ openDialog: ((req: { resolve: (v: unknown) => void }) => { resolveDialog = req.resolve; }) as never });
    const asked = askToConnectAfterSignIn();
    expect(connectPromptPending()).toBe(true);
    await vi.waitFor(() => expect(resolveDialog).not.toBeNull());
    expect(connectPromptPending()).toBe(true);
    resolveDialog!(false);
    await asked;
    useStore.setState({ openDialog: prev });
    expect(connectPromptPending()).toBe(false);
  });

  it('waits until the tour and checklist are closed', async () => {
    signedIn();
    const { bodies } = daemon([{ status: 'connected' }]);
    const { rerender } = renderHook(({ ready }) => useAskToConnectOnSignIn(ready), { initialProps: { ready: false } });
    await new Promise((r) => setTimeout(r, 0));
    expect(bodies).toEqual([]);
    rerender({ ready: true });
    await vi.waitFor(() => expect(bodies).toEqual([{ force: false, check: true }]));
  });

  it('does not ask after Not now, a Disconnect, or when already connected', async () => {
    for (const status of ['declined', 'connected']) {
      signedIn();
      const { bodies, spy } = daemon([{ status }]);
      const dialog = answerDialog(true);
      await askToConnectAfterSignIn();
      dialog.restore();
      expect(dialog.opened).toHaveLength(0);
      expect(bodies).toEqual([{ force: false, check: true }]);
      spy.mockRestore();
    }
  });
});

