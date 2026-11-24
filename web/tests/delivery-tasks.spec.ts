import { expect, test } from '@playwright/test'

test('任务中心展示外部回执并处理重试与执行前取消', async ({ page }) => {
  const retryTask = process.env.DESKREALM_BROWSER_DELIVERY_TASK
  const cancelTask = process.env.DESKREALM_BROWSER_CANCEL_TASK
  const manifest = process.env.DESKREALM_BROWSER_WINDOWS_MANIFEST
  if (!retryTask || !cancelTask || !manifest) throw new Error('Run scripts/deskrealm-m3-test delivery-tasks')
  await page.goto('/login')
  await page.getByLabel('账号').fill('admin')
  await page.getByLabel('密码', { exact: true }).fill('deskrealm test passphrase')
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page.getByRole('link', { name: '任务中心' })).toBeVisible()

  await page.goto('/tasks?task=' + retryTask)
  const failure = page.getByRole('row').filter({ hasText: 'retry-target' })
  await expect(failure).toContainText('外部执行失败')
  await failure.getByText('第 1 次尝试', { exact: false }).click()
  await expect(failure.getByText('外部回执', { exact: false })).toBeVisible()
  await failure.getByRole('checkbox').check()
  await page.getByRole('button', { name: '重试所选失败项' }).click()
  await page.getByRole('dialog', { name: '确认重试' }).getByRole('button', { name: '确认重试' }).click()
  await expect(page.getByRole('status')).toContainText('已提交 1 项重试')
  await expect(failure).toContainText('待执行')
  await expect(failure.getByRole('checkbox')).toHaveCount(0)

  await page.goto('/tasks?task=' + cancelTask)
  const queued = page.getByRole('row').filter({ hasText: 'cancel-target' })
  await queued.getByRole('checkbox').check()
  await page.getByRole('button', { name: '取消所选未开始项' }).click()
  await page.getByRole('dialog', { name: '确认取消' }).getByRole('button', { name: '确认取消' }).click()
  await expect(page.getByRole('status')).toContainText('已取消 1 项')
  await expect(queued).toContainText('已取消')
  await expect(queued.getByRole('checkbox')).toHaveCount(0)

  await page.getByRole('button', { name: '验证 Windows 开发包' }).click()
  const dialog = page.getByRole('dialog', { name: '验证 Windows 开发包' })
  await dialog.getByLabel('清单 SHA-256').fill('f'.repeat(64))
  await dialog.getByRole('button', { name: '提交验证' }).click()
  await expect(dialog.getByRole('alert')).toContainText('中心暂存区找不到')
  await dialog.getByLabel('清单 SHA-256').fill(manifest)
  await dialog.getByRole('button', { name: '提交验证' }).click()
  const evidence = page.getByRole('region', { name: '构建与签名证据' })
  await expect(evidence).toContainText('端域测试')
  await expect(evidence).toContainText('1234567890abcdef1234567890abcdef12345678')
  await expect(evidence.getByRole('rowheader', { name: 'x64' })).toBeVisible()
  await expect(evidence.getByRole('rowheader', { name: 'arm64' })).toBeVisible()
  await expect(evidence.getByText('开发签名验证时间')).toHaveCount(0)
})
