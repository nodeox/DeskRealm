import { expect, test } from '@playwright/test'
import { readFile } from 'node:fs/promises'
import { enroll, initialize, password, requestAs, signIn } from './resource-fixtures'

test('共享通讯录、审计和任务中心按实际授权与结果运行', async ({ page, browser, baseURL }) => {
  const invitation = process.env.DESKREALM_BROWSER_TICKET
  if (!invitation || !baseURL) throw new Error('Run scripts/deskrealm-m1-test resources-browser')
  await initialize(page, invitation, '通讯录开发验证')
  const post = await requestAs(page, baseURL)

  const member = 'field-member'
  await post('/v1/admin/users', { username: member, password }, 201)
  const userGroup = await post('/v1/admin/groups', { name: '桌面支持组' }, 201) as { id: string }
  await post('/v1/admin/groups/' + userGroup.id + '/members', { username: member }, 204)
  const firstGroup = await post('/v1/admin/device-groups', { name: '前台设备' }, 201) as { id: string }
  const secondGroup = await post('/v1/admin/device-groups', { name: '机房设备' }, 201) as { id: string }
  await post('/v1/admin/device-groups/' + firstGroup.id + '/grants', { username: member, can_approve: false }, 204)

  const first = '30105223'
  const hidden = '30105224'
  const firstID = await enroll(post, firstGroup.id, first)
  const hiddenID = await enroll(post, secondGroup.id, hidden)

  await page.getByRole('link', { name: '任务中心' }).click()
  const pendingTask = page.getByRole('row').filter({ hasText: '前台设备' })
  await expect(pendingTask).toContainText('待审批 1')
  await pendingTask.getByRole('button', { name: '详情' }).click()
  await expect(page.getByRole('row').filter({ hasText: first })).toContainText('等待设备审批')

  await page.getByRole('link', { name: '设备管理', exact: true }).click()
  const row = page.getByRole('row').filter({ hasText: first })
  await row.getByRole('button', { name: '批准', exact: true }).click()
  await page.getByRole('dialog', { name: '批准设备登记' }).getByRole('button', { name: '确认批准' }).click()
  await expect(row).toContainText('已批准')

  await page.getByRole('link', { name: '用户与权限', exact: true }).click()
  await page.getByRole('button', { name: '导入账号' }).click()
  await page.getByLabel('待导入账号').fill('browser-retry-account\nbrowser-stable-account')
  await page.getByRole('button', { name: '导入', exact: true }).click()
  await page.getByText('本次导入结果（2 项）').click()
  await page.getByRole('link', { name: '查看导入任务' }).click()
  const retryRow = page.getByRole('row').filter({ hasText: 'browser-retry-account' })
  const stableRow = page.getByRole('row').filter({ hasText: 'browser-stable-account' })
  await expect(retryRow).toContainText('账号写入失败')
  await expect(stableRow).toContainText('成功')
  await retryRow.getByRole('checkbox').check()
  await page.getByRole('button', { name: '重试所选失败项' }).click()
  const retryDialog = page.getByRole('dialog', { name: '确认重试' })
  await expect(retryDialog).toContainText('browser-retry-account')
  await expect(retryDialog).not.toContainText('browser-stable-account')
  await retryDialog.getByRole('button', { name: '确认重试' }).click()
  await expect(page.getByRole('status')).toContainText('已提交 1 项重试')
  await expect(async () => {
    await page.getByRole('button', { name: '刷新', exact: true }).click()
    await expect(retryRow).toContainText('第 1 次重试 · 账号待激活', { timeout: 500 })
  }).toPass({ timeout: 8000 })
  await expect(stableRow).not.toContainText('第 1 次重试')

  await page.getByRole('link', { name: '通讯录', exact: true }).click()
  await page.getByRole('button', { name: '新建通讯录' }).click()
  await page.getByLabel('通讯录名称').fill('值班设备')
  const createResponse = page.waitForResponse(response =>
    response.url().endsWith('/v1/address-books') && response.request().method() === 'POST')
  await page.getByRole('button', { name: '创建通讯录' }).click()
  const book = await (await createResponse).json() as { id: string }
  await expect(page.getByRole('heading', { name: '值班设备' })).toBeVisible()

  async function addEntry(deviceID: string, alias: string, note: string) {
    await page.getByRole('button', { name: '添加设备条目' }).click()
    await page.getByLabel('设备', { exact: true }).selectOption(deviceID)
    await page.getByLabel('别名').fill(alias)
    await page.getByLabel('标签').fill('试点、值班')
    await page.getByLabel('备注').fill(note)
    await page.getByRole('button', { name: '保存设备条目' }).click()
    await expect(page.getByRole('row').filter({ hasText: alias })).toBeVisible()
  }
  await addEntry(firstID, '前台主机', '可查看备注')
  await addEntry(hiddenID, '机房主机', '机房私有备注')

  await page.getByRole('button', { name: '共享设置' }).click()
  await page.getByLabel('共享给').selectOption('group')
  await page.getByLabel('共享用户组名称').fill('桌面支持组')
  await page.getByRole('button', { name: '保存共享' }).click()
  const grant = page.getByRole('listitem').filter({ hasText: '桌面支持组' })
  await expect(grant).toContainText('查看')

  const viewer = await browser.newPage({ baseURL, viewport: { width: 375, height: 800 } })
  try {
    await signIn(viewer, member)
    await viewer.getByRole('link', { name: '设备管理', exact: true }).click()
    await expect(viewer.getByRole('row').filter({ hasText: first })).toBeVisible()
    await expect(viewer.getByRole('row').filter({ hasText: hidden })).toHaveCount(0)
    await viewer.getByRole('link', { name: '任务中心' }).click()
    await expect(viewer.getByRole('row').filter({ hasText: '机房设备' })).toHaveCount(0)
    await expect(viewer.getByRole('row').filter({ hasText: '账号导入' })).toHaveCount(0)
    const visibleTask = viewer.getByRole('row').filter({ hasText: '前台设备' })
    await expect(visibleTask).toContainText('成功 1')
    await visibleTask.getByRole('button', { name: '详情' }).click()
    await expect(viewer.getByRole('row').filter({ hasText: first })).toContainText('设备已通过审批')
    await expect(viewer.getByRole('button', { name: '重试所选失败项' })).toHaveCount(0)
    await viewer.getByRole('link', { name: '通讯录', exact: true }).click()
    const openBook = viewer.getByRole('button', { name: '打开 值班设备' })
    await openBook.click()
    await expect(viewer.getByRole('heading', { name: '值班设备' })).toBeFocused()
    await expect(viewer.getByRole('row').filter({ hasText: '前台主机' })).toContainText('可查看备注')
    await expect(viewer.getByText('机房私有备注')).toHaveCount(0)
    await expect(viewer.getByRole('button', { name: '添加设备条目' })).toHaveCount(0)
    await expect(viewer.getByRole('button', { name: '共享设置' })).toHaveCount(0)
    expect(await viewer.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375)

    await page.getByLabel('共享用户组名称').fill('桌面支持组')
    await page.getByLabel('共享权限').selectOption('2')
    await page.getByRole('button', { name: '保存共享' }).click()
    await expect(grant).toContainText('编辑')
    await viewer.getByRole('button', { name: '刷新', exact: true }).click()
    await viewer.getByRole('row').filter({ hasText: '前台主机' }).getByRole('button', { name: '编辑' }).click()
    await viewer.getByLabel('别名').fill('前台支持机')
    await viewer.getByRole('button', { name: '保存设备条目' }).click()
    await expect(viewer.getByRole('row').filter({ hasText: '前台支持机' })).toBeVisible()
    await expect(viewer.getByRole('button', { name: '共享设置' })).toHaveCount(0)

    await grant.getByRole('button', { name: '撤销共享' }).click()
    const confirmation = page.getByRole('dialog', { name: '撤销共享' })
    await confirmation.getByRole('button', { name: '确认', exact: true }).click()
    await expect(grant).toHaveCount(0)
    await viewer.getByRole('button', { name: '刷新', exact: true }).click()
    await expect(openBook).toHaveCount(0)
    await expect(viewer.getByRole('alert')).toContainText('无权访问')
    expect((await viewer.request.get('/v1/address-books/' + book.id + '/entries')).status()).toBe(404)

    await expect(viewer.getByRole('link', { name: '会话与审计' })).toHaveCount(0)
    await page.getByRole('link', { name: '会话与审计' }).click()
    await page.getByRole('button', { name: '审计设置' }).click()
    await page.getByLabel('审计员账号').fill(member)
    await page.getByLabel('设备组名称').fill('前台设备')
    await page.getByLabel('允许导出该范围').check()
    await page.getByRole('button', { name: '保存审计授权' }).click()
    await expect(page.getByRole('status').filter({ hasText: '审计授权已保存' })).toBeVisible()
    await page.getByLabel('保留期（天）').fill('120')
    await page.getByRole('button', { name: '保存保留期' }).click()
    await expect(page.getByRole('status').filter({ hasText: '保留期已保存' })).toBeVisible()

    await viewer.reload()
    await viewer.getByRole('link', { name: '会话与审计' }).click()
    await expect(viewer.getByRole('button', { name: '审计设置' })).toHaveCount(0)
    await viewer.getByLabel('事件名称').fill('device.approved')
    await viewer.getByRole('button', { name: '查询', exact: true }).click()
    const audited = viewer.getByRole('row').filter({ hasText: 'device.approved' })
    await expect(audited).toHaveCount(1)
    await audited.getByRole('button', { name: '详情' }).click()
    const eventDetail = viewer.getByRole('dialog', { name: '事件详情' })
    await expect(eventDetail).toContainText(firstID)
    await expect(eventDetail).toContainText('已确认')
    await eventDetail.getByRole('button', { name: '关闭' }).click()

    const download = viewer.waitForEvent('download')
    await viewer.getByRole('button', { name: '导出 CSV' }).click()
    const csv = await download
    expect(csv.suggestedFilename()).toBe('deskrealm-audit.csv')
    const downloadedPath = await csv.path()
    if (!downloadedPath) throw new Error('CSV download path missing')
    const csvText = await readFile(downloadedPath, 'utf8')
    expect(csvText).toContain(firstID)
    expect(csvText).not.toContain(hiddenID)

    const auditor = page.getByRole('listitem').filter({ hasText: member })
    await auditor.getByRole('button', { name: '撤销审计授权' }).click()
    await page.getByRole('dialog', { name: '确认撤销审计授权' }).getByRole('button', { name: '确认', exact: true }).click()
    await expect(auditor).toHaveCount(0)
    expect((await viewer.request.get('/v1/admin/audit-events')).status()).toBe(403)
  } finally {
    await viewer.close()
  }
})
