import { useEntitlements, usePersonalView } from '../entitlements';
import type { Health, UpdateStatus } from '../types';
import { usePresentationMode } from './hideIdentities';
import {
  buildFlavorOf,
  identityBadge,
  identityLine,
  isPreviewChannelBuild,
  versionButtonLabel,
  type IdentityBadge,
} from './identityBadge';
import { usePreviewChannel } from './previewChannel';

export interface VersionIdentity {
  // The one-word status-bar badge, or null.
  badge: IdentityBadge | null;
  // A newer Preview build is ready to install (the dot beside the version).
  newerPreview: boolean;
  // The version button's accessible name.
  label: string;
  // The version popover's first line.
  line: string;
}

// Presentation mode and the personal view hide the badge, the new-preview dot
// and the plan words; the version stays.
export function useVersionIdentity(health: Health | null, status: UpdateStatus | null | undefined): VersionIdentity {
  const preview = usePreviewChannel();
  const presentation = usePresentationMode();
  const personal = usePersonalView();
  const plan = useEntitlements((s) => (s.loaded ? s.tier : 'none'));
  const team = useEntitlements((s) => s.loaded && s.team);
  const hidden = presentation || personal;
  const version = health?.version || status?.current || '';
  const onPreview = isPreviewChannelBuild(health);
  const badge = hidden ? null : identityBadge({ preview: onPreview, team });
  const newerPreview = !hidden && (preview?.newChanges ?? 0) > 0 && !preview?.job;
  return {
    badge,
    newerPreview,
    label: versionButtonLabel(version, badge, newerPreview),
    line: identityLine({ plan, build: buildFlavorOf(health, status), preview: onPreview, version, hidden }, status),
  };
}
