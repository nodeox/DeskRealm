<script setup lang="ts">
import { nextTick, onMounted, ref, watch } from 'vue'

type Screen = 'loading' | 'initialize' | 'login' | 'overview' | 'unavailable'
type Session = {
  username: string
  organization: string
  role: string
  csrf_token: string
}
type Overview = {
  organization: string
  can_manage: boolean
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

watch(() => props.ticket, (value) => {
  if (value) {
    ticket.value = value
    error.value = ''
  }
})

const messages: Record<string, string> = {
  already_initialized: '中心已完成初始化，请登录。',
  invalid_or_expired_ticket: '初始化链接无效或已过期。请在中心主机重新生成链接。',
  invalid_organization: '请输入 1 到 80 个字符的组织名称。',
  invalid_username: '账号须以英文字母开头，长度为 3 到 40 位，可包含数字、点、下划线和连字符。',
  invalid_password: '密码至少需要 12 个字符。',
  invalid_credentials: '账号或密码有误，请重新输入。',
  try_again_later: '请求过于频繁，请稍后重试。',
  origin_denied: '当前访问地址与中心配置不一致。',
  service_error: '中心暂时无法处理请求，请稍后重试。',
}

class RequestError extends Error {
  constructor(readonly status: number, readonly code: string) {
    super(messages[code] ?? '请求失败，请重试。')
  }
}

async function api<T>(path: string, options?: RequestInit): Promise<T> {
  const response = await fetch(path, options)
  if (!response.ok) {
    const data = await response.json().catch(() => null) as { error?: string } | null
    throw new RequestError(response.status, data?.error ?? 'service_error')
  }
  if (response.status === 204 || response.status === 201) return undefined as T
  return response.json() as Promise<T>
}

async function show(page: Screen) {
  screen.value = page
  const path = page === 'initialize' || page === 'login' || page === 'overview' ? '/' + page : ''
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
      return
    }
    throw cause
  }
  overview.value = await api<Overview>('/v1/overview')
  await show('overview')
}

async function load() {
  error.value = ''
  screen.value = 'loading'
  try {
    const { initialized } = await api<{ initialized: boolean }>('/v1/bootstrap/status')
    if (!initialized) await show('initialize')
    else await loadSignedIn(true)
  } catch {
    await show('unavailable')
  }
}

function json(value: object) {
  return { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(value) }
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
    } else {
      await api('/v1/auth/login', json({ username: username.value, password: password.value }))
      await loadSignedIn(false)
    }
  } catch (cause) {
    if (cause instanceof RequestError && cause.code === 'already_initialized') await show('login')
    error.value = cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
  } finally {
    password.value = ''
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
    await show('login')
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
  <div v-if="screen === 'overview' && session && overview" class="workspace">
    <aside class="sidebar" aria-label="工作台导航">
      <div class="brand"><span class="brand-icon" aria-hidden="true"></span><span><strong>端域</strong><small>DeskRealm</small></span></div>
      <p class="nav-heading">工作台</p>
      <a class="nav-current" href="/overview" aria-current="page">概览</a>
    </aside>
    <header class="topbar">
      <span class="breadcrumb">工作台 / 概览</span>
      <div class="account">
        <div><strong>{{ session.username }}</strong><small>{{ overview.can_manage ? '平台管理员' : '成员' }}</small></div>
        <button class="secondary" :disabled="submitting" @click="logout">退出登录</button>
      </div>
    </header>
    <main class="content">
      <h1 ref="heading" tabindex="-1">概览</h1>
      <p class="muted">{{ overview.organization }}</p>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <section class="panel" aria-labelledby="organization-title">
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
          <h1 ref="heading" tabindex="-1">{{ screen === 'initialize' ? '首次管理员初始化' : '登录端域控制台' }}</h1>
          <p v-if="screen === 'initialize' && !ticket" class="muted">
            请在中心主机运行 <code>deskrealm bootstrap-ticket</code>，用生成的链接打开初始化页面。
          </p>
          <form v-else @submit.prevent="submit">
            <p v-if="screen === 'initialize'" class="muted">设置组织名称和首个管理员账号。</p>
            <div v-if="screen === 'initialize'" class="field">
              <label for="organization">组织名称</label>
              <input id="organization" v-model="organization" autocomplete="organization" maxlength="80" required>
            </div>
            <div class="field">
              <label for="username">账号</label>
              <input id="username" v-model="username" autocomplete="username" maxlength="40" required>
            </div>
            <div class="field">
              <label for="password">密码</label>
              <input id="password" v-model="password" type="password"
                :autocomplete="screen === 'initialize' ? 'new-password' : 'current-password'"
                :minlength="screen === 'initialize' ? 12 : undefined" maxlength="256" required>
              <span v-if="screen === 'initialize'" class="hint">至少 12 个字符</span>
            </div>
            <p v-if="error" class="error" role="alert">{{ error }}</p>
            <button class="primary full" type="submit" :disabled="submitting">
              {{ submitting ? '正在提交…' : screen === 'initialize' ? '初始化中心' : '登录' }}
            </button>
          </form>
        </template>
      </section>
    </div>
  </main>
</template>
