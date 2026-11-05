import { describe, expect, it } from 'vitest';
import type { UpdateStatus } from '../types';
import { channelReleaseWait } from './channelReleaseWait';

const status = (extra: Partial<UpdateStatus> = {}): UpdateStatus => ({
  current: '0.0.100',
  latest: '0.0.100',
  available: false,
  lastSuccessMs: 1000,
  runningFlavor: 'public',
  updateChannel: 'team',
  ...extra,
});

describe('channelReleaseWait', () => {
  it('waits when the channel is not the running build and has nothing newer', () => {
    expect(channelReleaseWait(status())).toBe('Waiting for the next team release');
    expect(channelReleaseWait(status({ latest: '0.0.99' }))).toBe('Waiting for the next team release');
    expect(channelReleaseWait(status({ runningFlavor: 'team', updateChannel: 'public' })))
      .toBe('Waiting for the next public release');
  });

  it('says nothing when the channel is the running build', () => {
    expect(channelReleaseWait(status({ runningFlavor: 'team' }))).toBeNull();
    expect(channelReleaseWait(status({ runningFlavor: 'public', updateChannel: 'public' }))).toBeNull();
  });

  it('says nothing when an older daemon reports no running build or no channel', () => {
    expect(channelReleaseWait(null)).toBeNull();
    expect(channelReleaseWait(status({ runningFlavor: undefined }))).toBeNull();
    expect(channelReleaseWait(status({ updateChannel: undefined }))).toBeNull();
  });

  it('says nothing when the channel has something newer, even if it is held back', () => {
    expect(channelReleaseWait(status({ latest: '0.0.101', available: true }))).toBeNull();
    expect(channelReleaseWait(status({ latest: '0.0.101', available: false }))).toBeNull();
  });

  it('says nothing until the channel has answered', () => {
    expect(channelReleaseWait(status({ latest: undefined, lastSuccessMs: undefined }))).toBeNull();
    expect(channelReleaseWait(status({ lastError: 'team updates unavailable' }))).toBeNull();
  });

  it('waits on a successful answer that names no release of the channel', () => {
    expect(channelReleaseWait(status({ latest: undefined, runningFlavor: 'team', updateChannel: 'public' })))
      .toBe('Waiting for the next public release');
  });
});
