import { useState } from 'react';
import { api } from '../api';
import { useLocalProof } from '../lib/useLocalProof';

// Notices about approved devices at this computer (daemon human_proof.go)
// that stay on every screen until dealt with: a device approved without an
// approved device's proof (the first one, or one approved while the check
// was off), a removal made by such a device or one it approved, the check
// turned off in ~/.solu/env, or the setting lowered outside the app.
export function HumanProofBanner() {
  const { status, reload } = useLocalProof();
  const [error, setError] = useState<string | null>(null);
  if (!status) return null;
  const notices = status.notices ?? [];
  if (notices.length === 0 && !status.envOff && !status.lowered) return null;
  const approved = status.device === 'approved';
  const when = (at: string) => new Date(at).toLocaleString();

  return (
    <div className="phone-access-banner" role="status">
      <span className="phone-access-banner-text">
        {notices.map((n) => (
          <span key={n.id + n.at}>
            {n.via === 'removed'
              ? <>“{n.name}”, approved without an approved device’s proof, removed “{n.removed}”, {when(n.at)}. </>
              : n.via === 'env-recovery'
                ? <>Approved while the device check was off: “{n.name}”, {when(n.at)}. </>
                : <>First device approved on this computer: “{n.name}”, {when(n.at)}. </>}
            Not you? Remove it in Settings → Privacy &amp; security.{' '}
          </span>
        ))}
        {status.envOff && (
          <span>
            Approved devices aren’t checked at this computer (SOLU_HUMAN_PROOF=off in {'~/.solu/env (WSL, Linux or macOS) or %USERPROFILE%\\.solu\\env (Windows-native)'}), and any
            browser here can approve or remove devices. When you’re done, remove the line and restart the background service (see Settings → Privacy &amp; security).{' '}
          </span>
        )}
        {status.lowered && !status.envOff && (
          <span>The approved-device setting was changed outside the app ({status.lowered}). Set it again in Settings → Privacy &amp; security. </span>
        )}
        {error && <span className="phone-access-banner-sub" role="alert">{error}</span>}
      </span>
      {notices.length > 0 && approved && (
        <span className="phone-access-banner-actions">
          <button type="button" className="phone-access-banner-btn primary"
            onClick={() => void api.acknowledgeFirstDevice().then(() => reload(), (e: unknown) => setError(e instanceof Error ? e.message : String(e)))}>
            That was me
          </button>
        </span>
      )}
    </div>
  );
}
