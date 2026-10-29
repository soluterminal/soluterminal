import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AlertsMenu } from './AlertsMenu';
import { useEntitlements } from '../entitlements';
import { useCloud } from '../cloud';
import { useStore } from '../store';
import { alertDialog } from '../lib/dialog';

vi.mock('../lib/dialog', () => ({ alertDialog: vi.fn(async () => {}) }));

const IPHONE_UA =
  'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1';

function openMenu() {
  render(<AlertsMenu />);
  fireEvent.click(screen.getByRole('button', { name: /alerts/i }));
}

beforeEach(() => {
  useCloud.setState({ session: null, withoutAccount: false });
  useStore.setState({ notifyEnabled: true });
});

afterEach(() => {
  vi.restoreAllMocks();
  useEntitlements.setState({ loaded: false, team: false, operator: false, account: false });
  useStore.setState({ notifyEnabled: false });
});

describe('AlertsMenu push by tier', () => {
  it('no account: keeps the bell but offers no phone push test or iPhone hint', () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(IPHONE_UA);
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
    openMenu();
    expect(screen.getByText('Notifications')).toBeInTheDocument();
    expect(screen.queryByText('Test & setup')).toBeNull();
    expect(screen.queryByText(/add SoluTerminal to your Home Screen/)).toBeNull();
  });

  it('free account: offers the push test', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: true });
    openMenu();
    expect(screen.getByText('Test & setup')).toBeInTheDocument();
  });

  it('free account on an iPhone browser tab: explains the Home Screen requirement', () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(IPHONE_UA);
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: true });
    openMenu();
    expect(
      screen.getByText('On iPhone, add SoluTerminal to your Home Screen to get push alerts.'),
    ).toBeInTheDocument();
  });

  it('no iPhone hint once launched from the Home Screen', () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(IPHONE_UA);
    (navigator as unknown as { standalone?: boolean }).standalone = true;
    try {
      useEntitlements.setState({ loaded: true, team: true, operator: true, account: true });
      openMenu();
      expect(screen.queryByText(/add SoluTerminal to your Home Screen/)).toBeNull();
    } finally {
      delete (navigator as unknown as { standalone?: boolean }).standalone;
    }
  });
});

const MAC_SAFARI_UA =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15';

describe('AlertsMenu when turning notifications on fails', () => {
  async function turnOn() {
    useStore.setState({ notifyEnabled: false, setNotifyEnabled: async () => false });
    openMenu();
    fireEvent.click(screen.getByRole('button', { name: 'Off' }));
    await vi.waitFor(() => expect(alertDialog).toHaveBeenCalled());
    return vi.mocked(alertDialog).mock.calls.at(-1)![0] as string;
  }
  const initialSetNotify = useStore.getState().setNotifyEnabled;
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.mocked(alertDialog).mockClear();
    useStore.setState({ setNotifyEnabled: initialSetNotify });
    delete (navigator as unknown as { standalone?: boolean }).standalone;
  });

  it('iPhone browser tab: tells the user to add to the Home Screen and turn alerts on there', async () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(IPHONE_UA);
    vi.stubGlobal('Notification', undefined);
    const message = await turnOn();
    expect(message).toContain('notifications work only when SoluTerminal is opened from the Home Screen');
    expect(message).toContain('Share button');
    expect(message).toContain('Add to Home Screen');
    expect(message).toContain('Open SoluTerminal from its new Home Screen icon.');
    expect(message).toContain('Open Alerts there and turn Notifications on.');
    expect(message).not.toContain('does not support notifications');
  });

  it('other browsers without notifications keep the generic message', async () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(MAC_SAFARI_UA);
    vi.stubGlobal('Notification', undefined);
    expect(await turnOn()).toBe('This browser does not support notifications.\n\nIn-app toasts still work.');
  });

  it('iPhone Home Screen app with notifications blocked: points at Settings → Notifications → SoluTerminal', async () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(IPHONE_UA);
    (navigator as unknown as { standalone?: boolean }).standalone = true;
    vi.stubGlobal('Notification', { permission: 'denied' });
    const message = await turnOn();
    expect(message).toContain('Settings → Notifications → SoluTerminal (below Notification Style) → turn on Allow Notifications');
    expect(message).not.toContain('Settings → Websites');
  });

  it('desktop blocked help names the Mac path for Safari and the iPhone path separately', async () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(MAC_SAFARI_UA);
    vi.stubGlobal('Notification', { permission: 'denied' });
    const message = await turnOn();
    expect(message).toContain('• Safari on Mac: Safari → Settings → Websites → Notifications → set this site to Allow');
    expect(message).toContain('• iPhone / iPad (Home Screen app): Settings → Notifications → SoluTerminal');
    expect(message).not.toContain('Safari (iOS 16.4+): Settings → Websites');
  });
});
