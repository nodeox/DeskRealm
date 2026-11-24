import { expect, request, test, type Page } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { createHash, randomBytes, randomUUID } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import { enroll, initialize, password, requestAs, signIn } from './resource-fixtures'

const sessions = '/v1/admin/sessions'
const rules = '/v1/admin/access-rules'
const policies = '/v1/admin/strategies'

function fixtureSQL(sql: string) {
  const container = process.env.DESKREALM_BROWSER_DB_CONTAINER
  if (!container?.startsWith('deskrealm-test-deskrealm-m1.')) throw new Error('Temporary browser database required')
  execFileSync('docker', ['exec', '-i', container, 'psql', '-Xq', '--set', 'ON_ERROR_STOP=1',
    '--dbname', 'deskrealm', '--username', 'postgres'], { input: sql })
}

function uuid(value: string) {
  if (!/^[a-f\d]{8}(-[a-f\d]{4}){3}-[a-f\d]{12}$/.test(value)) throw new Error('Invalid fixture UUID')
  return "'" + value + "'::uuid"
}

function seedSession(controller: string, target: string, user: string, rule: string, path: 'direct' | 'relay', redeemed: boolean, node?: string) {
  const id = randomUUID()
  const connection = randomUUID()
  const receipt = 'drc_' + randomBytes(32).toString('base64url')
  const digest = createHash('sha256').update(receipt).digest('hex')
  const relay = path === 'relay' ? (node ? uuid(node) + '::text' : "'qa-relay'") : 'NULL'
  fixtureSQL(`BEGIN;
    INSERT INTO managed_sessions (
      id,grant_id,organization_id,user_id,controller_device_id,target_device_id,controller_digest,request_id,request_hash,
      purpose,requested_actions,controller_rustdesk_id,target_rustdesk_id,controller_key,target_key,policy_version,
      rule_id,rule_version,allowed_mask,confirmation_mode,signing_kid,signature,issued_at,expires_at
    ) SELECT ${uuid(id)},gen_random_uuid(),u.organization_id,u.id,c.id,d.id,decode(repeat('00',32),'hex'),
      gen_random_uuid(),decode(repeat('00',32),'hex'),'helpdesk',ARRAY['view'],c.rustdesk_id,d.rustdesk_id,
      decode(repeat('00',32),'hex'),decode(repeat('00',32),'hex'),1,${uuid(rule)},1,1,'attended','browser',
      decode(repeat('00',64),'hex'),statement_timestamp(),statement_timestamp()+interval '60 seconds'
      FROM users u, devices c, devices d
      WHERE u.id=${uuid(user)} AND c.id=${uuid(controller)} AND d.id=${uuid(target)};
    INSERT INTO managed_session_routes(session_id,connection_id,path,relay_id)
      VALUES(${uuid(id)},${uuid(connection)},'${path}',${relay});
    ${redeemed ? `INSERT INTO managed_session_redemptions(
      session_id,connection_id,path,relay_id,proof_hash,allowed_mask,redeemed_at,lease_until,receipt_digest
    ) VALUES(${uuid(id)},${uuid(connection)},'${path}',${relay},decode(repeat('00',32),'hex'),1,
      statement_timestamp(),statement_timestamp()+interval '30 seconds',decode('${digest}','hex'));` : ''}
    COMMIT;`)
  return { id, connection, receipt }
}

function seedRegionalHistory(session: ReturnType<typeof seedSession>, user: string, first: string, second: string) {
  const round = randomUUID()
  fixtureSQL(`BEGIN;
    WITH submitted AS (
      INSERT INTO tasks(id,organization_id,kind,owner_id)
      SELECT gen_random_uuid(),organization_id,'relay_enrollment',id FROM users WHERE id=${uuid(user)}
      RETURNING id,organization_id
    ) INSERT INTO relay_nodes(id,organization_id,task_id,name,site,region,endpoint,build,capacity)
      SELECT ${uuid(first)},organization_id,id,'华东中继','上海','east','relay-a.invalid:21117','1.1.16',4 FROM submitted;
    WITH submitted AS (
      INSERT INTO tasks(id,organization_id,kind,owner_id)
      SELECT gen_random_uuid(),organization_id,'relay_enrollment',id FROM users WHERE id=${uuid(user)}
      RETURNING id,organization_id
    ) INSERT INTO relay_nodes(id,organization_id,task_id,name,site,region,endpoint,build,capacity)
      SELECT ${uuid(second)},organization_id,id,'华南中继','深圳','south','relay-b.invalid:21117','1.1.16',4 FROM submitted;
    INSERT INTO session_relay_rounds(id,session_id,connection_id,version,controller_address,target_address,opened_at,deadline)
      VALUES(${uuid(round)},${uuid(session.id)},${uuid(session.connection)},1,'192.0.2.10','198.51.100.20',
        statement_timestamp()-interval '3 seconds',statement_timestamp()-interval '1 second');
    INSERT INTO session_relay_candidates(round_id,relay_id,site,region,endpoint,build,identity_key,config_version,node_revision)
      SELECT ${uuid(round)},id,site,region,endpoint,build,decode(repeat('00',32),'hex'),1,1 FROM relay_nodes
      WHERE id IN (${uuid(first)},${uuid(second)});
    INSERT INTO session_relay_reports(round_id,relay_id,role,samples,received_at) VALUES
      (${uuid(round)},${uuid(first)},1,'[{"result":"ok","rtt_us":10000},{"result":"ok","rtt_us":14000},{"result":"timeout"}]',statement_timestamp()),
      (${uuid(round)},${uuid(first)},2,'[{"result":"ok","rtt_us":20000},{"result":"ok","rtt_us":26000},{"result":"timeout"}]',statement_timestamp());
    INSERT INTO session_relay_selections(round_id,relay_id,attempt,reason,combined_rtt_us,selected_at,failed_at) VALUES
      (${uuid(round)},${uuid(first)},1,'dual_rtt',35000,statement_timestamp(),statement_timestamp()),
      (${uuid(round)},${uuid(second)},2,'geoip',NULL,statement_timestamp(),NULL);
    COMMIT;`)
}

async function confirm(page: Page, name: string, button: string) {
  const dialog = page.getByRole('dialog', { name })
  await expect(dialog).toBeVisible()
  await dialog.getByRole('button', { name: button, exact: true }).click()
  await expect(dialog).toBeHidden()
}

test('策略、设备回执和会话按实际授权与状态运行', async ({ page, browser, baseURL }) => {
  test.setTimeout(120_000)
  const ticket = process.env.DESKREALM_BROWSER_TICKET
  if (!ticket || !baseURL) throw new Error('Run scripts/deskrealm-m2-test managed-browser')
  expect((await page.request.get(sessions)).status()).toBe(401)
  await initialize(page, ticket, '受管工作台测试')
  const post = await requestAs(page, baseURL)
  const names = { manager: 'policy-manager', initiator: 'policy-owner', auditor: 'policy-auditor', other: 'policy-other' }
  const users = new Map<string, string>()
  for (const username of Object.values(names)) {
    users.set(username, (await post<{ id: string }>('/v1/admin/users', { username, password }, 201)).id)
  }
  const initiator = users.get(names.initiator)!
  const local = await post<{ id: string }>('/v1/admin/device-groups', { name: '研发终端' }, 201)
  const hidden = await post<{ id: string }>('/v1/admin/device-groups', { name: '隔离终端' }, 201)
  await post('/v1/admin/device-groups/' + local.id + '/grants', { username: names.manager, can_approve: true }, 204)
  await post('/v1/admin/audit-grants', { username: names.auditor, scope: 'device_group', group: '研发终端', can_export: true }, 204)
  const controller = await enroll(post, local.id, '42131001')
  const target = await enroll(post, local.id, '42131002')
  const outside = await enroll(post, hidden.id, '42131003')
  for (const id of [controller, target, outside]) await post('/v1/admin/devices/' + id + '/approve', {}, 200)
  const role = await post<{ id: string }>('/v1/admin/control-roles', { name: '仅看画面', allowed_actions: ['view'] }, 201)

  await page.goto('/policies')
  await page.getByRole('button', { name: '新建规则' }).click()
  await page.getByLabel('规则名称').fill('研发协助')
  await page.getByLabel('规则来源').selectOption(initiator)
  await page.getByLabel('规则目标').selectOption(target)
  await page.getByLabel('控制角色').selectOption(role.id)
  await page.getByRole('group', { name: '允许的操作' }).getByLabel('画面').check()
  await page.getByRole('button', { name: '保存规则草稿' }).click()
  const ruleRow = page.getByRole('row').filter({ hasText: '研发协助' })
  await expect(ruleRow).toContainText('待发布')

  await page.getByLabel('预览来源账号').selectOption(initiator)
  await page.getByLabel('预览主控设备').selectOption(controller)
  await page.getByLabel('预览目标设备').selectOption(target)
  await page.getByRole('button', { name: '预览连接规则' }).click()
  await expect(page.getByText('端点离线、未批准或不满足连接条件')).toBeVisible()
  // Simulate authenticated heartbeat records inside the isolated test database.
  fixtureSQL(`UPDATE devices SET managed_heartbeat_at=clock_timestamp(),managed_heartbeat_epoch=device_key_epoch,
    managed_reported_build='browser-test',managed_heartbeat_protocol=1 WHERE id IN (${uuid(controller)},${uuid(target)});`)
  await page.getByRole('button', { name: '预览连接规则' }).click()
  await expect(page.getByText('没有覆盖全部所需操作的允许规则')).toBeVisible()
  await ruleRow.getByRole('button', { name: '发布', exact: true }).click()
  await confirm(page, '确认发布规则', '确认提交')
  await expect(ruleRow).toContainText('已发布 1')
  await page.getByRole('button', { name: '预览连接规则' }).click()
  await expect(page.getByText('匹配已发布的允许规则')).toBeVisible()
  await page.getByRole('group', { name: '申请的操作' }).getByLabel('键鼠').check()
  await page.getByRole('button', { name: '预览连接规则' }).click()
  await expect(page.getByText('没有覆盖全部所需操作的允许规则')).toBeVisible()
  const { rules: available } = await (await page.request.get(rules)).json() as { rules: { id: string }[] }
  const rule = available[0].id

  await page.getByRole('button', { name: '设备策略', exact: true }).click()
  for (const name of ['值班基线', '小范围灰度']) {
    await page.getByRole('button', { name: '新建策略版本' }).click()
    await page.getByLabel('策略名称').fill(name)
    await page.getByRole('button', { name: '发布策略', exact: true }).click()
    await confirm(page, '确认发布策略版本', '确认提交')
    await expect(page.getByText(new RegExp('^' + name + ' · 版本 '))).toBeVisible()
  }
  await page.getByRole('button', { name: '新建分配' }).click()
  await page.getByLabel('分配范围').selectOption('device')
  await page.getByLabel('分配对象').selectOption(target)
  await page.getByLabel('基线版本').selectOption('1')
  await page.getByLabel('灰度版本').selectOption('2')
  await page.getByLabel('灰度比例').fill('30')
  await page.getByRole('button', { name: '保存分配' }).click()
  await confirm(page, '确认策略分配', '确认提交')
  const assignment = page.getByRole('row').filter({ hasText: '42131002' })
  await expect(assignment).toContainText('基线版本 1 · 灰度版本 2（30%）')
  await assignment.getByRole('button', { name: '编辑分配' }).click()
  await page.getByLabel('灰度版本').selectOption({ label: '关闭灰度' })
  await page.getByRole('button', { name: '保存分配' }).click()
  await confirm(page, '确认策略分配', '确认提交')
  await expect(assignment).not.toContainText('灰度版本')
  await assignment.getByRole('button', { name: '回滚', exact: true }).click()
  await page.getByRole('button', { name: '查看历史修订' }).click()
  await expect(page.getByRole('status').filter({ hasText: '基线版本 1 · 灰度版本 2（30%）' })).toBeVisible()
  await page.getByRole('button', { name: '回滚到此修订' }).click()
  await confirm(page, '确认回滚分配', '确认提交')
  await expect(assignment).toContainText('基线版本 1 · 灰度版本 2（30%）')
  await expect(assignment.getByRole('cell', { name: '3', exact: true })).toBeVisible()
  await assignment.getByRole('link', { name: '查看设备回执' }).click()
  const device = page.getByRole('dialog', { name: '设备详情' })
  await expect(device.getByText('等待应用回执')).toBeVisible()
  fixtureSQL(`INSERT INTO managed_policy_receipts(delivery_id,result,failed_mask,signature,signer_key)
    SELECT managed_policy_delivery_id,'failed',1,decode(repeat('00',64),'hex'),decode(repeat('00',32),'hex')
    FROM devices WHERE id=${uuid(target)};`)
  await device.getByRole('button', { name: '刷新设备策略' }).click()
  await expect(device.getByText('本机应用失败')).toBeVisible()
  await expect(device.getByText('失败字段')).toBeVisible()
  await device.getByRole('button', { name: '重试应用策略' }).click()
  await expect(device.getByText('等待应用回执')).toBeVisible()
  await device.getByLabel('归属用户 UUID').fill(initiator)
  await device.getByRole('button', { name: '保存策略归属' }).click()
  await expect(device.getByText(initiator, { exact: true })).toBeVisible()

  const manager = await browser.newPage({ baseURL, viewport: { width: 375, height: 812 } })
  const owner = await browser.newPage({ baseURL })
  const auditor = await browser.newPage({ baseURL, acceptDownloads: true })
  const endpoint = await request.newContext({ baseURL })
  try {
    await Promise.all([signIn(manager, names.manager), signIn(owner, names.initiator), signIn(auditor, names.auditor)])
    await manager.goto('/policies')
    await expect(manager).toHaveURL(/\/overview$/)
    expect((await manager.request.get(policies)).status()).toBe(403)

    // Feed projections synthetic pre-existing grants and redemptions without a desktop client.
    const direct = seedSession(controller, target, initiator, rule, 'direct', true)
    const relayed = seedSession(controller, outside, users.get(names.other)!, rule, 'relay', true)
    const pending = seedSession(controller, target, initiator, rule, 'relay', false)

    await page.goto('/overview')
    await expect(page.getByRole('link', { name: /^活动会话/ })).toHaveText('活动会话2')
    await page.getByRole('link', { name: /^活动会话/ }).click()
    await expect(page.getByRole('row').filter({ hasText: relayed.id })).toContainText('中继 · qa-relay')
    await expect(page.getByRole('row').filter({ hasText: pending.id })).toContainText('待核销确认')

    await manager.goto('/sessions')
    const managerRow = manager.getByRole('row').filter({ hasText: direct.id })
    await expect(managerRow).toContainText('直连')
    await expect(manager.getByRole('row').filter({ hasText: relayed.id })).toHaveCount(0)
    expect((await manager.request.get(sessions + '/' + relayed.id)).status()).toBe(404)
    expect(await manager.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375)
    const detailsButton = managerRow.getByRole('button', { name: '详情' })
    await detailsButton.focus()
    await detailsButton.press('Enter')
    const detail = manager.getByRole('dialog', { name: '会话详情' })
    await expect(detail.getByRole('status')).toHaveText('活动中')
    await manager.keyboard.press('Escape')
    await expect(detailsButton).toBeFocused()
    await detailsButton.press('Enter')
    await detail.getByRole('button', { name: '撤销会话' }).click()
    const revoke = manager.getByRole('dialog', { name: '确认撤销会话' })
    await expect(revoke.getByRole('button', { name: '确认撤销' })).toBeDisabled()
    await revoke.getByLabel('撤销原因').fill('工作台验收撤销')
    await confirm(manager, '确认撤销会话', '确认撤销')
    await expect(detail.getByRole('status')).toContainText('等待端点回执或租约到期')

    await owner.goto('/sessions?session=' + direct.id)
    await expect(owner.getByRole('dialog', { name: '会话详情' })).toContainText(direct.id)
    await expect(owner.getByRole('button', { name: '撤销会话' })).toHaveCount(0)
    await (await requestAs(owner, baseURL))(sessions + '/' + pending.id + '/revoke', { reason: '超出授权' }, 403)
    expect((await auditor.request.get(sessions + '/' + direct.id)).status()).toBe(404)
    await manager.goto('/sessions?session=' + relayed.id)
    await expect(manager.getByRole('alert')).toBeVisible()
    await expect(manager.getByRole('dialog', { name: '会话详情' })).toHaveCount(0)

    const firstRelay = randomUUID()
    const actualRelay = randomUUID()
    const regional = seedSession(controller, target, initiator, rule, 'relay', true, actualRelay)
    seedRegionalHistory(regional, initiator, firstRelay, actualRelay)
    await manager.goto('/sessions?session=' + regional.id)
    const regionalDetail = manager.getByRole('dialog', { name: '会话详情' })
    await expect(regionalDetail.getByText('中继 · ' + actualRelay, { exact: true })).toBeVisible()
    const history = regionalDetail.getByRole('table', { name: '第 1 轮中继样本' })
    const firstAttempt = history.getByRole('row').filter({ hasText: '华东中继' })
    await expect(firstAttempt).toContainText('12 ms')
    await expect(firstAttempt).toContainText('23 ms')
    await expect(firstAttempt).toContainText('超时')
    await expect(firstAttempt).toContainText('连接失败，已排除')
    await expect(firstAttempt).toContainText('双端合计 35 ms')
    const actualAttempt = history.getByRole('row').filter({ hasText: '华南中继' })
    await expect(actualAttempt).toContainText('缺少报告')
    await expect(actualAttempt).toContainText('实际核销')
    await expect(actualAttempt).toContainText('GeoIP 区域回退')
    expect(await manager.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375)
    expect((await auditor.request.get(sessions + '/' + regional.id)).status()).toBe(404)

    fixtureSQL(`UPDATE managed_session_redemptions SET redeemed_at=statement_timestamp()-interval '31 seconds',
      lease_until=statement_timestamp()-interval '1 second' WHERE session_id=${uuid(direct.id)};`)
    await page.goto('/sessions?session=' + direct.id)
    const adminDetail = page.getByRole('dialog', { name: '会话详情' })
    await expect(adminDetail.getByRole('status')).toHaveText('租约已到期 · 等待端点回执')
    const receipt = await endpoint.post('/v1/sessions/' + direct.id + '/stopped', {
      headers: { Authorization: 'Bearer ' + direct.receipt },
      data: { connection_id: direct.connection, lease_sequence: 1, reason: 'revoked' },
    })
    expect(receipt.status(), await receipt.text()).toBe(204)
    await adminDetail.getByRole('button', { name: '刷新详情' }).click()
    await expect(adminDetail.getByRole('status')).toHaveText('已收到端点停止回执')
    await expect(adminDetail.getByText(/中心收到停止回执 ·/)).toBeVisible()
    await adminDetail.getByRole('link', { name: '查看会话审计' }).click()
    await expect(page.getByLabel('关联 ID')).toHaveValue(direct.id)
    await expect(page.getByRole('rowheader', { name: /^收到会话停止回执/ })).toBeVisible()

    await auditor.goto('/audit?session_id=' + direct.id)
    await expect(auditor.getByRole('rowheader', { name: /^请求撤销会话/ })).toBeVisible()
    const download = auditor.waitForEvent('download')
    await auditor.getByRole('button', { name: '导出 CSV' }).click()
    const exportPath = await (await download).path()
    const csv = await readFile(exportPath, 'utf8')
    expect(csv).toContain(direct.id)
    expect(csv).toContain('session.stopped')
    expect(csv).not.toContain(relayed.id)
    expect(csv).not.toContain('device.approved')
  } finally {
    await Promise.all([manager.close(), owner.close(), auditor.close(), endpoint.dispose()])
  }
})
