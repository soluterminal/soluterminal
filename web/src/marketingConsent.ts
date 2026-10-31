import { api } from './api';
import { useCloud } from './cloud';

// Product-update email consent (PDPA / GDPR). The box at sign-up starts
// unticked; the cloud records the choice with this wording version so we can
// show what the user agreed to. Account and security mail never depends on it.

export const MARKETING_CONSENT_LABEL = 'Email me product updates (you can unsubscribe anytime)';
export const MARKETING_CONSENT_WORDING = '2026-10-23-v1';

// web-signup: an account created in the hosted app (the website's download
// step leads there); app-signup: the desktop app's first screen.
export type MarketingConsentSource = 'app-signup' | 'app-signin' | 'app-settings' | 'web-signup';

export interface MarketingConsentState {
  consent: boolean | null;
  wording_version?: string;
  source?: string;
  updated_at: string | null;
}

// Set when this page showed the box at the email-code step, so the
// new-account card never asks a second time while that choice is saving.
let askedAtSignup = false;

export function noteConsentAskedAtSignup(): void {
  askedAtSignup = true;
}

export function consentAskedAtSignup(): boolean {
  return askedAtSignup;
}

// recordSignupConsent stores the sign-up choice once the session exists. The
// cloud keeps the first choice and only lets a later sign-in opt in, so an
// unticked box at a routine sign-in never withdraws an earlier yes. Failure
// is silent: the user can still set it in Settings.
export async function recordSignupConsent(consent: boolean, source: MarketingConsentSource): Promise<void> {
  const token = useCloud.getState().session?.access_token ?? null;
  if (!token) return;
  try {
    await api.postMarketingConsent({ consent, source, wording_version: MARKETING_CONSENT_WORDING }, token);
  } catch {
    // Settings › Account shows and changes it later.
  }
}
