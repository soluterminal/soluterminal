// Words for what Disconnect did about removing this computer from the account
// (ST-257). Kept apart from cloudPair.ts so tests that mock the pairing API
// keep the real wording.
import type { DisconnectResult } from '../cloudPair';

// disconnectMessage is the toast after Disconnect. Only a confirmed removal is
// called one; a 404 (no such computer, or the key wasn't accepted: the cloud
// doesn't say which) and a refusal point at Your machines, URL shown as text.
export function disconnectMessage(r: DisconnectResult | undefined): { text: string; warn: boolean } {
  const url = r?.machinesUrl ?? '';
  switch (r?.cloudRemoval) {
    case 'removed':
      return { text: 'Cloud disconnected, and this computer was removed from your account.', warn: false };
    case 'unlisted':
      return {
        text: `Cloud disconnected. Your account no longer lists this computer, or didn't accept the removal. Check Your machines${url ? `: ${url}` : '.'}`,
        warn: true,
      };
    case 'refused':
      return {
        text: `Cloud disconnected, but your account didn't remove this computer. Remove it in Your machines${url ? `: ${url}` : '.'}`,
        warn: true,
      };
    case 'pending':
      return {
        text: 'Cloud disconnected. Removing this computer from your account will finish when the cloud answers.',
        warn: false,
      };
    default:
      return { text: 'Cloud disconnected — this computer no longer connects to the cloud.', warn: false };
  }
}

export const REVOKE_REFUSED_TEXT = "Your account didn't remove this computer when you disconnected it. Remove it in Your machines:";
