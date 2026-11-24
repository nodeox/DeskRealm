<script setup lang="ts">
import { computed, nextTick, onMounted, reactive, ref } from 'vue'
import { api, json, RequestError } from './api'

type Node = {
  node_id: string; task_id: string; operation_task_id?: string; name: string; site: string; region: string
  endpoint: string; build: string; config_version: number; capacity: number; revision: number
  state: string; mode: 'enabled' | 'draining' | 'disabled'; health_at?: string; active_sessions?: number
}
type Probe = { endpoint: string; config_version: number; checked_at: string; result: string; task_id?: string }
type RelaySession = { id: string; state: string; lease_until?: string }
type Detail = Node & { probes: Probe[]; sessions: RelaySession[] }
type Credential = { node_id: string; task_id: string; credential: string; expires_at: string }
type Action = 'drain' | 'disable' | 'resume' | 'change_endpoint'

const props = defineProps<{ csrf: string }>()
const emit = defineEmits<{ expired: [] }>()
const base = '/v1/admin/relays'
const nodes = ref<Node[]>([])
const detail = ref<Detail>()
const region = ref('')
const busy = ref(false)
const error = ref('')
const notice = ref<{ text: string; task: string }>()
const credential = ref<Credential>()
const dialog = ref<HTMLDialogElement>()
const detailHeading = ref<HTMLElement>()
const dialogError = ref('')
const action = ref<Action | 'create' | 'reissue'>('create')
const draft = reactive({ name: '', site: '', region: '', endpoint: '', build: '1.1.16', capacity: 16 })
const endpoint = ref('')
const states: Record<string, string> = {
  pending_enrollment: '待登记', awaiting_health: '待签名健康', awaiting_probe: '待探测',
  available: '可用', at_capacity: '已满载', probe_unreachable: '探测故障', draining: '排空中', disabled: '已停用',
}
const probeLabels: Record<string, string> = {
  verified: '已验证', unreachable: '端点不可达', protocol_mismatch: '身份或协议不匹配', invalid_capacity: '容量数据无效',
}
const sessionLabels: Record<string, string> = {
  active: '活动中', revocation_requested: '等待撤销回执', stopped: '已结束', lease_expired: '租约已到期',
}
const actionLabels = {
  create: '接入区域中继', reissue: '补发登记凭据', drain: '排空', disable: '停用', resume: '恢复',
  change_endpoint: '切换公布地址',
}
const results: Record<string, string> = {
  relay_draining: '已排空新会话。', relay_disabled: '已停用节点。', relay_resumed: '已恢复节点。',
  relay_endpoint_changed: '公布地址已更新，节点将同步新配置。',
  relay_probe_failed: '公布地址探测未通过。请检查详情中的探测结果后重试。',
  relay_health_required: '等待当前配置的签名健康回执，请稍后重试。',
}
const regions = computed(() => [...new Set(nodes.value.map(node => node.region))].sort())
const visibleNodes = computed(() => region.value ? nodes.value.filter(node => node.region === region.value) : nodes.value)
const hostCommands = computed(() => credential.value ? [
  "deskrealm-relay prepare --state /var/lib/deskrealm/relay --center '" + location.origin + "' --node " + credential.value.node_id,
  'deskrealm-relay enroll --state /var/lib/deskrealm/relay --credential-file /run/deskrealm/relay-credential',
].join('\n') : '')
const time = (value: string) => new Date(value).toLocaleString('zh-CN')

function message(cause: unknown) {
  if (cause instanceof RequestError && cause.status === 401) emit('expired')
  return cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
}

async function refresh(id = detail.value?.node_id) {
  busy.value = true
  error.value = ''
  try {
    nodes.value = (await api<{ nodes: Node[] }>(base)).nodes
    detail.value = id ? await api<Detail>(base + '/' + encodeURIComponent(id)) : undefined
  } catch (cause) {
    error.value = message(cause)
    if (cause instanceof RequestError && cause.status === 404) detail.value = undefined
  } finally {
    busy.value = false
  }
}

async function openNode(id: string) {
  notice.value = undefined
  await refresh(id)
  await nextTick()
  detailHeading.value?.focus()
}

function show(next: typeof action.value) {
  action.value = next
  dialogError.value = ''
  endpoint.value = ''
  dialog.value?.showModal()
}

async function submit() {
  if (busy.value) return
  const node = detail.value
  const operation = action.value
  let selected = node?.node_id
  busy.value = true
  dialogError.value = ''
  try {
    if (operation === 'create' || operation === 'reissue') {
      const response = await api<Credential>(operation === 'create' ? base : base + '/' + selected + '/credential',
        json(operation === 'create' ? draft : {}, props.csrf))
      credential.value = response
      selected = response.node_id
      notice.value = undefined
    } else if (node) {
      const response = await api<{ task_id: string; result_code: string }>(base + '/' + selected + '/lifecycle',
        json({ action: operation, revision: node.revision, endpoint: endpoint.value.trim() }, props.csrf))
      notice.value = { text: results[response.result_code] ?? response.result_code, task: response.task_id }
    }
    dialog.value?.close()
    await refresh(selected)
  } catch (cause) {
    dialogError.value = message(cause)
  } finally {
    busy.value = false
  }
}

onMounted(async () => {
  const query = new URLSearchParams(location.search)
  await refresh(query.get('node') ?? undefined)
  if (query.get('add') === '1' && !error.value) show('create')
})
</script>

<template>
  <section class="users-surface" aria-labelledby="relay-list-heading" :aria-busy="busy">
    <div class="section-heading">
      <h2 id="relay-list-heading">中继节点</h2>
      <div class="control-actions">
        <button type="button" class="secondary" :disabled="busy" @click="refresh()">刷新中继</button>
        <button type="button" class="primary" :disabled="busy" @click="show('create')">接入区域中继</button>
      </div>
    </div>
    <div v-if="regions.length > 1" class="field">
      <label for="relay-region">筛选区域</label>
      <select id="relay-region" v-model="region"><option value="">全部区域</option><option v-for="item in regions" :key="item">{{ item }}</option></select>
    </div>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <p v-if="notice" role="status">{{ notice.text }} <a :href="'/tasks?task=' + notice.task">查看操作任务</a></p>
    <div v-if="visibleNodes.length" class="table-wrap">
      <table>
        <thead><tr><th>节点</th><th>站点</th><th>区域</th><th>公布地址</th><th>状态</th><th>会话 / 容量</th><th>配置版本</th></tr></thead>
        <tbody><tr v-for="node in visibleNodes" :key="node.node_id">
          <td><button class="secondary" :disabled="busy" @click="openNode(node.node_id)">{{ node.name }}</button></td>
          <td>{{ node.site }}</td><td>{{ node.region }}</td><td><code>{{ node.endpoint }}</code></td>
          <td>{{ states[node.state] ?? node.state }}</td><td>{{ node.active_sessions ?? '—' }} / {{ node.capacity }}</td>
          <td>{{ node.config_version }}</td>
        </tr></tbody>
      </table>
    </div>
    <p v-else-if="!busy && !error" class="muted">暂无中继节点。</p>
  </section>
  <section v-if="credential" class="users-surface" aria-labelledby="relay-credential-heading">
    <div class="section-heading">
      <h2 id="relay-credential-heading">一次性登记凭据</h2>
      <button class="secondary" @click="credential = undefined">关闭凭据</button>
    </div>
    <p>有效期至 {{ time(credential.expires_at) }}。通过授权渠道将凭据交给中继主机运维人员。</p>
    <div class="field"><label for="relay-credential">登记凭据</label><textarea id="relay-credential" :value="credential.credential" readonly rows="2" spellcheck="false" /></div>
    <details>
      <summary>主机登记命令</summary>
      <p>将凭据保存到下方命令使用的文件，权限设为 0600。</p>
      <pre class="relay-commands">{{ hostCommands }}</pre>
    </details>
    <p>节点 ID <code>{{ credential.node_id }}</code> · <a :href="'/tasks?task=' + credential.task_id">登记任务</a></p>
  </section>
  <section v-if="detail" class="users-surface" aria-labelledby="relay-detail-heading">
    <div class="section-heading">
      <h2 id="relay-detail-heading" ref="detailHeading" tabindex="-1">节点详情 · {{ detail.name }}</h2>
      <div class="control-actions">
        <button v-if="detail.state === 'pending_enrollment'" class="secondary" :disabled="busy" @click="show('reissue')">补发登记凭据</button>
        <template v-else>
          <button v-if="detail.mode === 'enabled'" class="secondary" :disabled="busy" @click="show('drain')">排空</button>
          <button v-if="detail.mode !== 'disabled'" class="secondary" :disabled="busy" @click="show('disable')">停用</button>
          <button v-if="detail.mode !== 'enabled'" class="primary" :disabled="busy" @click="show('resume')">恢复</button>
          <button class="secondary" :disabled="busy" @click="show('change_endpoint')">切换公布地址</button>
        </template>
      </div>
    </div>
    <dl>
      <div><dt>构建版本</dt><dd>{{ detail.build }}</dd></div>
      <div><dt>最近健康回执</dt><dd>{{ detail.health_at ? time(detail.health_at) : '—' }}</dd></div>
      <div><dt>任务</dt><dd><a :href="'/tasks?task=' + detail.task_id">登记任务</a>
        <a v-if="detail.operation_task_id" :href="'/tasks?task=' + detail.operation_task_id"> · 最近操作任务</a></dd></div>
    </dl>
    <h3>最近探测</h3>
    <div v-if="detail.probes.length" class="table-wrap"><table>
      <thead><tr><th>时间</th><th>端点</th><th>配置版本</th><th>结果</th></tr></thead>
      <tbody><tr v-for="(probe, i) in detail.probes" :key="i">
        <td>{{ time(probe.checked_at) }}</td><td><code>{{ probe.endpoint }}</code></td><td>{{ probe.config_version }}</td>
        <td><a v-if="probe.task_id" :href="'/tasks?task=' + probe.task_id">{{ probeLabels[probe.result] }}</a>
          <span v-else>{{ probeLabels[probe.result] }}</span></td>
      </tr></tbody>
    </table></div>
    <p v-else class="muted">暂无探测记录。</p>
    <h3>已建立的中继会话</h3>
    <ul v-if="detail.sessions.length" class="member-list">
      <li v-for="session in detail.sessions" :key="session.id">
        <a :href="'/sessions?session=' + session.id">{{ session.id }}</a>
        <span>{{ sessionLabels[session.state] ?? session.state }}</span>
      </li>
    </ul>
    <p v-else class="muted">暂无已建立的会话。</p>
  </section>
  <dialog ref="dialog" aria-labelledby="relay-action-heading" @cancel.prevent="!busy && dialog?.close()">
    <h2 id="relay-action-heading">{{ actionLabels[action] }}</h2>
    <form @submit.prevent="submit">
      <fieldset v-if="action === 'create'" :disabled="busy" class="relay-fields">
        <div class="field"><label for="relay-name">节点名称</label><input id="relay-name" v-model.trim="draft.name" required maxlength="64"></div>
        <div class="field"><label for="relay-site">站点</label><input id="relay-site" v-model.trim="draft.site" required maxlength="64"></div>
        <div class="field"><label for="relay-location">区域</label><input id="relay-location" v-model.trim="draft.region" required maxlength="64"></div>
        <div class="field"><label for="relay-address">公布地址</label><input id="relay-address" v-model.trim="draft.endpoint" required placeholder="relay.example.com:21117"></div>
        <div class="field"><label for="relay-build">构建版本</label><input id="relay-build" v-model.trim="draft.build" required maxlength="64"></div>
        <div class="field"><label for="relay-capacity">并发容量</label><input id="relay-capacity" v-model.number="draft.capacity" type="number" min="1" max="100000" required></div>
      </fieldset>
      <div v-else-if="action === 'change_endpoint'" class="field">
        <label for="relay-new-endpoint">新公布地址</label>
        <input id="relay-new-endpoint" v-model.trim="endpoint" required :disabled="busy" placeholder="relay.example.com:21117">
      </div>
      <p v-else-if="action === 'drain'" class="dialog-copy">停止将新会话分配给 {{ detail?.name }}。已建立的会话继续运行。</p>
      <p v-else-if="action === 'resume'" class="dialog-copy">中心将核对 {{ detail?.name }} 的公布地址和签名健康。</p>
      <p v-else class="dialog-copy">{{ detail?.name }}</p>
      <p v-if="dialogError" class="error" role="alert">{{ dialogError }}</p>
      <div class="dialog-actions">
        <button type="button" class="secondary" :disabled="busy" @click="dialog?.close()">取消</button>
        <button type="submit" class="primary" :disabled="busy">确认{{ actionLabels[action] }}</button>
      </div>
    </form>
  </dialog>
</template>
