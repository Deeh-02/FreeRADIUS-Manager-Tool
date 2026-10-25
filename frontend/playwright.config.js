// Browser tests of the forms that write money, dates and access, against the real backend.
// Start the backend first (scripts/test-stack.sh start, API on :18080), then:
//   npm run test:browser
// The built UI is served by `vite preview`, which proxies /api to API_PROXY_TARGET.
import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: 'e2e',
  timeout: 60_000,
  workers: 1, // the tests share one database and run as one story
  retries: 0,
  reporter: [['list']],
  use: {
    baseURL: 'http://127.0.0.1:4173',
    timezoneId: 'Africa/Nairobi',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  webServer: {
    command: 'npm run build && npx vite preview --host 127.0.0.1 --port 4173 --strictPort',
    url: 'http://127.0.0.1:4173',
    reuseExistingServer: false,
    timeout: 120_000,
    env: { API_PROXY_TARGET: process.env.API_PROXY_TARGET || 'http://127.0.0.1:18080' },
  },
})
