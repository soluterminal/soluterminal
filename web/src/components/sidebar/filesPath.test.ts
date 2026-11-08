import { describe, expect, it } from 'vitest';
import { isAbsolutePath, resolveDraft, resolveForOpen, stripTrailingSep } from './filesPath';

describe('Files box paths', () => {
  it('keeps a Windows drive path as typed instead of joining it to the cwd', () => {
    const p = 'C:/Users/runneradmin/.solu/inapp-editor.txt';
    expect(resolveForOpen(p, 'C:\\Users\\runneradmin')).toBe(p);
    const r = resolveDraft(p, 'C:\\Users\\runneradmin');
    expect(r.dirAbs).toBe('C:/Users/runneradmin/.solu/');
    expect(r.needle).toBe('inapp-editor.txt');
  });

  it('accepts backslashes and UNC paths', () => {
    expect(resolveForOpen('D:\\work\\a.txt', 'C:\\Users\\x')).toBe('D:\\work\\a.txt');
    expect(resolveDraft('D:\\work\\a', 'C:\\Users\\x')).toEqual({ dirInput: 'D:\\work\\', dirAbs: 'D:\\work\\', needle: 'a' });
    expect(isAbsolutePath('\\\\host\\share\\f')).toBe(true);
    expect(isAbsolutePath('C:')).toBe(true);
  });

  it('resolves a relative draft under a Windows cwd', () => {
    expect(resolveForOpen('a.txt', 'C:\\Users\\x')).toBe('C:\\Users\\x\\a.txt');
    expect(resolveDraft('src\\ma', 'C:\\Users\\x')).toEqual({ dirInput: 'src\\', dirAbs: 'C:\\Users\\x\\src\\', needle: 'ma' });
    expect(resolveForOpen('a.txt', 'C:/Users/x')).toBe('C:/Users/x/a.txt');
  });

  it('leaves Linux behaviour unchanged', () => {
    expect(resolveForOpen('/etc/hosts', '/home/u')).toBe('/etc/hosts');
    expect(resolveForOpen('~/a', '/home/u')).toBe('~/a');
    expect(resolveForOpen('a/b', '/home/u')).toBe('/home/u/a/b');
    expect(resolveForOpen('a', '/home/u/')).toBe('/home/u/a');
    expect(resolveDraft('src/ma', '/home/u')).toEqual({ dirInput: 'src/', dirAbs: '/home/u/src/', needle: 'ma' });
    expect(resolveDraft('ma', undefined)).toEqual({ dirInput: '', dirAbs: '~', needle: 'ma' });
    // A backslash is an ordinary filename character on Linux.
    expect(resolveDraft('a\\b', '/home/u')).toEqual({ dirInput: '', dirAbs: '/home/u', needle: 'a\\b' });
    expect(resolveForOpen('a.txt', '/home/u/dir\\')).toBe('/home/u/dir\\/a.txt');
    expect(stripTrailingSep('/a/b/')).toBe('/a/b');
  });
});
