import { useCallback, useEffect, useState } from 'react';
import { useCloud } from '../../cloud';
import { Login } from '../../components/CloudLogin';
import './accountDelete.css';

// /account/delete on the hosted app: self-serve account deletion (ST-174).
// soluterminal.com/account/delete explains it and links here; the phone app's
// Settings › Delete account opens that page. The cloud enforces every rule
// (cloud/account_delete.go); this page only explains and asks.

export const DELETION_PLAN_ENDPOINT = '/api/account/deletion';
export const DELETE_ENDPOINT = '/api/account/delete';
/** Matches the cloud's accountDeleteFreshness. */
export const FRESH_SIGN_IN_MS = 10 * 60 * 1000;

export interface DeletionPlan {
  available: boolean;
  refused?: string;
  keepSignIn: boolean;
  otherProducts?: string[];
  computers: number;
}

type View =
  | { kind: 'loading' }
  | { kind: 'signedOut' }
  | { kind: 'plan'; plan: DeletionPlan; signedInAt: number | null }
  | { kind: 'deleting' }
  | { kind: 'deleted'; signIn: string; appleRevoke: string }
  | { kind: 'error'; message: string };

/** The newest amr timestamp (ms) in a Supabase access token, or null. */
export function lastSignInFromToken(token: string): number | null {
  try {
    const part = token.split('.')[1] ?? '';
    const json = atob(part.replace(/-/g, '+').replace(/_/g, '/').padEnd(Math.ceil(part.length / 4) * 4, '='));
    const amr = (JSON.parse(json) as { amr?: { timestamp?: number }[] }).amr ?? [];
    const newest = Math.max(0, ...amr.map((a) => (typeof a.timestamp === 'number' ? a.timestamp : 0)));
    return newest > 0 ? newest * 1000 : null;
  } catch {
    return null;
  }
}

export function isDeletionPlan(v: unknown): v is DeletionPlan {
  const p = v as DeletionPlan | null;
  return !!p && typeof p.available === 'boolean' && typeof p.keepSignIn === 'boolean' && typeof p.computers === 'number';
}

/** "Alpha and Beta", "Alpha, Beta and Gamma". */
export function joinProducts(names: string[]): string {
  if (names.length <= 1) return names[0] ?? 'another of our apps';
  return `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`;
}

async function accessToken(): Promise<string | null> {
  const sb = useCloud.getState().supabase;
  if (!sb) return null;
  const { data } = await sb.auth.getSession();
  return data.session?.access_token ?? null;
}

export default function AccountDeletePage() {
  const mode = useCloud((s) => s.mode);
  const bootstrap = useCloud((s) => s.bootstrap);
  const userId = useCloud((s) => s.session?.user?.id ?? null);
  const email = useCloud((s) => s.session?.user?.email ?? '');
  const signOut = useCloud((s) => s.signOut);
  const [view, setView] = useState<View>({ kind: 'loading' });
  const [typed, setTyped] = useState('');
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    document.title = 'Delete your account · SoluTerminal';
    const meta = document.createElement('meta');
    meta.name = 'robots';
    meta.content = 'noindex, nofollow';
    document.head.appendChild(meta);
    return () => meta.remove();
  }, []);
  useEffect(() => { void bootstrap(); }, [bootstrap]);
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), 15_000);
    return () => window.clearInterval(t);
  }, []);

  useEffect(() => {
    if (mode === 'detecting') return;
    if (view.kind === 'deleted' || view.kind === 'deleting') return;
    if (mode !== 'cloud' || !userId) {
      setView(mode === 'cloud' ? { kind: 'signedOut' } : { kind: 'error', message: 'Open this page at soluterminal.com/account/delete.' });
      return;
    }
    let cancelled = false;
    void (async () => {
      const token = await accessToken();
      if (cancelled) return;
      if (!token) { setView({ kind: 'signedOut' }); return; }
      try {
        const res = await fetch(DELETION_PLAN_ENDPOINT, { headers: { Authorization: `Bearer ${token}` }, cache: 'no-store' });
        const body: unknown = await res.json().catch(() => null);
        if (cancelled) return;
        if (!res.ok || !isDeletionPlan(body)) {
          setView({ kind: 'error', message: "Account deletion isn't available right now. Try again later." });
          return;
        }
        setView({ kind: 'plan', plan: body, signedInAt: lastSignInFromToken(token) });
      } catch {
        if (!cancelled) setView({ kind: 'error', message: "We couldn't reach SoluTerminal. Check your connection and try again." });
      }
    })();
    return () => { cancelled = true; };
    // view is read only to skip reloading after a deletion has started.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode, userId]);

  const signInAgain = useCallback(() => {
    setTyped('');
    void signOut();
  }, [signOut]);

  const confirmDelete = useCallback(async () => {
    setView({ kind: 'deleting' });
    const token = await accessToken();
    if (!token) { setView({ kind: 'signedOut' }); return; }
    try {
      const res = await fetch(DELETE_ENDPOINT, {
        method: 'POST',
        headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ confirm: 'DELETE' }),
      });
      const body = (await res.json().catch(() => null)) as
        | { error?: string; message?: string; signIn?: string; appleRevoke?: string }
        | null;
      if (res.ok) {
        setView({ kind: 'deleted', signIn: body?.signIn ?? 'deleted', appleRevoke: body?.appleRevoke ?? '' });
        void useCloud.getState().supabase?.auth.signOut({ scope: 'local' });
        return;
      }
      if (body?.error === 'reauth_required') {
        signInAgain();
        return;
      }
      setView({ kind: 'error', message: body?.message ?? "We couldn't delete your account. Nothing was deleted; try again later." });
    } catch {
      setView({ kind: 'error', message: "We couldn't reach SoluTerminal. Nothing was deleted; try again." });
    }
  }, [signInAgain]);

  return (
    <div className="ad-shell">
      <main className="ad-card">
        <h1>Delete your SoluTerminal account</h1>
        {view.kind === 'loading' && <p className="ad-muted" role="status">Loading…</p>}
        {view.kind === 'signedOut' && (
          <>
            <p>Sign in to the account you want to delete.</p>
            <Login />
          </>
        )}
        {view.kind === 'error' && <p role="alert">{view.message}</p>}
        {view.kind === 'deleting' && <p role="status">Deleting your account…</p>}
        {view.kind === 'deleted' && (
          <div role="status">
            <p><strong>Your account was deleted.</strong> You've been signed out.</p>
            {view.signIn === 'kept' ? (
              <p>Your sign-in stays for our other apps you use. Signing in to SoluTerminal again starts a fresh, empty account.</p>
            ) : (
              <p>Your sign-in was deleted too.</p>
            )}
            {view.appleRevoke === 'none_available' && (
              <p>
                You signed in with Apple. To remove SoluTerminal from your Apple ID too, open Settings › your name ›
                Sign in with Apple (or appleid.apple.com) and remove SoluTerminal.
              </p>
            )}
            <p className="ad-muted">SoluTerminal's files on your own computer stay there until you uninstall it.</p>
          </div>
        )}
        {view.kind === 'plan' && <PlanView plan={view.plan} email={email} signedInAt={view.signedInAt} now={now}
          typed={typed} onType={setTyped} onSignInAgain={signInAgain} onDelete={() => void confirmDelete()} />}
        <p className="ad-muted ad-foot">
          What's deleted and what's kept: <a href="https://soluterminal.com/account/delete/">soluterminal.com/account/delete</a>
        </p>
      </main>
    </div>
  );
}

function PlanView(props: {
  plan: DeletionPlan;
  email: string;
  signedInAt: number | null;
  now: number;
  typed: string;
  onType: (v: string) => void;
  onSignInAgain: () => void;
  onDelete: () => void;
}) {
  const { plan, email, signedInAt, now, typed, onType, onSignInAgain, onDelete } = props;
  if (plan.refused === 'team_account') {
    return (
      <p>
        This is a team account. Team accounts are deleted on request: email{' '}
        <a href="mailto:hello@soluterminal.com">hello@soluterminal.com</a> from {email || "this account's address"}.
      </p>
    );
  }
  if (!plan.available) {
    return (
      <p>
        Account deletion isn't available right now. Email <a href="mailto:hello@soluterminal.com">hello@soluterminal.com</a>{' '}
        and we'll do it for you.
      </p>
    );
  }
  const fresh = signedInAt !== null && now - signedInAt < FRESH_SIGN_IN_MS;
  return (
    <>
      <p>Signed in as <strong>{email}</strong>.</p>
      <p>This deletes, at once and for good:</p>
      <ul>
        <li>
          {plan.computers === 1 ? 'Your connected computer' : `Your ${plan.computers} connected computers`}
          {' '}(signed out of SoluTerminal's cloud), and your phone and browser notifications.
        </li>
        <li>Your settings, usage, crash and update reports, and your feedback reports.</li>
      </ul>
      {plan.keepSignIn ? (
        <p className="ad-note">
          Your sign-in stays because you also use {joinProducts(plan.otherProducts ?? [])}; SoluTerminal's data is
          deleted. You'll be signed out of all our apps, and signing in to SoluTerminal again starts a fresh account. To
          delete the sign-in everywhere, email <a href="mailto:hello@soluterminal.com">hello@soluterminal.com</a>.
        </p>
      ) : (
        <p className="ad-note">Your sign-in is deleted too.</p>
      )}
      {!fresh ? (
        <>
          <p>For your safety, sign in again before deleting your account.</p>
          <button type="button" className="ad-button" onClick={onSignInAgain}>Sign in again</button>
        </>
      ) : (
        <form onSubmit={(e) => { e.preventDefault(); if (typed === 'DELETE') onDelete(); }}>
          <label htmlFor="ad-confirm">Type DELETE to confirm</label>
          <input id="ad-confirm" className="ad-input" autoComplete="off" spellCheck={false} value={typed}
            onChange={(e) => onType(e.target.value)} />
          <button type="submit" className="ad-button ad-danger" disabled={typed !== 'DELETE'}>
            Delete my account
          </button>
        </form>
      )}
    </>
  );
}
