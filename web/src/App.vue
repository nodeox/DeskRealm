<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { api, json, RequestError } from './api'
import UsersPanel from './UsersPanel.vue'
import SecurityPanel from './SecurityPanel.vue'
import GroupsPanel from './GroupsPanel.vue'
import TokensPanel from './TokensPanel.vue'
import DevicesPanel from './DevicesPanel.vue'

type Screen = 'loading' | 'initialize' | 'login' | 'overview' | 'users' | 'groups' | 'devices' | 'api-tokens' | 'security' | 'accept-invitation' | 'recover' | 'unavailable'
type Session = {
  username: string
  organization: string
  role: string
  csrf_token: string
}
type Overview = {
  organization: string
  can_manage: boolean
  can_read_users: boolean
  can_read_devices: boolean
}

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
const workspaceTitles: Partial<Record<Screen, string>> = {
  overview: '概览', users: '用户与权限', groups: '用户组与委派', devices: '设备管理', 'api-tokens': 'API 令牌', security: '账号安全',
}
const workspaceTitle = computed(() => workspaceTitles[screen.value])
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
  const page = location.pathname === '/security' ? 'security' :
    overview.value.can_read_devices && location.pathname === '/devices' ? 'devices' :
    overview.value.can_manage && location.pathname === '/groups' ? 'groups' :
    overview.value.can_manage && location.pathname === '/api-tokens' ? 'api-tokens' :
    overview.value.can_read_users && location.pathname === '/users' ? 'users' : 'overview'
  await show(page)
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
      <nav class="nav-links">
        <a class="nav-link" href="/overview" :aria-current="screen === 'overview' ? 'page' : undefined">概览</a>
        <a v-if="overview.can_read_devices" class="nav-link" href="/devices" :aria-current="screen === 'devices' ? 'page' : undefined">设备管理</a>
        <a v-if="overview.can_read_users" class="nav-link" href="/users" :aria-current="screen === 'users' ? 'page' : undefined">用户与权限</a>
        <a v-if="overview.can_manage" class="nav-link" href="/groups" :aria-current="screen === 'groups' ? 'page' : undefined">用户组与委派</a>
        <a v-if="overview.can_manage" class="nav-link" href="/api-tokens" :aria-current="screen === 'api-tokens' ? 'page' : undefined">API 令牌</a>
        <a class="nav-link" href="/security" :aria-current="screen === 'security' ? 'page' : undefined">账号安全</a>
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
      <GroupsPanel v-else-if="screen === 'groups'" :csrf="session.csrf_token" @expired="load" />
      <TokensPanel v-else-if="screen === 'api-tokens'" :csrf="session.csrf_token" @expired="load" />
      <SecurityPanel v-else-if="screen === 'security'" :csrf="session.csrf_token" @expired="load" />
      <section v-else class="panel" aria-labelledby="organization-title">
        <h2 id="organization-title">当前组织</h2>
        <dl>
          <div><dt>组织</dt><dd>{{ overview.organization }}</dd></div>
          <div><dt>账号</dt><dd>{{ session.username }}</dd></div>
          <div><dt>管理角色</dt><dd>{{ overview.can_manage ? '平台管理员' : '成员' }}</dd></div>
        </dl>
      </section>
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
