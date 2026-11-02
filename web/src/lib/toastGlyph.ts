export interface DaemonToastGlyph {
  message: string;
  icon?: string;
  kind?: 'info' | 'warn';
}

/**
 * Daemon emit sites in schedules_agent.go, schedules_task.go, agents_run.go,
 * schedules_runner.go, workflow_janitor.go and optional features prefix human-readable toast text with glyphs. Resolve
 * those prefixes at the web boundary: the same daemon strings also feed the
 * Flutter mobile app, so changing their wire text would break that client.
 */
export function splitDaemonToastGlyph(text: string): DaemonToastGlyph {
  const match = /^(🤖|◇|⏰|🦉|🧹|⚠)\uFE0F?\s+/u.exec(text);
  if (!match) return { message: text };

  const glyph = match[1];
  const message = text.slice(match[0].length).trimStart();
  if (glyph === '⚠') return { message, kind: 'warn' };
  if (glyph === '🧹') return { message };
  return { message, icon: glyph };
}
