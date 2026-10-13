<template>
  <section v-if="job || jobs.length" class="mt-5 space-y-3 border-t border-border pt-4 text-sm">
    <div class="flex items-center justify-between gap-3">
      <span class="font-medium">导入任务</span>
      <select class="max-w-[18rem] rounded-lg border border-border bg-background px-2 py-1 text-xs" :value="job?.id || ''" aria-label="查看导入任务" @change="selectJob">
        <option value="" disabled>选择任务</option>
        <option v-for="item in jobs" :key="item.id" :value="item.id">{{ formatJobTime(item.created_at) }} · {{ item.total }} 个</option>
      </select>
    </div>
    <p v-if="error" class="text-xs text-destructive">进度读取失败：{{ error }}（后台任务继续执行）</p>
    <template v-if="job">
      <div class="flex justify-between text-xs text-muted-foreground"><span>入库</span><span>{{ job.import_processed }} / {{ job.total }} · 新增 {{ job.added }} · 跳过 {{ job.skipped }}</span></div>
      <div class="h-1.5 overflow-hidden rounded-full bg-muted"><div class="h-full rounded-full bg-primary transition-all" :style="{ width: percent(job.import_processed, job.total) }" /></div>
      <div class="flex justify-between text-xs text-muted-foreground"><span>额度同步</span><span>{{ job.sync_processed }} / {{ job.sync_total }} · 成功 {{ job.sync_success }} · 失败 {{ job.sync_failed }}</span></div>
      <div class="h-1.5 overflow-hidden rounded-full bg-muted"><div class="h-full rounded-full bg-primary transition-all" :style="{ width: percent(job.sync_processed, job.sync_total) }" /></div>
      <p class="text-xs text-muted-foreground">{{ statusLabel[job.status] || job.status }} · 剩余 {{ Math.max(0, job.sync_total - job.sync_processed) }} 个待同步</p>
      <p v-if="job.error" class="text-xs text-destructive">{{ job.error }}</p>
      <div class="max-h-48 overflow-y-auto rounded-lg border border-border">
        <div v-for="(event, i) in job.events" :key="i" class="flex items-start gap-2 border-b border-border/50 px-3 py-2 text-xs last:border-0">
          <span :class="event.status === 'success' ? 'text-emerald-600' : 'text-destructive'">{{ event.status === 'success' ? '成功' : '失败' }}</span>
          <div class="min-w-0 flex-1 break-words"><span>{{ event.email }}</span><p v-if="event.error" class="mt-1 text-muted-foreground">{{ event.error }}</p></div>
        </div>
        <p v-if="!job.events?.length" class="p-3 text-xs text-muted-foreground">等待处理结果…</p>
      </div>
      <button v-if="job.event_count > 200" type="button" class="text-xs text-primary" :disabled="downloading" @click="downloadLogs">{{ downloading ? '正在下载…' : `下载完整账号日志（${job.event_count} 条）` }}</button>
      <p v-if="downloadError" class="text-xs text-destructive">{{ downloadError }}</p>
    </template>
  </section>
</template>
<script setup lang="ts">
import { importJobsApi, type ImportJob } from '@/api/importJobs'
import { ref } from 'vue'
const downloading = ref(false)
const downloadError = ref('')
const props = defineProps<{ job: ImportJob | null; jobs: ImportJob[]; error: string }>()
const emit = defineEmits<{ select: [job: ImportJob] }>()
const statusLabel: Record<string, string> = { queued: '排队中', running: '处理中', completed: '处理完成', failed: '任务失败' }
function formatJobTime(value: string) {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('zh-CN', { hour12: false })
}
const percent = (done: number, total: number) => `${total ? Math.min(100, done / total * 100) : 0}%`
function selectJob(e: Event) { const job = props.jobs.find(j => j.id === (e.target as HTMLSelectElement).value); if (job) emit('select', job) }
async function downloadLogs() {
  if (!props.job || downloading.value) return
  const id = props.job.id
  downloading.value = true
  downloadError.value = ''
  try {
  const events = []
  for (let offset = 0; offset < props.job.total;) {
    const page = await importJobsApi.events(id, offset, 200)
    events.push(...page.events)
    if (page.next_offset <= offset) break
    offset = page.next_offset
  }
  const url = URL.createObjectURL(new Blob([JSON.stringify(events, null, 2)], { type: 'application/json' }))
  const a = document.createElement('a'); a.href = url; a.download = `import-${id}-logs.json`; a.click(); URL.revokeObjectURL(url)
  } catch { downloadError.value = '下载失败，请重试。' }
  finally { downloading.value = false }
}
</script>
