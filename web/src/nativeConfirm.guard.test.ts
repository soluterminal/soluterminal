import { readdirSync, readFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import * as ts from 'typescript';
import { expect, it } from 'vitest';

function nativeConfirmReferences(text: string, file: string): number[] {
  // Most runtime files cannot contain this reference. Avoid constructing
  // their ASTs so the inventory stays fast on the public-export CI runner.
  if (!text.includes('window') || !text.includes('confirm')) return [];
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true);
  const lines: number[] = [];
  const visit = (node: ts.Node) => {
    if ((ts.isPropertyAccessExpression(node) || ts.isElementAccessExpression(node)) &&
      ts.isIdentifier(node.expression) && node.expression.text === 'window') {
      const name = ts.isPropertyAccessExpression(node) ? node.name.text
        : node.argumentExpression && ts.isStringLiteralLike(node.argumentExpression) ? node.argumentExpression.text : '';
      if (name === 'confirm') lines.push(source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1);
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return lines;
}

function runtimeSources(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const file = join(dir, entry.name);
    if (entry.isDirectory()) return entry.name === '__tests__' || entry.name === 'test' ? [] : runtimeSources(file);
    return /\.(?:[cm]?js|jsx|tsx?)$/.test(file) && !/\.(?:test|spec)\.[^.]+$/.test(file) && !file.endsWith('.d.ts') ? [file] : [];
  });
}

it('rejects native window.confirm references in runtime web code', () => {
  const hits = runtimeSources(__dirname).flatMap((file) =>
    nativeConfirmReferences(readFileSync(file, 'utf8'), file).map((line) => `${relative(__dirname, file)}:${line}`));
  expect(hits).toEqual([]);
});

it('detects direct, optional and bracket confirm references while ignoring comments and strings', () => {
  expect(nativeConfirmReferences(`
    // window.confirm('comment')
    const explanation = "window.confirm('string')";
    window.confirm('direct');
    window?.confirm?.('optional');
    window['confirm']('bracket');
  `, 'fixture.ts')).toEqual([4, 5, 6]);
});
