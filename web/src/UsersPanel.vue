<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'

type UserStatus = 'pending' | 'active' | 'disabled' | 'archived'
type ManagedUser = { id: string; username: string; status: UserStatus; platform_admin: boolean }
type UserList = { users: ManagedUser[]; next?: string }
type Action = 'enable' | 'disable' | 'archive' | 'recovery' | 'totp-reset'
type Form = 'direct' | 'invite' | 'import'
type IssuedLink = { url: string; expires_at: string }
type ImportEntry = { username: string; result: 'pending' | 'exists' }

const props = defineProps<{ csrf: string }>()
const emit = defineEmits<{ expired: [] }>()
const statusLabel: Record<UserStatus, string> = { pending: '待激活', active: '已启用', disabled: '已停用', archived: '已归档' }
const actionLabel: Record<Action, string> = { enable: '启用', disable: '停用', archive: '归档', recovery: '恢复', 'totp-reset': '重置验证' }

const data = ref<ManagedUser[]>([])
const next = ref('')
const busy = ref(false)
const error = ref('')
const result = ref('')
const form = ref<Form>()
const username = ref('')
const password = ref('')
const importNames = ref('')
const importKey = ref('')
const imported = ref<ImportEntry[]>([])
const issued = ref<IssuedLink>()
const dialog = ref<HTMLDialogElement>()
const pending = ref<{ action: Action; user: ManagedUser }>()
const reason = ref('')
const adminPassword = ref('')
const dialogError = ref('')

function message(cause: unknown) {
  if (cause instanceof RequestError && cause.status === 401) emit('expired')
  return cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
}

function toggle(value: Form) {
  form.value = form.value === value ? undefined : value
  password.value = ''
  error.value = ''
  result.value = ''
}

async function refresh(after = '') {
  busy.value = true
  error.value = ''
  try {
    const page = await api<UserList>('/v1/admin/users' + (after ? '?after=' + encodeURIComponent(after) : ''))
    data.value = after ? [...data.value, ...page.users] : page.users
    next.value = page.next ?? ''
  } catch (cause) {
    error.value = message(cause)
  } finally {
    busy.value = false
  }
}

async function create() {
  busy.value = true
  error.value = ''
  result.value = ''
  try {
    const user = await api<ManagedUser>('/v1/admin/users', json({ username: username.value, password: password.value }, props.csrf))
    result.value = '已添加账号 ' + user.username
    username.value = ''
    form.value = undefined
    await refresh()
  } catch (cause) {
    error.value = message(cause)
  } finally {
    password.value = ''
    busy.value = false
  }
}

async function invite(name: string) {
  busy.value = true
  error.value = ''
  result.value = ''
  issued.value = undefined
  try {
    issued.value = await api<IssuedLink>('/v1/admin/user-invitations', json({ username: name }, props.csrf))
    username.value = ''
    form.value = undefined
    await refresh()
  } catch (cause) {
    error.value = message(cause)
  } finally {
    busy.value = false
  }
}

async function importUsers() {
  if (!importKey.value) importKey.value = crypto.randomUUID()
  const usernames = importNames.value.split(/[\n,]+/).map(value => value.trim()).filter(Boolean)
  busy.value = true
  error.value = ''
  try {
    const response = await api<{ entries: ImportEntry[] }>('/v1/admin/users/imports',
      json({ idempotency_key: importKey.value, usernames }, props.csrf))
    imported.value = response.entries
    form.value = undefined
    importNames.value = ''
    importKey.value = ''
    await refresh()
  } catch (cause) {
    error.value = message(cause)
  } finally {
    busy.value = false
  }
}

async function copyLink() {
  if (!issued.value) return
  try {
    await navigator.clipboard.writeText(issued.value.url)
    result.value = '链接已复制。'
  } catch {
    error.value = '无法访问剪贴板，请从链接字段手动复制。'
  }
}

async function select(action: Action, user: ManagedUser) {
  pending.value = { action, user }
  reason.value = ''
  adminPassword.value = ''
  dialogError.value = ''
  await nextTick()
  dialog.value?.showModal()
}

function resetDialog() {
  pending.value = undefined
  dialogError.value = ''
  adminPassword.value = ''
}

async function change() {
  const selected = pending.value
  if (!selected || busy.value) return
  busy.value = true
  dialogError.value = ''
  result.value = ''
  try {
    const path = '/v1/admin/users/' + selected.user.id + '/' + selected.action
    if (selected.action === 'recovery') {
      issued.value = await api<IssuedLink>(path, json({}, props.csrf))
    } else if (selected.action === 'totp-reset') {
      await api(path, json({ reason: reason.value, password: adminPassword.value }, props.csrf))
      result.value = selected.user.username + ' 的两步验证已重置。'
      await refresh()
    } else {
      const user = await api<ManagedUser>(path, json({ reason: reason.value }, props.csrf))
      data.value = data.value.map(row => row.id === user.id ? user : row)
      result.value = user.username + statusLabel[user.status]
    }
    dialog.value?.close()
  } catch (cause) {
    dialogError.value = message(cause)
  } finally {
    busy.value = false
    adminPassword.value = ''
  }
}

onMounted(() => refresh())
</script>

<template>
  <section class="users-surface" aria-labelledby="users-title">
    <div class="section-heading">
      <div><h2 id="users-title">用户列表</h2><p class="muted">管理组织账号和登录状态。</p></div>
      <div class="user-tools">
        <button class="secondary" type="button" :aria-expanded="form === 'import'" @click="toggle('import')">导入账号</button>
        <button class="secondary" type="button" :aria-expanded="form === 'invite'" @click="toggle('invite')">邀请成员</button>
        <button class="primary" type="button" :aria-expanded="form === 'direct'" @click="toggle('direct')">添加成员</button>
      </div>
    </div>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <p v-if="result" class="inline-success" role="status">{{ result }}</p>
    <form v-if="form === 'direct' || form === 'invite'" class="member-form" @submit.prevent="form === 'direct' ? create() : invite(username)">
      <div class="field">
        <label for="member-name">成员账号</label>
        <input id="member-name" v-model="username" autocomplete="off" maxlength="40" required>
      </div>
      <div v-if="form === 'direct'" class="field">
        <label for="member-password">初始密码</label>
        <input id="member-password" v-model="password" type="password" autocomplete="new-password"
          minlength="12" maxlength="256" required>
      </div>
      <button class="primary" type="submit" :disabled="busy">{{ form === 'direct' ? '创建成员' : '生成邀请链接' }}</button>
    </form>
    <form v-else-if="form === 'import'" class="link-form" @submit.prevent="importUsers">
      <div class="field">
        <label for="bulk-usernames">待导入账号</label>
        <textarea id="bulk-usernames" v-model="importNames" maxlength="4500" rows="4" required @input="importKey = ''"></textarea>
        <span class="hint">每行一个账号，最多 100 个。新账号在接受邀请后启用。</span>
      </div>
      <button class="primary" type="submit" :disabled="busy">导入</button>
    </form>
    <div v-if="issued" class="link-form">
      <div class="field">
        <label for="issued-user-link">一次性账号链接</label>
        <input id="issued-user-link" :value="issued.url" readonly>
        <span class="hint">有效期至 {{ new Date(issued.expires_at).toLocaleString() }}</span>
      </div>
      <div class="user-tools">
        <button type="button" class="secondary" @click="copyLink">复制链接</button>
        <button type="button" class="secondary" @click="issued = undefined">清除链接</button>
      </div>
    </div>
    <details v-if="imported.length" class="import-results">
      <summary>本次导入结果（{{ imported.length }} 项）</summary>
      <ul><li v-for="entry in imported" :key="entry.username">{{ entry.username }} · {{ entry.result === 'exists' ? '已存在' : '待激活' }}</li></ul>
    </details>
    <div class="table-wrap">
      <table>
        <thead><tr><th scope="col">账号</th><th scope="col">角色</th><th scope="col">状态</th><th scope="col">操作</th></tr></thead>
        <tbody>
          <tr v-for="user in data" :key="user.id">
            <th scope="row">{{ user.username }}</th>
            <td>{{ user.platform_admin ? '平台管理员' : '成员' }}</td>
            <td><span class="user-status" :data-state="user.status">{{ statusLabel[user.status] }}</span></td>
            <td class="row-actions">
              <button v-if="user.status === 'disabled'" class="secondary" type="button" @click="select('enable', user)">启用</button>
              <button v-if="user.status === 'active'" class="secondary" type="button" @click="select('disable', user)">停用</button>
              <button v-if="user.status !== 'archived'" class="secondary" type="button" @click="select('archive', user)">归档</button>
              <button v-if="user.status === 'pending'" class="secondary" type="button" :disabled="busy" @click="invite(user.username)">补发邀请</button>
              <button v-else class="secondary" type="button" @click="select('recovery', user)">恢复账号</button>
              <button v-if="user.status === 'active'" class="secondary" type="button" @click="select('totp-reset', user)">重置验证</button>
            </td>
          </tr>
          <tr v-if="!data.length">
            <td colspan="4">{{ busy ? '正在加载账号…' : '暂无可查看的账号。' }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <button v-if="next" type="button" class="secondary load-more" :disabled="busy" @click="refresh(next)">加载更多</button>

    <dialog ref="dialog" aria-labelledby="user-action-title" @close="resetDialog">
      <form v-if="pending" @submit.prevent="change">
        <h2 id="user-action-title">{{ actionLabel[pending.action] }}账号</h2>
        <p class="muted dialog-copy">
          将{{ actionLabel[pending.action] }} {{ pending.user.username }}。
          <span v-if="pending.action === 'recovery'">链接兑换后，旧登录会话将失效。</span>
          <span v-else-if="pending.action !== 'enable'">该账号的现有登录会话将失效。</span>
        </p>
        <div v-if="pending.action !== 'recovery'" class="field">
          <label for="user-action-reason">操作原因</label>
          <input id="user-action-reason" v-model="reason" maxlength="200" autofocus required>
        </div>
        <div v-if="pending.action === 'totp-reset'" class="field">
          <label for="admin-password">管理员当前密码</label>
          <input id="admin-password" v-model="adminPassword" type="password" autocomplete="current-password" maxlength="256" required>
        </div>
        <p v-if="dialogError" class="error" role="alert">{{ dialogError }}</p>
        <div class="dialog-actions">
          <button type="button" class="secondary" @click="dialog?.close()">取消</button>
          <button type="submit" class="primary" :disabled="busy">确认{{ actionLabel[pending.action] }}</button>
        </div>
      </form>
    </dialog>
  </section>
</template>
