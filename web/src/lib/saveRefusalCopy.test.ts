import { beforeEach, describe, expect, it, vi } from 'vitest';

const embed = vi.hoisted(() => ({ isEmbedded: vi.fn(() => true) }));
vi.mock('../embed', () => embed);

const proof = vi.hoisted(() => ({ localProofStatus: vi.fn((): { approvedDevices: number; device?: string } | null => ({ approvedDevices: 1 })) }));
vi.mock('./localProof', () => proof);

const scope = vi.hoisted(() => ({ prefix: '' }));
vi.mock('../api', async (orig) => {
  const actual = await orig<typeof import('../api')>();
  return { ...actual, currentRoutingScope: () => ({ ...actual.currentRoutingScope(), prefix: scope.prefix }) };
});

import { ApiError } from '../api';
import { saveFailureText } from './saveRefusalCopy';

const agentMessage = 'To edit a file, use the SoluTerminal app. Agents must use the MCP tool write_file_line.';
const refusal = (code: string, status = 403) => {
  const body = { caller: 'unverified', error: code, message: agentMessage };
  return new ApiError(status, JSON.stringify(body), body);
};

describe('saveFailureText', () => {
  beforeEach(() => {
    embed.isEmbedded.mockReturnValue(true);
    scope.prefix = '';
    proof.localProofStatus.mockReturnValue({ approvedDevices: 1 });
  });

  it.each(['human_required', 'device_required'])('gives a person in the frame plain advice for %s', (code) => {
    const text = saveFailureText(refusal(code));
    expect(text).toBe("This screen isn't approved to save files yet. Open Settings → Privacy & security → Ask to approve this browser.");
    expect(text).not.toMatch(/agent|MCP|\{/i);
  });

  it('says to approve a first browser at the computer when none is approved', () => {
    proof.localProofStatus.mockReturnValue({ approvedDevices: 0 });
    const text = saveFailureText(refusal('human_required'));
    expect(text).toContain('no browser is approved on this computer');
    expect(text).toContain('Approve this browser');
    expect(text).not.toMatch(/agent|MCP|\{/i);
  });

  it.each(['pane', 'worker'])('leaves the daemon message for a caller traced to a %s', (caller) => {
    const body = { caller, error: 'human_required', message: 'started from inside a pane' };
    expect(saveFailureText(new ApiError(403, JSON.stringify(body), body))).toBe('started from inside a pane');
  });

  it('sends a waiting screen to an approved device, not to the missing Ask button', () => {
    proof.localProofStatus.mockReturnValue({ approvedDevices: 1, device: 'pending' });
    const text = saveFailureText(refusal('human_required'));
    expect(text).toContain("Approve it from a browser or app you've already approved");
    expect(text).not.toContain('Ask to approve');
  });

  it('does not send a waiting screen to an approved device when none is approved', () => {
    proof.localProofStatus.mockReturnValue({ approvedDevices: 0, device: 'pending' });
    expect(saveFailureText(refusal('human_required'))).toContain('no browser is approved on this computer');
  });

  it('ignores a cached local standing while another machine is in view', () => {
    scope.prefix = '/m/other';
    proof.localProofStatus.mockReturnValue({ approvedDevices: 0 });
    const text = saveFailureText(refusal('human_required'));
    expect(text).not.toContain('no browser is approved on this computer');
    expect(text).toContain("Approve it from a browser or app you've already approved");
  });

  it('uses the Ask advice when the standing is unknown', () => {
    proof.localProofStatus.mockReturnValue(null);
    expect(saveFailureText(refusal('device_required'))).toContain('Ask to approve this browser');
  });

  it('shows the body message, not the JSON, outside the frame', () => {
    embed.isEmbedded.mockReturnValue(false);
    expect(saveFailureText(refusal('human_required'))).toBe(agentMessage);
  });

  it('shows the message of other refusals in the frame', () => {
    expect(saveFailureText(refusal('human_required', 409))).toBe(agentMessage);
  });

  it('falls back to the error text without a body message', () => {
    expect(saveFailureText(new ApiError(500, '500 Server Error'))).toBe('500 Server Error');
    expect(saveFailureText(new Error('boom'))).toBe('boom');
  });
});
