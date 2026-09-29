import { apiClient } from '../client'
import type { PaginatedResponse } from '@/types'

export interface UserRequestAudit {
  id: number
  created_at: string
  updated_at: string
  expires_at?: string
  user_id?: number
  api_key_id?: number
  group_id?: number
  group_name?: string
  protocol: string
  endpoint: string
  requested_model?: string
  upstream_model?: string
  client_request_id?: string
  response_id?: string
  previous_response_id?: string
  fallback_hash?: string
  status: string
  input_usage?: Record<string, unknown>
  output_usage?: Record<string, unknown>
  cache_usage?: Record<string, unknown>
  last_error?: string
  metadata?: Record<string, unknown>
  request_chatml?: string
  response_chatml?: string
  chatml?: string
  conversation_key?: string
}
export interface UserRequestAuditQuery {
  page?: number; page_size?: number; user_id?: number | string; group_id?: number | string; group_name?: string; requested_model?: string; response_id?: string; client_request_id?: string; status?: string; protocol?: string; start_time?: string; end_time?: string; q?: string
}
export interface UserRequestAuditConfig {
  retention_days: number
  cleanup_interval_hours: number
  max_shard_bytes: number
  group_ids: number[]
}
export interface UserRequestAuditStatus { total_bytes: number; file_count: number; current_shard_bytes: number; oldest_at?: string; latest_at?: string; last_cleanup_at?: string }
export interface UserRequestAuditGroupStat { group_id?: number; group_name?: string; total: number; completed: number; failed: number; input_total: number; output_total: number; latest_at?: string }
export type UserRequestAuditGroupStatsResponse = UserRequestAuditGroupStat[]
export type UserRequestAuditExportFormat = 'jsonl' | 'json'
export interface UserRequestAuditExportResult { blob: Blob; filename: string }
const exportFilters = (filters: UserRequestAuditQuery): Omit<UserRequestAuditQuery, 'page' | 'page_size'> => { const { page: _page, page_size: _pageSize, ...rest } = filters; return rest }
function filenameFromDisposition(value: unknown, format: UserRequestAuditExportFormat): string {
  const fallback = `user-request-audit.${format}`
  if (typeof value !== 'string') return fallback
  const match = value.match(/filename\*?=(?:UTF-8''|"?)([^";]+)"?/i)
  let candidate = ''
  try { candidate = match?.[1] ? decodeURIComponent(match[1]) : '' } catch { candidate = '' }
  candidate = candidate.replace(/[\\/:*?"<>|\x00-\x1f]/g, '_').trim()
  return candidate || fallback
}
export const userRequestAuditAPI = {
  async list(params: UserRequestAuditQuery): Promise<PaginatedResponse<UserRequestAudit>> { return (await apiClient.get('/admin/user-request-audit', { params })).data },
  async get(id: number): Promise<UserRequestAudit> { return (await apiClient.get(`/admin/user-request-audit/${id}`)).data },
  async getGroupStats(params: UserRequestAuditQuery): Promise<UserRequestAuditGroupStatsResponse> { return (await apiClient.get('/admin/user-request-audit/groups', { params: exportFilters(params) })).data },
  async getStatus(): Promise<UserRequestAuditStatus> { return (await apiClient.get('/admin/user-request-audit/status')).data },
  async getConfig(): Promise<UserRequestAuditConfig> { return (await apiClient.get('/admin/user-request-audit/config')).data },
  async updateConfig(config: Partial<UserRequestAuditConfig>): Promise<UserRequestAuditConfig> { return (await apiClient.put('/admin/user-request-audit/config', config)).data },
  async cleanup(): Promise<void> { await apiClient.post('/admin/user-request-audit/cleanup') },
  async export(params: UserRequestAuditQuery, format: UserRequestAuditExportFormat): Promise<UserRequestAuditExportResult> {
    const response = await apiClient.post('/admin/user-request-audit/export', { format, ...exportFilters(params) }, { responseType: 'blob' })
    return { blob: response.data as Blob, filename: filenameFromDisposition(response.headers?.['content-disposition'], format) }
  }
}
export default userRequestAuditAPI
