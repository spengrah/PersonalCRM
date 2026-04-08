import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { telegramApi } from '@/lib/telegram-api'
import { telegramKeys } from '@/lib/query-keys'
import type {
  TelegramAuthStartRequest,
  TelegramAuthVerifyCodeRequest,
  TelegramAuthVerifyPasswordRequest,
} from '@/lib/telegram-api'

export function useTelegramStatus() {
  return useQuery({
    queryKey: telegramKeys.status(),
    queryFn: telegramApi.getStatus,
    retry: false,
  })
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
      queryClient.invalidateQueries({ queryKey: telegramKeys.status() })
    },
  })
}
