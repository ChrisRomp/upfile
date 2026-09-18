const { defineConfig } = require('@playwright/test');
const path = require('node:path');

module.exports = defineConfig({
  testDir: __dirname,
  testMatch: '*.spec.cjs',
  fullyParallel: false,
  workers: 1,
  timeout: 45000,
  reporter: 'list',
  outputDir: '../test-results',
  webServer: process.env.CI ? {
    command: './dist/upfile-dev',
    cwd: path.resolve(__dirname, '..'),
    url: 'https://localhost:8443/healthz',
    ignoreHTTPSErrors: true,
    reuseExistingServer: false,
    timeout: 60000,
  } : undefined,
  use: {
    browserName: 'chromium',
    channel: process.env.PLAYWRIGHT_CHANNEL || 'chrome',
    ignoreHTTPSErrors: true,
    trace: 'retain-on-failure',
  },
  projects: [
    { name: 'desktop', use: { viewport: { width: 1365, height: 1000 } } },
    { name: 'mobile', use: { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true } },
  ],
});
