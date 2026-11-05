import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../api';
import { AlertsHandlerSetting, TAKE_ALERTS_BACK } from './AlertsHandlerSetting';

afterEach(() => vi.restoreAllMocks());

describe('AlertsHandlerSetting', () => {
  it('is hidden while no app holds the alerts', async () => {
    const get = vi.spyOn(api, 'getNotificationsHandler').mockResolvedValue({ active: false });
    const { container } = render(<AlertsHandlerSetting />);
    await waitFor(() => expect(get).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });

  it('names the holder and its device, and takes alerts back', async () => {
    const get = vi.spyOn(api, 'getNotificationsHandler')
      .mockResolvedValueOnce({ active: true, client: 'Desktop alerts', deviceName: 'Work PC window', expiresAtMs: Date.now() + 600_000 })
      .mockResolvedValue({ active: false });
    const release = vi.spyOn(api, 'releaseNotificationsHandler').mockResolvedValue({ released: true });
    render(<AlertsHandlerSetting />);
    expect(await screen.findByText('Alerts handled by Desktop alerts')).toBeInTheDocument();
    expect(screen.getByText(/an app on the approved device “Work PC window” until/)).toBeInTheDocument();
    expect(screen.getByText(/Device and security notices always come from this computer/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: TAKE_ALERTS_BACK }));
    await waitFor(() => expect(release).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.queryByRole('button', { name: TAKE_ALERTS_BACK })).not.toBeInTheDocument());
    expect(get).toHaveBeenCalledTimes(2);
  });

  it('shows the refusal when taking alerts back fails', async () => {
    vi.spyOn(api, 'getNotificationsHandler').mockResolvedValue({ active: true, deviceName: 'Phone', expiresAtMs: Date.now() + 60_000 });
    vi.spyOn(api, 'releaseNotificationsHandler').mockRejectedValue(new Error('Only you can take alerts back.'));
    render(<AlertsHandlerSetting />);
    expect(await screen.findByText('Alerts handled by Phone')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: TAKE_ALERTS_BACK }));
    expect(await screen.findByText(/Couldn't take alerts back: Error: Only you can take alerts back./)).toBeInTheDocument();
  });
});
