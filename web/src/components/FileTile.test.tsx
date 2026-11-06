import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const apiMock = vi.hoisted(() => ({
  readFile: vi.fn(),
  writeFileLine: vi.fn(),
}));
const embed = vi.hoisted(() => ({ isEmbedded: vi.fn(() => true) }));
vi.mock('../embed', () => embed);
vi.mock('../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api')>();
  return { ...actual, api: { ...actual.api, ...apiMock } };
});

import { ApiError } from '../api';
import { useStore } from '../store';
import type { FileTileInfo, Layout } from '../types';
import { FileTile } from './FileTile';

const info: FileTileInfo = {
  id: 'file_1',
  path: '/tmp/a.ts',
  createdAt: '2026-08-21T00:00:00.000Z',
};
const splitLayout: Layout = {
  type: 'split',
  direction: 'horizontal',
  ratio: 0.5,
  a: { type: 'pane', paneId: 'p1' },
  b: { type: 'file', fileId: 'file_1' },
};

describe('FileTile zoom', () => {
  beforeEach(() => {
    apiMock.readFile.mockResolvedValue({
      path: info.path,
      lines: ['line one', 'line two'],
      mtime: 1,
      size: 20,
      isEnv: false,
      trailingNewline: true,
    });
    useStore.setState({
      files: { file_1: info },
      layout: splitLayout,
      focusedPaneId: 'file_1',
      zoomedPaneId: null,
    });
  });

  afterEach(() => {
    useStore.setState({ files: {}, layout: null, focusedPaneId: null, zoomedPaneId: null });
    vi.restoreAllMocks();
  });

  it('shows a maximise affordance when the layout has multiple leaves', async () => {
    render(<FileTile info={info} />);
    expect(screen.getByRole('button', { name: 'Maximise pane' })).toBeInTheDocument();
    await waitFor(() => expect(apiMock.readFile).toHaveBeenCalledWith(info.path));
  });

  it('maximises and restores the file tile', () => {
    render(<FileTile info={info} />);

    fireEvent.click(screen.getByRole('button', { name: 'Maximise pane' }));
    expect(useStore.getState().zoomedPaneId).toBe('file_1');
    fireEvent.click(screen.getByRole('button', { name: 'Restore pane' }));
    expect(useStore.getState().zoomedPaneId).toBeNull();
  });

  it('toggles zoom from empty toolbar space without hijacking the path editor', () => {
    const { container } = render(<FileTile info={info} />);
    const toolbar = container.querySelector('.file-toolbar');
    const filePath = container.querySelector('.file-path');
    if (!toolbar || !filePath) throw new Error('file toolbar did not render');

    fireEvent.doubleClick(toolbar);
    expect(useStore.getState().zoomedPaneId).toBe('file_1');
    fireEvent.doubleClick(filePath);
    expect(useStore.getState().zoomedPaneId).toBe('file_1');
    expect(container.querySelector('.file-path-input')).toBeInTheDocument();
  });

  it('omits the zoom affordance for a single file leaf', () => {
    useStore.setState({ layout: { type: 'file', fileId: 'file_1' } });
    render(<FileTile info={info} />);
    expect(screen.queryByRole('button', { name: 'Maximise pane' })).not.toBeInTheDocument();
  });

  it('clears stale zoom when the zoomed file closes', () => {
    useStore.setState({ zoomedPaneId: 'file_1' });
    useStore.getState().closeFileInLayout('file_1');
    expect(useStore.getState().zoomedPaneId).toBeNull();
  });

  it('does not zoom a single layout leaf', () => {
    useStore.setState({ layout: { type: 'file', fileId: 'file_1' }, zoomedPaneId: null });
    useStore.getState().toggleZoom('file_1');
    expect(useStore.getState().zoomedPaneId).toBeNull();
  });
});

describe('FileTile refused save', () => {
  it('toasts plain advice and keeps the typed text', async () => {
    apiMock.readFile.mockResolvedValue({
      path: info.path, lines: ['line one'], mtime: 1, size: 8, isEnv: false, trailingNewline: true,
    });
    const body = { caller: 'unverified', error: 'human_required', message: 'Agents must use the MCP tool write_file_line.' };
    apiMock.writeFileLine.mockRejectedValue(new ApiError(403, JSON.stringify(body), body));
    const toast = vi.fn();
    const realToast = useStore.getState().toast;
    useStore.setState({ files: { file_1: info }, layout: splitLayout, focusedPaneId: 'file_1', toast });
    const { container } = render(<FileTile info={info} />);
    fireEvent.click(await screen.findByTitle('Edit this line'));
    const input = container.querySelector('input.file-line-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'typed text' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(toast).toHaveBeenCalled());
    const [text] = toast.mock.calls[0];
    expect(text).toBe("Save failed: This screen isn't approved to save files yet. Open Settings → Privacy & security → Ask to approve this browser.");
    expect(text).not.toMatch(/\{|agent/i);
    expect((container.querySelector('input[value="typed text"]') as HTMLInputElement).value).toBe('typed text');
    useStore.setState({ toast: realToast });
  });

  it('keeps the refusal until dismissed or edited, and a dismiss-click blur does not re-save', async () => {
    apiMock.readFile.mockResolvedValue({
      path: info.path, lines: ['line one'], mtime: 1, size: 8, isEnv: false, trailingNewline: true,
    });
    const body = { caller: 'unverified', error: 'human_required', message: 'x' };
    apiMock.writeFileLine.mockReset();
    apiMock.writeFileLine.mockRejectedValue(new ApiError(403, JSON.stringify(body), body));
    useStore.setState({ files: { file_1: info }, layout: splitLayout, focusedPaneId: 'file_1', toasts: [] });
    const { container } = render(<FileTile info={info} />);
    fireEvent.click(await screen.findByTitle('Edit this line'));
    const input = container.querySelector('input.file-line-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'typed text' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(useStore.getState().toasts).toHaveLength(1));
    expect(useStore.getState().toasts[0].ttlMs).toBe(0);
    fireEvent.blur(input);
    await Promise.resolve();
    expect(apiMock.writeFileLine).toHaveBeenCalledTimes(1);
    expect(useStore.getState().toasts).toHaveLength(1);
    fireEvent.change(input, { target: { value: 'typed text!' } });
    expect(useStore.getState().toasts).toHaveLength(0);
  });

  it('dismisses the refusal when a retry succeeds, and a blur after dismissal saves', async () => {
    apiMock.readFile.mockResolvedValue({
      path: info.path, lines: ['line one'], mtime: 1, size: 8, isEnv: false, trailingNewline: true,
    });
    const body = { caller: 'unverified', error: 'human_required', message: 'x' };
    apiMock.writeFileLine.mockReset();
    apiMock.writeFileLine.mockRejectedValueOnce(new ApiError(403, JSON.stringify(body), body));
    useStore.setState({ files: { file_1: info }, layout: splitLayout, focusedPaneId: 'file_1', toasts: [] });
    const { container } = render(<FileTile info={info} />);
    fireEvent.click(await screen.findByTitle('Edit this line'));
    const input = container.querySelector('input.file-line-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'typed' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(useStore.getState().toasts).toHaveLength(1));
    // Approved meanwhile: the person dismisses the toast, then leaves the line.
    useStore.getState().dismissToast(useStore.getState().toasts[0].id);
    apiMock.writeFileLine.mockResolvedValueOnce({ mtime: 2 });
    fireEvent.blur(input);
    await waitFor(() => expect(apiMock.writeFileLine).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(useStore.getState().toasts.map((t) => t.message)).toEqual(['Saved']));
  });

  it('removes the refusal when a retry succeeds without it being dismissed', async () => {
    apiMock.readFile.mockResolvedValue({
      path: info.path, lines: ['line one'], mtime: 1, size: 8, isEnv: false, trailingNewline: true,
    });
    const body = { caller: 'unverified', error: 'human_required', message: 'x' };
    apiMock.writeFileLine.mockReset();
    apiMock.writeFileLine.mockRejectedValueOnce(new ApiError(403, JSON.stringify(body), body));
    apiMock.writeFileLine.mockResolvedValueOnce({ mtime: 2 });
    useStore.setState({ files: { file_1: info }, layout: splitLayout, focusedPaneId: 'file_1', toasts: [] });
    const { container } = render(<FileTile info={info} />);
    fireEvent.click(await screen.findByTitle('Edit this line'));
    const input = container.querySelector('input.file-line-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'typed' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(useStore.getState().toasts).toHaveLength(1));
    fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(useStore.getState().toasts.map((t) => t.message)).toEqual(['Saved']));
  });

  it('keeps a refused change when another line is chosen, and clears the refusal on Escape', async () => {
    apiMock.readFile.mockResolvedValue({
      path: info.path, lines: ['line one', 'line two'], mtime: 1, size: 16, isEnv: false, trailingNewline: true,
    });
    const body = { caller: 'unverified', error: 'human_required', message: 'x' };
    apiMock.writeFileLine.mockReset();
    apiMock.writeFileLine.mockRejectedValue(new ApiError(403, JSON.stringify(body), body));
    useStore.setState({ files: { file_1: info }, layout: splitLayout, focusedPaneId: 'file_1', toasts: [] });
    const { container } = render(<FileTile info={info} />);
    const pencils = await screen.findAllByTitle('Edit this line');
    fireEvent.click(pencils[0]);
    const input = container.querySelector('input.file-line-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'typed text' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(useStore.getState().toasts).toHaveLength(1));
    // As a person does it: the click blurs the input, then hits the pencil.
    fireEvent.blur(input);
    fireEvent.click(screen.getAllByTitle('Edit this line')[0]);
    expect((container.querySelector('input.file-line-input') as HTMLInputElement).value).toBe('typed text');
    expect(useStore.getState().toasts.some((t) => /isn't saved/.test(t.message))).toBe(true);
    expect(document.activeElement).toBe(input);
    fireEvent.keyDown(document.activeElement as Element, { key: 'Escape' });
    expect(useStore.getState().toasts.some((t) => /Save failed/.test(t.message))).toBe(false);
  });

  it('moves to another line without a warning or a write when the open line was not changed', async () => {
    apiMock.readFile.mockResolvedValue({
      path: info.path, lines: ['line one', 'line two'], mtime: 1, size: 16, isEnv: false, trailingNewline: true,
    });
    apiMock.writeFileLine.mockReset();
    useStore.setState({ files: { file_1: info }, layout: splitLayout, focusedPaneId: 'file_1', toasts: [] });
    const { container } = render(<FileTile info={info} />);
    fireEvent.click((await screen.findAllByTitle('Edit this line'))[0]);
    fireEvent.blur(container.querySelector('input.file-line-input') as HTMLInputElement);
    fireEvent.click(screen.getAllByTitle('Edit this line')[1]);
    expect(apiMock.writeFileLine).not.toHaveBeenCalled();
    expect((container.querySelector('input.file-line-input') as HTMLInputElement).value).toBe('line two');
    expect(useStore.getState().toasts).toHaveLength(0);
  });

  it('keeps the draft when another line is clicked while its save is still answering', async () => {
    apiMock.readFile.mockResolvedValue({
      path: info.path, lines: ['line one', 'line two'], mtime: 1, size: 16, isEnv: false, trailingNewline: true,
    });
    const body = { caller: 'unverified', error: 'human_required', message: 'x' };
    let refuse!: () => void;
    apiMock.writeFileLine.mockReset();
    apiMock.writeFileLine.mockImplementation(() => new Promise((_, reject) => { refuse = () => reject(new ApiError(403, JSON.stringify(body), body)); }));
    useStore.setState({ files: { file_1: info }, layout: splitLayout, focusedPaneId: 'file_1', toasts: [] });
    const { container } = render(<FileTile info={info} />);
    fireEvent.click((await screen.findAllByTitle('Edit this line'))[0]);
    const input = container.querySelector('input.file-line-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'typed text' } });
    fireEvent.blur(input);
    fireEvent.click(screen.getAllByTitle('Edit this line')[0]);
    refuse();
    await waitFor(() => expect(useStore.getState().toasts.some((t) => /Save failed/.test(t.message))).toBe(true));
    expect((container.querySelector('input.file-line-input') as HTMLInputElement).value).toBe('typed text');
  });
});
