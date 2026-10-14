import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { accountImportsApi } from '@/api/accountImports'
import { importJobsApi, type ImportJob } from '@/api/importJobs'
import { parseAccountCredentials, type ImportedCredential } from '@/lib/accountCredentials'
import { useToast } from '@/composables/useToast'
import type { useAccountBulkProgressRuntime } from './accountBulkProgressRuntime'

export type AccountImportMode = 'oauth_login' | 'access_token' | 'refresh_token' | 'session_json' | 'cpa_json' | 'remote_cpa' | 'sub2api'
type AccountImportRuntimeOptions = {
  bulkProgress: ReturnType<typeof useAccountBulkProgressRuntime>
  normalizeErrorMessage: (error: unknown) => string
  setError: (prefix: string, error: unknown, notify?: boolean) => void
  loadData: (options?: { silentErrorToast?: boolean }) => Promise<void>
}

export function useAccountImportRuntime(options: AccountImportRuntimeOptions) {
  const importBusy = ref(false)
  const showImportModal = ref(false)
  const importMinimized = ref(false)
  const importMode = ref<AccountImportMode>('access_token')
  const oauthEmailHint = ref('')
  const oauthCallbackText = ref('')
  const oauthSessionId = ref('')
  const oauthAuthorizeUrl = ref('')
  const oauthRedirectUriPrefix = ref('')
  const manualTokenText = ref('')
  const sessionJsonText = ref('')
  const importJob = ref<ImportJob | null>(null)
  const importJobs = ref<ImportJob[]>([])
  const importMonitorError = ref('')
  const toast = useToast()
  let parsedFileRows: ImportedCredential[] = []
  let parsedFilePreview = ''
  let timer: ReturnType<typeof setTimeout> | undefined
  let stopped = false
  const importRunning = computed(() => ['queued', 'running'].includes(importJob.value?.status || ''))
  const importModeOptions = [
    { label: '导入 AT / JSON', value: 'access_token' },
    { label: '导入 Refresh Token', value: 'refresh_token' },
    { label: '导入 Session JSON', value: 'session_json' },
    { label: '导入 CPA JSON 文件', value: 'cpa_json' },
    { label: 'OAuth 登录已有账号', value: 'oauth_login' },
    { label: '从远程 CPA 服务器导入', value: 'remote_cpa' },
    { label: '从 Sub2API 服务器导入', value: 'sub2api' },
  ] as const
  function setImportMode(mode: AccountImportMode) { importMode.value = mode }
  async function openImportModal(mode: AccountImportMode = 'access_token') {
    showImportModal.value = true
    importMinimized.value = false
    setImportMode(mode)
    await refreshImportJobs()
  }
  function closeImportModal() { showImportModal.value = false; importMinimized.value = false }
  function minimizeImportModal() { showImportModal.value = false; importMinimized.value = true }
  async function refreshImportJobs() {
    try { importJobs.value = (await importJobsApi.list()).jobs } catch (error) { importMonitorError.value = options.normalizeErrorMessage(error) }
  }
  async function pollJob() {
    if (stopped || !importJob.value) return
    const id = importJob.value.id
    try {
      const next = await importJobsApi.get(id)
      if (stopped || importJob.value?.id !== id) return
      const wasRunning = importRunning.value
      importJob.value = next
      importMonitorError.value = ''
      if (wasRunning && !importRunning.value) {
        await options.loadData({ silentErrorToast: true })
        await refreshImportJobs()
      }
    } catch (error) { importMonitorError.value = options.normalizeErrorMessage(error) }
    if (!stopped && importRunning.value) timer = setTimeout(() => void pollJob(), 1500)
  }
  function selectImportJob(job: ImportJob) {
    if (timer) clearTimeout(timer)
    importJob.value = job
    void pollJob()
  }
  onMounted(async () => {
    await refreshImportJobs()
    if (stopped) return
    const running = importJobs.value.find(j => ['queued', 'running'].includes(j.status))
    if (running) { importMinimized.value = true; selectImportJob(running) }
  })
  onBeforeUnmount(() => { stopped = true; if (timer) clearTimeout(timer) })
  async function submitImport(payload: Record<string, unknown>) {
    if (importBusy.value || importRunning.value) return
    importBusy.value = true
    try {
      importJob.value = await importJobsApi.start(payload)
      manualTokenText.value = ''
      sessionJsonText.value = ''
      parsedFileRows = []
      parsedFilePreview = ''
      toast.success('导入任务已提交，关闭窗口或页面后仍会继续')
      await refreshImportJobs()
      void pollJob()
    } catch (error) { options.setError('提交导入任务失败', error) }
    finally { importBusy.value = false }
  }
  async function startImport(rows: ImportedCredential[]) { await submitImport({ accounts: rows }) }
  async function importManualTokenText() {
    try {
      const rows = parsedFileRows.length && manualTokenText.value === parsedFilePreview
        ? parsedFileRows : parseAccountCredentials(manualTokenText.value, importMode.value)
      await startImport(rows)
    } catch (error) { options.setError('识别凭据失败', error) }
  }
  async function importTokenTextFile(file: File | null | undefined) {
    if (!file) return
    try {
      parsedFileRows = parseAccountCredentials(await file.text(), importMode.value)
      const rtOnly = parsedFileRows.every(r => !r.access_token && r.refresh_token)
      importMode.value = rtOnly ? 'refresh_token' : 'access_token'
      parsedFilePreview = parsedFileRows.map(r => String(rtOnly ? r.refresh_token : r.access_token || r.refresh_token)).join('\n')
      manualTokenText.value = parsedFilePreview
      toast.success(`已识别 ${parsedFileRows.length} 个账号，点击开始导入`)
    } catch (error) { options.setError('读取文件失败', error) }
  }
  async function importSessionJson() {
    const text = sessionJsonText.value.trim()
    if (!text) { toast.warning('请先粘贴 Session JSON'); return }
    await submitImport({ text, mode: 'session_json' })
  }
  async function loadSessionJsonFile(file: File | null | undefined) {
    if (!file) return
    sessionJsonText.value = ''
    try {
      if (file.size > 16 * 1024 * 1024) throw new Error('Session JSON 文件不能超过 16 MB')
      const text = (await file.text()).trim()
      JSON.parse(text)
      sessionJsonText.value = text
      toast.success('已读取 Session JSON，请确认后点击开始导入')
    } catch (error) { options.setError('读取 Session JSON 失败', error) }
  }
  async function importLocalCPAFiles(files: FileList | File[] | null | undefined) {
    try {
      parsedFileRows = []
      for (const file of Array.from(files || [])) parsedFileRows.push(...parseAccountCredentials(await file.text()))
      const rtOnly = parsedFileRows.length > 0 && parsedFileRows.every(r => !r.access_token && r.refresh_token)
      importMode.value = rtOnly ? 'refresh_token' : 'access_token'
      parsedFilePreview = parsedFileRows.map(r => String(rtOnly ? r.refresh_token : r.access_token || r.refresh_token)).join('\n')
      manualTokenText.value = parsedFilePreview
      toast.success(`已识别 ${parsedFileRows.length} 个账号，点击开始导入`)
    } catch (error) { options.setError('读取 JSON 文件失败', error) }
  }
  async function startOAuthLogin() {
    importBusy.value = true
    try {
      const result = await accountImportsApi.startOAuthLogin(oauthEmailHint.value)
      oauthSessionId.value = String(result.session_id || '')
      oauthAuthorizeUrl.value = String(result.authorize_url || '')
      oauthRedirectUriPrefix.value = String(result.redirect_uri_prefix || '')
      oauthCallbackText.value = ''
      if (!oauthSessionId.value || !oauthAuthorizeUrl.value) {
        throw new Error('后端没有返回完整的 OAuth 授权会话')
      }
      window.open(oauthAuthorizeUrl.value, '_blank', 'noopener,noreferrer')
      toast.success('OAuth 授权链接已生成')
    } catch (error) {
      options.setError('生成 OAuth 授权链接失败', error)
    } finally {
      importBusy.value = false
    }
  }

  function openOAuthAuthorizeUrl() {
    if (!oauthAuthorizeUrl.value) {
      void startOAuthLogin()
      return
    }
    window.open(oauthAuthorizeUrl.value, '_blank', 'noopener,noreferrer')
  }

  async function copyOAuthAuthorizeUrl() {
    const value = oauthAuthorizeUrl.value.trim()
    if (!value) {
      toast.warning('请先生成 OAuth 授权链接')
      return
    }
    try {
      await navigator.clipboard.writeText(value)
      toast.success('授权链接已复制')
    } catch (error) {
      options.setError('复制 OAuth 授权链接失败', error)
    }
  }

  async function finishOAuthLogin() {
    const sessionId = oauthSessionId.value.trim()
    const callback = oauthCallbackText.value.trim()
    if (!sessionId) {
      toast.warning('请先生成 OAuth 授权链接')
      return
    }
    if (!callback) {
      toast.warning('请先粘贴 callback URL 或 code')
      return
    }

    importBusy.value = true
    try {
      const result = await accountImportsApi.finishOAuthLogin(sessionId, callback)
      await options.loadData({ silentErrorToast: true })
      const added = Number(result.added || 0)
      const skipped = Number(result.skipped || 0)
      const refreshed = Number(result.refreshed || 0)
      const errors = Array.isArray(result.errors) ? result.errors.length : 0
      if (errors > 0) {
        toast.warning(`OAuth 登录导入完成：新增 ${added}，跳过 ${skipped}，刷新 ${refreshed}，异常 ${errors}`)
      } else {
        toast.success(`OAuth 登录导入完成：新增 ${added}，跳过 ${skipped}，刷新 ${refreshed}`)
      }
      oauthEmailHint.value = ''
      oauthCallbackText.value = ''
      oauthSessionId.value = ''
      oauthAuthorizeUrl.value = ''
      oauthRedirectUriPrefix.value = ''
    } catch (error) {
      options.setError('OAuth 登录导入失败', error)
    } finally {
      importBusy.value = false
    }
  }

  return {
    importBusy, showImportModal, importMinimized, importMode, importModeOptions,
    importJob, importJobs, importMonitorError, importRunning, selectImportJob, minimizeImportModal,
    oauthEmailHint, oauthCallbackText, oauthSessionId, oauthAuthorizeUrl, oauthRedirectUriPrefix,
    manualTokenText, sessionJsonText, setImportMode, openImportModal, closeImportModal,
    importManualTokenText, importTokenTextFile, importSessionJson, loadSessionJsonFile, importLocalCPAFiles,
    startOAuthLogin, openOAuthAuthorizeUrl, copyOAuthAuthorizeUrl, finishOAuthLogin,
  }
}
