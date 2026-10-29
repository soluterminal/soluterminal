// Inventory of every gated surface in the web app and the flag it follows
// (entitlements.ts). The daemon's twin is daemon/feature_gates_test.go.
//
//   operator: the operator's internal tooling. Customers never get it, even
//             on a team plan.
//   team:     what a customer team plan could carry.
//   account:  a signed-in free account (phone, push, voice, smart approvals).
//
// featureGates.test.ts checks that each file listed here (or registered by an
// optional module through the featureGates slot in extensions.ts) reads the
// matching hook, and that no file reads a gate hook without being listed, so a
// new gate cannot ship uninventoried. A file that gates surfaces on two flags is
// listed once per flag.

export type GateFlag = 'operator' | 'team' | 'account';

export interface FeatureGate {
  // Source file, relative to web/src.
  file: string;
  flag: GateFlag;
  surface: string;
}

export const FEATURE_GATES: readonly FeatureGate[] = [
  // Operator: the engineering pipeline and its chrome.
  { file: 'App.tsx', flag: 'operator', surface: 'Orchestrator presets, Muse install banner, Tasks shortcut' },
  { file: 'components/Sidebar.tsx', flag: 'operator', surface: 'Agents sections; internal sidebar defaults' },
  { file: 'components/sidebar/SidebarCustomizer.tsx', flag: 'operator', surface: 'Agents in the customiser' },
  { file: 'components/ScheduleDialog.tsx', flag: 'operator', surface: 'Agent-task schedule options' },
  { file: 'components/SetupChecklist.tsx', flag: 'operator', surface: 'Muse row' },
  { file: 'components/WelcomeOverlay.tsx', flag: 'operator', surface: 'Orchestrator line on the welcome slides' },
  { file: 'components/Topbar.tsx', flag: 'operator', surface: 'Refresh view button' },
  { file: 'components/SettingsModal.tsx', flag: 'operator', surface: 'Services and Warden tabs' },
  { file: 'components/CommandPalette.tsx', flag: 'operator', surface: 'Tasks and Orchestrator palette actions' },
  { file: 'components/Pane.tsx', flag: 'operator', surface: 'Announce delegation, convert to Orchestrator' },
  { file: 'components/NativePane.tsx', flag: 'operator', surface: 'Announce delegation, convert to Orchestrator' },
  { file: 'components/NewPaneDialog.tsx', flag: 'operator', surface: 'Orchestrator role, GLM model chip' },
  { file: 'components/VersionPopover.tsx', flag: 'operator', surface: 'Raw commit notes' },
  { file: 'components/WhatsNewToast.tsx', flag: 'operator', surface: 'Raw commit notes' },
  { file: 'components/StatusBar.tsx', flag: 'operator', surface: 'Release chips and system detail' },
  { file: 'components/system/SystemPage.tsx', flag: 'operator', surface: 'Workflow, release and cleanup cards' },
  // Team: what a customer team plan could carry.
  { file: 'components/settings/DevicesTab.tsx', flag: 'team', surface: '"Team account" label' },
  { file: 'components/PaneCredentialBadge.tsx', flag: 'team', surface: 'Saved-account detail on the pane badge' },
  { file: 'components/SmartApprovalsHint.tsx', flag: 'team', surface: 'Team smart-approval limits' },
  { file: 'components/Sidebar.tsx', flag: 'team', surface: 'Credentials section' },
  { file: 'components/sidebar/SidebarCustomizer.tsx', flag: 'team', surface: 'Credentials in the customiser' },
  { file: 'components/ClaudeInstallBanner.tsx', flag: 'team', surface: '"Install status stays in Credentials" hint' },
  { file: 'components/NewPaneDialog.tsx', flag: 'team', surface: 'Raw "claude --resume" chip' },
  { file: 'components/system/SystemPage.tsx', flag: 'team', surface: 'Engineering detail: versions, Go memory, storage producers, host storage, listener' },
];

// The hooks each flag is read through.
export const GATE_HOOKS: Record<GateFlag, readonly string[]> = {
  operator: ['useOperatorFeatures', 'operatorFeaturesAllowed'],
  team: ['useTeamFeatures', 'teamFeaturesAllowed', 's.team', 'state.team'],
  account: ['useAccountFeatures', 'accountFeaturesAllowed', 'usePhoneAccessAllowed'],
};
