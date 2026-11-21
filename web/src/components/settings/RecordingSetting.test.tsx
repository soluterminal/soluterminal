import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError } from '../../api';
import { useCloud } from '../../cloud';
import { useStore } from '../../store';
import type { Health, RecordingSettings } from '../../types';
import { RecordingSetting } from './RecordingSetting';

// ST-317: deletion age and cap are daemon settings within bounds; an older
// daemon (no pane-recording.v2) shows no rows. A lowering at this computer
// deletes older recordings at once, so it asks first with the cleared text;
// an out-of-range entry is refused with its bounds, never pulled into them.

const bounds = { deleteAfterDaysMin: 1, deleteAfterDaysMax: 30, capMBMin: 500, capMBMax: 10000, hoursMin: 1, hoursMax: 24, hoursDefault: 8 };
const settings: RecordingSettings = { deleteAfterDays: 7, capMB: 2000, bounds };
const originalStore = useStore.getState();
const originalCloud = useCloud.getState();

type Dialog = { title?: string; message: string; confirmLabel?: string; cancelLabel?: string; resolve: (v: unknown) => void };
let dialogs: Dialog[] = [];
let answer = true;

beforeEach(() => {
  dialogs = [];
  answer = true;
  useStore.setState({ openDialog: (d: Dialog) => { dialogs.push(d); d.resolve(answer); } } as never);
  useCloud.setState({ mode: 'local', isLocal: true } as never);
  vi.spyOn(api, 'healthScoped').mockResolvedValue({ status: 'ok', features: ['pane-recording.v2', 'pane-recording-timeline.v1'] } as Health);
  vi.spyOn(api, 'getRecordingSettings').mockResolvedValue(settings);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  useStore.setState({ openDialog: originalStore.openDialog } as never);
  useCloud.setState({ mode: originalCloud.mode, isLocal: originalCloud.isLocal } as never);
});

async function edit(label: 'Days' | 'MB', value: string) {
  const field = screen.getByLabelText(label) as HTMLInputElement;
  fireEvent.change(field, { target: { value } });
  await act(async () => { fireEvent.blur(field); });
  return screen.getByLabelText(label) as HTMLInputElement;
}

async function mount() {
  render(<RecordingSetting />);
  await act(async () => {});
}

describe('RecordingSetting', () => {
  it('stays hidden on a daemon without the setting', async () => {
    vi.spyOn(api, 'healthScoped').mockResolvedValue({ status: 'ok', features: ['pane-recording.v1'] } as Health);
    const get = vi.spyOn(api, 'getRecordingSettings');
    get.mockClear();
    const { container } = render(<RecordingSetting />);
    await act(async () => {});
    expect(container.textContent).toBe('');
    expect(get).not.toHaveBeenCalled();
  });

  it('shows the values, the claims and the cleared sentence', async () => {
    await mount();
    expect(screen.getByText('Delete pane recordings after')).toBeInTheDocument();
    expect(screen.getByText(/never sent to the cloud/)).toBeInTheDocument();
    expect(screen.getByText('Lowering applies to recordings made from now on; to delete older ones sooner, lower it here at this computer.')).toBeInTheDocument();
  });

  it('asks before a lowering at this computer, with the cleared text, then saves', async () => {
    const put = vi.spyOn(api, 'putRecordingSettings').mockResolvedValue({ ...settings, deleteAfterDays: 3 });
    await mount();
    await edit('Days', '3');
    expect(dialogs).toHaveLength(1);
    expect(dialogs[0].title).toBe('Delete older recordings now?');
    expect(dialogs[0].message).toBe("Lowering this to 3 days deletes every recording last written more than 3 days ago, now. This can't be undone.");
    expect(dialogs[0].confirmLabel).toBe('Delete and save');
    expect(dialogs[0].cancelLabel).toBe('Cancel');
    expect(put).toHaveBeenCalledWith(expect.objectContaining({ deleteAfterDays: 3 }), expect.anything());
  });

  it('judges a lowering against the daemon now: a value raised elsewhere after Settings opened still asks', async () => {
    vi.spyOn(api, 'getRecordingSettings')
      .mockResolvedValueOnce(settings)
      .mockResolvedValue({ ...settings, deleteAfterDays: 14 });
    const put = vi.spyOn(api, 'putRecordingSettings').mockResolvedValue({ ...settings, deleteAfterDays: 10 });
    await mount();
    await edit('Days', '10');
    expect(dialogs).toHaveLength(1);
    expect(dialogs[0].message).toContain('Lowering this to 10 days');
    expect(put).toHaveBeenCalledWith(expect.objectContaining({ deleteAfterDays: 10 }), expect.anything());
  });

  it('reads and saves on the daemon whose values it shows', async () => {
    const get = vi.spyOn(api, 'getRecordingSettings');
    const put = vi.spyOn(api, 'putRecordingSettings').mockResolvedValue({ ...settings, deleteAfterDays: 14 });
    await mount();
    const shown = get.mock.calls[0][0];
    expect(shown).toBeDefined();
    await edit('Days', '14');
    expect(put.mock.calls[0][1]).toBe(shown);
    expect(get.mock.calls.every((c) => c[0] === shown)).toBe(true);
  });

  it('sends nothing when the values in force cannot be read', async () => {
    vi.spyOn(api, 'getRecordingSettings')
      .mockResolvedValueOnce(settings)
      .mockRejectedValueOnce(new Error('offline'))
      .mockResolvedValue(settings);
    const put = vi.spyOn(api, 'putRecordingSettings');
    await mount();
    await edit('Days', '3');
    expect(put).not.toHaveBeenCalled();
    expect(dialogs).toHaveLength(0);
  });

  it('sends the values it read, and on a 409 reads again and asks again', async () => {
    // Read 7 when deciding; another window raises to 14 before the save;
    // the daemon refuses; the page reads 14 and now asks before lowering.
    vi.spyOn(api, 'getRecordingSettings')
      .mockResolvedValueOnce(settings)
      .mockResolvedValueOnce(settings)
      .mockResolvedValue({ ...settings, deleteAfterDays: 14 });
    const put = vi.spyOn(api, 'putRecordingSettings')
      .mockRejectedValueOnce(new ApiError(409, 'the recording settings changed since they were read; read them again'))
      .mockResolvedValue({ ...settings, deleteAfterDays: 10 });
    await mount();
    await edit('Days', '10');
    expect(put).toHaveBeenCalledTimes(2);
    expect(put.mock.calls[0][0]).toEqual({ deleteAfterDays: 10, expectDeleteAfterDays: 7, expectCapMB: 2000 });
    expect(put.mock.calls[1][0]).toEqual({ deleteAfterDays: 10, expectDeleteAfterDays: 14, expectCapMB: 2000 });
    // 10 was a raise against 7 (no confirm) and a lowering against 14 (asked).
    expect(dialogs).toHaveLength(1);
    expect(dialogs[0].message).toContain('Lowering this to 10 days');
  });

  it('sends no expected values to a daemon without the timeline feature', async () => {
    vi.spyOn(api, 'healthScoped').mockResolvedValue({ status: 'ok', features: ['pane-recording.v2'] } as Health);
    const put = vi.spyOn(api, 'putRecordingSettings').mockResolvedValue({ ...settings, deleteAfterDays: 14 });
    await mount();
    await edit('Days', '14');
    expect(put.mock.calls[0][0]).toEqual({ deleteAfterDays: 14 });
  });

  it('names the space in the cap confirm', async () => {
    vi.spyOn(api, 'putRecordingSettings').mockResolvedValue({ ...settings, capMB: 1500 });
    await mount();
    await edit('MB', '1500');
    expect(dialogs[0].message).toBe("Lowering this to 1500 MB deletes the oldest recordings until the rest fit, now. This can't be undone.");
  });

  it('Cancel restores the value in force and sends nothing', async () => {
    answer = false;
    const put = vi.spyOn(api, 'putRecordingSettings');
    await mount();
    const field = await edit('Days', '3');
    expect(dialogs).toHaveLength(1);
    expect(put).not.toHaveBeenCalled();
    expect(field.value).toBe('7');
  });

  it('a raise asks nothing', async () => {
    const put = vi.spyOn(api, 'putRecordingSettings').mockResolvedValue({ ...settings, deleteAfterDays: 14 });
    await mount();
    await edit('Days', '14');
    expect(dialogs).toHaveLength(0);
    expect(put).toHaveBeenCalledWith(expect.objectContaining({ deleteAfterDays: 14 }), expect.anything());
  });

  it('reached through the cloud, a lowering deletes nothing now and asks nothing', async () => {
    useCloud.setState({ mode: 'cloud', isLocal: false } as never);
    const put = vi.spyOn(api, 'putRecordingSettings').mockResolvedValue({ ...settings, deleteAfterDays: 3 });
    await mount();
    await edit('Days', '3');
    expect(dialogs).toHaveLength(0);
    expect(put).toHaveBeenCalledWith(expect.objectContaining({ deleteAfterDays: 3 }), expect.anything());
  });

  it('refuses an out-of-range entry with its bounds and sends nothing', async () => {
    const put = vi.spyOn(api, 'putRecordingSettings');
    await mount();
    for (const [label, value, range] of [['Days', '0', '1 to 30 days'], ['Days', '50', '1 to 30 days'], ['Days', '2.5', '1 to 30 days'], ['MB', '100', '500 to 10000 MB'], ['MB', 'lots', '500 to 10000 MB']] as const) {
      const field = await edit(label, value);
      expect(screen.getByText(new RegExp(`Enter a whole number from ${range}\\. Nothing was saved\\.`))).toBeInTheDocument();
      expect(field.value).toBe(label === 'Days' ? '7' : '2000');
    }
    expect(put).not.toHaveBeenCalled();
    expect(dialogs).toHaveLength(0);
  });

  it('keeps an exact cap: focusing and leaving the field saves nothing', async () => {
    vi.spyOn(api, 'getRecordingSettings').mockResolvedValue({ ...settings, capMB: 1234 });
    const put = vi.spyOn(api, 'putRecordingSettings');
    await mount();
    const mb = screen.getByLabelText('MB') as HTMLInputElement;
    expect(mb.value).toBe('1234');
    fireEvent.focus(mb);
    await act(async () => { fireEvent.blur(mb); });
    expect(put).not.toHaveBeenCalled();
  });

  it('holds both fields until a save settles, so no edit is dropped', async () => {
    let finish: (s: RecordingSettings) => void = () => {};
    vi.spyOn(api, 'putRecordingSettings').mockReturnValue(new Promise((r) => { finish = r; }));
    await mount();
    await edit('Days', '14');
    expect(screen.getByLabelText('MB')).toBeDisabled();
    await act(async () => { finish({ ...settings, deleteAfterDays: 14 }); });
    expect(screen.getByLabelText('MB')).toBeEnabled();
  });

  it("shows the daemon's refusal and the value in force", async () => {
    vi.spyOn(api, 'putRecordingSettings').mockRejectedValue(new Error('only the person at this computer can raise how long recordings are kept or their cap'));
    await mount();
    await edit('MB', '5000');
    expect(screen.getByRole('alert').textContent).toContain('only the person at this computer');
    expect((screen.getByLabelText('MB') as HTMLInputElement).value).toBe('2000');
  });
});
