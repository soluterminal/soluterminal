import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  INSTALL_HELP_TITLE, INSTALL_LABEL, INSTALL_STEPS_ANDROID, INSTALL_STEPS_DESKTOP, INSTALL_STEPS_FIREFOX, INSTALL_THEN,
  InstallButton,
} from './InstallButton';

vi.mock('./useIsMobileView', () => ({ useIsMobileView: () => true }));

const UAS = {
  safari: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1',
  chrome: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/130.0.6723.90 Mobile/15E148 Safari/604.1',
  firefox: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/132.0 Mobile/15E148 Safari/605.1.15',
  edge: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 EdgiOS/130.0.2849.80 Mobile/15E148 Safari/605.1.15',
};
const ANDROID_CHROME = 'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36';
const ANDROID_FIREFOX = 'Mozilla/5.0 (Android 14; Mobile; rv:132.0) Gecko/132.0 Firefox/132.0';
const DESKTOP_EDGE = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0';

type Stash = { __SOLU_INSTALL_PROMPT__?: Event };

function installPrompt(prompt: () => Promise<void>) {
  const e = new Event('beforeinstallprompt') as Event & { prompt: () => Promise<void>; userChoice: Promise<unknown> };
  e.prompt = vi.fn(prompt);
  e.userChoice = Promise.resolve({ outcome: 'dismissed' });
  return e;
}

afterEach(() => {
  vi.restoreAllMocks();
  delete (window as unknown as Stash).__SOLU_INSTALL_PROMPT__;
});

describe('InstallButton on iOS', () => {
  for (const [browser, ua] of Object.entries(UAS)) {
    it(`${browser} on iPhone: Add to Home Screen opens its explanation on tap`, () => {
      vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(ua);
      render(<InstallButton />);
      expect(screen.queryByRole('button', { name: /Install/ })).toBeNull();
      const pill = screen.getByRole('button', { name: 'Add to Home Screen' });
      expect(pill).not.toHaveAttribute('title');
      expect(screen.queryByRole('dialog')).toBeNull();
      fireEvent.click(pill);
      const help = screen.getByRole('dialog', { name: 'Add SoluTerminal to your Home Screen' });
      expect(help).toHaveTextContent("Tap your browser's Share button (in Safari it can be under ⋯), then Add to Home Screen.");
      expect(help).toHaveTextContent('If you see Open as Web App, leave it on, then tap Add.');
      expect(help).toHaveTextContent('Then open SoluTerminal from its Home Screen icon.');
      expect(pill).toHaveAttribute('aria-expanded', 'true');
    });
  }

  it('hidden once opened from the Home Screen', () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(UAS.chrome);
    (navigator as unknown as { standalone?: boolean }).standalone = true;
    try {
      const { container } = render(<InstallButton />);
      expect(container).toBeEmptyDOMElement();
    } finally {
      delete (navigator as unknown as { standalone?: boolean }).standalone;
    }
  });

});

// F-011: a tap on Install never does nothing. F-012: hidden once installed.
describe('InstallButton on Android and desktop', () => {
  it('without a prompt, a tap shows the browser menu steps', () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(ANDROID_CHROME);
    render(<InstallButton />);
    expect(screen.queryByRole('button', { name: 'Add to Home Screen' })).toBeNull();
    expect(screen.queryByRole('dialog')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: INSTALL_LABEL }));
    const help = screen.getByRole('dialog', { name: INSTALL_HELP_TITLE });
    expect(help).toHaveTextContent(INSTALL_STEPS_ANDROID);
    expect(help).toHaveTextContent(INSTALL_THEN);
  });

  it('names the menu for Firefox and the address bar on a desktop browser', () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(ANDROID_FIREFOX);
    const { unmount } = render(<InstallButton />);
    fireEvent.click(screen.getByRole('button', { name: INSTALL_LABEL }));
    expect(screen.getByRole('dialog', { name: INSTALL_HELP_TITLE })).toHaveTextContent(INSTALL_STEPS_FIREFOX);
    unmount();
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(DESKTOP_EDGE);
    render(<InstallButton />);
    fireEvent.click(screen.getByRole('button', { name: INSTALL_LABEL }));
    expect(screen.getByRole('dialog', { name: INSTALL_HELP_TITLE })).toHaveTextContent(INSTALL_STEPS_DESKTOP);
  });

  it("with a prompt, a tap opens the browser's install dialog once", async () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(ANDROID_CHROME);
    const e = installPrompt(() => Promise.resolve());
    (window as unknown as Stash).__SOLU_INSTALL_PROMPT__ = e;
    render(<InstallButton />);
    fireEvent.click(screen.getByRole('button', { name: INSTALL_LABEL }));
    await waitFor(() => expect(e.prompt).toHaveBeenCalledTimes(1));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect((window as unknown as Stash).__SOLU_INSTALL_PROMPT__).toBeUndefined();
    // The prompt is spent: the next tap shows the steps instead.
    fireEvent.click(screen.getByRole('button', { name: INSTALL_LABEL }));
    expect(screen.getByRole('dialog', { name: INSTALL_HELP_TITLE })).toBeInTheDocument();
    expect(e.prompt).toHaveBeenCalledTimes(1);
  });

  it('a prompt that was already used falls back to the steps on the same tap', async () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(ANDROID_CHROME);
    const e = installPrompt(() => Promise.reject(new DOMException('already shown', 'InvalidStateError')));
    (window as unknown as Stash).__SOLU_INSTALL_PROMPT__ = e;
    render(<InstallButton />);
    fireEvent.click(screen.getByRole('button', { name: INSTALL_LABEL }));
    expect(await screen.findByRole('dialog', { name: INSTALL_HELP_TITLE })).toHaveTextContent(INSTALL_STEPS_ANDROID);
    expect(e.prompt).toHaveBeenCalledTimes(1);
  });

  it('hidden in an installed app window', () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(ANDROID_CHROME);
    const original = window.matchMedia;
    window.matchMedia = vi.fn((q: string) => ({ matches: q === '(display-mode: standalone)' }) as MediaQueryList);
    try {
      const { container } = render(<InstallButton />);
      expect(container).toBeEmptyDOMElement();
    } finally {
      window.matchMedia = original;
    }
  });

  it('hides once the app is installed', () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(ANDROID_CHROME);
    const { container } = render(<InstallButton />);
    expect(screen.getByRole('button', { name: INSTALL_LABEL })).toBeInTheDocument();
    act(() => { window.dispatchEvent(new Event('appinstalled')); });
    expect(container).toBeEmptyDOMElement();
  });
});
