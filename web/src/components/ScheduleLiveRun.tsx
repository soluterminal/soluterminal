import { useEffect, useRef, useState } from 'react';
import { api } from '../api';
import { useStore } from '../store';
import type { Schedule, ScheduleLive } from '../types';
import { fmtDuration } from '../lib/runCauses';

// The run in flight of an automation, live, with Stop (Automations stage 2,
// ST-299 item 12). Shown above its report or its history while the
// automation runs, or while its prompt waits for its pane. A task's text is
// what the assistant has written so far; a tool's output never shows here.

const POLL_MS = 1500;

export function ScheduleLiveRun({ schedule }: { schedule: Schedule }) {
  const toast = useStore((s) => s.toast);
  const active = !!schedule.agentRunning || !!schedule.waitingForPane;
  const [live, setLive] = useState<ScheduleLive | null>(null);
  const [stopping, setStopping] = useState(false);
  const [now, setNow] = useState(Date.now());
  const textRef = useRef<HTMLPreElement | null>(null);

  useEffect(() => {
    if (!active) {
      setLive(null);
      setStopping(false);
      return;
    }
    let gone = false;
    const read = async () => {
      try {
        const v = await api.scheduleLive(schedule.id);
        if (!gone) setLive(v);
      } catch {
        /* the next poll tries again */
      }
      if (!gone) setNow(Date.now());
    };
    void read();
    const h = setInterval(() => void read(), POLL_MS);
    return () => {
      gone = true;
      clearInterval(h);
    };
  }, [schedule.id, active]);

  // Keep the newest text in view, as a terminal does.
  useEffect(() => {
    const el = textRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [live?.text]);

  if (!active) return null;

  async function stop() {
    setStopping(true);
    try {
      // The run on screen, by id: if it ended and another began meanwhile,
      // the other is not stopped.
      await api.stopSchedule(schedule.id, live?.running ? { runId: live.runId } : { waitId: live?.waitId });
      toast(`Stopped "${schedule.name}"`, { kind: 'success' });
    } catch (e) {
      setStopping(false);
      toast(`Couldn't stop it: ${(e as Error).message || 'unknown error'}`, { kind: 'warn', icon: '⚠' });
    }
  }

  const running = live?.running || schedule.agentRunning;
  const since = live?.startedAt ? fmtDuration(now - new Date(live.startedAt).getTime()) : '';
  const head = running
    ? `Running now${since ? ` · ${since}` : ''}${live?.model ? ` · ${live.model}` : ''}`
    : `Waiting: ${live?.waiting || schedule.waitingReason || 'for its pane'}`;
  const isTask = schedule.kind === 'task';

  return (
    <div className="sched-live" role="region" aria-label="Run in progress" style={{ border: '1px solid var(--border, #333)', borderRadius: 6, padding: '8px 10px', margin: '8px 0' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 12 }}>
        <span style={{ flex: 1, fontWeight: 600 }}>{head}</span>
        <button
          className="reader-btn"
          disabled={stopping}
          onClick={() => void stop()}
          title={running ? 'Stop this run now' : 'Stop waiting: the prompt is not sent'}
        >{stopping ? 'Stopping…' : 'Stop'}</button>
      </div>
      {running && isTask && (
        <pre
          ref={textRef}
          style={{
            fontSize: 11,
            fontFamily: "'SF Mono','Menlo',monospace",
            background: 'var(--bg-dim, rgba(127,127,127,0.08))',
            padding: 8,
            marginTop: 6,
            maxHeight: 220,
            overflow: 'auto',
            whiteSpace: 'pre-wrap',
            wordBreak: 'break-word',
          }}
        >{live?.clipped ? '…\n' : ''}{live?.text || 'Waiting for its first words…'}</pre>
      )}
    </div>
  );
}
