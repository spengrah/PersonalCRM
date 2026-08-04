import { apiClient } from './api-client'

// These types are hand-written, mirroring backend/internal/api/handlers/
// whatsapp_dto.go — the same convention telegram-api.ts uses. They cover the
// fields the UI CONSUMES, not the whole wire shape: every field typed here is
// read by whatsapp-section.tsx and exercised by a named test in
// whatsapp-settings.spec.ts, so a field the UI never reads is dropped rather
// than carried unused. That equality is what bounds the drift risk of
// hand-writing them, so keep it exact — an unread field silently widens the
// unverified surface. The response envelope carries more (`configured`,
// `chat_type`, and the disconnect result's `remote_unlinked` /
// `already_unlinked` / `forced`); those are pinned Go-side by the API tests
// over the real router.

export type WhatsAppState =
  | 'not_ready'
  | 'not_paired'
  | 'pairing'
  | 'connecting'
  | 'connected'
  | 'reconnecting'
  | 'disconnected'
  | 'disconnect_failed'
  | 'error'

export interface WhatsAppPairing {
  method: 'qr' | 'phone'
  qr_code?: string
  pair_code?: string
  expires_at: string
}

export interface WhatsAppBackfill {
  pending: number
  processing: number
  failed: number
  dropped_inline_chunks: number
  observed_floor_at?: string
  stale?: boolean
}

export interface WhatsAppIngest {
  unresolved_lid_peers: number
}

export interface WhatsAppStatus {
  state: WhatsAppState
  reason?: string
  missing?: string
  jid?: string
  phone_number?: string
  push_name?: string
  connected_at?: string
  banned_until?: string
  pairing?: WhatsAppPairing
  // Tri-state on purpose: absent means no terminal decision has been taken,
  // false means one was and could not be durably recorded.
  terminal_reason_persisted?: boolean
  replaced_device_retained?: boolean
  // Tri-state on purpose: absent means no account is linked, false means one is
  // and the record of WHICH device could not be written.
  link_selector_persisted?: boolean
  backfill: WhatsAppBackfill
  ingest: WhatsAppIngest
}

export interface WhatsAppDisconnectResult {
  // Only the warning is consumed: it is the one field that changes what the
  // user must do next (finish the unlink from their phone).
  warning?: string
}

export interface WhatsAppPairRequest {
  method: 'qr' | 'phone'
  phone?: string
}

export type WhatsAppChatStatus = 'auto' | 'tracked' | 'ignored'

export interface WhatsAppChat {
  chat_jid: string
  chat_title?: string
  member_count?: number
  status: WhatsAppChatStatus
  effective_tracked: boolean
}

export interface WhatsAppChatStatusRequest {
  status: WhatsAppChatStatus
}

export const whatsappApi = {
  getStatus: () => apiClient.get<WhatsAppStatus>('/api/v1/whatsapp/auth/status'),

  startPairing: (data: WhatsAppPairRequest) =>
    apiClient.post<WhatsAppStatus>('/api/v1/whatsapp/auth/start', data),

  // The backend answers 204 with a genuinely empty body — unlike Telegram's
  // cancel, which returns a payload. api-client short-circuits on 204 before
  // parsing, so a void return needs no special handling here.
  cancelPairing: () => apiClient.post<void>('/api/v1/whatsapp/auth/cancel'),

  disconnect: (force?: boolean) =>
    apiClient.delete<WhatsAppDisconnectResult>(
      force ? '/api/v1/whatsapp/auth?force=true' : '/api/v1/whatsapp/auth'
    ),

  listChats: () => apiClient.get<WhatsAppChat[]>('/api/v1/whatsapp/chats'),

  updateChatStatus: (chatJid: string, data: WhatsAppChatStatusRequest) =>
    apiClient.patch<WhatsAppChat>(`/api/v1/whatsapp/chats/${encodeURIComponent(chatJid)}`, data),
}
