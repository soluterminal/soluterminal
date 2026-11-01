import { describe, expect, it } from 'vitest';
import type { PaneInfo } from '../types';
import { paneRowLabel } from './paneRowLabel';

const pane = (title: string, identity?: PaneInfo['identity']) => ({ id: 'p', title, cmd: ['claude'], identity } as PaneInfo);

describe('paneRowLabel', () => {
  it('shows the title as before without a handle', () => {
    expect(paneRowLabel(pane('Release lead'))).toEqual({ handle: '', detail: 'Release lead', text: 'Release lead', tooltip: 'Release lead' });
    expect(paneRowLabel({ ...pane(''), cmd: ['bash', '-l'] }).text).toBe('bash -l');
  });
  it('leads with the handle and keeps the title as detail', () => {
    expect(paneRowLabel(pane('Release lead', { handle: 'cmo' }))).toMatchObject({ handle: '@cmo', detail: 'Release lead', text: '@cmo · Release lead' });
  });
  it('strips a title prefix that repeats the handle', () => {
    expect(paneRowLabel(pane('@cmo · launch', { handle: 'cmo' }))).toMatchObject({ detail: 'launch', text: '@cmo · launch' });
    expect(paneRowLabel(pane('@CMO: launch plan', { handle: 'cmo' })).detail).toBe('launch plan');
    expect(paneRowLabel(pane('@cmo', { handle: 'cmo' }))).toMatchObject({ detail: '', text: '@cmo' });
    expect(paneRowLabel(pane('@cmo — ', { handle: 'cmo' })).text).toBe('@cmo');
    // A longer handle in the title is not the same handle.
    expect(paneRowLabel(pane('@cmo-2 notes', { handle: 'cmo' })).detail).toBe('@cmo-2 notes');
  });
  it('puts the full title and the working-on line in the tooltip', () => {
    expect(paneRowLabel(pane('@cmo · launch', { handle: 'cmo', workingOn: 'Drafting the launch post' })).tooltip).toBe('@cmo · launch\nDrafting the launch post');
    expect(paneRowLabel(pane('Shell', { workingOn: 'Running tests' })).tooltip).toBe('Shell\nRunning tests');
  });
});
