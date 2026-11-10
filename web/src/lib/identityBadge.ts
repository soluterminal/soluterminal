import type { Health, UpdateStatus } from '../types';
import { channelReleaseWait } from './channelReleaseWait';
import { isPreviewVersion } from './previewChannel';

// Which plan, build and channel this install is on, for the status-bar badge
// and the first line of the version popover.
//
// Sources, most authoritative first:
// - Build: health.build.flavor, the flavour stamped into the daemon binary
//   (daemon/update_flavor.go). The update status's runningFlavor is the same
//   value; older daemons have neither, and then the build goes unnamed.
// - Preview: health.build.channel === 'preview', stamped only by
//   scripts/follow-main.sh, which every Preview-channel install goes through.
//   A hand-made build can have a 4-part version too, so the version is only
//   the fallback for daemons that predate build.channel.
// - Plan: the effective entitlements verdict (entitlements.ts), so the
//   personal view already reads as a free account.

export type BuildFlavor = 'public' | 'team';
export type PlanTier = 'none' | 'account' | 'team';
export type IdentityBadge = 'PREVIEW' | 'TEAM' | 'PRO';

export function buildFlavorOf(
  health: Pick<Health, 'build'> | null | undefined,
  status: Pick<UpdateStatus, 'runningFlavor'> | null | undefined,
): BuildFlavor | null {
  const flavor = health?.build?.flavor ?? status?.runningFlavor;
  return flavor === 'team' || flavor === 'public' ? flavor : null;
}

export function isPreviewChannelBuild(health: Pick<Health, 'build' | 'version'> | null | undefined): boolean {
  const channel = health?.build?.channel;
  if (channel) return channel === 'preview';
  return isPreviewVersion(health?.version);
}

// At most one word, PREVIEW before TEAM before PRO. Free and public installs
// get none. `pro` stays false until a Pro tier exists.
export function identityBadge(facts: { preview: boolean; team: boolean; pro?: boolean }): IdentityBadge | null {
  if (facts.preview) return 'PREVIEW';
  if (facts.team) return 'TEAM';
  if (facts.pro) return 'PRO';
  return null;
}

const BADGE_LABEL: Record<IdentityBadge, string> = {
  PREVIEW: 'Preview channel',
  TEAM: 'Team plan',
  PRO: 'Pro plan',
};

// The version button's accessible name: what the badge and the new-preview
// dot show, in words.
export function versionButtonLabel(version: string, badge: IdentityBadge | null, newerPreview: boolean): string {
  return [
    `v${version}`,
    badge ? BADGE_LABEL[badge] : '',
    newerPreview ? 'newer preview ready' : '',
  ].filter(Boolean).join(' · ');
}

const PLAN_WORDS: Record<PlanTier, string> = { none: '', account: 'Free account', team: 'Team plan' };

// The plan and the installed build disagree. The install moves only when its
// channel (daemon/update_channel.go) differs from the running build, and then
// only with that channel's next newer release, so the line promises a move
// only when the update status shows one coming.
function mismatchLine(plan: PlanTier, build: BuildFlavor, status: UpdateStatus | null | undefined): string | null {
  const wantTeam = plan === 'team' && build === 'public';
  const wantPublic = plan === 'account' && build === 'team' && status?.updateChannel === 'public';
  if (!wantTeam && !wantPublic) return null;
  const head = wantTeam ? 'Team plan, public build installed' : 'Free account, team build installed';
  const target = wantTeam ? 'team' : 'public';
  if (status?.updateChannel !== target) return head;
  if (status.available) return `${head} — moving to the ${target} build`;
  if (channelReleaseWait(status)) return `${head} — moves to the ${target} build with the next ${target} release`;
  return head;
}

export interface IdentityLineFacts {
  plan: PlanTier;
  build: BuildFlavor | null;
  preview: boolean;
  version: string;
  // Presentation mode or the personal view: the version alone.
  hidden: boolean;
}

// "Team plan · Team build · Preview channel · v0.0.1239.10". A mismatch
// replaces the plan and build words with one plain sentence.
export function identityLine(facts: IdentityLineFacts, status: UpdateStatus | null | undefined): string {
  const version = facts.version ? `v${facts.version}` : '';
  if (facts.hidden) return version;
  const mismatch = facts.build ? mismatchLine(facts.plan, facts.build, status) : null;
  const parts = mismatch
    ? [mismatch]
    : [PLAN_WORDS[facts.plan], facts.build === 'team' ? 'Team build' : facts.build === 'public' ? 'Public build' : ''];
  if (facts.preview) parts.push('Preview channel');
  parts.push(version);
  return parts.filter(Boolean).join(' · ');
}
