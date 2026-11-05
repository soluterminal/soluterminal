import type { UpdateStatus } from '../types';
import { isVersionNewer } from './useCloudShellUpdate';

// The line for an install that follows the other build (daemon/update_channel.go)
// and has nothing of that build to install yet, or null when that is not the
// case. It needs a successful answer from the channel: before one, or with a
// failing check, nobody knows whether a newer release exists. A successful answer
// can have no latest (the channel's repo serves another flavour's release that is
// not newer, Updater.Check). A newer release that is held or halted is not a wait
// for the next one, so it says nothing.
export function channelReleaseWait(status: UpdateStatus | null | undefined): string | null {
  const channel = status?.updateChannel;
  const running = status?.runningFlavor;
  if (!channel || !running || channel === running) return null;
  if (status?.available || status?.lastError || !status?.lastSuccessMs) return null;
  if (status.latest && isVersionNewer(status.latest, status.current)) return null;
  return `Waiting for the next ${channel} release`;
}
