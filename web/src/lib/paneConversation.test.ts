import { describe, expect, it } from 'vitest';
import type { PaneInfo } from '../types';
import { paneConversation } from './paneConversation';

const pane = (p: Partial<PaneInfo>) => ({ id: 'p', cmd: ['bash'], title: 'Build', status: 'running', ...p }) as PaneInfo;

describe('paneConversation', () => {
  it('is the Claude session of a resumable Claude pane', () => {
    expect(paneConversation(pane({ cmd: ['claude'], claudeSessionId: 'session-1', resumeReady: true }))).toEqual({ id: 'session-1', noun: 'session' });
  });
  it('is the thread of a Codex pane, shell-launched or not', () => {
    expect(paneConversation(pane({ agent: 'codex', kind: 'native', codexThreadId: 'thread-1', resumeReady: true }))).toEqual({ id: 'thread-1', noun: 'thread' });
    expect(paneConversation(pane({ hasCodex: true, codexThreadId: 'thread-2', resumeReady: true }))).toEqual({ id: 'thread-2', noun: 'thread' });
  });
  it('is the Muse session of a Muse pane', () => {
    expect(paneConversation(pane({ agent: 'muse', kind: 'native', museSessionId: 'muse-1', resumeReady: true }))).toEqual({ id: 'muse-1', noun: 'session' });
  });
  it('is nothing until the conversation can be resumed', () => {
    // The daemon pins claudeSessionId at SessionStart, before the transcript exists.
    expect(paneConversation(pane({ cmd: ['claude'], claudeSessionId: 'session-1' }))).toBeNull();
    expect(paneConversation(pane({ resumeReady: true }))).toBeNull();
  });
});
