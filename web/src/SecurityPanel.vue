<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, json, RequestError } from './api'

const props = defineProps<{ csrf: string }>()
const emit = defineEmits<{ expired: [] }>()
const status = ref<{ available: boolean; enabled: boolean }>()
const password = ref('')
const code = ref('')
const secret = ref('')
const error = ref('')
const busy = ref(false)

function report(cause: unknown) {
  if (cause instanceof RequestError && cause.code === 'session_required') emit('expired')
  error.value = cause instanceof RequestError ? cause.message : '无法连接中心，请重试。'
}

onMounted(async () => {
  try {
    status.value = await api('/v1/auth/totp')
  } catch (cause) {
    report(cause)
  }
})

async function submit() {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    if (secret.value) {
      await api('/v1/auth/totp/confirm', json({ password: password.value, code: code.value }, props.csrf))
      secret.value = ''
      status.value = { available: true, enabled: true }
    } else {
      const result = await api<{ secret: string }>('/v1/auth/totp/setup', json({ password: password.value }, props.csrf))
      secret.value = result.secret
    }
  } catch (cause) {
    report(cause)
  } finally {
    password.value = ''
    code.value = ''
    busy.value = false
  }
}
</script>

<template>
  <section class="panel" aria-labelledby="totp-heading">
    <h2 id="totp-heading">两步验证</h2>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <p v-else-if="!status" class="muted" role="status">正在读取验证状态…</p>
    <p v-if="status?.enabled" class="muted" role="status">已启用。登录时需输入验证器生成的验证码。</p>
    <p v-else-if="status && !status.available" class="muted">中心尚未开放两步验证设置。</p>
    <form v-else-if="status?.available" class="link-form" @submit.prevent="submit">
      <p v-if="!secret" class="muted">确认当前密码，然后将密钥添加到验证器应用。</p>
      <div v-else class="field">
        <label for="totp-secret">验证器密钥</label>
        <input id="totp-secret" :value="secret" readonly>
        <span class="hint">在验证器中选择基于时间的验证码，添加后输入当前六位代码。</span>
      </div>
      <div class="field">
        <label for="totp-password">当前密码</label>
        <input id="totp-password" v-model="password" type="password" autocomplete="current-password" required maxlength="256">
      </div>
      <div v-if="secret" class="field">
        <label for="totp-code">验证码</label>
        <input id="totp-code" v-model="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6" required>
      </div>
      <button class="primary" type="submit" :disabled="busy">{{ secret ? '确认并启用' : '生成验证器密钥' }}</button>
    </form>
  </section>
</template>
