<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'
import { localTime } from './managed'

type Arch = 'x64' | 'arm64'
type Release = {
  id: string
  version: string
  published_at: string
  revoked_at?: string
  packages: Record<Arch, { sha256: string; bytes: number }>
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

function describe(cause: unknown) {
  if (cause instanceof RequestError && cause.status === 401) emit('expired')
  return cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
}

async function load(after = '') {
  const path = after ? base + '?' + new URLSearchParams({ before: after }) : base
  const page = await api<{ releases: Release[]; next_cursor?: string }>(path)
  releases.value = after ? [...releases.value, ...page.releases] : page.releases
  cursor.value = page.next_cursor ?? ''
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
      <h2 id="windows-releases-heading">开发包下载</h2>
      <button type="button" class="secondary" :disabled="busy" @click="refresh()">刷新</button>
    </div>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <div v-if="releases.length" class="table-wrap">
      <table>
        <thead><tr><th scope="col">版本</th><th scope="col">发布时间</th><th scope="col">工件</th><th v-if="admin" scope="col">操作</th></tr></thead>
        <tbody>
          <tr v-for="release in releases" :key="release.id">
            <th scope="row">{{ release.version }}</th>
            <td>{{ localTime(release.published_at) }}</td>
            <td v-if="release.revoked_at">已撤回 · {{ localTime(release.revoked_at) }}</td>
            <td v-else>
              <div v-for="arch in (['x64', 'arm64'] as const)" :key="arch">
                <a :href="base + '/' + release.id + '/' + arch">下载 {{ arch === 'arm64' ? 'ARM64' : arch }}</a>
                <details><summary>SHA-256</summary><code>{{ release.packages[arch].sha256 }}</code></details>
              </div>
            </td>
            <td v-if="admin">
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
</template>

<style scoped>
td code { overflow-wrap: anywhere; }
</style>
