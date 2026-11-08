// Path helpers for the Files sidebar box. POSIX behaviour is unchanged; Windows
// drive (C:/ or C:\) and UNC (\\host\share) paths are absolute and may use \.

const DRIVE_RE = /^[A-Za-z]:([\\/]|$)/;

export function isWindowsPath(p: string): boolean {
  return DRIVE_RE.test(p) || p.startsWith('\\\\');
}

export function isAbsolutePath(p: string): boolean {
  return p.startsWith('/') || p.startsWith('~') || isWindowsPath(p);
}

function lastSep(s: string, win: boolean): number {
  return win ? Math.max(s.lastIndexOf('/'), s.lastIndexOf('\\')) : s.lastIndexOf('/');
}

function joinPath(base: string, rel: string): string {
  const win = isWindowsPath(base);
  if (base.endsWith('/') || (win && base.endsWith('\\'))) return base + rel;
  const sep = win && base.includes('\\') && !base.includes('/') ? '\\' : '/';
  return base + sep + rel;
}

export interface ResolvedPath { dirInput: string; dirAbs: string; needle: string }

export function resolveDraft(draft: string, focusedCwd: string | undefined): ResolvedPath {
  const win = isWindowsPath(draft) || (!isAbsolutePath(draft) && !!focusedCwd && isWindowsPath(focusedCwd));
  const slash = lastSep(draft, win);
  const dirInput = slash >= 0 ? draft.slice(0, slash + 1) : '';
  const needle = slash >= 0 ? draft.slice(slash + 1) : draft;

  let dirAbs: string;
  if (dirInput === '' || !isAbsolutePath(dirInput)) {
    const base = focusedCwd || '~';
    dirAbs = dirInput ? joinPath(base, dirInput) : base;
  } else {
    dirAbs = dirInput;
  }
  return { dirInput, dirAbs, needle };
}

export function resolveForOpen(path: string, base: string | undefined): string {
  const trimmed = path.trim();
  if (!trimmed) return trimmed;
  if (isAbsolutePath(trimmed)) return trimmed;
  if (!base) return trimmed;
  return joinPath(base, trimmed);
}

export function stripTrailingSep(p: string): string {
  return p.replace(/[\\/]$/, '');
}
