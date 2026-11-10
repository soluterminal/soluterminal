import { beforeEach, expect, it, vi } from 'vitest';

// Cloud mode: the browser posts voice straight to the cloud, so the
// computer's "Only devices I approve" policy decides whether `context` goes.
const routing = vi.hoisted(() => ({ prefix: '/m/pc', backend: '' }));
const apiFetch = vi.hoisted(() => vi.fn());
const rootFetch = vi.hoisted(() => vi.fn());
vi.mock('../api', () => ({
  apiFetch,
  rootFetch,
  currentPrefix: () => routing.prefix,
  currentApiPrefix: () => routing.prefix + routing.backend,
  j: (r: Response) => r.json(),
}));

import voiceApi from './voice.api';

const CONTEXT = 'npm run dev\n$ ';
const ok = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
const settle = async () => {
  for (let n = 0; n < 10; n++) await Promise.resolve();
};

beforeEach(() => {
  routing.prefix = '/m/pc';
  routing.backend = '';
  apiFetch.mockReset();
  rootFetch.mockReset().mockImplementation(async () => ok({ text: 'hello', cleanup: 'ok' }));
});

async function dictate(check: ReturnType<typeof voiceApi.startVoiceContextCheck>) {
  await voiceApi.transcribe(new Blob(['x']), { filename: 'audio.wav', context: CONTEXT, contextCheck: check });
  await voiceApi.cleanupVoice('hello', { context: CONTEXT, contextCheck: check });
  const [transcribe, cleanup] = rootFetch.mock.calls;
  expect(transcribe[0]).toBe('/api/transcribe');
  expect(cleanup[0]).toBe('/api/voice/cleanup');
  return {
    transcribed: (transcribe[1].body as FormData).get('context'),
    cleaned: JSON.parse(cleanup[1].body as string) as Record<string, unknown>,
  };
}

it('reads the policy from the computer when recording starts', () => {
  apiFetch.mockReturnValue(new Promise(() => {}));
  voiceApi.startVoiceContextCheck();
  expect(apiFetch).toHaveBeenCalledWith('/api/remote-devices', undefined, { signRead: true });
});

it('leaves context out of both requests when only approved devices are allowed', async () => {
  apiFetch.mockResolvedValue(ok({ policy: 'approved', devices: [] }));
  const check = voiceApi.startVoiceContextCheck();
  await settle();
  const sent = await dictate(check);
  expect(sent.transcribed).toBeNull();
  expect(sent.cleaned).not.toHaveProperty('context');
  expect(sent.cleaned).toMatchObject({ text: 'hello' });
});

it('still sends context when any device may connect', async () => {
  apiFetch.mockResolvedValue(ok({ policy: 'any', devices: [] }));
  const check = voiceApi.startVoiceContextCheck();
  await settle();
  const sent = await dictate(check);
  expect(sent.transcribed).toBe(CONTEXT);
  expect(sent.cleaned.context).toBe(CONTEXT);
});

it.each([
  ['an older computer without the endpoint (404)', () => Promise.resolve(ok('not found', 404))],
  ['a device the computer has not approved (401)', () => Promise.resolve(ok({ error: 'device_not_approved' }, 401))],
  ['an unreachable computer', () => Promise.reject(new TypeError('Failed to fetch'))],
  ['an unexpected answer', () => Promise.resolve(ok({ policy: '' }))],
])('leaves context out for %s', async (_name, answer) => {
  apiFetch.mockImplementation(answer);
  const check = voiceApi.startVoiceContextCheck();
  await settle();
  const sent = await dictate(check);
  expect(sent.transcribed).toBeNull();
  expect(sent.cleaned).not.toHaveProperty('context');
});

it('leaves context out when the policy has not answered by upload time', async () => {
  apiFetch.mockReturnValue(new Promise(() => {}));
  const check = voiceApi.startVoiceContextCheck();
  await settle();
  const sent = await dictate(check);
  expect(sent.transcribed).toBeNull();
  expect(sent.cleaned).not.toHaveProperty('context');
});

it('is decided by the answer given when recording started', async () => {
  // The start read answered "any"; a later change is not read again.
  apiFetch.mockResolvedValueOnce(ok({ policy: 'any' })).mockResolvedValue(ok({ policy: 'approved' }));
  const allowed = voiceApi.startVoiceContextCheck();
  await settle();
  expect((await dictate(allowed)).transcribed).toBe(CONTEXT);
  expect(apiFetch).toHaveBeenCalledTimes(1);

  // The start read failed; the policy opening up later does not let context go.
  rootFetch.mockClear();
  apiFetch.mockReset().mockRejectedValueOnce(new TypeError('Failed to fetch')).mockResolvedValue(ok({ policy: 'any' }));
  const failed = voiceApi.startVoiceContextCheck();
  await settle();
  const sent = await dictate(failed);
  expect(sent.transcribed).toBeNull();
  expect(sent.cleaned).not.toHaveProperty('context');
  expect(apiFetch).toHaveBeenCalledTimes(1);
});

it('leaves context out when the backend changed after recording started', async () => {
  apiFetch.mockResolvedValue(ok({ policy: 'any' }));
  const check = voiceApi.startVoiceContextCheck();
  await settle();
  routing.backend = '/b/windows';
  expect((await dictate(check)).transcribed).toBeNull();
});

it('leaves context out in cloud mode when no check was made', async () => {
  await voiceApi.cleanupVoice('hello', { context: CONTEXT });
  expect(JSON.parse(rootFetch.mock.calls[0][1].body as string)).not.toHaveProperty('context');
});

it('sends context as before in local mode, where the computer filters it itself', async () => {
  routing.prefix = '';
  const check = voiceApi.startVoiceContextCheck();
  expect(apiFetch).not.toHaveBeenCalled();
  const sent = await dictate(check);
  expect(sent.transcribed).toBe(CONTEXT);
  expect(sent.cleaned.context).toBe(CONTEXT);
});
