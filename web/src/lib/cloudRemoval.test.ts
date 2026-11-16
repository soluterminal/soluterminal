import { describe, expect, it } from 'vitest';
import { disconnectMessage } from './cloudRemoval';

describe('disconnectMessage', () => {
  const url = 'https://cloud.example/';
  it('calls only a confirmed removal a removal', () => {
    expect(disconnectMessage({ cloudRemoval: 'removed', machinesUrl: url }))
      .toEqual({ text: 'Cloud disconnected, and this computer was removed from your account.', warn: false });
    const unlisted = disconnectMessage({ cloudRemoval: 'unlisted', machinesUrl: url });
    expect(unlisted.warn).toBe(true);
    expect(unlisted.text).not.toMatch(/was removed/);
    expect(unlisted.text).toContain("no longer lists this computer, or didn't accept the removal");
    expect(unlisted.text).toContain(url);
  });
  it('tells the person to remove a refused computer, with the address as text', () => {
    const refused = disconnectMessage({ cloudRemoval: 'refused', machinesUrl: url });
    expect(refused.warn).toBe(true);
    expect(refused.text).toContain("didn't remove this computer");
    expect(refused.text).toContain(url);
  });
  it('says a pending removal will finish later', () => {
    expect(disconnectMessage({ cloudRemoval: 'pending', machinesUrl: url }).text).toMatch(/will finish when the cloud answers/);
  });
  it('keeps the old message for an older daemon or an unpaired computer', () => {
    expect(disconnectMessage(undefined).text).toMatch(/no longer connects to the cloud/);
  });
});
