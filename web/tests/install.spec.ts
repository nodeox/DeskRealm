import { expect, test } from '@playwright/test'

test('HTTPS 中心首次管理员初始化和登录', async ({ page }) => {
  const link = process.env.DESKREALM_BROWSER_TICKET
  if (!link) throw new Error('Run scripts/deskrealm-m3-test install-bootstrap')

  await page.goto(link)
  await expect(page).toHaveURL(/\/initialize$/)
  await page.getByLabel('组织名称').fill('安装验证组织')
  await page.getByLabel('账号').fill('install-admin')
  await page.getByLabel('密码', { exact: true }).fill('deskrealm-development-passphrase')
  await page.getByRole('button', { name: '初始化中心' }).click()
  await expect(page).toHaveURL(/\/login$/)

  await page.getByLabel('密码', { exact: true }).fill('deskrealm-development-passphrase')
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page.getByRole('heading', { name: '资源与待办' })).toBeVisible()
  await expect(page.getByRole('main').getByText('安装验证组织', { exact: true })).toBeVisible()

  const cookies = await page.context().cookies()
  expect(cookies.find(cookie => cookie.name === '__Host-deskrealm')).toEqual(
    expect.objectContaining({ secure: true, httpOnly: true }),
  )
})
