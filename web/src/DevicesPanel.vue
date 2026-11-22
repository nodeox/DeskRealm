<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'

type Group = { id: string; name: string; can_approve: boolean }
type Grant = { username: string; can_approve: boolean }
type Device = {
  id: string; group_id: string; group_name: string; platform: string; rustdesk_id: string
  registration_state: string; connection_state: string; expected_policy_version: number | null
  applied_policy_version: number | null; can_approve: boolean; version: number
}
type DevicePage = { devices: Device[]; next?: string }
type Issued = { credential: string; expires_at: string }
type Decision = 'approve' | 'reject' | 'move' | 'rename-id' | 'rotate' | 'disable' | 'restore' | 'retire'
type Rotation = Issued & { device_id: string; version: number; nonce: string }
type History = {
  rustdesk_ids: { rustdesk_id: string; recorded_at: string }[]
  keys: { fingerprint: string; recorded_at: string; revoked_at: string | null }[]
}

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
const destination = ref('')
const newID = ref('')
const rotation = ref<Rotation>()
const details = ref<Device>()
const history = ref<History>()
const historyDialog = ref<HTMLDialogElement>()
const actions: Record<Decision, string> = {
  approve: '批准设备登记', reject: '拒绝设备登记', move: '移组', 'rename-id': '修改 RustDesk ID',
  rotate: '轮换密钥', disable: '禁用设备', restore: '恢复设备', retire: '退役设备',
}
const confirmations: Record<Decision, string> = {
  approve: '', reject: '', move: '移组后，设备按目标组的访问范围管理。',
  'rename-id': '设备 UUID 保持不变，原 ID 保留在历史中。',
  rotate: '生成限时凭据。终端证明持有新密钥后，原密钥失效。',
  disable: '撤销设备密钥及待使用的轮换凭据，保留设备和审批记录。',
  restore: '生成限时恢复凭据。终端证明持有新密钥后恢复已批准状态。',
  retire: '撤销设备密钥，结束纳管并保留历史记录。退役后无法恢复此设备。',
}
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
  historyDialog.value?.close()
  pending.value = { device, action }
  destination.value = ''
  newID.value = device.rustdesk_id
  error.value = ''
  await nextTick()
  dialog.value?.showModal()
}

async function showDetails(device: Device) {
  await execute(async () => {
    const path = '/v1/admin/devices/' + device.id
    const [current, records] = await Promise.all([api<Device>(path), api<History>(path + '/history')])
    details.value = current
    history.value = records
    await nextTick()
    historyDialog.value?.showModal()
  })
}

function decide() {
  const request = pending.value
  if (!request) return
  return execute(async () => {
    const { device, action } = request
    const path = '/v1/admin/devices/' + device.id
    if (action === 'rotate' || action === 'restore') {
      rotation.value = undefined
      rotation.value = await api<Rotation>(path + '/key-rotation',
        json({ version: device.version, restore: action === 'restore' }, props.csrf))
    } else {
      if (rotation.value?.device_id === device.id) rotation.value = undefined
      const approval = action === 'approve' || action === 'reject'
      const result = await api<Device>(path + (approval ? '/' : '/lifecycle/') + action, json(approval ? {} : {
        version: device.version,
        ...(action === 'move' ? { group_id: destination.value } : {}),
        ...(action === 'rename-id' ? { rustdesk_id: newID.value } : {}),
      }, props.csrf))
      devices.value = devices.value.map(row => result.id === row.id ? result : row)
    }
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
    <div v-if="rotation" class="link-form">
      <div class="field">
        <label for="rotation-credential">一次性密钥轮换凭据</label>
        <textarea id="rotation-credential" :value="JSON.stringify(rotation, null, 2)" rows="7" readonly></textarea>
        <span class="hint">有效期至 {{ new Date(rotation.expires_at).toLocaleString() }}。请通过组织渠道交给目标终端。</span>
      </div>
      <button class="secondary" type="button" @click="rotation = undefined">清除轮换凭据</button>
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
              <button type="button" class="secondary" :disabled="busy" @click="showDetails(device)">详情</button>
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

  <dialog ref="historyDialog" aria-labelledby="device-detail-title">
    <div v-if="details && history">
      <h2 id="device-detail-title">设备详情</h2>
      <p class="muted dialog-copy">{{ details.rustdesk_id }} · {{ details.group_name }} · {{ states[details.registration_state] }}
        <small class="device-identifier">设备 UUID {{ details.id }}</small>
      </p>
      <h3>RustDesk ID 历史</h3>
      <ul class="member-list">
        <li v-for="(entry, i) in history.rustdesk_ids" :key="i"><span>{{ entry.rustdesk_id }}</span><small>{{ new Date(entry.recorded_at).toLocaleString() }}</small></li>
      </ul>
      <h3>密钥历史</h3>
      <ul class="member-list">
        <li v-for="key in history.keys" :key="key.fingerprint"><small class="device-identifier">{{ key.fingerprint }}</small><span>{{ key.revoked_at ? '已撤销' : '已登记' }}</span></li>
      </ul>
      <div v-if="details.can_approve && details.registration_state !== 'retired'" class="user-tools device-detail-actions">
        <button v-if="groups.some(group => group.can_approve && group.id !== details?.group_id)" type="button" class="secondary" @click="ask(details, 'move')">移组</button>
        <button type="button" class="secondary" @click="ask(details, 'rename-id')">修改 RustDesk ID</button>
        <template v-if="details.registration_state === 'approved'">
          <button type="button" class="secondary" @click="ask(details, 'rotate')">轮换密钥</button>
          <button type="button" class="secondary" @click="ask(details, 'disable')">禁用设备</button>
        </template>
        <button v-if="details.registration_state === 'disabled'" type="button" class="secondary" @click="ask(details, 'restore')">恢复设备</button>
        <button type="button" class="secondary" @click="ask(details, 'retire')">退役设备</button>
      </div>
      <div class="dialog-actions"><button class="secondary" type="button" @click="historyDialog?.close()">关闭</button></div>
    </div>
  </dialog>

  <dialog ref="dialog" aria-labelledby="device-decision-title">
    <form v-if="pending" @submit.prevent="decide">
      <h2 id="device-decision-title">{{ actions[pending.action] }}</h2>
      <p class="muted dialog-copy">{{ pending.device.rustdesk_id }} · {{ pending.device.group_name }}
        <small v-if="confirmations[pending.action]">{{ confirmations[pending.action] }}</small>
      </p>
      <div v-if="pending.action === 'move'" class="field">
        <label for="device-destination">新设备组</label>
        <select id="device-destination" v-model="destination" required>
          <option value="">选择设备组</option>
          <option v-for="group in groups.filter(group => group.can_approve && group.id !== pending?.device.group_id)" :key="group.id" :value="group.id">{{ group.name }}</option>
        </select>
      </div>
      <div v-if="pending.action === 'rename-id'" class="field">
        <label for="device-new-id">新 RustDesk ID</label>
        <input id="device-new-id" v-model="newID" pattern="[A-Za-z0-9][A-Za-z0-9_\-]{2,31}" required maxlength="32">
      </div>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <div class="dialog-actions">
        <button class="secondary" type="button" @click="dialog?.close()">取消</button>
        <button class="primary" type="submit" :disabled="busy">{{ pending.action === 'approve' ? '确认批准' : pending.action === 'reject' ? '确认拒绝' : '确认' }}</button>
      </div>
    </form>
  </dialog>
</template>
