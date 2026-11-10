import { afterEach, describe, expect, it } from 'vitest';
import { api, configureApi } from './api';

afterEach(() => {
  configureApi({ prefix: '', token: null });
});

describe('api.wsUrl', () => {
  it('adds exact-page retirement identity only when explicitly supplied', () => {
    expect(new URL(api.wsUrl('owner', 80, 24, { retirementViewerId: 'page-instance' })).searchParams.get('retirement_viewer')).toBe('page-instance');
    expect(new URL(api.wsUrl('ordinary', 80, 24)).searchParams.has('retirement_viewer')).toBe(false);
  });
  it('marks browser pane websockets as web clients while preserving mobile sizing', () => {
    configureApi({ prefix: '/m/machine-1', token: 'header.payload.sig' });

    const url = api.wsUrl('pane_1', 50, 20, {
      skipReplay: true,
      since: 123,
      epoch: 456,
      mobile: true,
    });

    expect(url).toContain('/m/machine-1/api/panes/pane_1/io?');
    expect(url).toContain('cols=50');
    expect(url).toContain('rows=20');
    expect(url).toContain('skip_replay=1');
    expect(url).toContain('client=web');
    expect(url).toContain('mobile=1');
    expect(url).toContain('since=123');
    expect(url).toContain('epoch=456');
    // The JWT must never ride the URL (proxy/access-log leak); it rides
    // the Sec-WebSocket-Protocol handshake via api.wsProtocols instead.
    expect(url).not.toContain('access_token');
  });
});

describe('api.wsUrl terminal size', () => {
  it('sends the terminal size alongside the seeded cols/rows', () => {
    const url = new URL(api.wsUrl('pane_1', 120, 30, { termCols: 130, termRows: 39 }));
    expect(url.searchParams.get('cols')).toBe('120');
    expect(url.searchParams.get('rows')).toBe('30');
    expect(url.searchParams.get('term_cols')).toBe('130');
    expect(url.searchParams.get('term_rows')).toBe('39');
  });
  it('omits the terminal size when it is unknown', () => {
    const url = new URL(api.wsUrl('pane_1', 120, 30, { termCols: 0, termRows: 0, replayWidth: true }));
    expect(url.searchParams.has('term_cols')).toBe(false);
    expect(url.searchParams.has('term_rows')).toBe(false);
    expect(url.searchParams.has('replay_width')).toBe(false);
  });
  it('says the replay is written at its own width only when asked', () => {
    const url = new URL(api.wsUrl('pane_1', 220, 30, { termCols: 220, termRows: 39, replayWidth: true }));
    expect(url.searchParams.get('replay_width')).toBe('1');
    const old = new URL(api.wsUrl('pane_1', 220, 30, { termCols: 220, termRows: 39 }));
    expect(old.searchParams.has('replay_width')).toBe(false);
  });
});

describe('api.wsProtocols', () => {
  it('offers solu-auth plus the bearer.<jwt> entry in cloud mode', () => {
    configureApi({ prefix: '/m/machine-1', token: 'header.payload.sig' });

    expect(api.wsProtocols()).toEqual(['solu-auth', 'bearer.header.payload.sig']);
  });

  it('returns undefined (not []) in local mode so no negotiation is requested', () => {
    configureApi({ prefix: '', token: null });

    expect(api.wsProtocols()).toBeUndefined();
  });
});
