import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './tests',
  outputDir: process.env.DESKREALM_BROWSER_OUTPUT ?? 'test-results',
  reporter: process.env.DESKREALM_BROWSER_REPORT
    ? [['list'], ['json', { outputFile: process.env.DESKREALM_BROWSER_REPORT }]]
    : 'list',
  use: {
    browserName: 'chromium',
    baseURL: process.env.DESKREALM_BROWSER_BASE,
  },
})
