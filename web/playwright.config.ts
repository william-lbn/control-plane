import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './e2e',
  timeout: 30 * 60 * 1000,
  expect: { timeout: 30_000 },
  workers: 1,
  retries: 0,
  forbidOnly: true,
  outputDir: process.env.NEON_E2E_ARTIFACTS || '../artifacts/ui',
  reporter: [['list']],
  use: {
    baseURL: process.env.NEON_E2E_BASE_URL,
    browserName: 'chromium',
    headless: true,
    viewport: { width: 1536, height: 1024 },
    locale: 'zh-CN',
    trace: 'off',
    video: 'off',
    screenshot: 'off',
  },
});
