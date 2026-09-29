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
export interface UserRequestAuditQuery { page?: number; page_size?: number; user_id?: number|string; group_id?: number|string; requested_model?: string; response_id?: string; client_request_id?: string; status?: string; protocol?: string; start_time?: string; end_time?: string }
export interface UserRequestAuditConfig { retention_days: number }
export const userRequestAuditAPI = {
  async list(params: UserRequestAuditQuery): Promise<PaginatedResponse<UserRequestAudit>> { return (await apiClient.get('/admin/user-request-audit', { params })).data },
  async get(id: number): Promise<UserRequestAudit> { return (await apiClient.get(`/admin/user-request-audit/${id}`)).data },
  async getConfig(): Promise<UserRequestAuditConfig> { return (await apiClient.get('/admin/user-request-audit/config')).data },
  async updateConfig(config: UserRequestAuditConfig): Promise<UserRequestAuditConfig> { return (await apiClient.put('/admin/user-request-audit/config', config)).data }
}
export default userRequestAuditAPI
