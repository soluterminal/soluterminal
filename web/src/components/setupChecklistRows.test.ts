import { describe, expect, it, vi } from 'vitest';
import { accountRow, agentReady, agentRow, cleanAgentVersion, whereAgentsRunRow } from './setupChecklistRows';

const noop = { installing: false, preparing: false, onInstall: () => {}, onLogin: () => {} };

describe('agentRow', () => {
  it('never says "not installed" before the first probe answers', () => {
    expect(agentRow('codex', null, noop)).toMatchObject({ detail: 'Checking…', status: 'pending' });
    expect(agentRow('codex', { checked: false, installed: false, version: '', authed: false }, noop).status).toBe('pending');
  });

  it('shows Installing while an install pane runs, then Log in, then Ready', () => {
    const missing = { checked: true, installed: false, version: '', authed: false };
    expect(agentRow('codex', missing, { ...noop, installing: true })).toMatchObject({ status: 'pending' });
    expect(agentRow('codex', missing, { ...noop, installing: true }).fix).toBeUndefined();
    const signedOut = agentRow('codex', { checked: true, installed: true, version: 'codex-cli 0.164.0', authed: false }, noop);
    expect(signedOut).toMatchObject({ status: 'warn', fix: { label: 'Log in' } });
    expect(signedOut.detail).toContain('version 0.164.0');
    expect(agentRow('claude', { checked: true, installed: true, version: '2.1.306 (Claude Code)', authed: true }, noop))
      .toMatchObject({ status: 'ok', detail: 'Ready to use (version 2.1.306).' });
  });

  it('uses plain words only', () => {
    for (const s of [null, { checked: true, installed: false, version: '', authed: false }, { checked: true, installed: true, version: '', authed: false }]) {
      for (const agent of ['claude', 'codex'] as const) {
        const { detail } = agentRow(agent, s, noop);
        expect(detail).not.toMatch(/CLI|daemon|this side|`/);
      }
    }
  });
});

describe('agentReady', () => {
  it('needs installed and signed in', () => {
    expect(agentReady(null)).toBe(false);
    expect(agentReady('unsupported')).toBe(false);
    expect(agentReady({ checked: true, installed: true, version: '', authed: false })).toBe(false);
    expect(agentReady({ checked: true, installed: true, version: '', authed: true })).toBe(true);
  });
});

describe('cleanAgentVersion', () => {
  it('keeps just the number', () => {
    expect(cleanAgentVersion('codex-cli 0.164.0')).toBe('0.164.0');
    expect(cleanAgentVersion('2.1.306 (Claude Code)')).toBe('2.1.306');
  });
});

describe('accountRow', () => {
  it('is Optional when signed out and a tick when signed in', () => {
    expect(accountRow('cloud', 'x', false, false)).toMatchObject({ status: 'optional', badge: 'Optional' });
    expect(accountRow('local', 'x', false, false).status).toBe('optional');
    expect(accountRow('cloud', 'x', true, false).status).toBe('ok');
    expect(accountRow('detecting', 'x', false, false).status).toBe('pending');
  });
});

describe('whereAgentsRunRow', () => {
  it('names the active environment', () => {
    expect(whereAgentsRunRow({ backend: { actual: 'wsl' } })?.detail).toBe('Linux (WSL)');
    expect(whereAgentsRunRow({ backend: { actual: 'dual' } })?.detail).toBe('Linux (WSL) and Windows PowerShell');
    expect(whereAgentsRunRow({ backend: null, healthBackendID: 'win' })?.detail).toBe('Windows PowerShell');
    expect(whereAgentsRunRow({ backend: null, shellPlatform: 'windows-amd64' })?.detail).toBe('Windows PowerShell');
    expect(whereAgentsRunRow({ backend: null, paneEnvironment: 'wsl' })?.detail).toBe('Linux (WSL)');
    expect(whereAgentsRunRow({ backend: null, paneEnvironment: 'windows' })?.detail).toBe('Windows PowerShell');
    expect(whereAgentsRunRow({ backend: null })).toBeNull();
  });

  it('offers optional WSL setup on Windows PowerShell only when the app can run it', () => {
    const onSetUpWsl = vi.fn();
    const withAction = whereAgentsRunRow({ backend: { actual: 'host', requested: 'host' }, onSetUpWsl });
    expect(withAction).toMatchObject({ status: 'ok', detail: 'Windows PowerShell. Setting up Linux (WSL) is optional.' });
    withAction?.fix?.onClick();
    expect(onSetUpWsl).toHaveBeenCalledOnce();
    expect(whereAgentsRunRow({ backend: { actual: 'host' } })?.fix).toBeUndefined();
    expect(whereAgentsRunRow({ backend: { actual: 'wsl' }, onSetUpWsl })?.fix).toBeUndefined();
  });

  it('offers to use Linux (WSL), not set it up, when the PC already has a distro', () => {
    const onSetUpWsl = vi.fn();
    const row = whereAgentsRunRow({ backend: { actual: 'host', requested: 'host', wslDistro: true }, onSetUpWsl });
    expect(row).toMatchObject({
      status: 'ok',
      detail: 'Windows PowerShell. Linux (WSL) is on this PC too.',
      fix: { label: 'Use Linux (WSL)' },
    });
    row?.fix?.onClick();
    expect(onSetUpWsl).toHaveBeenCalledOnce();
    // No distro, or a launch that did not look: setting it up stays the offer.
    for (const wslDistro of [false, null, undefined]) {
      const other = whereAgentsRunRow({ backend: { actual: 'host', requested: 'host', wslDistro }, onSetUpWsl });
      expect(other).toMatchObject({
        detail: 'Windows PowerShell. Setting up Linux (WSL) is optional.',
        fix: { label: 'Set up Linux (WSL)' },
      });
    }
  });

  it('warns when Linux (WSL) did not start', () => {
    const r = whereAgentsRunRow({ backend: { actual: 'host', requested: 'wsl', fallback: true }, onSetUpWsl: () => {} });
    expect(r).toMatchObject({ status: 'warn', fix: { label: 'Set up Linux (WSL)' } });
  });
});
