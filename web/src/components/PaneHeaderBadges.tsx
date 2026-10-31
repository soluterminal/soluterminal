
import { useEffect, useMemo, useState } from 'react';
import { IconBolt, IconClock } from './ui/icons';
import type { PaneInfo } from '../types';
import { useStore } from '../store';
import { agentLabel, effectivePaneAgent } from '../lib/agentKind';
import { formatNextFire } from '../lib/cron';
import { api } from '../api';
import { copyToClipboard } from '../lib/clipboard';

import { PaneAgentStaleDot } from './PaneAgentStaleDot';
import { PaneCredentialBadge } from './PaneCredentialBadge';
import { PaneAutoModeChip } from './PaneAutoModeChip';

// AutoApprovingChip surfaces the daemon's auto-pending attention signal
// where the user's eye already is: the pane header. Two phases share it:
//   1. Evaluation in flight (attention === 'auto-pending', no confirm yet —
//      cautious mode's LLM is deciding): "⚡ auto-approving…", no cancel
//      (there is nothing to cancel until the daemon registers a confirm).
//   2. Server-side countdown armed (an ariaAutoPending entry exists for
//      this pane): live seconds + a cancel button wired to the SAME
//      /api/aria/confirm endpoint —
//      the cancel window is finally visible without having that panel open.
// Renders nothing when neither phase applies, so non-auto panes pay zero.
function AutoApprovingChip({ pane }: { pane: PaneInfo }) {
  const autoPending = useStore((s) => s.ariaAutoPending);
  const resolveAriaAutoPending = useStore((s) => s.resolveAriaAutoPending);
  const entry = useMemo(
    () => autoPending.find((p) => p.paneId === pane.id && p.status === 'pending'),
    [autoPending, pane.id],
  );
  const [nowMs, setNowMs] = useState(() => Date.now());
  const [cancelling, setCancelling] = useState(false);

  const inputHold = pane.kind !== 'native' && !!pane.autoMode && pane.state === 'choice-prompt'
    && (pane.attention === 'auto-pending' || pane.attention === 'needs-user')
    && (pane.attentionReason === 'user typing' || pane.attentionReason?.startsWith('composer has text ('));
  const blocked = !inputHold && !!pane.autoMode && pane.state === 'choice-prompt'
    && pane.attention === 'needs-user' && !!pane.attentionReason;

  const hasCountdown = !inputHold && !blocked && !!entry && typeof entry.deadlineAt === 'number' && entry.countdownMs > 0;
  useEffect(() => {
    if (!hasCountdown) return;
    const h = window.setInterval(() => setNowMs(Date.now()), 250);
    return () => window.clearInterval(h);
  }, [hasCountdown]);

  if (!inputHold && !blocked && pane.attention !== 'auto-pending' && !entry) return null;

  const remainingMs = hasCountdown && entry?.deadlineAt ? Math.max(0, entry.deadlineAt - nowMs) : 0;
  const cancel = async () => {
    if (!entry || cancelling) return;
    setCancelling(true);
    try {
      await api.ariaConfirm(entry.confirmId, false);
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      // Unknown/expired = already fired or cancelled elsewhere — reconcile
      // the local strip entry so the chip doesn't linger (mirrors
      // the daemon's server-side countdown fallback).
      if (/unknown|expired|404/i.test(msg)) {
        resolveAriaAutoPending(entry.confirmId, { approved: false, cancelled: true });
      }
    } finally {
      setCancelling(false);
    }
  };

  return (
    <span
      className={`pane-autoapproving-chip${inputHold || blocked ? ' is-held' : ''}`}
      title={blocked ? `Auto-approval needs your review: ${pane.attentionReason}` : inputHold ? `Auto-approval is waiting: ${pane.attentionReason}` : hasCountdown
        ? 'About to auto-approve this prompt — cancel to handle it yourself'
        : 'Auto-approve is evaluating this prompt'}
      onMouseDownCapture={(e) => e.stopPropagation()}
      onDoubleClick={(e) => e.stopPropagation()}
      onClick={(e) => e.stopPropagation()}
    >
      <span aria-hidden>{inputHold || blocked ? <IconClock size={12} style={{ verticalAlign: '-2px' }} /> : <IconBolt size={12} style={{ verticalAlign: '-2px' }} />}</span>
      {blocked ? ' auto blocked' : inputHold ? ' waiting for input' : hasCountdown ? ` approving in ${Math.ceil(remainingMs / 1000)}s` : ' auto-approving…'}
      {entry && !inputHold && !blocked && (
        <button
          type="button"
          className="pane-autoapproving-cancel"
          disabled={cancelling}
          aria-label="Cancel the pending auto-approval"
          onClick={(e) => {
            e.stopPropagation();
            void cancel();
          }}
        >
          {cancelling ? '…' : 'cancel'}
        </button>
      )}
    </span>
  );
}

// NextScheduleChip surfaces "a scheduled prompt is coming up for THIS pane" in
// the pane's own title bar. You can create one straight from the pane's
// right-click menu ("Create schedule…"), but until now nothing on the pane said
// one existed — you had to go find it in the Schedules sidebar.
//
// Reads the store's already-live `schedules` (bootstrap + the `schedules-changed`
// SSE event keep it fresh), so the chip needs no fetch of its own and updates
// itself as schedules are added/edited/fired.
//
// Filtering on paneId ALONE is exact — the daemon's validator lets only
// prompt-kind schedules carry a paneId ('agent'/'task' kinds are rejected with
// one). Deliberately NOT `kind === 'prompt'`: the dialog omits `kind` entirely
// when saving a prompt schedule (and legacy entries predate the field), so a
// kind check would miss almost every real schedule.
//
// Renders nothing when this pane has no upcoming run, so panes without a
// schedule — nearly all of them — pay nothing beyond the selector.
function NextScheduleChip({ pane }: { pane: PaneInfo }) {
  const schedules = useStore((s) => s.schedules);
  const [nowMs, setNowMs] = useState(() => Date.now());

  const next = useMemo(() => {
    // nextFire is server-computed and absent when a schedule is disabled, so its
    // presence already gates enablement — no need to check `enabled` too.
    const mine = schedules.filter((s) => s.paneId === pane.id && !!s.nextFire);
    if (!mine.length) return null;
    // A snoozed schedule still advertises the occurrence the snooze suppresses
    // (the daemon's NextFire ignores snoozeUntil by design), so that time is NOT
    // a promise we can keep — and the occurrence AFTER it needs cron parsing we
    // deliberately don't do client-side. Prefer a schedule we can state
    // truthfully; fall back to flagging the snooze rather than naming a time
    // that won't happen.
    const live = mine.filter((s) => !(s.snoozeUntil && new Date(s.snoozeUntil).getTime() > nowMs));
    const soonest = (live.length ? live : mine)
      .slice()
      .sort((a, b) => new Date(a.nextFire!).getTime() - new Date(b.nextFire!).getTime())[0];
    return { schedule: soonest, snoozed: live.length === 0 };
  }, [schedules, pane.id, nowMs]);

  // Tick only as fast as the label can actually change — no schedule, no timer.
  //
  // Two rules keep this from spinning:
  //   1. Only a LIVE countdown earns 1Hz. "snoozed" and "imminent" are static
  //      labels sitting at or past zero, so bucketing on the countdown alone
  //      would pin them at 1Hz forever (a snooze lasts hours). They tick slowly:
  //      what changes them is the snooze expiring or `schedules-changed` landing.
  //   2. The effect keys on the BUCKET, never the raw countdown — that moves on
  //      every tick, so an effect keyed on it would tear the timer down and
  //      rebuild it each time.
  const msToFire = next ? new Date(next.schedule.nextFire!).getTime() - nowMs : 0;
  const period = !next
    ? 0
    : next.snoozed || msToFire <= 0
      ? 30_000
      : msToFire < 90_000
        ? 1_000
        : msToFire < 3_600_000
          ? 15_000
          : 60_000;
  useEffect(() => {
    if (!period) return;
    const h = window.setInterval(() => setNowMs(Date.now()), period);
    return () => window.clearInterval(h);
  }, [period]);

  if (!next) return null;
  const { schedule, snoozed } = next;
  const when = snoozed ? 'snoozed' : formatNextFire(schedule.nextFire);
  const promptPreview = (schedule.prompt ?? '').trim().replace(/\s+/g, ' ').slice(0, 120);

  return (
    <span
      className={`pane-schedule-chip${snoozed ? ' is-snoozed' : ''}`}
      title={[
        snoozed
          ? `Scheduled prompt "${schedule.name}" — next run snoozed`
          : `Scheduled prompt "${schedule.name}" runs ${when} in this pane`,
        promptPreview && `\n${promptPreview}${(schedule.prompt ?? '').length > 120 ? '…' : ''}`,
      ]
        .filter(Boolean)
        .join('')}
      // The header is draggable and double-click zooms it; every badge in here
      // has to opt out of both (mirrors the auto/resume badges).
      onMouseDownCapture={(e) => e.stopPropagation()}
      onDoubleClick={(e) => e.stopPropagation()}
      onClick={(e) => e.stopPropagation()}
    >
      <span aria-hidden><IconClock size={12} style={{ verticalAlign: '-2px' }} /></span>
      {when}
    </span>
  );
}

interface PaneHeaderBadgesProps {
  pane: PaneInfo;
  paneIndex: number;
  // Focus panes default to showing the badge for both supported agents. Terminal
  // panes explicitly gate it with isAutoApproveCapable so plain shells omit it.
  showAutoMode?: boolean;
}

// The #index / auto-approval / resume badge cluster shared by the terminal
// (PaneHeader) and native (NativePane) headers so the two stay in lockstep.
// All three badges live inside a draggable header, so each escapes drag-start
// via stopPropagation + onMouseDownCapture (mirrors the mode toggle).
export function PaneHeaderBadges({ pane, paneIndex, showAutoMode = true }: PaneHeaderBadgesProps) {
  const toast = useStore((s) => s.toast);

  const [copiedResume, setCopiedResume] = useState(false);
  const paneAgent = effectivePaneAgent(pane) ?? (pane.codexThreadId ? 'codex' : pane.claudeSessionId ? 'claude' : null);
  const paneAgentLabel = agentLabel(paneAgent);
  const conversationID = paneAgent === 'codex'
    ? pane.codexThreadId
    : paneAgent === 'muse'
      ? pane.museSessionId
      : pane.claudeSessionId;
  const packBase = pane.contextPack?.replace(/\\/g, '/').split('/').filter(Boolean).pop() ?? pane.contextPack;
  const packStatus = pane.contextPackStatus ?? 'injected';
  const packTitle = pane.contextPack ? {
    injected: `Context pack ${packBase} — briefed at spawn. Right-click the pane to change it.`,
    cwd: `Context pack ${packBase} — already in scope via the working directory.`,
    unsupported: `Context pack ${packBase} attached but not applied — no adapter for this command (SOLU_CONTEXT_PACK is set).`,
    missing: `Context pack ${packBase} not found at spawn — pane started unbriefed.`,
  }[packStatus] : '';

  const copySessionId = async () => {
    const id = conversationID;
    if (!id) return;
    if (await copyToClipboard(id)) {
      setCopiedResume(true);
      window.setTimeout(() => setCopiedResume(false), 1200);
      toast('Session id copied', { kind: 'success', ttlMs: 1800 });
    } else {
      toast('Copy failed — clipboard unavailable', { kind: 'warn' });
    }
  };

  return (
    <>
      {paneIndex > 0 && (
        <span
          className="pane-index-badge"
          title={`Alt+${paneIndex} focuses this pane`}
        >#{paneIndex}</span>
      )}
      {/* Role identity — standing, not an alert: this pane IS the orchestrator.
          Deliberately NOT the ⚙ glyph (that means an in-session workflow, see
          StatusBar.tsx). Rendered in the shared cluster, so it shows in both the
          terminal (PaneHeader) and Focus (NativePane) headers. */}
      {pane.role === 'task_owner' && <span className="pane-role-badge" title="Interactive task owner — dispatches specialists for its assigned task">owner</span>}
      {pane.role === 'orchestrator' && (
        <span
          className="pane-role-badge"
          title={`Orchestrator pane (${agentLabel(paneAgent)}) — delegates work to tasks and agents. Right-click the pane to change its role.`}
          aria-label={`Orchestrator pane (${agentLabel(paneAgent)}) — delegates work to tasks and agents. Right-click the pane to change its role.`}
          onMouseDownCapture={(e) => e.stopPropagation()}
          onDoubleClick={(e) => e.stopPropagation()}
          onClick={(e) => e.stopPropagation()}
        >orch</span>
      )}
      {pane.contextPack && (
        <span
          className={`pane-pack-badge pane-pack-${packStatus}`}
          title={packTitle}
          aria-label={packTitle}
          onMouseDownCapture={(e) => e.stopPropagation()}
          onDoubleClick={(e) => e.stopPropagation()}
          onClick={(e) => e.stopPropagation()}
        >pack</span>
      )}
      <PaneCredentialBadge pane={pane} />
      <NextScheduleChip pane={pane} />
      {/* Delegated work runs through the team-only Tasks engine. */}


      <PaneAgentStaleDot pane={pane} />
      {/* Opens a three-option menu; never changes the mode on its own, and
          Always approve asks first (PaneAutoModeChip). */}
      {showAutoMode && <PaneAutoModeChip pane={pane} />}
      {/* AFTER the auto badge: the transient chip reads as a status hanging off
          the control it belongs to, and — because it mounts/unmounts mid-turn —
          appearing to the badge's RIGHT never shoves the badge sideways under
          the cursor mid-click. */}
      {showAutoMode && <AutoApprovingChip pane={pane} />}
      {conversationID && pane.resumeReady && (
        <button
          type="button"
          className={`pane-resume-badge${copiedResume ? ' copied' : ''}`}
          aria-label={`Copy ${paneAgentLabel} ${paneAgent === 'codex' ? 'thread' : 'session'} id ${conversationID}`}
          title={copiedResume
            ? 'Copied!'
            : `${paneAgentLabel} resume ready — click to copy ${paneAgent === 'codex' ? 'thread' : 'session'} id\n${conversationID}`}
          onMouseDownCapture={(e) => e.stopPropagation()}
          onDoubleClick={(e) => e.stopPropagation()}
          onClick={(e) => {
            e.stopPropagation();
            void copySessionId();
          }}
        >
          {copiedResume ? 'copied ✓' : 'resume'}
        </button>
      )}
    </>
  );
}
