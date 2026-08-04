import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { whatsappApi } from '@/lib/whatsapp-api'
import { whatsappKeys } from '@/lib/query-keys'
import type {
  WhatsAppChatStatusRequest,
  WhatsAppPairRequest,
  WhatsAppStatus,
} from '@/lib/whatsapp-api'

// notPairedSnapshot rewrites a cached status as "no account is linked",
// preserving the non-optional backfill and ingest blocks. The settings section
// derives its whole step from status.state, so a mutation that only invalidated
// would leave the stale connected payload in place for one render — and nothing
// would move it back once the refetch landed into the same shape.
function notPairedSnapshot(prev: WhatsAppStatus | undefined): WhatsAppStatus | undefined {
  if (!prev) return prev
  return {
    ...prev,
    state: 'not_paired',
    reason: undefined,
    pairing: undefined,
    jid: undefined,
    phone_number: undefined,
    push_name: undefined,
    connected_at: undefined,
  }
}

export function useWhatsAppStatus() {
  return useQuery({
    queryKey: whatsappKeys.status(),
    queryFn: whatsappApi.getStatus,
    // Matching useTelegramStatus. The 404 (feature off) is already non-retried
    // by the shared client, but a 500 would otherwise retry three times before
    // the section could say anything at all.
    retry: false,
    // No staleTime: pinning one would override the query-client's Playwright
    // escape hatch.
    refetchInterval: query => {
      const data = query.state.data
      if (!data) return false
      // Polling during a pairing is not optional: whatsmeow emits successive QR
      // codes and the status endpoint is the only way the browser sees a
      // refreshed one before the displayed code expires.
      if (
        data.state === 'pairing' ||
        data.state === 'connecting' ||
        data.state === 'reconnecting'
      ) {
        return 3000
      }
      if (data.backfill.pending + data.backfill.processing > 0) return 3000
      return false
    },
  })
}

export function useStartWhatsAppPairing() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (data: WhatsAppPairRequest) => whatsappApi.startPairing(data),
    onSuccess: () => {
      // The 202 body already carries the first code, but the cache is what the
      // section renders from.
      queryClient.invalidateQueries({ queryKey: whatsappKeys.status() })
    },
  })
}

export function useCancelWhatsAppPairing() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: () => whatsappApi.cancelPairing(),
    onSuccess: () => {
      queryClient.setQueryData<WhatsAppStatus>(whatsappKeys.status(), notPairedSnapshot)
      queryClient.invalidateQueries({ queryKey: whatsappKeys.status() })
    },
  })
}

export function useDisconnectWhatsApp() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ force }: { force?: boolean } = {}) => whatsappApi.disconnect(force),
    onSuccess: () => {
      queryClient.setQueryData<WhatsAppStatus>(whatsappKeys.status(), notPairedSnapshot)
      queryClient.invalidateQueries({ queryKey: whatsappKeys.status() })
      // An unlinked account's chat list is no longer meaningful.
      queryClient.invalidateQueries({ queryKey: whatsappKeys.chats() })
    },
  })
}

export function useWhatsAppChats() {
  return useQuery({
    queryKey: whatsappKeys.chats(),
    queryFn: whatsappApi.listChats,
  })
}

export function useUpdateWhatsAppChatStatus() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({
      chatJid,
      status,
    }: {
      chatJid: string
      status: WhatsAppChatStatusRequest['status']
    }) => whatsappApi.updateChatStatus(chatJid, { status }),
    onSuccess: () => {
      // Only the chat list depends on this; no status field does.
      queryClient.invalidateQueries({ queryKey: whatsappKeys.chats() })
    },
  })
}
