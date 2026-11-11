import { beforeEach, describe, expect, it } from 'vitest';
import {
  agentMemoryGrowingFast, agentMemoryLabel, paneAgentRssMB, recordAgentMemory,
  resetAgentMemoryGrowthForTests, unusualMemoryThresholdMB,
} from './agentMemory';

describe('pane agent memory compatibility', () => {
  it('uses provider-neutral memory for Codex panes', () => {
    expect(paneAgentRssMB({ agentRssMB: 384, claudeRssMB: 99 })).toBe(384);
  });

  it('falls back to the legacy Claude-named wire field', () => {
    expect(paneAgentRssMB({ claudeRssMB: 512 })).toBe(512);
    expect(paneAgentRssMB({})).toBe(0);
  });
});

describe('unusual agent memory', () => {
  it('is at least 1.5 GB, or 10% of the machine', () => {
    expect(unusualMemoryThresholdMB(8192)).toBe(1536);
    expect(unusualMemoryThresholdMB(32768)).toBe(3277);
    expect(unusualMemoryThresholdMB(null)).toBe(1536);
    expect(unusualMemoryThresholdMB(undefined)).toBe(1536);
    expect(unusualMemoryThresholdMB(0)).toBe(1536);
  });

  it('labels MB under a gigabyte and GB from there', () => {
    expect(agentMemoryLabel(384)).toBe('384 MB');
    expect(agentMemoryLabel(1843)).toBe('1.8 GB');
  });
});

describe('fast agent memory growth', () => {
  const min = 60_000;
  beforeEach(() => resetAgentMemoryGrowthForTests());

  it('flags 500 MB more within ten minutes once past 1 GB', () => {
    expect(recordAgentMemory('p', 700, 0)).toBe(false);
    expect(recordAgentMemory('p', 950, 3 * min)).toBe(false);
    expect(recordAgentMemory('p', 1250, 6 * min)).toBe(true);
    expect(agentMemoryGrowingFast('p')).toBe(true);
  });

  it('counts a reading that held since before the window', () => {
    // 600 MB unchanged for half an hour (no new sample), then a jump.
    recordAgentMemory('p', 600, 0);
    expect(recordAgentMemory('p', 1200, 30 * min)).toBe(true);
  });

  it('does not flag slow growth', () => {
    recordAgentMemory('p', 700, 0);
    recordAgentMemory('p', 900, 11 * min);
    recordAgentMemory('p', 1100, 22 * min);
    expect(recordAgentMemory('p', 1300, 33 * min)).toBe(false);
  });

  it('does not flag growth that stays under 1 GB', () => {
    recordAgentMemory('p', 100, 0);
    expect(recordAgentMemory('p', 900, min)).toBe(false);
  });

  it('stays flagged while the agent is big, and clears after a restart', () => {
    recordAgentMemory('p', 600, 0);
    recordAgentMemory('p', 1200, min);
    expect(recordAgentMemory('p', 1150, 30 * min)).toBe(true);
    expect(recordAgentMemory('p', 300, 31 * min)).toBe(false);
    expect(agentMemoryGrowingFast('p')).toBe(false);
  });

  it('keeps each pane apart and ignores missing samples', () => {
    recordAgentMemory('a', 600, 0);
    recordAgentMemory('a', 1200, min);
    expect(agentMemoryGrowingFast('a')).toBe(true);
    expect(agentMemoryGrowingFast('b')).toBe(false);
    expect(recordAgentMemory('a', 0, 2 * min)).toBe(true);
  });
});
