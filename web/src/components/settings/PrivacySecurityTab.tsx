import { AgentAuthoritySetting } from './AgentAuthoritySetting';
import { RemoteDevicesSetting } from './RemoteDevicesSetting';
import { AlertsHandlerSetting } from './AlertsHandlerSetting';
import { HumanProofSetting } from './HumanProofSetting';
import { AgentIntegrationSetting } from './AgentIntegrationSetting';
import { CheckinSetting } from './CheckinSetting';

// Settings › Privacy & security: the switches that most change what can act on
// this computer come first — agents in panes approving prompts, which browsers
// and apps here may act for you, then which remote devices may use it (and end-to-end encryption) and which app, if
// any, delivers Code alerts instead — followed by the Claude Code and Codex
// integration and the telemetry choices. These groups
// lived at the bottom of Settings › Devices until the tab was split
// (2026-10-31); each component, and its behaviour, is unchanged. On a phone
// the modal shows only this browser's encryption switch here instead.
export function PrivacySecurityTab() {
  return (
    <>
      <AgentAuthoritySetting />
      <HumanProofSetting />
      <RemoteDevicesSetting />
      <AlertsHandlerSetting />
      <AgentIntegrationSetting />
      <CheckinSetting />
    </>
  );
}
