import { useEffect } from 'react';
import { create } from 'zustand';
import { api } from './api';
import { useCloud } from './cloud';
import type { EntitlementsTier } from './types';
import { useRelayConfigured } from './lib/relayConfigured';

// Which non-public surfaces this install may show. The daemon enforces the
// same verdict; hiding the UI just keeps strangers from meeting a wall of
// 403s. Until the first answer arrives, if it fails, and on daemons without
// the endpoint, those surfaces stay hidden: a stranger must never see them
// flash in.
//
// Tiers: "none" is the full local app; "account" (a signed-in free account)
// adds phone access, push, cloud voice and smart approvals; "team" adds the
// team-plan features. Separately, "operator" covers the operator's own
// engineering tooling, which customers never get (featureGates.ts lists every
// gated surface and its flag).
//
// The personal view (daemon/personal_view.go) shows a team or operator
// install exactly as a free account sees it: `real` keeps the daemon's
// verdict and the top-level fields carry the effective one.
export interface EntitlementsVerdict {
  tier: EntitlementsTier;
  team: boolean;
  operator: boolean;
  account: boolean;
  accountSwitching: boolean;
  teamCredentials: boolean;
  // Cloud smart approvals for this install; null when the daemon predates
  // the field.
  smartApprovals: boolean | null;
}

interface EntitlementsState extends EntitlementsVerdict {
  loaded: boolean;
  real: EntitlementsVerdict;
  viewPersonal: boolean;
  load: () => Promise<void>;
  setPersonalView: (personal: boolean) => Promise<void>;
}

const NO_VERDICT: EntitlementsVerdict = {
  tier: 'none',
  team: false,
  operator: false,
  account: false,
  accountSwitching: false,
  teamCredentials: false,
  smartApprovals: null,
};

// applyPersonalView turns a team or operator verdict into what a free
// account sees. Installs with nothing to hide are returned unchanged.
export function applyPersonalView(real: EntitlementsVerdict, personal: boolean): EntitlementsVerdict {
  if (!personal || (!real.team && !real.operator)) return real;
  return {
    ...real,
    tier: real.tier === 'none' ? 'none' : 'account',
    team: false,
    operator: false,
    account: real.account || real.tier !== 'none',
    accountSwitching: false,
    teamCredentials: false,
  };
}

export const useEntitlements = create<EntitlementsState>((set, get) => ({
  loaded: false,
  ...NO_VERDICT,
  real: NO_VERDICT,
  viewPersonal: false,
  load: async () => {
    const cloud = useCloud.getState();
    // Only the local daemon needs the account token: a relayed daemon is
    // paired and asks the cloud with its own machine identity.
    const token = cloud.isLocal ? cloud.session?.access_token ?? null : null;
    try {
      const e = await api.getEntitlements(token);
      if (!e) {
        // No endpoint: an unknown verdict, so only the free local app shows.
        set({ loaded: true, ...NO_VERDICT, real: NO_VERDICT, viewPersonal: false });
        return;
      }
      // Daemons before the tier fields only knew team or not.
      const tier: EntitlementsTier = e.tier ?? (e.team ? 'team' : e.account ? 'account' : 'none');
      const real: EntitlementsVerdict = {
        tier,
        team: e.team,
        // Daemons before the operator/team split only knew team.
        operator: typeof e.operator === 'boolean' ? e.operator : e.team,
        account: e.account ?? (tier === 'account' || tier === 'team'),
        accountSwitching: e.accountSwitching,
        teamCredentials: e.teamCredentials,
        smartApprovals: typeof e.smartApprovals === 'boolean' ? e.smartApprovals : null,
      };
      const viewPersonal = e.viewPersonal === true && (real.team || real.operator);
      set({ loaded: true, ...applyPersonalView(real, viewPersonal), real, viewPersonal });
    } catch {
      // Keep the previous verdict on a transient failure.
    }
  },
  setPersonalView: async (personal: boolean) => {
    await api.setPersonalView(personal);
    await get().load();
  },
}));

// A different signed-in account can carry a different verdict.
useCloud.subscribe((state, prev) => {
  if (state.session?.user?.id === prev.session?.user?.id) return;
  if (useEntitlements.getState().loaded) void useEntitlements.getState().load();
});

type CloudSnapshot = ReturnType<typeof useCloud.getState>;

// A real (non-anonymous) Supabase session in this browser. The cloud refuses
// anonymous sessions for account features, so they never count.
function hasAccountSession(cloud: CloudSnapshot): boolean {
  const session = cloud.session;
  if (!session) return false;
  return (session.user as { is_anonymous?: boolean } | undefined)?.is_anonymous !== true;
}

function accountAllowed(cloud: CloudSnapshot, ent: EntitlementsState): boolean {
  if (hasAccountSession(cloud)) return true;
  if (!ent.loaded) return false;
  if (ent.team) return true;
  // Chose "Skip for now" here: the account features stay hidden even if the
  // daemon is paired under someone's free account.
  if (cloud.withoutAccount) return false;
  return ent.account;
}

// Free-account features: phone access (relay pairing, the phone web app),
// push notifications, cloud voice and smart approvals. Shown when the daemon
// reports an account or team verdict, or when this browser holds a signed-in
// session (the daemon's verdict can lag a fresh sign-in).
export function useAccountFeatures(): boolean {
  const signedIn = useCloud(hasAccountSession);
  const withoutAccount = useCloud((s) => s.withoutAccount);
  const daemon = useEntitlements((s) => {
    if (!s.loaded) return false;
    if (s.team) return true;
    return !withoutAccount && s.account;
  });
  return signedIn || daemon;
}

// Non-hook read of the same verdict for event handlers.
export function accountFeaturesAllowed(): boolean {
  return accountAllowed(useCloud.getState(), useEntitlements.getState());
}

// Phone access (relay pairing, the phone web app) needs a free account, and
// a relay: a desktop whose daemon has none (a build with no hosted cloud
// configured) offers no pairing at all.
export function usePhoneAccessAllowed(): boolean {
  const account = useAccountFeatures();
  const relay = useRelayConfigured();
  return account && relay !== false;
}

// Team-plan surfaces: what a customer team plan could carry (today the
// account label and saved provider accounts). Hidden until the daemon
// confirms a team install, so a stranger never sees them flash in.
export function useTeamFeatures(): boolean {
  return useEntitlements((s) => s.loaded && s.team);
}

// Non-hook read of the same verdict for event handlers and palette builders.
export function teamFeaturesAllowed(): boolean {
  const s = useEntitlements.getState();
  return s.loaded && s.team;
}

// The operator's internal tooling: the Tasks console and its chips, task
// owners, the Orchestrator, Agents, delegation, the release train, Aria,
// Muse, System ops cards and internal credential rows. Never
// shown to customers, even on a team plan.
export function useOperatorFeatures(): boolean {
  return useEntitlements((s) => s.loaded && s.operator);
}

// Non-hook read of the operator verdict.
export function operatorFeaturesAllowed(): boolean {
  const s = useEntitlements.getState();
  return s.loaded && s.operator;
}

// Whether the personal view is on (team and operator surfaces hidden).
export function usePersonalView(): boolean {
  return useEntitlements((s) => s.loaded && s.viewPersonal);
}

// Non-hook read of the personal view for event handlers and palette builders.
export function personalViewOn(): boolean {
  const s = useEntitlements.getState();
  return s.loaded && s.viewPersonal;
}

// Whether this install has anything for the personal view to hide.
export function usePersonalViewAvailable(): boolean {
  return useEntitlements((s) => s.loaded && (s.real.team || s.real.operator));
}

// Ask for the verdict when a top-level view mounts (the main app and the
// standalone Tasks window), not only when the event stream opens: team-only
// surfaces stay hidden until it arrives, so a slow or blocked stream must not
// hold them back.
export function useLoadEntitlementsOnMount(): void {
  useEffect(() => {
    if (!useEntitlements.getState().loaded) void useEntitlements.getState().load();
  }, []);
}
