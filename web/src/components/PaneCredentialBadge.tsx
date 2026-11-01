import { useState, type MouseEvent } from 'react';
import { api } from '../api';
import { useStore } from '../store';
import { useTeamFeatures } from '../entitlements';
import type { PaneInfo } from '../types';
import { useHideIdentities } from '../lib/hideIdentities';
import { restartPaneFlow } from '../lib/paneRestart';
import { maskIdentity } from '../lib/identityLabel';
import { TopbarPopover } from './TopbarPopover';
import { sectionOffered } from './sidebar/sidebarSections';

// The pane's account chip speaks only when something needs the user, or is
// visibly in motion. A healthy session (live, following, or already on the
// selected account) renders nothing. When there is a problem, the chip opens a
// popover that names it and offers the one action that fixes it.
//
// Launch metadata cannot prove a CLI's current billing identity: "still on X"
// means the session was started on X and nothing has confirmed a switch since.

type PaneAccountPane = Pick<PaneInfo, 'credential' | 'codexAuth'> & Partial<Pick<PaneInfo, 'id'>>;

export type PaneAccountAction = 'reconnect' | 'cancel' | 'restart' | 'credentials';

export interface PaneAccountStatus {
  tone: 'warn' | 'muted' | 'info';
  label: string;
  title: string;
  detail?: {
    heading: string;
    body: string;
    note?: string;
    error?: string;
    action?: { kind: PaneAccountAction; label: string };
  };
}

function recoveryText(recovery?: string) {
  return recovery === 'waiting' ? 'model busy · retry scheduled'
    : recovery === 'retrying' ? 'recovering interrupted turn'
    : recovery === 'exhausted' ? 'model busy · retry paused' : '';
}

function recoveryStatus(recovery: string, retryAt?: number): PaneAccountStatus {
  const next = retryAt ? ` Next attempt after ${new Date(retryAt).toLocaleTimeString()}.` : '';
  return { tone: 'info', label: recovery, title: `${recovery}.${next} Type in the terminal to cancel automatic recovery. Completed work remains in this conversation.` };
}

export function paneAccountStatus(pane: PaneAccountPane, name: (account?: string) => string): PaneAccountStatus | null {
  const credential = pane.credential;
  const launched = credential?.account ? name(credential.account) : 'its launch account';
  const active = credential?.activeAccount ? name(credential.activeAccount) : 'another account';
  const mismatch = !!credential && (credential.restartRequired || (!!credential.account && !!credential.activeAccount && credential.account !== credential.activeAccount));
  const stillOn: PaneAccountStatus = { tone: 'warn', label: credential?.account ? `still on ${launched}` : 'account changed', title: `Started on ${launched}; ${active} is selected.` };
  const restart = { kind: 'restart' as const, label: 'Restart session' };

  const auth = pane.codexAuth;
  if (auth) {
    switch (auth.state) {
      case 'live': {
        const recovery = recoveryText(auth.recovery);
        return recovery ? recoveryStatus(recovery, auth.retryAt) : null;
      }
      case 'switching':
        return { tone: 'info', label: 'switching account', title: 'Waiting for Codex to accept the selected account.' };
      case 'switch-pending':
      case 'waiting-for-idle':
        return { tone: 'info', label: 'switch after current turn', title: 'Codex adopts the selected account once its active work, tools and approvals finish.' };
      case 'starting':
        return { tone: 'info', label: 'connecting account', title: 'Connecting this session to the selected account.' };
      case 'signed-out':
        return { tone: 'warn', label: 'signed out', title: 'Codex is signed out.', detail: {
          heading: 'Codex is signed out',
          body: 'Sign in again in Credentials.',
          action: { kind: 'credentials', label: 'Open Credentials' },
        } };
      case 'needs-attention':
        return { tone: 'warn', label: 'login needs attention', title: 'The Codex login could not be refreshed.', detail: {
          heading: 'Codex login needs attention',
          body: 'The live login could not be refreshed. Check the account in Credentials.',
          action: { kind: 'credentials', label: 'Open Credentials' },
        } };
      case 'reconnect-required':
        break;
      default:
        return { tone: 'muted', label: 'account unverified', title: 'This session’s account adoption is unknown.' };
    }

    if (auth.reconnectArmed) {
      return { tone: 'info', label: 'reconnects when idle', title: 'Reconnect queued for the next idle prompt.', detail: {
        heading: 'Reconnect queued',
        body: 'Runs at Codex’s next idle prompt: no draft, approval or background terminal.',
        action: { kind: 'cancel', label: 'Cancel' },
      } };
    }
    if (auth.canReconnect) {
      const note = [
        'Waits for an idle prompt if Codex is busy.',
        ...(auth.reconnectMode === 'shell' ? ['This pane’s shell and its background jobs are replaced; you get a fresh shell when Codex exits.'] : []),
      ].join(' ');
      const action = { kind: 'reconnect' as const, label: auth.reconnectError ? 'Try again' : 'Reconnect' };
      if (mismatch) {
        return { ...stillOn, detail: {
          heading: `Still using ${launched}`,
          body: `You selected ${active}. Reconnect resumes this conversation on ${active} and keeps later switches automatic.`,
          note, error: auth.reconnectError, action,
        } };
      }
      return { tone: auth.reconnectError ? 'warn' : 'muted', label: auth.reconnectError ? 'reconnect failed' : 'not live', title: 'Account switches won’t reach this session.', detail: {
        heading: 'Account switches won’t reach this session',
        body: 'It stays on its current account if you or auto-switch change accounts. Reconnect resumes this conversation with live switching.',
        note, error: auth.reconnectError, action,
      } };
    }
    if (auth.blocker === 'team') {
      return { tone: 'warn', label: 'restart for account', title: 'Restart to use the selected account.', detail: {
        heading: 'Restart to use the selected account',
        body: 'This session uses a different login. Restart resumes the conversation on the selected account.',
        action: restart,
      } };
    }
    if (!mismatch) return null; // Nothing to fix until the selection moves.
    return { ...stillOn, detail: {
      heading: `Still using ${launched}`,
      body: `You selected ${active}. This session can’t switch live${auth.blocker === 'custom-launch' ? ' (custom launch options)' : ''}; restart it to pick up ${active}.`,
      note: auth.blocker === 'custom-launch' ? 'Restart resumes the conversation without the custom launch options.' : undefined,
      action: restart,
    } };
  }

  if (!credential) return null;
  if (credential.restartRequired) {
    return { ...stillOn, detail: {
      heading: `Still using ${launched}`,
      body: `${active} is selected. This CLI picks it up when it restarts; restart resumes the conversation.`,
      action: restart,
    } };
  }
  const recovery = recoveryText(credential.recovery);
  if (recovery) return recoveryStatus(recovery, credential.retryAt);
  if (credential.liveState === 'switch-pending' || credential.liveState === 'switching') {
    return { tone: 'info', label: 'switch after current turn', title: `Account change queued until this pane finishes its current work. Selected account: ${active}.` };
  }
  return null;
}

export function PaneCredentialBadge({ pane }: { pane: PaneAccountPane }) {
  const [hidden] = useHideIdentities();
  const [working, setWorking] = useState(false);
  const [error, setError] = useState('');
  const team = useTeamFeatures();
  const status = paneAccountStatus(pane, (account) => account ? (hidden ? maskIdentity(account) : account) : 'unknown');
  if (!status) return null;
  const className = `pane-account-chip is-${status.tone}`;
  // The pane header is draggable and double-click zooms it; badges opt out.
  const stop = {
    onMouseDownCapture: (e: MouseEvent) => e.stopPropagation(),
    onDoubleClick: (e: MouseEvent) => e.stopPropagation(),
    onClick: (e: MouseEvent) => e.stopPropagation(),
  };
  // Offer the registered repair surface when it is available. Otherwise,
  // direct the user to sign in again from a shell.
  const detail = status.detail?.action?.kind === 'credentials' && !(team && sectionOffered('credentials'))
    ? { ...status.detail, body: 'Run codex login in a shell pane, then restart this pane.', action: { kind: 'restart' as const, label: 'Restart pane' } }
    : status.detail;
  if (!detail || !pane.id) {
    return <span className={className} title={status.title} aria-label={status.title} {...stop}>{status.label}</span>;
  }
  const id = pane.id;

  const run = async (kind: PaneAccountAction, close: () => void) => {
    setError('');
    if (kind === 'credentials') {
      // Credentials is a sidebar section (the Settings tab of that name is
      // the Shinoque gateway), so reveal it even when the user hid it.
      close();
      window.dispatchEvent(new CustomEvent('solu:reveal-sidebar-section', { detail: { key: 'credentials' } }));
      return;
    }
    if (kind === 'restart') { close(); await restartPaneFlow(id); return; }
    setWorking(true);
    try {
      if (kind === 'cancel') {
        await api.reconnectCodexAuth(id, { cancel: true });
      } else {
        const result = await api.reconnectCodexAuth(id, { when: 'idle' });
        useStore.getState().toast(result.armed ? 'Codex reconnects at its next idle prompt' : 'Reconnecting Codex — conversation kept', { kind: 'info', icon: '↻', ttlMs: 3000 });
      }
      await useStore.getState().refreshPanes();
      close();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not reconnect Codex');
    } finally { setWorking(false); }
  };

  return (
    <TopbarPopover
      align="left"
      minWidth={260}
      onOpenChange={(open) => { if (!open) setError(''); }}
      trigger={({ toggle, open }) => (
        <span {...stop}>
          <button type="button" className={className} title={status.title} aria-label={status.title} aria-expanded={open}
            onClick={(e) => { e.stopPropagation(); toggle(); }}>{status.label}</button>
        </span>
      )}
    >
      {({ close }) => (
        <div className="pane-account-popover" role="dialog" aria-label={detail.heading}>
          <div className="pane-account-heading">{detail.heading}</div>
          <p className="pane-account-body">{detail.body}</p>
          {detail.note && <p className="pane-account-note">{detail.note}</p>}
          {(error || detail.error) && <p className="pane-account-error" role="alert">{error || detail.error}</p>}
          {detail.action && (
            <div className="pane-account-actions">
              <button type="button" className="pane-account-action" disabled={working}
                onClick={() => void run(detail.action!.kind, close)}>{working ? 'Working…' : detail.action.label}</button>
            </div>
          )}
        </div>
      )}
    </TopbarPopover>
  );
}
