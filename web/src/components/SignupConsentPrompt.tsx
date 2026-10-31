import { useEffect, useState } from 'react';
import { api } from '../api';
import { useCloud } from '../cloud';
import { inDesktopApp } from '../lib/arrival';
import {
  MARKETING_CONSENT_LABEL,
  consentAskedAtSignup,
  recordSignupConsent,
} from '../marketingConsent';

// Google sign-ups skip the email-code step, where the unticked product-updates
// box lives. The new-account card asks once instead, and only an account with
// no recorded choice. Saving (ticked or not) and dismissing both record the
// choice, so the box never comes back on any device; a dismiss is the same
// "no" as leaving the box unticked at the code step. A local mark covers a
// save that failed.

const DONE_KEY_PREFIX = 'solu.signupConsentDone.';

function doneKey(userId: string): string {
  return `${DONE_KEY_PREFIX}${userId}`;
}

function markedDone(userId: string): boolean {
  try {
    return localStorage.getItem(doneKey(userId)) === '1';
  } catch {
    return false;
  }
}

function markDone(userId: string) {
  try {
    localStorage.setItem(doneKey(userId), '1');
  } catch {
    /* storage blocked: the recorded choice still hides it next time */
  }
}

export function SignupConsentPrompt() {
  const token = useCloud((s) => s.session?.access_token ?? null);
  const userId = useCloud((s) => s.session?.user?.id ?? null);
  const [state, setState] = useState<'checking' | 'ask' | 'saved' | 'hidden'>('checking');
  const [checked, setChecked] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!token || !userId || consentAskedAtSignup() || markedDone(userId)) {
      setState('hidden');
      return;
    }
    let cancelled = false;
    api
      .getMarketingConsent(token)
      .then((current) => {
        if (!cancelled) setState(current.consent === null ? 'ask' : 'hidden');
      })
      .catch(() => {
        // Unknown: don't ask. Settings › Account has the choice.
        if (!cancelled) setState('hidden');
      });
    return () => {
      cancelled = true;
    };
  }, [token, userId]);

  if (state === 'checking' || state === 'hidden' || !userId) return null;
  if (state === 'saved') {
    return (
      <div className="cloud-gate-footnote cloud-gate-consent-saved" role="status">
        Saved. You can change this in Settings.
      </div>
    );
  }

  const record = async (consent: boolean, after: 'saved' | 'hidden') => {
    setBusy(true);
    markDone(userId);
    await recordSignupConsent(consent, inDesktopApp() ? 'app-signup' : 'web-signup');
    setBusy(false);
    setState(after);
  };

  return (
    <div className="cloud-gate-consent-prompt">
      <label className="cloud-gate-consent">
        <input type="checkbox" checked={checked} onChange={(e) => setChecked(e.target.checked)} />
        <span>{MARKETING_CONSENT_LABEL}</span>
      </label>
      <div className="cloud-gate-consent-actions">
        <button type="button" className="cloud-gate-link" disabled={busy} onClick={() => void record(checked, 'saved')}>
          Save
        </button>
        <button type="button" className="cloud-gate-link" disabled={busy} onClick={() => void record(false, 'hidden')}>
          Dismiss
        </button>
      </div>
    </div>
  );
}
