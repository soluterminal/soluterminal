import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../../store';
import type { McpServer, PaneInfo } from '../../types';
import { MCPSection } from './MCPSection';

const CWD = '/work/project';
const servers: McpServer[] = [
  { name: 'remote', type: 'http', source: 'user', url: 'https://mcp.example.com/sse?api_key=[redacted: token]', status: 'alive' },
  { name: 'local', type: 'stdio', source: '.mcp.json', command: 'npx', args: ['-y', 'some-mcp', '--api-key', '[redacted: token]'] },
  { name: 'bare', type: '', source: 'codex' },
];

describe('MCPSection', () => {
  const writeText = vi.fn(async (_text: string) => {});
  const initial = useStore.getState();

  beforeEach(() => {
    writeText.mockClear();
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    useStore.setState({
      focusedPaneId: 'p1',
      panes: { p1: { id: 'p1', cwd: CWD } as PaneInfo },
      mcpServersByCwd: { [CWD]: servers },
      refreshMcpServers: vi.fn(async () => {}),
      toast: vi.fn(),
    });
  });

  afterEach(() => {
    useStore.setState(initial, true);
  });

  it('copies exactly the text the row shows', async () => {
    render(<MCPSection collapsed={false} onToggle={() => {}} />);

    const address = 'https://mcp.example.com/sse?api_key=[redacted: token]';
    fireEvent.click(screen.getByText(address));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText).toHaveBeenLastCalledWith(address);

    const commandLine = 'npx -y some-mcp --api-key [redacted: token]';
    fireEvent.click(screen.getByText(commandLine));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(2));
    expect(writeText).toHaveBeenLastCalledWith(commandLine);
  });

  it('copies nothing for a row without details', () => {
    render(<MCPSection collapsed={false} onToggle={() => {}} />);
    fireEvent.click(screen.getByText('bare'));
    expect(writeText).not.toHaveBeenCalled();
  });
});
