import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { InstallButton } from './InstallButton';

vi.mock('./useIsMobileView', () => ({ useIsMobileView: () => true }));

const UAS = {
  safari: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1',
  chrome: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/130.0.6723.90 Mobile/15E148 Safari/604.1',
  firefox: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/132.0 Mobile/15E148 Safari/605.1.15',
  edge: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 EdgiOS/130.0.2849.80 Mobile/15E148 Safari/605.1.15',
};
const ANDROID_CHROME = 'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36';

afterEach(() => vi.restoreAllMocks());

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

  it('Android Chrome keeps the Chrome-style install diagnostics', () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(ANDROID_CHROME);
    render(<InstallButton />);
    expect(screen.queryByRole('button', { name: 'Add to Home Screen' })).toBeNull();
    expect(screen.getByRole('button', { name: /⤓/ })).toBeInTheDocument();
  });
});
