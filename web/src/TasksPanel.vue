<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'

import { taskKinds as kinds, taskProgress as progress, taskStates as states, type TaskState, type TaskSummary } from './overview'

type Target = { id: string; object_key: string; state: TaskState; result_code: string; can_retry: boolean; version: number }
type Attempt = { id?: string; target_id: string; sequence: number; result_code?: string; started_at: string; finished_at?: string }
type InstallStep = {
  name: 'images' | 'database' | 'services' | 'health'
  state: 'pending' | 'running' | 'completed' | 'failed' | 'interrupted'
  attempts: number
  updated_at?: string
}
type Task = TaskSummary & {
  targets: Target[]
  attempts: Attempt[]
  installation?: { deployment_id: string; revision: number; steps: InstallStep[] }
}

const props = defineProps<{ csrf: string }>()
const emit = defineEmits<{ expired: [] }>()
const results: Record<string, string> = {
  pending: '账号待激活', exists: '账号已存在', import_failed: '账号写入失败', pending_approval: '等待设备审批',
  approved: '设备已通过审批', rejected: '设备已被拒绝', retired: '待审批设备已退役',
  lease_expired: '执行租约已过期', authorization_changed: '重试申请人的管理员权限已变更',
}
const steps = { images: '准备镜像', database: '检查数据库', services: '启动服务', health: '验证服务健康' }
const installStates = { pending: '待执行', running: '执行中', completed: '成功', failed: '失败', interrupted: '已中断' }
const base = '/v1/admin/tasks'
const retryOnly = new URLSearchParams(location.search).get('retry') === 'available'
const tasks = ref<TaskSummary[]>([])
const next = ref('')
const task = ref<Task>()
const viewedRevision = ref('')
const selected = ref<string[]>([])
const selection = computed(() => task.value?.targets.filter(target => target.can_retry && selected.value.includes(target.id)) ?? [])
const error = ref('')
const notice = ref('')
const busy = ref(false)
const detailHeading = ref<HTMLElement>()
const confirm = ref<HTMLDialogElement>()
const dialogError = ref('')

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
  const response = await api<{ tasks: TaskSummary[]; next_cursor?: string }>(base + (filters.size ? '?' + filters : ''))
  tasks.value = after ? [...tasks.value, ...response.tasks] : response.tasks
  next.value = response.next_cursor ?? ''
}

async function readDetail(id: string, revision = '') {
  const query = revision ? '?' + new URLSearchParams({ revision }) : ''
  task.value = await api<Task>(base + '/' + encodeURIComponent(id) + query)
  viewedRevision.value = revision
  selected.value = []
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

function confirmSelection() {
  dialogError.value = ''
  confirm.value?.showModal()
}

async function retry() {
  if (!task.value || !selection.value.length || busy.value) return
  const id = task.value.id
  const targets = selection.value.map(({ id, version }) => ({ id, version }))
  busy.value = true
  dialogError.value = ''
  try {
    const reply = await api<{ queued: number }>(base + '/' + id + '/retry', json({ targets }, props.csrf))
    confirm.value?.close()
    notice.value = `已提交 ${reply.queued} 项重试。`
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
      <button class="secondary" :disabled="busy" @click="refresh()">刷新</button>
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
      <button v-if="task.targets.some(target => target.can_retry)" class="primary" :disabled="!selection.length || busy" @click="confirmSelection">重试所选失败项</button>
    </div>
    <dl>
      <div><dt>任务 ID</dt><dd><code>{{ task.id }}</code></dd></div>
      <div v-if="task.device_id"><dt>设备 ID</dt><dd><code>{{ task.device_id }}</code></dd></div>
      <div v-if="task.installation"><dt>部署 ID</dt><dd><code>{{ task.installation.deployment_id }}</code></dd></div>
      <div v-if="task.installation"><dt>记录修订</dt><dd>{{ task.installation.revision }}</dd></div>
    </dl>
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
              <label v-if="target.can_retry" class="control-actions"><input v-model="selected" type="checkbox" :value="target.id" :disabled="busy"> {{ target.object_key }}</label>
              <span v-else>{{ target.object_key }}</span>
            </td>
            <td>{{ states[target.state] }}</td><td>{{ results[target.result_code] ?? '—' }}</td>
            <td>
              <div v-for="attempt in task.attempts.filter(row => row.target_id === target.id)" :key="attempt.sequence">
                <details>
                  <summary>{{ attempt.sequence ? '第 ' + attempt.sequence + ' 次重试' : '首次处理' }} · {{ attempt.result_code ? results[attempt.result_code] : '执行中' }}</summary>
                  <div>开始于 {{ formatTime(attempt.started_at) }}</div>
                  <div v-if="attempt.finished_at">完成于 {{ formatTime(attempt.finished_at) }}</div>
                  <div v-if="attempt.id"><code>{{ attempt.id }}</code></div>
                </details>
              </div>
              <span v-if="!task.attempts.some(row => row.target_id === target.id)">—</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
  <dialog ref="confirm" aria-labelledby="task-retry-heading" @cancel.prevent="!busy && confirm?.close()">
    <h2 id="task-retry-heading">确认重试</h2>
    <p>将重新调度以下 {{ selection.length }} 个失败对象。</p>
    <ul><li v-for="target in selection" :key="target.id">{{ target.object_key }}</li></ul>
    <p v-if="dialogError" class="error" role="alert">{{ dialogError }}</p>
    <div class="dialog-actions">
      <button class="secondary" :disabled="busy" @click="confirm?.close()">取消</button>
      <button class="primary" :disabled="busy" @click="retry">确认重试</button>
    </div>
  </dialog>
</template>
