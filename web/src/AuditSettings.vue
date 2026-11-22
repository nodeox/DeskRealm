<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'

type Grant = { id: string; username: string; scope: string; group?: string; can_export: boolean }
const props = defineProps<{ csrf: string }>()
const emit = defineEmits<{ expired: []; pruned: [] }>()
const capabilities = '/v1/admin/audit-options'
const root = '/v1/admin/audit-grants'
const retention = '/v1/admin/audit-retention'
const labels: Record<string, string> = { organization: '组织', self: '本人事件', user_group: '用户组', device_group: '设备组' }
const grants = ref<Grant[]>([])
const username = ref('')
const scope = ref('device_group')
const group = ref('')
const canExport = ref(false)
const days = ref(90)
const busy = ref(false)
const error = ref('')
const message = ref('')
const confirmation = ref<Grant | 'prune'>()
const dialog = ref<HTMLDialogElement>()

async function run(task: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  message.value = ''
  try { await task() } catch (cause) {
    if (cause instanceof RequestError && cause.code === 'session_required') emit('expired')
    error.value = cause instanceof RequestError ? cause.message : '无法完成请求，请重试。'
  } finally { busy.value = false }
}

function saveGrant() {
  return run(async () => {
    const groupScoped = scope.value === 'user_group' || scope.value === 'device_group'
    await api(root, json({ username: username.value, scope: scope.value, group: groupScoped ? group.value : '', can_export: canExport.value }, props.csrf))
    grants.value = await api<Grant[]>(root)
    message.value = '审计授权已保存。'
  })
}

function saveRetention() {
  return run(async () => {
    await api(retention, json({ days: days.value }, props.csrf))
    message.value = '保留期已保存。'
  })
}

async function ask(value: Grant | 'prune') {
  confirmation.value = value
  error.value = ''
  await nextTick()
  dialog.value?.showModal()
}

function confirm() {
  const action = confirmation.value
  if (!action) return
  return run(async () => {
    if (action === 'prune') {
      const result = await api<{ removed: number }>(retention + '/prune', json({}, props.csrf))
      message.value = '本批清理 ' + result.removed + ' 条已过期事件。'
      emit('pruned')
    } else {
      await api(root + '/' + action.id + '/revoke', json({}, props.csrf))
      grants.value = grants.value.filter(row => row.id !== action.id)
      message.value = '审计授权已撤销。'
    }
    dialog.value?.close()
  })
}

onMounted(() => run(async () => {
  const [settings, rows] = await Promise.all([api<{ retention_days: number }>(capabilities), api<Grant[]>(root)])
  days.value = settings.retention_days
  grants.value = rows
}))
</script>

<template>
  <section class="audit-settings" aria-labelledby="audit-settings-title">
    <h3 id="audit-settings-title">审计设置</h3>
    <p v-if="error" role="alert" class="error">{{ error }}</p>
    <p v-if="message" role="status" class="inline-success">{{ message }}</p>
    <div class="groups-layout">
      <section aria-labelledby="audit-retention-title">
        <h4 id="audit-retention-title">日志保留</h4>
        <form class="group-form" @submit.prevent="saveRetention">
          <div class="field"><label for="audit-retention-days">保留期（天）</label><input id="audit-retention-days" v-model.number="days" type="number" min="1" max="3650" required></div>
          <button class="primary" :disabled="busy" type="submit">保存保留期</button>
        </form>
        <button class="secondary" type="button" :disabled="busy" @click="ask('prune')">清理过期事件</button>
      </section>
      <section aria-labelledby="audit-grants-title">
        <h4 id="audit-grants-title">审计员授权</h4>
        <form @submit.prevent="saveGrant">
          <div class="field"><label for="audit-grant-user">审计员账号</label><input id="audit-grant-user" v-model="username" maxlength="40" required></div>
          <div class="field"><label for="audit-grant-scope">审计范围</label><select id="audit-grant-scope" v-model="scope">
            <option v-for="(label, value) in labels" :key="value" :value="value">{{ label }}</option>
          </select></div>
          <div v-if="scope === 'device_group' || scope === 'user_group'" class="field">
            <label for="audit-grant-group">{{ scope === 'device_group' ? '设备组名称' : '用户组名称' }}</label><input id="audit-grant-group" v-model="group" maxlength="80" required>
          </div>
          <label class="audit-check"><input v-model="canExport" type="checkbox">允许导出该范围</label>
          <button class="primary" type="submit" :disabled="busy">保存审计授权</button>
        </form>
        <ul v-if="grants.length" class="member-list audit-grants">
          <li v-for="grant in grants" :key="grant.id">
            <div>{{ grant.username }}<small>{{ labels[grant.scope] }} {{ grant.group }} · {{ grant.can_export ? '可查询及导出' : '可查询' }}</small></div>
            <button class="secondary" type="button" :disabled="busy" @click="ask(grant)">撤销审计授权</button>
          </li>
        </ul>
      </section>
    </div>
  </section>
  <dialog ref="dialog" aria-labelledby="audit-confirm-title">
    <form v-if="confirmation" @submit.prevent="confirm">
      <h2 id="audit-confirm-title">{{ confirmation === 'prune' ? '确认清理过期事件' : '确认撤销审计授权' }}</h2>
      <p class="muted dialog-copy">
        {{ confirmation === 'prune' ? '每批最多删除 250 条满足保留规则的事件。待回执及待投递事件会保留。' :
          '撤销 ' + confirmation.username + ' 对该范围的访问。' }}
      </p>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <div class="dialog-actions"><button class="secondary" type="button" @click="dialog?.close()">取消</button><button class="primary" type="submit" :disabled="busy">确认</button></div>
    </form>
  </dialog>
</template>
