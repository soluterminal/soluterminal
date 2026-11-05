import { test, expect } from 'playwright/test';
import { spawn } from 'node:child_process';
import { writeFileSync } from 'node:fs';

// Opt-in only, via --spec failure-artifacts.spec.ts. A successful infra probe
// returns a failing test result with trace, solud log and cleanup.json.
test('deliberate failure retains evidence and reaps a detached child', async ({ page }, testInfo) => {
  await page.goto('/');
  const child = spawn('/bin/sleep', ['300'], { detached: true, stdio: 'ignore' });
  child.unref();
  writeFileSync(testInfo.outputPath('detached-child.json'), JSON.stringify({ pid: child.pid }));
  await testInfo.attach('probe', { body: 'Intentional failure to verify artifacts and process-tree cleanup.', contentType: 'text/plain' });
  expect(false, 'Intentional failure-artifacts probe; inspect cleanup.json and trace.zip').toBe(true);
});
