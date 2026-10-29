import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import type { Session } from '@supabase/supabase-js';
import { useCloud } from '../cloud';
import { CloudGate } from './CloudGate';

function setDevice({
  width,
  height,
  coarse,
  touchPoints,
}: {
  width: number;
  height: number;
  coarse: boolean;
  touchPoints: number;
}) {
  Object.defineProperty(window, 'innerWidth', { configurable: true, value: width });
  Object.defineProperty(window, 'innerHeight', { configurable: true, value: height });
  Object.defineProperty(window, 'visualViewport', {
    configurable: true,
    value: {
      width,
      height,
      offsetTop: 0,
      offsetLeft: 0,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    },
  });
  Object.defineProperty(navigator, 'maxTouchPoints', { configurable: true, value: touchPoints });
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    value: vi.fn((query: string) => ({
      matches: query === '(pointer: coarse)' ? coarse : false,
      media: query,
      onchange: null,
      addListener: vi.fn(),
      removeListener: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  });
}

function seedMachinePickerState() {
  useCloud.setState({
    mode: 'cloud',
    isLocal: false,
    session: { access_token: 'token', user: { email: 'user@example.com' } } as Session,
    authorised: true,
    machines: [],
    machinesLoaded: true,
    selectedMachineId: null,
    error: null,
    bootstrap: vi.fn(async () => {}),
    signOut: vi.fn(async () => {}),
  });
}

describe('CloudGate mobile machine picker', () => {
  beforeEach(() => {
    useCloud.setState(useCloud.getInitialState(), true);
  });

  afterEach(() => {
    vi.restoreAllMocks();
    useCloud.setState(useCloud.getInitialState(), true);
  });

  it('does not offer a Windows desktop app download on phones', () => {
    setDevice({ width: 390, height: 844, coarse: true, touchPoints: 5 });
    seedMachinePickerState();

    render(
      <CloudGate>
        <div>app</div>
      </CloudGate>,
    );

    expect(screen.getByText(/there's nothing to install on your phone\./i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Share link' })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /download for windows/i })).toBeNull();
    expect(screen.queryByText(/download desktop app/i)).toBeNull();
    expect(document.body.textContent).not.toMatch(/curl|install\.sh|github/i);
  });

  it('keeps the desktop installer link available on desktop', () => {
    setDevice({ width: 1280, height: 900, coarse: false, touchPoints: 0 });
    seedMachinePickerState();
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue('Mozilla/5.0 (Windows NT 10.0; Win64; x64)');
    vi.spyOn(navigator, 'platform', 'get').mockReturnValue('Win32');

    render(
      <CloudGate>
        <div>app</div>
      </CloudGate>,
    );

    const link = screen.getByRole('link', { name: /download for windows/i });
    expect(link).toHaveAttribute('href', 'https://soluterminal.com/download/SoluTerminal-Setup.exe');
  });

  it('renders the access denied gate with sign-out still available', () => {
    const signOut = vi.fn(async () => {});
    useCloud.setState({
      mode: 'cloud',
      isLocal: false,
      session: { access_token: 'token', user: { email: 'user@example.com' } } as Session,
      authorised: false,
      selectedMachineId: null,
      error: null,
      bootstrap: vi.fn(async () => {}),
      signOut,
    });

    render(
      <CloudGate>
        <div>app</div>
      </CloudGate>,
    );

    expect(screen.getByRole('heading', { name: /account not ready/i })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /sign out/i }));
    expect(signOut).toHaveBeenCalledTimes(1);
  });
});
