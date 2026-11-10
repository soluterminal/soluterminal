// Named UI slots for optional features.
//
// The app renders each slot where the feature would appear. Modules under
// ./private/ fill the slots: each file named *.register.ts or *.register.tsx
// default-exports a WebExtension, and loadExtensions.ts (imported first by
// App.tsx) registers them. A tree without ./private registers nothing, so tsc,
// vite and vitest work with or without those files, and every slot falls
// back to rendering nothing (or to the default documented on it).
//
// Two lower layers follow the same rule: a feature's API calls live in
// ./private/**/*.api.ts (merged into `api` by api.ts) and its store state in
// ./private/**/*.slice.ts (merged into the store by store.ts); each declares
// its additions on FeatureApi / FeatureSlices.
//
// This module imports no feature code, so reading a slot never pulls a
// feature into a module graph (or a test) that did not load it. Tests of a
// feature's slot behaviour import loadExtensions.ts, or set their own
// registrations with setExtensionsForTests.

import type {
  ComponentType,
  MouseEvent as ReactMouseEvent,
  PointerEvent as ReactPointerEvent,
  RefObject,
  TouchEvent as ReactTouchEvent,
} from 'react';
import type { AgentReport } from './api';
import type { AgentKind } from './lib/agentKind';
import type { MiddleKey } from './components/sidebar/sidebarSections';
import type { FeatureGate } from './featureGates';
import type { SettingsRegistryEntries } from './settingsRegistry';
import type { SettingsTab } from './settings';
import type { OrchestratorHostInfo, PaneInfo, ScheduleChatRef } from './types';

/** One command-palette action. */
export interface PaletteAction {
  id: string;
  label: string;
  hint?: string;
  category: string;
  keywords?: string;
  /** Words this action answers to first: typing one (or the first four or
   *  more letters of one) ranks it above every other match. */
  aliases?: string[];
  shortcut?: string;
  run: () => void | Promise<void>;
}

export interface PaletteContext {
  /** Closes the palette; call it before running the action. */
  close: () => void;
}

export interface SidebarSectionProps {
  collapsed: boolean;
  onToggle: () => void;
}

/** Whether an agent can host a coordinating pane role, and why not. */
export interface RoleHostSupport {
  supported: boolean;
  available: boolean;
  reason: string;
  label: string;
}

export interface PaneRoleHosts {
  support: (agent: AgentKind | null, hosts: OrchestratorHostInfo[] | null) => RoleHostSupport;
  convertAllowed: (
    pane: Pick<PaneInfo, 'agent' | 'cmd' | 'kind' | 'codexThreadId' | 'status'>,
    hosts: OrchestratorHostInfo[] | null,
  ) => boolean;
  /** Hosts assumed when the daemon has no host endpoint. */
  legacyDaemonHosts: OrchestratorHostInfo[];
}

/** Durable reports that join the inbox next to schedule reports. */
export interface InboxReports {
  useReports: () => AgentReport[];
  acknowledge: (report: AgentReport) => Promise<void>;
  acknowledgeAll: (reports: AgentReport[]) => Promise<void>;
}

/** A Settings tab, placed after a core tab. */
export interface SettingsTabEntry {
  key: SettingsTab;
  label: string;
  after: SettingsTab;
  /** Whether the tab is offered right now. A hook: the modal calls it on
   *  every render. */
  useVisible: () => boolean;
  Component: ComponentType;
}

/** Delivery of a task schedule's reports to a chat (Schedule.deliverChat). */
export interface ScheduleChatDeliverySlot {
  /** The opt-in checkbox text. */
  label: string;
  /** Dim text after the label. */
  hint?: string;
  /** Toast when the box is ticked but no chat is picked. */
  missingChoice: string;
  /** Picks the destination; value.chatId is '' until one is chosen. */
  Picker: ComponentType<{ value: ScheduleChatRef; onChange: (value: ScheduleChatRef) => void }>;
}

/** A daemon UI event as the app's event stream parses it. */
export interface UiEvent {
  type: string;
  [field: string]: unknown;
}

/** App-wide key handling that runs ahead of the app's own shortcuts. */
export interface GlobalKeys {
  /** Sees every capture-phase keydown first; true when it consumed the key
   *  (the app's own shortcuts then skip it). */
  keydown: (e: KeyboardEvent) => boolean;
  keyup: (e: KeyboardEvent) => void;
  /** The window lost focus or the page was hidden. */
  blur: () => void;
  /** The app's key listeners are being removed (they re-attach after). */
  detach: () => void;
}

/** A section inside a core Settings tab, after the tab's own groups. */
export interface SettingsSectionEntry {
  tab: SettingsTab;
  Component: ComponentType;
  /** Whether the section is offered right now. A hook: called on every
   *  render. Leave it out for a section that always shows. */
  useVisible?: () => boolean;
}

/** A group of rows in the keyboard-shortcut overlay. */
export interface ShortcutGroupEntry {
  title: string;
  items: { keys?: string[]; label: string; actionId?: string }[];
  /** Whether the group shows; a hook called on every render. */
  useVisible: () => boolean;
}

/** Composer dictation, wired by a feature module. */
export interface ComposerVoiceConfig {
  /** The composer's chat textarea — recording target + where dictation lands. */
  targetRef: RefObject<HTMLTextAreaElement>;
  /** Disambiguation context for the cleanup LLM, captured at record-start
   *  (e.g. the focused pane's terminal tail). Best-effort. */
  getContext?: (() => string | undefined) | undefined;
  /** Submit the draft after dictation when auto-send (🚀) is on. */
  onSubmitText?: (() => void) | undefined;
  isTargetCurrent?: (() => boolean) | undefined;
  captureTargetGuard?: (() => () => boolean) | undefined;
  /** Error sink (the surface shows it as a toast; the box also self-reports). */
  onError?: ((message: string, kind: 'error' | 'warn') => void) | undefined;
  /** Register this composer as App's Alt+S recorder while its pane is focused. */
  shortcutActive?: boolean | undefined;
  /** Pane id used to pin the global voice session and delivery indicator. */
  shortcutPaneId?: string | undefined;
  /** Master switch — false disables every gesture (e.g. no focused target). */
  enabled: boolean;
}

/** Hold-mode slide affordance the user is currently arming. */
export type SlideArm = 'none' | 'lock' | 'cancel';

/** Handlers spread onto the textarea while it is empty (the composer gates on
 *  content). Touch + mouse are handled separately. */
export interface TextareaVoiceHandlers {
  onTouchStart: (e: ReactTouchEvent<HTMLTextAreaElement>) => void;
  onTouchMove: (e: ReactTouchEvent<HTMLTextAreaElement>) => void;
  onTouchEnd: (e: ReactTouchEvent<HTMLTextAreaElement>) => void;
  onTouchCancel: (e: ReactTouchEvent<HTMLTextAreaElement>) => void;
  onPointerDown: (e: ReactPointerEvent<HTMLTextAreaElement>) => void;
  onPointerMove: (e: ReactPointerEvent<HTMLTextAreaElement>) => void;
  onPointerUp: (e: ReactPointerEvent<HTMLTextAreaElement>) => void;
  onPointerCancel: (e: ReactPointerEvent<HTMLTextAreaElement>) => void;
  onContextMenu: (e: ReactMouseEvent<HTMLTextAreaElement>) => void;
}

/** What the composer reads from its dictation engine. */
export interface ComposerVoiceState {
  /** "idle" | "recording" | "transcribing". */
  phase: string;
  recording: boolean;
  transcribing: boolean;
  /** Locked (hands-free) recording vs an active hold — drives the overlay UI. */
  handsFree: boolean;
  /** Which slide affordance is armed during a hold (lock / cancel / none). */
  slide: SlideArm;
  elapsedMs: number;
  handlers: TextareaVoiceHandlers;
  /** Stop hands-free + deliver (overlay ⏹). */
  stopAndSend: () => void;
  /** Cancel/discard the current recording (overlay ✕). */
  cancel: () => void;
  /** Raw-then-patch: the polish call is in flight; drives the "polishing…"
   *  affordance and the provisional (dim) styling on the just-shown text. */
  polishing: boolean;
  /** Submit should be blocked right now — polishing AND still inside the cap
   *  window. The composer swallows Enter / disables Send while true. */
  submitBlocked: boolean;
  /** The composer calls this from the textarea's onChange so a genuine user
   *  edit (not our own polish patch) cancels the pending polish + gate. */
  onComposerInput: (value: string) => void;
}

export interface ComposerVoiceSlot {
  /** The engine. A hook: the composer calls it on every render. */
  useComposerVoice: (config: ComposerVoiceConfig) => ComposerVoiceState;
  /** The overlay shown over the box while recording or transcribing. */
  Recording: ComponentType<{ voice: ComposerVoiceState }>;
}

/** Dictation aimed at a terminal pane. */
export interface PaneDictation {
  phase: string;
  aboutToCancel: boolean;
}

export interface PaneDictationSlot {
  /** The pane's dictation state, or null. A hook. */
  usePaneDictation: (paneId: string) => PaneDictation | null;
  /** Non-hook read: whether a dictation is aimed at the pane right now. */
  isDictating: (paneId: string) => boolean;
  /** Called for every keystroke the user sends to a terminal pane. */
  noteTyped: (paneId: string) => void;
  /** The chip drawn over the pane while dictating. */
  Chip: ComponentType<PaneDictation>;
}

export interface WebExtension {
  /** Status-bar chips, after the phone-access chip. Each renders its own
   *  leading separator and returns null when it has nothing to say. */
  statusBarChips?: ComponentType[];
  /** Renderers for sidebar middle sections. A section key without a renderer
   *  is left out of the sidebar and its customizer. */
  sidebarSections?: Partial<Record<MiddleKey, ComponentType<SidebarSectionProps>>>;
  /** Components mounted once at the app root (dialog hosts, listeners). */
  appHosts?: ComponentType[];
  /** Command-palette actions, built each time the palette opens. */
  paletteActions?: ((ctx: PaletteContext) => PaletteAction[])[];
  /** The coordinating pane role. Without it no agent can host the role. */
  paneRoleHosts?: PaneRoleHosts;
  inboxReports?: InboxReports;
  /** Search categories, sections and controls contributed by optional modules. */
  settingsRegistryEntries?: SettingsRegistryEntries[];
  /** Settings tabs. */
  settingsTabs?: SettingsTabEntry[];
  /** Handlers for daemon UI events, by event type. */
  uiEvents?: Record<string, (event: UiEvent) => void>;
  /** Called each time the UI event stream (re)opens, with the entitlement
   *  reload it started, to resync state that only events update. */
  eventStreamOpened?: ((entitlements: Promise<void>) => void)[];
  /** Key handlers, created once per App. */
  globalKeys?: (() => GlobalKeys)[];
  /** Rendered first inside the app root (out-of-flow overlays). */
  rootOverlays?: ComponentType[];
  /** Top-bar items between the browser button and the comms group. Each
   *  renders its own leading divider, or nothing. */
  topbarItems?: ComponentType[];
  /** Phone bottom-bar items before the palette button. */
  mobileBarItems?: ComponentType[];
  /** Sections inside core Settings tabs. */
  settingsSections?: SettingsSectionEntry[];
  /** Groups in the keyboard-shortcut overlay. */
  shortcutGroups?: ShortcutGroupEntry[];
  composerVoice?: ComposerVoiceSlot;
  paneDictation?: PaneDictationSlot;
  scheduleChatDelivery?: ScheduleChatDeliverySlot;
  /** Feature-gate inventory entries for the files these modules own
   *  (see featureGates.ts). */
  featureGates?: FeatureGate[];
}

type ListSlot =
  | 'statusBarChips' | 'appHosts' | 'paletteActions' | 'settingsTabs' | 'eventStreamOpened'
  | 'globalKeys' | 'rootOverlays' | 'topbarItems' | 'mobileBarItems' | 'settingsSections'
  | 'shortcutGroups' | 'featureGates' | 'settingsRegistryEntries';
type SingleSlot = 'paneRoleHosts' | 'inboxReports' | 'composerVoice' | 'paneDictation' | 'scheduleChatDelivery';

let registered: WebExtension[] = [];
let testOverride: WebExtension[] | null = null;

function extensions(): WebExtension[] {
  return testOverride ?? registered;
}

/** Adds feature modules' registrations (loadExtensions.ts calls this once). */
export function registerExtensions(list: readonly WebExtension[]): void {
  registered = [...registered, ...list];
}

/** Every entry registered for a list slot, in register-file order. */
export function slotList<K extends ListSlot>(name: K): NonNullable<WebExtension[K]> {
  return extensions().flatMap((ext) => (ext[name] ?? []) as unknown[]) as NonNullable<WebExtension[K]>;
}

/** The registration for a single-valued slot (the last one wins). */
export function slotValue<K extends SingleSlot>(name: K): WebExtension[K] {
  let value: WebExtension[K] = undefined;
  for (const ext of extensions()) if (ext[name]) value = ext[name];
  return value;
}

/** The handler registered for a daemon UI event type, if any. */
export function uiEventHandler(type: string): ((event: UiEvent) => void) | undefined {
  let found: ((event: UiEvent) => void) | undefined;
  for (const ext of extensions()) found = ext.uiEvents?.[type] ?? found;
  return found;
}

/** The renderer registered for a sidebar section, if any. */
export function sidebarSectionSlot(key: MiddleKey): ComponentType<SidebarSectionProps> | undefined {
  let found: ComponentType<SidebarSectionProps> | undefined;
  for (const ext of extensions()) found = ext.sidebarSections?.[key] ?? found;
  return found;
}

const NO_ROLE_HOST: Omit<RoleHostSupport, 'label'> = { supported: false, available: false, reason: '' };

/** Whether the coordinating pane role exists in this build at all. */
export function paneRolesAvailable(): boolean {
  return !!slotValue('paneRoleHosts');
}

/** Whether `agent` can host the coordinating pane role. */
export function roleHostSupport(agent: AgentKind | null, hosts: OrchestratorHostInfo[] | null): RoleHostSupport {
  return slotValue('paneRoleHosts')?.support(agent, hosts) ?? { ...NO_ROLE_HOST, label: agent ?? 'agent' };
}

/** Whether a running pane may be converted to (or from) the role. */
export function roleConvertAllowed(
  pane: Pick<PaneInfo, 'agent' | 'cmd' | 'kind' | 'codexThreadId' | 'status'>,
  hosts: OrchestratorHostInfo[] | null,
): boolean {
  return slotValue('paneRoleHosts')?.convertAllowed(pane, hosts) ?? false;
}

const NO_REPORTS: AgentReport[] = [];

/** Extra inbox reports. The registration is fixed for the page's lifetime, so
 *  the hook order stays stable across renders. */
export function useInboxReports(): AgentReport[] {
  const source = slotValue('inboxReports');
  return source ? source.useReports() : NO_REPORTS;
}

/** Marks one extra inbox report read (no-op without a source). */
export function acknowledgeInboxReport(report: AgentReport): Promise<void> {
  return slotValue('inboxReports')?.acknowledge(report) ?? Promise.resolve();
}

/** Marks several extra inbox reports read (no-op without a source). */
export function acknowledgeAllInboxReports(reports: AgentReport[]): Promise<void> {
  return slotValue('inboxReports')?.acknowledgeAll(reports) ?? Promise.resolve();
}

const noop = () => {};
const INERT_COMPOSER_VOICE: ComposerVoiceState = {
  phase: 'idle',
  recording: false,
  transcribing: false,
  handsFree: false,
  slide: 'none',
  elapsedMs: 0,
  handlers: {
    onTouchStart: noop, onTouchMove: noop, onTouchEnd: noop, onTouchCancel: noop,
    onPointerDown: noop, onPointerMove: noop, onPointerUp: noop, onPointerCancel: noop,
    onContextMenu: noop,
  },
  stopAndSend: noop,
  cancel: noop,
  polishing: false,
  submitBlocked: false,
  onComposerInput: noop,
};

/** Whether a module provides composer dictation. */
export function composerVoiceAvailable(): boolean {
  return !!slotValue('composerVoice');
}

/** The composer's dictation engine: inert (never recording) without one. */
export function useComposerVoice(config: ComposerVoiceConfig): ComposerVoiceState {
  const slot = slotValue('composerVoice');
  return slot ? slot.useComposerVoice(config) : INERT_COMPOSER_VOICE;
}

/** The pane's dictation state, or null (always null without a module). */
export function usePaneDictation(paneId: string): PaneDictation | null {
  const slot = slotValue('paneDictation');
  return slot ? slot.usePaneDictation(paneId) : null;
}

/** Non-hook read of whether a dictation is aimed at the pane. */
export function paneDictating(paneId: string): boolean {
  return slotValue('paneDictation')?.isDictating(paneId) ?? false;
}

/** Tells a dictation module the user typed into a terminal pane. */
export function notePaneTyped(paneId: string): void {
  slotValue('paneDictation')?.noteTyped(paneId);
}

/** Replaces the registrations for one test; pass null to restore. */
export function setExtensionsForTests(next: WebExtension[] | null): void {
  testOverride = next;
}

/** The chat-delivery feature for schedule reports, when one is built in. */
export function scheduleChatDelivery(): ScheduleChatDeliverySlot | undefined {
  return slotValue('scheduleChatDelivery');
}
