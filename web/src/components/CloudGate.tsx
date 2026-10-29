import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useCloud, type CloudMachine } from '../cloud';
import { lifecycleLog } from '../debug';
import { confirmDialog, alertDialog } from '../lib/dialog';
import { DialogHost } from './DialogHost';
import { detectVisitorOS } from './DesktopInstallerLink';
import { GET_STARTED_TITLE, GetStartedCard, SiteInstallerLink, usesWindowsDownload } from './GetStartedCard';
import { useCloudTeamAccount } from './useCloudTeamAccount';
import { BrandBar, brandShellClass, useHostedBrand } from './CloudBrand';
import { RotateTokenButton } from './RotateTokenButton';
import { useIsMobileDevice } from './useIsMobileView';
import { isEmbedded } from '../embed';
import { tauriInvoke } from '../tauri-browser';
import { Login } from './CloudLogin';
import { HeardAboutPrompt } from './HeardAboutPrompt';
import { SignupConsentPrompt } from './SignupConsentPrompt';
import { INSTALL_SCRIPT_DEFAULT_CLOUD, INSTALL_SCRIPT_URL } from '../lib/siteLinks';
import { WINDOWS_VERSIONS_NOTE } from '../lib/windowsOnlyCopy';

// CloudGate wraps the app. Mode is decided by probing /api/config:
// exists → cloud flow (Supabase login + machine picker); missing →
// local passthrough. This single check works for both `127.0.0.1:8787`
// (daemon, no /api/config) and `127.0.0.1:8788` or public domains
// (cloud, has /api/config).
export function CloudGate({ children }: { children: ReactNode }) {
  const mode = useCloud((s) => s.mode);
  const isLocal = useCloud((s) => s.isLocal);
  const bootstrap = useCloud((s) => s.bootstrap);
  const session = useCloud((s) => s.session);
  const recoveryPending = useCloud((s) => s.recoveryPending);
  const authorised = useCloud((s) => s.authorised);
  const signOut = useCloud((s) => s.signOut);
  const selectedMachineId = useCloud((s) => s.selectedMachineId);
  const error = useCloud((s) => s.error);
  const withoutAccount = useCloud((s) => s.withoutAccount);
  const appAdmittedRef = useRef(false);

  useEffect(() => { void bootstrap(); }, [bootstrap]);
  useEffect(() => {
    if (!session || authorised === false || (!isLocal && selectedMachineId === null)) {
      appAdmittedRef.current = false;
      return;
    }
    if (authorised === true && (isLocal || selectedMachineId)) {
      appAdmittedRef.current = true;
    }
  }, [authorised, isLocal, selectedMachineId, session]);
  useEffect(() => {
    const admitted = appAdmittedRef.current;
    let branch = 'app';
    if (mode === 'detecting') branch = 'detecting';
    else if (mode === 'local') branch = 'local';
    else if (error && !session) branch = 'error';
    else if (isLocal && withoutAccount && !session && !recoveryPending) branch = 'app-no-account';
    else if (!session || recoveryPending) branch = 'login';
    else if (authorised === null) branch = admitted ? 'app-authorising' : 'checking';
    else if (authorised === false) branch = 'access-denied';
    else if (isLocal) branch = 'app-local-auth';
    else if (!selectedMachineId) branch = 'machine-picker';
    lifecycleLog('cloudgate.branch', {
      branch,
      mode,
      hasSession: !!session,
      authorised,
      selectedMachineId,
      isLocal,
      admitted,
    });
  }, [authorised, error, isLocal, mode, recoveryPending, selectedMachineId, session, withoutAccount]);

  if (mode === 'detecting') return <CenteredMessage>Loading soluterminal…</CenteredMessage>;
  if (mode === 'local') return <>{children}</>;

  // cloud mode (covers both cloud-relay and local-auth flows)
  if (error && !session) return <CenteredMessage>Error: {error}</CenteredMessage>;
  // Local daemon used without an account: the daemon's loopback/token gate
  // already protects its API; signing in only adds cloud features.
  if (isLocal && withoutAccount && !session && !recoveryPending) return <>{children}</>;
  if (!session || recoveryPending) return <Login />;

  // Post-login gate: every account gets its public.users row from the
  // project's signup trigger, so a missing row means provisioning failed.
  // Team-only features are gated separately (entitlements). Uses a
  // tri-state so we don't flash the app for a frame while the check runs.
  if (authorised === null) {
    return appAdmittedRef.current ? <>{children}</> : <CenteredMessage>Checking access…</CenteredMessage>;
  }
  if (authorised === false) {
    return (
      <div className="cloud-gate-shell">
        <div className="cloud-gate-panel cloud-gate-panel--compact">
          <h1 className="cloud-gate-title">Account not ready</h1>
          <div className="cloud-gate-subtitle">
            This account has no SoluTerminal profile yet. Sign out and try again in a minute;
            if it keeps happening, send feedback from the app (Ctrl+K → Send feedback) or email
            hello@soluterminal.com.
          </div>
          <button
            type="button"
            onClick={() => void signOut()}
            className="primary cloud-gate-primary"
          >Sign out</button>
        </div>
      </div>
    );
  }

  // Local-auth: daemon is the only reachable host, nothing to pick.
  // Fall straight through to the app with prefix=''; applySession has
  // already wired the bearer token into configureApi.
  if (isLocal) return <>{children}</>;
  if (!selectedMachineId) {
    return <MachinePicker />;
  }
  return <>{children}</>;
}

function CenteredMessage({ children }: { children: ReactNode }) {
  return (
    <div className="cloud-gate-shell">
      <div className="cloud-gate-panel cloud-gate-message">{children}</div>
    </div>
  );
}

// MachineVersionLine renders the secondary line under a machine's name: which
// login owns it and the daemon + installer versions it last announced. Every
// part is optional — older daemons announce no versions, and the owner is
// skipped when there's no login to show. The daemon and shell (installer) update
// on separate channels, so both are shown when they differ.
function MachineVersionLine({ machine }: { machine: CloudMachine }) {
  const owner = machine.ownerName || machine.ownerEmail;
  const versions: string[] = [];
  if (machine.shellVersion) versions.push(`installer ${machine.shellVersion}`);
  if (machine.daemonVersion) versions.push(`daemon ${machine.daemonVersion}`);
  if (!owner && versions.length === 0) return null;
  return (
    <span className="cloud-gate-machine-meta">
      {owner && (
        <span className="cloud-gate-machine-owner" title={machine.ownerEmail || undefined}>
          {owner}
        </span>
      )}
      {versions.length > 0 && (
        <span className="cloud-gate-machine-versions">{versions.join(' · ')}</span>
      )}
    </span>
  );
}

function MachinePicker() {
  const machines = useCloud((s) => s.machines);
  const machinesLoaded = useCloud((s) => s.machinesLoaded);
  const loadError = useCloud((s) => s.error);
  const selectMachine = useCloud((s) => s.selectMachine);
  const refresh = useCloud((s) => s.refreshMachines);
  const signOut = useCloud((s) => s.signOut);
  const session = useCloud((s) => s.session);
  const isMobile = useIsMobileDevice();
  const desktop = isDesktopShell();
  const hosted = useHostedBrand();

  const [showClaim, setShowClaim] = useState(false);
  const [showHost, setShowHost] = useState(false);
  const bearer = session?.access_token ?? null;
  const team = useCloudTeamAccount(bearer);
  const email = session?.user?.email ?? null;
  // Until the first list arrives, an empty list says nothing: a returning
  // user must not see the new-account steps flash past.
  const loading = !machinesLoaded && machines.length === 0;
  // Only an account with no machines gets the get-started steps, whichever
  // way it came in from the website. A returning user goes straight to their
  // machines. The desktop shell already is the app, so it never gets them.
  const firstRun = !desktop && !loading && machines.length === 0;

  const otherWays = (
    <>
      {showClaim ? (
        <ClaimForm
          bearer={bearer}
          email={email}
          team={team}
          onDone={() => { setShowClaim(false); void refresh(); }}
          onCancel={() => setShowClaim(false)}
        />
      ) : (
        <button
          type="button"
          onClick={() => setShowClaim(true)}
          className="cloud-gate-link cloud-gate-link--spaced"
        >
          + Claim a new machine
        </button>
      )}

      {/* "Make THIS computer a host" — a phone can't be a host, so this only
          shows on a real PC. It's the discoverable counterpart to the
          (undiscoverable) desktop tray item: turn the machine you're sitting
          at into a host you can drive from your phone. */}
      {!isMobile && (
        showHost ? (
          <HostThisComputer team={team} onClose={() => setShowHost(false)} />
        ) : (
          <button
            type="button"
            onClick={() => setShowHost(true)}
            className="cloud-gate-link cloud-gate-link--spaced"
          >
            + Run soluterminal on this computer (host)
          </button>
        )
      )}

      <button type="button" onClick={() => void refresh()} className="cloud-gate-link">
        Refresh
      </button>
    </>
  );

  if (loading) {
    return (
      <div className={brandShellClass(hosted)}>
        <div className="cloud-gate-panel cloud-gate-panel--wide">
          {hosted && <BrandBar />}
          <div className="cloud-gate-message" role="status">
            {loadError ? "Couldn't load your machines." : 'Loading your machines…'}
          </div>
          {loadError && (
            <button type="button" onClick={() => void refresh()} className="cloud-gate-link">
              Try again
            </button>
          )}
        </div>
      </div>
    );
  }

  return (
    <div className={brandShellClass(hosted)}>
      <div className="cloud-gate-panel cloud-gate-panel--wide">
        {hosted && <BrandBar />}
        <div className="cloud-gate-header">
          <h1 className="cloud-gate-title">{firstRun ? GET_STARTED_TITLE : 'Your machines'}</h1>
          <button type="button" onClick={() => void signOut()} className="cloud-gate-link">Sign out</button>
        </div>

        {firstRun && <GetStartedCard email={email} />}
        {firstRun && <SignupConsentPrompt />}
        {firstRun && <HeardAboutPrompt />}

        {desktop && machines.length === 0 && !showClaim && (
          <div className="cloud-gate-empty">
            <div>No machines yet. The desktop app is already installed. Choose “Run soluterminal on this computer (host)” below to start a local terminal here.</div>
          </div>
        )}

        <div className="cloud-gate-machine-list">
          {machines.map((m) => {
            const selectable = m.online || m.reconnecting === true;
            const status = m.reconnecting ? 'reconnecting…' : m.online ? 'online' : 'offline';
            const statusClass = m.reconnecting ? 'is-reconnecting' : m.online ? 'is-online' : 'is-offline';
            return (
            <div key={m.id} className="cloud-gate-machine-row">
              <button
                type="button"
                onClick={() => selectMachine(m.id)}
                disabled={!selectable}
                className="cloud-gate-machine"
              >
                <span className="cloud-gate-machine-id">
                  <span className="cloud-gate-machine-name">{m.name}</span>
                  <MachineVersionLine machine={m} />
                </span>
                <span className={`cloud-gate-machine-status ${statusClass}`}>
                  {status}
                </span>
              </button>
              <RotateTokenButton machine={m} bearer={session?.access_token ?? null} />
              <button
                type="button"
                title={`Remove ${m.name} from your account. The daemon will be disconnected and you can re-claim it later.`}
                aria-label={`Remove ${m.name}`}
                onClick={async () => {
                  if (!(await confirmDialog({
                    message: `Remove "${m.name}"?\n\nIts daemon will be disconnected. You can re-pair it anytime by adding it here again.`,
                    confirmLabel: 'Remove',
                    danger: true,
                  }))) return;
                  try {
                    const resp = await fetch(`/api/machines/${encodeURIComponent(m.id)}`, {
                      method: 'DELETE',
                      headers: session?.access_token
                        ? { Authorization: `Bearer ${session.access_token}` }
                        : {},
                    });
                    if (!resp.ok) {
                      await alertDialog(`Remove failed: ${await resp.text() || resp.status}`);
                      return;
                    }
                    await refresh();
                  } catch (err: any) {
                    await alertDialog(`Remove failed: ${err?.message ?? err}`);
                  }
                }}
                className="cloud-gate-remove"
              >
                ×
              </button>
            </div>
            );
          })}
        </div>

        {firstRun ? (
          <details className="cloud-gate-other-ways" open={showClaim || showHost || undefined}>
            <summary>Other ways to connect</summary>
            {otherWays}
          </details>
        ) : (
          otherWays
        )}

        {!firstRun && usesWindowsDownload(isMobile) && (
          <div className="cloud-gate-footnote">
            Setting up another computer?{' '}
            <SiteInstallerLink className="cloud-gate-inline-link">Download for Windows</SiteInstallerLink>
          </div>
        )}
      </div>
      {/* The machine picker renders instead of <App> (which hosts the main
          DialogHost), so mount one here too — the remove-machine confirm/
          alert dialogs route through the same store and need a renderer. */}
      <DialogHost />
    </div>
  );
}

// Derive the install + claim one-liners from the cloud origin so
// staging/self-hosted deploys show the correct host instead of a
// hardcoded one. The on-screen commands MUST be runnable: `solud --claim`
// alone FATALS (daemon/main.go requires --relay). The fresh-machine path
// is the install one-liner (installs AND claims in one step); an
// already-installed machine uses the explicit --claim --relay form.
// The script comes from the GitHub release, not from this cloud, and is the
// same static, self-verifying file for every host, so --cloud points its
// pairing at this origin unless it is the default one.
export function hostCommands(origin = window.location.origin): { installCmd: string; fallbackCmd: string; claimCmd: string } {
  const cloudArg = origin === INSTALL_SCRIPT_DEFAULT_CLOUD ? '' : ` -s -- --cloud ${origin}`;
  const installCmd = `curl -fsSL ${INSTALL_SCRIPT_URL} | sh${cloudArg}`;
  // Same bytes from this cloud, for networks that block GitHub.
  const fallbackCmd = `curl -fsSL ${origin}/install.sh | sh${cloudArg}`;
  const relayWss = `${origin.replace(/^http/, 'ws')}/relay`;
  const claimCmd = `solud --claim --relay ${relayWss}`;
  return { installCmd, fallbackCmd, claimCmd };
}

// True only inside the Tauri desktop webview (window.__SOLU_TAURI__ set by
// lib.rs init_script). A browser tab / phone PWA returns false.
function isDesktopShell(): boolean {
  return !isEmbedded() && !!(window as unknown as { __SOLU_TAURI__?: boolean }).__SOLU_TAURI__;
}

// HostThisComputer: discoverable "make the machine I'm on a host" panel.
// Cloud-launch desktop windows have no local daemon, and App (which mounts
// the pairing panel) is not mounted at the machine picker. Switch to Local
// only after explicit restart confirmation; never dispatch an unhandled
// pairing event. In a Windows browser route to the desktop app (the POSIX
// one-liners can't run in PowerShell); everywhere else it shows the
// install/claim commands to run on this computer's terminal.
function HostThisComputer({ team, onClose }: { team: boolean; onClose: () => void }) {
  const { installCmd, fallbackCmd, claimCmd } = hostCommands();
  const desktop = isDesktopShell();
  const windowsVisitor = detectVisitorOS() === 'windows';
  const email = useCloud((s) => s.session?.user?.email);
  const invoke = tauriInvoke();
  const [switching, setSwitching] = useState(false);
  const [switchError, setSwitchError] = useState(false);

  const startLocal = async () => {
    if (!invoke || switching) return;
    // Set pending before awaiting the modal to prevent duplicate requests.
    setSwitching(true);
    const confirmed = await confirmDialog({
      title: 'Run projects on this computer',
      message: `Restart soluterminal in local hosting mode? The app will start the terminal backend on this computer. Sign in ${email ? `as ${email}` : 'with the same soluterminal account'} if prompted. Existing cloud machines and shared credentials will not be changed.`,
      confirmLabel: 'Restart in local mode',
    });
    if (!confirmed) { setSwitching(false); return; }
    setSwitchError(false);
    try {
      await invoke('desktop_set_launch_mode', { mode: 'local' });
    } catch {
      // Old shells or capability restrictions: do not suggest reinstallation
      // or swallow the error. The already-shipped tray item has the same path.
      setSwitchError(true);
      setSwitching(false);
    }
  };

  return (
    <div className="cloud-gate-claim">
      <div className="cloud-gate-claim-account">
        Turn <strong>this computer</strong> into a host — run soluterminal here and
        control it from your phone or another machine on your account.
      </div>
      {desktop ? (
        <>
          <div className="cloud-gate-claim-help">
            You're in the desktop app, currently connecting to cloud machines.
            Run projects on this computer to start its local terminal. Sign in
            {email ? <>as <strong>{email}</strong></> : <>with the same soluterminal account</>} if prompted.
            To receive team-shared accounts, also link this local host to that
            account using the tray menu’s “Pair with cloud…” option. Signing in
            alone does not link the machine; the pairing panel is labelled
            “Set up phone access”, but is also needed for shared credentials.
          </div>
          {(!invoke || switchError) && (
            <div className="cloud-gate-claim-help" role={switchError ? 'alert' : undefined}>
              {switchError && <>The in-app hosting switch was unavailable. </>}
              Right-click the soluterminal tray icon and choose “Switch to running projects on this computer (Local daemon)”. This restarts the app in local hosting mode.
            </div>
          )}
          <div className="cloud-gate-actions">
            {invoke && <button
              type="button"
              className="primary cloud-gate-primary"
              disabled={switching}
              onClick={() => void startLocal()}
            >
              {switching ? 'Restarting…' : 'Run projects on this computer'}
            </button>}
            <button type="button" onClick={onClose} disabled={switching} className="cloud-gate-link cloud-gate-link--action">
              Cancel
            </button>
          </div>
        </>
      ) : windowsVisitor ? (
        <>
          <div className="cloud-gate-claim-help">
            This computer runs Windows — the desktop app is the host here.{' '}
            <SiteInstallerLink className="cloud-gate-inline-link">Download the desktop app</SiteInstallerLink>{' '}
            and sign in with the same email. To pair it with a code instead, it shows
            a 6-character code to claim above with "+ Claim a new machine" (or later via
            its tray "Pair with cloud…").
            {team && (
              <>
                {' '}Hosting from a WSL terminal instead? Run:
                <code className="cloud-gate-code">{installCmd}</code>
                {' '}If that computer can't reach GitHub, use <code className="cloud-gate-code">{fallbackCmd}</code>.
              </>
            )}
          </div>
          <div className="cloud-gate-actions">
            <button type="button" onClick={onClose} className="cloud-gate-link cloud-gate-link--action">
              Done
            </button>
          </div>
        </>
      ) : team ? (
        <>
          <div className="cloud-gate-claim-help">
            In a terminal on the computer you want to host from, install soluterminal:
            <code className="cloud-gate-code">{installCmd}</code>
            {' '}If that computer can't reach GitHub, use <code className="cloud-gate-code">{fallbackCmd}</code>.
            It prints a 6-character code — claim it above with "+ Claim a new machine".
            Already running soluterminal there?
            Run <code className="cloud-gate-code">{claimCmd}</code> instead.
          </div>
          <div className="cloud-gate-actions">
            <button type="button" onClick={onClose} className="cloud-gate-link cloud-gate-link--action">
              Done
            </button>
          </div>
        </>
      ) : (
        <>
          <div className="cloud-gate-claim-help">
            {WINDOWS_VERSIONS_NOTE} Download it on your Windows PC from
            soluterminal.com/download and sign in with the same email.
          </div>
          <div className="cloud-gate-actions">
            <button type="button" onClick={onClose} className="cloud-gate-link cloud-gate-link--action">
              Done
            </button>
          </div>
        </>
      )}
    </div>
  );
}

// ClaimForm: user pastes the 6-char code printed by `./solud --claim`,
// picks a name, submits → cloud binds the pending claim to this user
// and the daemon's next poll gets its token.
function ClaimForm({ bearer, email, team, onDone, onCancel }: {
  bearer: string | null;
  email: string | null;
  team: boolean;
  onDone: () => void;
  onCancel: () => void;
}) {
  const [code, setCode] = useState('');
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!bearer) { setMessage('not signed in'); return; }
    setBusy(true);
    setMessage(null);
    try {
      const res = await fetch('/api/claim/accept', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${bearer}`,
        },
        body: JSON.stringify({ code: code.trim(), name: name.trim() }),
      });
      if (!res.ok) {
        const txt = await res.text();
        // The cloud now returns {error, message} JSON on claim failures; show the
        // human message. Fall back to the raw body for older/plain-text responses.
        let shown = txt;
        try {
          const body = JSON.parse(txt);
          if (body && typeof body.message === 'string') shown = body.message;
        } catch { /* not JSON — show as-is */ }
        setMessage(shown || `HTTP ${res.status}`);
        return;
      }
      onDone();
    } catch (err: any) {
      setMessage(err?.message ?? String(err));
    } finally {
      setBusy(false);
    }
  };

  // Origin-derived install + claim one-liners (see hostCommands). The
  // on-screen commands MUST be runnable and reflect the cloud host so
  // staging/self-hosted deploys show the correct origin.
  const { installCmd, fallbackCmd, claimCmd } = hostCommands();

  return (
    <form onSubmit={submit} className="cloud-gate-claim">
      <div className="cloud-gate-claim-account">
        {email
          ? <>This computer will be linked to your soluterminal account, <strong>{email}</strong>. On your phone, sign in with this same email to control it.</>
          : <>This computer will be linked to the soluterminal account you're signed in with. Use this same account on your phone to control it.</>}
      </div>
      <div className="cloud-gate-claim-help">
        {team && (
          <>
            On the computer you want to add — Linux, macOS, or WSL — install soluterminal:
            <code className="cloud-gate-code">{installCmd}</code>
            {' '}If that computer can't reach GitHub, use <code className="cloud-gate-code">{fallbackCmd}</code>.
            It prints a 6-character code — enter it below. Already running soluterminal there?
            Run <code className="cloud-gate-code">{claimCmd}</code> instead.{' '}
          </>
        )}
        On a Windows PC,{' '}
        <SiteInstallerLink className="cloud-gate-inline-link">install the desktop app</SiteInstallerLink>
        {' '}— it shows a 6-character code during setup (or via its tray "Pair with cloud…").
        Enter it below.
      </div>
      <label className="cloud-gate-label">
        Code
        <input
          value={code}
          onChange={(e) => setCode(e.target.value.toUpperCase())}
          placeholder="ABC234"
          autoFocus
          className="cloud-gate-input"
          maxLength={10}
          required
        />
      </label>
      <label className="cloud-gate-label">
        Name this machine
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="laptop"
          className="cloud-gate-input"
          required
        />
      </label>
      <div className="cloud-gate-actions">
        <button type="submit" disabled={busy} className="primary cloud-gate-primary">
          {busy ? 'Claiming…' : 'Claim'}
        </button>
        <button type="button" onClick={onCancel} className="cloud-gate-link cloud-gate-link--action">
          Cancel
        </button>
      </div>
      {message && <div className="cloud-gate-error cloud-gate-error--compact">{message}</div>}
    </form>
  );
}
