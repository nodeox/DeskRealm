export const auditLabels: Record<string, string> = {
  'organization.initialized': '初始化组织', 'user.login': '登录', 'user.logout': '退出登录',
  'user.created': '创建账号', 'user.disabled': '停用账号', 'user.active': '启用账号',
  'device.approved': '批准设备', 'device.rejected': '拒绝设备',
  'device.enrollment_pending': '登记设备', 'device.enrollment_issued': '签发入网凭据',
  'device.moved': '设备移组', 'address_book.shared': '共享通讯录', 'address_book.share_revoked': '撤销通讯录共享',
  'audit.exported': '导出审计', 'audit.granted': '授予审计权限', 'audit.grant_revoked': '撤销审计权限',
  'audit.retention_changed': '更新保留期', 'audit.retention_pruned': '清理过期审计',
  'user.import_batch': '导入账号', 'task.retry_requested': '提交任务重试', 'task.target_finished': '完成任务执行',
}
