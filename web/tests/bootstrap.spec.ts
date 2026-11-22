import { expect, test } from '@playwright/test'

test('首次管理员初始化、成员停用、刷新和退出', async ({ page, browser }) => {
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

  await page.getByRole('link', { name: '用户与权限' }).click()
  await expect(page.getByRole('heading', { name: '用户与权限', exact: true })).toBeVisible()
  await page.getByRole('button', { name: '添加成员' }).click()
  await page.getByLabel('成员账号').fill('member01')
  await page.getByLabel('初始密码').fill('member-test-password')
  await page.getByRole('button', { name: '创建成员' }).click()
  const row = page.getByRole('row').filter({ hasText: 'member01' })
  await expect(row).toContainText('已启用')

  const member = await browser.newPage()
  try {
    await member.goto(new URL('/login', invite).toString())
    await member.getByLabel('账号').fill('member01')
    await member.getByLabel('密码', { exact: true }).fill('member-test-password')
    await member.getByRole('button', { name: '登录', exact: true }).click()
    await expect(member.getByRole('heading', { name: '概览', exact: true })).toBeVisible()
    await expect(member.getByRole('link', { name: '用户与权限' })).toHaveCount(0)

    await row.getByRole('button', { name: '停用', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: '停用账号' })
    await dialog.getByLabel('操作原因').fill('浏览器验证')
    await dialog.getByRole('button', { name: '确认停用' }).click()
    await expect(row).toContainText('已停用')
    await member.reload()
    await expect(member.getByRole('heading', { name: '登录端域控制台' })).toBeVisible()
  } finally {
    await member.close()
  }

  await page.getByRole('link', { name: '概览', exact: true }).click()
  await page.setViewportSize({ width: 375, height: 800 })
  await page.reload()
  await expect(page.getByRole('heading', { name: '概览', exact: true })).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375)
  await page.getByRole('button', { name: '退出登录' }).click()
  await expect(page.getByRole('heading', { name: '登录端域控制台' })).toBeVisible()
  await page.goto('/overview')
  await expect(page).toHaveURL(/\/login$/)
})
