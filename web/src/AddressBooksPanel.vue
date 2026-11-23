<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'

type Book = { id: string; name: string; owner: boolean; permission: number }
type Entry = {
  id: string; device_id: string; rustdesk_id: string; registration_state: string
  alias: string; tags: string[]; note: string; version: number
}
type Device = { id: string; rustdesk_id: string; group_name: string }
type Share = { id: string; kind: 'user' | 'group'; name: string; permission: number }
type Draft = { entry?: Entry; device: string; alias: string; tags: string; note: string }

const props = defineProps<{ csrf: string }>()
const emit = defineEmits<{ expired: [] }>()
const url = '/v1/address-books'
const permissions: Record<number, string> = { 1: '查看', 2: '编辑', 3: '管理' }
const states: Record<string, string> = { pending: '待审批', approved: '已批准', rejected: '已拒绝', disabled: '已禁用', retired: '已退役' }
const books = ref<Book[]>([])
const entries = ref<Entry[]>([])
const shares = ref<Share[]>([])
const devices = ref<Device[]>([])
const bookNext = ref('')
const entryNext = ref('')
const shareNext = ref('')
const deviceNext = ref('')
const book = ref<Book>()
const titleDraft = ref<{ name: string; rename: boolean }>()
const entryDraft = ref<Draft>()
const shareKind = ref<Share['kind']>('user')
const shareName = ref('')
const sharePermission = ref(1)
const showShares = ref(false)
const busy = ref(false)
const error = ref('')
const confirmation = ref<{ kind: 'entry'; value: Entry } | { kind: 'share'; value: Share }>()
const dialog = ref<HTMLDialogElement>()
const detailHeading = ref<HTMLElement>()
const titleInput = ref<HTMLInputElement>()
const connectionDialog = ref<HTMLDialogElement>()
const connectionInput = ref<HTMLInputElement>()
const connection = ref<{ alias: string; target: string }>()
const copyNotice = ref('')
let connectionTrigger: HTMLButtonElement | undefined

async function showConnection(entry: Entry, event: MouseEvent) {
  const current = book.value
  if (!current || entry.registration_state !== 'approved') return
  connectionTrigger = event.currentTarget as HTMLButtonElement
  connection.value = {
    alias: entry.alias || entry.rustdesk_id,
    target: 'drbook-' + current.id.replaceAll('-', '') + '-' + entry.id.replaceAll('-', ''),
  }
  copyNotice.value = ''
  await nextTick()
  connectionDialog.value?.showModal()
  connectionInput.value?.select()
}

async function copyConnection() {
  const selected = connection.value
  if (!selected) return
  try {
    await navigator.clipboard.writeText(selected.target)
    if (connection.value === selected) copyNotice.value = '已复制连接目标。'
  } catch {
    if (connection.value !== selected) return
    connectionInput.value?.select()
    copyNotice.value = '请复制已选中的连接目标。'
  }
}

function closeConnection() {
  connection.value = undefined
  connectionTrigger?.focus()
  connectionTrigger = undefined
}

async function run(task: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    await task()
  } catch (cause) {
    if (cause instanceof RequestError && cause.code === 'session_required') emit('expired')
    error.value = cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
    if (cause instanceof RequestError && cause.code === 'address_book_not_found') {
      clearBook()
      await listBooks().catch(() => { books.value = []; bookNext.value = '' })
    }
  } finally {
    busy.value = false
  }
}

function fromCursor(path: string, after: string) {
  return after ? path + '?after=' + encodeURIComponent(after) : path
}

function clearBook() {
  book.value = undefined
  titleDraft.value = undefined
  entryDraft.value = undefined
  entries.value = []
  entryNext.value = ''
  showShares.value = false
  shares.value = []
  shareNext.value = ''
  shareName.value = ''
}

async function listBooks(after = '') {
  const page = await api<{ books: Book[]; next?: string }>(fromCursor(url, after))
  books.value = after ? [...books.value, ...page.books] : page.books
  bookNext.value = page.next ?? ''
}

async function listEntries(id: string, after = '') {
  const page = await api<{ entries: Entry[]; next?: string }>(fromCursor(url + '/' + id + '/entries', after))
  entries.value = after ? [...entries.value, ...page.entries] : page.entries
  entryNext.value = page.next ?? ''
}

async function openBook(id: string) {
  return run(async () => {
    const result = await api<Book>(url + '/' + id)
    clearBook()
    await listEntries(id)
    book.value = result
    await nextTick()
    detailHeading.value?.focus()
  })
}

function refreshBook() {
  return book.value ? openBook(book.value.id) : run(() => listBooks())
}

async function editTitle(rename = false) {
  if (rename && !book.value) return
  titleDraft.value = { name: rename ? book.value!.name : '', rename }
  await nextTick()
  titleInput.value?.focus()
}

function saveTitle() {
  const draft = titleDraft.value
  if (!draft) return
  const editing = draft.rename ? book.value?.id : undefined
  return run(async () => {
    const saved = await api<Book>(url + (editing ? '/' + editing : ''), json({ name: draft.name }, props.csrf))
    if (!editing) {
      clearBook()
      await listEntries(saved.id)
    }
    book.value = saved
    titleDraft.value = undefined
    await listBooks()
  })
}

async function loadDevices(after = '') {
  const page = await api<{ devices: Device[]; next?: string }>(fromCursor('/v1/admin/devices', after))
  devices.value = after ? [...devices.value, ...page.devices] : page.devices
  deviceNext.value = page.next ?? ''
}

function edit(entry?: Entry) {
  return run(async () => {
    if (!entry) await loadDevices()
    entryDraft.value = { entry, device: entry?.device_id ?? '', alias: entry?.alias ?? '',
      tags: entry?.tags.join('、') ?? '', note: entry?.note ?? '' }
  })
}

function saveEntry() {
  const current = book.value
  const draft = entryDraft.value
  if (!current || !draft) return
  return run(async () => {
    const root = url + '/' + current.id + '/entries'
    const target = draft.entry
    const tags = draft.tags.split(/[、,，]/).map(value => value.trim()).filter(Boolean)
    const data = { alias: draft.alias, tags: [...new Set(tags)], note: draft.note,
      ...(target ? { version: target.version } : { device_id: draft.device }) }
    await api(root + (target ? '/' + target.id : ''), json(data, props.csrf))
    entryDraft.value = undefined
    await listEntries(current.id)
  })
}

async function listShares(id: string, after = '') {
  const page = await api<{ shares: Share[]; next?: string }>(fromCursor(url + '/' + id + '/shares', after))
  shares.value = after ? [...shares.value, ...page.shares] : page.shares
  shareNext.value = page.next ?? ''
}

function manageShares() {
  const current = book.value
  if (!current) return
  if (showShares.value) { showShares.value = false; return }
  return run(async () => {
    await listShares(current.id)
    showShares.value = true
  })
}

function shareBook() {
  const current = book.value
  if (!current) return
  return run(async () => {
    await api(url + '/' + current.id + '/shares', json({
      kind: shareKind.value, name: shareName.value, permission: sharePermission.value,
    }, props.csrf))
    shareName.value = ''
    await listShares(current.id)
  })
}

async function confirm(action: NonNullable<typeof confirmation.value>) {
  confirmation.value = action
  error.value = ''
  await nextTick()
  dialog.value?.showModal()
}

function applyConfirmation() {
  const action = confirmation.value
  const current = book.value
  if (!action || !current) return
  return run(async () => {
    const root = url + '/' + current.id
    if (action.kind === 'entry') {
      await api(root + '/entries/' + action.value.id + '/remove', json({ version: action.value.version }, props.csrf))
      if (entryDraft.value?.entry?.id === action.value.id) entryDraft.value = undefined
      await listEntries(current.id)
    } else {
      await api(root + '/shares/' + action.value.id + '/revoke', json({}, props.csrf))
      await listShares(current.id)
    }
    dialog.value?.close()
  })
}

onMounted(() => run(() => listBooks()))
</script>

<template>
  <section class="users-surface" aria-labelledby="books-title">
    <div class="section-heading">
      <h2 id="books-title">通讯录列表</h2>
      <div class="user-tools">
        <button class="secondary" :disabled="busy" type="button" @click="refreshBook">刷新</button>
        <button class="primary" :disabled="busy" type="button" @click="editTitle()">新建通讯录</button>
      </div>
    </div>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <form v-if="titleDraft" class="group-form" @submit.prevent="saveTitle">
      <div class="field"><label for="book-name">通讯录名称</label><input id="book-name" ref="titleInput" v-model="titleDraft.name" maxlength="80" required></div>
      <button class="primary" type="submit" :disabled="busy">{{ titleDraft.rename ? '保存名称' : '创建通讯录' }}</button>
      <button class="secondary" type="button" @click="titleDraft = undefined">取消</button>
    </form>
    <ul v-if="books.length" class="member-list">
      <li v-for="item in books" :key="item.id">
        <div><strong>{{ item.name }}</strong><small>{{ item.owner ? '我的通讯录' : '共享通讯录' }} · {{ permissions[item.permission] }}</small></div>
        <button class="secondary" type="button" :aria-label="'打开 ' + item.name" :disabled="busy" @click="openBook(item.id)">打开</button>
      </li>
    </ul>
    <p v-else class="muted" role="status">{{ busy ? '正在加载通讯录…' : '暂无通讯录。' }}</p>
    <button v-if="bookNext" type="button" class="secondary load-more" :disabled="busy" @click="run(() => listBooks(bookNext))">加载更多通讯录</button>
  </section>

  <section v-if="book" class="users-surface address-book-detail" aria-labelledby="book-detail-title">
    <div class="section-heading">
      <h2 id="book-detail-title" ref="detailHeading" tabindex="-1">{{ book.name }}</h2>
      <div class="user-tools">
        <button v-if="book.permission >= 2" class="primary" type="button" :disabled="busy" @click="edit()">添加设备条目</button>
        <button v-if="book.permission === 3" class="secondary" type="button" :disabled="busy" @click="editTitle(true)">修改名称</button>
        <button v-if="book.permission === 3" class="secondary" type="button" :disabled="busy" :aria-expanded="showShares" @click="manageShares">共享设置</button>
      </div>
    </div>

    <form v-if="entryDraft" class="link-form" @submit.prevent="saveEntry">
      <div v-if="!entryDraft.entry" class="field">
        <label for="book-device">设备</label>
        <select id="book-device" v-model="entryDraft.device" required>
          <option value="">选择已授权设备</option>
          <option v-for="device in devices" :key="device.id" :value="device.id">{{ device.rustdesk_id }} · {{ device.group_name }}</option>
        </select>
        <button v-if="deviceNext" type="button" class="secondary" :disabled="busy" @click="run(() => loadDevices(deviceNext))">加载更多设备</button>
      </div>
      <div class="field"><label for="book-alias">别名</label><input id="book-alias" v-model="entryDraft.alias" maxlength="80"></div>
      <div class="field"><label for="book-tags">标签</label><input id="book-tags" v-model="entryDraft.tags" maxlength="395" placeholder="用顿号或逗号分隔"></div>
      <div class="field"><label for="book-note">备注</label><textarea id="book-note" v-model="entryDraft.note" rows="3" maxlength="500"></textarea></div>
      <div class="user-tools">
        <button class="primary" type="submit" :disabled="busy">保存设备条目</button>
        <button class="secondary" type="button" @click="entryDraft = undefined">取消</button>
      </div>
    </form>

    <div v-if="entries.length" class="table-wrap">
      <table>
        <thead><tr><th scope="col">设备与别名</th><th scope="col">登记状态</th><th scope="col">标签</th><th scope="col">备注</th><th scope="col">操作</th></tr></thead>
        <tbody>
          <tr v-for="entry in entries" :key="entry.id">
            <th scope="row">{{ entry.alias || entry.rustdesk_id }}<small v-if="entry.alias">{{ entry.rustdesk_id }}</small></th>
            <td>{{ states[entry.registration_state] }}</td>
            <td>{{ entry.tags.join('、') }}</td>
            <td class="address-book-note">{{ entry.note }}</td>
            <td class="row-actions">
              <button v-if="entry.registration_state === 'approved'" class="secondary" type="button" :disabled="busy" @click="showConnection(entry, $event)">准备连接</button>
              <template v-if="book.permission >= 2">
                <button class="secondary" type="button" :disabled="busy" @click="edit(entry)">编辑</button>
                <button class="secondary" type="button" :disabled="busy" @click="confirm({ kind: 'entry', value: entry })">移除</button>
              </template>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-else class="muted" role="status">没有可查看的设备条目。</p>
    <button v-if="entryNext" type="button" class="secondary load-more" :disabled="busy" @click="run(() => listEntries(book!.id, entryNext))">加载更多条目</button>

    <section v-if="showShares && book.permission === 3" class="address-book-detail" aria-labelledby="shares-title">
      <h3 id="shares-title">共享设置</h3>
      <form class="link-form" @submit.prevent="shareBook">
        <div class="field"><label for="book-share-kind">共享给</label><select id="book-share-kind" v-model="shareKind"><option value="user">用户</option><option value="group">用户组</option></select></div>
        <div class="field"><label for="book-share-name">{{ shareKind === 'user' ? '共享账号' : '共享用户组名称' }}</label><input id="book-share-name" v-model="shareName" maxlength="80" required></div>
        <div class="field"><label for="book-share-permission">共享权限</label><select id="book-share-permission" v-model.number="sharePermission"><option :value="1">查看</option><option :value="2">编辑</option><option :value="3">管理</option></select></div>
        <button class="primary" type="submit" :disabled="busy">保存共享</button>
      </form>
      <ul v-if="shares.length" class="member-list">
        <li v-for="share in shares" :key="share.id">
          <div><strong>{{ share.name }}</strong><small>{{ share.kind === 'user' ? '用户' : '用户组' }} · {{ permissions[share.permission] }}</small></div>
          <button class="secondary" type="button" :disabled="busy" @click="confirm({ kind: 'share', value: share })">撤销共享</button>
        </li>
      </ul>
      <p v-else class="muted">还没有共享记录。</p>
      <button v-if="shareNext" type="button" class="secondary load-more" :disabled="busy" @click="run(() => listShares(book!.id, shareNext))">加载更多共享</button>
    </section>
  </section>

  <dialog ref="connectionDialog" aria-labelledby="book-connect-title" @close="closeConnection">
    <template v-if="connection">
      <h2 id="book-connect-title">从通讯录连接</h2>
      <p>{{ connection.alias }}</p>
      <p class="muted dialog-copy">将连接目标粘贴到端域 Windows 客户端的远程 ID 输入框，再发起连接。中心会检查当前账号及条目的权限。</p>
      <div class="field">
        <label for="book-connect-target">连接目标</label>
        <input id="book-connect-target" ref="connectionInput" :value="connection.target" spellcheck="false" readonly>
      </div>
      <p v-if="copyNotice" role="status">{{ copyNotice }}</p>
      <div class="dialog-actions">
        <button class="secondary" type="button" @click="connectionDialog?.close()">关闭</button>
        <button class="primary" type="button" @click="copyConnection">复制连接目标</button>
      </div>
    </template>
  </dialog>

  <dialog ref="dialog" aria-labelledby="book-confirm-title">
    <form v-if="confirmation" @submit.prevent="applyConfirmation">
      <h2 id="book-confirm-title">{{ confirmation.kind === 'entry' ? '移除设备条目' : '撤销共享' }}</h2>
      <p class="muted dialog-copy">
        {{ confirmation.kind === 'entry' ? (confirmation.value.alias || confirmation.value.rustdesk_id) : confirmation.value.name }}
      </p>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <div class="dialog-actions">
        <button class="secondary" type="button" @click="dialog?.close()">取消</button>
        <button class="primary" type="submit" :disabled="busy">确认</button>
      </div>
    </form>
  </dialog>
</template>
