import { ApiError } from '../api';

// GitHub's own authorize screen is the only place a connect may send the
// browser; anything else in the answer is not followed.
export function isGithubAuthorizeUrl(url: string): boolean {
  return url.startsWith('https://github.com/login/oauth/authorize?');
}

// The connect step runs on SoluTerminal on the web, as the signed-in person.
// In the desktop app this window's own address is the local service, so the
// link goes to the fixed web address there.
const WEB_ORIGIN = 'https://soluterminal-cloud.onrender.com';

export function githubConfirmHref(installationId: number, here = typeof window === 'undefined' ? '' : window.location.origin): string {
  const onTheWeb = /^https:\/\//.test(here) && !/^https:\/\/(localhost|127\.0\.0\.1|tauri\.localhost)(:|$)/.test(here);
  return `${onTheWeb ? here : WEB_ORIGIN}/?github=claim&installation_id=${Math.trunc(installationId)}`;
}

export const GITHUB_CONNECT_SIGN_IN = 'Sign in to SoluTerminal on the web to finish connecting GitHub, then try Connect GitHub again.';

const FALLBACK = 'Could not finish the GitHub connection — try Connect GitHub again.';

const MESSAGES: Record<string, string> = {
  not_installation_admin:
    'GitHub says this GitHub account does not administer every repository the app is installed on. Sign in to GitHub as the account it is installed on, or as an owner of its organization, then try Connect GitHub again.',
  installation_too_large:
    'The app is installed on more repositories than can be checked (1,000). On GitHub, limit it to selected repositories, then try Connect GitHub again.',
  connect_started_by_another_account:
    'This connection was started from a different SoluTerminal account. Try Connect GitHub again from this one.',
  connect_expired: 'That connection attempt expired. Try Connect GitHub again.',
  already_connected_by_another_user:
    'That GitHub installation is already connected to another SoluTerminal account.',
  github_authorization_failed: 'GitHub did not confirm the connection. Try Connect GitHub again.',
  user_session_required: GITHUB_CONNECT_SIGN_IN,
  too_many_connect_attempts: 'Too many connection attempts. Wait a minute, then try Connect GitHub again.',
};

// What to tell the person when a connect is refused or fails.
export function githubConnectFailure(err: unknown): string {
  if (err instanceof ApiError && err.body && typeof err.body === 'object') {
    const code = (err.body as { error?: unknown }).error;
    if (typeof code === 'string' && Object.prototype.hasOwnProperty.call(MESSAGES, code)) return MESSAGES[code];
  }
  return FALLBACK;
}
