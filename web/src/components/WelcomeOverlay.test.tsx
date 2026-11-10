import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { useEntitlements } from '../entitlements';
import { WelcomeOverlay } from './WelcomeOverlay';

function tourText(): string {
  let text = '';
  for (let i = 0; i < 3; i++) {
    text += screen.getByRole('dialog').textContent ?? '';
    const next = screen.queryByRole('button', { name: 'Next' });
    if (next) fireEvent.click(next);
  }
  return text;
}

describe('WelcomeOverlay', () => {
  it('keeps phone, Orchestrator and ARIA out of the no-account tour', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
    render(<WelcomeOverlay open onClose={() => {}} />);
    const text = tourText();
    expect(text).not.toMatch(/phone/i);
    expect(text).not.toMatch(/orchestrator/i);
    expect(text).not.toMatch(/ARIA/);
  });

  it('mentions the phone but not the Orchestrator for a free account', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: true });
    render(<WelcomeOverlay open onClose={() => {}} />);
    const text = tourText();
    expect(text).toMatch(/your phone/);
    expect(text).not.toMatch(/orchestrator/i);
  });

  it('still mentions phone and the Orchestrator for a team install, without ARIA', () => {
    useEntitlements.setState({ loaded: true, team: true, operator: true });
    render(<WelcomeOverlay open onClose={() => {}} />);
    const text = tourText();
    expect(text).toMatch(/your phone/);
    expect(text).toMatch(/Orchestrator pane/);
    expect(text).not.toMatch(/ARIA/);
  });

  it('makes step 2 about starting an agent, not shortcuts, in three steps or fewer', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
    render(<WelcomeOverlay open onClose={() => {}} />);
    expect(screen.getByText(/1 \/ [1-3]$/)).toBeInTheDocument();
    expect(screen.getByRole('dialog').textContent).not.toMatch(/Codex/);
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(screen.getByRole('dialog', { name: 'Start your first agent' })).toBeInTheDocument();
    expect(screen.getByText('Start Claude Code')).toBeInTheDocument();
    expect(screen.getByRole('dialog').querySelector('kbd')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(screen.getByRole('dialog').textContent).not.toMatch(/Restarting/);
  });

  it('keeps new-user jargon out of the tour', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: true });
    render(<WelcomeOverlay open onClose={() => {}} />);
    expect(tourText()).not.toMatch(/daemon|solud|MCP|\bhost\b|distro/i);
  });

  it('ends with what leaves this computer and a privacy link', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
    render(<WelcomeOverlay open onClose={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(screen.queryByText('What leaves this computer')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(screen.getByText('What leaves this computer')).toBeInTheDocument();
    expect(screen.getByText(/anonymous daily check-in/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Privacy policy' })).toHaveAttribute(
      'href',
      'https://soluterminal.com/privacy/',
    );
  });

  it('tells a free account what voice and the relay send', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: true });
    render(<WelcomeOverlay open onClose={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(screen.getByText(/last few lines of terminal text/)).toBeInTheDocument();
    expect(screen.getByText(/through our relay/)).toBeInTheDocument();
  });

  it('can be skipped from its first slide, with a single Skip there', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
    const close = vi.fn();
    render(<WelcomeOverlay open onClose={close} />);
    expect(screen.getAllByRole('button', { name: /skip/i })).toHaveLength(1);
    fireEvent.click(screen.getByRole('button', { name: 'Skip tour' }));
    expect(close).toHaveBeenCalledOnce();
  });

  it('tells someone who skips from slide 1 about the check-in first', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
    const close = vi.fn();
    render(<WelcomeOverlay open onClose={close} />);
    // Shown on the slide that has Skip tour, before anything is pressed.
    const dialog = screen.getByRole('dialog', { name: 'Welcome to SoluTerminal' });
    expect(dialog.textContent).toMatch(
      /one anonymous check-in a day \(version, OS, a random install ID and two yes\/no usage flags\)/,
    );
    expect(dialog.textContent).toMatch(/turn it off in Settings/);
    expect(screen.getByRole('button', { name: 'Skip tour' })).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: 'Skip tour' }));
    expect(close).toHaveBeenCalledOnce();
  });

  it('links from the check-in line to the full privacy note', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
    render(<WelcomeOverlay open onClose={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: 'What leaves this computer →' }));
    expect(screen.getByText('What leaves this computer')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Privacy policy' })).toBeInTheDocument();
    // The short line belongs to the first slide only.
    expect(screen.queryByRole('button', { name: 'What leaves this computer →' })).toBeNull();
  });

  it('keeps Skip in the corner on the later slides', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
    render(<WelcomeOverlay open onClose={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(screen.queryByRole('button', { name: 'Skip tour' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Skip' })).toBeInTheDocument();
  });

  it('points at the waiting-on-you bell, and names the sliders as the alert settings', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
    render(<WelcomeOverlay open onClose={() => {}} />);
    const text = tourText();
    expect(text).toMatch(/bell in the top bar shows how many are\s+waiting on you/);
    expect(text).toMatch(/sliders\s+button next to it sets how alerts and sounds reach you/);
  });
});
