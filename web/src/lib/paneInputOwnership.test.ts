import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  CLAIM_ACK_TIMEOUT_MS,
  isKeypressInput,
  INPUT_HOLD_MS,
  createPaneInputOwnership,
  type PaneClaimReason,
} from './paneInputOwnership';

// A fake pane socket: records claim frames and delivered input, lets the test
// flip visibility, and uses vitest's fake clock for the watchdogs.
function harness(opts: { visible?: boolean; sendInput?: () => boolean } = {}) {
  vi.useFakeTimers();
  let visible = opts.visible ?? true;
  const claims: PaneClaimReason[] = [];
  const delivered: string[] = [];
  const logs: Array<{ name: string; detail: Record<string, unknown> }> = [];
  const warns: unknown[] = [];
  const banners: unknown[] = [];
  const o = createPaneInputOwnership({
    paneId: 'p1',
    sendClaim: (reason) => { claims.push(reason); return true; },
    sendInput: (bytes) => { if (!visible || opts.sendInput?.() === false) return false; delivered.push(new TextDecoder().decode(bytes)); return true; },
    visible: () => visible,
    onInputUnclaimed: (owner) => { banners.push(owner); },
    log: (name, detail) => { logs.push({ name, detail }); },
    warn: (message, detail) => { warns.push({ message, detail }); },
    setTimer: (fn, ms) => setTimeout(fn, ms),
    clearTimer: (h) => clearTimeout(h as ReturnType<typeof setTimeout>),
  });
  const type = (s: string, user = true) => o.holdInput(new TextEncoder().encode(s), user);
  return {
    o, claims, delivered, logs, warns, banners, type,
    setVisible: (v: boolean) => { visible = v; },
    names: () => logs.map((l) => l.name),
  };
}

describe('paneInputOwnership', () => {
  afterEach(() => { vi.useRealTimers(); });

  it('a fresh (re)connected socket is unconfirmed until the daemon says owned on it', () => {
    const h = harness();
    expect(h.o.confirmed).toBe(false);
    h.o.onOwnership(false, 'none');
    expect(h.o.confirmed).toBe(false);
    h.o.onOwnership(true, 'you');
    expect(h.o.confirmed).toBe(true);
  });

  it('a reconnect claim is sent and its unanswered claim is retried after the watchdog', () => {
    const h = harness();
    expect(h.o.claim('adopt-unowned')).toBe('sent');
    expect(h.claims).toEqual(['adopt-unowned']);
    expect(h.o.claimPending).toBe(true);
    // The daemon's subscribe-time frame says nobody owns it; still pending.
    h.o.onOwnership(false, 'none');
    vi.advanceTimersByTime(CLAIM_ACK_TIMEOUT_MS);
    expect(h.names()).toContain('pane.ownership.claim-timeout');
    expect(h.claims).toEqual(['adopt-unowned', 'adopt-unowned']);
    h.o.onOwnership(true, 'you');
    expect(h.o.claimPending).toBe(false);
    vi.advanceTimersByTime(CLAIM_ACK_TIMEOUT_MS * 3);
    expect(h.claims).toHaveLength(2);
  });

  it('retries are bounded when the daemon never answers', () => {
    const h = harness();
    h.o.claim('reconnect');
    vi.advanceTimersByTime(CLAIM_ACK_TIMEOUT_MS * 10);
    expect(h.claims).toEqual(['reconnect', 'reconnect', 'reconnect']);
    expect(h.o.claimPending).toBe(false);
  });

  it('a claim requested while hidden is kept, not dropped, and sent when the pane is shown', () => {
    const h = harness({ visible: false });
    expect(h.o.claim('adopt-unowned')).toBe('deferred');
    expect(h.claims).toEqual([]);
    h.o.onVisible(); // still hidden: nothing
    expect(h.claims).toEqual([]);
    h.setVisible(true);
    h.o.onVisible();
    expect(h.claims).toEqual(['adopt-unowned']);
    h.o.onVisible();
    expect(h.claims).toHaveLength(1);
    expect(h.names()).toEqual(expect.arrayContaining(['pane.ownership.claim-deferred', 'pane.ownership.claim-resumed']));
  });

  it('a claim kept while hidden resumes as the conditional adopt-unowned, never a late takeover', () => {
    const h = harness({ visible: false });
    h.o.claim('takeover');
    h.o.claim('reconnect');
    h.setVisible(true);
    h.o.onVisible();
    expect(h.claims).toEqual(['adopt-unowned']);
  });

  it('typed bytes before ownership claim, are held, and are delivered once owned', () => {
    const h = harness();
    expect(h.type('l')).toBe(true);
    expect(h.type('s')).toBe(true);
    expect(h.claims).toEqual(['adopt-unowned']);
    expect(h.delivered).toEqual([]);
    vi.advanceTimersByTime(INPUT_HOLD_MS - 100);
    h.o.onOwnership(true, 'you');
    expect(h.delivered).toEqual(['l', 's']);
    expect(h.names()).toContain('pane.input.reclaimed');
    vi.advanceTimersByTime(INPUT_HOLD_MS * 2);
    expect(h.banners).toEqual([]);
    expect(h.warns).toEqual([]);
  });

  it('held typing that is still unowned after the hold window shows the banner and logs, never silently', () => {
    const h = harness();
    h.type('x');
    vi.advanceTimersByTime(INPUT_HOLD_MS);
    expect(h.delivered).toEqual([]);
    expect(h.banners).toEqual([undefined]);
    expect(h.warns).toHaveLength(1);
    const dropped = h.logs.find((l) => l.name === 'pane.input.dropped');
    expect(dropped?.detail).toMatchObject({ paneId: 'p1', reason: 'claim-pending', bytes: 1 });
    // A late grant must not replay what was already reported dropped.
    h.o.onOwnership(true, 'you');
    expect(h.delivered).toEqual([]);
  });

  it('holds typing through a slow grant within the entire acknowledgement window', () => {
    const h = harness();
    h.o.claim('takeover');
    h.type('slow');
    expect(INPUT_HOLD_MS).toBeGreaterThanOrEqual(CLAIM_ACK_TIMEOUT_MS);
    vi.advanceTimersByTime(CLAIM_ACK_TIMEOUT_MS - 1);
    expect(h.banners).toEqual([]);
    h.o.onOwnership(true, 'you');
    expect(h.delivered).toEqual(['slow']);
    expect(h.warns).toEqual([]);
  });

  it('caps held bytes at 64 KiB and never extends the first byte deadline for more typing or retries', () => {
    const h = harness();
    h.o.claim('takeover');
    expect(h.type('x'.repeat(64 * 1024 - 1))).toBe(true);
    vi.advanceTimersByTime(INPUT_HOLD_MS - 1);
    expect(h.type('y')).toBe(true);
    expect(h.type('overflow')).toBe(false);
    vi.advanceTimersByTime(1);
    expect(h.claims).toEqual(['takeover', 'takeover']);
    expect(h.banners).toEqual([undefined]);
    expect(h.logs.filter(l => l.name === 'pane.input.dropped').map(l => l.detail)).toEqual([
      expect.objectContaining({ reason: 'hold-overflow', bytes: 8 }),
      expect.objectContaining({ reason: 'claim-pending', bytes: 64 * 1024 }),
    ]);
    h.o.onOwnership(true, 'you');
    expect(h.delivered).toEqual([]);
  });

  it('a local hold expiry reports unclaimed input without cancelling the pending takeover or retries', () => {
    const h = harness();
    h.o.claim('takeover');
    h.type('x');
    vi.advanceTimersByTime(INPUT_HOLD_MS);
    expect(h.o.claimPending).toBe(true);
    expect(h.claims).toEqual(['takeover', 'takeover']);
    expect(h.banners).toEqual([undefined]);
    h.o.onOwnership(true, 'you');
    expect(h.o.confirmed).toBe(true);
  });

  it('reports refused delivery without claiming the bytes were reclaimed', () => {
    const h = harness({ sendInput: () => false });
    h.type('blocked');
    h.o.onOwnership(true, 'you');
    expect(h.delivered).toEqual([]);
    expect(h.logs.find(l => l.name === 'pane.input.dropped')?.detail).toMatchObject({ reason: 'delivery-refused', bytes: 7 });
    expect(h.names()).not.toContain('pane.input.reclaimed');
    expect(h.banners).toEqual([]);
  });

  it('a denial (owner other) reports held typing and shows the banner at once', () => {
    const h = harness();
    h.type('y');
    h.o.onOwnership(false, 'other');
    expect(h.banners).toEqual(['other']);
    expect(h.delivered).toEqual([]);
    // Further typing with a peer holding input: no new claim, banner again.
    h.type('z');
    expect(h.claims).toEqual(['adopt-unowned']);
    expect(h.banners).toEqual(['other', 'other']);
  });

  it('terminal replies and programmatic nudges never claim or raise the banner', () => {
    const h = harness();
    expect(h.type('\x1b[?1;2c')).toBe(false);
    expect(h.type('\x1b[I')).toBe(false);
    expect(h.type('\x0c', false)).toBe(false);
    vi.advanceTimersByTime(INPUT_HOLD_MS * 2);
    expect(h.claims).toEqual([]);
    expect(h.banners).toEqual([]);
    expect(h.warns).toEqual([]);
  });

  it('a programmatic nudge sent while a claim is in flight is held and delivered in order', () => {
    const h = harness();
    h.o.claim('adopt-unowned');
    expect(h.type('\x0c', false)).toBe(true);
    h.type('a');
    h.o.onOwnership(true, 'you');
    expect(h.delivered).toEqual(['\x0c', 'a']);
  });

  it('never queues emulator replies during a pending claim, even alongside held typing', () => {
    const h = harness();
    h.o.claim('takeover');
    for (const user of [true, false]) {
      expect(h.type('\x1b[12;40R', user)).toBe(false);
      expect(h.type('\x1b[?1;2c', user)).toBe(false);
      h.type('a');
    }
    h.o.onOwnership(true, 'you');
    expect(h.delivered).toEqual(['a', 'a']);
    expect(h.claims).toEqual(['takeover']);
    expect(h.warns).toEqual([]);
  });

  it('keystroke claims are throttled while one is pending', () => {
    const h = harness();
    for (const ch of 'hello') h.type(ch);
    expect(h.claims).toEqual(['adopt-unowned']);
  });

  it('close drops held input and stops claiming', () => {
    const h = harness();
    h.type('q');
    h.o.close();
    vi.advanceTimersByTime(CLAIM_ACK_TIMEOUT_MS * 3);
    expect(h.claims).toEqual(['adopt-unowned']);
    expect(h.banners).toEqual([]);
    expect(h.logs.find((l) => l.name === 'pane.input.dropped')?.detail).toMatchObject({ reason: 'closing' });
    expect(h.o.claim('takeover')).toBe('failed');
  });

  it('arrows, Alt combos and bracketed pastes count as typing; emulator replies do not', () => {
    const enc = (x: string) => new TextEncoder().encode(x);
    for (const key of ['a', '\x1b', '\x1b[A', '\x1bOA', '\x1bx', '\x1b[200~ls\x1b[201~', '\x1b[3~', '\x1b[1;5C']) {
      expect(isKeypressInput(enc(key)), JSON.stringify(key)).toBe(true);
    }
    for (const reply of ['\x1b[?1;2c', '\x1b[>0;276;0c', '\x1b[12;40R', '\x1b[I', '\x1b[O', '\x1b[<0;3;4M', '\x1b]11;rgb:0/0/0\x07', '\x1b[?2004;1$y']) {
      expect(isKeypressInput(enc(reply)), JSON.stringify(reply)).toBe(false);
    }
  });

  it('a bracketed paste typed before ownership claims and is delivered once owned', () => {
    const h = harness();
    expect(h.type('\x1b[200~echo pasted\x1b[201~')).toBe(true);
    expect(h.claims).toEqual(['adopt-unowned']);
    h.o.onOwnership(true, 'you');
    expect(h.delivered).toEqual(['\x1b[200~echo pasted\x1b[201~']);
  });

  it('typing held for a claim is reported, not swallowed, when the pane is hidden before the grant', () => {
    const h = harness();
    h.type('secret-ish');
    h.setVisible(false);
    h.o.onOwnership(true, 'you');
    expect(h.delivered).toEqual([]);
    expect(h.warns).toHaveLength(1);
    expect(h.logs.find((l) => l.name === 'pane.input.dropped')?.detail).toMatchObject({ reason: 'hidden-before-grant', bytes: 10 });
    expect(h.names()).not.toContain('pane.input.reclaimed');
    // Nothing is replayed when the pane is shown again.
    h.setVisible(true);
    h.o.onVisible();
    expect(h.delivered).toEqual([]);
  });
});
