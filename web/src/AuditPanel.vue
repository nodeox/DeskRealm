<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'
import AuditSettings from './AuditSettings.vue'

type Event = {
  id: string; actor_id?: string; actor_name?: string; action: string; result: string; source: string
  resource_id?: string; session_id?: string; task_id?: string; device_group_id?: string; occurred_at: string
  received_at: string; receipt_state: 'confirmed' | 'pending'; receipt_result?: string; acknowledged_at?: string
  duplicate_receipts: number; reason?: string
}

const props = defineProps<{ csrf: string; admin: boolean }>()
const emit = defineEmits<{ expired: [] }>()
const base = '/v1/admin/audit-events'
const events = ref<Event[]>([])
const cursor = ref('')
const filters = ref({ action: '', status: '', association: 'session_id', id: '', from: '', to: '' })
const applied = ref('')
const canExport = ref(false)
const showSettings = ref(false)
const selection = ref<Event>()
const details = ref<HTMLDialogElement>()
const busy = ref(false)
const error = ref('')
const labels: Record<string, string> = {
  'organization.initialized': '初始化组织', 'user.login': '登录', 'user.logout': '退出登录',
  'user.created': '创建账号', 'user.disabled': '停用账号', 'user.active': '启用账号',
  'device.approved': '批准设备', 'device.rejected': '拒绝设备',
  'device.enrollment_pending': '登记设备', 'device.enrollment_issued': '签发入网凭据',
  'device.moved': '设备移组', 'address_book.shared': '共享通讯录', 'address_book.share_revoked': '撤销通讯录共享',
  'audit.exported': '导出审计', 'audit.granted': '授予审计权限', 'audit.grant_revoked': '撤销审计权限',
  'audit.retention_changed': '更新保留期', 'audit.retention_pruned': '清理过期审计',
}

function status(event: Event) {
  if (event.receipt_state === 'pending') return '待回执'
  return event.duplicate_receipts ? '已确认 · 重复 ' + event.duplicate_receipts : '已确认'
}

function date(value: string) {
  return new Date(value).toLocaleString()
}

async function run(task: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try { await task() } catch (cause) {
    if (cause instanceof RequestError && cause.code === 'session_required') emit('expired')
    if (cause instanceof RequestError && cause.code === 'forbidden') {
      events.value = []
      cursor.value = ''
      canExport.value = false
      selection.value = undefined
    }
    error.value = cause instanceof RequestError ? cause.message : '无法完成请求，请重试。'
  } finally {
    busy.value = false
  }
}

async function load(query: string, after = '') {
  const params = new URLSearchParams(query)
  if (after) params.set('before', after)
  const encoded = params.toString()
  const result = await api<{ events: Event[]; next?: string; can_export: boolean }>(base + (encoded ? '?' + encoded : ''))
  events.value = after ? [...events.value, ...result.events] : result.events
  cursor.value = result.next ?? ''
  canExport.value = result.can_export
}

function search() {
  return run(async () => {
    const { action, status, association, id, from, to } = filters.value
    const query = new URLSearchParams()
    if (action.trim()) query.set('action', action.trim())
    if (status) query.set('status', status)
    if (id.trim()) query.set(association, id.trim())
    if (from) query.set('from', new Date(from).toISOString())
    if (to) query.set('to', new Date(to).toISOString())
    await load(query.toString())
    applied.value = query.toString()
  })
}

function exportCSV() {
  return run(async () => {
    const response = await fetch(base + '/export' + (applied.value ? '?' + applied.value : ''), json({}, props.csrf))
    if (!response.ok) {
      const failure = await response.json().catch(() => null) as { error?: string } | null
      throw new RequestError(response.status, failure?.error ?? 'service_error')
    }
    const location = URL.createObjectURL(await response.blob())
    const link = document.createElement('a')
    link.href = location
    link.download = 'deskrealm-audit.csv'
    link.click()
    window.setTimeout(() => URL.revokeObjectURL(location), 1000)
  })
}

function showDetail(id: string) {
  return run(async () => {
    selection.value = await api<Event>(base + '/' + id)
    await nextTick()
    details.value?.showModal()
  })
}

onMounted(() => run(() => load('')))
</script>

<template>
  <section class="users-surface" aria-labelledby="audit-heading">
    <div class="section-heading">
      <h2 id="audit-heading">审计事件</h2>
      <div class="user-tools">
        <button v-if="admin" class="secondary" type="button" :aria-expanded="showSettings" @click="showSettings = !showSettings">审计设置</button>
        <button v-if="canExport" class="secondary" type="button" :disabled="busy" @click="exportCSV">导出 CSV</button>
      </div>
    </div>
    <AuditSettings v-if="showSettings" :csrf="csrf" @expired="emit('expired')" @pruned="run(() => load(applied))" />
    <form class="audit-filters" @submit.prevent="search">
      <div class="field"><label for="audit-action">事件名称</label><input id="audit-action" v-model="filters.action" placeholder="例如 device.approved" maxlength="80"></div>
      <div class="field"><label for="audit-state">回执状态</label><select id="audit-state" v-model="filters.status">
        <option value="">全部</option><option value="confirmed">已确认</option><option value="pending">待回执</option><option value="duplicate">有重复回执</option>
      </select></div>
      <div class="field"><label for="audit-from">起始时间</label><input id="audit-from" v-model="filters.from" type="datetime-local"></div>
      <div class="field"><label for="audit-to">结束时间</label><input id="audit-to" v-model="filters.to" type="datetime-local"></div>
      <div class="field"><label for="audit-related">关联类型</label><select id="audit-related" v-model="filters.association">
        <option value="session_id">会话</option><option value="task_id">任务</option><option value="resource_id">资源</option>
      </select></div>
      <div class="field"><label for="audit-id">关联 ID</label><input id="audit-id" v-model="filters.id" maxlength="36"></div>
      <button class="primary" type="submit" :disabled="busy">查询</button>
    </form>
    <p v-if="error" role="alert" class="error">{{ error }}</p>
    <div class="table-wrap">
      <table>
        <thead><tr><th scope="col">事件</th><th scope="col">操作者</th><th scope="col">发生时间</th><th scope="col">回执</th><th scope="col">详情</th></tr></thead>
        <tbody>
          <tr v-for="event in events" :key="event.id">
            <th scope="row">{{ labels[event.action] || event.action }}<small>{{ event.action }}</small></th>
            <td>{{ event.actor_name || '服务' }}</td>
            <td>{{ date(event.occurred_at) }}</td>
            <td>{{ status(event) }}</td>
            <td><button class="secondary" type="button" :disabled="busy" @click="showDetail(event.id)">详情</button></td>
          </tr>
          <tr v-if="!events.length"><td colspan="5">{{ busy ? '正在查询…' : '没有符合条件的可见事件。' }}</td></tr>
        </tbody>
      </table>
    </div>
    <button v-if="cursor" class="secondary load-more" type="button" :disabled="busy" @click="run(() => load(applied, cursor))">加载更多</button>
  </section>
  <dialog ref="details" aria-labelledby="audit-detail-title">
    <div v-if="selection">
      <h2 id="audit-detail-title">事件详情</h2>
      <dl class="audit-detail">
        <dt>事件</dt><dd>{{ labels[selection.action] || selection.action }}</dd>
        <dt>事件 ID</dt><dd>{{ selection.id }}</dd>
        <dt>操作者</dt><dd>{{ selection.actor_name || '服务' }}</dd>
        <dt>来源</dt><dd>{{ selection.source }}</dd>
        <dt>结果</dt><dd>{{ selection.result }}</dd>
        <dt>发生时间</dt><dd>{{ date(selection.occurred_at) }}</dd>
        <dt>接收时间</dt><dd>{{ date(selection.received_at) }}</dd>
        <dt>回执</dt><dd>{{ status(selection) }}</dd>
        <template v-if="selection.receipt_result"><dt>回执结果</dt><dd>{{ selection.receipt_result === 'success' ? '成功' : '失败' }}</dd></template>
        <template v-if="selection.acknowledged_at"><dt>确认时间</dt><dd>{{ date(selection.acknowledged_at) }}</dd></template>
        <template v-if="selection.resource_id"><dt>资源 ID</dt><dd>{{ selection.resource_id }}</dd></template>
        <template v-if="selection.session_id"><dt>会话 ID</dt><dd>{{ selection.session_id }}</dd></template>
        <template v-if="selection.task_id"><dt>任务 ID</dt><dd>{{ selection.task_id }}</dd></template>
        <template v-if="selection.device_group_id"><dt>历史设备组 ID</dt><dd>{{ selection.device_group_id }}</dd></template>
        <template v-if="selection.reason"><dt>事件元数据</dt><dd>{{ selection.reason }}</dd></template>
      </dl>
      <div class="dialog-actions"><button class="secondary" type="button" @click="details?.close()">关闭</button></div>
    </div>
  </dialog>
</template>
