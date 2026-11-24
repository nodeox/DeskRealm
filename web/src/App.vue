<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { api, json, RequestError } from './api'
import UsersPanel from './UsersPanel.vue'
import SecurityPanel from './SecurityPanel.vue'
import GroupsPanel from './GroupsPanel.vue'
import TokensPanel from './TokensPanel.vue'
import DevicesPanel from './DevicesPanel.vue'
import AddressBooksPanel from './AddressBooksPanel.vue'
import AuditPanel from './AuditPanel.vue'
import TasksPanel from './TasksPanel.vue'
import PoliciesPanel from './PoliciesPanel.vue'
import SessionsPanel from './SessionsPanel.vue'
import OverviewPanel from './OverviewPanel.vue'
import RelaysPanel from './RelaysPanel.vue'
import type { Overview } from './overview'

type WorkspacePage = 'overview' | 'users' | 'groups' | 'devices' | 'policies' | 'sessions' | 'address-books' | 'audit' | 'tasks' | 'relays' | 'api-tokens' | 'security'
type Screen = WorkspacePage | 'loading' | 'initialize' | 'login' | 'accept-invitation' | 'recover' | 'unavailable'
type Session = { username: string; csrf_token: string }
type Capability = 'can_manage' | 'can_read_users' | 'can_read_devices' | 'can_read_audit' | 'can_read_tasks'

const navigation: { page: WorkspacePage; title: string; capability?: Capability }[] = [
  { page: 'overview', title: '概览' },
  { page: 'devices', title: '设备管理', capability: 'can_read_devices' },
  { page: 'address-books', title: '通讯录' },
  { page: 'policies', title: '访问策略', capability: 'can_manage' },
  { page: 'sessions', title: '会话管理' },
  { page: 'relays', title: '区域中继', capability: 'can_manage' },
  { page: 'audit', title: '审计事件', capability: 'can_read_audit' },
  { page: 'tasks', title: '任务中心', capability: 'can_read_tasks' },
  { page: 'users', title: '用户与权限', capability: 'can_read_users' },
  { page: 'groups', title: '用户组与委派', capability: 'can_manage' },
  { page: 'api-tokens', title: 'API 令牌', capability: 'can_manage' },
  { page: 'security', title: '账号安全' },
]

const props = defineProps<{ ticket: string }>()
const ticket = ref(props.ticket)
const screen = ref<Screen>('loading')
const heading = ref<HTMLElement>()
const organization = ref('')
const username = ref('')
const password = ref('')
const error = ref('')
const submitting = ref(false)
const session = ref<Session>()
const overview = ref<Overview>()
const oidcAvailable = ref(false)
const verification = ref(false)
const code = ref('')
const refreshing = ref(false)
const workspaceTitle = computed(() => navigation.find(item => item.page === screen.value)?.title)
const availablePages = computed(() => navigation.filter(item => !item.capability || overview.value?.[item.capability]))
const accountLink = computed(() => screen.value === 'accept-invitation' || screen.value === 'recover')
const needsTicket = computed(() => accountLink.value || screen.value === 'initialize')
const formTitle = computed(() => {
  if (screen.value === 'initialize') return '首次管理员初始化'
  if (screen.value === 'accept-invitation') return '接受组织邀请'
  if (screen.value === 'recover') return '恢复账号'
  if (verification.value) return '两步验证'
  return '登录端域控制台'
})

watch(() => props.ticket, (value) => {
  if (value) {
    ticket.value = value
    error.value = ''
  }
})

async function show(page: Screen) {
  screen.value = page
  const path = page === 'loading' || page === 'unavailable' ? '' : '/' + page
  if (path && path !== location.pathname) history.replaceState(null, '', path)
  await nextTick()
  heading.value?.focus()
}

async function loadSignedIn(allowAnonymous: boolean) {
  try {
    session.value = await api<Session>('/v1/auth/session')
  } catch (cause) {
    if (allowAnonymous && cause instanceof RequestError && cause.status === 401) {
      await show('login')
      return false
    }
    throw cause
  }
  overview.value = await api<Overview>('/v1/overview')
  const requested = navigation.find(item => location.pathname === '/' + item.page)
  const allowed = requested && availablePages.value.includes(requested)
  if (requested && !allowed) error.value = '当前账号无权访问「' + requested.title + '」。'
  await show(allowed ? requested.page : 'overview')
  return true
}

async function load() {
  error.value = ''
  screen.value = 'loading'
  try {
    const { initialized } = await api<{ initialized: boolean }>('/v1/bootstrap/status')
    if (!initialized) await show('initialize')
    else if (location.pathname === '/accept-invitation') await show('accept-invitation')
    else if (location.pathname === '/recover') await show('recover')
    else {
      const params = new URLSearchParams(location.search)
      verification.value = params.get('verification') === 'totp'
      if (params.has('oidc') || verification.value) history.replaceState(null, '', '/login')
      if (verification.value) await show('login')
      if (verification.value || !(await loadSignedIn(true))) {
        const options = await api<{ oidc: boolean }>('/v1/auth/options').catch(() => ({ oidc: false }))
        oidcAvailable.value = options.oidc
        if (params.get('oidc') === 'failed') error.value = '外部身份登录失败。请联系管理员核对账号绑定。'
      }
    }
  } catch {
    await show('unavailable')
  }
}

async function submit() {
  if (submitting.value) return
  submitting.value = true
  error.value = ''
  try {
    if (screen.value === 'initialize') {
      await api('/v1/bootstrap', json({
        organization: organization.value,
        username: username.value,
        password: password.value,
        ticket: ticket.value,
      }))
      ticket.value = ''
      await show('login')
    } else if (accountLink.value) {
      await api('/v1/auth/' + screen.value, json({ ticket: ticket.value, password: password.value }))
      ticket.value = ''
      await show('login')
    } else if (verification.value) {
      await api('/v1/auth/totp/verify', json({ code: code.value }))
      verification.value = false
      await loadSignedIn(false)
    } else {
      const response = await api<{ next: 'totp' } | undefined>('/v1/auth/login', json({ username: username.value, password: password.value }))
      if (response?.next === 'totp') {
        verification.value = true
        await nextTick()
        document.getElementById('login-code')?.focus()
      } else await loadSignedIn(false)
    }
  } catch (cause) {
    if (cause instanceof RequestError && cause.code === 'already_initialized') await show('login')
    error.value = cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
  } finally {
    password.value = ''
    code.value = ''
    submitting.value = false
  }
}

async function refreshOverview() {
  if (refreshing.value) return
  refreshing.value = true
  error.value = ''
  try {
    overview.value = await api<Overview>('/v1/overview')
  } catch (cause) {
    if (cause instanceof RequestError && cause.status === 401) await load()
    else error.value = '无法刷新概览。当前显示上次读取的数据。'
  } finally {
    refreshing.value = false
  }
}

async function logout() {
  if (!session.value || submitting.value) return
  submitting.value = true
  error.value = ''
  try {
    await api('/v1/auth/logout', { method: 'POST', headers: { 'X-CSRF-Token': session.value.csrf_token } })
    session.value = undefined
    overview.value = undefined
    await load()
  } catch (cause) {
    if (cause instanceof RequestError && cause.status === 401) {
      session.value = undefined
      await show('login')
    } else {
      error.value = '退出请求失败，请重试。'
    }
  } finally {
    submitting.value = false
  }
}

onMounted(load)
</script>

<template>
  <div v-if="workspaceTitle && session && overview" class="workspace">
    <aside class="sidebar" aria-label="工作台导航">
      <div class="brand"><span class="brand-icon" aria-hidden="true"></span><span><strong>端域</strong><small>DeskRealm</small></span></div>
      <p class="nav-heading">工作台</p>
      <nav class="nav-links" aria-label="工作台导航">
        <a v-for="item in availablePages" :key="item.page" class="nav-link" :href="'/' + item.page"
          :aria-current="screen === item.page ? 'page' : undefined">{{ item.title }}</a>
      </nav>
    </aside>
    <header class="topbar">
      <span class="breadcrumb">工作台 / {{ workspaceTitle }}</span>
      <div class="account">
        <div><strong>{{ session.username }}</strong><small>{{ overview.can_manage ? '平台管理员' : '成员' }}</small></div>
        <button class="secondary" :disabled="submitting" @click="logout">退出登录</button>
      </div>
    </header>
    <main class="content">
      <h1 ref="heading" tabindex="-1">{{ workspaceTitle }}</h1>
      <p class="muted">{{ overview.organization }}</p>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <UsersPanel v-if="screen === 'users'" :csrf="session.csrf_token" :admin="overview.can_manage" @expired="load" />
      <DevicesPanel v-else-if="screen === 'devices'" :csrf="session.csrf_token" :admin="overview.can_manage" @expired="load" />
      <AddressBooksPanel v-else-if="screen === 'address-books'" :csrf="session.csrf_token" @expired="load" />
      <PoliciesPanel v-else-if="screen === 'policies'" :csrf="session.csrf_token" @expired="load" />
      <SessionsPanel v-else-if="screen === 'sessions'" :csrf="session.csrf_token" :can-audit="overview.can_read_audit" :can-read-devices="overview.can_read_devices" @expired="load" />
      <AuditPanel v-else-if="screen === 'audit'" :csrf="session.csrf_token" :admin="overview.can_manage" @expired="load" />
      <TasksPanel v-else-if="screen === 'tasks'" :csrf="session.csrf_token" :admin="overview.can_manage" @expired="load" />
      <RelaysPanel v-else-if="screen === 'relays'" :csrf="session.csrf_token" @expired="load" />
      <GroupsPanel v-else-if="screen === 'groups'" :csrf="session.csrf_token" @expired="load" />
      <TokensPanel v-else-if="screen === 'api-tokens'" :csrf="session.csrf_token" @expired="load" />
      <SecurityPanel v-else-if="screen === 'security'" :csrf="session.csrf_token" @expired="load" />
      <OverviewPanel v-else :overview="overview" :refreshing="refreshing" @refresh="refreshOverview" />
    </main>
  </div>

  <main v-else class="auth-layout">
    <div class="auth-wrap">
      <div class="brand auth-brand"><span class="brand-icon" aria-hidden="true"></span><span><strong>端域</strong><small>DeskRealm</small></span></div>
      <section class="auth-panel">
        <template v-if="screen === 'loading'">
          <h1 ref="heading" tabindex="-1">连接中心</h1>
          <p class="muted" role="status">正在获取中心状态…</p>
        </template>
        <template v-else-if="screen === 'unavailable'">
          <h1 ref="heading" tabindex="-1">中心暂不可用</h1>
          <p class="muted">请检查中心地址和连接后重试。</p>
          <button class="primary" @click="load">重试</button>
        </template>
        <template v-else>
          <h1 ref="heading" tabindex="-1">{{ formTitle }}</h1>
          <p v-if="screen === 'initialize' && !ticket" class="muted">
            请在中心主机运行 <code>deskrealm bootstrap-ticket</code>，用生成的链接打开初始化页面。
          </p>
          <p v-else-if="accountLink && !ticket" class="muted">
            请使用管理员发放的链接。<a href="/login">前往登录</a>
          </p>
          <form v-else @submit.prevent="submit">
            <p v-if="screen === 'initialize'" class="muted">设置组织名称和首个管理员账号。</p>
            <p v-else-if="accountLink" class="muted">设置新密码后登录端域控制台。</p>
            <div v-if="screen === 'initialize'" class="field">
              <label for="organization">组织名称</label>
              <input id="organization" v-model="organization" autocomplete="organization" maxlength="80" required>
            </div>
            <div v-if="!accountLink && !verification" class="field">
              <label for="username">账号</label>
              <input id="username" v-model="username" autocomplete="username" maxlength="40" required>
            </div>
            <div v-if="verification" class="field">
              <label for="login-code">验证码</label>
              <input id="login-code" v-model="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6" required>
            </div>
            <div v-else class="field">
              <label for="password">{{ accountLink ? '新密码' : '密码' }}</label>
              <input id="password" v-model="password" type="password"
                :autocomplete="needsTicket ? 'new-password' : 'current-password'"
                :minlength="needsTicket ? 12 : undefined" maxlength="256" required>
              <span v-if="needsTicket" class="hint">至少 12 个字符</span>
            </div>
            <p v-if="error" class="error" role="alert">{{ error }}</p>
            <button class="primary full" type="submit" :disabled="submitting">
              {{ submitting ? '正在提交…' : screen === 'initialize' ? '初始化中心' : accountLink ? '设置密码' : verification ? '验证登录' : '登录' }}
            </button>
            <button v-if="verification" class="secondary full" type="button" @click="verification = false; error = ''; code = ''">返回登录</button>
          </form>
          <a v-if="screen === 'login' && oidcAvailable && !verification" class="external-login" href="/v1/auth/oidc/start">使用组织身份登录</a>
        </template>
      </section>
    </div>
  </main>
</template>
