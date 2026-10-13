import apiClient from './client'
import type { VersionCheckResponse, VersionInfoResponse } from '@/types/api'
import { projectReleasePageUrl, projectRepository } from '@/config/project'

type UpdateMetaResponse = {
  current_version?: string
  latest_version?: string
  release_name?: string
  release_url?: string
  published_at?: string
  release_notes?: string
  changelog?: string
  update_available?: boolean
  status?: string
  error?: string
  check_message?: string
}

function toVersionInfo(payload: { version?: string }): VersionInfoResponse {
  const version = String(payload.version || '').trim()
  return {
    version,
    tag: version ? (version.startsWith('v') ? version : `v${version}`) : '',
    commit: '',
  }
}

export const versionApi = {
  async current() {
    const payload = await apiClient.get<never, { version: string }>('/version')
    return toVersionInfo(payload)
  },

  async check(force = false): Promise<VersionCheckResponse> {
    const payload = await apiClient.get<never, UpdateMetaResponse>('/meta/update', {
      params: force ? { force: true } : undefined,
    })
    if (!payload || typeof payload !== 'object' || !('status' in payload)) {
      throw new Error('GitHub 版本接口返回格式异常')
    }
    const current = toVersionInfo({ version: payload.current_version })
    const latestVersion = String(payload.latest_version || '').trim()
    const latestTag = latestVersion
      ? (latestVersion.startsWith('v') ? latestVersion : `v${latestVersion}`)
      : ''
    return {
      ...current,
      repository: projectRepository,
      latest_tag: latestTag,
      latest_version: latestVersion,
      release_name: payload.release_name,
      release_url: payload.release_url || projectReleasePageUrl,
      published_at: payload.published_at,
      release_notes: payload.release_notes,
      changelog: payload.changelog,
      is_latest: Boolean(latestVersion) && !payload.update_available,
      update_available: Boolean(payload.update_available),
      check_error: payload.status === 'error' ? (payload.error || 'GitHub 版本检查失败') : undefined,
      check_message: payload.check_message,
    }
  },
}
