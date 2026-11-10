import { useCallback, useSyncExternalStore } from 'react';

const STORAGE_KEY = 'solu.hideIdentities';
const EVENT_NAME = 'solu:hide-identities-changed';

export function readHideIdentities(): boolean {
	try { return localStorage.getItem(STORAGE_KEY) === '1'; } catch { return false; }
}

function subscribe(listener: () => void): () => void {
	const changed = () => listener();
	window.addEventListener(EVENT_NAME, changed);
	window.addEventListener('storage', changed);
	return () => {
		window.removeEventListener(EVENT_NAME, changed);
		window.removeEventListener('storage', changed);
	};
}

function setHideIdentities(value: boolean): void {
	try { localStorage.setItem(STORAGE_KEY, value ? '1' : '0'); } catch { /* storage may be disabled */ }
	window.dispatchEvent(new CustomEvent(EVENT_NAME, { detail: { hidden: value } }));
}

/** Shared screen-share identity preference for every routing/readout surface. */
export function useHideIdentities(): [boolean, (value: boolean) => void] {
	const hidden = useSyncExternalStore(subscribe, readHideIdentities, () => false);
	const update = useCallback((value: boolean) => setHideIdentities(value), []);
	return [hidden, update];
}

/**
 * Presentation mode (Settings › Account) is this preference: for screenshots
 * and videos it hides the account email in the sidebar, Settings › Account and
 * the command palette, hides the status-bar badge (and operator chips), and
 * masks pane account names. Other pages and terminal text are not covered.
 * It changes nothing else, unlike the personal view.
 */
export function usePresentationMode(): boolean {
	return useHideIdentities()[0];
}
