import { describe, expect, it } from 'vitest';
import { standalonePageFor } from '../../standalonePages';
import { isDeletionPlan, joinProducts, lastSignInFromToken } from './AccountDeletePage';

function token(payload: object): string {
  const b64 = (o: object) => btoa(JSON.stringify(o)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  return `${b64({ alg: 'HS256' })}.${b64(payload)}.sig`;
}

describe('account deletion page', () => {
  it('is served at /account/delete', () => {
    expect(standalonePageFor('/account/delete')?.path).toBe('/account/delete');
    expect(standalonePageFor('/account/delete/')?.path).toBe('/account/delete');
    expect(standalonePageFor('/account')).toBeNull();
  });

  it('reads the newest sign-in time from the token, not iat', () => {
    expect(lastSignInFromToken(token({ iat: 2000, amr: [{ method: 'password', timestamp: 100 }, { method: 'otp', timestamp: 900 }] }))).toBe(900_000);
    expect(lastSignInFromToken(token({ iat: 2000 }))).toBeNull();
    expect(lastSignInFromToken('not-a-jwt')).toBeNull();
  });

  it('names the other products', () => {
    expect(joinProducts([])).toBe('another of our apps');
    expect(joinProducts(['Alpha'])).toBe('Alpha');
    expect(joinProducts(['Alpha', 'Beta', 'Gamma'])).toBe('Alpha, Beta and Gamma');
  });

  it('accepts only a well-formed plan', () => {
    expect(isDeletionPlan({ available: true, keepSignIn: false, computers: 1 })).toBe(true);
    expect(isDeletionPlan({ available: true })).toBe(false);
    expect(isDeletionPlan(null)).toBe(false);
  });
});
