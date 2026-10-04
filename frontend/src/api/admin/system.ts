/** Read-only system information endpoints. */

import { apiClient } from '../client'

export async function getVersion(): Promise<{ version: string }> {
  const { data } = await apiClient.get<{ version: string }>('/admin/system/version')
  return data
}

export const systemAPI = { getVersion }

export default systemAPI
