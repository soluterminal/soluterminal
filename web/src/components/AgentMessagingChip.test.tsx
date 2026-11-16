import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import type { PaneInfo } from '../types';
import { AGENT_MESSAGING_COPY, AgentMessagingChip } from './AgentMessagingChip';

const apiMock = vi.hoisted(() => ({ setAgentMessagingPaused: vi.fn() }));
vi.mock('../api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../api')>()),
  api: apiMock,
}));
vi.mock('../lib/useDaemonHealth', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../lib/useDaemonHealth')>()),
  refreshDaemonHealth: vi.fn(),
}));

function pane(id: string, extra: Partial<PaneInfo> = {}): PaneInfo {
  return { id, status: 'running', ...extra } as PaneInfo;
}

const held = (n: number) => Array.from({ length: n }, (_, i) => ({ id: `h${i}`, sender: '@lead', text: 'hi', at: 1 }));

describe('AgentMessagingChip', () => {
  beforeEach(() => {
    useStore.setState({ panes: {}, refreshPanes: vi.fn().mockResolvedValue(undefined), toast: vi.fn() } as never);
    apiMock.setAgentMessagingPaused.mockReset();
  });
  afterEach(() => vi.clearAllMocks());

  it('stays hidden with fewer than two panes while messaging is on', () => {
    useStore.setState({ panes: { a: pane('a') } });
    const { container } = render(<AgentMessagingChip paused={false} />);
    expect(container.textContent).toBe('');
  });

  it('always shows a pause, with the held count, even with one pane', () => {
    useStore.setState({ panes: { a: pane('a', { heldMessages: held(2) }) } });
    render(<AgentMessagingChip paused />);
    expect(screen.getByRole('button', { pressed: true }).textContent).toContain(`${AGENT_MESSAGING_COPY.paused} · 2 held`);
  });

  it('pauses in one click and shows the answer before the next health poll', async () => {
    useStore.setState({ panes: { a: pane('a'), b: pane('b') } });
    apiMock.setAgentMessagingPaused.mockResolvedValue({ paused: true, held: 0 });
    render(<AgentMessagingChip paused={false} />);
    const button = screen.getByRole('button', { pressed: false });
    expect(button.textContent).toContain(AGENT_MESSAGING_COPY.on);
    await act(async () => { fireEvent.click(button); });
    expect(apiMock.setAgentMessagingPaused).toHaveBeenCalledWith(true);
    expect(screen.getByRole('button', { pressed: true }).textContent).toContain(AGENT_MESSAGING_COPY.paused);
  });

  it('resumes in one click', async () => {
    useStore.setState({ panes: { a: pane('a'), b: pane('b') } });
    apiMock.setAgentMessagingPaused.mockResolvedValue({ paused: false, held: 0 });
    render(<AgentMessagingChip paused />);
    await act(async () => { fireEvent.click(screen.getByRole('button', { pressed: true })); });
    expect(apiMock.setAgentMessagingPaused).toHaveBeenCalledWith(false);
    expect(screen.getByRole('button', { pressed: false }).textContent).toContain(AGENT_MESSAGING_COPY.on);
  });

  it('keeps the state and says why when the request fails', async () => {
    useStore.setState({ panes: { a: pane('a'), b: pane('b') } });
    apiMock.setAgentMessagingPaused.mockRejectedValue(new Error('could not save the setting'));
    render(<AgentMessagingChip paused={false} />);
    await act(async () => { fireEvent.click(screen.getByRole('button')); });
    const button = screen.getByRole('button', { pressed: false });
    expect(button.getAttribute('aria-label')).toContain("Couldn't pause: could not save the setting");
  });

  it('applies a pause whose setting could not be saved, and says so', async () => {
    useStore.setState({ panes: { a: pane('a'), b: pane('b') } });
    const warning = 'Paused for now, but the setting couldn\'t be saved: restarting SoluTerminal would lift the pause.';
    apiMock.setAgentMessagingPaused.mockResolvedValue({ paused: true, held: 0, warning, warningKind: 'unsaved' });
    render(<AgentMessagingChip paused={false} />);
    await act(async () => { fireEvent.click(screen.getByRole('button')); });
    const button = screen.getByRole('button', { pressed: true });
    expect(button.textContent).toContain(`${AGENT_MESSAGING_COPY.paused} · ${AGENT_MESSAGING_COPY.unsavedSuffix}`);
    expect(button.getAttribute('aria-label')).toContain(warning);
    expect(useStore.getState().toast).toHaveBeenCalledWith(warning, { kind: 'warn' });
  });

  it('shows an unreadable saved setting even with one pane, toasting it once', () => {
    useStore.setState({ panes: { a: pane('a') } });
    const warning = "The saved agent-messaging setting couldn't be read, so agent messaging is on.";
    const { rerender } = render(<AgentMessagingChip paused={false} warning={warning} warningKind="unreadable" />);
    expect(screen.getByRole('button', { pressed: false }).textContent).toContain(`${AGENT_MESSAGING_COPY.on} · ${AGENT_MESSAGING_COPY.unreadableSuffix}`);
    rerender(<AgentMessagingChip paused={false} warning={warning} warningKind="unreadable" />);
    expect(useStore.getState().toast).toHaveBeenCalledTimes(1);
  });

  it('dismisses an unreadable-setting notice without changing the state', async () => {
    useStore.setState({ panes: { a: pane('a'), b: pane('b') } });
    apiMock.setAgentMessagingPaused.mockResolvedValue({ paused: false, held: 0 });
    const warning = "The saved agent-messaging setting couldn't be read, so agent messaging is on.";
    render(<AgentMessagingChip paused={false} warning={warning} warningKind="unreadable" />);
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: /^Dismiss/ })); });
    expect(apiMock.setAgentMessagingPaused).toHaveBeenCalledWith(false);
    expect(screen.queryByRole('button', { name: /^Dismiss/ })).toBeNull();
    expect(screen.getByRole('button', { pressed: false }).textContent).toBe(AGENT_MESSAGING_COPY.on);
  });

  it('offers no Dismiss for an unsaved warning', () => {
    useStore.setState({ panes: { a: pane('a'), b: pane('b') } });
    render(<AgentMessagingChip paused warning="Paused for now, but…" warningKind="unsaved" />);
    expect(screen.queryByRole('button', { name: /^Dismiss/ })).toBeNull();
  });

  it('says a Dismiss whose save failed can be tried again, and keeps offering it', async () => {
    useStore.setState({ panes: { a: pane('a'), b: pane('b') } });
    const warning = "The saved agent-messaging setting couldn't be read, so agent messaging is on.";
    // The daemon answers 200 and keeps the unreadable notice when the save fails.
    apiMock.setAgentMessagingPaused.mockResolvedValue({ paused: false, held: 0, warning, warningKind: 'unreadable' });
    render(<AgentMessagingChip paused={false} warning={warning} warningKind="unreadable" />);
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: /^Dismiss/ })); });
    expect(screen.getByRole('button', { pressed: false }).getAttribute('aria-label')).toBe(AGENT_MESSAGING_COPY.dismissFailed);
    expect(screen.getByRole('button', { name: /^Dismiss/ })).toBeInTheDocument();
  });

  it('keeps the reason when a Dismiss request is refused', async () => {
    useStore.setState({ panes: { a: pane('a'), b: pane('b') } });
    apiMock.setAgentMessagingPaused.mockRejectedValue(new Error('403 forbidden'));
    const warning = "The saved agent-messaging setting couldn't be read, so agent messaging is on.";
    render(<AgentMessagingChip paused={false} warning={warning} warningKind="unreadable" />);
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: /^Dismiss/ })); });
    expect(screen.getByRole('button', { pressed: false }).getAttribute('aria-label')).toBe("Couldn't dismiss: 403 forbidden");
  });
});
