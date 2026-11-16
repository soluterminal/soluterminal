import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../api';
import { useDeveloperOverrides } from '../../lib/developerOverrides';
import { DeveloperOverridesChip } from '../DeveloperOverridesChip';
import { DEVELOPER_OVERRIDES_LABEL, DeveloperOverridesSetting } from './DeveloperOverridesSetting';

beforeEach(() => useDeveloperOverrides.setState({ loaded: false, on: false, required: false }));
afterEach(() => vi.restoreAllMocks());

describe('DeveloperOverridesSetting', () => {
  it('stays hidden on a daemon without the endpoint', async () => {
    const get = vi.spyOn(api, 'getDeveloperOverrides').mockResolvedValue(null);
    render(<DeveloperOverridesSetting />);
    await waitFor(() => expect(get).toHaveBeenCalled());
    expect(screen.queryByText(DEVELOPER_OVERRIDES_LABEL, { selector: 'label, span, div' })).toBeNull();
  });

  it('is off by default, turns on, and the status bar then shows it', async () => {
    vi.spyOn(api, 'getDeveloperOverrides').mockResolvedValue({ on: false, required: true });
    const set = vi.spyOn(api, 'setDeveloperOverrides').mockResolvedValue({ on: true, required: true, by: 'settings' });
    render(
      <>
        <DeveloperOverridesSetting />
        <DeveloperOverridesChip />
      </>,
    );
    const select = await screen.findByRole('combobox');
    expect(select).toHaveValue('off');
    expect(screen.queryByText('Developer overrides on')).toBeNull();
    fireEvent.change(select, { target: { value: 'on' } });
    await waitFor(() => expect(set).toHaveBeenCalledWith(true));
    await waitFor(() => expect(select).toHaveValue('on'));
    expect(screen.getByText('Developer overrides on')).toBeInTheDocument();
    expect(screen.getByText('Developer overrides are on.')).toBeInTheDocument();
  });

  it('shows the refusal when the daemon refuses', async () => {
    vi.spyOn(api, 'getDeveloperOverrides').mockResolvedValue({ on: false, required: true });
    vi.spyOn(api, 'setDeveloperOverrides').mockRejectedValue(new Error('only at the computer itself'));
    render(<DeveloperOverridesSetting />);
    fireEvent.change(await screen.findByRole('combobox'), { target: { value: 'on' } });
    expect(await screen.findByText(/Couldn't save developer overrides/)).toBeInTheDocument();
  });

  it('the indicator follows a refresh after the daemon event', async () => {
    const get = vi.spyOn(api, 'getDeveloperOverrides').mockResolvedValue({ on: false, required: true });
    render(<DeveloperOverridesChip />);
    await waitFor(() => expect(get).toHaveBeenCalled());
    expect(screen.queryByText('Developer overrides on')).toBeNull();
    get.mockResolvedValue({ on: true, required: true, by: 'channel' });
    await act(async () => {
      await useDeveloperOverrides.getState().refresh();
    });
    expect(screen.getByText('Developer overrides on')).toBeInTheDocument();
  });
});
