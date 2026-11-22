import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './tests',
  outputDir: process.env.DESKREALM_BROWSER_OUTPUT ?? 'test-results',
  reporter: 'list',
  use: {
    browserName: 'chromium',
    baseURL: process.env.DESKREALM_BROWSER_BASE,
  },
})
