import { expect, test } from '@playwright/test'
import { createHmac, generateKeyPairSync, sign } from 'node:crypto'

function verificationCode(secret: string, step: number): string {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  const bits = [...secret].map(letter => alphabet.indexOf(letter).toString(2).padStart(5, '0')).join('')
  const key = Buffer.from((bits.match(/.{8}/g) ?? []).map(value => parseInt(value, 2)))
  const counter = Buffer.alloc(8)
  counter.writeBigUInt64BE(BigInt(step))
  const hash = createHmac('sha1', key).update(counter).digest()
  const offset = hash[hash.length - 1] & 15
  return ((hash.readUInt32BE(offset) & 0x7fffffff) % 1000000).toString().padStart(6, '0')
}

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

  await page.getByRole('button', { name: '导入账号', exact: true }).click()
  await page.getByLabel('待导入账号').fill('invite01')
  await page.getByRole('button', { name: '导入', exact: true }).click()
  const invitedRow = page.getByRole('row').filter({ hasText: 'invite01' })
  await expect(invitedRow).toContainText('待激活')
  await invitedRow.getByRole('button', { name: '补发邀请' }).click()
  const linkInput = page.getByLabel('一次性账号链接')
  await expect(linkInput).toHaveValue(/\/accept-invitation#ticket=/)
  const invited = await browser.newPage()
  try {
    await invited.goto(await linkInput.inputValue())
    await expect(invited).toHaveURL(/\/accept-invitation$/)
    await invited.getByLabel('新密码').fill('invited-test-passphrase')
    await invited.getByRole('button', { name: '设置密码' }).click()
    await expect(invited.getByRole('heading', { name: '登录端域控制台' })).toBeVisible()
    await page.reload()
    await expect(invitedRow).toContainText('已启用')
    await invitedRow.getByRole('button', { name: '恢复账号' }).click()
    await page.getByRole('dialog', { name: '恢复账号' }).getByRole('button', { name: '确认恢复' }).click()
    await expect(linkInput).toHaveValue(/\/recover#ticket=/)
    await invited.goto(await linkInput.inputValue())
    await expect(invited).toHaveURL(/\/recover$/)
    await invited.getByLabel('新密码').fill('recovered-test-passphrase')
    await invited.getByRole('button', { name: '设置密码' }).click()
    await invited.getByLabel('账号').fill('invite01')
    await invited.getByLabel('密码', { exact: true }).fill('recovered-test-passphrase')
    await invited.getByRole('button', { name: '登录', exact: true }).click()
    await expect(invited.getByRole('heading', { name: '概览', exact: true })).toBeVisible()
  } finally {
    await invited.close()
  }

  await page.getByRole('link', { name: '用户组与委派' }).click()
  await page.getByLabel('新用户组名称').fill('运维一组')
  await page.getByRole('button', { name: '创建用户组' }).click()
  await page.getByLabel('成员账号').fill('member01')
  await page.getByRole('button', { name: '加入用户组' }).click()
  await expect(page.getByRole('listitem').filter({ hasText: 'member01' })).toBeVisible()
  await page.getByLabel('授权账号').fill('invite01')
  await page.getByLabel('授权用户组').selectOption({ label: '运维一组' })
  await page.getByRole('button', { name: '授予角色' }).click()
  await expect(page.getByRole('listitem').filter({ hasText: 'invite01' })).toContainText('用户查看员')
  await page.getByRole('button', { name: '新建控制角色' }).click()
  await page.getByLabel('控制角色名称').fill('只读运维')
  await page.getByRole('button', { name: '保存控制角色' }).click()
  await expect(page.getByRole('listitem').filter({ hasText: '只读运维' })).toContainText('画面')

  await page.getByRole('link', { name: 'API 令牌' }).click()
  await page.getByLabel('用途').fill('账号核对')
  await page.getByLabel('用户组').selectOption({ label: '运维一组' })
  await page.getByRole('button', { name: '生成令牌' }).click()
  await expect(page.getByLabel('新令牌')).toHaveValue(/^[A-Za-z0-9_-]{43}$/)
  const tokenRow = page.getByRole('row').filter({ hasText: '账号核对' })
  await tokenRow.getByRole('button', { name: '撤销令牌' }).click()
  await page.getByRole('dialog', { name: '撤销 API 令牌' }).getByRole('button', { name: '确认撤销' }).click()
  await expect(tokenRow).toContainText('已撤销')

  await page.getByRole('link', { name: '设备管理' }).click()
  await page.getByRole('button', { name: '新建设备组' }).click()
  await page.getByLabel('设备组名称').fill('试点终端组')
  const groupResponse = page.waitForResponse(response =>
    response.url().endsWith('/v1/admin/device-groups') && response.request().method() === 'POST')
  await page.getByRole('button', { name: '创建设备组' }).click()
  const deviceGroup = await (await groupResponse).json() as { id: string }
  await expect(page.getByLabel('目标设备组')).toHaveValue(deviceGroup.id)
  await page.getByRole('button', { name: '发放入网凭据' }).click()
  await page.getByLabel('凭据用途').fill('浏览器纳管验证')
  await page.getByRole('button', { name: '生成入网凭据' }).click()
  const receipt = page.getByLabel('一次性入网凭据')
  await expect(receipt).toHaveValue(/^[A-Za-z0-9_-]{43}$/)

  const keys = generateKeyPairSync('ed25519')
  const publicKey = keys.publicKey.export({ format: 'jwk' }).x
  if (!publicKey) throw new Error('Missing Ed25519 public key')
  const enrollment = {
    credential: await receipt.inputValue(),
    group_id: deviceGroup.id,
    platform: 'windows-amd64',
    rustdesk_id: '10101011',
    public_key: Buffer.from(publicKey, 'base64url').toString('base64'),
  }
  const challengeResponse = await page.request.post('/v1/enrollment/challenge', { data: enrollment })
  expect(challengeResponse.ok()).toBeTruthy()
  const challenge = await challengeResponse.json() as { attempt_id: string; nonce: string }
  const message = Buffer.from([
    'deskrealm/device-enrollment/v1', challenge.attempt_id, challenge.nonce, enrollment.group_id,
    enrollment.platform, enrollment.rustdesk_id, enrollment.public_key,
  ].join('\n'))
  const completion = await page.request.post('/v1/enrollment/complete', {
    data: { ...enrollment, attempt_id: challenge.attempt_id, signature: sign(null, message, keys.privateKey).toString('base64') },
  })
  expect(completion.ok()).toBeTruthy()
  await page.reload()
  const enrolled = page.getByRole('row').filter({ hasText: enrollment.rustdesk_id })
  await expect(enrolled).toContainText('待审批')
  await expect(enrolled).toContainText('未知')
  await expect(enrolled).toContainText('未下发')
  await enrolled.getByRole('button', { name: '批准', exact: true }).click()
  await page.getByRole('dialog', { name: '批准设备登记' }).getByRole('button', { name: '确认批准' }).click()
  await expect(enrolled).toContainText('已批准')

  await page.getByRole('link', { name: '概览', exact: true }).click()
  await page.setViewportSize({ width: 375, height: 800 })
  await page.reload()
  await expect(page.getByRole('heading', { name: '概览', exact: true })).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375)
  await page.getByRole('button', { name: '退出登录' }).click()
  await expect(page.getByRole('heading', { name: '登录端域控制台' })).toBeVisible()
  await page.goto('/overview')
  await expect(page).toHaveURL(/\/login$/)

  await page.getByLabel('账号').fill('admin')
  await page.getByLabel('密码', { exact: true }).fill('test-passphrase-for-deskrealm')
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await page.getByRole('link', { name: '账号安全' }).click()
  await page.getByLabel('当前密码').fill('test-passphrase-for-deskrealm')
  await page.getByRole('button', { name: '生成验证器密钥' }).click()
  const secret = await page.getByLabel('验证器密钥').inputValue()
  const step = Math.floor(Date.now() / 30000)
  await page.getByLabel('当前密码').fill('test-passphrase-for-deskrealm')
  await page.getByLabel('验证码').fill(verificationCode(secret, step))
  await page.getByRole('button', { name: '确认并启用' }).click()
  await expect(page.getByRole('status').filter({ hasText: '已启用' })).toBeVisible()
  await page.getByRole('button', { name: '退出登录' }).click()
  await page.getByLabel('账号').fill('admin')
  await page.getByLabel('密码', { exact: true }).fill('test-passphrase-for-deskrealm')
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page.getByRole('heading', { name: '两步验证' })).toBeVisible()
  await page.getByLabel('验证码').fill(verificationCode(secret, step))
  await page.getByRole('button', { name: '验证登录' }).click()
  await expect(page.getByRole('alert')).toContainText('已使用')
  await page.getByLabel('验证码').fill(verificationCode(secret, step + 1))
  await page.getByRole('button', { name: '验证登录' }).click()
  await expect(page.getByRole('heading', { name: '概览', exact: true })).toBeVisible()
})
