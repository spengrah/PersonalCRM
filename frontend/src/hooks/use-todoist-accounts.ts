'use client'

import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { oauthApi } from '@/lib/oauth-api'

/**
 * Query key for Todoist accounts
 */
export const todoistAccountsQueryKey = ['todoist-accounts'] as const

/**
 * Hook to fetch all connected Todoist accounts
 */
export function useTodoistAccounts() {
  return useQuery({
    queryKey: todoistAccountsQueryKey,
    queryFn: () => oauthApi.listTodoistAccounts(),
  })
}

/**
 * Hook to fetch a specific Todoist account's status
 */
export function useTodoistAccountStatus(id: string) {
  return useQuery({
    queryKey: [...todoistAccountsQueryKey, id],
    queryFn: () => oauthApi.getTodoistAccountStatus(id),
    enabled: !!id,
  })
}

/**
 * Hook to revoke (disconnect) a Todoist account
 */
export function useRevokeTodoistAccount() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (id: string) => oauthApi.revokeTodoistAccount(id),
    onSuccess: () => {
      // Invalidate the Todoist accounts list to refetch
      queryClient.invalidateQueries({ queryKey: todoistAccountsQueryKey })
    },
  })
}

/**
 * Hook to get the Todoist OAuth authorization URL
 */
export function useTodoistAuthUrl() {
  return useMutation({
    mutationFn: () => oauthApi.getTodoistAuthUrl(),
  })
}
