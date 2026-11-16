import { randomUUID } from 'node:crypto';
import fs from 'node:fs/promises';
import path from 'node:path';

export const DEEPSEEK_FLASH_MODEL = 'deepseek-flash';
export const DEEPSEEK_VISION_MODEL = 'deepseek-v4-flash-vision-exp';

const LEGACY_NON_VISION = new Set([
  'deepseek-v4-pro',
  'deepseek-v4-flash',
  'deepseek-chat',
  'deepseek-reasoner',
]);

// Side-effect-free mirror of shared/providers.CanonicalDeepSeekDispatchModel.
// It exists at the launcher boundary because extracted harness assets run
// independently of the Go daemon and must fail before spawning a child.
export function canonicalDeepseekModel(raw) {
  const trimmed = String(raw ?? '').trim();
  if (!trimmed) return DEEPSEEK_FLASH_MODEL;
  const parts = trimmed.split('/');
  if (parts.some(part => !part.trim())) throw new Error(`Unsupported DeepSeek model: ${trimmed}`);
  const component = parts.at(-1).toLowerCase();
  if (component === DEEPSEEK_FLASH_MODEL) return DEEPSEEK_FLASH_MODEL;
  if (component === DEEPSEEK_VISION_MODEL) return DEEPSEEK_VISION_MODEL;
  if (LEGACY_NON_VISION.has(component)) return DEEPSEEK_FLASH_MODEL;
  throw new Error(`Unsupported DeepSeek model: ${trimmed}`);
}

// DeepSeek Harness 0.2.2 dropped the vision-exp model; its deepseek-flash
// (V4.1-Flash) takes images itself. A saved vision-exp selection therefore
// runs as deepseek-flash under the harness, as it does under Pi.
export function dshModel(raw) {
  const model = canonicalDeepseekModel(raw);
  return model === DEEPSEEK_VISION_MODEL ? DEEPSEEK_FLASH_MODEL : model;
}

export function validateDeepseekSelector(raw) {
  if (raw !== DEEPSEEK_FLASH_MODEL && raw !== DEEPSEEK_VISION_MODEL) {
    throw new Error(`Unsupported DeepSeek model: ${String(raw ?? '')}`);
  }
  return raw;
}

// DeepSeek Harness 0.2.2 adds two fields to every model request, sent to
// whatever base URL is configured: the whole session log (dsh_session_log,
// default on since 0.2.2) and the active plugin list (dsh_plugin_packages).
// A managed launch turns both off; the Go workflow runtime does the same
// (wfDeepseekHarnessPrivacyRows).
export const DSH_PRIVACY_ROWS = [
  { id: 'session-log-deepseek', config: { enabled: false } },
  { id: 'plugin-package-inventory-deepseek', config: { enabled: false } },
];

// The pane client's patch: the ACP row's model, plus the privacy rows.
export function dshPatchEntries(model) {
  return [{ id: 'acp', config: { provider: 'deepseek-official', model } }, ...DSH_PRIVACY_ROWS];
}

// dsh reads its anonymous id from <DSH_HOME>/.anonymous-user-id once per
// process. Replacing it before each launch makes it a per-launch id.
export async function rotateDshAnonymousId(dshHome) {
  const file = path.join(dshHome, '.anonymous-user-id');
  const temp = `${file}.${process.pid}.tmp`;
  await fs.writeFile(temp, `${randomUUID()}\n`, { mode: 0o600 });
  await fs.rename(temp, file);
}

// DeepSeek Harness 0.2.2's ACP model option carries each choice as a
// JSON-encoded [provider, model] pair, e.g. '["deepseek-official","deepseek-flash"]';
// 0.1.5 sent the bare model id. Only the official DeepSeek provider is ours: a
// pair naming any other provider (dsh also mounts a pi-ai adapter) is refused.
export const DSH_OFFICIAL_PROVIDER = 'deepseek-official';
export function decodeDshModelValue(value) {
  const text = String(value ?? '');
  if (!text.startsWith('[')) return { model: text, pair: false };
  let parsed;
  try { parsed = JSON.parse(text); } catch { throw new Error(`Unsupported DeepSeek model: ${text}`); }
  if (!Array.isArray(parsed) || parsed.length !== 2 || parsed.some(part => typeof part !== 'string') || parsed[0] !== DSH_OFFICIAL_PROVIDER) {
    throw new Error(`Unsupported DeepSeek model: ${text}`);
  }
  return { model: parsed[1], pair: true };
}
export function encodeDshModelValue(model, pair) {
  return pair ? JSON.stringify([DSH_OFFICIAL_PROVIDER, model]) : model;
}
