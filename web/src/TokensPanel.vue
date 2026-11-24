<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'

type Group = { id: string; name: string }
type Token = {
  id: string
  purpose: string
  action: string
  scope: 'organization' | 'group' | 'self' | 'device_group'
  group_id?: string
  device_group_id?: string
  expires_at: string
  revoked_at?: string
}

const props = defineProps<{ csrf: string }>()
const emit = defineEmits<{ expired: [] }>()
const tokens = ref<Token[]>([])
const groups = ref<Group[]>([])
const deviceGroups = ref<Group[]>([])
const purpose = ref('')
const action = ref<'users.read' | 'windows.deploy'>('users.read')
const scope = ref<'organization' | 'group' | 'self'>('group')
const groupID = ref('')
const deviceGroupID = ref('')
const minutes = ref(60)
const secret = ref('')
const error = ref('')
const busy = ref(false)
const revoking = ref<Token>()
const dialog = ref<HTMLDialogElement>()

function scopeLabel(token: Token) {
  if (token.scope === 'device_group') {
    return deviceGroups.value.find(group => group.id === token.device_group_id)?.name ?? '设备组'
  }
  if (token.scope === 'organization') return '组织'
  if (token.scope === 'self') return '本人'
  return groups.value.find(group => group.id === token.group_id)?.name ?? '用户组'
}

function state(token: Token) {
  if (token.revoked_at) return '已撤销'
  return new Date(token.expires_at) <= new Date() ? '已过期' : '有效'
}

async function withRequest(task: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    await task()
  } catch (cause) {
    if (cause instanceof RequestError && cause.code === 'session_required') emit('expired')
    error.value = cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
  } finally {
    busy.value = false
  }
}

function issue() {
  return withRequest(async () => {
    secret.value = ''
    const result = await api<Token & { token: string }>('/v1/admin/api-tokens', json({
      purpose: purpose.value, action: action.value, scope: action.value === 'windows.deploy' ? 'device_group' : scope.value,
      group_id: action.value === 'users.read' && scope.value === 'group' ? groupID.value : '',
      device_group_id: action.value === 'windows.deploy' ? deviceGroupID.value : '',
      lifetime_minutes: minutes.value,
    }, props.csrf))
    const { token, ...record } = result
    secret.value = token
    purpose.value = ''
    tokens.value = [record, ...tokens.value]
  })
}

async function askRevoke(token: Token) {
  revoking.value = token
  error.value = ''
  await nextTick()
  dialog.value?.showModal()
}

function revoke() {
  const value = revoking.value
  if (!value) return
  return withRequest(async () => {
    await api('/v1/admin/api-tokens/' + value.id + '/revoke', json({}, props.csrf))
    secret.value = ''
    tokens.value = await api<Token[]>('/v1/admin/api-tokens')
    dialog.value?.close()
  })
}

onMounted(() => withRequest(async () => {
  const [availableGroups, availableDevices, existingTokens] = await Promise.all([
    api<Group[]>('/v1/admin/groups'),
    api<Array<Group & { can_approve: boolean }>>('/v1/admin/device-groups'),
    api<Token[]>('/v1/admin/api-tokens'),
  ])
  groups.value = availableGroups
  deviceGroups.value = availableDevices.filter(group => group.can_approve)
  tokens.value = existingTokens
}))
</script>

<template>
  <section class="users-surface" aria-labelledby="token-title">
    <h2 id="token-title">签发令牌</h2>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <form class="link-form" @submit.prevent="issue">
      <div class="field">
        <label for="token-purpose">用途</label>
        <input id="token-purpose" v-model="purpose" maxlength="80" required>
      </div>
      <div class="field">
        <label for="token-action">权限</label>
        <select id="token-action" v-model="action" @change="secret = ''">
          <option value="users.read">读取账号</option>
          <option value="windows.deploy">部署 Windows 终端</option>
        </select>
      </div>
      <div v-if="action === 'users.read'" class="field">
        <label for="token-scope">作用域</label>
        <select id="token-scope" v-model="scope">
          <option value="group">用户组</option><option value="self">本人</option><option value="organization">组织</option>
        </select>
      </div>
      <div v-if="action === 'users.read' && scope === 'group'" class="field">
        <label for="token-group">用户组</label>
        <select id="token-group" v-model="groupID" required>
          <option value="">选择用户组</option>
          <option v-for="group in groups" :key="group.id" :value="group.id">{{ group.name }}</option>
        </select>
      </div>
      <div v-if="action === 'windows.deploy'" class="field">
        <label for="token-device-group">设备组</label>
        <select id="token-device-group" v-model="deviceGroupID" required>
          <option value="">选择设备组</option>
          <option v-for="group in deviceGroups" :key="group.id" :value="group.id">{{ group.name }}</option>
        </select>
      </div>
      <div class="field">
        <label for="token-minutes">有效期（分钟）</label>
        <input id="token-minutes" v-model.number="minutes" type="number" min="1" max="10080" required>
      </div>
      <button class="primary" type="submit" :disabled="busy">生成令牌</button>
    </form>
    <div v-if="secret" class="link-form">
      <div class="field">
        <label for="token-value">新令牌</label>
        <input id="token-value" :value="secret" readonly>
        <span class="hint">令牌仅显示一次，请复制到授权的自动化环境。</span>
      </div>
      <button class="secondary" type="button" @click="secret = ''">清除令牌</button>
    </div>
    <div class="table-wrap">
      <table>
        <thead><tr><th scope="col">用途</th><th scope="col">范围</th><th scope="col">有效期</th><th scope="col">状态</th><th scope="col">操作</th></tr></thead>
        <tbody>
          <tr v-for="token in tokens" :key="token.id">
            <th scope="row">{{ token.purpose }}</th>
            <td>{{ token.action === 'windows.deploy' ? 'Windows 部署 · ' : '' }}{{ scopeLabel(token) }}</td>
            <td>{{ new Date(token.expires_at).toLocaleString() }}</td>
            <td>{{ state(token) }}</td>
            <td><button v-if="state(token) === '有效'" type="button" class="secondary" @click="askRevoke(token)">撤销令牌</button></td>
          </tr>
          <tr v-if="!tokens.length"><td colspan="5">{{ busy ? '正在加载…' : '暂无令牌。' }}</td></tr>
        </tbody>
      </table>
    </div>
  </section>
  <dialog ref="dialog" aria-labelledby="token-revoke">
    <form @submit.prevent="revoke">
      <h2 id="token-revoke">撤销 API 令牌</h2>
      <p v-if="revoking" class="muted dialog-copy">「{{ revoking.purpose }}」令牌将立即失效。</p>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <div class="dialog-actions">
        <button class="secondary" type="button" @click="dialog?.close()">取消</button>
        <button class="primary" type="submit" :disabled="busy">确认撤销</button>
      </div>
    </form>
  </dialog>
</template>
