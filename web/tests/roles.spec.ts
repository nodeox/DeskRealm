import { expect, test, type Page } from '@playwright/test'
import { readFile } from 'node:fs/promises'
import { enroll, initialize, password, requestAs, signIn } from './resource-fixtures'

const nav = (page: Page) => page.getByRole('navigation', { name: '工作台导航' })
const metric = (page: Page, name: string) => page.getByRole('region', { name: '资源与待办' }).getByRole('link', { name: new RegExp('^' + name) })

test('四类角色的概览、入口和操作遵守真实授权', async ({ page, browser, baseURL }) => {
  const ticket = process.env.DESKREALM_BROWSER_TICKET
  if (!ticket || !baseURL) throw new Error('Run scripts/deskrealm-m1-test roles-browser')
  await initialize(page, ticket, '角色验收组织')
  const post = await requestAs(page, baseURL)
  await expect(metric(page, '在线设备')).toHaveText('在线设备0')
  await expect(metric(page, '待审批设备')).toHaveText('待审批设备0')
  await expect(page.getByRole('region', { name: '近期任务' })).toContainText('暂无可查看的任务')
  await expect(nav(page).getByRole('link', { name: '用户组与委派' })).toBeVisible()

  const managerName = 'group-manager'
  const operatorName = 'desk-operator'
  const auditorName = 'desk-auditor'
  const staffName = 'team-staff'
  const outsiderName = 'outside-staff'
  const accounts: Record<string, string> = {}
  for (const username of [managerName, operatorName, auditorName, staffName, outsiderName]) {
    accounts[username] = (await post<{ id: string }>('/v1/admin/users', { username, password }, 201)).id
  }
  const userGroup = await post<{ id: string }>('/v1/admin/groups', { name: '值班用户组' }, 201)
  await post('/v1/admin/groups/' + userGroup.id + '/members', { username: staffName }, 204)
  await post('/v1/admin/role-grants', { username: managerName, role: 'user_manager', scope: 'group', group_id: userGroup.id }, 201)
  const local = await post<{ id: string }>('/v1/admin/device-groups', { name: '值班终端组' }, 201)
  const hidden = await post<{ id: string }>('/v1/admin/device-groups', { name: '财务终端组' }, 201)
  const grant = '/v1/admin/device-groups/' + local.id + '/grants'
  await post(grant, { username: managerName, can_approve: true }, 204)
  await post(grant, { username: operatorName, can_approve: false }, 204)
  await post('/v1/admin/audit-grants', { username: auditorName, scope: 'device_group', group: '值班终端组', can_export: true }, 204)
  const includedDevice = '32142011'
  const excludedDevice = '32142012'
  const includedID = await enroll(post, local.id, includedDevice)
  const excludedID = await enroll(post, hidden.id, excludedDevice)

  const publicBook = await post<{ id: string }>('/v1/address-books', { name: '值班通讯录' }, 201)
  const privateBook = await post<{ id: string }>('/v1/address-books', { name: '内部通讯录' }, 201)
  await post('/v1/address-books/' + publicBook.id + '/entries', { device_id: includedID, alias: '值班主机' }, 201)
  await post('/v1/address-books/' + publicBook.id + '/shares', { kind: 'user', name: operatorName, permission: 1 }, 204)

  await page.getByRole('button', { name: '刷新概览' }).click()
  await expect(metric(page, '可查看账号')).toHaveText('可查看账号6')
  await expect(metric(page, '待审批设备')).toHaveText('待审批设备2')
  await expect(metric(page, '可查看通讯录')).toHaveText('可查看通讯录2')
  await post('/v1/admin/users/imports', {
    idempotency_key: 'roles-browser-import', usernames: ['browser-retry-account', 'roles-stable-account'],
  }, 200)
  await page.getByRole('button', { name: '刷新概览' }).click()
  await expect(metric(page, '可重试对象')).toHaveText('可重试对象1')
  await metric(page, '可重试对象').click()
  await expect(page.getByRole('row').filter({ hasText: '账号导入' })).toHaveCount(1)
  await expect(page.getByRole('row').filter({ hasText: '设备入网' })).toHaveCount(0)
  await page.goto('/overview')
  await page.getByRole('region', { name: '近期任务' }).getByRole('link', { name: /账号导入/ }).click()
  const failed = page.getByRole('row').filter({ hasText: 'browser-retry-account' })
  await failed.getByRole('checkbox').check()
  await page.getByRole('button', { name: '重试所选失败项' }).click()
  const retry = page.getByRole('dialog', { name: '确认重试' })
  await expect(retry).not.toContainText('roles-stable-account')
  await retry.getByRole('button', { name: '确认重试' }).click()
  await expect(page.getByRole('status')).toContainText('已提交 1 项重试')
  await expect(async () => {
    await page.getByRole('button', { name: '刷新', exact: true }).click()
    await expect(failed).toContainText('第 1 次重试 · 账号待激活', { timeout: 500 })
  }).toPass({ timeout: 8000 })

  const manager = await browser.newPage({ baseURL })
  try {
    await signIn(manager, managerName)
    await expect(metric(manager, '可查看账号')).toHaveText('可查看账号1')
    await expect(metric(manager, '待审批设备')).toHaveText('待审批设备1')
    await expect(metric(manager, '可重试对象')).toHaveCount(0)
    await expect(manager.getByRole('region', { name: '近期任务' })).toContainText('值班终端组')
    await expect(manager.getByRole('region', { name: '近期任务' })).not.toContainText('财务终端组')
    await expect(nav(manager).getByRole('link', { name: '用户组与委派' })).toHaveCount(0)
    expect((await manager.request.get('/v1/admin/tasks?retry=available')).status()).toBe(403)
    await manager.goto('/users')
    await expect(manager.getByRole('row').filter({ hasText: staffName }).getByRole('button', { name: '停用', exact: true })).toBeVisible()
    await expect(manager.getByRole('row').filter({ hasText: outsiderName })).toHaveCount(0)
    await expect(manager.getByRole('button', { name: '添加成员' })).toHaveCount(0)
    expect((await manager.request.get('/v1/admin/users/' + accounts[outsiderName])).status()).toBe(404)

    await manager.goto('/overview')
    const pending = metric(manager, '待审批设备')
    await pending.focus()
    await expect(pending).toBeFocused()
    await pending.press('Enter')
    await expect(manager).toHaveURL(/\/devices\?review=pending$/)
    await expect(manager.getByRole('heading', { name: '设备管理', exact: true })).toBeFocused()
    await expect(manager.getByRole('row').filter({ hasText: excludedDevice })).toHaveCount(0)
    await manager.getByRole('row').filter({ hasText: includedDevice }).getByRole('button', { name: '批准', exact: true }).click()
    await manager.getByRole('dialog', { name: '批准设备登记' }).getByRole('button', { name: '确认批准' }).click()
    await expect(manager.getByText('暂无待审批设备。')).toBeVisible()
    await manager.goto('/overview')
    await expect(metric(manager, '待审批设备')).toHaveText('待审批设备0')
    await expect(manager.getByRole('region', { name: '近期任务' })).toContainText('成功 1')
  } finally {
    await manager.close()
  }
  await post('/v1/admin/devices/' + excludedID + '/approve', {}, 200)

  const operator = await browser.newPage({ baseURL, viewport: { width: 375, height: 800 } })
  try {
    await signIn(operator, operatorName)
    await expect(metric(operator, '可查看通讯录')).toHaveText('可查看通讯录1')
    await expect(metric(operator, '待审批设备')).toHaveCount(0)
    await expect(nav(operator).getByRole('link', { name: '用户与权限' })).toHaveCount(0)
    expect(await operator.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375)
    await operator.goto('/devices')
    const row = operator.getByRole('row').filter({ hasText: includedDevice })
    await expect(row).toContainText('已批准')
    await expect(operator.getByRole('row').filter({ hasText: excludedDevice })).toHaveCount(0)
    await expect(operator.getByRole('button', { name: '发放入网凭据' })).toHaveCount(0)
    await row.getByRole('button', { name: '详情' }).click()
    const deviceDetail = operator.getByRole('dialog', { name: '设备详情' })
    await expect(deviceDetail).toContainText(includedID)
    await expect(deviceDetail.getByRole('button', { name: '轮换密钥' })).toHaveCount(0)
    await operator.keyboard.press('Escape')
    await expect(row.getByRole('button', { name: '详情' })).toBeFocused()
    expect((await operator.request.get('/v1/admin/devices/' + excludedID)).status()).toBe(404)

    await nav(operator).getByRole('link', { name: '通讯录', exact: true }).click()
    await expect(operator.getByRole('button', { name: '打开 内部通讯录' })).toHaveCount(0)
    await operator.getByRole('button', { name: '打开 值班通讯录' }).click()
    await expect(operator.getByRole('row').filter({ hasText: '值班主机' })).toBeVisible()
    await expect(operator.getByRole('button', { name: '添加设备条目' })).toHaveCount(0)
    expect((await operator.request.get('/v1/address-books/' + privateBook.id)).status()).toBe(404)
    await operator.goto('/audit')
    await expect(operator.getByRole('alert')).toContainText('无权访问「审计事件」')
    expect((await operator.request.get('/v1/admin/audit-events')).status()).toBe(403)
  } finally {
    await operator.close()
  }

  const approvals = await (await page.request.get('/v1/admin/audit-events?action=device.approved')).json() as {
    events: { id: string; resource_id: string }[]
  }
  const hiddenEvent = approvals.events.find(event => event.resource_id === excludedID)
  expect(hiddenEvent).toBeDefined()
  const auditor = await browser.newPage({ baseURL })
  try {
    await signIn(auditor, auditorName)
    await expect(nav(auditor).getByRole('link', { name: '审计事件' })).toBeVisible()
    await expect(nav(auditor).getByRole('link', { name: '设备管理' })).toHaveCount(0)
    await expect(nav(auditor).getByRole('link', { name: '任务中心' })).toHaveCount(0)
    await expect(metric(auditor, '在线设备')).toHaveCount(0)
    await auditor.getByRole('region', { name: '近期审计' }).getByRole('link', { name: /批准设备/ }).click()
    const event = auditor.getByRole('dialog', { name: '事件详情' })
    await expect(event).toContainText(includedID)
    await expect(event).toContainText('已确认')
    await auditor.goto('/audit?event=' + hiddenEvent!.id)
    await expect(auditor.getByRole('alert')).toContainText('事件不存在或已超出访问范围')
    await expect(event).toHaveCount(0)
    await expect(auditor.getByRole('button', { name: '审计设置' })).toHaveCount(0)
    await auditor.getByLabel('事件名称').fill('device.approved')
    await auditor.getByRole('button', { name: '查询', exact: true }).click()
    await expect(auditor.getByRole('row').filter({ hasText: 'device.approved' })).toHaveCount(1)
    const artifact = auditor.waitForEvent('download')
    await auditor.getByRole('button', { name: '导出 CSV' }).click()
    const csv = await (await artifact).path()
    if (!csv) throw new Error('CSV path missing')
    const content = await readFile(csv, 'utf8')
    expect(content).toContain(includedID)
    expect(content).not.toContain(excludedID)
    const auditorPost = await requestAs(auditor, baseURL)
    await auditorPost('/v1/admin/audit-retention', { days: 120 }, 403)
  } finally {
    await auditor.close()
  }

  const anonymous = await browser.newPage({ baseURL })
  try {
    await anonymous.goto('/tasks')
    await expect(anonymous.getByRole('heading', { name: '登录端域控制台' })).toBeVisible()
    expect((await anonymous.request.get('/v1/overview')).status()).toBe(401)
    expect((await anonymous.request.get('/v1/admin/tasks')).status()).toBe(401)
  } finally {
    await anonymous.close()
  }
})
