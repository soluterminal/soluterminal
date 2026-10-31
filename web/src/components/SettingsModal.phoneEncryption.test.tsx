import { act, cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

const view = vi.hoisted(() => ({ mobile: true, candidate: null as string | null }));
vi.mock('./useIsMobileView', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./useIsMobileView')>()),
  useIsMobileDevice: () => view.mobile,
}));
vi.mock('./settings/RemoteDevicesSetting', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./settings/RemoteDevicesSetting')>()),
  privateRelayCandidateInView: () => view.candidate,
  PhoneEncryptionSetting: () => <div>phone encryption switch</div>,
}));
vi.mock('./settings/DevicesTab', () => ({ DevicesTab: () => <div>full devices tab</div> }));
vi.mock('./settings/PrivacySecurityTab', () => ({ PrivacySecurityTab: () => <div>full privacy tab</div> }));

import { useSettings } from '../settings';
import { SettingsModal } from './SettingsModal';

// A phone browser has none of the split Devices tabs, except Privacy &
// security holding its end-to-end encryption switch when that applies (the
// computer approved it and its key is pinned). Old 'devices' links land there.
describe('SettingsModal on a phone', () => {
  afterEach(() => {
    cleanup();
    useSettings.setState({ open: false });
  });

  const SPLIT_TABS = ['Account', 'Notifications & voice', 'Privacy & security', 'Devices & updates'];

  it('hides every split tab when encryption does not apply', () => {
    view.mobile = true;
    view.candidate = null;
    useSettings.setState({ open: true, tab: 'devices' });
    render(<SettingsModal />);
    for (const name of SPLIT_TABS) expect(screen.queryByRole('tab', { name })).toBeNull();
    expect(screen.queryByText('phone encryption switch')).toBeNull();
  });

  it('shows Privacy & security with only the encryption switch when it applies', () => {
    view.mobile = true;
    view.candidate = 'mach_1/wsl';
    useSettings.setState({ open: true, tab: 'devices' });
    render(<SettingsModal />);
    expect(screen.getByRole('tab', { name: 'Privacy & security' })).toHaveAttribute('aria-selected', 'true');
    for (const name of ['Account', 'Notifications & voice', 'Devices & updates']) {
      expect(screen.queryByRole('tab', { name })).toBeNull();
    }
    expect(screen.getByText('phone encryption switch')).toBeInTheDocument();
    expect(screen.queryByText('full devices tab')).toBeNull();
    expect(screen.queryByText('full privacy tab')).toBeNull();
  });

  it('keeps the full Devices & updates and Privacy & security tabs on a computer', () => {
    view.mobile = false;
    view.candidate = 'mach_1/wsl';
    useSettings.setState({ open: true, tab: 'devices' });
    render(<SettingsModal />);
    expect(screen.getByText('full devices tab')).toBeInTheDocument();
    expect(screen.queryByText('phone encryption switch')).toBeNull();
    act(() => useSettings.setState({ tab: 'privacy' }));
    expect(screen.getByText('full privacy tab')).toBeInTheDocument();
    expect(screen.queryByText('phone encryption switch')).toBeNull();
  });
});
