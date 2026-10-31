import { describe, expect, it } from 'vitest';
import type { SystemStatus } from '../../api';
import { plainBlockingLine } from './systemPlain';

describe('plainBlockingLine', () => {
  const status = (hostDisk: 'warn' | 'critical', layer: 'host' | 'both') => ({
    disk: { blockingLayer: layer, freeMB: 4096, host: { volume: 'C:', freeMB: 41000 } },
    level: { overall: hostDisk, reasons: [], areas: { hostDisk } },
  }) as unknown as SystemStatus;

  it('says the Windows drive is almost full only when the alarm is critical', () => {
    expect(plainBlockingLine(status('critical', 'host'))).toMatch(/^Your Windows drive C: is almost full/);
    expect(plainBlockingLine(status('warn', 'host'))).toMatch(/^Some background work waits until your Windows drive C: has more space/);
    expect(plainBlockingLine(status('warn', 'host'))).not.toMatch(/almost full/);
    expect(plainBlockingLine(status('critical', 'both'))).toMatch(/are both almost full/);
    expect(plainBlockingLine(status('warn', 'both'))).toMatch(/^Disk space is low \(4\.00 GB free\)\..*Some background work waits/);
  });
});
