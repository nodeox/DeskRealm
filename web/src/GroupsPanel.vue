<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'
import ControlRolesPanel from './ControlRolesPanel.vue'

type Group = { id: string; name: string }
type Grant = { id: string; username: string; role: string; scope: string; group_id?: string }
const props = defineProps<{ csrf: string }>()
const emit = defineEmits<{ expired: [] }>()
const groups = ref<Group[]>([])
const grants = ref<Grant[]>([])
const members = ref<string[]>([])
const name = ref('')
const selected = ref('')
const member = ref('')
const username = ref('')
const role = ref('user_viewer')
const scope = ref('group')
const grantGroup = ref('')
const error = ref('')
const busy = ref(false)
const revoking = ref<Grant>()
const revokeDialog = ref<HTMLDialogElement>()

const roleName: Record<string, string> = { user_viewer: '用户查看员', user_manager: '用户管理员' }
const scopeName: Record<string, string> = { organization: '组织', group: '用户组', self: '本人' }

async function execute(action: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    await action()
  } catch (cause) {
    if (cause instanceof RequestError && cause.code === 'session_required') emit('expired')
    error.value = cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
  } finally {
    busy.value = false
  }
}

async function refresh() {
  const [groupList, grantList] = await Promise.all([
    api<Group[]>('/v1/admin/groups'),
    api<Grant[]>('/v1/admin/role-grants'),
  ])
  groups.value = groupList
  grants.value = grantList
}

async function loadMembers() {
  members.value = selected.value ? await api<string[]>('/v1/admin/groups/' + selected.value + '/members') : []
}

function createGroup() {
  return execute(async () => {
    const group = await api<Group>('/v1/admin/groups', json({ name: name.value }, props.csrf))
    name.value = ''
    selected.value = group.id
    await refresh()
    await loadMembers()
  })
}

function membership(value: string, remove: boolean) {
  return execute(async () => {
    await api('/v1/admin/groups/' + selected.value + '/members', json({ username: value, remove }, props.csrf))
    member.value = ''
    await loadMembers()
  })
}

function grant() {
  return execute(async () => {
    await api('/v1/admin/role-grants', json({
      username: username.value,
      role: role.value,
      scope: scope.value,
      group_id: scope.value === 'group' ? grantGroup.value : '',
    }, props.csrf))
    username.value = ''
    await refresh()
  })
}

async function openRevoke(value: Grant) {
  revoking.value = value
  error.value = ''
  await nextTick()
  revokeDialog.value?.showModal()
}

function revoke() {
  const value = revoking.value
  if (!value) return
  return execute(async () => {
    await api('/v1/admin/role-grants/' + value.id + '/revoke', json({}, props.csrf))
    revokeDialog.value?.close()
    await refresh()
  })
}

function displayScope(value: Grant) {
  return value.scope === 'group'
    ? groups.value.find(group => group.id === value.group_id)?.name ?? '用户组'
    : scopeName[value.scope] ?? value.scope
}

onMounted(() => execute(refresh))
</script>

<template>
  <p v-if="error" role="alert" class="error">{{ error }}</p>
  <div class="groups-layout">
    <section class="users-surface" aria-labelledby="groups-heading">
      <h2 id="groups-heading">用户组</h2>
      <form class="group-form" @submit.prevent="createGroup">
        <div class="field">
          <label for="group-name">新用户组名称</label>
          <input id="group-name" v-model="name" required maxlength="80">
        </div>
        <button class="primary" type="submit" :disabled="busy">创建用户组</button>
      </form>

      <div class="field">
        <label for="selected-group">查看成员</label>
        <select id="selected-group" v-model="selected" :disabled="busy" @change="execute(loadMembers)">
          <option value="">选择用户组</option>
          <option v-for="group in groups" :key="group.id" :value="group.id">{{ group.name }}</option>
        </select>
      </div>
      <template v-if="selected">
        <form class="group-form" @submit.prevent="membership(member, false)">
          <div class="field">
            <label for="group-user">成员账号</label>
            <input id="group-user" v-model="member" required maxlength="40">
          </div>
          <button type="submit" class="secondary" :disabled="busy">加入用户组</button>
        </form>
        <ul class="member-list" v-if="members.length">
          <li v-for="value in members" :key="value">
            <span>{{ value }}</span>
            <button type="button" class="secondary" :disabled="busy" @click="membership(value, true)">移出用户组</button>
          </li>
        </ul>
        <p v-else class="muted">当前用户组没有成员。</p>
      </template>
    </section>

    <section class="users-surface" aria-labelledby="grant-heading">
      <h2 id="grant-heading">管理角色委派</h2>
      <form class="link-form" @submit.prevent="grant">
        <div class="field">
          <label for="grant-user">授权账号</label>
          <input id="grant-user" v-model="username" required maxlength="40">
        </div>
        <div class="field">
          <label for="grant-role">角色</label>
          <select id="grant-role" v-model="role">
            <option v-for="(label, id) in roleName" :key="id" :value="id">{{ label }}</option>
          </select>
        </div>
        <div class="field">
          <label for="grant-scope">作用域</label>
          <select id="grant-scope" v-model="scope">
            <option v-for="(label, id) in scopeName" :key="id" :value="id">{{ label }}</option>
          </select>
        </div>
        <div v-if="scope === 'group'" class="field">
          <label for="grant-group">授权用户组</label>
          <select id="grant-group" v-model="grantGroup" required>
            <option value="">选择用户组</option>
            <option v-for="group in groups" :key="group.id" :value="group.id">{{ group.name }}</option>
          </select>
        </div>
        <button type="submit" class="primary" :disabled="busy">授予角色</button>
      </form>
      <ul v-if="grants.length" class="member-list">
        <li v-for="value in grants" :key="value.id">
          <div><strong>{{ value.username }}</strong><small>{{ roleName[value.role] }} · {{ displayScope(value) }}</small></div>
          <button type="button" class="secondary" :disabled="busy" @click="openRevoke(value)">撤销</button>
        </li>
      </ul>
      <p v-else class="muted">暂无委派记录。</p>
    </section>
  </div>

  <div class="control-role-layout"><ControlRolesPanel :csrf="csrf" @expired="emit('expired')" /></div>

  <dialog ref="revokeDialog" aria-labelledby="revoke-title">
    <form @submit.prevent="revoke">
      <h2 id="revoke-title">撤销角色委派</h2>
      <p v-if="revoking" class="muted dialog-copy">
        撤销 {{ revoking.username }} 在「{{ displayScope(revoking) }}」的{{ roleName[revoking.role] }}权限。
      </p>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <div class="dialog-actions">
        <button class="secondary" type="button" @click="revokeDialog?.close()">取消</button>
        <button class="primary" type="submit" :disabled="busy">确认撤销</button>
      </div>
    </form>
  </dialog>
</template>
