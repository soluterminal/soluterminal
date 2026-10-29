import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// The tab must reflect the REAL pairing state — a paired user seeing "Set up"
// reads as "not connected" (the original bug).
const mocks = vi.hoisted(() => ({
  desktop: vi.fn(() => true),
  status: vi.fn(() => ({ status: null as unknown, error: null, refresh: () => {} })),
  getAutostartStatus: vi.fn(),
  setAutostart: vi.fn(),
}));
vi.mock('../../cloudPair', async (importOriginal) => {
  const real = await importOriginal<typeof import('../../cloudPair')>();
  return {
    ...real,
    isDesktopShell: mocks.desktop,
    usePhoneAccessStatus: mocks.status,
  };
});
vi.mock('../../tauri-browser', async (importOriginal) => {
  const real = await importOriginal<typeof import('../../tauri-browser')>();
  return {
    ...real,
    getAutostartStatus: mocks.getAutostartStatus,
    setAutostart: mocks.setAutostart,
  };
});

import { DevicesTab } from './DevicesTab';
import { CHECKIN_HINT } from './CheckinSetting';
import { useEntitlements } from '../../entitlements';
import { useCloud } from '../../cloud';
import { api } from '../../api';
import { useStore } from '../../store';
import type { UpdateStatus } from '../../types';

beforeEach(() => {
  useEntitlements.setState({ loaded: true, team: true, operator: true, account: true });
  useCloud.setState({ session: null, withoutAccount: false });
  vi.spyOn(api, 'getCheckinSettings').mockResolvedValue(null);
  vi.spyOn(api, 'getPrivacySettings').mockResolvedValue(null);
  vi.spyOn(api, 'getAgentAuthority').mockResolvedValue(null);
  vi.spyOn(api, 'listRemoteDevices').mockResolvedValue(null);
  mocks.desktop.mockReturnValue(true);
  mocks.status.mockReturnValue({ status: null, error: null, refresh: () => {} });
  mocks.getAutostartStatus.mockReset().mockResolvedValue(null);
  mocks.setAutostart.mockReset();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('DevicesTab phone access status', () => {
  it('shows Connected + owner email and a Manage button when paired', () => {
    mocks.status.mockReturnValue({
      status: { configured: true, hasCreds: true, credsInvalid: false, claimCorrupt: false, relayState: 'online', ownerEmail: 'a@b.c' },
      error: null,
      refresh: () => {},
    });
    render(<DevicesTab />);
    expect(screen.getByText('Connected — a@b.c')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Manage' })).toBeInTheDocument();
  });

  it('shows Set up while unpaired', () => {
    mocks.status.mockReturnValue({ status: null, error: null, refresh: () => {} });
    render(<DevicesTab />);
    expect(screen.getByText('Not set up')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Set up' })).toBeInTheDocument();
  });

  it('shows the installer link without pairing outside the desktop shell', () => {
    mocks.desktop.mockReturnValue(false);
    render(<DevicesTab />);
    expect(screen.getByText('Desktop app')).toBeInTheDocument();
    expect(screen.queryByText('Phone access')).not.toBeInTheDocument();
  });

  // The download must ALSO be reachable in-shell (installer for another
  // machine / manual reinstall) — it used to be hidden there.
  it('offers the installer download inside the desktop shell too', () => {
    mocks.desktop.mockReturnValue(true);
    mocks.status.mockReturnValue({ status: null, error: null, refresh: () => {} });
    render(<DevicesTab />);
    expect(screen.getByText('Desktop app')).toBeInTheDocument();
    expect(screen.getByText('Phone access')).toBeInTheDocument();
  });
});

describe('DevicesTab launch on startup', () => {
  it('shows an unchecked toggle when Windows autostart is supported and disabled', async () => {
    mocks.getAutostartStatus.mockResolvedValue({ supported: true, enabled: false });
    render(<DevicesTab />);

    expect(await screen.findByLabelText('Launch on startup')).not.toBeChecked();
  });

  it('enables launch on startup through the desktop command', async () => {
    mocks.getAutostartStatus.mockResolvedValue({ supported: true, enabled: false });
    mocks.setAutostart.mockResolvedValue({ supported: true, enabled: true });
    render(<DevicesTab />);

    const checkbox = await screen.findByLabelText('Launch on startup');
    fireEvent.click(checkbox);

    await waitFor(() => expect(mocks.setAutostart).toHaveBeenCalledWith(true));
    await waitFor(() => expect(checkbox).toBeChecked());
  });

  it('reverts the toggle and surfaces an inline error when registration fails', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    mocks.getAutostartStatus.mockResolvedValue({ supported: true, enabled: false });
    mocks.setAutostart.mockRejectedValue(new Error('registry unavailable'));
    const { container } = render(<DevicesTab />);

    const checkbox = await screen.findByLabelText('Launch on startup');
    fireEvent.click(checkbox);

    await waitFor(() => expect(checkbox).not.toBeChecked());
    expect(container.querySelector('.settings-error')).toHaveTextContent('registry unavailable');
    warn.mockRestore();
  });

  it('serializes rapid clicks while a registry update is pending', async () => {
    let finishUpdate!: (status: { supported: boolean; enabled: boolean }) => void;
    const pendingUpdate = new Promise<{ supported: boolean; enabled: boolean }>((resolve) => {
      finishUpdate = resolve;
    });
    mocks.getAutostartStatus.mockResolvedValue({ supported: true, enabled: false });
    mocks.setAutostart.mockReturnValue(pendingUpdate);
    render(<DevicesTab />);

    const checkbox = await screen.findByLabelText('Launch on startup');
    fireEvent.click(checkbox);
    await waitFor(() => expect(mocks.setAutostart).toHaveBeenCalledTimes(1));
    expect(checkbox).toBeDisabled();

    fireEvent.click(checkbox);
    expect(mocks.setAutostart).toHaveBeenCalledTimes(1);

    finishUpdate({ supported: true, enabled: true });
    await waitFor(() => expect(checkbox).toBeEnabled());
    expect(checkbox).toBeChecked();
  });

  it('hides the toggle outside a compatible desktop shell', async () => {
    mocks.getAutostartStatus.mockResolvedValue(null);
    render(<DevicesTab />);

    await waitFor(() => expect(mocks.getAutostartStatus).toHaveBeenCalled());
    expect(screen.queryByText('Launch on startup')).not.toBeInTheDocument();
  });

  it('hides the toggle on unsupported desktop platforms', async () => {
    mocks.getAutostartStatus.mockResolvedValue({ supported: false, enabled: false });
    render(<DevicesTab />);

    await waitFor(() => expect(mocks.getAutostartStatus).toHaveBeenCalled());
    expect(screen.queryByText('Launch on startup')).not.toBeInTheDocument();
  });
});

describe('DevicesTab install command', () => {
  it('offers no install one-liner to a no-account install on Linux', () => {
    vi.spyOn(navigator, 'platform', 'get').mockReturnValue('Linux x86_64');
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
    render(<DevicesTab />);
    expect(screen.queryByText('Desktop app')).toBeNull();
    expect(document.body.textContent).not.toMatch(/install\.sh/);
    vi.restoreAllMocks();
  });

  it('keeps the install one-liner for a team install', () => {
    vi.spyOn(navigator, 'platform', 'get').mockReturnValue('Linux x86_64');
    useEntitlements.setState({ loaded: true, team: true, operator: true });
    render(<DevicesTab />);
    expect(screen.getByText('Desktop app')).toBeInTheDocument();
    vi.restoreAllMocks();
  });
});

describe('DevicesTab account tiers', () => {
  it('no account: hides phone access and offers sign-in for the phone', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: false });
    useCloud.setState({ withoutAccount: true, session: null });
    const requestSignIn = vi.fn();
    useCloud.setState({ requestSignIn });
    render(<DevicesTab />);
    expect(screen.queryByText('Phone access')).toBeNull();
    expect(screen.getByText('Not signed in')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Create free account or sign in' }));
    expect(requestSignIn).toHaveBeenCalled();
  });

  it('signed in: shows who and offers sign out', () => {
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: true });
    useCloud.setState({ withoutAccount: false, session: { user: { email: 'pat@example.com' } } as never });
    render(<DevicesTab />);
    expect(screen.getByText('Signed in as pat@example.com')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Sign out' })).toBeInTheDocument();
    useCloud.setState({ session: null });
  });

  it('free account: shows phone access and the install one-liner', () => {
    vi.spyOn(navigator, 'platform', 'get').mockReturnValue('Linux x86_64');
    useEntitlements.setState({ loaded: true, team: false, operator: false, account: true });
    render(<DevicesTab />);
    expect(screen.getByText('Phone access')).toBeInTheDocument();
    expect(screen.getByText('Desktop app')).toBeInTheDocument();
    expect(screen.queryByText('Not signed in')).toBeNull();
  });
});

describe('DevicesTab anonymous check-in', () => {
  it('stays hidden on a daemon without the endpoint', async () => {
    render(<DevicesTab />);
    await waitFor(() => expect(api.getCheckinSettings).toHaveBeenCalled());
    expect(screen.queryByText(/anonymous daily check-in/i)).toBeNull();
  });

  it('shows the daemon preference and saves a change', async () => {
    vi.mocked(api.getCheckinSettings).mockResolvedValue({ enabled: true });
    const put = vi.spyOn(api, 'setCheckinSettings').mockResolvedValue({ enabled: false });
    render(<DevicesTab />);
    const box = await screen.findByLabelText('Send an anonymous daily check-in');
    expect(box).toBeChecked();
    expect(
      screen.getByText(CHECKIN_HINT),
    ).toBeInTheDocument();
    fireEvent.click(box);
    await waitFor(() => expect(put).toHaveBeenCalledWith(false));
    await waitFor(() => expect(box).not.toBeChecked());
  });
});

describe('DevicesTab usage statistics and crash reports', () => {
  const privacy = {
    usageUpload: false,
    usageUploadEnvDisabled: false,
    crashReports: true,
    crashReportsEnvDisabled: false,
  };

  it('shows usage off and crash reports on, with a privacy link', async () => {
    vi.mocked(api.getPrivacySettings).mockResolvedValue(privacy);
    render(<DevicesTab />);
    expect(await screen.findByLabelText('Share usage statistics')).not.toBeChecked();
    expect(screen.getByLabelText('Send crash reports')).toBeChecked();
    expect(screen.getByRole('link', { name: 'Privacy policy' })).toHaveAttribute(
      'href',
      'https://soluterminal.com/privacy/',
    );
  });

  it('saves a usage opt-in to the daemon', async () => {
    vi.mocked(api.getPrivacySettings).mockResolvedValue(privacy);
    const put = vi
      .spyOn(api, 'setPrivacySettings')
      .mockResolvedValue({ ...privacy, usageUpload: true });
    render(<DevicesTab />);
    const box = await screen.findByLabelText('Share usage statistics');
    fireEvent.click(box);
    await waitFor(() => expect(put).toHaveBeenCalledWith({ usageUpload: true }));
    await waitFor(() => expect(box).toBeChecked());
  });

  it('rolls back and explains when saving fails', async () => {
    vi.mocked(api.getPrivacySettings).mockResolvedValue(privacy);
    vi.spyOn(api, 'setPrivacySettings').mockRejectedValue(new Error('403'));
    render(<DevicesTab />);
    const box = await screen.findByLabelText('Send crash reports');
    fireEvent.click(box);
    expect(await screen.findByText(/Couldn't save the crash report setting/)).toBeInTheDocument();
    expect(box).toBeChecked();
  });
});

// On public the channel default pauses the unattended installer restart, so
// "Restart when idle" would do nothing there: the row hides, and the saved
// choice stays as it is. Only an explicit pause hides it.
describe('DevicesTab desktop app restarts', () => {
  const initial = useStore.getState();
  const status = (extra: Partial<UpdateStatus>): UpdateStatus => ({
    current: '0.0.1226', available: false, updateChannel: 'public', ...extra,
  });

  beforeEach(() => {
    useStore.setState({ updateSettings: { autoApply: 'off', shellAutoApply: 'when-idle' } });
  });

  afterEach(() => {
    useStore.setState({ updateStatus: initial.updateStatus, updateSettings: initial.updateSettings });
  });

  it('hides the row and leaves the saved choice alone while the channel pauses it', () => {
    const setShell = vi.fn(async () => {});
    useStore.setState({
      setUpdateShellAutoApply: setShell,
      updateStatus: status({
        shellAutoApplyPaused: true,
        shellAutoApplyPausedReason: 'automatic desktop restarts are not enabled for this channel yet',
      }),
    });
    render(<DevicesTab />);
    expect(screen.getByText('Automatic updates')).toBeInTheDocument();
    expect(screen.queryByText('Desktop app restarts')).not.toBeInTheDocument();
    expect(screen.queryByRole('option', { name: 'Restart when idle' })).not.toBeInTheDocument();
    expect(setShell).not.toHaveBeenCalled();
    expect(useStore.getState().updateSettings?.shellAutoApply).toBe('when-idle');
    useStore.setState({ setUpdateShellAutoApply: initial.setUpdateShellAutoApply });
  });

  it('shows the row with the saved choice when the channel allows it', () => {
    useStore.setState({ updateStatus: status({ updateChannel: 'team', shellAutoApplyPaused: false }) });
    render(<DevicesTab />);
    expect(screen.getByText('Desktop app restarts')).toBeInTheDocument();
    expect(screen.getByRole('option', { name: 'Restart when idle' })).toBeInTheDocument();
    expect(screen.getByDisplayValue('Restart when idle')).toBeInTheDocument();
  });

  it('shows the row as before when an older daemon leaves the field out', () => {
    useStore.setState({ updateStatus: status({}) });
    render(<DevicesTab />);
    expect(screen.getByText('Desktop app restarts')).toBeInTheDocument();
    expect(screen.getByDisplayValue('Restart when idle')).toBeInTheDocument();
  });
});
