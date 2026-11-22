<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'

type UserStatus = 'active' | 'disabled' | 'archived'
type ManagedUser = { id: string; username: string; status: UserStatus; platform_admin: boolean }
type UserList = { users: ManagedUser[]; next?: string }
type Action = 'enable' | 'disable' | 'archive'

const props = defineProps<{ csrf: string }>()
const emit = defineEmits<{ expired: [] }>()
const statusLabel: Record<UserStatus, string> = { active: '已启用', disabled: '已停用', archived: '已归档' }
const actionLabel: Record<Action, string> = { enable: '启用', disable: '停用', archive: '归档' }

const data = ref<ManagedUser[]>([])
const next = ref('')
const busy = ref(false)
const error = ref('')
const result = ref('')
const showForm = ref(false)
const username = ref('')
const password = ref('')
const dialog = ref<HTMLDialogElement>()
const pending = ref<{ action: Action; user: ManagedUser }>()
const reason = ref('')
const dialogError = ref('')

function message(cause: unknown) {
  if (cause instanceof RequestError && cause.status === 401) emit('expired')
  return cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
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
    showForm.value = false
    await refresh()
  } catch (cause) {
    error.value = message(cause)
  } finally {
    password.value = ''
    busy.value = false
  }
}

async function select(action: Action, user: ManagedUser) {
  pending.value = { action, user }
  reason.value = ''
  dialogError.value = ''
  await nextTick()
  dialog.value?.showModal()
}

function resetDialog() {
  pending.value = undefined
  dialogError.value = ''
}

async function change() {
  const selected = pending.value
  if (!selected || busy.value) return
  busy.value = true
  dialogError.value = ''
  result.value = ''
  try {
    const user = await api<ManagedUser>('/v1/admin/users/' + selected.user.id + '/' + selected.action,
      json({ reason: reason.value }, props.csrf))
    data.value = data.value.map(row => row.id === user.id ? user : row)
    result.value = user.username + statusLabel[user.status]
    dialog.value?.close()
  } catch (cause) {
    dialogError.value = message(cause)
  } finally {
    busy.value = false
  }
}

onMounted(() => refresh())
</script>

<template>
  <section class="users-surface" aria-labelledby="users-title">
    <div class="section-heading">
      <div><h2 id="users-title">用户列表</h2><p class="muted">管理组织账号和登录状态。</p></div>
      <button class="primary" type="button" @click="showForm = !showForm">{{ showForm ? '取消添加' : '添加成员' }}</button>
    </div>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <p v-if="result" class="inline-success" role="status">{{ result }}</p>
    <form v-if="showForm" class="member-form" @submit.prevent="create">
      <div class="field">
        <label for="member-name">成员账号</label>
        <input id="member-name" v-model="username" autocomplete="off" maxlength="40" required>
      </div>
      <div class="field">
        <label for="member-password">初始密码</label>
        <input id="member-password" v-model="password" type="password" autocomplete="new-password"
          minlength="12" maxlength="256" required>
      </div>
      <button class="primary" type="submit" :disabled="busy">创建成员</button>
    </form>
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
          <span v-if="pending.action !== 'enable'">该账号的现有登录会话将失效。</span>
        </p>
        <div class="field">
          <label for="user-action-reason">操作原因</label>
          <input id="user-action-reason" v-model="reason" maxlength="200" autofocus required>
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
