import { noteMachineBackends } from './lib/deviceIdentity';
import { isEmbedded } from './embed';
import { setApiPrincipal, setApiPrincipalAccess, routingPrincipalEpoch } from './api';
import type { SupabaseClient, Session } from '@supabase/supabase-js';
import { create } from 'zustand';
import { configureApi, setBackend } from './api';
import { setCloudDualBackend } from './dualBackend';
import { debugLog } from './debug';
import { unsubscribePushFromCloud } from './push';
import { isPreviewBuild } from './previewBoot';
import { inDesktopApp, signupMetadata } from './lib/arrival';
import { DEFAULT_SITE_INSTALLER, resolveSiteInstaller, type SiteInstaller } from './lib/siteInstaller';

export interface CloudConfig {
  supabaseUrl: string;
  supabaseAnonKey: string;
  vapidPublicKey?: string; // optional — absent if cloud hasn't been configured for push
  // Set to true by the LOCAL daemon's /api/config. Means "auth is
  // required, but skip the machine picker — this host is the only
  // machine and API calls stay un-prefixed (no /m/<id>/)." Absent on
  // the cloud relay's /api/config, so the cloud flow is unchanged.
  isLocal?: boolean;
  // Local daemon only: the app works without an account; signing in only
  // adds cloud features (phone access, relay, push).
  accountOptional?: boolean;
  // Extra sign-in providers enabled on the Supabase project, e.g. ['google'].
  authProviders?: string[];
  // Cloud only: the website's installer file and its size label, for the
  // get-started card. Absent means the built-in defaults (lib/siteLinks).
  siteInstallerUrl?: string;
  siteInstallerSize?: string;
}

const NO_ACCOUNT_STORAGE_KEY = 'solu.localNoAccount';

function storedWithoutAccount(): boolean {
  try {
    return localStorage.getItem(NO_ACCOUNT_STORAGE_KEY) === '1';
  } catch {
    return false;
  }
}

export interface CloudMachine {
  id: string;
  name: string;
  online: boolean;
  reconnecting?: boolean;
  lastSeenAt?: string;
  // IDs of daemons currently relayed to the cloud for this machine.
  // Single-daemon hosts report e.g. ["host"] or [""] (legacy); Tauri
  // Dual installs report ["wsl", "win"]. When length > 1 the frontend
  // enables dual-backend UI — /m/<mid>/b/<bid>/api/* routing,
  // per-backend sidebar, L/W switcher on cross-backend workspace
  // clicks. Absent / empty ≡ machine is offline.
  backends: string[];
  // Versions the daemon last announced at relay handshake. The daemon and the
  // shell (installer) update on separate channels, so they can differ.
  // Absent until the machine has connected with a version-announcing daemon.
  daemonVersion?: string;
  shellVersion?: string;
  shellPlatform?: string;
  // Who this machine belongs to. For your own machines this is you; on a
  // teammate drill-down it's the teammate. Lets the UI label a machine by
  // login, not just its self-assigned name.
  ownerEmail?: string;
  ownerName?: string;
}

// Mode:
//   'detecting' — fetching /api/config to decide.
//   'local'     — no /api/config, behave as before (no auth, no picker).
//                 Legacy path: old daemons (or dev builds with no Supabase
//                 creds baked in) fall here.
//   'cloud'     — /api/config exists; run Supabase flow. Sub-branch is
//                 controlled by the `isLocal` flag below:
//                   isLocal=false (default) → full cloud flow: Login →
//                     MachinePicker → App, with /m/<id>/ prefixing.
//                   isLocal=true (local daemon served /api/config) →
//                     Login → App directly, no picker, no prefix.
//                 See CloudGate.tsx for the rendering logic.
export type CloudMode = 'detecting' | 'local' | 'cloud';

function isHostedCloudOrigin(): boolean {
  if (typeof window === 'undefined') return false;
  const host = window.location.hostname;
  return (
    host === 'soluterminal-cloud.onrender.com' ||
    (host.endsWith('.onrender.com') && host.includes('soluterminal'))
  );
}

const SELECTED_MACHINE_STORAGE_KEY = 'solu.selectedMachineId';

function storedMachineId(): string | null {
  if (typeof window === 'undefined') return null;
  try {
    return localStorage.getItem(SELECTED_MACHINE_STORAGE_KEY);
  } catch {
    return null;
  }
}

function storeMachineId(id: string | null): void {
  if (typeof window === 'undefined') return;
  try {
    if (id) localStorage.setItem(SELECTED_MACHINE_STORAGE_KEY, id);
    else localStorage.removeItem(SELECTED_MACHINE_STORAGE_KEY);
  } catch { /* ignore */ }
}

function canSelectMachine(m: CloudMachine | undefined): m is CloudMachine {
  return !!m && (m.online || m.reconnecting === true) && Array.isArray(m.backends) && m.backends.length > 0;
}

interface CloudState {
  mode: CloudMode;
  // True when /api/config was served by the local daemon (carries
  // isLocal=true). Skips the machine-picker step and keeps API calls
  // un-prefixed. Always false in the cloud-relay flow.
  isLocal: boolean;
  supabase: SupabaseClient | null;
  session: Session | null;
  // Whether the signed-in user has a row in SoluEx's public.users
  // table — the marker that they came through SoluEx Dash Web rather
  // than signing up directly against Supabase from soluterminal.
  //   null  — not yet checked (Supabase session exists but we haven't
  //           queried, or no session at all).
  //   true  — row found; the user is a legit SoluEx account.
  //   false — no row; treat as unauthorised, render the access-denied
  //           screen instead of the app.
  // Kept as a tri-state (vs boolean) so CloudGate doesn't flash the
  // app for a frame between "session landed" and "check returned".
  authorised: boolean | null;
  machines: CloudMachine[];
  // The machine list has been fetched once for this session, so an empty
  // list means a new account rather than one still loading.
  machinesLoaded: boolean;
  selectedMachineId: string | null;
  error: string | null;
  vapidPublicKey: string | null; // from /api/config; used to subscribe to web push
  accountOptional: boolean;
  authProviders: string[];
  siteInstaller: SiteInstaller;
  // Local daemon used without signing in (accountOptional only).
  withoutAccount: boolean;
  continueWithoutAccount: () => void;
  requestSignIn: () => void;
  signInWithGoogle: () => Promise<void>;

  // Fetch /api/config to decide mode. If the endpoint exists we're in
  // cloud mode and initialise Supabase; otherwise we passthrough.
  bootstrap: () => Promise<void>;

  signIn: (email: string, password: string) => Promise<void>;
  // Email-first sign-in: which step to show for this email. "password" for
  // an account with a password, otherwise "code". Never throws: any failure
  // answers "code", which works for every account.
  lookupSigninMethod: (email: string) => Promise<'password' | 'code'>;
  // Two-step email OTP flow. sendOtpCode triggers Supabase to email a
  // 6-digit code; verifyOtpCode submits the code to establish the
  // session. Prefer this over magic-link in the Tauri desktop app:
  // clicking a magic link from an email client opens the default
  // browser, which lands the session in the wrong webview. OTP keeps
  // the whole flow inside the Tauri window.
  sendOtpCode: (email: string) => Promise<void>;
  verifyOtpCode: (email: string, code: string) => Promise<void>;
  recoveryPending: boolean;
  sendRecoveryCode: (email: string) => Promise<void>;
  verifyRecoveryCode: (email: string, code: string) => Promise<void>;
  completePasswordReset: (newPassword: string) => Promise<void>;
  cancelPasswordRecovery: () => void;
  // Stores the new-account card's "How did you hear about SoluTerminal?"
  // answer in the account's user_metadata, next to first_touch.
  saveHeardAbout: (answer: string) => Promise<void>;
  signOut: () => Promise<void>;

  refreshMachines: () => Promise<void>;
  selectMachine: (id: string) => void;
  deselectMachine: () => void;

  // Check whether the current session's user has a public.users row.
  // Sets state.authorised to true/false. Called automatically whenever
  // a session is established (bootstrap restore + onAuthStateChange).
  checkAuthorisation: () => Promise<void>;
}

export const useCloud = create<CloudState>((set, get) => ({
  mode: 'detecting',
  isLocal: false,
  supabase: null,
  session: null,
  authorised: null,
  machines: [],
  machinesLoaded: false,
  selectedMachineId: null,
  error: null,
  vapidPublicKey: null,
  recoveryPending: false,
  accountOptional: false,
  authProviders: [],
  siteInstaller: DEFAULT_SITE_INSTALLER,
  withoutAccount: false,

  continueWithoutAccount: () => {
    try { localStorage.setItem(NO_ACCOUNT_STORAGE_KEY, '1'); } catch { /* ignore */ }
    set({ withoutAccount: true });
  },
  requestSignIn: () => {
    try { localStorage.removeItem(NO_ACCOUNT_STORAGE_KEY); } catch { /* ignore */ }
    set({ withoutAccount: false });
  },
  signInWithGoogle: async () => {
    const sb = get().supabase;
    if (!sb) throw new Error('not ready');
    const { error } = await sb.auth.signInWithOAuth({
      provider: 'google',
      options: { redirectTo: window.location.origin + window.location.pathname },
    });
    if (error) throw error;
  },

  bootstrap: async () => {
    if (isPreviewBuild) {
      set({ mode: 'local', isLocal: false, error: null });
      return;
    }
    // Probe /api/config. 200 → cloud mode; 404/error → local mode.
    // Exception: when the app itself was loaded from the hosted cloud
    // origin, a transient config failure must not downgrade the UI into
    // unauthenticated local mode against the cloud host.
    let cfg: CloudConfig | null = null;
    let configError: unknown = null;
    try {
      const res = await fetch('/api/config');
      if (res.ok) {
        const body = await res.json();
        if (body && body.supabaseUrl && body.supabaseAnonKey) cfg = body;
      }
    } catch (e) {
      configError = e;
    }

    if (!cfg) {
      if (isHostedCloudOrigin()) {
        const detail = configError instanceof Error ? configError.message : 'Cloud config unavailable';
        set({ mode: 'cloud', error: detail });
        return;
      }
      set({ mode: 'local' });
      return;
    }

    try {
      const { createClient } = await import('@supabase/supabase-js');
      const sb = createClient(cfg.supabaseUrl, cfg.supabaseAnonKey, {
        auth: {
          // Persist session in localStorage so reloads don't log us out.
          persistSession: true,
          autoRefreshToken: true,
          detectSessionInUrl: true,
        },
      });

      sb.auth.onAuthStateChange((_event, session) => {
        applySession(set, session);
        // Re-fetch machines whenever auth identity changes — but only
        // in the cloud-relay flow. Local-auth has nothing to list (one
        // machine, this one, reached without any /m prefix).
        if (session && !get().isLocal) void get().refreshMachines();
        // Re-check authorisation on every sign-in (and clear it on
        // sign-out). CloudGate gates the app on authorised === true
        // so a new sign-in flash-transitions null → true/false rather
        // than inheriting whatever the last user's verdict was.
        if (session) void get().checkAuthorisation();
        else set({ authorised: null });
      });

      const accountOptional = cfg.isLocal === true && cfg.accountOptional === true;
      set({
        supabase: sb,
        mode: accountOptional && isEmbedded() ? 'detecting' : 'cloud',
        isLocal: cfg.isLocal === true,
        vapidPublicKey: cfg.vapidPublicKey ?? null,
        accountOptional,
        authProviders: Array.isArray(cfg.authProviders) ? cfg.authProviders : [],
        siteInstaller: resolveSiteInstaller(cfg.siteInstallerUrl, cfg.siteInstallerSize),
        withoutAccount: accountOptional && storedWithoutAccount(),
      });

      // Seed state from any existing session AFTER supabase is stored.
      // checkAuthorisation reads get().supabase; doing this before the set
      // left restored sessions stuck at authorised=null until another auth
      // event happened to fire.
      const { data } = await sb.auth.getSession();
      set({
        // Optional embeds stay detecting until the session/default is known:
        // no Login flash or skipped-account state for a restored session.
        mode: 'cloud',
        // This default is memory-only. Settings can requestSignIn(); a new
        // standalone document reads only the explicit skip preference.
        withoutAccount: accountOptional && (storedWithoutAccount() || (isEmbedded() && !data.session)),
      });
      applySession(set, data.session);
      if (data.session) void get().checkAuthorisation();
      if (data.session && !cfg.isLocal) void get().refreshMachines();
    } catch (e: any) {
      set({ error: e.message ?? String(e), mode: 'cloud' });
    }
  },

  signIn: async (email, password) => {
    const sb = get().supabase;
    if (!sb) throw new Error('not ready');
    const { error } = await sb.auth.signInWithPassword({ email, password });
    if (error) throw error;
  },

  lookupSigninMethod: async (email) => {
    try {
      // Same origin in both places: the cloud answers it for the hosted
      // app, the local daemon forwards it for the desktop app.
      const res = await fetch('/api/auth/method', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: email.trim() }),
      });
      if (!res.ok) return 'code';
      const body = await res.json();
      return body?.method === 'password' ? 'password' : 'code';
    } catch {
      return 'code';
    }
  },

  sendOtpCode: async (email) => {
    const sb = get().supabase;
    if (!sb) throw new Error('not ready');
    // emailRedirectTo is a routing signal, not a link: the shared project's
    // auth email router (supabase/functions/send-email) brands any email
    // whose redirect is a SoluTerminal host as SoluTerminal and sends its
    // code-only template, whatever product the account was created in.
    // Self-serve: an unknown email gets an account. signup_app lets the
    // shared project's email templates brand the mail for this product.
    // The hosted app in a browser also sends where the visitor came from
    // (first-touch tags) and the sign-up surface; Supabase keeps them only
    // when it creates the account. The desktop app sends neither.
    const data = get().isLocal || inDesktopApp()
      ? { signup_app: 'soluterminal' }
      : { ...signupMetadata(), signup_app: 'soluterminal' };
    const { error } = await sb.auth.signInWithOtp({
      email,
      options: { shouldCreateUser: true, data, emailRedirectTo: authEmailRedirect(get().isLocal) },
    });
    if (error) throw error;
  },
  verifyOtpCode: async (email, code) => {
    const sb = get().supabase;
    if (!sb) throw new Error('not ready');
    const { error } = await sb.auth.verifyOtp({
      email,
      token: code.trim(),
      type: 'email',
    });
    if (error) throw error;
  },

  sendRecoveryCode: async (email) => {
    const sb = get().supabase;
    if (!sb) throw new Error('not ready');
    // redirectTo only tells the auth email router this reset came from
    // SoluTerminal; its template carries the code and no link, so recovery
    // stays inside the app.
    const { error } = await sb.auth.resetPasswordForEmail(email, {
      redirectTo: authEmailRedirect(get().isLocal),
    });
    if (error) throw error;
  },
  verifyRecoveryCode: async (email, code) => {
    const sb = get().supabase;
    if (!sb) throw new Error('not ready');
    const { error } = await sb.auth.verifyOtp({
      email,
      token: code.trim(),
      type: 'recovery',
    });
    if (error) throw error;
    set({ recoveryPending: true });
  },
  completePasswordReset: async (newPassword) => {
    const sb = get().supabase;
    if (!sb) throw new Error('not ready');
    const { error } = await sb.auth.updateUser({ password: newPassword });
    if (error) throw error;
    await sb.auth.signOut({ scope: 'local' });
    // onAuthStateChange normally applies this first, but also update the
    // store here so a delayed/missing auth callback cannot briefly expose
    // the authorised app after recoveryPending is cleared.
    applySession(set, null);
  },
  cancelPasswordRecovery: () => {
    set({ recoveryPending: false });
  },
  saveHeardAbout: async (answer) => {
    const sb = get().supabase;
    if (!sb) throw new Error('not ready');
    // A data-only update merges this one key into user_metadata; it sends
    // no email and leaves first_touch and the other sign-up keys alone.
    const { error } = await sb.auth.updateUser({ data: { heard_about: answer } });
    if (error) throw error;
  },

  signOut: async () => {
    // Leave a console trail so we can tell from a user's devtools
    // whether their click even reached this code path. Every prior
    // "sign-out still doesn't work" investigation has spent most of
    // its time re-confirming that the handler fires at all.
    debugLog('[signOut] invoked');
    setApiPrincipal(null, true);

    // Step 1: wipe localStorage synchronously FIRST. This is the only
    // source-of-truth Supabase rehydrates from on next bootstrap, so
    // nuking it here guarantees a fresh state even if every async
    // call below hangs or throws.
    try {
      for (let i = localStorage.length - 1; i >= 0; i--) {
        const k = localStorage.key(i);
        if (k && (k.startsWith('sb-') || k.includes('supabase.auth.token'))) {
          localStorage.removeItem(k);
        }
      }
    } catch { /* ignore */ }

    // Step 2: best-effort push-subscription teardown + Supabase local
    // signOut, both bounded by a 1s timeout. Before the timeout was
    // added, either call could stall the whole flow in the Tauri
    // WebView (network requests pending, dialog ACL throw, etc.) and
    // the Sign out button silently appeared to do nothing.
    const bearer = get().session?.access_token ?? null;
    const bounded = <T>(p: Promise<T>, ms = 1000) =>
      Promise.race([
        p,
        new Promise<T>((_, rej) => setTimeout(() => rej(new Error('signOut step timeout')), ms)),
      ]);
    if (bearer) {
      try { await bounded(unsubscribePushFromCloud(bearer)); } catch { /* best effort */ }
    }
    const sb = get().supabase;
    if (sb) {
      try {
        await bounded(sb.auth.signOut({ scope: 'local' }));
      } catch {
        // Swallow — localStorage is already cleared and we're about to
        // hard-reload anyway.
      }
    }

    // Step 3: clear the Zustand store so any remaining render pass
    // that wins the race against the reload gets a clean !session
    // state and redirects to Login.
    applySession(set, null);
    storeMachineId(null);
    set({ selectedMachineId: null, machines: [], machinesLoaded: false, authorised: null, recoveryPending: false });

    // Step 4: hard reload. This is the reliable kill-switch — whatever
    // auto-refresh token timer, cached session object, or stale
    // React state might be holding on gets obliterated. CloudGate
    // bootstraps fresh from an empty localStorage → mode='cloud',
    // session=null → Login screen. Daemon-side panes keep running
    // across the reload so the user loses no work; their layout
    // restores on next sign-in via the usual autosave path.
    debugLog('[signOut] reloading');
    try {
      window.location.reload();
    } catch {
      // If reload is somehow blocked (Tauri ACL?), fall back to
      // setting the href — functionally equivalent in a SPA.
      window.location.href = window.location.href;
    }
  },

  // Verifies the signed-in user has a row in SoluEx's public.users
  // table — our proxy for "this user came through SoluEx Dash Web"
  // rather than signing up directly against the Supabase endpoint
  // from a soluterminal binary. Sets authorised to true/false on
  // success; leaves it null on transient errors (network drop, etc.)
  // so the user gets a retry on the next session event rather than
  // being locked out by a flaky connection.
  //
  // Uses maybeSingle() so a missing row returns data=null, error=null
  // (instead of throwing PGRST116). RLS on public.users is assumed to
  // permit a logged-in user to read their own row; if it doesn't, the
  // query returns null and the user is denied access — which is the
  // correct failure mode (better to over-deny than over-allow).
  checkAuthorisation: async () => {
    const sb = get().supabase;
    const session = get().session;
    const epoch = routingPrincipalEpoch();
    if (!sb || !session) {
      set({ authorised: null });
      return;
    }
    try {
      const { data, error } = await sb
        .from('users')
        .select('id')
        .eq('id', session.user.id)
        .maybeSingle();
      if (epoch !== routingPrincipalEpoch() || get().session?.user.id !== session.user.id) return;
      if (error) {
        // Network or RLS error. Before the app is admitted, keep the
        // tri-state pending so CloudGate holds on "Checking access…".
        // After admission, keep the previous true verdict; transient
        // token-refresh/network failures during desktop restore must
        // not unmount the whole App and dispose every xterm surface.
        // eslint-disable-next-line no-console
        console.warn('[checkAuthorisation] query error:', error.message);
        set({ authorised: get().authorised === true ? true : null });
        return;
      }
      if (!data) setApiPrincipal(session.user.id, true);
      setApiPrincipalAccess(!!data);
      set({ authorised: !!data });
    } catch (e) {
      if (epoch !== routingPrincipalEpoch() || get().session?.user.id !== session.user.id) return;
      // eslint-disable-next-line no-console
      console.warn('[checkAuthorisation] threw:', (e as Error).message);
      set({ authorised: get().authorised === true ? true : null });
    }
  },

  refreshMachines: async () => {
    try {
      const res = await fetch('/api/machines', {
        headers: authHeader(get().session),
      });
      if (!res.ok) throw new Error(`machines fetch: ${res.status}`);
      const machines: CloudMachine[] = await res.json();
      set({ machines, machinesLoaded: true });
      for (const m of machines) noteMachineBackends(m.id, Array.isArray(m.backends) ? m.backends : []);
      const selected = get().selectedMachineId;
      const requested = requestedMachineIdFromLocation();
      if (requested && selected !== requested) {
        const m = machines.find((x) => x.id === requested);
        if (canSelectMachine(m)) {
          get().selectMachine(requested);
          return;
        }
      }
      const remembered = storedMachineId();
      if (!selected && remembered && remembered !== requested) {
        const m = machines.find((x) => x.id === remembered);
        if (canSelectMachine(m)) {
          get().selectMachine(remembered);
          return;
        }
      }
      if (selected) {
        const m = machines.find((x) => x.id === selected);
        const online = m ? m.online : null;
        const backends = m ? m.backends : null;
        if (!canSelectMachine(m)) {
          // Treat machine-list misses/offline flips as stale data once
          // a machine is selected. Clearing selectedMachineId here makes
          // CloudGate render MachinePicker and unmount App, which turns
          // a transient cloud/relay blip during desktop restore into a
          // full terminal remount. Keep the existing prefix/backend; the
          // pane sockets will show reconnecting if the machine is truly
          // offline, and explicit deselect/sign-out still clears state.
          debugLog('[cloud] selected machine temporarily unavailable; keeping current selection', {
            selected,
            online,
            backends,
          });
        }
      }
    } catch (e: any) {
      set({ error: e.message ?? String(e) });
    }
  },

  selectMachine: (id) => {
    set({ selectedMachineId: id });
    storeMachineId(id);
    const tok = get().session?.access_token ?? null;
    configureApi({ prefix: `/m/${id}`, token: tok });

    // Enable dual-backend routing if this machine has multiple daemons
    // (the Tauri Dual install reports ["wsl", "win"]). Flipping the
    // cloud-side flag makes isRouterMode() → true, which the store's
    // bootstrap uses to fetch /m/<mid>/b/wsl/api/* AND
    // /m/<mid>/b/win/api/* in parallel and merge them into a single
    // sidebar. Also pin an initial active backend so /b/<bid>/api/*
    // URLs are built correctly until the first switchBackend call.
    const m = get().machines.find((x) => x.id === id);
    const multi = !!m && Array.isArray(m.backends) && m.backends.length > 1;
    setCloudDualBackend(multi);
    if (multi) {
      // Prefer WSL — matches the desktop default and the active-backend
      // URL defaulting logic in activeBackendFromUrl().
      const initial = (m?.backends.includes('wsl') ? 'wsl' : m?.backends[0]) ?? 'wsl';
      setBackend(initial as 'wsl' | 'win');
      try {
        const url = new URL(window.location.href);
        url.searchParams.set('b', initial);
        window.history.replaceState(null, '', url.toString());
      } catch { /* ignore */ }
    } else {
      setBackend(null);
      try {
        const url = new URL(window.location.href);
        url.searchParams.delete('b');
        window.history.replaceState(null, '', url.toString());
      } catch { /* ignore */ }
    }
  },

  deselectMachine: () => {
    set({ selectedMachineId: null });
    storeMachineId(null);
    configureApi({ prefix: '', token: null });
    // Leaving the machine turns off dual-backend. Reset so reselecting
    // another (single-backend) machine doesn't inherit the flag.
    setCloudDualBackend(false);
    setBackend(null);
  },
}));

/**
 * A session is relayed only when the UI reaches the daemon through the cloud
 * relay. Relayed requests use the `/m/<machineId>/` prefix: cloud/proxy.go
 * stamps X-Forwarded-Prefix, which the daemon's fileAccessOverRelay checks.
 * Cloud sign-in alone is not remote: the local daemon serves `/api/config`
 * with `isLocal: true` (daemon/config.go), and API calls stay un-prefixed.
 * This mirrors StatusBar ConnChip (`relayed = !isLocal` in cloud mode) and
 * BackendSwitcher (`cloudMode === 'cloud' && !cloudIsLocal`). Settings
 * that require a cloud session deliberately keep gating on cloud mode,
 * independently of whether a relay is in use.
 */
// Fixed SoluTerminal address for sign-ins started in the desktop app, whose
// window origin is the local daemon. It is on the project's redirect allow-list.
export const DESKTOP_AUTH_REDIRECT = 'https://soluterminal-cloud.onrender.com/auth/signin';

/**
 * The redirect sent with SoluTerminal's sign-in and reset emails. The shared
 * Supabase project routes on its host (supabase/functions/send-email/route.ts),
 * so accounts with no signup_app tag, or another product's, still get
 * SoluTerminal's sender and code template when they sign in here.
 */
export function authEmailRedirect(isLocal: boolean): string {
  if (isLocal || inDesktopApp() || typeof window === 'undefined') return DESKTOP_AUTH_REDIRECT;
  const origin = window.location.origin;
  if (!/^https:\/\//.test(origin)) return DESKTOP_AUTH_REDIRECT;
  return `${origin}/auth/signin`;
}

export function isRelayedSession(state: Pick<CloudState, 'mode' | 'isLocal'>): boolean {
  return state.mode === 'cloud' && !state.isLocal;
}

function applySession(set: (s: Partial<CloudState>) => void, session: Session | null) {
  const previousEpoch = routingPrincipalEpoch();
  setApiPrincipal(session?.user.id ?? null);
  set(session ? { session, ...(previousEpoch !== routingPrincipalEpoch() ? {authorised:null} : {}) } : { session, recoveryPending: false });
  // Refresh the api layer's token so subsequent calls carry it. We
  // don't set a prefix here — that only happens once a machine is
  // picked.
  configureApi({ token: session?.access_token ?? null });
}

function authHeader(session: Session | null): HeadersInit {
  if (!session) return {};
  return { Authorization: `Bearer ${session.access_token}` };
}

export function machineIdFromPath(pathname: string): string | null {
  const match = /^\/m\/([^/]+)(?:\/|$)/.exec(pathname);
  if (!match) return null;
  try {
    return decodeURIComponent(match[1]);
  } catch {
    return null;
  }
}

function requestedMachineIdFromLocation(): string | null {
  if (typeof window === 'undefined') return null;
  return machineIdFromPath(window.location.pathname);
}

// DEV-only hook for local UI tooling (e.g. playwright driving `vite dev`):
// lets a test driver inject a fake session to exercise signed-in-only chrome
// (account footer) without real Supabase creds. import.meta.env.DEV is
// statically false in production builds, so this is dead-code-eliminated.
if (import.meta.env.DEV && typeof window !== 'undefined') {
  const w = window as unknown as { __soluDev?: Record<string, unknown> };
  w.__soluDev = { ...w.__soluDev, useCloud };
}
