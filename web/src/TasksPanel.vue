<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'

import { taskKinds as kinds, taskProgress as progress, taskStates as states, type TaskState, type TaskSummary } from './overview'

type Target = {
  id: string; object_key: string; state: TaskState; result_code: string; can_retry: boolean; version: number
  can_cancel?: boolean; operation_id?: string
}
type Attempt = {
  id?: string; target_id: string; sequence: number; result_code?: string; started_at: string; finished_at?: string
  receipt_id?: string
}
type InstallStep = {
  name: 'images' | 'database' | 'services' | 'health'
  state: 'pending' | 'running' | 'completed' | 'failed' | 'interrupted'
  attempts: number
  updated_at?: string
}
type WindowsDelivery = {
  manifest_sha256: string
  verified_at?: string
  verifier_key_sha256?: string
  manifest: {
    approval: {
      version: string
      source_commit: string
      brand: { id: string; revision: number; name: string; icon_sha256: string }
      center: { public_origin: string; id_server: string }
      group_ids: string[]
      builds: Record<string, { manifest_sha256: string; client_sha256: string }>
    }
    packages: Record<string, { file: string; sha256: string; bytes: number }>
  }
}
type Task = TaskSummary & {
  targets: Target[]
  attempts: Attempt[]
  installation?: { deployment_id: string; revision: number; steps: InstallStep[] }
  windows_delivery?: WindowsDelivery
  windows_rollout?: {
    release_id: string; rollback_release_id?: string; batch: number; batches: number; revision: number
    state: 'active' | 'ready' | 'paused' | 'succeeded' | 'canceled'; deadline: string
    devices: { device_id: string; batch: number; target_id: string }[]
  }
}

const props = defineProps<{ csrf: string; admin: boolean; windowsReleaseEnabled: boolean }>()
const emit = defineEmits<{ expired: [] }>()
const results: Record<string, string> = {
  pending: '账号待激活', exists: '账号已存在', import_failed: '账号写入失败', pending_approval: '等待设备审批',
  approved: '设备已通过审批', rejected: '设备已被拒绝', retired: '待审批设备已退役',
  lease_expired: '执行租约已过期', authorization_changed: '重试申请人的管理员权限已变更',
  delivery_succeeded: '外部执行成功', delivery_failed: '外部执行失败', canceled: '执行前已取消',
  windows_verified: '开发签名及双架构摘要已验证',
  windows_files_missing: '暂存文件缺失或不可读',
  windows_manifest_invalid: '清单摘要或中心身份不符',
  windows_signature_invalid: '开发签名验证失败',
  windows_package_invalid: '归档内容或双架构摘要不符',
  windows_brand_conflict: '品牌修订已绑定不同内容',
  relay_pending_enrollment: '等待中继登记', relay_pending_probe: '已登记，等待验活',
  relay_available: '中继已入池', relay_draining: '中继已排空新会话', relay_disabled: '中继已停用',
  relay_resumed: '中继已恢复', relay_endpoint_changed: '公布地址已验证并更新',
  relay_probe_failed: '公布地址探测未通过', relay_health_required: '等待当前配置的签名健康回执',
  relay_upgrade_pending: '等待中继排空', relay_upgrade_claimed: '节点已领取升级任务',
  relay_upgrade_succeeded: '中继升级已验证', relay_upgrade_rolled_back: '中继原版本已恢复', relay_upgrade_canceled: '中继升级已取消',
  windows_batch_waiting: '等待放行', windows_batch_open: '可获取安装器', windows_batch_healthy: '版本与健康已验证',
  windows_batch_device_changed: '设备身份或所属组已变更', windows_batch_release_revoked: '发布已撤回',
  windows_batch_health_timeout: '未在期限内收到健康结果', windows_batch_install_failed: '运维报告安装失败',
  windows_batch_canceled: '发布已取消', windows_batch_rollback_verified: '原版本已验证',
}
const rolloutStates = { active: '当前批次已放行', ready: '可放行下一批', paused: '已暂停', succeeded: '全部通过', canceled: '已取消' }
const steps = { images: '准备镜像', database: '检查数据库', services: '启动服务', health: '验证服务健康' }
const installStates = { pending: '待执行', running: '执行中', completed: '成功', failed: '失败', interrupted: '已中断' }
const base = '/v1/admin/tasks'
const retryOnly = new URLSearchParams(location.search).get('retry') === 'available'
const tasks = ref<TaskSummary[]>([])
const next = ref('')
const task = ref<Task>()
const viewedRevision = ref('')
const selected = ref<string[]>([])
const chosen = computed(() => task.value?.targets.filter(target => selected.value.includes(target.id)) ?? [])
const retrySelection = computed(() => chosen.value.filter(target => target.can_retry))
const cancelSelection = computed(() => chosen.value.filter(target => target.can_cancel))
const action = ref<'retry' | 'cancel'>('retry')
const selection = computed(() => action.value === 'retry' ? retrySelection.value : cancelSelection.value)
const error = ref('')
const notice = ref('')
const busy = ref(false)
const detailHeading = ref<HTMLElement>()
const confirm = ref<HTMLDialogElement>()
const dialogError = ref('')
const canVerifyWindows = ref(false)
const windowsDialog = ref<HTMLDialogElement>()
const windowsDigest = ref('')
const windowsError = ref('')
const releaseDialog = ref<HTMLDialogElement>()
const releaseError = ref('')
const rolloutOnly = ref(false)
const published = ref('')

function message(cause: unknown) {
  if (cause instanceof RequestError && cause.status === 401) emit('expired')
  return cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
}

function formatTime(value: string) {
  return new Date(value).toLocaleString('zh-CN')
}

async function readList(after = '') {
  const filters = new URLSearchParams()
  if (retryOnly) filters.set('retry', 'available')
  if (after) filters.set('before', after)
  const response = await api<{ tasks: TaskSummary[]; next_cursor?: string; can_verify_windows?: boolean }>(base + (filters.size ? '?' + filters : ''))
  tasks.value = after ? [...tasks.value, ...response.tasks] : response.tasks
  next.value = response.next_cursor ?? ''
  canVerifyWindows.value = !!response.can_verify_windows
}

function openWindowsDialog() {
  windowsDigest.value = ''
  windowsError.value = ''
  windowsDialog.value?.showModal()
}

async function submitWindowsDelivery() {
  const digest = windowsDigest.value.trim().toLowerCase()
  if (busy.value || !/^[0-9a-f]{64}$/.test(digest)) return
  busy.value = true
  windowsError.value = ''
  try {
    const task = await api<{ id: string }>(base + '/windows-delivery', json({
      request_key: 'windows-' + digest, manifest_sha256: digest,
    }, props.csrf))
    windowsDialog.value?.close()
    await readList()
    await openTask(task.id)
    notice.value = '验证任务已提交。'
  } catch (cause) {
    if (windowsDialog.value?.open) windowsError.value = message(cause)
    else error.value = message(cause)
  } finally {
    busy.value = false
  }
}

async function readDetail(id: string, revision = '') {
  const query = revision ? '?' + new URLSearchParams({ revision }) : ''
  task.value = await api<Task>(base + '/' + encodeURIComponent(id) + query)
  viewedRevision.value = revision
  published.value = ''
  selected.value = []
}

async function publishWindows() {
  if (!task.value?.windows_delivery?.verified_at || busy.value) return
  busy.value = true
  releaseError.value = ''
  try {
    const release = await api<{ id: string }>(base + '/' + task.value.id + '/windows-release', json({ rollout_only: rolloutOnly.value }, props.csrf))
    published.value = release.id
    releaseDialog.value?.close()
  } catch (cause) {
    releaseError.value = message(cause)
  } finally {
    busy.value = false
  }
}

async function changeRollout(action: string, device = '') {
  const current = task.value
  if (!current?.windows_rollout || busy.value) return
  if (action === 'cancel' && !window.confirm('取消尚未执行的发布并保留已有结果？')) return
  if (action === 'fail' && !window.confirm('记录此设备安装失败并暂停后续批次？')) return
  busy.value = true
  error.value = ''
  try {
    await api(base + '/' + current.id + '/windows-rollout',
      json({ action, revision: current.windows_rollout.revision, device_id: device }, props.csrf))
    await readDetail(current.id)
    await readList()
  } catch (cause) {
    error.value = message(cause)
  } finally {
    busy.value = false
  }
}

async function refresh(after = '') {
  busy.value = true
  error.value = ''
  try {
    await readList(after)
    if (task.value && !after) await readDetail(task.value.id, viewedRevision.value)
  } catch (cause) {
    if (cause instanceof RequestError && cause.status === 404) task.value = undefined
    error.value = message(cause)
  } finally {
    busy.value = false
  }
}

async function openTask(id: string, revision = '') {
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    await readDetail(id, revision)
    await nextTick()
    detailHeading.value?.focus()
  } catch (cause) {
    task.value = undefined
    error.value = message(cause)
  } finally {
    busy.value = false
  }
}

function confirmSelection(next: 'retry' | 'cancel') {
  action.value = next
  dialogError.value = ''
  confirm.value?.showModal()
}

async function submitSelection() {
  if (!task.value || !selection.value.length || busy.value) return
  const id = task.value.id
  const targets = selection.value.map(({ id, version }) => ({ id, version }))
  busy.value = true
  dialogError.value = ''
  try {
    const reply = await api<{ queued?: number; canceled?: number }>(base + '/' + id + '/' + action.value, json({ targets }, props.csrf))
    confirm.value?.close()
    notice.value = action.value === 'retry' ? `已提交 ${reply.queued} 项重试。` : `已取消 ${reply.canceled} 项。`
    await refresh()
  } catch (cause) {
    dialogError.value = message(cause)
  } finally {
    busy.value = false
  }
}

onMounted(async () => {
  await refresh()
  const query = new URLSearchParams(location.search)
  const id = query.get('task')
  if (id) await openTask(id, query.get('revision') ?? '')
})
</script>

<template>
  <section class="users-surface" aria-labelledby="tasks-heading">
    <div class="section-heading">
      <h2 id="tasks-heading">任务列表</h2>
      <div class="control-actions">
        <a v-if="admin" href="/relays?add=1">接入区域中继</a>
        <button v-if="canVerifyWindows && !retryOnly" class="secondary" :disabled="busy" @click="openWindowsDialog">验证 Windows 开发包</button>
        <button class="secondary" :disabled="busy" @click="refresh()">刷新</button>
      </div>
    </div>
    <p v-if="retryOnly" class="muted active-filter">仅显示含可重试失败对象的任务。<a href="/tasks">显示全部任务</a></p>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <p v-if="notice" class="muted" role="status">{{ notice }}</p>
    <div v-if="tasks.length" class="table-wrap">
      <table>
        <thead><tr><th>类型</th><th>设备组</th><th>创建时间</th><th>进度</th><th>操作</th></tr></thead>
        <tbody>
          <tr v-for="item in tasks" :key="item.id">
            <td>{{ kinds[item.kind] }}</td><td>{{ item.group || '—' }}</td><td>{{ formatTime(item.created_at) }}</td>
            <td>{{ progress(item) }}</td>
            <td><button class="secondary" :disabled="busy" @click="openTask(item.id)">详情</button></td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else-if="!busy && !error" class="muted">{{ retryOnly ? '暂无可重试的任务。' : '暂无任务。' }}</p>
    <button v-if="next" class="secondary" :disabled="busy" @click="refresh(next)">加载更多</button>
  </section>
  <section v-if="task" class="users-surface" aria-labelledby="task-detail-heading">
    <div class="section-heading">
      <h2 id="task-detail-heading" ref="detailHeading" tabindex="-1">{{ kinds[task.kind] }}详情</h2>
      <div v-if="task.targets.some(target => target.can_retry || target.can_cancel)" class="control-actions">
        <button v-if="task.targets.some(target => target.can_retry)" class="primary" :disabled="!retrySelection.length || busy" @click="confirmSelection('retry')">重试所选失败项</button>
        <button v-if="task.targets.some(target => target.can_cancel)" class="secondary" :disabled="!cancelSelection.length || busy" @click="confirmSelection('cancel')">取消所选未开始项</button>
      </div>
    </div>
    <dl>
      <div><dt>任务 ID</dt><dd><code>{{ task.id }}</code></dd></div>
      <div v-if="task.device_id"><dt>设备 ID</dt><dd><code>{{ task.device_id }}</code></dd></div>
      <div v-if="task.installation"><dt>部署 ID</dt><dd><code>{{ task.installation.deployment_id }}</code></dd></div>
      <div v-if="task.installation"><dt>记录修订</dt><dd>{{ task.installation.revision }}</dd></div>
    </dl>
    <section v-if="task.windows_rollout">
      <h3>批次 {{ task.windows_rollout.batch }} / {{ task.windows_rollout.batches }} · {{ rolloutStates[task.windows_rollout.state] }}</h3>
      <p>发布 ID <code>{{ task.windows_rollout.release_id }}</code></p>
      <p v-if="task.windows_rollout.rollback_release_id">回滚发布 ID <code>{{ task.windows_rollout.rollback_release_id }}</code></p>
      <p v-if="task.windows_rollout.state === 'active'">健康检查截止 {{ formatTime(task.windows_rollout.deadline) }}</p>
      <div v-if="!['canceled', 'succeeded'].includes(task.windows_rollout.state)" class="control-actions">
        <button class="secondary" :disabled="busy" @click="changeRollout('review')">核对设备结果</button>
        <button v-if="task.windows_rollout.state === 'ready'" class="primary" :disabled="busy" @click="changeRollout('advance')">放行下一批</button>
        <button v-if="task.windows_rollout.state === 'paused' && !task.queued" class="primary" :disabled="busy" @click="changeRollout('retry')">重试本批失败设备</button>
        <button class="secondary" :disabled="busy" @click="changeRollout('cancel')">取消待执行设备</button>
      </div>
    </section>
    <section v-if="task.windows_delivery" class="windows-evidence" aria-labelledby="windows-evidence-heading">
      <h3 id="windows-evidence-heading">构建与签名证据</h3>
      <p v-if="published" class="inline-success" role="status">开发包已发布。<a href="/windows-releases">查看 Windows 发布</a></p>
      <button v-else-if="admin && windowsReleaseEnabled && task.windows_delivery.verified_at" type="button" class="primary"
        :disabled="busy" @click="releaseError = ''; rolloutOnly = false; releaseDialog?.showModal()">发布已验证开发包</button>
      <dl>
        <div><dt>版本</dt><dd>{{ task.windows_delivery.manifest.approval.version }}</dd></div>
        <div><dt>品牌模板</dt><dd>{{ task.windows_delivery.manifest.approval.brand.name }}（{{ task.windows_delivery.manifest.approval.brand.id }} · 修订 {{ task.windows_delivery.manifest.approval.brand.revision }}）</dd></div>
        <div><dt>图标 SHA-256</dt><dd><code>{{ task.windows_delivery.manifest.approval.brand.icon_sha256 }}</code></dd></div>
        <div><dt>构建源码</dt><dd><code>{{ task.windows_delivery.manifest.approval.source_commit }}</code></dd></div>
        <div><dt>中心</dt><dd>{{ task.windows_delivery.manifest.approval.center.public_origin }}</dd></div>
        <div><dt>目标组</dt><dd><ul><li v-for="group in task.windows_delivery.manifest.approval.group_ids" :key="group"><code>{{ group }}</code></li></ul></dd></div>
        <div><dt>清单 SHA-256</dt><dd><code>{{ task.windows_delivery.manifest_sha256 }}</code></dd></div>
        <div v-if="task.windows_delivery.verified_at"><dt>开发签名验证时间</dt><dd>{{ formatTime(task.windows_delivery.verified_at) }}</dd></div>
        <div v-if="task.windows_delivery.verifier_key_sha256"><dt>开发公钥 SHA-256</dt><dd><code>{{ task.windows_delivery.verifier_key_sha256 }}</code></dd></div>
      </dl>
      <div class="table-wrap">
        <table aria-label="Windows 工件">
          <thead><tr><th scope="col">架构</th><th scope="col">归档 SHA-256</th><th scope="col">原生构建证据 SHA-256</th></tr></thead>
          <tbody>
            <tr v-for="(artifact, arch) in task.windows_delivery.manifest.packages" :key="arch">
              <th scope="row">{{ arch }}</th>
              <td><code>{{ artifact.sha256 }}</code></td>
              <td><code>{{ task.windows_delivery.manifest.approval.builds[arch]?.manifest_sha256 }}</code></td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
    <div v-if="task.installation" class="table-wrap">
      <table>
        <thead><tr><th>步骤</th><th>状态</th><th>尝试次数</th><th>更新时间</th></tr></thead>
        <tbody>
          <tr v-for="step in task.installation.steps" :key="step.name">
            <td>{{ steps[step.name] }}</td><td>{{ installStates[step.state] }}</td>
            <td>{{ step.attempts }}</td><td>{{ step.updated_at ? formatTime(step.updated_at) : '—' }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <div v-else class="table-wrap">
      <table>
        <thead><tr><th>对象</th><th>状态</th><th>结果</th><th>执行记录</th></tr></thead>
        <tbody>
          <tr v-for="target in task.targets" :key="target.id">
            <td>
              <label v-if="target.can_retry || target.can_cancel" class="control-actions"><input v-model="selected" type="checkbox" :value="target.id" :disabled="busy"> {{ target.object_key }}</label>
              <a v-else-if="task.kind === 'relay_enrollment' || task.kind === 'relay_lifecycle'" :href="'/relays?node=' + target.object_key">{{ target.object_key }}</a>
              <span v-else>{{ target.object_key }}</span>
              <div v-if="task.windows_rollout" class="muted">第 {{ task.windows_rollout.devices.find(device => device.target_id === target.id)?.batch }} 批</div>
              <div v-if="target.operation_id" class="muted">操作 ID <code>{{ target.operation_id }}</code></div>
            </td>
            <td>{{ (task.kind === 'relay_enrollment' || task.windows_rollout) && target.state === 'waiting' ? '待执行' : states[target.state] }}</td>
            <td>{{ results[target.result_code] ?? '—' }}
              <template v-if="task.windows_rollout && !['canceled', 'succeeded'].includes(task.windows_rollout.state)">
                <button v-if="target.state === 'queued'" class="secondary" :disabled="busy" @click="changeRollout('fail', target.object_key)">记录安装失败</button>
                <button v-if="target.state === 'failed' && target.result_code !== 'windows_batch_rollback_verified' && task.windows_rollout.rollback_release_id"
                  class="secondary" :disabled="busy" @click="changeRollout('confirm_rollback', target.object_key)">核对回滚</button>
              </template>
            </td>
            <td>
              <div v-for="attempt in task.attempts.filter(row => row.target_id === target.id)" :key="attempt.sequence">
                <details>
                  <summary>{{ task.kind === 'delivery' || task.windows_rollout ? '第 ' + attempt.sequence + ' 次尝试' : attempt.sequence ? '第 ' + attempt.sequence + ' 次重试' : '首次处理' }} · {{ attempt.result_code ? results[attempt.result_code] : '执行中' }}</summary>
                  <div>开始于 {{ formatTime(attempt.started_at) }}</div>
                  <div v-if="attempt.finished_at">完成于 {{ formatTime(attempt.finished_at) }}</div>
                  <div v-if="attempt.id"><code>{{ attempt.id }}</code></div>
                  <div v-if="attempt.receipt_id">外部回执 <code>{{ attempt.receipt_id }}</code></div>
                </details>
              </div>
              <span v-if="!task.attempts.some(row => row.target_id === target.id)">—</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
  <dialog ref="releaseDialog" aria-labelledby="release-dialog-heading" @cancel.prevent="!busy && releaseDialog?.close()">
    <form @submit.prevent="publishWindows">
      <h2 id="release-dialog-heading">确认发布开发包</h2>
      <label class="control-actions"><input type="checkbox" v-model="rolloutOnly">仅通过分批任务分发</label>
      <p v-if="rolloutOnly">发布签名安装器后，已获准的当前批次可通过部署工具获取。</p>
      <p v-else>已批准的目标设备组可下载此版本的 x64 和 ARM64 开发 ZIP。</p>
      <p v-if="releaseError" class="error" role="alert">{{ releaseError }}</p>
      <div class="dialog-actions">
        <button type="button" class="secondary" :disabled="busy" @click="releaseDialog?.close()">取消</button>
        <button type="submit" class="primary" :disabled="busy">确认发布</button>
      </div>
    </form>
  </dialog>
  <dialog ref="windowsDialog" aria-labelledby="windows-dialog-heading" @cancel.prevent="!busy && windowsDialog?.close()">
    <form @submit.prevent="submitWindowsDelivery">
      <h2 id="windows-dialog-heading">验证 Windows 开发包</h2>
      <p>输入已存放在中心暂存区的发布清单 SHA-256。</p>
      <div class="field"><label for="windows-digest">清单 SHA-256</label>
        <input id="windows-digest" v-model.trim="windowsDigest" required pattern="[0-9A-Fa-f]{64}" maxlength="64" spellcheck="false">
      </div>
      <p v-if="windowsError" class="error" role="alert">{{ windowsError }}</p>
      <div class="dialog-actions">
        <button type="button" class="secondary" :disabled="busy" @click="windowsDialog?.close()">取消</button>
        <button type="submit" class="primary" :disabled="busy">提交验证</button>
      </div>
    </form>
  </dialog>
  <dialog ref="confirm" aria-labelledby="task-retry-heading" @cancel.prevent="!busy && confirm?.close()">
    <h2 id="task-retry-heading">{{ action === 'retry' ? '确认重试' : '确认取消' }}</h2>
    <p>{{ action === 'retry' ? '将重新调度以下失败对象。' : '将取消以下尚未开始的对象。' }}</p>
    <ul><li v-for="target in selection" :key="target.id">{{ target.object_key }}</li></ul>
    <p v-if="dialogError" class="error" role="alert">{{ dialogError }}</p>
    <div class="dialog-actions">
      <button class="secondary" :disabled="busy" @click="confirm?.close()">取消</button>
      <button class="primary" :disabled="busy" @click="submitSelection">{{ action === 'retry' ? '确认重试' : '确认取消' }}</button>
    </div>
  </dialog>
</template>

<style scoped>
.windows-evidence code { overflow-wrap: anywhere; }
.windows-evidence ul { padding-inline-start: 1.25rem; }
</style>
