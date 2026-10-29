import { useEffect, useRef, useState } from 'react';
import { api, type FeedbackAttachment, type FeedbackReq, type FeedbackStatus } from '../../api';
import { SITE_CONTACT_EMAIL } from '../../lib/siteLinks';
import { useStore } from '../../store';
import { useCloud } from '../../cloud';
import { useUnsentInput } from '../../lib/unsentInput';

// The feedback form — extracted from the old FeedbackModal so the inbox
// panel's Compose view hosts it (the modal is retired; compose now lives
// where the replies land). Same contract as before: the client collects
// intent only; the daemon redacts context and the cloud files the issue.
//
// Drafts: type/title/body/snapshot persist to localStorage on every change
// and restore on mount, so closing the panel mid-thought loses nothing.
// Screenshots are NOT drafted (megabytes of base64 in localStorage is how
// quota errors happen); they re-attach in seconds.

type FeedbackType = 'bug' | 'idea' | 'question';

const TYPE_OPTIONS: { value: FeedbackType; label: string; hint: string }[] = [
  { value: 'bug', label: 'Bug', hint: 'Something is broken or behaving wrong' },
  { value: 'idea', label: 'Idea', hint: 'A feature or improvement you want' },
  { value: 'question', label: 'Question', hint: 'You need help or clarification' },
];

const ALLOWED_MIME = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'] as const;
const MAX_BYTES = 5 * 1024 * 1024;
const MAX_FILES = 3;
const DRAFT_KEY = 'solu.feedbackDraft.v1';

const webVersion = typeof __SOLU_WEB_VERSION__ === 'string' ? __SOLU_WEB_VERSION__ : 'dev';

interface Draft {
  type: FeedbackType;
  title: string;
  body: string;
  includeSnapshot: boolean;
}

function loadDraft(): Draft | null {
  try {
    const raw = localStorage.getItem(DRAFT_KEY);
    if (!raw) return null;
    const d = JSON.parse(raw) as Draft;
    if (typeof d.title !== 'string' || typeof d.body !== 'string') return null;
    return d;
  } catch {
    return null;
  }
}

export function FeedbackComposer({ onDone, onCancel }: { onDone: () => void; onCancel: () => void }) {
  const toast = useStore((s) => s.toast);
  const focusedPaneId = useStore((s) => s.focusedPaneId);

  const draft = useRef<Draft | null | undefined>(undefined);
  if (draft.current === undefined) draft.current = loadDraft();
  const [type, setType] = useState<FeedbackType>(draft.current?.type ?? 'bug');
  const [title, setTitle] = useState(draft.current?.title ?? '');
  const [body, setBody] = useState(draft.current?.body ?? '');
  useUnsentInput(body);
  // Off by default: pane text and logs go only when the user asks.
  const [includeSnapshot, setIncludeSnapshot] = useState(draft.current?.includeSnapshot ?? false);
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [attachments, setAttachments] = useState<FeedbackAttachment[]>([]);
  const [sizes, setSizes] = useState<number[]>([]);
  const [dragOver, setDragOver] = useState(false);
  // Number of addFiles() encode batches still resolving — submit must wait so
  // screenshots can't be dropped silently.
  const [encoding, setEncoding] = useState(0);
  const fileInputRef = useRef<HTMLInputElement>(null);
  // Checked before the user types, so a computer that can't send feedback
  // shows another way to reach us instead of failing after the form is filled.
  const [status, setStatus] = useState<FeedbackStatus | null>(null);

  useEffect(() => {
    let mounted = true;
    void api
      .feedbackStatus()
      .then((s) => {
        if (mounted) setStatus(s);
      })
      .catch(() => {
        /* unknown: keep the form; submit reports any error */
      });
    return () => {
      mounted = false;
    };
  }, []);

  // Persist the text draft on every change; cheap (tiny payload) and it makes
  // "close panel, come back tomorrow" free.
  useEffect(() => {
    const d: Draft = { type, title, body, includeSnapshot };
    try {
      if (!title && !body) localStorage.removeItem(DRAFT_KEY);
      else localStorage.setItem(DRAFT_KEY, JSON.stringify(d));
    } catch { /* quota — drafts are best-effort */ }
  }, [type, title, body, includeSnapshot]);

  function validateFile(file: File): string | null {
    if (!(ALLOWED_MIME as readonly string[]).includes(file.type)) {
      return `${file.name}: unsupported type (PNG, JPEG, GIF, or WebP only).`;
    }
    if (file.size > MAX_BYTES) {
      return `${file.name}: too large (max 5 MB).`;
    }
    return null;
  }

  function fileToAttachment(file: File): Promise<FeedbackAttachment> {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onerror = () => reject(reader.error ?? new Error('read failed'));
      reader.onload = () => {
        const result = String(reader.result ?? '');
        const comma = result.indexOf(',');
        const data = comma >= 0 ? result.slice(comma + 1) : result;
        resolve({ name: file.name, mime: file.type as FeedbackAttachment['mime'], data });
      };
      reader.readAsDataURL(file);
    });
  }

  async function addFiles(list: FileList | null | undefined) {
    if (!list || list.length === 0) return;
    const accepted: File[] = [];
    for (const file of Array.from(list)) {
      const problem = validateFile(file);
      if (problem) { setErr(problem); continue; }
      if (attachments.length + accepted.length >= MAX_FILES) {
        setErr(`You can attach at most ${MAX_FILES} screenshots.`);
        break;
      }
      accepted.push(file);
    }
    if (accepted.length === 0) return;
    setEncoding((n) => n + 1);
    try {
      const encoded = await Promise.all(accepted.map(fileToAttachment));
      // Cap against the freshest state so near-simultaneous adds can't push
      // past MAX_FILES; sizes stays index-aligned.
      setAttachments((prev) => [...prev, ...encoded].slice(0, MAX_FILES));
      setSizes((prev) => [...prev, ...accepted.map((f) => f.size)].slice(0, MAX_FILES));
    } catch (e) {
      setErr(`Could not read image: ${(e as Error).message}`);
    } finally {
      setEncoding((n) => n - 1);
    }
  }

  function removeAttachment(idx: number) {
    setAttachments((prev) => prev.filter((_, i) => i !== idx));
    setSizes((prev) => prev.filter((_, i) => i !== idx));
  }

  async function submit(e?: React.FormEvent) {
    e?.preventDefault();
    if (encoding > 0) { setErr('Still adding screenshots — try again in a moment.'); return; }
    const t = title.trim();
    const b = body.trim();
    if (!t) { setErr('Add a short title.'); return; }
    if (!b) { setErr('Describe what happened.'); return; }
    const payload: FeedbackReq = {
      type,
      title: t,
      body: b,
      includeSnapshot,
      clientVersion: webVersion,
      ...(includeSnapshot && focusedPaneId ? { paneId: focusedPaneId } : {}),
      ...(attachments.length ? { attachments } : {}),
    };
    setErr(null);
    setSubmitting(true);
    try {
      const res = await api.submitFeedback(payload);
      toast(
        res.number ? `Feedback filed — #${res.number}`
          : res.queued ? 'Feedback received — it will be filed shortly'
          : 'Feedback sent',
        { kind: 'success', icon: '✓' },
      );
      try { localStorage.removeItem(DRAFT_KEY); } catch { /* best effort */ }
      onDone();
    } catch (e) {
      setErr((e as Error).message || 'Could not send feedback.');
    } finally {
      setSubmitting(false);
    }
  }

  const active = TYPE_OPTIONS.find((o) => o.value === type)!;

  if (status && !status.available) {
    return <FeedbackUnavailable reason={status.reason} onCancel={onCancel} onLeave={onDone} />;
  }

  return (
    <form className="inbox-compose" onSubmit={submit}>
      <div>
        <label>Type</label>
        <div className="dialog-chip-row">
          {TYPE_OPTIONS.map((o) => (
            <button
              key={o.value}
              type="button"
              onClick={() => setType(o.value)}
              className={`inbox-filter${type === o.value ? ' is-active' : ''}`}
              title={o.hint}
            >{o.label}</button>
          ))}
        </div>
        <div className="dialog-help">{active.hint}.</div>
      </div>

      <div>
        <label htmlFor="fb-title">Title</label>
        <input
          id="fb-title"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          placeholder="Short summary"
          className="dialog-control"
          autoFocus
          maxLength={200}
        />
      </div>

      <div>
        <label htmlFor="fb-body">Details</label>
        <textarea
          id="fb-body"
          value={body}
          onChange={(e) => setBody(e.target.value)}
          placeholder={type === 'bug'
            ? 'What happened? What did you expect? Steps to reproduce?'
            : 'Tell us more…'}
          className="dialog-control"
          rows={6}
        />
      </div>

      <div>
        <label>Screenshots</label>
        <div
          onDragOver={(e) => { e.preventDefault(); if (!dragOver) setDragOver(true); }}
          onDragLeave={(e) => { e.preventDefault(); setDragOver(false); }}
          onDrop={(e) => { e.preventDefault(); setDragOver(false); void addFiles(e.dataTransfer.files); }}
          className={`inbox-dropzone${dragOver ? ' is-drag' : ''}`}
        >
          <input
            ref={fileInputRef}
            type="file"
            accept="image/png,image/jpeg,image/gif,image/webp"
            multiple
            style={{ display: 'none' }}
            onChange={(e) => {
              void addFiles(e.target.files);
              e.target.value = '';
            }}
          />
          <button
            type="button"
            onClick={() => fileInputRef.current?.click()}
            disabled={attachments.length >= MAX_FILES}
          >
            Add screenshots
          </button>
          <span className="dialog-help" style={{ margin: 0 }}>
            {dragOver ? 'Drop images to attach' : 'or drop images here'}
          </span>
        </div>

        {attachments.length > 0 && (
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10, marginTop: 8 }}>
            {attachments.map((a, i) => (
              <div key={i} style={{ position: 'relative', width: 96 }}>
                <img
                  src={`data:${a.mime};base64,${a.data}`}
                  alt={a.name}
                  style={{ width: 96, height: 64, objectFit: 'cover', borderRadius: 4, display: 'block' }}
                />
                <button
                  type="button"
                  aria-label={`Remove ${a.name}`}
                  onClick={() => removeAttachment(i)}
                  style={{
                    position: 'absolute', top: -6, right: -6,
                    width: 20, height: 20, borderRadius: '50%', lineHeight: '18px', padding: 0,
                  }}
                >×</button>
                <div
                  className="dialog-help"
                  style={{ margin: 0, fontSize: 11, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                  title={a.name}
                >
                  {a.name} · {Math.max(1, Math.round((sizes[i] ?? 0) / 1024))} KB
                </div>
              </div>
            ))}
          </div>
        )}

        {attachments.length > 0 && (
          // The cloud doesn't keep images yet: committed into the feedback
          // repo they could never be deleted on request (cloud/feedback.go
          // feedbackStoresImages). Say so before the report goes.
          <div className="dialog-help" style={{ marginTop: 6 }}>
            Screenshots aren't stored yet; please describe what you saw.
          </div>
        )}

        <div className="inbox-note-warn" style={{ marginTop: 6 }}>
          Screenshots are sent as-is and are not redacted — don't include
          passwords, tokens, API keys, or anything sensitive visible on screen.
        </div>
      </div>

      <div>
        <label style={{ display: 'flex', gap: 8, alignItems: 'flex-start', cursor: 'pointer' }}>
          <input
            type="checkbox"
            checked={includeSnapshot}
            onChange={(e) => setIncludeSnapshot(e.target.checked)}
            style={{ marginTop: 3 }}
          />
          <span>Attach redacted active-pane text and recent logs</span>
        </label>
        <div className="dialog-help">
          This is terminal text, not a screenshot. Tokens, keys, and home paths
          are stripped on your machine before anything is sent.
        </div>
      </div>

      {err && <div className="dialog-error">{err}</div>}

      <div className="dialog-actions">
        {encoding > 0 && (
          <span className="dialog-help" style={{ margin: 0, marginRight: 'auto' }}>
            Processing images…
          </span>
        )}
        <button type="button" onClick={onCancel} disabled={submitting}>Cancel</button>
        <button type="submit" className="primary" disabled={submitting || encoding > 0}>
          {submitting ? 'Sending…' : 'Send feedback'}
        </button>
      </div>
    </form>
  );
}


// Shown instead of the form when this computer can't send feedback. The
// address is shown as text with a Copy button: mailto links aren't reliably
// handed to an email app from the desktop shell. Signed out, a Sign in button
// opens the normal sign-in where this install offers one; onLeave closes the
// surrounding window first, since the sign-in screen replaces the app.
function FeedbackUnavailable({
  reason,
  onCancel,
  onLeave,
}: {
  reason?: FeedbackStatus['reason'];
  onCancel: () => void;
  onLeave: () => void;
}) {
  const toast = useStore((s) => s.toast);
  const canSignIn = useCloud((s) => (
    s.mode === 'cloud' && s.isLocal && s.accountOptional && (s.withoutAccount || !s.session)
  ));
  const signIn = () => {
    onLeave();
    useCloud.getState().requestSignIn();
  };
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(SITE_CONTACT_EMAIL);
      toast('Email address copied', { kind: 'success', icon: '✓' });
    } catch {
      toast(`Couldn't copy. The address is ${SITE_CONTACT_EMAIL}`, { kind: 'error' });
    }
  };
  return (
    <div className="inbox-compose">
      <h3 style={{ margin: 0 }}>
        {reason === 'unpaired' ? 'Connect this computer to send feedback' : 'Sign in to send feedback'}
      </h3>
      <p className="dialog-help">
        {reason === 'unpaired'
          ? "Your account isn't connected on this computer yet, so feedback can't be sent from here."
          : 'Feedback from the app goes through a free account, so we can reply to you here.'}{' '}
        Or email us at <strong style={{ userSelect: 'all' }}>{SITE_CONTACT_EMAIL}</strong>.
      </p>
      <div className="dialog-actions">
        <button type="button" onClick={onCancel}>
          Close
        </button>
        {reason !== 'unpaired' && canSignIn ? (
          <>
            <button type="button" onClick={() => void copy()}>
              Copy email address
            </button>
            <button type="button" className="primary" onClick={signIn}>
              Sign in
            </button>
          </>
        ) : (
          <button type="button" className="primary" onClick={() => void copy()}>
            Copy email address
          </button>
        )}
      </div>
    </div>
  );
}
