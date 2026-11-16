import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { useStore } from '../store';
import { cleanedByLabel, historyStatusLabel, slowStartLabel, VoiceHistoryInline } from './VoiceHistoryButton';

afterEach(() => {
  useStore.setState({ voiceHistory: [] });
});

describe('voice history cleanup label', () => {
  it('shows who cleaned each dictation', () => {
    useStore.setState({
      voiceHistory: [
        { id: 3, text: 'Run npm test.', paneTitle: 'claude', delivered: true, cleanedBy: 'local' },
        { id: 2, text: 'Grep for TODO.', paneTitle: 'claude', delivered: true, cleanedBy: 'cloud' },
        { id: 1, text: 'raw words', paneTitle: 'claude', delivered: true },
      ],
    });
    render(<VoiceHistoryInline />);
    expect(screen.getByText('cleaned with your Claude plan')).toBeInTheDocument();
    expect(screen.getByText('cleaned by SoluTerminal')).toBeInTheDocument();
    expect(screen.getAllByText(/cleaned (with|by)/)).toHaveLength(2);
  });

  it('labels both paths in plain words', () => {
    expect(cleanedByLabel('local')).toBe('cleaned with your Claude plan');
    expect(cleanedByLabel('cloud')).toBe('cleaned by SoluTerminal');
  });
});

describe('voice history explains each dictation', () => {
  const base = { id: 1, text: 'Run the tests.', paneTitle: 'claude', delivered: true };

  it('names the cleanup outcome', () => {
    expect(historyStatusLabel({ ...base, polish: 'cleaned', cleanedBy: 'local' })).toBe('cleaned with your Claude plan');
    expect(historyStatusLabel({ ...base, polish: 'unchanged', cleanedBy: 'local' })).toBe('checked with your Claude plan, nothing to fix');
    expect(historyStatusLabel({ ...base, polish: 'cleaned', cleanedBy: 'api_key' })).toBe('cleaned with your API key');
    expect(historyStatusLabel({ ...base, polish: 'unchanged', cleanedBy: 'api_key' })).toBe('checked with your API key, nothing to fix');
    expect(historyStatusLabel({ ...base, polish: 'unchanged', cleanedBy: 'cloud' })).toBe('checked by SoluTerminal, nothing to fix');
    expect(historyStatusLabel({ ...base, polish: 'short' })).toBe('too short to clean');
    expect(historyStatusLabel({ ...base, polish: 'kept-raw' })).toBe('you typed first, kept as heard');
    expect(historyStatusLabel({ ...base, polish: 'failed' })).toBe('cleanup failed, kept as heard');
    expect(historyStatusLabel(base)).toBeNull();
  });

  it('flags a slow cold mic start but not a warm one', () => {
    expect(slowStartLabel({ ...base, startDelayMs: 420, warmMic: false })).toBe('mic took 0.4 s to start');
    expect(slowStartLabel({ ...base, startDelayMs: 420, warmMic: true })).toBeNull();
    expect(slowStartLabel({ ...base, startDelayMs: 90, warmMic: false })).toBeNull();
    expect(slowStartLabel(base)).toBeNull();
  });

  it('shows what was heard when cleanup changed it', () => {
    useStore.setState({
      voiceHistory: [{ ...base, text: 'Commit and push.', raw: 'commit and posh', polish: 'cleaned', cleanedBy: 'local' }],
    });
    render(<VoiceHistoryInline />);
    expect(screen.getByText('Heard: commit and posh')).toBeInTheDocument();
    useStore.setState({ voiceHistory: [] });
  });
});
