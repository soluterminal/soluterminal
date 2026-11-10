import { useEffect, useId, useRef, useState } from 'react';
import { useFocusTrap } from './useFocusTrap';
import { useAccountFeatures, useOperatorFeatures } from '../entitlements';
import { WelcomePrivacyNote } from './WelcomePrivacyNote';

interface Props {
  open: boolean;
  onClose: () => void;
}

interface Slide {
  title: string;
  body: React.ReactNode;
}

// Phone access needs a free account and the Orchestrator is team-only on the
// public beta, so the tour mentions each only where it applies. The copy must
// hold on a fresh PC: only Claude Code has guided install today, so the tour
// names the button that exists rather than every agent.
function buildSlides(team: boolean, phone: boolean): Slide[] {
  return [
    {
      title: 'Welcome to SoluTerminal',
      body: (
        <>
          Run AI coding agents like Claude Code side by side with your terminals,
          and see at a glance which one needs you. Close the window and they keep
          working{phone ? '; check in from your phone anytime.' : '.'}
        </>
      ),
    },
    {
      title: 'Start your first agent',
      body: (
        <>
          When you're done here, click <strong>Start Claude Code</strong> in the
          empty window. If it isn't on this PC yet, the setup checklist that comes next
          helps you install it.
          {team && ' Or keep one Orchestrator pane to coordinate work across your panes.'}
        </>
      ),
    },
    {
      title: 'When an agent needs you',
      body: (
        <>
          Its pane turns amber and the bell in the top bar shows how many are
          waiting on you. Click the bell to jump straight there. The sliders
          button next to it sets how alerts and sounds reach you.
        </>
      ),
    },
  ];
}

export function WelcomeOverlay({ open, onClose }: Props) {
  const [slide, setSlide] = useState(0);
  const cardRef = useRef<HTMLDivElement>(null);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const titleId = useId();
  useFocusTrap(cardRef, open);
  const team = useOperatorFeatures();
  const phone = useAccountFeatures();
  const slides = buildSlides(team, phone);

  useEffect(() => {
    if (!open) return;
    setSlide(0);
    headingRef.current?.focus();
  }, [open]);

  if (!open) return null;

  const first = slide === 0;
  const last = slide >= slides.length - 1;
  const current = slides[slide];

  return (
    <div className="dialog-backdrop" onMouseDown={onClose}>
      <div
        ref={cardRef}
        className="dialog welcome-overlay"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        onKeyDown={(e) => {
          e.stopPropagation();
          if (e.key === 'Escape') { e.preventDefault(); onClose(); }
          if (e.key === 'ArrowRight') { e.preventDefault(); setSlide((s) => Math.min(slides.length - 1, s + 1)); }
          if (e.key === 'ArrowLeft') { e.preventDefault(); setSlide((s) => Math.max(0, s - 1)); }
        }}
        onMouseDown={(e) => e.stopPropagation()}
        style={{ width: 460, maxWidth: '92vw' }}
      >
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <h2 id={titleId} ref={headingRef} tabIndex={-1} style={{ margin: 0, fontSize: 16 }}>{current.title}</h2>
          {/* The first slide offers Skip tour beside Next instead. */}
          {!first && (
            <button
              type="button"
              onClick={onClose}
              title="Skip (Esc)"
              aria-label="Skip"
              style={{ fontSize: 11, padding: '2px 8px' }}
            >Skip</button>
          )}
        </div>
        <div style={{ marginTop: 12, marginBottom: 16, lineHeight: 1.55, fontSize: 13 }}>
          {current.body}
          {last && <WelcomePrivacyNote account={phone} />}
        </div>
        <div className="row" style={{ alignItems: 'center', justifyContent: 'space-between' }}>
          <div role="status" aria-live="polite" style={{ fontSize: 11, color: 'var(--fg-dim)' }}>
            {slide + 1} / {slides.length}
          </div>
          <div style={{ display: 'flex', gap: 6 }}>
            {/* Getting to the first agent shouldn't take three slides. Skipping
                still goes on to the setup checklist, and Help › Getting
                started brings the tour back. */}
            {first && (
              <button type="button" title="Skip tour (Esc)" onClick={onClose}>Skip tour</button>
            )}
            {slide > 0 && (
              <button
                type="button"
                onClick={() => setSlide((s) => Math.max(0, s - 1))}
              >Back</button>
            )}
            <button
              type="button"
              className="primary"
              onClick={() => {
                if (last) onClose();
                else setSlide((s) => s + 1);
              }}
            >{last ? 'Get started' : 'Next'}</button>
          </div>
        </div>
        {/* Skip tour skips the last slide's full privacy note, so the first
            slide says the check-in is on and links to that note. */}
        {first && (
          <div style={{ marginTop: 12, fontSize: 11, color: 'var(--fg-dim)', lineHeight: 1.5 }}>
            SoluTerminal sends one anonymous check-in a day (version, OS, a random install ID and two yes/no usage
            flags). You can turn it off in Settings.{' '}
            <button
              type="button"
              onClick={() => setSlide(slides.length - 1)}
              style={{ fontSize: 11, padding: 0, border: 0, background: 'none', color: 'var(--accent)', cursor: 'pointer', textDecoration: 'underline' }}
            >What leaves this computer →</button>
          </div>
        )}
      </div>
    </div>
  );
}
