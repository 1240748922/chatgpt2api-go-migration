<script setup lang="ts">
import { ref, computed } from 'vue'
import { Icon } from '@iconify/vue'
import type { AccountAvailability } from '@/types/api'
const props = defineProps<{ value: AccountAvailability }>()
const details = ref(false)
const number = (n: unknown) => Number(n || 0).toLocaleString('zh-CN')
const states = computed(() => Object.entries(props.value.labels).map(([key, label]) => ({ key, label, count: props.value.states[key] || 0 })))
const maintenanceLabel = computed(() => ({ running: '正在维护', idle: '等待下一批', disabled: '已暂停', degraded: '恢复探针' }[props.value.maintenance.status || ''] || '等待状态'))
</script>

<template>
  <section class="rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-700 dark:bg-slate-900">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div class="flex items-center gap-2 text-sm font-medium"><Icon icon="lucide:shield-check" class="h-4 w-4 text-emerald-600" />账号可用性</div>
      <button type="button" class="flex items-center gap-1 text-xs text-slate-500 hover:text-sky-600" :aria-expanded="details" @click="details = !details">{{ details ? '收起详情' : '查看详情' }}<Icon :icon="details ? 'lucide:chevron-up' : 'lucide:chevron-down'" class="h-4 w-4" /></button>
    </div>
    <div class="mt-4 grid grid-cols-2 gap-4 md:grid-cols-4">
      <div><div class="text-xs text-slate-500">凭据就绪</div><div class="mt-1 text-2xl font-semibold tabular-nums">{{ number(value.states.ready) }}</div></div>
      <div><div class="text-xs text-slate-500">文生图就绪额度</div><div class="mt-1 text-2xl font-semibold tabular-nums">{{ number(value.ready_quota.generation?.known) }}</div><div class="mt-1 text-xs text-slate-400">额度未知 {{ number(value.ready_quota.generation?.unknown) }} 个</div></div>
      <div><div class="text-xs text-slate-500">图生图就绪额度</div><div class="mt-1 text-2xl font-semibold tabular-nums">{{ number(value.ready_quota.edit?.known) }}</div><div class="mt-1 text-xs text-slate-400">额度未知 {{ number(value.ready_quota.edit?.unknown) }} 个</div></div>
      <div><div class="text-xs text-slate-500">当前生图并发</div><div class="mt-1 text-2xl font-semibold tabular-nums">{{ number(value.active_images) }}</div><div class="mt-1 text-xs text-slate-400">{{ value.shared_storage ? '所有实例合计' : '本实例' }}</div></div>
    </div>
    <div class="mt-4 flex flex-wrap items-center gap-x-5 gap-y-2 border-t border-slate-100 pt-3 text-xs text-slate-500 dark:border-slate-800">
      <span class="flex items-center gap-1.5"><Icon icon="lucide:refresh-cw" class="h-3.5 w-3.5" :class="value.maintenance.running ? 'animate-spin' : ''" />{{ maintenanceLabel }} · 同步中 {{ number(value.maintenance.running) }} 个</span>
      <span>本批进度 {{ number(value.maintenance.processed) }} / {{ number(value.maintenance.batch_size) }}</span>
      <span v-if="value.prewarm">本实例预热 {{ number(value.prewarm.ready) }} / {{ number(value.prewarm.capacity) }}</span>
    </div>
    <div v-if="details" class="mt-4 space-y-4 border-t border-slate-100 pt-4 text-xs dark:border-slate-800">
      <div class="grid grid-cols-2 gap-x-6 gap-y-3 md:grid-cols-3"><div v-for="state in states" :key="state.key" class="flex justify-between gap-3"><span class="text-slate-500">{{ state.label }}</span><span class="font-medium tabular-nums">{{ number(state.count) }}</span></div></div>
      <p class="text-slate-500">{{ value.maintenance.reason }} · 待维护 {{ number(value.maintenance.pending) }} 个</p>
      <p class="text-slate-500">数据库 P95 {{ number(value.metrics.database_p95_ms) }}ms · 账号写入 P95 {{ number(value.metrics.account_write_p95_ms) }}ms</p>
      <p class="leading-5 text-slate-400">只使用有效期明确且足够的 AT。就绪额度只合计未禁用、未冷却账号的已知额度，两种额度有重叠；额度未知的账号单列。凭据就绪数包含正在生图的账号，不等于空闲槽位。已确认失效的凭据保留供手动处理。</p>
      <p class="text-slate-400">采样 {{ new Date(value.sampled_at).toLocaleString('zh-CN') }}</p>
    </div>
  </section>
</template>
