import { expect, test } from '@playwright/test'
import type { Page } from '@playwright/test'
import { generateKeyPairSync, sign } from 'node:crypto'
import { readFile } from 'node:fs/promises'

const password = 'deskrealm-browser-test-passphrase'

async function signIn(page: Page, username: string) {
  await page.goto('/login')
  await page.getByLabel('账号').fill(username)
  await page.getByLabel('密码', { exact: true }).fill(password)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page.getByRole('heading', { name: '概览', exact: true })).toBeVisible()
}

test('共享通讯录和审计遵守范围并即时撤权', async ({ page, browser, baseURL }) => {
  const invitation = process.env.DESKREALM_BROWSER_TICKET
  if (!invitation || !baseURL) throw new Error('Run scripts/deskrealm-m1-test resources-browser')
  await page.goto(invitation)
  await page.getByLabel('组织名称').fill('通讯录开发验证')
  await page.getByLabel('账号').fill('admin')
  await page.getByLabel('密码', { exact: true }).fill(password)
  await page.getByRole('button', { name: '初始化中心' }).click()
  await expect(page.getByRole('heading', { name: '登录端域控制台' })).toBeVisible()
  await signIn(page, 'admin')
  const session = await (await page.request.get('/v1/auth/session')).json() as { csrf_token: string }

  async function post(path: string, data: object, status: number) {
    const response = await page.request.post(path, { data, headers: { Origin: baseURL!, 'X-CSRF-Token': session.csrf_token } })
    expect(response.status(), path).toBe(status)
    if (status !== 204) return response.json()
  }

  const member = 'field-member'
  await post('/v1/admin/users', { username: member, password }, 201)
  const userGroup = await post('/v1/admin/groups', { name: '桌面支持组' }, 201) as { id: string }
  await post('/v1/admin/groups/' + userGroup.id + '/members', { username: member }, 204)
  const firstGroup = await post('/v1/admin/device-groups', { name: '前台设备' }, 201) as { id: string }
  const secondGroup = await post('/v1/admin/device-groups', { name: '机房设备' }, 201) as { id: string }
  await post('/v1/admin/device-groups/' + firstGroup.id + '/grants', { username: member, can_approve: false }, 204)

  async function enroll(groupID: string, id: string) {
    const grant = await post('/v1/admin/enrollment-credentials', {
      group_id: groupID, platform: 'windows-amd64', purpose: '自动化验证', lifetime_minutes: 10,
    }, 201) as { credential: string }
    const keys = generateKeyPairSync('ed25519')
    const point = keys.publicKey.export({ format: 'jwk' }).x
    if (!point) throw new Error('Ed25519 public key missing')
    const input = {
      credential: grant.credential, group_id: groupID, platform: 'windows-amd64', rustdesk_id: id,
      public_key: Buffer.from(point, 'base64url').toString('base64'),
    }
    const challenge = await post('/v1/enrollment/challenge', input, 200) as { attempt_id: string; nonce: string }
    const message = Buffer.from([
      'deskrealm/device-enrollment/v1', challenge.attempt_id, challenge.nonce, groupID,
      input.platform, id, input.public_key,
    ].join('\n'))
    const result = await post('/v1/enrollment/complete', {
      ...input, attempt_id: challenge.attempt_id, signature: sign(null, message, keys.privateKey).toString('base64'),
    }, 200) as { device_id: string }
    return result.device_id
  }

  const first = '30105223'
  const hidden = '30105224'
  const firstID = await enroll(firstGroup.id, first)
  const hiddenID = await enroll(secondGroup.id, hidden)

  await page.getByRole('link', { name: '设备管理', exact: true }).click()
  const row = page.getByRole('row').filter({ hasText: first })
  await row.getByRole('button', { name: '批准', exact: true }).click()
  await page.getByRole('dialog', { name: '批准设备登记' }).getByRole('button', { name: '确认批准' }).click()
  await expect(row).toContainText('已批准')

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
