import { expect, test } from '@playwright/test'
import { createHash } from 'node:crypto'
import { readFile } from 'node:fs/promises'

test('任务中心展示外部回执并处理重试与执行前取消', async ({ page }) => {
  const retryTask = process.env.DESKREALM_BROWSER_DELIVERY_TASK
  const cancelTask = process.env.DESKREALM_BROWSER_CANCEL_TASK
  const manifest = process.env.DESKREALM_BROWSER_WINDOWS_MANIFEST
  const releaseTask = process.env.DESKREALM_BROWSER_WINDOWS_RELEASE_TASK
  if (!retryTask || !cancelTask || !manifest || !releaseTask) throw new Error('Run scripts/deskrealm-m3-test delivery-tasks')
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
  await expect(evidence.getByRole('button', { name: '发布已验证开发包' })).toHaveCount(0)

  await page.goto('/tasks?task=' + releaseTask)
  await page.getByRole('button', { name: '发布已验证开发包' }).click()
  const publish = page.getByRole('dialog', { name: '确认发布开发包' })
  await publish.getByRole('button', { name: '确认发布' }).click()
  await page.getByRole('link', { name: '查看 Windows 发布' }).click()
  const release = page.getByRole('row').filter({ hasText: '0.1.0-dev.4' })
  const fileLink = release.getByRole('link', { name: '下载 ARM64' })
  await expect(fileLink).toBeVisible()
  const download = page.waitForEvent('download')
  await fileLink.click()
  const result = await download
  expect(result.suggestedFilename()).toBe('DeskRealm-arm64.zip')
  const checksum = createHash('sha256').update(await readFile(await result.path())).digest('hex')
  await expect(release.getByText(checksum, { exact: true })).toHaveCount(1)

  await release.getByRole('button', { name: '撤回发布' }).click()
  await page.getByRole('dialog', { name: '撤回发布' }).getByRole('button', { name: '确认撤回' }).click()
  await expect(release).toContainText('已撤回')
  await expect(release.getByRole('link')).toHaveCount(0)
})
