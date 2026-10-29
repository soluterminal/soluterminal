import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { describe, expect, it } from 'vitest';

const SRC = join(__dirname, '..');

function sourceFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) return sourceFiles(path);
    return /\.(tsx?|css|html|json|md)$/.test(name) ? [path] : [];
  });
}

describe('Windows-only copy', () => {
  it('promises no other platforms anywhere in the app', () => {
    // Built from parts so this file doesn't match itself.
    const promise = new RegExp(['coming', 'soon'].join('\\s+'), 'i');
    const hits = sourceFiles(SRC).filter((file) => promise.test(readFileSync(file, 'utf8')));
    expect(hits.map((file) => relative(SRC, file))).toEqual([]);
  });
});
