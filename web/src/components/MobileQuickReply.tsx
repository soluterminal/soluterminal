import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import { effectiveTerminalTarget, terminalSessionKey, useStore } from '../store';
import { submitToPane } from '../ptySubmit';
import { api, currentRoutingScope, onRoutingScopeChange, routingPrincipalAllowed, type RoutingScope } from '../api';
import { captureSessionGesture, processIdentity, terminalSession, useTerminalWindowVisible } from '../lib/terminalViewSessions';
import { viewIsCurrent } from '../lib/viewActionGuard';
import { beginQuickReplyAttachment, quickReplyDraft, setQuickReplyDraft } from '../lib/quickReplyDrafts';
import { Composer } from './composer/Composer';
import { MobileApprovalBar } from './MobileApprovalBar';
import { isMenuKeyReply, paneScreen, sendMenuKey } from '../lib/menuKeys';
import type { PaneInfo } from '../types';

// Mobile reply follows the visible terminal, including a detached owner view.
// Each mounted composer is pinned to scope, process and navigation generation.
export function MobileQuickReply() {
  useSyncExternalStore(onRoutingScopeChange, () => `${currentRoutingScope().key}|${routingPrincipalAllowed()}`);
  const targetId = useStore(effectiveTerminalTarget);
  const pane = useStore((s) => targetId ? s.panes[targetId] ?? (s.mainPresentation.kind === 'owner' ? s.mainPresentation.pane : undefined) : undefined);
  const generation = useStore((s) => s.navigationGeneration);
  const windowVisible = useTerminalWindowVisible();
  const scope = currentRoutingScope();
  const visible = windowVisible && pane?.status === 'running' && (pane.state === 'awaiting-input' || pane.state === 'choice-prompt');
  useEffect(() => {
    if (!visible) return;
    document.body.classList.add('quick-reply-visible');
    return () => document.body.classList.remove('quick-reply-visible');
  }, [visible]);
  if (!targetId || !pane || !visible) return null;
  const key = terminalSessionKey(scope, targetId);
  return <TargetQuickReply key={JSON.stringify([key, processIdentity(pane), generation])} pane={pane} scope={scope} generation={generation} sessionKey={key} />;
}

function TargetQuickReply({ pane, scope, generation, sessionKey }: { pane: PaneInfo; scope: RoutingScope; generation: number; sessionKey: string }) {
  const identity = processIdentity(pane);
  const [text, setText] = useState(() => quickReplyDraft(sessionKey, identity));
  const textRef = useRef(text);
  const mounted = useRef(true);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const liveMetadata = () => {
    const state = useStore.getState();
    return state.panes[pane.id] ?? (state.mainPresentation.kind === 'owner' ? state.mainPresentation.pane : undefined);
  };
  const current = () => {
    const state = useStore.getState();
    const metadata = liveMetadata();
    return mounted.current && viewIsCurrent(scope, generation) && effectiveTerminalTarget(state) === pane.id && !!metadata && processIdentity(metadata) === identity;
  };
  const capture = () => {
    const session = terminalSession(sessionKey);
    const gesture = session ? captureSessionGesture(session) : () => true;
    return () => current() && gesture();
  };
  const changeDraft = (value: string) => {
    if (!current()) return;
    textRef.current = value;
    setQuickReplyDraft(sessionKey, identity, value);
    setText(value);
  };
  const label = pane.title || pane.cmd?.[0] || 'terminal';
  const exe = (pane.cmd?.[0] || '').split('/').pop()?.toLowerCase() || '';
  function submit() {
    if (!current() || !textRef.current) return;
    const valid = capture();
    const sender = useStore.getState().senders[sessionKey];
    if (!sender) return;
    const send = (bytes: string) => valid() && useStore.getState().senders[sessionKey] === sender && sender(bytes);
    // A single menu key at a choice prompt goes out without the forced Enter:
    // Claude/Codex menus act on the key itself, and the Enter would answer the
    // next screen (a queued permission prompt's default row). See menuKeys.ts.
    if (liveMetadata()?.state === 'choice-prompt' && isMenuKeyReply(textRef.current)) {
      const key = textRef.current.trim().toLowerCase();
      if (sendMenuKey(send, key, () => paneScreen(useStore.getState().paneSnapshots[sessionKey]))) changeDraft('');
      return;
    }
    const hasClaude = pane.hasClaude === true || exe === 'claude';
    const bracketed = !hasClaude && textRef.current.includes('\n');
    const body = bracketed ? `\x1b[200~${textRef.current}\x1b[201~` : textRef.current;
    // Guard both the body and delayed Enter against target/process/sender drift.
    if (submitToPane(send, body, { appendEnter: true, useBracketedPaste: bracketed })) changeDraft('');
  }
  async function pickFile(file: File) {
    if (!current()) return;
    if (!file.type.startsWith('image/')) {
      useStore.getState().toast('Only images can be attached right now', { kind: 'warn', ttlMs: 3000 });
      return;
    }
    const valid = capture();
    if (!valid()) return;
    const done = beginQuickReplyAttachment(sessionKey);
    try {
      const { path } = await api.pasteImage(pane.id, file, scope);
      if (!valid()) return;
      const isMuse = pane.hasMuse === true || exe === 'muse' || exe.startsWith('muse-bin-');
      const prior = textRef.current;
      changeDraft((prior && !/\s$/.test(prior) ? prior + ' ' : prior) + (isMuse ? '' : '@') + path + ' ');
      useStore.getState().toast(`Attached ${path.split(/[\\/]/).pop()}`, { kind: 'info', ttlMs: 2500 });
    } catch (err) {
      if (valid()) useStore.getState().toast(`Attach failed: ${(err as Error).message}`, { kind: 'error', ttlMs: 4000 });
    } finally { done(); }
  }
  return <div className="mobile-quick-reply" role="form" aria-label="Quick reply to focused terminal">
    {pane.state === 'choice-prompt' && <MobileApprovalBar sessionKey={sessionKey} capture={capture} />}
    <Composer inputRef={inputRef} draft={text} onDraftChange={changeDraft}
      placeholder={`Reply to ${label}${pane.state === 'choice-prompt' ? ' — y / n / number' : ''}…`}
      voiceHint={`Hold to talk · or reply to ${label}…`}
      onKeyDown={(e) => { if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); submit(); } }}
      onSend={submit} sendDisabled={text === ''} sendLabel="Send reply" onPickFile={pickFile}
      voice={{ onSubmitText: submit, isTargetCurrent: current, captureTargetGuard: capture, shortcutPaneId: pane.id,
        getContext: () => current() ? useStore.getState().paneSnapshots[sessionKey]?.() : undefined,
        onError: (message, kind) => { if (current()) useStore.getState().toast(message, { kind }); },
      }} />
  </div>;
}
