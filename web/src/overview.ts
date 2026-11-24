export type TaskState = 'waiting' | 'queued' | 'running' | 'succeeded' | 'failed' | 'canceled'
export type TaskSummary = Record<TaskState, number> & {
  id: string
  kind: 'user_import' | 'device_enrollment' | 'center_install' | 'delivery' | 'relay_enrollment' | 'relay_lifecycle' | 'windows_delivery'
  created_at: string
  device_id?: string
  group?: string
}

export const taskStates: Record<TaskState, string> = {
  waiting: '待审批', queued: '待执行', running: '执行中', succeeded: '成功', failed: '失败', canceled: '已取消',
}
export const taskKinds = {
  user_import: '账号导入', device_enrollment: '设备入网', center_install: '中心安装',
  delivery: '交付任务', relay_enrollment: '中继登记', relay_lifecycle: '中继操作',
  windows_delivery: 'Windows 开发包验证',
}

export function taskProgress(item: TaskSummary) {
  return (Object.keys(taskStates) as TaskState[]).filter(key => item[key] > 0).map(key => {
    const label = key === 'waiting' && (item.kind === 'center_install' || item.kind === 'relay_enrollment') ? '待执行' : taskStates[key]
    return `${label} ${item[key]}`
  }).join(' · ')
}

export type Overview = {
  organization: string
  can_manage: boolean
  can_read_users: boolean
  can_read_devices: boolean
  can_read_audit: boolean
  can_read_tasks: boolean
  can_read_windows_releases?: boolean
  metrics: {
    active_sessions: number
    users?: number
    online_devices?: number
    pending_approvals?: number
    address_books: number
    retryable_targets?: number
    available_relays?: number
  }
  recent_tasks: TaskSummary[]
  recent_audit: { id: string; action: string; occurred_at: string; receipt_state: 'confirmed' | 'pending' }[]
}
