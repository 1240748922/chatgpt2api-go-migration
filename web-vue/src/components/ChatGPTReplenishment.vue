<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref } from 'vue'
import { Icon } from '@iconify/vue'
import apiClient from '@/api/client'

type Settings = { enabled: boolean; registration_source: string; registration_mode: string; minimum_available: number; target_available: number; max_batch_size: number; workers: number; interval_seconds: number; cooldown_seconds: number; launch_timeout_seconds: number; mailbox_file: string; cfworker_domain: string; smailr_domain: string; no_2fa: boolean; no_phone_reuse: boolean; import_new_sessions: boolean; [key: string]: unknown }
type Job = { id: string; status: string; count: number; created_at: string; error?: string; logs?: string[]; imported?: number; sync_success?: number; sync_failed?: number; session_count?: number; import_job_id?: string; import_processed?: number; sync_processed?: number; sync_total?: number; registration_warning?: string }
type Snapshot = { settings: Settings; jobs: Job[]; worker: { last_seen_at?: string } }
const root = '/api/accounts/replenishment'
const settings = ref<Settings>()
const jobs = ref<Job[]>([])
const provider = ref<Record<string, unknown>>({})
const active = ref('')
const error = ref('')
const message = ref('')
const busy = ref(false)
const providerOpen = ref(false)
const workerTime = ref('')
const manualCount = ref(10)
const poolText = ref('')
let timer: ReturnType<typeof setTimeout> | undefined
let stopped = false
const labels: Record<string, string> = { queued: '等待注册', running: '正在注册', registration_complete: '注册已完成', importing: '导入与额度同步', completed: '完成', failed: '失败' }
async function load(initial = false) {
  try {
    const snap = await apiClient.get<never, Snapshot>(root)
    jobs.value = snap.jobs
    workerTime.value = snap.worker.last_seen_at || ''
    if (initial || !settings.value) settings.value = snap.settings
    if (!active.value && snap.jobs[0]) active.value = snap.jobs[0].id
  } catch (e) { error.value = String(e instanceof Error ? e.message : e) }
}
async function poll() { await load(); if (!stopped) timer = setTimeout(poll, 3000) }
async function act(fn: () => Promise<unknown>, success: string) {
  busy.value = true; error.value = ''; message.value = ''
  try { await fn(); message.value = success; await load() }
  catch (e) { error.value = String(e instanceof Error ? e.message : e) }
  finally { busy.value = false }
}
function save() { return act(() => apiClient.patch(root, settings.value), '注册设置已保存') }
function run() { return act(async () => { await apiClient.patch(root, settings.value); const job = await apiClient.post<never, Job>(`${root}/run`, null, { params: { manual_count: manualCount.value } }); active.value = job.id }, '任务已提交，关闭页面后仍会继续执行') }
async function openProvider() {
  providerOpen.value = !providerOpen.value
  if (providerOpen.value) await act(async () => { provider.value = await apiClient.get<never, Record<string, unknown>>(`${root}/provider-config`); poolText.value = (provider.value.proxy_pool as string[] || []).join('\n') }, '')
}
function saveProvider() { return act(() => apiClient.patch(`${root}/provider-config`, { ...Object.fromEntries(Object.entries(provider.value).filter(([k]) => !k.endsWith('_configured'))), proxy_pool: poolText.value.split('\n').map(v => v.trim()).filter(Boolean) }), '注册服务配置已保存') }
async function mailbox(event: Event) {
 const input = event.target as HTMLInputElement; const file = input.files?.[0]; if (!file) return
 await act(async () => { const data = await apiClient.post<never, { mailbox_file: string }>(`${root}/mailbox-file`, { filename: file.name, content: await file.text() }); if (settings.value) settings.value.mailbox_file = data.mailbox_file }, '邮箱文件已上传'); input.value = ''
}
onMounted(async () => { await load(true); if (!stopped) void poll() })
onBeforeUnmount(() => { stopped = true; clearTimeout(timer) })
</script>

<template>
  <section class="mb-5 rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-700 dark:bg-slate-900">
    <div class="flex flex-wrap items-center justify-between gap-3"><h2 class="flex items-center gap-2 text-base font-semibold"><Icon icon="lucide:user-round-plus" class="h-5 w-5" />ChatGPT 注册与自动补号</h2><button class="text-sm text-sky-600" @click="openProvider">注册服务配置</button></div>
    <p class="mt-2 text-xs text-slate-500">沿用原注册工具，注册代理仅用于注册。成功账号自动入库并同步凭据与额度，网页关闭不影响执行。</p>
    <p v-if="error" role="alert" class="mt-3 rounded-lg bg-red-50 p-3 text-sm text-red-600">{{ error }}</p><p v-if="message" role="status" class="mt-3 text-sm text-emerald-600">{{ message }}</p>
    <div v-if="settings" class="mt-4">
      <div class="grid grid-cols-2 gap-4 lg:grid-cols-4">
        <label class="space-y-1 text-xs text-slate-500">注册来源<select v-model="settings.registration_source" class="replenish-input"><option value="configured">使用工具配置</option><option value="mailbox_file">邮箱文件</option><option value="cfworker">CF Worker</option><option value="smailr">Smailr</option><option value="remail_target">Remail</option><option value="phone">手机注册</option></select></label>
        <label class="space-y-1 text-xs text-slate-500">注册模式<select v-model="settings.registration_mode" class="replenish-input"><option value="password">密码</option><option value="passwordless">无密码</option><option value="har">HAR</option><option value="legacy">Legacy</option></select></label>
        <label class="space-y-1 text-xs text-slate-500">低于此账号数补号<input v-model.number="settings.minimum_available" type="number" min="0" class="replenish-input" /></label>
        <label class="space-y-1 text-xs text-slate-500">目标可用账号数<input v-model.number="settings.target_available" type="number" min="0" class="replenish-input" /></label>
        <label class="space-y-1 text-xs text-slate-500">每批最多<input v-model.number="settings.max_batch_size" type="number" min="1" max="1000" class="replenish-input" /></label>
        <label class="space-y-1 text-xs text-slate-500">注册并发<input v-model.number="settings.workers" type="number" min="1" max="16" class="replenish-input" /></label>
        <label class="space-y-1 text-xs text-slate-500">检查间隔（秒）<input v-model.number="settings.interval_seconds" type="number" min="5" class="replenish-input" /></label>
        <label class="space-y-1 text-xs text-slate-500">批次冷却（秒）<input v-model.number="settings.cooldown_seconds" type="number" min="5" class="replenish-input" /></label>
      </div>
      <div v-if="settings.registration_source === 'mailbox_file'" class="mt-3 flex flex-wrap items-center gap-3 text-xs"><input type="file" accept=".txt,.csv,.json" @change="mailbox" /><span>{{ settings.mailbox_file || '尚未上传邮箱文件' }}</span></div>
      <label v-if="settings.registration_source === 'cfworker'" class="mt-3 block text-xs">CF Worker 域名<input v-model="settings.cfworker_domain" class="replenish-input" /></label>
      <label v-if="settings.registration_source === 'smailr'" class="mt-3 block text-xs">Smailr 域名<input v-model="settings.smailr_domain" class="replenish-input" /></label>
      <div class="mt-4 flex flex-wrap items-center gap-4 text-xs"><label class="flex items-center gap-2"><input v-model="settings.enabled" type="checkbox" />自动补号</label><label class="flex items-center gap-2"><input v-model="settings.import_new_sessions" type="checkbox" />自动导入成功账号</label><button class="rounded-lg border px-4 py-2" :disabled="busy" @click="save">保存设置</button><input v-model.number="manualCount" aria-label="手动注册数量" type="number" min="1" max="1000" class="replenish-input !w-24" /><button class="rounded-lg bg-sky-600 px-4 py-2 text-white disabled:opacity-50" :disabled="busy" @click="run">手动注册</button></div>
      <p class="mt-3 text-xs text-slate-400">注册工作进程：{{ workerTime ? `最近在线 ${new Date(workerTime).toLocaleString()}` : '尚未连接，请启动 registrar 服务' }}</p>
    </div>
    <div v-if="providerOpen" class="mt-4 grid grid-cols-1 gap-3 rounded-lg border p-4 md:grid-cols-2">
      <label v-for="field in ['remail_base_url','remail_api_key','smailr_base_url','smailr_api_key','cfworker_url','cfworker_admin_token','cfworker_api_token','smsbower_api_key','smsbower_country','registration_proxy']" :key="field" class="text-xs text-slate-500">{{ ({remail_base_url:'Remail 地址',remail_api_key:'Remail 密钥',smailr_base_url:'Smailr 地址',smailr_api_key:'Smailr 密钥',cfworker_url:'CF Worker 地址',cfworker_admin_token:'CF Worker 管理令牌',cfworker_api_token:'CF Worker API 令牌',smsbower_api_key:'SMSBower 密钥',smsbower_country:'SMSBower 国家',registration_proxy:'注册代理'} as Record<string,string>)[field] }}<input v-model="provider[field]" :type="field.includes('key') || field.includes('token') ? 'password' : 'text'" :placeholder="provider[`${field}_configured`] ? '已配置，留空保留' : ''" autocomplete="off" class="replenish-input" /></label>
      <label class="text-xs text-slate-500 md:col-span-2">注册代理池（每行一个，仅用于注册）<textarea v-model="poolText" rows="3" class="replenish-input" /></label><button class="rounded-lg border px-4 py-2 text-sm" :disabled="busy" @click="saveProvider">保存注册服务配置</button>
    </div>
    <div v-if="jobs.length" class="mt-5 grid gap-3 lg:grid-cols-[240px_1fr]">
      <div class="max-h-72 space-y-2 overflow-y-auto"><button v-for="job in jobs" :key="job.id" class="block w-full rounded-lg border p-3 text-left text-xs" :class="active===job.id ? 'border-sky-500 bg-sky-50 dark:bg-slate-800' : 'border-slate-200 dark:border-slate-700'" @click="active=job.id"><span>{{ labels[job.status] || job.status }} · {{ job.count }} 个</span><span class="mt-1 block text-slate-400">{{ new Date(job.created_at).toLocaleString() }}</span></button></div>
      <div v-for="job in jobs.filter(j=>j.id===active)" :key="job.id" class="min-w-0"><div class="mb-2 text-xs text-slate-500">注册结果 {{ job.session_count || 0 }} · 入库进度 {{ job.import_processed || 0 }}/{{ job.session_count || 0 }} · 已新增 {{ job.imported || 0 }} · 同步进度 {{ job.sync_processed || 0 }}/{{ job.sync_total || 0 }} · 额度同步成功 {{ job.sync_success || 0 }} / 失败 {{ job.sync_failed || 0 }}</div><p v-if="job.registration_warning" class="mb-2 text-xs text-amber-600">{{ job.registration_warning }}</p><p v-if="job.error" class="mb-2 text-xs text-red-500">{{ job.error }}</p><pre class="h-60 overflow-auto rounded-lg bg-slate-950 p-3 text-xs leading-5 text-slate-200">{{ job.logs?.join('\n') || '等待注册工作进程执行…' }}</pre></div>
    </div>
  </section>
</template>

<style scoped>
.replenish-input { display: block; width: 100%; border: 1px solid #cbd5e1; border-radius: .5rem; padding: .5rem .75rem; font-size: .875rem; color: inherit; background: transparent; }
</style>
