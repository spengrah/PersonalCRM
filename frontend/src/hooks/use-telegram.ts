import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { telegramApi } from '@/lib/telegram-api'
import { telegramKeys } from '@/lib/query-keys'
import type {
  TelegramAuthStartRequest,
  TelegramAuthVerifyCodeRequest,
  TelegramAuthVerifyPasswordRequest,
  UpdateChatStatusRequest,
} from '@/lib/telegram-api'

export function useTelegramStatus() {
  const query = useQuery({
    queryKey: telegramKeys.status(),
    queryFn: telegramApi.getStatus,
    retry: false,
    refetchInterval: query => {
      const data = query.state.data
      if (!data) return false
      // Poll during backfill, or right after connect before backfill status is known
      if (data.backfill_in_progress) return 3000
      if (data.connected && !data.last_sync_at) return 3000
      return false
    },
  })
  return query
}

export function useStartTelegramAuth() {
  return useMutation({
    mutationFn: (data: TelegramAuthStartRequest) => telegramApi.startAuth(data),
  })
}

export function useVerifyTelegramCode() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (data: TelegramAuthVerifyCodeRequest) => telegramApi.verifyCode(data),
    onSuccess: data => {
      if (data.status === 'connected') {
        queryClient.invalidateQueries({ queryKey: telegramKeys.status() })
      }
    },
  })
}

export function useVerifyTelegramPassword() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (data: TelegramAuthVerifyPasswordRequest) => telegramApi.verifyPassword(data),
    onSuccess: data => {
      if (data.status === 'connected') {
        queryClient.invalidateQueries({ queryKey: telegramKeys.status() })
      }
    },
  })
}

export function useDisconnectTelegram() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: () => telegramApi.disconnect(),
    onSuccess: () => {
      // Snap the cached status to disconnected before invalidating: the
      // settings section re-derives its step from the cached status, and
      // leaving the stale connected payload in place flips the view back
      // to connected before the refetch lands — after which nothing moves
      // it to disconnected again.
      queryClient.setQueryData(telegramKeys.status(), {
        status: 'disconnected',
        connected: false,
      })
      queryClient.invalidateQueries({ queryKey: telegramKeys.status() })
    },
  })
}

export function useTelegramChats() {
  return useQuery({
    queryKey: telegramKeys.chats(),
    queryFn: telegramApi.listChats,
  })
}

export function useUpdateTelegramChatStatus() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({
      chatId,
      status,
    }: {
      chatId: number
      status: UpdateChatStatusRequest['status']
    }) => telegramApi.updateChatStatus(chatId, { status }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: telegramKeys.chats() })
      queryClient.invalidateQueries({ queryKey: telegramKeys.status() })
    },
  })
}
