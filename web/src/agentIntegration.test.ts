import { renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from './api';
import { useCloud } from './cloud';
import type { AgentIntegrationSettings } from './api';
import {
  AGENT_INTEGRATION_BODY,
  AGENT_INTEGRATION_NOTICE,
  AGENT_INTEGRATION_TITLE,
  askAgentIntegrationOnce,
  resetAgentIntegrationPromptForTests,
  SETUP_QUIET_MS,
  SETUP_RECHECK_MS,
  setupInProgress,
  useAgentIntegrationPrompt,
} from './agentIntegration';
import { useStore } from './store';
import { CODEX_LOGIN_PANE_TITLE } from './codexBootstrap';
import { LOGIN_PANE_TITLE } from './claudeBootstrap';
import { markSetupStarted, resetSetupActivityForTests } from './lib/setupActivity';
import { useAgentInstallPrompt } from './agentStart';

function settings(over: Partial<AgentIntegrationSettings> = {}): AgentIntegrationSettings {
  return {
    state: '',
    effective: 'ask',
    needsAnswer: true,
    noticePending: false,
    operatorDefault: false,
    changes: [],
    ...over,
  };
}

type DialogReq = { title?: string; message: string; confirmLabel?: string; cancelLabel?: string; resolve: (v: unknown) => void };

function answerDialog(choice: boolean) {
  const opened: DialogReq[] = [];
  useStore.setState({
    openDialog: ((req: DialogReq) => {
      opened.push(req);
      req.resolve(choice);
    }) as never,
  });
  return opened;
}

function captureToasts() {
  const toasts: Array<{ message: string; action?: { label: string } }> = [];
  useStore.setState({
    toast: ((message: string, opts?: { action?: { label: string } }) => {
      toasts.push({ message, action: opts?.action });
    }) as never,
  });
  return toasts;
}

beforeEach(() => {
  resetAgentIntegrationPromptForTests();
  resetSetupActivityForTests();
});
afterEach(() => vi.restoreAllMocks());

describe('askAgentIntegrationOnce', () => {
  it('asks the first-run question and turns it on', async () => {
    vi.spyOn(api, 'getAgentIntegration').mockResolvedValue(settings());
    const set = vi.spyOn(api, 'setAgentIntegration').mockResolvedValue(settings({ state: 'on', effective: 'on', needsAnswer: false }));
    const opened = answerDialog(true);
    captureToasts();
    await askAgentIntegrationOnce();
    expect(opened).toHaveLength(1);
    expect(opened[0].title).toBe(AGENT_INTEGRATION_TITLE);
    expect(opened[0].message).toBe(AGENT_INTEGRATION_BODY);
    expect(opened[0].confirmLabel).toBe('Turn on');
    expect(opened[0].cancelLabel).toBe('Not now');
    expect(set).toHaveBeenCalledWith('on');
  });

  it('says what it really adds, in plain words', () => {
    const copy = [AGENT_INTEGRATION_TITLE, AGENT_INTEGRATION_BODY, AGENT_INTEGRATION_NOTICE].join(' ');
    expect(AGENT_INTEGRATION_BODY).toMatch(/already work together/);
    expect(AGENT_INTEGRATION_BODY).toMatch(/outside SoluTerminal/);
    expect(AGENT_INTEGRATION_BODY).toMatch(/three entries .* hooks/);
    expect(AGENT_INTEGRATION_BODY).toMatch(/uninstalling, removes them/);
    expect(copy).not.toMatch(/daemon|solud|MCP|\bhost\b|distro/i);
  });

  it('records "Not now" as off and is never asked twice', async () => {
    const get = vi.spyOn(api, 'getAgentIntegration').mockResolvedValue(settings());
    const set = vi.spyOn(api, 'setAgentIntegration').mockResolvedValue(settings({ state: 'off', effective: 'off', needsAnswer: false }));
    answerDialog(false);
    captureToasts();
    await askAgentIntegrationOnce();
    await askAgentIntegrationOnce();
    expect(set).toHaveBeenCalledWith('off');
    expect(get).toHaveBeenCalledTimes(1);
  });

  it('shows the existing-install notice instead of asking', async () => {
    vi.spyOn(api, 'getAgentIntegration').mockResolvedValue(settings({ state: 'on', effective: 'on', needsAnswer: false, noticePending: true }));
    const ack = vi.spyOn(api, 'ackAgentIntegrationNotice').mockResolvedValue(settings({ state: 'on', effective: 'on', needsAnswer: false }));
    const opened = answerDialog(true);
    const toasts = captureToasts();
    await askAgentIntegrationOnce();
    expect(opened).toHaveLength(0);
    expect(toasts[0].message).toBe(AGENT_INTEGRATION_NOTICE);
    expect(toasts[0].action?.label).toBe('Settings');
    expect(ack).toHaveBeenCalled();
  });

  it('stays quiet when already decided or on an older daemon', async () => {
    const opened = answerDialog(true);
    vi.spyOn(api, 'getAgentIntegration').mockResolvedValueOnce(settings({ state: 'off', effective: 'off', needsAnswer: false }));
    await askAgentIntegrationOnce();
    resetAgentIntegrationPromptForTests();
    vi.spyOn(api, 'getAgentIntegration').mockResolvedValueOnce(null);
    await askAgentIntegrationOnce();
    expect(opened).toHaveLength(0);
  });
});

describe('useAgentIntegrationPrompt', () => {
  const initialCloud = useCloud.getState();
  afterEach(() => useCloud.setState(initialCloud, true));

  async function promptOpens(cloud: { mode: 'local' | 'cloud'; isLocal: boolean }): Promise<boolean> {
    useCloud.setState(cloud);
    const get = vi.spyOn(api, 'getAgentIntegration').mockResolvedValue(settings());
    vi.spyOn(api, 'setAgentIntegration').mockResolvedValue(settings({ state: 'off', effective: 'off', needsAnswer: false }));
    const opened = answerDialog(false);
    captureToasts();
    renderHook(() => useAgentIntegrationPrompt(true));
    // Let any request the hook started settle before judging it.
    await new Promise((r) => setTimeout(r, 10));
    expect(opened.length).toBe(get.mock.calls.length);
    return opened.length === 1;
  }

  it('asks on the desktop, which boots in cloud mode with isLocal', async () => {
    expect(await promptOpens({ mode: 'cloud', isLocal: true })).toBe(true);
  });

  it('asks on a plain local install', async () => {
    expect(await promptOpens({ mode: 'local', isLocal: false })).toBe(true);
  });

  it('never asks a relayed session (phone or cloud relay)', async () => {
    expect(await promptOpens({ mode: 'cloud', isLocal: false })).toBe(false);
    expect(api.getAgentIntegration).not.toHaveBeenCalled();
  });

  it('waits until the first-run screens are closed', async () => {
    useCloud.setState({ mode: 'cloud', isLocal: true });
    const get = vi.spyOn(api, 'getAgentIntegration').mockResolvedValue(settings());
    answerDialog(false);
    const { rerender } = renderHook(({ ready }) => useAgentIntegrationPrompt(ready), { initialProps: { ready: false } });
    await new Promise((r) => setTimeout(r, 0));
    expect(get).not.toHaveBeenCalled();
    rerender({ ready: true });
    await waitFor(() => expect(get).toHaveBeenCalledTimes(1));
  });
});

describe('consent question timing', () => {
  const initialCloud = useCloud.getState();
  beforeEach(() => {
    vi.useFakeTimers();
    useCloud.setState({ mode: 'cloud', isLocal: true });
  });
  afterEach(() => {
    vi.useRealTimers();
    useCloud.setState(initialCloud, true);
    useStore.setState({ panes: {} });
    useAgentInstallPrompt.setState({ agent: null });
  });

  function setPanes(titles: string[]) {
    useStore.setState({
      panes: Object.fromEntries(titles.map((title, i) => [`p${i}`, { id: `p${i}`, title, cmd: [] }])) as never,
    });
  }

  it('waits while the sign-in pane the checklist opened is up, then asks', async () => {
    const get = vi.spyOn(api, 'getAgentIntegration').mockResolvedValue(settings());
    vi.spyOn(api, 'setAgentIntegration').mockResolvedValue(settings({ state: 'off', effective: 'off', needsAnswer: false }));
    const opened = answerDialog(false);
    captureToasts();
    // Clicking "Log in" marks the step, opens the pane, then closes the checklist.
    markSetupStarted();
    setPanes(['shell', CODEX_LOGIN_PANE_TITLE]);
    renderHook(() => useAgentIntegrationPrompt(true));
    await vi.advanceTimersByTimeAsync(SETUP_QUIET_MS + SETUP_RECHECK_MS);
    expect(get).not.toHaveBeenCalled();
    expect(opened).toHaveLength(0);

    // Sign-in done and its pane closed: the next idle check asks.
    setPanes(['shell']);
    await vi.advanceTimersByTimeAsync(SETUP_RECHECK_MS);
    expect(get).toHaveBeenCalledTimes(1);
    expect(opened).toHaveLength(1);
  });

  it('waits out the quiet window after a setup step even with no pane left', async () => {
    const get = vi.spyOn(api, 'getAgentIntegration').mockResolvedValue(settings());
    answerDialog(false);
    markSetupStarted();
    renderHook(() => useAgentIntegrationPrompt(true));
    await vi.advanceTimersByTimeAsync(SETUP_QUIET_MS - SETUP_RECHECK_MS);
    expect(get).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(2 * SETUP_RECHECK_MS);
    expect(get).toHaveBeenCalledTimes(1);
  });

  it('counts install and sign-in panes and an open install card as busy', () => {
    expect(setupInProgress()).toBe(false);
    setPanes([LOGIN_PANE_TITLE]);
    expect(setupInProgress()).toBe(true);
    setPanes(['claude']);
    expect(setupInProgress()).toBe(false);
    useAgentInstallPrompt.setState({ agent: 'codex' });
    expect(setupInProgress()).toBe(true);
  });
});
