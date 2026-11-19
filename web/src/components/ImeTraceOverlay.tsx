import { useEffect, useState } from 'react';
import {
  clearImeTrace,
  getImeTraceEvents,
  isImeTraceEnabled,
  setImeTraceEnabled,
  subscribeImeTrace,
  type ImeTraceEvent,
} from '../imeTrace';

// Floating overlay that renders the IME event ring buffer (imeTrace.ts) so the
// mobile keyboard-gibberish trace can be captured + copied from a phone with NO
// DevTools. Mirror of ScrollDiagOverlay. Enable with ?imetrace=1 (sticky). Sits
// at the top so it never fights the soft keyboard at the bottom.
// See feedback_mobile_ime_composition / FOLLOWUPS-mobile-2026-06-19.md.
export function ImeTraceOverlay() {
  const [, force] = useState(0);
  const [copyState, setCopyState] = useState<'idle' | 'copied' | 'err'>('idle');

  useEffect(() => {
    const bump = () => force((n) => (n + 1) % 1_000_000);
    return subscribeImeTrace(bump);
  }, []);

  if (!isImeTraceEnabled()) return null;
  const events = getImeTraceEvents();
  const firstT = events[0]?.t ?? 0;

  const formatEvent = (ev: ImeTraceEvent): string => {
    const dt = (ev.t - firstT).toFixed(0);
    const parts: string[] = [`+${dt.padStart(6)}ms`, ev.kind.padEnd(11)];
    if (ev.data !== undefined) parts.push(`data=${JSON.stringify(ev.data)}`);
    if (ev.sent !== undefined) parts.push(`sent=${JSON.stringify(ev.sent)}`);
    if (ev.inputType !== undefined) parts.push(`type=${ev.inputType}`);
    if (ev.isComposing !== undefined) parts.push(`composing=${ev.isComposing ? 1 : 0}`);
    if (ev.taValue !== undefined) parts.push(`ta=${JSON.stringify(ev.taValue)}`);
    if (ev.anchor !== undefined) parts.push(`at=${ev.anchor}`);
    return parts.join(' ');
  };

  const copy = async () => {
    const ua = typeof navigator !== 'undefined' ? navigator.userAgent : '';
    const text = `# soluterminal IME trace — ${events.length} events\n# ${ua}\n` + events.map(formatEvent).join('\n');
    try {
      await navigator.clipboard.writeText(text);
      setCopyState('copied');
      setTimeout(() => setCopyState('idle'), 1200);
    } catch {
      // clipboard API can fail in non-HTTPS / unfocused contexts — fall back
      // to a hidden textarea + execCommand('copy').
      try {
        const ta = document.createElement('textarea');
        ta.value = text;
        ta.style.position = 'fixed';
        ta.style.opacity = '0';
        document.body.appendChild(ta);
        ta.select();
        document.execCommand('copy');
        document.body.removeChild(ta);
        setCopyState('copied');
        setTimeout(() => setCopyState('idle'), 1200);
      } catch {
        setCopyState('err');
        setTimeout(() => setCopyState('idle'), 1500);
      }
    }
  };

  const btn: React.CSSProperties = {
    background: 'transparent',
    border: '1px solid #3a4a60',
    color: '#d0e0ff',
    fontFamily: 'inherit',
    fontSize: 11,
    padding: '3px 9px',
    borderRadius: 3,
    cursor: 'pointer',
  };

  return (
    <div
      style={{
        position: 'fixed',
        top: 'env(safe-area-inset-top, 0px)',
        left: 0,
        right: 0,
        zIndex: 100000,
        maxHeight: '42vh',
        overflowY: 'auto',
        background: 'rgba(10, 14, 20, 0.96)',
        borderBottom: '1px solid #3a4a60',
        color: '#d0e0ff',
        fontFamily: 'SF Mono, Menlo, monospace',
        fontSize: 11,
        padding: 8,
        lineHeight: 1.4,
        userSelect: 'text',
        WebkitUserSelect: 'text',
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
        <strong style={{ flex: 1 }}>IME trace</strong>
        <span style={{ color: '#8aa0c0', fontSize: 10 }}>{events.length}</span>
        <button type="button" style={btn} onClick={copy} title="Copy the whole trace">
          {copyState === 'copied' ? '✓ copied' : copyState === 'err' ? '✗ err' : 'copy'}
        </button>
        <button type="button" style={btn} onClick={clearImeTrace} title="Clear the trace">
          clear
        </button>
        <button type="button" style={btn} onClick={() => setImeTraceEnabled(false)} title="Disable IME trace">
          ×
        </button>
      </div>
      {events.length === 0 && (
        <div style={{ color: '#8aa0c0' }}>No events yet — tap a Claude pane and type with the keyboard.</div>
      )}
      {events.map((ev, i) => (
        <div key={i} style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
          {formatEvent(ev)}
        </div>
      ))}
    </div>
  );
}
