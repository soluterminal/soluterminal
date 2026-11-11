import { describe, expect, it } from 'vitest';
import { ApiError } from '../api';
import { githubConfirmHref, githubConnectFailure, isGithubAuthorizeUrl } from './githubConnect';

describe('isGithubAuthorizeUrl', () => {
  it('accepts only GitHub\'s authorize screen', () => {
    expect(isGithubAuthorizeUrl('https://github.com/login/oauth/authorize?client_id=x&state=y')).toBe(true);
    for (const url of [
      'https://github.com.example.net/login/oauth/authorize?client_id=x',
      'https://example.net/login/oauth/authorize?client_id=x',
      'http://github.com/login/oauth/authorize?client_id=x',
      'https://github.com/login/oauth/authorize',
      'https://github.com/apps/x/installations/new',
      'javascript:alert(1)',
      '/?github=connected',
      '',
    ]) {
      expect(isGithubAuthorizeUrl(url)).toBe(false);
    }
  });
});

describe('githubConfirmHref', () => {
  it('points at the connect step on the web', () => {
    expect(githubConfirmHref(42, 'https://soluterminal-cloud.onrender.com')).toBe(
      'https://soluterminal-cloud.onrender.com/?github=claim&installation_id=42',
    );
    expect(githubConfirmHref(42, 'https://staging.example.net')).toBe('https://staging.example.net/?github=claim&installation_id=42');
  });

  it('uses the fixed web address from the desktop app or a local service', () => {
    for (const here of ['http://127.0.0.1:8787', 'http://localhost:9791', 'https://tauri.localhost', 'tauri://localhost', '']) {
      expect(githubConfirmHref(42, here)).toBe('https://soluterminal-cloud.onrender.com/?github=claim&installation_id=42');
    }
  });
});

describe('githubConnectFailure', () => {
  it('explains a refusal the cloud names', () => {
    const refused = new ApiError(403, 'refused', { error: 'not_installation_admin' });
    expect(githubConnectFailure(refused)).toContain('does not administer every repository');
    const large = new ApiError(403, 'refused', { error: 'installation_too_large' });
    expect(githubConnectFailure(large)).toContain('selected repositories');
    const other = new ApiError(403, 'refused', { error: 'connect_started_by_another_account' });
    expect(githubConnectFailure(other)).toContain('different SoluTerminal account');
  });

  it('falls back for anything else', () => {
    const fallback = 'Could not finish the GitHub connection — try Connect GitHub again.';
    expect(githubConnectFailure(new ApiError(502, 'bad gateway', { error: 'installation_unverifiable' }))).toBe(fallback);
    expect(githubConnectFailure(new ApiError(403, 'refused', { error: 'toString' }))).toBe(fallback);
    expect(githubConnectFailure(new ApiError(500, 'plain text'))).toBe(fallback);
    expect(githubConnectFailure(new Error('network'))).toBe(fallback);
  });
});
