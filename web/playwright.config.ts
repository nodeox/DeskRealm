import { defineConfig } from '@playwright/test'

const tlsPin = process.env.DESKREALM_BROWSER_TLS_SPKI
const localHost = process.env.DESKREALM_BROWSER_LOCAL_HOST

export default defineConfig({
  testDir: './tests',
  outputDir: process.env.DESKREALM_BROWSER_OUTPUT ?? 'test-results',
  reporter: process.env.DESKREALM_BROWSER_REPORT
    ? [['list'], ['json', { outputFile: process.env.DESKREALM_BROWSER_REPORT }]]
    : 'list',
  use: {
    browserName: 'chromium',
    baseURL: process.env.DESKREALM_BROWSER_BASE,
    launchOptions: {
      args: [
        ...(tlsPin ? [`--ignore-certificate-errors-spki-list=${tlsPin}`] : []),
        ...(localHost ? [`--host-resolver-rules=MAP ${localHost} 127.0.0.1`, '--no-proxy-server'] : []),
      ],
    },
  },
})
