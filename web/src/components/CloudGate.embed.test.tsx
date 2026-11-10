import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useCloud } from '../cloud';
import { __recomputeEmbedForTests } from '../embed';
import { CloudGate } from './CloudGate';
import { AccountTab } from './settings/AccountTab';
import type { Session } from '@supabase/supabase-js';

const restored = vi.hoisted(() => ({ session: null as Session | null, wait: null as Promise<void> | null }));

vi.mock('@supabase/supabase-js', () => ({
  createClient: () => ({ auth: {
    onAuthStateChange: vi.fn(),
    getSession: async () => {
      await restored.wait;
      return { data: { session: restored.session } };
    },
  } }),
}));

const config = {
  supabaseUrl: 'https://auth.example.test', supabaseAnonKey: 'fixture-key',
  isLocal: true, accountOptional: true,
};

function embedded(framed = true) {
  __recomputeEmbedForTests({
    framed, search: '?embed=shinoque', saved: null,
    ancestorOrigin: 'http://window.shinoque.localhost:14700', referrer: '',
  });
}

function AppWithAccountSettings() {
  return <><div>Local app</div><AccountTab /></>;
}

beforeEach(() => {
  localStorage.clear();
  restored.session = null;
  restored.wait = null;
  useCloud.setState(useCloud.getInitialState(), true);
  embedded();
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(config))));
});

afterEach(() => {
  cleanup();
  useCloud.setState(useCloud.getInitialState(), true);
  __recomputeEmbedForTests({ framed: false, search: '', saved: null, ancestorOrigin: '', referrer: '' });
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('CloudGate optional-account embeds', () => {
  it('does not flash the sign-in card while the stored session is loading', async () => {
    let finish!: () => void;
    restored.wait = new Promise<void>((resolve) => { finish = resolve; });
    render(<CloudGate><div>Local app</div></CloudGate>);
    await waitFor(() => expect(useCloud.getState().supabase).not.toBeNull());
    expect(screen.getByText('Loading soluterminal…')).toBeInTheDocument();
    expect(screen.queryByLabelText(/^email$/i)).toBeNull();
    await act(async () => finish());
    expect(await screen.findByText('Local app')).toBeInTheDocument();
    expect(screen.queryByLabelText(/^email$/i)).toBeNull();
  });

  it('preserves immediate standalone entry for an explicit skip while session loading waits', async () => {
    embedded(false);
    localStorage.setItem('solu.localNoAccount', '1');
    let finish!: () => void;
    restored.wait = new Promise<void>((resolve) => { finish = resolve; });
    render(<CloudGate><div>Local app</div></CloudGate>);
    expect(await screen.findByText('Local app')).toBeInTheDocument();
    expect(screen.queryByText('Loading soluterminal…')).toBeNull();
    await act(async () => finish());
  });

  it('skips the card on a fresh origin and keeps sign-in reachable from Account settings', async () => {
    render(<CloudGate><AppWithAccountSettings /></CloudGate>);
    expect(await screen.findByText('Local app')).toBeInTheDocument();
    expect(screen.queryByLabelText(/^email$/i)).toBeNull();
    expect(screen.queryByRole('button', { name: /skip for now/i })).toBeNull();
    expect(useCloud.getState().withoutAccount).toBe(true);
    expect(localStorage.getItem('solu.localNoAccount')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Create free account or sign in' }));
    expect(await screen.findByLabelText(/^email$/i)).toBeInTheDocument();
    expect(screen.queryByText('Local app')).toBeNull();
    expect(useCloud.getState().withoutAccount).toBe(false);
  });

  it('shows the standalone card on the same origin after an embedded load', async () => {
    const view = render(<CloudGate><div>Local app</div></CloudGate>);
    await screen.findByText('Local app');
    view.unmount();
    embedded(false);
    useCloud.setState(useCloud.getInitialState(), true);
    render(<CloudGate><div>Standalone app</div></CloudGate>);
    expect(await screen.findByLabelText(/^email$/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /skip for now/i })).toBeInTheDocument();
    expect(screen.queryByText('Standalone app')).toBeNull();
    expect(localStorage.getItem('solu.localNoAccount')).toBeNull();
  });

  it.each([
    { isLocal: true, accountOptional: false },
    { isLocal: false, accountOptional: true },
  ])('keeps required-account and hosted login gates: %j', async (overrides) => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ ...config, ...overrides }))));
    render(<CloudGate><div>Local app</div></CloudGate>);
    expect(await screen.findByLabelText(/^email$/i)).toBeInTheDocument();
    expect(useCloud.getState().withoutAccount).toBe(false);
    expect(screen.queryByText('Local app')).toBeNull();
  });

  it('preserves restored account authorization instead of marking it skipped', async () => {
    restored.session = { access_token: 'fixture-token', user: { id: 'fixture-user' } } as Session;
    const checkAuthorisation = vi.fn(async () => {});
    useCloud.setState({ checkAuthorisation });
    render(<CloudGate><div>Local app</div></CloudGate>);
    expect(await screen.findByText('Checking access…')).toBeInTheDocument();
    expect(useCloud.getState().withoutAccount).toBe(false);
    expect(checkAuthorisation).toHaveBeenCalledOnce();
    expect(screen.queryByText('Local app')).toBeNull();
  });

  it('preserves the explicit standalone skip preference', async () => {
    embedded(false);
    useCloud.getState().continueWithoutAccount();
    useCloud.setState(useCloud.getInitialState(), true);
    render(<CloudGate><div>Local app</div></CloudGate>);
    await waitFor(() => expect(screen.getByText('Local app')).toBeInTheDocument());
    expect(localStorage.getItem('solu.localNoAccount')).toBe('1');
  });
});
