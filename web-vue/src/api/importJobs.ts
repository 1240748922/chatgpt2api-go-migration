import { apiClient } from './client'

export type ImportEvent = { email: string; status: string; error: string; time: string }
export type ImportJob = {
  id: string; status: string; total: number; import_processed: number; sync_processed: number;
  sync_total: number; added: number; skipped: number; sync_success: number; sync_failed: number;
  created_at: string; updated_at: string; error?: string; events: ImportEvent[]; event_count: number
}
export const importJobsApi = {
  list: () => apiClient.get<never, { jobs: ImportJob[] }>('/api/account-import-jobs'),
  get: (id: string) => apiClient.get<never, ImportJob>(`/api/account-import-jobs/${encodeURIComponent(id)}`),
  start: (payload: Record<string, unknown>) => apiClient.post<Record<string, unknown>, ImportJob>('/api/account-import-jobs', payload),
  events: (id: string, offset = 0, limit = 100) => apiClient.get<never, { events: ImportEvent[]; total: number; next_offset: number; rows_total: number }>(`/api/account-import-jobs/${encodeURIComponent(id)}/events`, { params: { offset, limit } }),
}
