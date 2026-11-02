import { useSyncExternalStore } from 'react';
import { getEmbedTheme, isEmbedded } from '../embed';

function subscribeToHostTheme(onChange: () => void) {
  // The bridge is installed before React mounts and updates getEmbedTheme()
  // before this listener runs, including when a control is already open.
  window.addEventListener('message', onChange);
  return () => window.removeEventListener('message', onChange);
}

export function getHostTheme() {
  return isEmbedded() ? getEmbedTheme() : null;
}

export function useEmbedTheme() {
  return useSyncExternalStore(subscribeToHostTheme, getHostTheme);
}
