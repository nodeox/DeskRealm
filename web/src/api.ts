const messages: Record<string, string> = {
  already_initialized: '中心已完成初始化，请登录。',
  invalid_or_expired_ticket: '初始化链接无效或已过期。请在中心主机重新生成链接。',
  invalid_organization: '请输入 1 到 80 个字符的组织名称。',
  invalid_username: '账号须以英文字母开头，长度为 3 到 40 位，可包含数字、点、下划线和连字符。',
  invalid_password: '密码至少需要 12 个字符。',
  invalid_credentials: '账号或密码有误，请重新输入。',
  try_again_later: '请求过于频繁，请稍后重试。',
  origin_denied: '当前访问地址与中心配置不一致。',
  service_error: '中心暂时无法处理请求，请稍后重试。',
  username_in_use: '该账号已存在。',
  last_platform_admin: '需保留至少一位启用的平台管理员。',
  user_not_found: '该账号已不存在，请刷新列表。',
  invalid_user_transition: '当前账号状态不允许此操作。',
  invalid_reason: '请输入 1 到 200 个字符的操作原因。',
  forbidden: '当前账号没有管理该资源的权限。',
  invalid_or_expired_identity_link: '链接已使用、过期或无效，请联系管理员重新发放。',
  invalid_import: '请输入 1 到 100 个格式正确且不重复的账号。',
  import_request_conflict: '本次导入内容已变更，请重新提交。',
  invalid_verification_code: '验证码错误、已使用或登录请求已过期。',
  totp_enrollment_unavailable: '当前验证器状态不允许此操作。',
  totp_unavailable: '中心暂时无法验证两步验证码。',
  invalid_group: '请填写 1 到 80 个字符的用户组名称。',
  group_name_in_use: '这个用户组名称已存在。',
  group_not_found: '用户组不存在，请刷新页面。',
  invalid_grant: '请检查账号、角色和授权范围。',
  invalid_api_token_request: '请检查用途、作用域和有效期。',
  invalid_admin_transfer: '请选择已启用并设置本地密码的其他账号。',
  invalid_control_role: '请检查角色名称和允许的操作。',
  control_role_name_in_use: '该控制角色名称已使用。',
  control_role_stale: '该角色已变更，请查看最新版本后编辑。',
  device_group_name_in_use: '该设备组名称已使用。',
  device_group_not_found: '设备组不存在或当前账号无权操作。',
  device_not_found: '设备不存在或当前账号无权操作。',
  invalid_enrollment_request: '请检查设备组、设备平台、凭据用途和有效期。',
  device_decision_conflict: '设备已处理，请刷新列表。',
}

export class RequestError extends Error {
  constructor(readonly status: number, readonly code: string) {
    super(messages[code] ?? '请求失败，请重试。')
  }
}

export async function api<T>(path: string, options?: RequestInit): Promise<T> {
  const response = await fetch(path, options)
  if (!response.ok) {
    const data = await response.json().catch(() => null) as { error?: string } | null
    throw new RequestError(response.status, data?.error ?? 'service_error')
  }
  if (response.status === 204) return undefined as T
  const type = response.headers.get('Content-Type')
  if (response.status === 201 && !type) return undefined as T
  return response.json() as Promise<T>
}

export function json(value: object, csrf?: string): RequestInit {
  const headers = new Headers({ 'Content-Type': 'application/json' })
  if (csrf) headers.set('X-CSRF-Token', csrf)
  return { method: 'POST', headers, body: JSON.stringify(value) }
}
