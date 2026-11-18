import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import '../../loadExtensions';
import { slotList } from '../../extensions';
import { settingsControls } from '../../settingsRegistry';
import { MARKETING_CONSENT_LABEL } from '../../marketingConsent';
import { AGENT_AUTHORITY_LABEL } from './AgentAuthoritySetting';
import { AGENT_INTEGRATION_LABEL } from './AgentIntegrationSetting';
import { CHECKIN_LABEL, CRASH_LABEL, USAGE_LABEL } from './CheckinSetting';
import { DEVELOPER_OVERRIDES_LABEL } from './DeveloperOverridesSetting';
import { HUMAN_PROOF_LABEL } from './HumanProofSetting';
import { PERSONAL_VIEW_LABEL } from './PersonalViewSetting';
import { PRESENTATION_MODE_LABEL } from './PresentationModeSetting';
import { PRIVATE_RELAY_LABEL, REMOTE_POLICY_LABEL } from './RemoteDevicesSetting';
import {
  VOICE_API_KEY_LABEL,
  VOICE_AUTO_SEND_LABEL,
  VOICE_KEEP_MIC_LABEL,
  VOICE_LANGUAGE_LABEL,
  VOICE_LOCAL_LABEL,
  VOICE_READY_TICK_LABEL,
} from './VoiceCleanupSetting';
import { VOICE_WORDS_LABEL } from './VoiceWordsSetting';
import { WORKER_MODEL_LABEL } from './WorkerModelSetting';

// ST-281: search can only find what the registry lists, so every row a
// Settings tab can show must be in it. Rows are read from the source (not a
// render), so rows that appear only with data, an account or team features
// are covered too.

const SETTINGS_DIR = join(process.cwd(), 'src', 'components', 'settings');

// Rows that repeat per item and aren't settings of their own: the Aria tab's
// per-agent model, permission and Remove.
const PER_ITEM = new Set(['Remove', 'Permission']);

// A label passed as a literal string, '…' or "…" in braces.
const LITERAL = /<Settings(?:Row|Check)\b[^>]*?\blabel=(?:"([^"]+)"|\{'([^']+)'\}|\{"([^"]+)"\})/gs;

function literalRowLabels(): Map<string, string> {
  const found = new Map<string, string>();
  for (const name of readdirSync(SETTINGS_DIR)) {
    if (!name.endsWith('.tsx') || name.endsWith('.test.tsx')) continue;
    // The Aria tab's rows are registered by its feature module; check them
    // only when that module is present.
    if (name === 'AgentTab.tsx' && !slotList('settingsTabs').some((tab) => tab.key === 'agent')) continue;
    const source = readFileSync(join(SETTINGS_DIR, name), 'utf8');
    for (const match of source.matchAll(LITERAL)) {
      const label = match[1] ?? match[2] ?? match[3];
      if (label && !PER_ITEM.has(label)) found.set(label, name);
    }
  }
  return found;
}

describe('settings search coverage (ST-281)', () => {
  const registered = new Set(settingsControls().map(({ label }) => label));

  it('registers every literal row label in the Settings tabs', () => {
    const labels = literalRowLabels();
    expect(labels.size).toBeGreaterThan(20);
    const missing = [...labels].filter(([label]) => !registered.has(label)).map(([label, file]) => `${label} (${file})`);
    expect(missing).toEqual([]);
  });

  it('registers every row whose label is a shared constant', () => {
    const constants = [
      MARKETING_CONSENT_LABEL, AGENT_AUTHORITY_LABEL, AGENT_INTEGRATION_LABEL, CHECKIN_LABEL, CRASH_LABEL,
      USAGE_LABEL, DEVELOPER_OVERRIDES_LABEL, HUMAN_PROOF_LABEL, PERSONAL_VIEW_LABEL, PRESENTATION_MODE_LABEL,
      PRIVATE_RELAY_LABEL, REMOTE_POLICY_LABEL, VOICE_API_KEY_LABEL, VOICE_AUTO_SEND_LABEL, VOICE_KEEP_MIC_LABEL,
      VOICE_LANGUAGE_LABEL, VOICE_LOCAL_LABEL, VOICE_READY_TICK_LABEL, VOICE_WORDS_LABEL, WORKER_MODEL_LABEL,
    ];
    expect(constants.filter((label) => !registered.has(label))).toEqual([]);
  });

  it('notices a row label the registry lacks', () => {
    // The pattern really reads labels: a sample row is found, and an
    // unregistered label would be reported.
    const sample = '<SettingsRow label="A brand new setting" hint="x">';
    const labels = [...sample.matchAll(LITERAL)].map((m) => m[1]);
    expect(labels).toEqual(['A brand new setting']);
    expect(registered.has('A brand new setting')).toBe(false);
  });
});
