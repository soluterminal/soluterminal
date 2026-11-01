import type { PaneInfo } from '../types';

export interface PaneRowLabel {
  /** "@cmo", or '' without a handle. */
  handle: string;
  /** The title, minus a leading "@handle" it already carries; '' when nothing is left. */
  detail: string;
  /** One line for screen readers and lists: "@cmo · launch", "@cmo" or the title. */
  text: string;
  /** The full title, then the working-on line when there is one. */
  tooltip: string;
}

const SEPARATORS = /^[\s·•:|,\-–—]+/;
// What may follow a whole handle: a hyphen continues it ("@cmo-2").
const AFTER_HANDLE = /^[\s·•:|,–—]/;

/** How a pane names itself in lists: its @handle first (the stable name),
 *  then its title in dimmer text without repeating the handle. */
export function paneRowLabel(pane: PaneInfo, title = pane.title || pane.cmd.join(' ')): PaneRowLabel {
  const raw = pane.identity?.handle?.trim().replace(/^@/, '') ?? '';
  const workingOn = pane.identity?.workingOn?.trim();
  const tooltip = workingOn ? `${title}\n${workingOn}` : title;
  if (!raw) return { handle: '', detail: title, text: title, tooltip };
  const handle = `@${raw}`;
  let detail = title.trim();
  if (detail.toLowerCase().startsWith(handle.toLowerCase())) {
    const after = detail.slice(handle.length);
    // Only a whole handle: "@cmo-2" is not "@cmo".
    if (!after || AFTER_HANDLE.test(after)) detail = after.replace(SEPARATORS, '').trim();
  }
  return { handle, detail, text: detail ? `${handle} · ${detail}` : handle, tooltip };
}
