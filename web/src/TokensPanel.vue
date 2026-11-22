<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'

type Group = { id: string; name: string }
type Token = {
  id: string
  purpose: string
  action: string
  scope: 'organization' | 'group' | 'self'
  group_id?: string
  expires_at: string
  revoked_at?: string
}

const props = defineProps<{ csrf: string }>()
const emit = defineEmits<{ expired: [] }>()
const tokens = ref<Token[]>([])
const groups = ref<Group[]>([])
const purpose = ref('')
const scope = ref<Token['scope']>('group')
const groupID = ref('')
const minutes = ref(60)
const secret = ref('')
const error = ref('')
const busy = ref(false)
const revoking = ref<Token>()
const dialog = ref<HTMLDialogElement>()

function scopeLabel(token: Token) {
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
      purpose: purpose.value, action: 'users.read', scope: scope.value,
      group_id: scope.value === 'group' ? groupID.value : '',
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
  const [availableGroups, existingTokens] = await Promise.all([
    api<Group[]>('/v1/admin/groups'),
    api<Token[]>('/v1/admin/api-tokens'),
  ])
  groups.value = availableGroups
  tokens.value = existingTokens
}))
</script>

<template>
  <section class="users-surface" aria-labelledby="token-title">
    <h2 id="token-title">签发令牌</h2>
    <p class="muted">用于按授权范围读取账号列表和详情。</p>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <form class="link-form" @submit.prevent="issue">
      <div class="field">
        <label for="token-purpose">用途</label>
        <input id="token-purpose" v-model="purpose" maxlength="80" required>
      </div>
      <div class="field">
        <label for="token-scope">作用域</label>
        <select id="token-scope" v-model="scope">
          <option value="group">用户组</option><option value="self">本人</option><option value="organization">组织</option>
        </select>
      </div>
      <div v-if="scope === 'group'" class="field">
        <label for="token-group">用户组</label>
        <select id="token-group" v-model="groupID" required>
          <option value="">选择用户组</option>
          <option v-for="group in groups" :key="group.id" :value="group.id">{{ group.name }}</option>
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
            <td>{{ scopeLabel(token) }}</td>
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
