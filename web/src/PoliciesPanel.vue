<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { api, json, RequestError } from './api'
import { actionLabels, actionsText, localTime, scopeLabels } from './managed'

type Option = { id: string; name: string }
type Role = Option & { version: number; allowed_actions: string[] }
type Rule = {
  id: string; name: string; version: number; published_version: number | null
  purpose: string; effect: 'allow' | 'deny'; allowed_actions: string[]
  source_user_id?: string; source_group_id?: string; controller_device_id?: string
  target_device_id?: string; target_group_id?: string; control_role_id?: string
}
type Policy = { policy_version: number; name: string; allowed_actions: string[]; unattended_purposes: string[]; published_at: string }
type Assignment = {
  id: string; scope: string; object_id?: string; revision: number
  policy_version: number | null; canary_version: number | null; canary_percent: number
}
type Decision = { allowed: boolean; reason: string; rule_id?: string; rule_version?: number; allowed_actions?: string[]; target_device_id?: string }
type Confirmation = { title: string; summary: string; path: string; data: object; success: string }

const props = defineProps<{ csrf: string }>()
const emit = defineEmits<{ expired: [] }>()
const rulePath = '/v1/admin/access-rules'
const policyPath = '/v1/admin/strategies'
const assignmentPath = policyPath + '/assignments'
const rules = ref<Rule[]>([])
const policies = ref<Policy[]>([])
const assignments = ref<Assignment[]>([])
const users = ref<Option[]>([])
const devices = ref<Option[]>([])
const userGroups = ref<Option[]>([])
const deviceGroups = ref<Option[]>([])
const roles = ref<Role[]>([])
const tab = ref<'rules' | 'strategies'>('rules')
const filter = ref('')
const ruleState = ref('')
const busy = ref(false)
const error = ref('')
const notice = ref('')
const confirmation = ref<Confirmation>()
const dialog = ref<HTMLDialogElement>()
let invoking: HTMLElement | undefined

const ruleDraft = ref<ReturnType<typeof makeRule>>()
const publication = ref<{ name: string; actions: string[]; purposes: string }>()
const assignment = ref<Assignment>()
const rollback = ref<{ row: Assignment; revision: number; historical?: Assignment }>()
const preview = ref({ user: '', controller: '', selector: 'device', target: '', book: '', entry: '', purpose: 'helpdesk', actions: ['view'] })
const decision = ref<Decision>()
watch(preview, () => { decision.value = undefined }, { deep: true })
const previewReasons: Record<string, string> = {
  matched_allow: '匹配已发布的允许规则', explicit_deny: '匹配显式拒绝规则',
  no_matching_allow: '没有覆盖全部所需操作的允许规则', endpoint_unavailable: '端点离线、未批准或不满足连接条件',
  subject_inactive: '来源账号已停用或不存在', address_book_unavailable: '通讯录条目不可用或超出来源账号权限',
}

const visibleRules = computed(() => {
  const text = filter.value.trim().toLowerCase()
  return rules.value.filter(rule => (!ruleState.value || rule.effect === ruleState.value)
    && (!text || [rule.name, rule.purpose, targetOf(rule), sourceOf(rule)].some(value => value.toLowerCase().includes(text))))
})
const draftRole = computed(() => roles.value.find(role => role.id === ruleDraft.value?.role))
const assignmentOptions = computed(() => assignment.value ? scopeOptions(assignment.value.scope) : [])
const assignablePolicies = computed(() => policies.value.filter(item => assignment.value?.scope === 'device' || !item.unattended_purposes.length))

function lookup(options: Option[], id?: string) {
  return options.find(item => item.id === id)?.name ?? id ?? '—'
}
function sourceOf(rule: Rule) {
  return rule.source_user_id ? lookup(users.value, rule.source_user_id) : lookup(userGroups.value, rule.source_group_id)
}
function targetOf(rule: Rule) {
  return rule.target_device_id ? lookup(devices.value, rule.target_device_id) : lookup(deviceGroups.value, rule.target_group_id)
}
function scopeOptions(scope: string) {
  if (scope === 'device') return devices.value
  if (scope === 'user') return users.value
  return scope === 'group' ? deviceGroups.value : []
}
function scopeOf(row: Assignment) {
  return scopeLabels[row.scope] + (row.object_id ? ' · ' + lookup(scopeOptions(row.scope), row.object_id) : '')
}
function policyMix(row: Assignment) {
  const base = row.policy_version === null ? '继承低优先级策略' : '基线版本 ' + row.policy_version
  return base + (row.canary_version === null ? '' : ' · 灰度版本 ' + row.canary_version + '（' + row.canary_percent + '%）')
}
function makeRule(rule?: Rule) {
  return {
    id: rule?.id ?? '', version: rule?.version ?? 0, name: rule?.name ?? '', purpose: rule?.purpose ?? 'helpdesk',
    effect: rule?.effect ?? 'allow', sourceKind: rule?.source_group_id ? 'group' : 'user',
    source: rule?.source_user_id ?? rule?.source_group_id ?? '', controller: rule?.controller_device_id ?? '',
    targetKind: rule?.target_group_id ? 'group' : 'device', target: rule?.target_device_id ?? rule?.target_group_id ?? '',
    role: rule?.control_role_id ?? '', actions: [...(rule?.allowed_actions ?? [])],
  }
}

async function fetchPages<T>(path: string, key: string, cursorKey = 'after') {
  const items: T[] = []
  let cursor: string | number | undefined
  do {
    const data: { next?: string | number } & Record<string, unknown> = await api(path + (cursor ? '?' + new URLSearchParams({ [cursorKey]: String(cursor) }) : ''))
    items.push(...data[key] as T[])
    cursor = data.next
  } while (cursor)
  return items
}

async function load() {
  const [ruleList, policyList, assignmentList, accountList, deviceList, members, groups, roleList] = await Promise.all([
    fetchPages<Rule>(rulePath, 'rules'), fetchPages<Policy>(policyPath, 'policies', 'before'),
    fetchPages<Assignment>(assignmentPath, 'assignments'), fetchPages<{ id: string; username: string }>('/v1/admin/users', 'users'),
    fetchPages<{ id: string; rustdesk_id: string; group_name: string }>('/v1/admin/devices', 'devices'),
    api<Option[]>('/v1/admin/groups'), api<Option[]>('/v1/admin/device-groups'), api<Role[]>('/v1/admin/control-roles'),
  ])
  rules.value = ruleList
  policies.value = policyList
  assignments.value = assignmentList
  users.value = accountList.map(row => ({ id: row.id, name: row.username }))
  devices.value = deviceList.map(row => ({ id: row.id, name: row.rustdesk_id + ' · ' + row.group_name }))
  userGroups.value = members
  deviceGroups.value = groups
  roles.value = roleList
}

async function run(work: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  notice.value = ''
  try { await work() } catch (cause) {
    if (cause instanceof RequestError && cause.status === 401) emit('expired')
    if (cause instanceof RequestError && cause.status === 403) {
      rules.value = []; policies.value = []; assignments.value = []; ruleDraft.value = undefined; assignment.value = undefined
      users.value = []; devices.value = []; userGroups.value = []; deviceGroups.value = []; roles.value = []
      publication.value = undefined; rollback.value = undefined; decision.value = undefined
      dialog.value?.close()
    }
    error.value = cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
  } finally { busy.value = false }
}

async function ask(value: Confirmation) {
  invoking = document.activeElement instanceof HTMLElement ? document.activeElement : undefined
  confirmation.value = value
  error.value = ''
  await nextTick()
  dialog.value?.showModal()
}

function confirm() {
  const value = confirmation.value
  if (!value) return
  return run(async () => {
    await api(value.path, json(value.data, props.csrf))
    dialog.value?.close()
    assignment.value = undefined
    rollback.value = undefined
    publication.value = undefined
    await load()
    notice.value = value.success
  })
}
function saveRule() {
  const draft = ruleDraft.value
  if (!draft) return
  return run(async () => {
    await api(rulePath + (draft.id ? '/' + draft.id : ''), json({
      name: draft.name, purpose: draft.purpose, effect: draft.effect, version: draft.version,
      [draft.sourceKind === 'group' ? 'source_group_id' : 'source_user_id']: draft.source,
      [draft.targetKind === 'group' ? 'target_group_id' : 'target_device_id']: draft.target,
      controller_device_id: draft.controller,
      ...(draft.effect === 'allow' ? { control_role_id: draft.role, control_role_version: draftRole.value?.version, allowed_actions: draft.actions } : {}),
    }, props.csrf))
    ruleDraft.value = undefined
    await load()
    notice.value = '规则草稿已保存。'
  })
}
function changeRule(rule: Rule, archive = false) {
  return ask({
    title: archive ? '确认归档规则' : '确认发布规则',
    summary: rule.name + ' · ' + sourceOf(rule) + ' → ' + targetOf(rule) + ' · ' + rule.purpose + ' · 版本 ' + rule.version,
    path: rulePath + '/' + rule.id + (archive ? '/archive' : '/publish'), data: { version: rule.version },
    success: archive ? '规则已归档。' : '规则已发布。',
  })
}
function simulate() {
  const input = preview.value
  const submitted = JSON.stringify(input)
  decision.value = undefined
  return run(async () => {
    const target = input.selector === 'book' ? { book_id: input.book, entry_id: input.entry }
      : { [input.selector === 'device' ? 'target_device_id' : 'target_rustdesk_id']: input.target }
    const result = await api<Decision>(rulePath + '/preview', json({
      user_id: input.user, controller_device_id: input.controller, purpose: input.purpose,
      requested_actions: input.actions, ...target,
    }, props.csrf))
    if (JSON.stringify(preview.value) === submitted) decision.value = result
  })
}
function publishPolicy() {
  const item = publication.value
  if (!item) return
  return ask({ title: '确认发布策略版本', summary: item.name + ' · ' + actionsText(item.actions) + '。分配后将向匹配设备交付。',
    path: policyPath, data: { name: item.name, allowed_actions: item.actions, unattended_purposes: item.purposes.trim().split(/\s+/).filter(Boolean) },
    success: '策略版本已发布。' })
}
function editAssignment(row?: Assignment) {
  rollback.value = undefined
  assignment.value = row ? { ...row } : { id: '', scope: 'organization', revision: 0, policy_version: null, canary_version: null, canary_percent: 0 }
}
function saveAssignment() {
  const row = assignment.value
  if (!row) return
  return ask({
    title: '确认策略分配', summary: scopeOf(row) + ' · ' + policyMix(row),
    path: assignmentPath + (row.id ? '/' + row.id : ''),
    data: {
      revision: row.revision, policy_version: row.policy_version, canary_version: row.canary_version,
      canary_percent: row.canary_version === null ? 0 : row.canary_percent,
      ...(!row.id ? { scope: row.scope, object_id: row.object_id ?? '' } : {}),
    },
    success: '分配已保存。终端应用结果可在设备详情查看。',
  })
}
function readRevision() {
  const draft = rollback.value
  if (!draft) return
  draft.historical = undefined
  return run(async () => {
    draft.historical = await api<Assignment>(assignmentPath + '/' + draft.row.id + '?revision=' + draft.revision)
  })
}
function confirmRollback() {
  const draft = rollback.value
  if (!draft?.historical) return
  return ask({ title: '确认回滚分配', summary: scopeOf(draft.row) + ' · 恢复修订 ' + draft.historical.revision + ' · ' + policyMix(draft.historical),
    path: assignmentPath + '/' + draft.row.id + '/rollback',
    data: { revision: draft.row.revision, rollback_to_revision: draft.historical.revision },
    success: '回滚已提交并生成新修订。' })
}
onMounted(() => run(load))
</script>

<template>
  <div class="user-tools managed-tabs" aria-label="策略类别">
    <button class="secondary" type="button" :aria-pressed="tab === 'rules'" @click="tab = 'rules'">连接规则</button>
    <button class="secondary" type="button" :aria-pressed="tab === 'strategies'" @click="tab = 'strategies'">设备策略</button>
    <button class="secondary" type="button" :disabled="busy" @click="run(load)">刷新策略</button>
  </div>
  <p v-if="error" class="error" role="alert">{{ error }}</p>
  <p v-if="notice" class="inline-success" role="status">{{ notice }}</p>

  <div v-if="tab === 'rules'" class="managed-stack" :aria-busy="busy">
    <section class="users-surface" aria-labelledby="rule-heading">
      <div class="section-heading"><h2 id="rule-heading">连接规则</h2><button type="button" class="primary" :disabled="busy" @click="ruleDraft = makeRule()">新建规则</button></div>
      <form v-if="ruleDraft" class="link-form" @submit.prevent="saveRule">
        <div class="managed-form">
          <label class="field">规则名称<input v-model.trim="ruleDraft.name" maxlength="80" required></label>
          <label class="field">用途<input v-model.trim="ruleDraft.purpose" pattern="[a-z0-9][a-z0-9_\-]{0,63}" required></label>
          <label class="field">来源类型<select v-model="ruleDraft.sourceKind" @change="ruleDraft.source = ''"><option value="user">用户</option><option value="group">用户组</option></select></label>
          <label class="field">规则来源<select v-model="ruleDraft.source" required><option value="">请选择</option><option v-for="item in (ruleDraft.sourceKind === 'user' ? users : userGroups)" :key="item.id" :value="item.id">{{ item.name }}</option></select></label>
          <label class="field">目标类型<select v-model="ruleDraft.targetKind" @change="ruleDraft.target = ''"><option value="device">设备</option><option value="group">设备组</option></select></label>
          <label class="field">规则目标<select v-model="ruleDraft.target" required><option value="">请选择</option><option v-for="item in scopeOptions(ruleDraft.targetKind)" :key="item.id" :value="item.id">{{ item.name }}</option></select></label>
          <label class="field">主控设备限制<select v-model="ruleDraft.controller"><option value="">不限</option><option v-for="item in devices" :key="item.id" :value="item.id">{{ item.name }}</option></select></label>
          <label class="field">判定<select v-model="ruleDraft.effect"><option value="allow">允许</option><option value="deny">拒绝</option></select></label>
          <label v-if="ruleDraft.effect === 'allow'" class="field">控制角色<select v-model="ruleDraft.role" required @change="ruleDraft.actions = []"><option value="">请选择</option><option v-for="item in roles" :key="item.id" :value="item.id">{{ item.name }} · 版本 {{ item.version }}</option></select></label>
        </div>
        <fieldset v-if="ruleDraft.effect === 'allow' && draftRole" class="control-actions"><legend>允许的操作</legend>
          <label v-for="action in draftRole.allowed_actions" :key="action"><input type="checkbox" v-model="ruleDraft.actions" :value="action">{{ actionLabels[action] }}</label>
        </fieldset>
        <div class="user-tools"><button class="primary" :disabled="busy || (ruleDraft.effect === 'allow' && !ruleDraft.actions.length)">保存规则草稿</button><button type="button" class="secondary" @click="ruleDraft = undefined">取消编辑</button></div>
      </form>
      <div class="managed-form">
        <label class="field">筛选规则<input v-model="filter" placeholder="名称、用途、来源或目标"></label>
        <label class="field">筛选判定<select v-model="ruleState"><option value="">全部判定</option><option value="allow">允许</option><option value="deny">拒绝</option></select></label>
      </div>
      <div class="table-wrap"><table>
        <thead><tr><th scope="col">规则</th><th scope="col">范围</th><th scope="col">版本</th><th scope="col">操作</th></tr></thead>
        <tbody>
          <tr v-for="rule in visibleRules" :key="rule.id">
            <th scope="row">{{ rule.name }}<small>{{ rule.purpose }} · {{ rule.effect === 'allow' ? '允许' : '拒绝' }}</small></th>
            <td>{{ sourceOf(rule) }} → {{ targetOf(rule) }}<small v-if="rule.effect === 'allow'">{{ actionsText(rule.allowed_actions) }}</small></td>
            <td>最新 {{ rule.version }}<small>{{ rule.published_version ? '已发布 ' + rule.published_version : '待发布' }}</small></td>
            <td class="row-actions">
              <button type="button" class="secondary" :disabled="busy" @click="ruleDraft = makeRule(rule)">编辑</button>
              <button v-if="rule.published_version !== rule.version" type="button" class="secondary" :disabled="busy" @click="changeRule(rule)">发布</button>
              <button type="button" class="secondary" :disabled="busy" @click="changeRule(rule, true)">归档</button>
            </td>
          </tr>
          <tr v-if="!visibleRules.length"><td colspan="4">{{ busy ? '正在加载规则…' : '暂无匹配的规则。' }}</td></tr>
        </tbody>
      </table></div>
    </section>

    <section class="users-surface" aria-labelledby="preview-heading">
      <h2 id="preview-heading">连接预览</h2>
      <form class="link-form" @submit.prevent="simulate">
        <div class="managed-form">
          <label class="field">预览来源账号<select v-model="preview.user" required><option value="">请选择</option><option v-for="item in users" :key="item.id" :value="item.id">{{ item.name }}</option></select></label>
          <label class="field">预览主控设备<select v-model="preview.controller" required><option value="">请选择</option><option v-for="item in devices" :key="item.id" :value="item.id">{{ item.name }}</option></select></label>
          <label class="field">预览目标类型<select v-model="preview.selector" @change="preview.target = ''"><option value="device">设备</option><option value="rustdesk">RustDesk ID</option><option value="book">通讯录条目</option></select></label>
          <label v-if="preview.selector === 'device'" class="field">预览目标设备<select v-model="preview.target" required><option value="">请选择</option><option v-for="item in devices" :key="item.id" :value="item.id">{{ item.name }}</option></select></label>
          <label v-else-if="preview.selector === 'rustdesk'" class="field">目标 RustDesk ID<input v-model.trim="preview.target" required></label>
          <template v-else><label class="field">通讯录 ID<input v-model.trim="preview.book" required></label><label class="field">通讯录条目 ID<input v-model.trim="preview.entry" required></label></template>
          <label class="field">预览用途<input v-model.trim="preview.purpose" pattern="[a-z0-9][a-z0-9_\-]{0,63}" required></label>
        </div>
        <fieldset class="control-actions"><legend>申请的操作</legend>
          <label v-for="(label, key) in actionLabels" :key="key"><input type="checkbox" v-model="preview.actions" :value="key">{{ label }}</label>
        </fieldset>
        <button class="primary" :disabled="busy || !preview.actions.length">预览连接规则</button>
      </form>
      <div v-if="decision" class="managed-result" role="status">
        <strong>{{ decision.allowed ? '允许' : '拒绝' }}</strong> · {{ previewReasons[decision.reason] ?? decision.reason }}
        <p v-if="decision.target_device_id">目标设备 {{ lookup(devices, decision.target_device_id) }}</p>
        <p v-if="decision.rule_id" class="device-identifier">匹配规则 {{ rules.find(rule => rule.id === decision?.rule_id && rule.version === decision.rule_version)?.name ?? decision.rule_id }} · 版本 {{ decision.rule_version }}</p>
        <p v-if="decision.allowed">可授予操作 {{ actionsText(decision.allowed_actions) }}</p>
      </div>
    </section>
  </div>

  <div v-else class="managed-stack" :aria-busy="busy">
    <section class="users-surface" aria-labelledby="policy-heading">
      <div class="section-heading"><h2 id="policy-heading">已发布策略版本</h2><button type="button" class="primary" :disabled="busy" @click="publication = { name: '', actions: ['view'], purposes: '' }">新建策略版本</button></div>
      <form v-if="publication" class="link-form" @submit.prevent="publishPolicy">
        <label class="field">策略名称<input v-model.trim="publication.name" maxlength="80" required></label>
        <fieldset class="control-actions"><legend>允许的操作</legend>
          <label v-for="(label, key) in actionLabels" :key="key"><input type="checkbox" v-model="publication.actions" :value="key">{{ label }}</label>
        </fieldset>
        <label class="field">无人值守用途（用空格分隔）<input v-model="publication.purposes"></label>
        <div class="user-tools"><button class="primary" :disabled="busy">发布策略</button><button type="button" class="secondary" @click="publication = undefined">取消编辑</button></div>
      </form>
      <ul v-if="policies.length" class="member-list"><li v-for="item in policies" :key="item.policy_version">
        <div><strong>{{ item.name }} · 版本 {{ item.policy_version }}</strong><small>{{ actionsText(item.allowed_actions) }}<template v-if="item.unattended_purposes.length"> · 无人值守 {{ item.unattended_purposes.join('、') }}</template></small></div>
        <time>{{ localTime(item.published_at) }}</time>
      </li></ul>
      <p v-else class="muted">{{ busy ? '正在加载策略…' : '暂无已发布版本。' }}</p>
    </section>
    <section class="users-surface" aria-labelledby="assignment-heading">
      <div class="section-heading"><h2 id="assignment-heading">策略分配</h2><button type="button" class="primary" :disabled="busy || !policies.length" @click="editAssignment()">新建分配</button></div>
      <form v-if="assignment" class="link-form" @submit.prevent="saveAssignment">
        <p v-if="assignment.id" class="muted">{{ scopeOf(assignment) }} · 修订 {{ assignment.revision }}</p>
        <div class="managed-form">
          <label v-if="!assignment.id" class="field">分配范围<select v-model="assignment.scope" @change="assignment.object_id = ''; assignment.policy_version = null; assignment.canary_version = null"><option v-for="(label, key) in scopeLabels" :key="key" :value="key">{{ label }}</option></select></label>
          <label v-if="!assignment.id && assignment.scope !== 'organization'" class="field">分配对象<select v-model="assignment.object_id" required><option value="">请选择</option><option v-for="item in assignmentOptions" :key="item.id" :value="item.id">{{ item.name }}</option></select></label>
          <label class="field">基线版本<select v-model="assignment.policy_version"><option :value="null">继承</option><option v-for="item in assignablePolicies" :key="item.policy_version" :value="item.policy_version">{{ item.name }} · {{ item.policy_version }}</option></select></label>
          <label class="field">灰度版本<select v-model="assignment.canary_version"><option :value="null">关闭灰度</option><option v-for="item in assignablePolicies" :key="item.policy_version" :value="item.policy_version">{{ item.name }} · {{ item.policy_version }}</option></select></label>
          <label v-if="assignment.canary_version !== null" class="field">灰度比例（%）<input type="number" min="1" max="99" v-model.number="assignment.canary_percent" required></label>
        </div>
        <div class="user-tools"><button class="primary" :disabled="busy || (!assignment.id && assignment.policy_version === null && assignment.canary_version === null)">保存分配</button><button type="button" class="secondary" @click="assignment = undefined">取消编辑</button></div>
      </form>
      <form v-if="rollback" class="link-form" @submit.prevent="readRevision">
        <p class="muted">{{ scopeOf(rollback.row) }} · 当前修订 {{ rollback.row.revision }}</p>
        <label class="field">目标修订<input type="number" min="1" :max="rollback.row.revision - 1" v-model.number="rollback.revision" required @input="rollback.historical = undefined"></label>
        <p v-if="rollback.historical" class="muted" role="status">{{ policyMix(rollback.historical) }}</p>
        <div class="user-tools">
          <button class="secondary" :disabled="busy">查看历史修订</button>
          <button v-if="rollback.historical" type="button" class="primary" :disabled="busy" @click="confirmRollback">回滚到此修订</button>
          <button type="button" class="secondary" @click="rollback = undefined">取消回滚</button>
        </div>
      </form>
      <div class="table-wrap"><table>
        <thead><tr><th scope="col">范围</th><th scope="col">版本</th><th scope="col">修订</th><th scope="col">操作</th></tr></thead>
        <tbody>
          <tr v-for="row in assignments" :key="row.id">
            <th scope="row">{{ scopeOf(row) }}</th><td>{{ policyMix(row) }}</td><td>{{ row.revision }}</td>
            <td class="row-actions">
              <button type="button" class="secondary" :disabled="busy" @click="editAssignment(row)">编辑分配</button>
              <button v-if="row.revision > 1" type="button" class="secondary" :disabled="busy" @click="assignment = undefined; rollback = { row, revision: row.revision - 1 }">回滚</button>
              <a v-if="row.scope === 'device'" :href="'/devices?device=' + row.object_id">查看设备回执</a>
            </td>
          </tr>
          <tr v-if="!assignments.length"><td colspan="4">{{ busy ? '正在加载分配…' : '暂无策略分配。' }}</td></tr>
        </tbody>
      </table></div>
    </section>
  </div>

  <dialog ref="dialog" aria-labelledby="strategy-confirm-title" @close="invoking?.focus()">
    <form v-if="confirmation" @submit.prevent="confirm">
      <h2 id="strategy-confirm-title">{{ confirmation.title }}</h2>
      <p class="muted dialog-copy device-identifier">{{ confirmation.summary }}</p>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <div class="dialog-actions"><button type="button" class="secondary" @click="dialog?.close()">返回</button><button class="primary" :disabled="busy">确认提交</button></div>
    </form>
  </dialog>
</template>
