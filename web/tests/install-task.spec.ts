import { expect, test } from '@playwright/test'

test('任务中心显示中心安装结果和已导入的中断记录', async ({ page }) => {
  const task = process.env.DESKREALM_BROWSER_INSTALL_TASK
  const revision = process.env.DESKREALM_BROWSER_INSTALL_REVISION
  if (!task || !revision) throw new Error('Run scripts/deskrealm-m3-test install-task')
  await page.goto('/login')
  await page.getByLabel('账号').fill('install-admin')
  await page.getByLabel('密码', { exact: true }).fill('deskrealm-development-passphrase')
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await page.getByRole('link', { name: '任务中心' }).click()

  const row = page.getByRole('row').filter({ hasText: '中心安装' })
  await expect(row).toHaveCount(1)
  await expect(row).toContainText('成功 4')
  await row.getByRole('button', { name: '详情' }).click()
  await expect(page.getByRole('heading', { name: '中心安装详情' })).toBeVisible()
  await expect(page.getByText(task, { exact: true })).toBeVisible()
  await expect(page.getByRole('row').filter({ hasText: '验证服务健康' })).toContainText('成功')
  await expect(page.getByRole('button', { name: '重试所选失败项' })).toHaveCount(0)

  const query = new URLSearchParams({ task, revision })
  await page.goto('/tasks?' + query)
  const health = page.getByRole('row').filter({ hasText: '验证服务健康' })
  await expect(health).toContainText('已中断')
  await page.getByRole('button', { name: '刷新', exact: true }).click()
  await expect(health).toContainText('已中断')
})
