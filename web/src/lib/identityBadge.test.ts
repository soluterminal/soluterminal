import { describe, expect, it } from 'vitest';
import type { UpdateStatus } from '../types';
import {
  buildFlavorOf,
  identityBadge,
  identityLine,
  isPreviewChannelBuild,
  versionButtonLabel,
  type BuildFlavor,
  type PlanTier,
} from './identityBadge';

const builds: BuildFlavor[] = ['public', 'team'];
const plans: PlanTier[] = ['none', 'account', 'team'];
const channels = ['preview', 'release'] as const;

describe('identityBadge', () => {
  // Every build × plan × channel: the badge follows the channel, then the
  // plan, and never the build (a mismatch is the popover's job).
  const cases = builds.flatMap((build) => plans.flatMap((plan) => channels.map((channel) => ({
    build, plan, channel,
    want: channel === 'preview' ? 'PREVIEW' : plan === 'team' ? 'TEAM' : null,
  }))));
  it.each(cases)('$build build, $plan plan, $channel → $want', ({ plan, channel, want }) => {
    expect(identityBadge({ preview: channel === 'preview', team: plan === 'team' })).toBe(want);
  });

  it('puts PRO last and shows it only when a Pro tier says so', () => {
    expect(identityBadge({ preview: false, team: false, pro: true })).toBe('PRO');
    expect(identityBadge({ preview: false, team: true, pro: true })).toBe('TEAM');
    expect(identityBadge({ preview: true, team: true, pro: true })).toBe('PREVIEW');
    expect(identityBadge({ preview: false, team: false })).toBeNull();
  });
});

describe('build sources', () => {
  it('reads the flavour from health, then the update status, else unknown', () => {
    expect(buildFlavorOf({ build: { flavor: 'team' } }, { runningFlavor: 'public' })).toBe('team');
    expect(buildFlavorOf({ build: {} }, { runningFlavor: 'public' })).toBe('public');
    expect(buildFlavorOf(null, null)).toBeNull();
  });

  it('trusts build.channel over the version shape', () => {
    expect(isPreviewChannelBuild({ version: '0.0.1239.10', build: { channel: 'preview' } })).toBe(true);
    // A hand-made build with a 4-part version is not Preview.
    expect(isPreviewChannelBuild({ version: '0.0.1239.10', build: { channel: 'local' } })).toBe(false);
    expect(isPreviewChannelBuild({ version: '0.0.1240', build: { channel: 'release' } })).toBe(false);
    // Daemons before build.channel: the 4-part version is the only signal.
    expect(isPreviewChannelBuild({ version: '0.0.1239.10' })).toBe(true);
    expect(isPreviewChannelBuild({ version: '0.0.1240' })).toBe(false);
  });
});

describe('identityLine', () => {
  const line = (
    over: Partial<Parameters<typeof identityLine>[0]>,
    status: Partial<UpdateStatus> | null = null,
  ) => identityLine({ plan: 'none', build: 'public', preview: false, version: '0.0.1240', hidden: false, ...over }, status as UpdateStatus | null);

  it('names plan, build, channel and version', () => {
    expect(line({ plan: 'team', build: 'team', preview: true, version: '0.0.1239.10' }))
      .toBe('Team plan · Team build · Preview channel · v0.0.1239.10');
    expect(line({ plan: 'account' })).toBe('Free account · Public build · v0.0.1240');
    expect(line({ plan: 'none' })).toBe('Public build · v0.0.1240');
    expect(line({ build: null })).toBe('v0.0.1240');
  });

  it('never nudges a free public install toward a team', () => {
    for (const plan of ['none', 'account'] as const) {
      expect(line({ plan })).not.toMatch(/sign in|team/i);
    }
  });

  it('says a team plan on the public build is moving when the team update is there', () => {
    expect(line({ plan: 'team' }, { updateChannel: 'team', runningFlavor: 'public', available: true, current: '0.0.1240', latest: '0.0.1241' }))
      .toBe('Team plan, public build installed — moving to the team build · v0.0.1240');
  });

  it('says it moves with the next team release when there is none yet', () => {
    expect(line({ plan: 'team' }, { updateChannel: 'team', runningFlavor: 'public', available: false, current: '0.0.1240', lastSuccessMs: 1 }))
      .toBe('Team plan, public build installed — moves to the team build with the next team release · v0.0.1240');
  });

  it('promises nothing while the install still follows the public channel', () => {
    expect(line({ plan: 'team' }, { updateChannel: 'public', runningFlavor: 'public' }))
      .toBe('Team plan, public build installed · v0.0.1240');
    expect(line({ plan: 'team' }, null)).toBe('Team plan, public build installed · v0.0.1240');
    // A failing check: nobody knows whether a team release exists.
    expect(line({ plan: 'team' }, { updateChannel: 'team', runningFlavor: 'public', lastError: 'offline', lastSuccessMs: 1 }))
      .toBe('Team plan, public build installed · v0.0.1240');
  });

  it('states the reverse move only once the daemon has chosen the public channel', () => {
    expect(line({ plan: 'account', build: 'team' }, { updateChannel: 'public', runningFlavor: 'team', available: true, current: '0.0.1240', latest: '0.0.1241' }))
      .toBe('Free account, team build installed — moving to the public build · v0.0.1240');
    expect(line({ plan: 'account', build: 'team' }, { updateChannel: 'team', runningFlavor: 'team' }))
      .toBe('Free account · Team build · v0.0.1240');
    // Signed out is not a verdict: nothing moves.
    expect(line({ plan: 'none', build: 'team' }, { updateChannel: 'public', runningFlavor: 'team' }))
      .toBe('Team build · v0.0.1240');
  });

  it('shows the version alone in Presentation mode or the personal view', () => {
    expect(line({ plan: 'team', build: 'team', preview: true, hidden: true }, { updateChannel: 'public', runningFlavor: 'team' }))
      .toBe('v0.0.1240');
  });
});

describe('versionButtonLabel', () => {
  it('says the badge and the new-preview dot in words', () => {
    expect(versionButtonLabel('0.0.1239.10', 'PREVIEW', true)).toBe('v0.0.1239.10 · Preview channel · newer preview ready');
    expect(versionButtonLabel('0.0.1240', 'TEAM', false)).toBe('v0.0.1240 · Team plan');
    expect(versionButtonLabel('0.0.1240', null, false)).toBe('v0.0.1240');
  });
});
