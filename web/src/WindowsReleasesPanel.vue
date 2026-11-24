<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'
import { localTime } from './managed'

type Arch = 'x64' | 'arm64'
type Artifact = { sha256: string; bytes: number }
type Release = {
  id: string
  version: string
  published_at: string
  revoked_at?: string
  packages: Record<Arch, Artifact>
  rollout_only?: boolean
  installer_publication?: {
    installers: Record<Arch, Artifact>
    signer_sha256: string
    evidence_sha256: string
  }
}
const props = defineProps<{ csrf: string; admin: boolean }>()
const emit = defineEmits<{ expired: [] }>()
const base = '/v1/windows-releases'
const releases = ref<Release[]>([])
const cursor = ref('')
const busy = ref(false)
const error = ref('')
const actionError = ref('')
const selected = ref<Release>()
const dialog = ref<HTMLDialogElement>()
const installerDialog = ref<HTMLDialogElement>()
const rolloutDialog = ref<HTMLDialogElement>()
const installerPublishingEnabled = ref(false)
const evidenceSHA = ref('')
const rolloutGroup = ref('')
const rollbackRelease = ref('')
const batchDevices = ref('')
const rolloutRequest = ref('')
const rolloutTask = ref('')

function describe(cause: unknown) {
  if (cause instanceof RequestError && cause.status === 401) emit('expired')
  return cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
}

async function load(after = '') {
  const path = after ? base + '?' + new URLSearchParams({ before: after }) : base
  const page = await api<{ releases: Release[]; next_cursor?: string; can_publish_installers?: boolean }>(path)
  releases.value = after ? [...releases.value, ...page.releases] : page.releases
  cursor.value = page.next_cursor ?? ''
  installerPublishingEnabled.value = !!page.can_publish_installers
}

async function refresh(after = '') {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    await load(after)
  } catch (cause) {
    releases.value = []
    cursor.value = ''
    error.value = describe(cause)
  } finally {
    busy.value = false
  }
}

function askRevoke(release: Release) {
  selected.value = release
  actionError.value = ''
  dialog.value?.showModal()
}

function askPublishInstallers(release: Release) {
  selected.value = release
  actionError.value = ''
  evidenceSHA.value = ''
  installerDialog.value?.showModal()
}

function askRollout(release: Release) {
  selected.value = release
  actionError.value = ''
  rolloutGroup.value = ''
  rollbackRelease.value = ''
  batchDevices.value = ''
  rolloutRequest.value = crypto.randomUUID()
  rolloutDialog.value?.showModal()
}

async function createRollout() {
  const release = selected.value
  if (!release || busy.value) return
  const batches = batchDevices.value.trim().split(/\n/).map(line => line.trim().split(/[\s,]+/))
  const deviceID = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/
  if (batches.length > 20 || batches.some(batch => batch.length > 50 || batch.some(id => !deviceID.test(id)))) {
    actionError.value = '每行填写一批有效设备 UUID，用空格或逗号分隔。'
    return
  }
  busy.value = true
  actionError.value = ''
  try {
    const result = await api<{ task_id: string }>('/v1/admin/windows-releases/' + release.id + '/rollouts',
      json({ request_id: rolloutRequest.value, group_id: rolloutGroup.value.trim(), rollback_release_id: rollbackRelease.value.trim(), batches }, props.csrf))
    rolloutTask.value = result.task_id
    rolloutDialog.value?.close()
  } catch (cause) {
    actionError.value = describe(cause)
  } finally {
    busy.value = false
  }
}

async function publishInstallers() {
  const release = selected.value
  if (!release || busy.value) return
  busy.value = true
  actionError.value = ''
  try {
    const updated = await api<Release>('/v1/admin/windows-releases/' + release.id + '/installers',
      json({ evidence_sha256: evidenceSHA.value.trim() }, props.csrf))
    releases.value = releases.value.map(row => row.id === updated.id ? updated : row)
    installerDialog.value?.close()
  } catch (cause) {
    actionError.value = describe(cause)
  } finally {
    busy.value = false
  }
}

async function revoke() {
  if (!selected.value || busy.value) return
  busy.value = true
  actionError.value = ''
  try {
    await api('/v1/admin/windows-releases/' + selected.value.id + '/revoke', json({}, props.csrf))
    // Remove the stale download link before refreshing the list.
    releases.value = releases.value.filter(row => row.id !== selected.value?.id)
    dialog.value?.close()
    await load()
  } catch (cause) {
    if (dialog.value?.open) actionError.value = describe(cause)
    else error.value = describe(cause)
  } finally {
    busy.value = false
  }
}

onMounted(() => refresh())
</script>

<template>
  <section class="users-surface" aria-labelledby="windows-releases-heading" :aria-busy="busy">
    <div class="section-heading">
      <h2 id="windows-releases-heading">Windows 下载</h2>
      <button type="button" class="secondary" :disabled="busy" @click="refresh()">刷新</button>
    </div>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <p v-if="rolloutTask" role="status">发布任务已创建。<a :href="'/tasks?task=' + rolloutTask">查看发布批次</a></p>
    <div v-if="releases.length" class="table-wrap">
      <table>
        <thead><tr><th scope="col">版本</th><th scope="col">发布时间</th><th scope="col">工件</th><th v-if="admin" scope="col">操作</th></tr></thead>
        <tbody>
          <tr v-for="release in releases" :key="release.id">
            <th scope="row">{{ release.version }}</th>
            <td>{{ localTime(release.published_at) }}</td>
            <td v-if="release.revoked_at">已撤回 · {{ localTime(release.revoked_at) }}</td>
            <td v-else-if="release.rollout_only">仅通过分批任务分发</td>
            <td v-else>
              <div v-for="arch in (['x64', 'arm64'] as const)" :key="arch">
                <a :href="base + '/' + release.id + '/' + arch">下载 {{ arch === 'arm64' ? 'ARM64' : arch }}</a>
                <details><summary>SHA-256</summary><code>{{ release.packages[arch].sha256 }}</code></details>
              </div>
              <template v-if="release.installer_publication">
                <div v-for="arch in (['x64', 'arm64'] as const)" :key="'installer-' + arch">
                  <a :href="base + '/' + release.id + '/installers/' + arch">安装器 {{ arch === 'arm64' ? 'ARM64' : arch }}</a>
                  <details><summary>安装器 SHA-256</summary><code>{{ release.installer_publication.installers[arch].sha256 }}</code></details>
                </div>
                <details><summary>开发签名证书指纹</summary><code>{{ release.installer_publication.signer_sha256 }}</code></details>
              </template>
            </td>
            <td v-if="admin">
              <button v-if="installerPublishingEnabled && !release.revoked_at && !release.installer_publication"
                type="button" class="secondary" :disabled="busy" @click="askPublishInstallers(release)">发布签名安装器</button>
              <button v-if="!release.revoked_at && release.rollout_only && release.installer_publication"
                type="button" class="primary" :disabled="busy" @click="askRollout(release)">创建分批发布</button>
              <button v-if="!release.revoked_at" type="button" class="secondary" :disabled="busy"
                @click="askRevoke(release)">撤回发布</button>
              <span v-else>—</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else-if="!busy && !error" class="muted">暂无可下载的开发包。</p>
    <button v-if="cursor" type="button" class="secondary load-more" :disabled="busy" @click="refresh(cursor)">加载更多</button>
  </section>
  <dialog ref="dialog" aria-labelledby="withdraw-windows-heading" @cancel.prevent="!busy && dialog?.close()">
    <form @submit.prevent="revoke">
      <h2 id="withdraw-windows-heading">撤回发布</h2>
      <p>撤回 {{ selected?.version }} 后，目标设备组将无法再发起下载。</p>
      <p v-if="actionError" class="error" role="alert">{{ actionError }}</p>
      <div class="dialog-actions">
        <button type="button" class="secondary" :disabled="busy" @click="dialog?.close()">取消</button>
        <button type="submit" class="primary" :disabled="busy">确认撤回</button>
      </div>
    </form>
  </dialog>
  <dialog ref="rolloutDialog" aria-labelledby="rollout-heading" @cancel.prevent="!busy && rolloutDialog?.close()">
    <form @submit.prevent="createRollout">
      <h2 id="rollout-heading">分批发布 {{ selected?.version }}</h2>
      <div class="field"><label for="rollout-group">已批准的设备组 UUID</label>
        <input id="rollout-group" v-model.trim="rolloutGroup" required pattern="[0-9a-f-]{36}"></div>
      <div class="field"><label for="rollout-devices">设备 UUID，每行一批</label>
        <textarea id="rollout-devices" v-model="batchDevices" required rows="5" maxlength="6000" spellcheck="false"></textarea></div>
      <div class="field"><label for="rollout-prior">原签名发布 UUID（可选）</label>
        <input id="rollout-prior" v-model.trim="rollbackRelease" pattern="[0-9a-f-]{36}"></div>
      <p v-if="actionError" class="error" role="alert">{{ actionError }}</p>
      <div class="dialog-actions">
        <button type="button" class="secondary" :disabled="busy" @click="rolloutDialog?.close()">取消</button>
        <button type="submit" class="primary" :disabled="busy">放行第一批</button>
      </div>
    </form>
  </dialog>
  <dialog ref="installerDialog" aria-labelledby="publish-installers-heading" @cancel.prevent="!busy && installerDialog?.close()">
    <form @submit.prevent="publishInstallers">
      <h2 id="publish-installers-heading">发布 {{ selected?.version }} 安装器</h2>
      <p>已获准下载此版本的设备组可以下载签名安装器。</p>
      <div class="field">
        <label for="installer-evidence-sha">安装器验签证据 SHA-256</label>
        <input id="installer-evidence-sha" v-model.trim="evidenceSHA" required maxlength="64" pattern="[a-f0-9]{64}">
      </div>
      <p v-if="actionError" class="error" role="alert">{{ actionError }}</p>
      <div class="dialog-actions">
        <button type="button" class="secondary" :disabled="busy" @click="installerDialog?.close()">取消</button>
        <button type="submit" class="primary" :disabled="busy">确认发布安装器</button>
      </div>
    </form>
  </dialog>
</template>

<style scoped>
td code { overflow-wrap: anywhere; }
</style>
