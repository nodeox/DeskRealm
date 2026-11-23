export const actionLabels: Record<string, string> = {
  view: '画面', input: '键鼠', audio: '音频', clipboard: '剪贴板', file_transfer: '文件传输',
}

export const scopeLabels: Record<string, string> = {
  organization: '组织默认', group: '设备组', user: '归属用户', device: '设备',
}

export type PolicyReceipt = {
  owner_user_id?: string
  device_version: number
  delivery_id?: string
  expected_policy_version: number | null
  applied_policy_version: number | null
  state: 'unassigned' | 'pending' | 'applied' | 'failed'
  failed_fields?: string[]
  received_at?: string
}

export const receiptLabels = {
  unassigned: '未分配', pending: '等待应用回执', applied: '已应用', failed: '本机应用失败',
}

export const fieldLabels: Record<string, string> = {
  allowed_actions: '允许的操作', unattended_purposes: '无人值守用途', schema_version: '策略格式版本',
}

export function localTime(value?: string) {
  return value ? new Date(value).toLocaleString('zh-CN') : '—'
}

export function actionsText(actions: string[] = []) {
  return actions.map(action => actionLabels[action] ?? action).join('、') || '无'
}
