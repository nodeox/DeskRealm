import { expect, test } from '@playwright/test'

test('Windows 分批发布在失败时保留批次并限制下载', async ({ page }) => {
  const task = process.env.DESKREALM_BROWSER_ROLLOUT_TASK
  const version = process.env.DESKREALM_BROWSER_ROLLOUT_VERSION
  if (!task || !version) throw new Error('Run scripts/deskrealm-m3-test windows-rollout')
  await page.goto('/login')
  await page.getByLabel('账号').fill('admin')
  await page.getByLabel('密码', { exact: true }).fill('deskrealm test passphrase')
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page.getByRole('navigation', { name: '工作台导航' })).toBeVisible()

  await page.goto('/windows-releases')
  const row = page.getByRole('row').filter({ has: page.getByRole('rowheader', { name: version }) })
  await expect(row).toContainText('仅通过分批任务分发')
  await expect(row.getByRole('link', { name: /下载|安装器/ })).toHaveCount(0)
  await row.getByRole('button', { name: '创建分批发布' }).click()
  const dialog = page.getByRole('dialog', { name: '分批发布 ' + version })
  await expect(dialog.getByLabel('设备 UUID，每行一批')).toBeVisible()
  await dialog.getByRole('button', { name: '取消' }).click()

  await page.goto('/tasks?task=' + task)
  await expect(page.getByRole('heading', { name: '批次 1 / 2 · 已暂停' })).toBeVisible()
  await expect(page.getByRole('button', { name: '放行下一批' })).toHaveCount(0)
  await page.getByRole('button', { name: '核对设备结果' }).click()
  await expect(page.getByText('原版本已验证', { exact: true })).toBeVisible()
  await expect(page.getByText('等待放行', { exact: true })).toBeVisible()
})
