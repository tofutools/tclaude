// Playwright runs against a sandbox ui.sh has already started: SANDBOX_URL is
// its URL. It drives the host's Chrome (channel: 'chrome'), so nothing is
// downloaded; set SANDBOX_CHROME to a binary path to use another one.
import { defineConfig } from '@playwright/test';

const url = process.env.SANDBOX_URL;
if (!url) throw new Error('SANDBOX_URL is not set: run this through scripts/sandbox/ui.sh');

export default defineConfig({
  testDir: './tests',
  // One sandbox, one browser: the tests share the seeded world.
  workers: 1,
  timeout: 10 * 60 * 1000,
  reporter: [['list']],
  outputDir: process.env.SANDBOX_OUT || 'test-results',
  use: {
    baseURL: url,
    headless: true,
    viewport: { width: 1440, height: 900 },
    ...(process.env.SANDBOX_CHROME
      ? { launchOptions: { executablePath: process.env.SANDBOX_CHROME } }
      : { channel: 'chrome' }),
  },
});
