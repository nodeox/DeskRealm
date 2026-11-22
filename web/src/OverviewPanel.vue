<script setup lang="ts">
import { computed } from 'vue'
import { taskKinds, taskProgress, type Overview } from './overview'
import { auditLabels } from './auditLabels'

const props = defineProps<{ overview: Overview; refreshing: boolean }>()
const emit = defineEmits<{ refresh: [] }>()
const cards = [
  { key: 'online_devices', label: '在线设备', path: '/devices' },
  { key: 'pending_approvals', label: '待审批设备', path: '/devices?review=pending' },
  { key: 'users', label: '可查看账号', path: '/users' },
  { key: 'address_books', label: '可查看通讯录', path: '/address-books' },
  { key: 'retryable_targets', label: '可重试对象', path: '/tasks?retry=available' },
] as const
const visibleCards = computed(() => cards.filter(card => props.overview.metrics[card.key] !== undefined))

function localTime(value: string) {
  return new Date(value).toLocaleString('zh-CN')
}
</script>

<template>
  <section aria-labelledby="overview-metrics-heading" :aria-busy="refreshing">
    <div class="section-heading">
      <h2 id="overview-metrics-heading">资源与待办</h2>
      <button class="secondary" type="button" :disabled="refreshing" @click="emit('refresh')">刷新概览</button>
    </div>
    <ul class="overview-metrics">
      <li v-for="card in visibleCards" :key="card.key">
        <a :href="card.path">
          <span>{{ card.label }}</span>
          <strong>{{ overview.metrics[card.key] }}</strong>
        </a>
      </li>
    </ul>
  </section>
  <div v-if="overview.can_read_tasks || overview.can_read_audit" class="overview-recent">
    <section v-if="overview.can_read_tasks" class="users-surface" aria-labelledby="recent-tasks-heading">
      <div class="section-heading">
        <h2 id="recent-tasks-heading">近期任务</h2>
        <a href="/tasks">全部任务</a>
      </div>
      <ul v-if="overview.recent_tasks.length" class="member-list">
        <li v-for="task in overview.recent_tasks" :key="task.id">
          <a :href="'/tasks?task=' + task.id">
            {{ taskKinds[task.kind] }}<template v-if="task.group"> · {{ task.group }}</template>
            <small>{{ taskProgress(task) }}</small>
          </a>
          <time :datetime="task.created_at">{{ localTime(task.created_at) }}</time>
        </li>
      </ul>
      <p v-else class="muted">暂无可查看的任务。</p>
    </section>
    <section v-if="overview.can_read_audit" class="users-surface" aria-labelledby="recent-audit-heading">
      <div class="section-heading">
        <h2 id="recent-audit-heading">近期审计</h2>
        <a href="/audit">查看审计</a>
      </div>
      <ul v-if="overview.recent_audit.length" class="member-list">
        <li v-for="event in overview.recent_audit" :key="event.id">
          <a :href="'/audit?event=' + event.id">
            {{ auditLabels[event.action] || event.action }}
            <small>{{ event.receipt_state === 'pending' ? '待回执' : '已确认' }}</small>
          </a>
          <time :datetime="event.occurred_at">{{ localTime(event.occurred_at) }}</time>
        </li>
      </ul>
      <p v-else class="muted">暂无可查看的审计事件。</p>
    </section>
  </div>
  <p v-if="overview.can_manage" class="muted overview-integrations">活动会话与中继健康指标待接入。</p>
</template>
