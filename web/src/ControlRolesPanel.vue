<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'

type Role = { id: string; name: string; allowed_actions: string[]; version: number }
const titles = {
  view: '画面', input: '键鼠', audio: '音频', clipboard: '剪贴板', file_transfer: '文件传输',
}
const props = defineProps<{ csrf: string }>()
const emit = defineEmits<{ expired: [] }>()
const roles = ref<Role[]>([])
const draft = ref<Role>()
const error = ref('')
const busy = ref(false)

function edit(role?: Role) {
  error.value = ''
  draft.value = role
    ? { ...role, allowed_actions: [...role.allowed_actions] }
    : { id: '', name: '', allowed_actions: ['view'], version: 0 }
}

function explain(cause: unknown) {
  if (cause instanceof RequestError && cause.code === 'session_required') emit('expired')
  error.value = cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
}

async function refresh() {
  roles.value = await api<Role[]>('/v1/admin/control-roles')
}

async function save() {
  if (busy.value || !draft.value) return
  const { id, name, allowed_actions, version } = draft.value
  busy.value = true
  error.value = ''
  try {
    await api('/v1/admin/control-roles' + (id ? '/' + id : ''), json({ name, allowed_actions, version }, props.csrf))
    draft.value = undefined
    await refresh()
  } catch (cause) {
    explain(cause)
    if (cause instanceof RequestError && cause.code === 'control_role_stale') {
      draft.value = undefined
      await refresh().catch(explain)
    }
  } finally {
    busy.value = false
  }
}

onMounted(() => refresh().catch(explain))
</script>

<template>
  <section class="users-surface" aria-labelledby="control-role-title">
    <div class="section-heading">
      <div>
        <h2 id="control-role-title">控制角色</h2>
        <p class="muted">配置远程会话可申请的操作。</p>
      </div>
      <button class="primary" type="button" @click="edit()">新建控制角色</button>
    </div>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <form v-if="draft" class="link-form" @submit.prevent="save">
      <div class="field">
        <label for="control-role-name">控制角色名称</label>
        <input id="control-role-name" v-model="draft.name" required maxlength="80">
      </div>
      <fieldset class="control-actions">
        <legend>允许的操作</legend>
        <label v-for="(label, key) in titles" :key="key">
          <input v-model="draft.allowed_actions" :value="key" type="checkbox">{{ label }}
        </label>
      </fieldset>
      <div class="user-tools">
        <button class="primary" type="submit" :disabled="busy || !draft.allowed_actions.length">保存控制角色</button>
        <button class="secondary" type="button" @click="draft = undefined">取消</button>
      </div>
    </form>
    <ul v-if="roles.length" class="member-list">
      <li v-for="role in roles" :key="role.id">
        <div><strong>{{ role.name }}</strong><small>{{ role.allowed_actions.map(action => titles[action as keyof typeof titles]).join('、') }}</small></div>
        <button class="secondary" type="button" @click="edit(role)">编辑</button>
      </li>
    </ul>
    <p v-else class="muted">暂无控制角色。</p>
  </section>
</template>
