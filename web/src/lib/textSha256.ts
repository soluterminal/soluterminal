import { hex, sha256, utf8 } from './privateRelay/bytes';

/** Web Crypto exists only in secure contexts (https or this computer). */
export function canHashText(): boolean {
  return typeof crypto !== 'undefined' && !!crypto.subtle;
}

/** Lowercase hex SHA-256 of the text's UTF-8 bytes: what the daemon checks
 *  before Send types a message (pane_unsent.go unsentTextSha256). */
export async function textSha256(text: string): Promise<string> {
  return hex(await sha256(utf8.encode(text)));
}
