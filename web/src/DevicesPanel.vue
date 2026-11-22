<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'

type Group = { id: string; name: string; can_approve: boolean }
type Grant = { username: string; can_approve: boolean }
type Device = {
  id: string; group_id: string; group_name: string; platform: string; rustdesk_id: string
  registration_state: string; connection_state: string; expected_policy_version: number | null
  applied_policy_version: number | null; can_approve: boolean
}
type DevicePage = { devices: Device[]; next?: string }
type Issued = { credential: string; expires_at: string }
type Decision = 'approve' | 'reject'

const props = defineProps<{ csrf: string; admin: boolean }>()
const emit = defineEmits<{ expired: [] }>()
const groups = ref<Group[]>([])
const devices = ref<Device[]>([])
const grants = ref<Grant[]>([])
const next = ref('')
const busy = ref(false)
const error = ref('')
const mode = ref<'group' | 'grant' | 'credential'>()
const name = ref('')
const groupID = ref('')
const username = ref('')
const allowApproval = ref(false)
const platform = ref('windows-amd64')
const purpose = ref('')
const minutes = ref(30)
const issued = ref<Issued>()
const dialog = ref<HTMLDialogElement>()
const pending = ref<{ device: Device; action: Decision }>()
const states: Record<string, string> = {
  pending: '待审批', approved: '已批准', rejected: '已拒绝', disabled: '已禁用', retired: '已退役',
  unknown: '未知', online: '在线', offline: '离线',
}

function report(cause: unknown) {
  if (cause instanceof RequestError && cause.code === 'session_required') emit('expired')
  error.value = cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
}

async function execute(fn: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try { await fn() } catch (cause) { report(cause) } finally { busy.value = false }
}

async function loadList(cursor = '') {
  const result = await api<DevicePage>('/v1/admin/devices' + (cursor ? '?after=' + cursor : ''))
  devices.value = cursor ? [...devices.value, ...result.devices] : result.devices
  next.value = result.next ?? ''
}

async function loadGrants() {
  grants.value = groupID.value ? await api<Grant[]>('/v1/admin/device-groups/' + groupID.value + '/grants') : []
}

function toggle(view: typeof mode.value) {
  if (busy.value) return
  mode.value = mode.value === view ? undefined : view
  error.value = ''
  if (mode.value === 'grant') void execute(loadGrants)
}

function selectGroup() {
  grants.value = []
  if (mode.value === 'grant') return execute(loadGrants)
}

function createGroup() {
  return execute(async () => {
    const group = await api<Group>('/v1/admin/device-groups', json({ name: name.value }, props.csrf))
    groups.value = await api<Group[]>('/v1/admin/device-groups')
    groupID.value = group.id
    name.value = ''
    mode.value = 'grant'
    await loadGrants()
  })
}

function shareGroup(user: string, revoke: boolean) {
  return execute(async () => {
    await api('/v1/admin/device-groups/' + groupID.value + '/grants',
      json({ username: user, can_approve: allowApproval.value, revoke }, props.csrf))
    username.value = ''
    await loadGrants()
  })
}

function issue() {
  return execute(async () => {
    issued.value = undefined
    issued.value = await api<Issued>('/v1/admin/enrollment-credentials',
      json({ group_id: groupID.value, purpose: purpose.value, platform: platform.value, lifetime_minutes: minutes.value }, props.csrf))
    purpose.value = ''
  })
}

async function ask(device: Device, action: Decision) {
  pending.value = { device, action }
  error.value = ''
  await nextTick()
  dialog.value?.showModal()
}

function decide() {
  const request = pending.value
  if (!request) return
  return execute(async () => {
    const result = await api<Device>('/v1/admin/devices/' + request.device.id + '/' + request.action, json({}, props.csrf))
    devices.value = devices.value.map(device => result.id === device.id ? result : device)
    dialog.value?.close()
  })
}

onMounted(() => execute(async () => {
  groups.value = await api<Group[]>('/v1/admin/device-groups')
  await loadList()
}))
</script>

<template>
  <section class="users-surface" aria-labelledby="device-title">
    <div class="section-heading">
      <div><h2 id="device-title">设备台账</h2><p class="muted">查看登记、连接及策略回执。</p></div>
      <div class="user-tools">
        <button v-if="admin" class="secondary" type="button" :aria-expanded="mode === 'group'" @click="toggle('group')">新建设备组</button>
        <button v-if="admin && groups.length" class="secondary" type="button" :aria-expanded="mode === 'grant'" @click="toggle('grant')">管理设备组委派</button>
        <button v-if="groups.some(group => group.can_approve)" class="primary" type="button"
          :aria-expanded="mode === 'credential'" @click="toggle('credential')">发放入网凭据</button>
      </div>
    </div>

    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <form v-if="mode === 'group'" class="link-form" @submit.prevent="createGroup">
      <div class="field"><label for="device-group-name">设备组名称</label><input id="device-group-name" v-model="name" maxlength="80" required></div>
      <button class="primary" :disabled="busy">创建设备组</button>
    </form>

    <div v-if="mode === 'grant' || mode === 'credential'" class="link-form">
      <div class="field">
        <label for="device-group-select">目标设备组</label>
        <select id="device-group-select" v-model="groupID" :disabled="busy" @change="selectGroup">
          <option value="">选择设备组</option>
          <option v-for="group in groups.filter(group => mode === 'grant' || group.can_approve)" :key="group.id" :value="group.id">{{ group.name }}</option>
        </select>
      </div>
      <template v-if="groupID && mode === 'grant'">
        <form class="link-form" @submit.prevent="shareGroup(username, false)">
          <div class="field"><label for="device-grant-user">成员账号</label><input id="device-grant-user" v-model="username" maxlength="40" required></div>
          <div class="field"><label for="device-grant-mode">权限</label>
            <select id="device-grant-mode" v-model="allowApproval"><option :value="false">仅查看</option><option :value="true">查看及审批</option></select>
          </div>
          <button class="primary" :disabled="busy">保存委派</button>
        </form>
        <ul v-if="grants.length" class="member-list">
          <li v-for="grant in grants" :key="grant.username">
            <div>{{ grant.username }}<small>{{ grant.can_approve ? '查看及审批' : '仅查看' }}</small></div>
            <button class="secondary" type="button" :disabled="busy" @click="shareGroup(grant.username, true)">撤销委派</button>
          </li>
        </ul>
      </template>
      <form v-if="mode === 'credential'" @submit.prevent="issue">
        <div class="field"><label for="enrollment-purpose">凭据用途</label><input id="enrollment-purpose" v-model="purpose" maxlength="80" required></div>
        <div class="field"><label for="device-platform">设备平台</label>
          <select id="device-platform" v-model="platform"><option value="windows-amd64">Windows x64</option><option value="windows-arm64">Windows ARM64</option></select>
        </div>
        <div class="field"><label for="enrollment-minutes">有效期（分钟）</label><input id="enrollment-minutes" v-model.number="minutes" type="number" min="1" max="1440" required></div>
        <button class="primary" :disabled="busy || !groupID">生成入网凭据</button>
      </form>
    </div>

    <div v-if="issued" class="link-form">
      <div class="field">
        <label for="device-credential">一次性入网凭据</label><input id="device-credential" :value="issued.credential" readonly>
        <span class="hint">有效期至 {{ new Date(issued.expires_at).toLocaleString() }}。请通过组织渠道交给目标终端。</span>
      </div>
      <button class="secondary" type="button" @click="issued = undefined">清除凭据</button>
    </div>

    <div class="table-wrap">
      <table>
        <thead><tr><th scope="col">设备</th><th scope="col">设备组</th><th scope="col">登记</th><th scope="col">连接</th><th scope="col">策略</th><th scope="col">操作</th></tr></thead>
        <tbody>
          <tr v-for="device in devices" :key="device.id">
            <th scope="row">{{ device.rustdesk_id }}<small>{{ device.platform }}</small></th>
            <td>{{ device.group_name }}</td>
            <td>{{ states[device.registration_state] }}</td>
            <td>{{ states[device.connection_state] }}</td>
            <td v-if="device.expected_policy_version === null">未下发</td>
            <td v-else>期望 {{ device.expected_policy_version }} · {{ device.applied_policy_version === null ? '待回执' : '已应用 ' + device.applied_policy_version }}</td>
            <td class="row-actions">
              <template v-if="device.can_approve && device.registration_state === 'pending'">
                <button type="button" class="secondary" @click="ask(device, 'approve')">批准</button>
                <button type="button" class="secondary" @click="ask(device, 'reject')">拒绝</button>
              </template>
            </td>
          </tr>
          <tr v-if="!devices.length"><td colspan="6">{{ busy ? '正在加载设备…' : '暂无可查看的设备。' }}</td></tr>
        </tbody>
      </table>
    </div>
    <button v-if="next" class="secondary load-more" type="button" :disabled="busy" @click="execute(() => loadList(next))">加载更多</button>
  </section>

  <dialog ref="dialog" aria-labelledby="device-decision-title">
    <form @submit.prevent="decide">
      <h2 id="device-decision-title">{{ pending?.action === 'approve' ? '批准设备登记' : '拒绝设备登记' }}</h2>
      <p v-if="pending" class="muted dialog-copy">{{ pending.device.rustdesk_id }} · {{ pending.device.group_name }}</p>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <div class="dialog-actions">
        <button class="secondary" type="button" @click="dialog?.close()">取消</button>
        <button class="primary" type="submit" :disabled="busy">确认{{ pending?.action === 'approve' ? '批准' : '拒绝' }}</button>
      </div>
    </form>
  </dialog>
</template>
