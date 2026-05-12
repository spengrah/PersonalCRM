import { apiClient } from './api-client'

/**
 * Mac host (paired Mac daemon) returned by the admin API.
 * Mirrors backend/internal/api/handlers/mac_host.go MacHostView.
 */
export interface MacHost {
  id: string
  hostname: string
  daemon_version: string
  protocol_version: number
  last_heartbeat_at?: string
  permissions: Record<string, unknown>
  source_health: Record<string, unknown>
  cursor_epoch: number
  api_key_revoked_at?: string
  created_at: string
  updated_at: string
}

/**
 * Response from POST /api/v1/host/pairing-token. The plaintext token
 * is shown ONCE — the server never echoes it back on subsequent reads.
 */
export interface PairingTokenResponse {
  token: string
  expires_at: string
}

export const macHostsApi = {
  /**
   * GET /api/v1/host — list all active (non-revoked) Mac hosts.
   */
  list: async (): Promise<MacHost[]> => {
    return apiClient.get<MacHost[]>('/api/v1/host')
  },

  /**
   * GET /api/v1/host/:id — single host detail. Includes revoked hosts
   * (the admin UI may want to surface historical detail).
   */
  get: async (id: string): Promise<MacHost> => {
    return apiClient.get<MacHost>(`/api/v1/host/${id}`)
  },

  /**
   * DELETE /api/v1/host/:id — revoke the host's API key + delete its
   * push-strategy sync state rows. Daemon's next request returns 401.
   */
  delete: async (id: string): Promise<{ ok: boolean }> => {
    return apiClient.delete<{ ok: boolean }>(`/api/v1/host/${id}`)
  },

  /**
   * POST /api/v1/host/pairing-token — mint a single-use pairing token.
   * The plaintext token is returned ONCE; subsequent admin calls cannot
   * recover it.
   */
  createPairingToken: async (): Promise<PairingTokenResponse> => {
    return apiClient.post<PairingTokenResponse>('/api/v1/host/pairing-token')
  },
}
