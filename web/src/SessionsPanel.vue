<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'
import { actionsText, localTime } from './managed'

type Activity = {
  state: string
  connection_id?: string
  lease_sequence?: number
  lease_until?: string
  revocation_requested_at?: string
  revocation_reason?: string
  lease_expired_at?: string
  stop_received_at?: string
  stop_reason?: string
}
type Session = Activity & {
  id: string
  user_id: string
  controller_device_id: string
  target_device_id: string
  purpose: string
  confirmation_mode: string
  policy_version: number
  issued_at: string
  grant_expires_at: string
  redeemed_at?: string
  path?: 'direct' | 'relay'
  relay_id?: string
  allowed_actions: string[]
  can_revoke: boolean
}
type Page = { sessions: Session[]; next?: string; metrics: Record<'total' | 'active' | 'direct' | 'relayed', number> }

const props = defineProps<{ csrf: string; canAudit: boolean; canReadDevices: boolean }>()
const emit = defineEmits<{ expired: [] }>()
const base = '/v1/admin/sessions'
const rows = ref<Session[]>([])
const metrics = ref<Page['metrics']>()
const after = ref('')
const detail = ref<Session>()
const detailDialog = ref<HTMLDialogElement>()
const revokeDialog = ref<HTMLDialogElement>()
const detailRefresh = ref<HTMLButtonElement>()
const detailClose = ref<HTMLButtonElement>()
const revoking = ref(false)
const detailError = ref('')
const error = ref('')
const status = ref('')
const reason = ref('')
const busy = ref(false)
let detailTrigger: HTMLElement | undefined

const stateLabels: Record<string, string> = {
  issued: '已签发 · 待核销', active: '活动中', revocation_requested: '已请求撤销 · 等待端点回执或租约到期',
  revoked: '授权已撤销', lease_expired: '租约已到期 · 等待端点回执', grant_expired: '授权已到期',
  stopped: '已收到端点停止回执',
}
const stopLabels: Record<string, string> = {
  local_end: '端点主动结束', revoked: '撤销', lease_expired: '租约到期', connection_lost: '连接中断',
}
const timeline = computed(() => {
  const row = detail.value
  if (!row) return []
  return [
    { label: '签发授权', at: row.issued_at },
    { label: '受控端核销', at: row.redeemed_at },
    { label: '请求撤销', at: row.revocation_requested_at },
    { label: '租约到期', at: row.lease_expired_at },
    { label: '中心收到停止回执', at: row.stop_received_at },
  ].filter(event => event.at).sort((a, b) => Date.parse(a.at!) - Date.parse(b.at!))
})

function pathOf(row: Session) {
  if (row.path === 'direct') return '直连'
  if (row.path === 'relay') return row.relay_id ? '中继 · ' + row.relay_id : '中继'
  return '待核销确认'
}

function describe(cause: unknown) {
  if (cause instanceof RequestError && cause.status === 401) emit('expired')
  return cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
}

async function run(task: () => Promise<void>, inDetail = false) {
  if (busy.value) return
  busy.value = true
  const message = inDetail ? detailError : error
  message.value = ''
  try { await task() } catch (cause) {
    message.value = describe(cause)
    if (cause instanceof RequestError) {
      if (inDetail && [403, 404].includes(cause.status)) {
        detail.value = undefined
        revokeDialog.value?.close()
      }
      if (cause.status === 403 && !inDetail) {
        rows.value = []
        metrics.value = undefined
        after.value = ''
      }
    }
  } finally { busy.value = false }
}

async function load(cursor = '') {
  const page = await api<Page>(base + (cursor ? '?' + new URLSearchParams({ before: cursor }) : ''))
  metrics.value = page.metrics
  rows.value = cursor ? [...rows.value, ...page.sessions] : page.sessions
  after.value = page.next ?? ''
}

function refresh() {
  status.value = ''
  return run(async () => {
    await load()
    status.value = '会话列表已刷新。'
  })
}

function view(id: string, event?: MouseEvent) {
  detailTrigger = event?.currentTarget as HTMLElement | undefined
  detailError.value = ''
  return run(async () => {
    detail.value = await api<Session>(base + '/' + encodeURIComponent(id))
    await nextTick()
    detailDialog.value?.showModal()
  })
}

function refreshDetail() {
  const id = detail.value?.id
  if (id) return run(async () => { detail.value = await api<Session>(base + '/' + encodeURIComponent(id)) }, true)
}

async function confirmRevoke() {
  reason.value = ''
  detailError.value = ''
  await nextTick()
  revoking.value = true
  revokeDialog.value?.showModal()
}

async function closeRevoke() {
  revoking.value = false
  if (detail.value && ['active', 'issued'].includes(detail.value.state)) return
  await nextTick()
  if (detailRefresh.value && !detailRefresh.value.disabled) detailRefresh.value.focus()
  else detailClose.value?.focus()
}

function revoke() {
  const row = detail.value
  if (!row?.can_revoke || !reason.value.trim()) return
  return run(async () => {
    const activity = await api<Activity>(base + '/' + encodeURIComponent(row.id) + '/revoke', json({ reason: reason.value.trim() }, props.csrf))
    detail.value = { ...row, ...activity }
    rows.value = rows.value.map(item => item.id === row.id ? detail.value! : item)
    revokeDialog.value?.close()
    status.value = '撤销请求已提交。'
    await load()
  }, true)
}

onMounted(async () => {
  await run(() => load())
  const id = new URLSearchParams(location.search).get('session')
  if (id) await view(id)
})
</script>

<template>
  <section class="users-surface" aria-labelledby="session-list-title" :aria-busy="busy">
    <div class="section-heading">
      <h2 id="session-list-title">受管会话</h2>
      <button class="secondary" type="button" :disabled="busy" @click="refresh">刷新会话</button>
    </div>
    <p v-if="status" class="inline-success" role="status">{{ status }}</p>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <dl v-if="metrics" class="session-counts">
      <div><dt>活动中</dt><dd>{{ metrics.active }}</dd></div>
      <div><dt>已核销直连</dt><dd>{{ metrics.direct }}</dd></div>
      <div><dt>已核销中继</dt><dd>{{ metrics.relayed }}</dd></div>
      <div><dt>可见会话</dt><dd>{{ metrics.total }}</dd></div>
    </dl>
    <div class="table-wrap">
      <table>
        <thead><tr><th scope="col">会话</th><th scope="col">用途</th><th scope="col">实际路径</th><th scope="col">状态</th><th scope="col">操作</th></tr></thead>
        <tbody>
          <tr v-for="row in rows" :key="row.id">
            <th scope="row" class="device-identifier">{{ row.id }}</th><td>{{ row.purpose }}</td>
            <td>{{ pathOf(row) }}</td><td>{{ stateLabels[row.state] ?? row.state }}</td>
            <td class="row-actions"><button class="secondary" type="button" :disabled="busy" @click="view(row.id, $event)">详情</button></td>
          </tr>
          <tr v-if="!rows.length && !error"><td colspan="5">{{ busy ? '正在加载会话…' : '暂无可查看的会话。' }}</td></tr>
        </tbody>
      </table>
    </div>
    <button v-if="after" class="secondary load-more" type="button" :disabled="busy" @click="run(() => load(after))">加载更多</button>
  </section>

  <dialog ref="detailDialog" aria-labelledby="session-detail-title" @close="detailTrigger?.focus()">
    <h2 id="session-detail-title">会话详情</h2>
    <p v-if="detailError && !revoking" class="error" role="alert">{{ detailError }}</p>
    <template v-if="detail">
      <p class="muted dialog-copy" role="status">{{ stateLabels[detail.state] ?? detail.state }}</p>
      <dl class="audit-detail">
        <dt>会话 ID</dt><dd class="device-identifier">{{ detail.id }}</dd>
        <dt>受控设备</dt><dd>
          <a v-if="canReadDevices" :href="'/devices?device=' + detail.target_device_id">{{ detail.target_device_id }}</a>
          <template v-else>{{ detail.target_device_id }}</template>
        </dd>
        <dt>发起人</dt><dd>{{ detail.user_id }}</dd>
        <dt>主控设备</dt><dd>{{ detail.controller_device_id }}</dd>
        <dt>用途</dt><dd>{{ detail.purpose }}</dd>
        <dt>实际路径</dt><dd>{{ pathOf(detail) }}</dd>
        <dt>确认方式</dt><dd>{{ detail.confirmation_mode === 'attended' ? '有人值守' : '无人值守' }}</dd>
        <dt>允许的操作</dt><dd>{{ actionsText(detail.allowed_actions) }}</dd>
        <dt>策略版本</dt><dd>{{ detail.policy_version }}</dd>
        <dt>授权截止</dt><dd>{{ localTime(detail.grant_expires_at) }}</dd>
        <template v-if="detail.lease_until">
          <dt>租约截止</dt><dd>{{ localTime(detail.lease_until) }}</dd>
          <dt>租约序号</dt><dd>{{ detail.lease_sequence }}</dd>
        </template>
        <template v-if="detail.revocation_reason"><dt>撤销原因</dt><dd>{{ detail.revocation_reason }}</dd></template>
        <template v-if="detail.stop_reason"><dt>停止原因</dt><dd>{{ stopLabels[detail.stop_reason] ?? detail.stop_reason }}</dd></template>
      </dl>
      <h3>会话时间线</h3>
      <ol class="session-timeline"><li v-for="event in timeline" :key="event.label">{{ event.label }} · {{ localTime(event.at) }}</li></ol>
      <div class="user-tools device-detail-actions">
        <a v-if="canAudit" class="secondary action-link" :href="'/audit?session_id=' + detail.id">查看会话审计</a>
        <button ref="detailRefresh" class="secondary" type="button" :disabled="busy" @click="refreshDetail">刷新详情</button>
        <button v-if="detail.can_revoke && ['active', 'issued'].includes(detail.state)" class="primary" type="button"
          :disabled="busy" @click="confirmRevoke">撤销会话</button>
      </div>
    </template>
    <div class="dialog-actions"><button ref="detailClose" class="secondary" type="button" @click="detailDialog?.close()">关闭详情</button></div>
  </dialog>

  <dialog ref="revokeDialog" aria-labelledby="session-revoke-title" @close="closeRevoke">
    <form @submit.prevent="revoke">
      <h2 id="session-revoke-title">确认撤销会话</h2>
      <p class="muted dialog-copy device-identifier">{{ detail?.id }}</p>
      <p v-if="detailError" class="error" role="alert">{{ detailError }}</p>
      <div class="field"><label for="session-reason">撤销原因</label><input id="session-reason" v-model="reason" maxlength="120" required></div>
      <div class="dialog-actions">
        <button class="secondary" type="button" @click="revokeDialog?.close()">返回</button>
        <button class="primary" type="submit" :disabled="busy || !reason.trim()">确认撤销</button>
      </div>
    </form>
  </dialog>
</template>
