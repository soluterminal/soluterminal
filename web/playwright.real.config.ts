import { defineConfig, devices } from 'playwright/test';

const base = new URL(process.env.SOLU_REAL_E2E_URL ?? 'http://invalid');
if (base.hostname !== '127.0.0.1' || base.protocol !== 'http:' || !base.port || base.port === '8787') {
  throw new Error('Real-daemon tests require the isolated side-daemon-e2e launcher');
}
if (!/^\d+$/.test(process.env.SOLU_REAL_E2E_PID ?? '')) throw new Error('Missing real solud PID');

// Run only when selected: the failure probe, and specs that need a startup
// scenario (scripts/side-daemon-e2e.py SCENARIOS).
const selectedOnly = ['failure-artifacts.spec.ts', 'approve-device.spec.ts', 'browser-origin.spec.ts'];

export default defineConfig({
  testDir: './e2e-real',
  testMatch: '**/*.spec.ts',
  testIgnore: selectedOnly.filter((spec) => spec !== process.env.SOLU_REAL_E2E_SPEC).map((spec) => `**/${spec}`),
  outputDir: 'test-results/real',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: true,
  timeout: 30_000,
  reporter: [['html', { outputFolder: 'playwright-report', open: 'never' }], ['list']],
  use: {
    baseURL: base.origin,
    serviceWorkers: 'block',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    viewport: { width: 1440, height: 1000 },
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 1000 } } }],
});
