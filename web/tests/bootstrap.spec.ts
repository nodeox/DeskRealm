import { expect, test } from '@playwright/test'

test('首次管理员初始化、登录、刷新和退出', async ({ page }) => {
  const invite = process.env.DESKREALM_BROWSER_TICKET
  if (!invite) throw new Error('Run scripts/deskrealm-m1-test bootstrap-browser')

  await page.goto('/initialize#ticket=' + 'A'.repeat(43))
  await expect(page).toHaveURL(/\/initialize$/)
  await page.getByLabel('组织名称').fill('开发验收组织')
  await page.getByLabel('账号').fill('admin')
  await page.getByLabel('密码', { exact: true }).fill('test-passphrase-for-deskrealm')
  await page.getByRole('button', { name: '初始化中心' }).click()
  await expect(page.getByRole('alert')).toContainText('初始化链接无效或已过期')

  await page.goto(invite)
  await expect(page).toHaveURL(/\/initialize$/)
  await page.getByLabel('组织名称').fill('开发验收组织')
  await page.getByLabel('账号').fill('admin')
  await page.getByLabel('密码', { exact: true }).fill('test-passphrase-for-deskrealm')
  await page.getByRole('button', { name: '初始化中心' }).click()
  await expect(page.getByRole('heading', { name: '登录端域控制台' })).toBeVisible()
  await expect(page).toHaveURL(/\/login$/)

  await page.getByLabel('密码', { exact: true }).fill('wrong-password')
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('账号或密码有误')
  await page.getByLabel('密码', { exact: true }).fill('test-passphrase-for-deskrealm')
  await page.getByLabel('密码', { exact: true }).press('Enter')
  await expect(page.getByRole('heading', { name: '当前组织' })).toBeVisible()
  await expect(page.getByRole('definition').filter({ hasText: '开发验收组织' })).toBeVisible()

  await page.setViewportSize({ width: 375, height: 800 })
  await page.reload()
  await expect(page.getByRole('heading', { name: '概览', exact: true })).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375)
  await page.getByRole('button', { name: '退出登录' }).click()
  await expect(page.getByRole('heading', { name: '登录端域控制台' })).toBeVisible()
  await page.goto('/overview')
  await expect(page).toHaveURL(/\/login$/)
})
