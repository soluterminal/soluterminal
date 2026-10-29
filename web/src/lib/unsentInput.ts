import { useEffect, useId } from 'react';

// Text the user typed and has not sent yet, in any compose box on this page.
//
// The unattended updates must not run over it: the daemon swap reloads the
// page and the installer update restarts the app, and either one throws away
// a box's text. Native pane drafts have their own per-pane store
// (useNativePane's hasAnyUnsentDraft, which also covers panes that are not
// mounted); every other compose box reports here while it is mounted.
const holders = new Set<string>();

export function reportUnsentInput(key: string, text: string): void {
  if (text.trim().length > 0) holders.add(key);
  else holders.delete(key);
}

export function hasRegisteredUnsentInput(): boolean {
  return holders.size > 0;
}

// Report a compose box's text while it is mounted. Closing the box (sending,
// cancelling or unmounting) clears it.
export function useUnsentInput(text: string): void {
  const key = useId();
  useEffect(() => {
    reportUnsentInput(key, text);
  }, [key, text]);
  useEffect(() => () => reportUnsentInput(key, ''), [key]);
}

export function resetUnsentInputForTests(): void {
  holders.clear();
}
