import { expect, type Page } from '@playwright/test'
import { generateKeyPairSync, sign } from 'node:crypto'

export const password = 'deskrealm-browser-test-passphrase'

export async function signIn(page: Page, username: string) {
  await page.goto('/login')
  await page.getByLabel('账号').fill(username)
  await page.getByLabel('密码', { exact: true }).fill(password)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page.getByRole('heading', { name: '概览', exact: true })).toBeVisible()
}

export async function initialize(page: Page, ticket: string, organization: string) {
  await page.goto(ticket)
  await page.getByLabel('组织名称').fill(organization)
  await page.getByLabel('账号').fill('admin')
  await page.getByLabel('密码', { exact: true }).fill(password)
  await page.getByRole('button', { name: '初始化中心' }).click()
  await expect(page.getByRole('heading', { name: '登录端域控制台' })).toBeVisible()
  await signIn(page, 'admin')
}

export async function requestAs(page: Page, origin: string) {
  const session = await page.request.get('/v1/auth/session')
  expect(session.ok()).toBeTruthy()
  const { csrf_token } = await session.json() as { csrf_token: string }
  return async <T = unknown>(path: string, data: object, status: number): Promise<T> => {
    const response = await page.request.post(path, { data, headers: { Origin: origin, 'X-CSRF-Token': csrf_token } })
    expect(response.status(), path).toBe(status)
    return status === 204 ? undefined as T : response.json()
  }
}

export async function enroll(post: Awaited<ReturnType<typeof requestAs>>, groupID: string, id: string) {
  const grant = await post<{ credential: string }>('/v1/admin/enrollment-credentials', {
    group_id: groupID, platform: 'windows-amd64', purpose: '自动化验证', lifetime_minutes: 10,
  }, 201)
  const keys = generateKeyPairSync('ed25519')
  const point = keys.publicKey.export({ format: 'jwk' }).x
  if (!point) throw new Error('Ed25519 public key missing')
  const input = {
    credential: grant.credential, group_id: groupID, platform: 'windows-amd64', rustdesk_id: id,
    public_key: Buffer.from(point, 'base64url').toString('base64'),
  }
  const challenge = await post<{ attempt_id: string; nonce: string }>('/v1/enrollment/challenge', input, 200)
  const message = Buffer.from([
    'deskrealm/device-enrollment/v1', challenge.attempt_id, challenge.nonce, groupID,
    input.platform, id, input.public_key,
  ].join('\n'))
  const result = await post<{ device_id: string }>('/v1/enrollment/complete', {
    ...input, attempt_id: challenge.attempt_id, signature: sign(null, message, keys.privateKey).toString('base64'),
  }, 200)
  return result.device_id
}
