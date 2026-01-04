'use client'

import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { oauthApi } from '@/lib/oauth-api'

/**
 * Query key for Google accounts
 */
export const googleAccountsQueryKey = ['google-accounts'] as const

/**
 * Hook to fetch all connected Google accounts
 */
export function useGoogleAccounts() {
  return useQuery({
    queryKey: googleAccountsQueryKey,
    queryFn: () => oauthApi.listGoogleAccounts(),
  })
}

/**
 * Hook to fetch a specific Google account's status
 */
export function useGoogleAccountStatus(id: string) {
  return useQuery({
    queryKey: [...googleAccountsQueryKey, id],
    queryFn: () => oauthApi.getGoogleAccountStatus(id),
    enabled: !!id,
  })
}

/**
 * Hook to revoke (disconnect) a Google account
 */
export function useRevokeGoogleAccount() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (id: string) => oauthApi.revokeGoogleAccount(id),
    onSuccess: () => {
      // Invalidate the Google accounts list to refetch
      queryClient.invalidateQueries({ queryKey: googleAccountsQueryKey })
    },
  })
}

/**
 * Hook to get the Google OAuth authorization URL
 */
export function useGoogleAuthUrl() {
  return useMutation({
    mutationFn: () => oauthApi.getGoogleAuthUrl(),
  })
}
