import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  authEmailRedirect,
  DESKTOP_AUTH_REDIRECT,
  isRelayedSession,
  machineIdFromLocation,
  machineIdFromPath,
  useCloud,
  webSignInProviders,
  type CloudMachine,
} from './cloud';

const initialCloudState = useCloud.getInitialState();
const selectedMachineStorageKey = 'solu.selectedMachineId';

describe('webSignInProviders', () => {
  it('uses the website list on the cloud, not the phone list', () => {
    expect(webSignInProviders({ authProviders: ['google', 'apple'], webAuthProviders: [] })).toEqual([]);
    expect(webSignInProviders({ authProviders: ['google', 'apple'], webAuthProviders: ['google'] })).toEqual(['google']);
  });

  it('falls back to authProviders on a local daemon, which sends no website list', () => {
    expect(webSignInProviders({ authProviders: ['google'] })).toEqual(['google']);
    expect(webSignInProviders({})).toEqual([]);
  });
});

describe('machineIdFromPath', () => {
  it('parses machine deep-link paths', () => {
    expect(machineIdFromPath('/m/machine-123/')).toBe('machine-123');
    expect(machineIdFromPath('/m/machine%20with%20spaces')).toBe('machine with spaces');
  });

  it('ignores non-machine paths and malformed encodings', () => {
    expect(machineIdFromPath('/')).toBeNull();
    expect(machineIdFromPath('/machines/machine-123')).toBeNull();
    expect(machineIdFromPath('/m/%E0%A4%A')).toBeNull();
  });
});

describe('machineIdFromLocation', () => {
  it('prefers ?machine= (a push tap, ST-117) and falls back to the path', () => {
    expect(machineIdFromLocation('/', '?machine=m%201&pane=p1')).toBe('m 1');
    expect(machineIdFromLocation('/m/path-id/', '?machine=query-id')).toBe('query-id');
    expect(machineIdFromLocation('/m/path-id/', '?pane=p1')).toBe('path-id');
    expect(machineIdFromLocation('/', '?machine=')).toBeNull();
    expect(machineIdFromLocation('/', '')).toBeNull();
  });
});

describe('isRelayedSession', () => {
  it.each([
    { state: { mode: 'detecting' as const, isLocal: false }, expected: false },
    { state: { mode: 'local' as const, isLocal: false }, expected: false },
    { state: { mode: 'cloud' as const, isLocal: true }, expected: false },
    { state: { mode: 'cloud' as const, isLocal: false }, expected: true },
  ])('returns $expected for $state', ({ state, expected }) => {
    expect(isRelayedSession(state)).toBe(expected);
  });
});

describe('cloud machine deep links', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    useCloud.setState(initialCloudState, true);
    localStorage.removeItem(selectedMachineStorageKey);
    window.history.replaceState({}, '', '/');
  });

  it('selects an online machine requested by /m/<machineId> and preserves pane query', async () => {
    const machines: CloudMachine[] = [
      { id: 'other', name: 'Other', online: true, backends: ['host'] },
      { id: 'target', name: 'Target', online: true, backends: ['host'] },
    ];
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      json: async () => machines,
    } as Response);
    window.history.replaceState({}, '', '/m/target/?pane=pane-1');
    useCloud.setState({
      session: { access_token: 'token' } as any,
      selectedMachineId: null,
      machines: [],
    });

    await useCloud.getState().refreshMachines();

    expect(useCloud.getState().selectedMachineId).toBe('target');
    expect(window.location.pathname).toBe('/m/target/');
    expect(window.location.search).toBe('?pane=pane-1');
  });

  it('selects the machine a push tap names in ?machine=, then drops it from the URL', async () => {
    const machines: CloudMachine[] = [
      { id: 'remembered', name: 'Remembered', online: true, backends: ['host'] },
      { id: 'target', name: 'Target', online: true, backends: ['host'] },
    ];
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({ ok: true, json: async () => machines } as Response);
    localStorage.setItem(selectedMachineStorageKey, 'remembered');
    window.history.replaceState({}, '', '/?machine=target&pane=pane-1');
    useCloud.setState({ session: { access_token: 'token' } as any, selectedMachineId: null, machines: [] });

    await useCloud.getState().refreshMachines();

    expect(useCloud.getState().selectedMachineId).toBe('target');
    expect(window.location.pathname).toBe('/');
    expect(window.location.search).toBe('?pane=pane-1');

    // Picking another computer afterwards is not pulled back by a refresh.
    useCloud.getState().selectMachine('remembered');
    await useCloud.getState().refreshMachines();
    expect(useCloud.getState().selectedMachineId).toBe('remembered');
  });

  it('drops a ?machine= it cannot select, so it does not take over later', async () => {
    let online = false;
    vi.spyOn(globalThis, 'fetch').mockImplementation(async () => ({
      ok: true,
      json: async () => [
        { id: 'remembered', name: 'Remembered', online: true, backends: ['host'] },
        { id: 'target', name: 'Target', online, backends: online ? ['host'] : [] },
      ],
    }) as Response);
    localStorage.setItem(selectedMachineStorageKey, 'remembered');
    window.history.replaceState({}, '', '/?machine=target');
    useCloud.setState({ session: { access_token: 'token' } as any, selectedMachineId: null, machines: [] });

    await useCloud.getState().refreshMachines();
    expect(useCloud.getState().selectedMachineId).toBe('remembered');
    expect(window.location.search).toBe('');

    online = true;
    await useCloud.getState().refreshMachines();
    expect(useCloud.getState().selectedMachineId).toBe('remembered');
  });

  it('restores the last selected online machine during a normal PWA launch', async () => {
    const machines: CloudMachine[] = [
      { id: 'other', name: 'Other', online: true, backends: ['host'] },
      { id: 'remembered', name: 'Remembered', online: true, backends: ['host'] },
    ];
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      json: async () => machines,
    } as Response);
    localStorage.setItem(selectedMachineStorageKey, 'remembered');
    window.history.replaceState({}, '', '/');
    useCloud.setState({
      session: { access_token: 'token' } as any,
      selectedMachineId: null,
      machines: [],
    });

    await useCloud.getState().refreshMachines();

    expect(useCloud.getState().selectedMachineId).toBe('remembered');
  });

  it('persists and clears the selected machine explicitly', () => {
    const machines: CloudMachine[] = [
      { id: 'target', name: 'Target', online: true, backends: ['host'] },
    ];
    useCloud.setState({
      session: { access_token: 'token' } as any,
      selectedMachineId: null,
      machines,
    });

    useCloud.getState().selectMachine('target');
    expect(localStorage.getItem(selectedMachineStorageKey)).toBe('target');

    useCloud.getState().deselectMachine();
    expect(localStorage.getItem(selectedMachineStorageKey)).toBeNull();
  });
});

describe('password recovery actions', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    useCloud.setState(initialCloudState, true);
  });

  function recoveryClient() {
    return {
      auth: {
        resetPasswordForEmail: vi.fn(async () => ({ error: null })),
        verifyOtp: vi.fn(async () => ({ error: null })),
        updateUser: vi.fn(async () => ({ error: null })),
        signOut: vi.fn(async () => ({ error: null })),
      },
    };
  }

  it('sends a recovery code with a SoluTerminal redirect so the router brands it', async () => {
    const mockSb = recoveryClient();
    useCloud.setState({ supabase: mockSb as any, isLocal: true });

    await useCloud.getState().sendRecoveryCode('person@example.com');

    expect(mockSb.auth.resetPasswordForEmail).toHaveBeenCalledWith('person@example.com', {
      redirectTo: DESKTOP_AUTH_REDIRECT,
    });
  });

  it('verifies a trimmed recovery code and marks recovery pending', async () => {
    const mockSb = recoveryClient();
    useCloud.setState({ supabase: mockSb as any });

    await useCloud.getState().verifyRecoveryCode('person@example.com', ' 123456 ');

    expect(mockSb.auth.verifyOtp).toHaveBeenCalledWith({
      email: 'person@example.com',
      token: '123456',
      type: 'recovery',
    });
    expect(useCloud.getState().recoveryPending).toBe(true);
  });

  it('does not mark recovery pending when code verification fails', async () => {
    const mockSb = recoveryClient();
    const error = { code: 'otp_expired', message: 'expired' };
    mockSb.auth.verifyOtp.mockResolvedValueOnce({ error } as any);
    useCloud.setState({ supabase: mockSb as any });

    await expect(useCloud.getState().verifyRecoveryCode('person@example.com', '123456')).rejects.toBe(error);
    expect(useCloud.getState().recoveryPending).toBe(false);
  });

  it('updates the password, signs out locally, and clears recovery pending', async () => {
    const mockSb = recoveryClient();
    useCloud.setState({
      supabase: mockSb as any,
      session: { access_token: 'recovery-token' } as any,
      recoveryPending: true,
    });

    await useCloud.getState().completePasswordReset('new-password');

    expect(mockSb.auth.updateUser).toHaveBeenCalledWith({ password: 'new-password' });
    expect(mockSb.auth.signOut).toHaveBeenCalledWith({ scope: 'local' });
    expect(useCloud.getState().session).toBeNull();
    expect(useCloud.getState().recoveryPending).toBe(false);
  });

  it('leaves recovery pending and rethrows when the password update fails', async () => {
    const mockSb = recoveryClient();
    const error = { code: 'weak_password', message: 'Password is too short' };
    mockSb.auth.updateUser.mockResolvedValueOnce({ error } as any);
    useCloud.setState({ supabase: mockSb as any, recoveryPending: true });

    await expect(useCloud.getState().completePasswordReset('short')).rejects.toBe(error);
    expect(mockSb.auth.signOut).not.toHaveBeenCalled();
    expect(useCloud.getState().recoveryPending).toBe(true);
  });

  it('propagates thrown recovery action errors without changing pending state', async () => {
    const mockSb = recoveryClient();
    const networkError = new Error('network unavailable');
    useCloud.setState({ supabase: mockSb as any });

    mockSb.auth.resetPasswordForEmail.mockRejectedValueOnce(networkError);
    await expect(useCloud.getState().sendRecoveryCode('person@example.com')).rejects.toBe(networkError);

    mockSb.auth.verifyOtp.mockRejectedValueOnce(networkError);
    await expect(useCloud.getState().verifyRecoveryCode('person@example.com', '123456')).rejects.toBe(networkError);
    expect(useCloud.getState().recoveryPending).toBe(false);

    useCloud.setState({ recoveryPending: true });
    mockSb.auth.updateUser.mockRejectedValueOnce(networkError);
    await expect(useCloud.getState().completePasswordReset('new-password')).rejects.toBe(networkError);
    expect(mockSb.auth.signOut).not.toHaveBeenCalled();
    expect(useCloud.getState().recoveryPending).toBe(true);
  });

  it('cancels password recovery idempotently', () => {
    useCloud.setState({ recoveryPending: true });
    useCloud.getState().cancelPasswordRecovery();
    expect(useCloud.getState().recoveryPending).toBe(false);

    useCloud.getState().cancelPasswordRecovery();
    expect(useCloud.getState().recoveryPending).toBe(false);
  });

  it('clears stale recovery state during normal sign out', async () => {
    const mockSb = recoveryClient();
    useCloud.setState({
      supabase: mockSb as any,
      session: null,
      recoveryPending: true,
    });

    await useCloud.getState().signOut();

    expect(useCloud.getState().session).toBeNull();
    expect(useCloud.getState().recoveryPending).toBe(false);
  });

  it('requires Supabase to be ready for recovery actions', async () => {
    useCloud.setState({ supabase: null });

    await expect(useCloud.getState().sendRecoveryCode('person@example.com')).rejects.toThrow('not ready');
    await expect(useCloud.getState().verifyRecoveryCode('person@example.com', '123456')).rejects.toThrow('not ready');
    await expect(useCloud.getState().completePasswordReset('new-password')).rejects.toThrow('not ready');
  });
});

describe('auth email redirect', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    useCloud.setState(initialCloudState, true);
  });

  it('uses the fixed SoluTerminal address for the local daemon (desktop)', () => {
    expect(authEmailRedirect(true)).toBe(DESKTOP_AUTH_REDIRECT);
    expect(new URL(DESKTOP_AUTH_REDIRECT).protocol).toBe('https:');
  });

  it('uses the hosted origin in a browser over https', () => {
    vi.stubGlobal('location', { ...window.location, origin: 'https://app.soluterminal.com' } as Location);
    expect(authEmailRedirect(false)).toBe('https://app.soluterminal.com/auth/signin');
  });

  it('falls back to the fixed address on a non-https origin', () => {
    vi.stubGlobal('location', { ...window.location, origin: 'http://127.0.0.1:8787' } as Location);
    expect(authEmailRedirect(false)).toBe(DESKTOP_AUTH_REDIRECT);
  });

  it('sends the code request with the redirect and the signup tag', async () => {
    const signInWithOtp = vi.fn(async () => ({ error: null }));
    useCloud.setState({ supabase: { auth: { signInWithOtp } } as any, isLocal: true });

    await useCloud.getState().sendOtpCode('person@example.com');

    expect(signInWithOtp).toHaveBeenCalledWith({
      email: 'person@example.com',
      options: {
        shouldCreateUser: true,
        data: { signup_app: 'soluterminal' },
        emailRedirectTo: DESKTOP_AUTH_REDIRECT,
      },
    });
  });
});

describe('saveHeardAbout', () => {
  afterEach(() => {
    useCloud.setState(initialCloudState, true);
  });

  it('merges only heard_about into user_metadata', async () => {
    const updateUser = vi.fn(async () => ({ error: null }));
    useCloud.setState({ supabase: { auth: { updateUser } } as any });

    await useCloud.getState().saveHeardAbout('hacker_news');

    expect(updateUser).toHaveBeenCalledWith({ data: { heard_about: 'hacker_news' } });
  });

  it('rethrows a failed update', async () => {
    const error = new Error('offline');
    useCloud.setState({ supabase: { auth: { updateUser: vi.fn(async () => ({ error })) } } as any });

    await expect(useCloud.getState().saveHeardAbout('skipped')).rejects.toBe(error);
  });
});
